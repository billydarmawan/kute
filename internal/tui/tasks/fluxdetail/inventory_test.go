package fluxdetail

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

// recordingLister serves fixed objects per kind, filtered by namespace the
// way a cache read is, and records every (kind, namespace) it was asked for
// — the inventory load's whole cost to the cluster.
type recordingLister struct {
	objs  map[kube.ResourceKind][]runtime.Object
	reads []string
}

func (l *recordingLister) ListRaw(_ context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	l.reads = append(l.reads, string(kind)+"@"+namespace)
	var out []runtime.Object
	for _, o := range l.objs[kind] {
		if _, ns, _ := metaNameNamespace(o); namespace == "" || ns == namespace {
			out = append(out, o)
		}
	}
	return out, nil
}

func discovered(kind, plural, group string) kube.DiscoveredKind {
	return kube.DiscoveredKind{
		Kind: kind, Plural: plural, Group: group,
		GVR:         schema.GroupVersionResource{Group: group, Version: "v1", Resource: plural},
		Versions:    []kube.CRDVersion{{Name: "v1", Served: true, Storage: true}},
		Established: true,
		CRDName:     plural + "." + group,
	}
}

func readyCR(apiVersion, kind, namespace, name, status string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": status},
		}},
	}}
}

// TestInventoryResolvesGroupAndReadsNarrowly is H1: an inventory entry is
// identified by (group, Kind), not the bare Kind. Read by bare Kind, a Flux
// HelmRelease entry listed §18a's Helm releases cluster-wide, a Knative
// Service resolved against core Services, and every entry's kind — Secrets
// included — started a cluster-wide informer for a "–".
func TestInventoryResolvesGroupAndReadsNarrowly(t *testing.T) {
	ksvc := discovered("Service", "services", "serving.knative.dev")
	// Two discovered groups sharing an otherwise-unclaimed Kind: only the
	// whole snapshot (AssignRegistryKinds) knows which one is qualified.
	widgetA := discovered("Widget", "widgets", "a.example.com")
	widgetB := discovered("Widget", "widgets", "b.example.com")
	kust := discovered("Kustomization", "kustomizations", kube.FluxGroupKustomize)
	fluxHR := discovered("HelmRelease", "helmreleases", kube.FluxGroupHelm)
	reg, groups := resources.BuildDiscoveredRegistry(
		kube.AssignRegistryKinds([]kube.DiscoveredKind{ksvc, widgetA, widgetB, kust, fluxHR}), nil)

	ks := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kube.FluxGroupKustomize + "/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "apps", "namespace": "flux-system"},
		"status": map[string]any{"inventory": map[string]any{"entries": []any{
			map[string]any{"id": "web_hello_serving.knative.dev_Service", "v": "v1"},
			map[string]any{"id": "flux-system_podinfo_helm.toolkit.fluxcd.io_HelmRelease", "v": "v2"},
			map[string]any{"id": "web_gadget_b.example.com_Widget", "v": "v1"},
			map[string]any{"id": "web_api_apps_Deployment", "v": "v1"},
			map[string]any{"id": "web_creds__Secret", "v": "v1"},
			map[string]any{"id": "web_cfg__ConfigMap", "v": "v1"},
		}}},
	}}
	one := int32(1)
	lister := &recordingLister{objs: map[kube.ResourceKind][]runtime.Object{
		"Kustomization": {ks},
		// The same names under the wrong group, so a bare-Kind read would
		// find *something* and report its readiness.
		kube.KindService:              {&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "web"}}},
		"Service.serving.knative.dev": {readyCR("serving.knative.dev/v1", "Service", "web", "hello", "True")},
		kube.KindFluxHelmRelease:      {readyCR(kube.FluxGroupHelm+"/v2", "HelmRelease", "flux-system", "podinfo", "False")},
		"Widget":                      {readyCR("a.example.com/v1", "Widget", "web", "gadget", "False")},
		"Widget.b.example.com":        {readyCR("b.example.com/v1", "Widget", "web", "gadget", "True")},
		kube.KindDeployment: {&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "web"},
			Spec:       appsv1.DeploymentSpec{Replicas: &one},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
		}},
	}}

	sess := &tui.Session{Theme: tui.Dark(), Registry: reg, Groups: groups,
		Location: tui.Location{Context: "c", Namespace: "flux-system"}}
	m := New(Config{Session: sess, Lister: lister, Kind: "Kustomization", Namespace: "flux-system", Name: "apps"})
	m.SetSize(120, 30)
	upd, _ := m.Update(m.load()())
	got := upd.(*Model)

	type want struct {
		kind  kube.ResourceKind
		ready string
	}
	wants := map[string]want{
		"hello":   {"Service.serving.knative.dev", "ready"},
		"podinfo": {kube.KindFluxHelmRelease, "not ready"},
		"gadget":  {"Widget.b.example.com", "ready"},
		"api":     {kube.KindDeployment, "1/1"},
		"creds":   {kube.KindSecret, "–"},
		"cfg":     {kube.KindConfigMap, "–"},
	}
	if len(got.inventory) != len(wants) {
		t.Fatalf("inventory = %+v, want %d rows", got.inventory, len(wants))
	}
	for _, it := range got.inventory {
		w := wants[it.Name]
		if it.Kind != w.kind || it.Ready != w.ready {
			t.Errorf("%s: kind %q ready %q, want kind %q ready %q", it.Name, it.Kind, it.Ready, w.kind, w.ready)
		}
	}

	// What the inventory cost: one read per readable (kind, namespace) its
	// entries name — no cluster-wide read, no read of a kind with no
	// readiness, and never §18a's Helm kind or core Services.
	invReads := slices.DeleteFunc(slices.Clone(lister.reads), func(r string) bool {
		return r == "Kustomization@flux-system" // the object itself
	})
	wantReads := []string{
		"Service.serving.knative.dev@web",
		string(kube.KindFluxHelmRelease) + "@flux-system",
		"Widget.b.example.com@web",
		"Deployment@web",
	}
	slices.Sort(invReads)
	slices.Sort(wantReads)
	if !slices.Equal(invReads, wantReads) {
		t.Errorf("inventory reads = %v, want %v", invReads, wantReads)
	}

	// ↵ on the Knative row opens the Knative kind, not core Services.
	got.invExpanded = true // it is healthy, so folded by default
	for i, it := range got.visibleInventory() {
		if it.Name != "hello" {
			continue
		}
		got.selected = i
		_, cmd := got.Update(tea.KeyPressMsg{Text: "enter"})
		if cmd == nil {
			t.Fatal("↵ produced no navigation")
		}
		if msg, ok := cmd().(tui.GotoResourceMsg); !ok || msg.Kind != "Service.serving.knative.dev" {
			t.Errorf("↵ on the Knative Service sent %#v", cmd())
		}
		return
	}
	t.Fatal("the Knative Service row is not visible")
}

