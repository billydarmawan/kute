package helmdetail

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
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

// NamespaceEvents answers like the real read: only namespace's own Events.
func (e fakeEvents) NamespaceEvents(_ context.Context, namespace string) ([]kube.Event, error) {
	var out []kube.Event
	for _, ev := range e {
		if namespace == "" || ev.Namespace == namespace {
			out = append(out, ev)
		}
	}
	return out, nil
}

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
	m.now = release.Hooks[0].LastRun.StartedAt.Add(90 * time.Minute) // hook ages read the screen's clock
	view := ansi.Strip(m.Render())
	for _, want := range []string{"pending-install", "Initial install underway", "Helm transaction was not finalized", "pre-upgrade", "Job/timesheet-backend-migrate", "for 1h", "object no longer present", "Deployment/timesheet-backend", "not created"} {
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

// startedLister adds the KindStarted seam to recordingLister: only kinds in
// started have a running cache.
type startedLister struct {
	recordingLister
	started map[kube.ResourceKind]bool
}

func (l *startedLister) KindStarted(kind kube.ResourceKind, _ string) bool { return l.started[kind] }

// TestDeclaredSecretReadsOnlyAStartedSecretCache: a chart's Secret must not
// start the shared cluster-wide Secret informer from this screen. With no
// Secret cache running the row reads neutral "not checked" rather than "not
// created"; once something else has started that cache it is read normally.
func TestDeclaredSecretReadsOnlyAStartedSecretCache(t *testing.T) {
	release := pendingRelease()
	release.Manifest += `---
apiVersion: v1
kind: Secret
metadata:
  name: timesheet-backend-creds
`
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "timesheet-backend-creds", Namespace: "timesheet"}}
	secretRow := func(m Model) diagnosticRow {
		t.Helper()
		for _, row := range m.rows {
			if row.ref.Kind == "Secret" {
				return row
			}
		}
		t.Fatalf("no Secret row in %+v", m.rows)
		return diagnosticRow{}
	}
	readSecret := func(l *startedLister) bool {
		for _, kind := range l.reads {
			if kind == kube.KindSecret {
				return true
			}
		}
		return false
	}

	cold := &startedLister{
		recordingLister: recordingLister{objects: map[kube.ResourceKind][]runtime.Object{kube.KindSecret: {secret}}},
		started:         map[kube.ResourceKind]bool{},
	}
	m := New(Config{Session: diagnosticSession(), Lister: cold, Events: fakeEvents{}, Release: release})
	m.SetSize(120, 36)
	updated, _ := m.Update(m.load()())
	m = *updated.(*Model)
	if readSecret(cold) {
		t.Fatalf("read Secret with no Secret cache started: %v", cold.reads)
	}
	for _, kind := range []kube.ResourceKind{kube.KindService, kube.KindDeployment, kube.KindJob} {
		if !slices.Contains(cold.reads, kind) {
			t.Errorf("declared %s not read: %v", kind, cold.reads)
		}
	}
	row := secretRow(m)
	if !row.unchecked || row.live || !strings.HasPrefix(row.liveState, "not checked") {
		t.Fatalf("cold Secret row = %+v, want unchecked", row)
	}
	if view := ansi.Strip(m.Render()); !strings.Contains(view, "not checked") {
		t.Errorf("render missing \"not checked\":\n%s", view)
	}

	warm := &startedLister{
		recordingLister: recordingLister{objects: map[kube.ResourceKind][]runtime.Object{kube.KindSecret: {secret}}},
		started:         map[kube.ResourceKind]bool{kube.KindSecret: true},
	}
	m = New(Config{Session: diagnosticSession(), Lister: warm, Events: fakeEvents{}, Release: release})
	updated, _ = m.Update(m.load()())
	m = *updated.(*Model)
	if !readSecret(warm) {
		t.Fatalf("started Secret cache not read: %v", warm.reads)
	}
	if row := secretRow(m); row.unchecked || !row.live || row.liveState != "present" {
		t.Fatalf("warm Secret row = %+v, want present", row)
	}
}

