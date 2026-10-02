package fluxdetail

import (
	"context"
	"slices"
	"testing"

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