// TestDependsOnReadsRegistryKindInOwnNamespace is M1: a Flux HelmRelease's
// spec.dependsOn was read through its bare Kind — §18a's synthetic Helm
// kind, whose objects findObject can never match — and in the object's own
// namespace even when the entry names another, so a suspended dependency
// never showed.
func TestDependsOnReadsRegistryKindInOwnNamespace(t *testing.T) {
	fluxHR := discovered("HelmRelease", "helmreleases", kube.FluxGroupHelm)
	reg, groups := resources.BuildDiscoveredRegistry(kube.AssignRegistryKinds([]kube.DiscoveredKind{fluxHR}), nil)

	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kube.FluxGroupHelm + "/v2", "kind": "HelmRelease",
		"metadata": map[string]any{"name": "app", "namespace": "apps"},
		"spec": map[string]any{"dependsOn": []any{
			map[string]any{"name": "db", "namespace": "data"},
		}},
	}}
	db := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kube.FluxGroupHelm + "/v2", "kind": "HelmRelease",
		"metadata": map[string]any{"name": "db", "namespace": "data"},
		"spec":     map[string]any{"suspend": true},
	}}
	lister := &recordingLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindFluxHelmRelease: {app, db},
	}}
	sess := &tui.Session{Theme: tui.Dark(), Registry: reg, Groups: groups,
		Location: tui.Location{Context: "c", Namespace: "apps"}}
	m := New(Config{Session: sess, Lister: lister, Kind: kube.KindFluxHelmRelease, Namespace: "apps", Name: "app"})
	m.SetSize(120, 30)
	upd, _ := m.Update(m.load()())
	got := upd.(*Model)

	if len(got.chn.DependsOn) != 1 {
		t.Fatalf("dependsOn = %+v, want one entry", got.chn.DependsOn)
	}
	if dep := got.chn.DependsOn[0]; dep.Namespace != "data" || !dep.Suspended {
		t.Errorf("dependency = %+v, want db in data, suspended", dep)
	}
	for _, r := range lister.reads {
		if r == string(kube.KindHelmRelease)+"@apps" || r == string(kube.KindHelmRelease)+"@data" {
			t.Errorf("read §18a's Helm kind: %v", lister.reads)
		}
	}
	if !slices.Contains(lister.reads, string(kube.KindFluxHelmRelease)+"@data") {
		t.Errorf("reads = %v, want the dependency read in its own namespace", lister.reads)
	}
}