// TestReloadIntoLoadingRestartsTheSpinner: a reload that finds a cache
// unsynced flips a ready screen back to loading, whose spinner chain stopped
// when the screen became ready — it must be restarted, or the spinner
// freezes. A load that lands while already loading must not start a second
// chain.
func TestReloadIntoLoadingRestartsTheSpinner(t *testing.T) {
	release := pendingRelease()
	m := New(Config{Session: diagnosticSession(), Lister: &recordingLister{}, Events: fakeEvents{}, Release: release})
	hasTick := func(cmd tea.Cmd) bool {
		if cmd == nil {
			return false
		}
		batch, ok := cmd().(tea.BatchMsg)
		if !ok {
			return false
		}
		for _, c := range batch {
			if c == nil {
				continue
			}
			if _, ok := c().(spinner.TickMsg); ok {
				return true
			}
		}
		return false
	}

	_, cmd := m.Update(loadedMsg{epoch: m.reloadEpoch, release: release, pending: true})
	if hasTick(cmd) {
		t.Fatal("a pending load while already loading started a second spinner chain")
	}
	updated, _ := m.Update(loadedMsg{epoch: m.reloadEpoch, release: release})
	m = *updated.(*Model)
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready", m.state)
	}
	updated, cmd = m.Update(loadedMsg{epoch: m.reloadEpoch, release: release, pending: true})
	m = *updated.(*Model)
	// A tea.Tick cmd can only be run once, so ask hasTick exactly once.
	if restarted := hasTick(cmd); m.state != tui.TaskStateLoading || !restarted {
		t.Fatalf("state = %s, spinner restarted = %v; want loading with the tick chain restarted", m.state, restarted)
	}
}

// TestHookEvidenceReadsTheHooksOwnNamespace: a hook a chart renders into
// another namespace has its Events there, so its evidence must come from
// that namespace — not read "not present" off the release's.
func TestHookEvidenceReadsTheHooksOwnNamespace(t *testing.T) {
	release := pendingRelease()
	release.Hooks[0].Manifest = `apiVersion: batch/v1
kind: Job
metadata:
  name: timesheet-backend-migrate
  namespace: migrations
`
	events := fakeEvents{{
		Type: "Normal", Reason: "Completed", Message: "Job completed",
		Object: "Job/timesheet-backend-migrate", Namespace: "migrations",
		LastSeen: release.Hooks[0].LastRun.StartedAt.Add(time.Minute),
	}}
	m := New(Config{Session: diagnosticSession(), Lister: &recordingLister{}, Events: events, Release: release})
	updated, _ := m.Update(m.load()())
	m = *updated.(*Model)
	if got := m.rows[0].liveState; got != "Completed event · object no longer present" {
		t.Fatalf("hook evidence = %q, want the Completed event from its own namespace", got)
	}
}

// TestBodyWindowMatchesWholeListLayout: Body wraps and styles only the rows
// that can reach the screen, and must lay them out exactly as wrapping every
// row and then windowing would — for every selection, including wrapped
// rows taller than the viewport.
func TestBodyWindowMatchesWholeListLayout(t *testing.T) {
	m := New(Config{Session: diagnosticSession(), Lister: &recordingLister{}, Events: fakeEvents{}, Release: pendingRelease()})
	m.state = tui.TaskStateReady
	for i := range 40 {
		live := strings.Repeat("evidence words ", i%7)
		m.rows = append(m.rows, diagnosticRow{typeLabel: "object", ref: kube.HelmObjectRef{Kind: "ConfigMap", Name: "cm-" + strings.Repeat("x", i%3)}, helmState: "declared", liveState: "not created " + live, kindType: rowObject})
	}
	reference := func(m Model, width, height int) string {
		theme := m.Theme()
		columns := []components.Column{{Title: "Source", Min: 15}, {Title: "Resource", Min: 18, Flex: true}, {Title: "Helm state", Min: 12}, {Title: "Live evidence", Min: 22, Flex: true}}
		table := components.Table{Columns: columns, Selected: m.selected, Width: width, Height: max(height, 3),
			HeaderStyle: lipgloss.NewStyle().Foreground(theme.TextFaint).Bold(true)}
		widths := table.ColumnWidths(width)
		wrapped := make([][]string, len(m.rows))
		for i, row := range m.rows {
			wrapped[i] = renderDiagnosticRow(row, i == m.selected, widths, width, theme, m.now)
		}
		budget := max(height-2, 1)
		start, through := 0, 0
		for i := 0; i <= m.selected && i < len(wrapped); i++ {
			through += len(wrapped[i])
		}
		for start < m.selected && through > budget {
			through -= len(wrapped[start])
			start++
		}
		var lines []string
		remaining := budget
		for i := start; i < len(wrapped) && remaining > 0; i++ {
			if len(wrapped[i]) > remaining {
				if i == start || i == m.selected {
					lines = append(lines, wrapped[i][:remaining]...)
				}
				break
			}
			lines = append(lines, wrapped[i]...)
			remaining -= len(wrapped[i])
		}
		return strings.Join(lines, "\n")
	}
	for _, size := range [][2]int{{80, 3}, {80, 12}, {120, 30}} {
		for sel := range m.rows {
			m.selected = sel
			got := strings.SplitN(m.Body(size[0], size[1]), "\n", 3)
			body := ""
			if len(got) == 3 {
				body = got[2]
			}
			if want := reference(m, size[0], size[1]); body != want {
				t.Fatalf("size %v selected %d: windowed body differs from the whole-list layout", size, sel)
			}
		}
	}
}
