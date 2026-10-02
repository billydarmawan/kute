package helmdetail

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

type recordingLister struct {
	objects map[kube.ResourceKind][]runtime.Object
	reads   []kube.ResourceKind
}

func (l *recordingLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	l.reads = append(l.reads, kind)
	return l.objects[kind], nil
}

type fakeEvents []kube.Event

func (e fakeEvents) NamespaceEvents(context.Context, string) ([]kube.Event, error) { return e, nil }

func diagnosticSession() *tui.Session {
	return &tui.Session{
		Location: tui.Location{Context: "dev", Namespace: "timesheet"},
		Theme:    tui.Dark(), Registry: resources.DefaultRegistry(),
	}
}

func pendingRelease() kube.HelmRelease {
	started := time.Date(2026, 9, 3, 10, 54, 12, 0, time.FixedZone("MSK", 3*60*60))
	return kube.HelmRelease{
		Name: "timesheet-backend", Namespace: "timesheet", Revision: 1,
		Status: "pending-install", StatusReason: "Initial install underway", Updated: started,
		Manifest: `apiVersion: v1
kind: Service
metadata:
  name: timesheet-backend
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: timesheet-backend
`,
		Hooks: []kube.HelmHook{{
			Name: "timesheet-backend-migrate", Kind: "Job", Events: []string{"pre-install", "pre-upgrade"},
			DeletePolicies: []string{"before-hook-creation", "hook-succeeded"},
			LastRun:        kube.HelmHookRun{StartedAt: started, Phase: "Running"},
			Manifest: `apiVersion: batch/v1
kind: Job
metadata:
  name: timesheet-backend-migrate
`,
		}},
	}
}

func TestPendingInstallExplainsCompletedMissingHookAndUncreatedObjects(t *testing.T) {
	release := pendingRelease()
	lister := &recordingLister{objects: map[kube.ResourceKind][]runtime.Object{
		kube.KindHelmRelease: {kube.NewHelmReleaseObject(release)},
	}}
	events := fakeEvents{{
		Type: "Normal", Reason: "Completed", Message: "Job completed",
		Object: "Job/timesheet-backend-migrate", Namespace: "timesheet",
		LastSeen: release.Hooks[0].LastRun.StartedAt.Add(74 * time.Minute),
	}}
	m := New(Config{Session: diagnosticSession(), Lister: lister, Events: events, Release: release})
	m.SetSize(120, 36)
	updated, _ := m.Update(m.load()())
	m = *updated.(*Model)
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready", m.state)
	}
	if !strings.Contains(m.diagnosis, "Helm transaction was not finalized") {
		t.Fatalf("diagnosis = %q", m.diagnosis)
	}
	if len(m.rows) != 3 {
		t.Fatalf("rows = %d, want hook + two objects", len(m.rows))
	}
	if got := m.rows[0].liveState; got != "Completed event · object no longer present" {
		t.Fatalf("hook evidence = %q", got)
	}
	if m.rows[1].liveState != "not created" || m.rows[2].liveState != "not created" {
		t.Fatalf("object evidence = %+v", m.rows[1:])
	}
	view := ansi.Strip(m.Render())
	for _, want := range []string{"pending-install", "Initial install underway", "Helm transaction was not finalized", "pre-upgrade", "Job/timesheet-backend-migrate", "since 10:54", "object no longer present", "Deployment/timesheet-backend", "not created"} {
		if !strings.Contains(view, want) {
			t.Errorf("render missing %q:\n%s", want, view)
		}
	}
}

func TestDetailReadsOnlyKindsDeclaredByRelease(t *testing.T) {
	release := pendingRelease()
	lister := &recordingLister{objects: map[kube.ResourceKind][]runtime.Object{
		kube.KindHelmRelease: {kube.NewHelmReleaseObject(release)},
		kube.KindJob:         {&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "timesheet-backend-migrate", Namespace: "timesheet"}}},
	}}
	m := New(Config{Session: diagnosticSession(), Lister: lister, Events: fakeEvents{}, Release: release})
	m.load()()
	want := map[kube.ResourceKind]bool{kube.KindHelmRelease: true, kube.KindJob: true, kube.KindService: true, kube.KindDeployment: true}
	for _, kind := range lister.reads {
		delete(want, kind)
	}
	if len(want) != 0 {
		t.Fatalf("missing reads for release kinds: %v; got %v", want, lister.reads)
	}
	for _, kind := range lister.reads {
		if kind == kube.KindPod || kind == kube.KindSecret || kind == kube.KindNode {
			t.Fatalf("breadth-first/unrelated read: %v", lister.reads)
		}
	}
}