// TestSuspendedOutranksAFrozenFailure is M3: a suspended Kustomization whose
// frozen Ready is False rendered RECONCILE FAILED with a retry countdown —
// a retry that never comes — while the list showed it ‖ suspended.
func TestSuspendedOutranksAFrozenFailure(t *testing.T) {
	kust := discovered("Kustomization", "kustomizations", kube.FluxGroupKustomize)
	reg, groups := resources.BuildDiscoveredRegistry(kube.AssignRegistryKinds([]kube.DiscoveredKind{kust}), nil)
	ks := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kube.FluxGroupKustomize + "/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "apps", "namespace": "flux-system"},
		"spec":     map[string]any{"suspend": true, "interval": "10m"},
		"status": map[string]any{"conditions": []any{map[string]any{
			"type": "Ready", "status": "False", "reason": "HealthCheckFailed", "message": "boom",
			"lastTransitionTime": time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339),
		}}},
	}}
	lister := &recordingLister{objs: map[kube.ResourceKind][]runtime.Object{"Kustomization": {ks}}}
	sess := &tui.Session{Theme: tui.Dark(), Registry: reg, Groups: groups,
		Location: tui.Location{Context: "c", Namespace: "flux-system"}}
	m := New(Config{Session: sess, Lister: lister, Kind: "Kustomization", Namespace: "flux-system", Name: "apps"})
	m.SetSize(120, 30)
	upd, _ := m.Update(m.load()())
	view := plain(upd.(*Model).Render())

	if strings.Contains(view, "RECONCILE FAILED") || strings.Contains(view, "retry in") {
		t.Errorf("a suspended object must not render a live failure card:\n%s", view)
	}
	if !strings.Contains(view, "SUSPENDED") {
		t.Errorf("expected the suspended card:\n%s", view)
	}
	if !strings.Contains(view, "last status before suspend · HealthCheckFailed") || !strings.Contains(view, "boom") {
		t.Errorf("expected the frozen failure as a muted history line:\n%s", view)
	}
}

// TestRetryCountdownProjectsPastTheFirstAttempt is M4: Ready's
// lastTransitionTime stays put while the object keeps failing, so anchoring
// the countdown to it read "retry in now" for every failure older than one
// interval. Past the first attempt it projects onto the interval grid.
func TestRetryCountdownProjectsPastTheFirstAttempt(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	since := now.Add(-3*time.Hour - 2*time.Minute) // 3h02m into a 10m interval
	m := Model{now: now, fail: &failure{Since: since, RetryAt: since.Add(10 * time.Minute), RetryEvery: 10 * time.Minute}}
	if got := m.retryIn(); got != "~8m 00s" {
		t.Errorf("retryIn = %q, want ~8m 00s", got)
	}
	// Within the first interval the anchored countdown is exact.
	m.fail.Since = now.Add(-4 * time.Minute)
	m.fail.RetryAt = m.fail.Since.Add(10 * time.Minute)
	if got := m.retryIn(); got != "6m 00s" {
		t.Errorf("retryIn = %q, want 6m 00s", got)
	}
}

// syncingLister is recordingLister with per-kind cache state, the way
// *kube.Cluster reports a lazily started informer.
type syncingLister struct {
	*recordingLister
	unsynced map[kube.ResourceKind]bool
	errs     map[kube.ResourceKind]error
}

func (l syncingLister) KindSynced(kind kube.ResourceKind, _ string) bool { return !l.unsynced[kind] }
func (l syncingLister) KindError(kind kube.ResourceKind, _ string) error { return l.errs[kind] }

// TestColdCacheIsNotAGoneObject is M7: a Kustomization whose informer this
// screen starts read as "no longer exists" until its first event — and for
// good when the kind is forbidden — and inventory rows its caches hadn't
// filled yet were folded away as ready.
func TestColdCacheIsNotAGoneObject(t *testing.T) {
	kust := discovered("Kustomization", "kustomizations", kube.FluxGroupKustomize)
	reg, groups := resources.BuildDiscoveredRegistry(kube.AssignRegistryKinds([]kube.DiscoveredKind{kust}), nil)
	sess := &tui.Session{Theme: tui.Dark(), Registry: reg, Groups: groups,
		Location: tui.Location{Context: "c", Namespace: "flux-system"}}
	open := func(l resources.RawLister) (*Model, tea.Cmd) {
		m := New(Config{Session: sess, Lister: l, Kind: "Kustomization", Namespace: "flux-system", Name: "apps"})
		m.SetSize(120, 30)
		upd, cmd := m.Update(m.load()())
		return upd.(*Model), cmd
	}

	cold := syncingLister{recordingLister: &recordingLister{}, unsynced: map[kube.ResourceKind]bool{"Kustomization": true}}
	got, cmd := open(cold)
	if got.state != tui.TaskStateLoading || strings.Contains(plain(got.Render()), "no longer exists") {
		t.Errorf("a cold cache must not report the object gone: state %s, %q", got.state, got.feedback)
	}
	if cmd == nil {
		t.Fatal("expected a cache-sync retry while the cache fills")
	}
	if retry, ok := cmd().(tui.CacheSyncRetryMsg); !ok || retry.Gen != got.loadEpoch {
		t.Errorf("retry = %#v, want this load's epoch %d", cmd(), got.loadEpoch)
	}

	denied := syncingLister{recordingLister: &recordingLister{},
		errs: map[kube.ResourceKind]error{"Kustomization": errors.New("kustomizations is forbidden")}}
	got, _ = open(denied)
	if strings.Contains(got.feedback, "no longer exists") || !strings.Contains(got.feedback, "forbidden") {
		t.Errorf("an unreadable kind must say why, not that the object is gone: %q", got.feedback)
	}

	// Found, but the inventory's Deployment cache is still filling.
	ks := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kube.FluxGroupKustomize + "/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "apps", "namespace": "flux-system"},
		"status": map[string]any{"inventory": map[string]any{"entries": []any{
			map[string]any{"id": "web_api_apps_Deployment", "v": "v1"},
		}}},
	}}
	filling := syncingLister{
		recordingLister: &recordingLister{objs: map[kube.ResourceKind][]runtime.Object{"Kustomization": {ks}}},
		unsynced:        map[kube.ResourceKind]bool{kube.KindDeployment: true},
	}
	got, cmd = open(filling)
	if got.state != tui.TaskStateReady || got.feedback != "loading inventory…" || cmd == nil {
		t.Errorf("filling inventory: state %s, feedback %q, retry %v", got.state, got.feedback, cmd != nil)
	}
}