func TestRegistryKindUsesAPIGroupForFluxHelmRelease(t *testing.T) {
	reg, _ := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{{
		Kind: "HelmRelease", Plural: "helmreleases", Group: kube.FluxGroupHelm,
		GVR:      schema.GroupVersionResource{Group: kube.FluxGroupHelm, Version: "v2", Resource: "helmreleases"},
		Versions: []kube.CRDVersion{{Name: "v2", Served: true, Storage: true}}, Established: true,
	}}, nil)
	ref := kube.HelmObjectRef{APIVersion: kube.FluxGroupHelm + "/v2", Kind: "HelmRelease", Name: "app"}
	if got := registryKind(reg, ref); got != kube.KindFluxHelmRelease {
		t.Fatalf("registryKind() = %s, want %s", got, kube.KindFluxHelmRelease)
	}
}

func TestClusterScopedManifestRefUsesClusterScope(t *testing.T) {
	ref := normalizeRef(resources.DefaultRegistry(), kube.HelmObjectRef{APIVersion: "v1", Kind: "Namespace", Namespace: "release-ns", Name: "created-ns"})
	if ref.Namespace != "" {
		t.Fatalf("Namespace scope = %q, want cluster-wide", ref.Namespace)
	}
}

// secretLister adds the release-Secret seam to recordingLister.
type secretLister struct {
	recordingLister
	secrets     []runtime.Object
	secretReads int
}

func (l *secretLister) ListHelmReleaseSecrets(context.Context, string) ([]runtime.Object, error) {
	l.secretReads++
	return l.secrets, nil
}

// TestEventBurstReloadsOnceAfterDebounce: Events are always relevant to this
// screen, so a burst of them must not re-run the whole load per event — it
// schedules one debounced reload, and only the newest one fires.
func TestEventBurstReloadsOnceAfterDebounce(t *testing.T) {
	release := pendingRelease()
	lister := &secretLister{secrets: []runtime.Object{kube.EncodeHelmReleaseSecret(release)}}
	m := New(Config{Session: diagnosticSession(), Lister: lister, Events: fakeEvents{}, Release: release})
	updated, _ := m.Update(m.load()())
	m = *updated.(*Model)
	lister.reads, lister.secretReads = nil, 0

	var cmds []tea.Cmd
	for range 5 {
		_, cmd := m.Update(kube.ResourceChangedMsg{Kind: kube.KindEvent})
		if cmd == nil {
			t.Fatal("expected an Event change to schedule a reload")
		}
		cmds = append(cmds, cmd)
	}
	if lister.secretReads != 0 || len(lister.reads) != 0 {
		t.Fatalf("Event changes loaded immediately: secretReads=%d reads=%v", lister.secretReads, lister.reads)
	}
	for i, cmd := range cmds {
		due, ok := cmd().(reloadDueMsg)
		if !ok {
			t.Fatalf("scheduled cmd %d produced %T, want reloadDueMsg", i, due)
		}
		_, load := m.Update(due)
		if (load != nil) != (i == len(cmds)-1) {
			t.Fatalf("reloadDueMsg %d: load=%v, want only the newest to load", i, load != nil)
		}
		if load != nil {
			load()
		}
	}
	if lister.secretReads != 1 {
		t.Fatalf("release re-read %d times for one burst, want 1", lister.secretReads)
	}
}

// TestReleaseReadUsesReleaseSecretSeam: the release itself is re-read through
// the narrow release-Secret cache, never the decorated ListRaw(HelmRelease)
// that decodes every release in the namespace.
func TestReleaseReadUsesReleaseSecretSeam(t *testing.T) {
	release := pendingRelease()
	newer := release
	newer.Revision, newer.Status = 2, "deployed"
	other := release
	other.Name = "unrelated"
	lister := &secretLister{secrets: []runtime.Object{
		kube.EncodeHelmReleaseSecret(release), kube.EncodeHelmReleaseSecret(newer), kube.EncodeHelmReleaseSecret(other),
	}}
	m := New(Config{Session: diagnosticSession(), Lister: lister, Events: fakeEvents{}, Release: release})
	msg := m.load()().(loadedMsg)
	for _, kind := range lister.reads {
		if kind == kube.KindHelmRelease {
			t.Fatalf("read the decorated HelmRelease list: %v", lister.reads)
		}
	}
	if lister.secretReads != 1 || msg.release.Revision != 2 || msg.release.Status != "deployed" {
		t.Fatalf("secretReads=%d release=%d/%s, want one seam read landing revision 2 deployed", lister.secretReads, msg.release.Revision, msg.release.Status)
	}
}
