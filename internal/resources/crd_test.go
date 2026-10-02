package resources

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/kube"
)

func certificateDiscoveredKind() kube.DiscoveredKind {
	return kube.DiscoveredKind{
		Kind: "Certificate", Plural: "certificates", Group: "cert-manager.io",
		GVR:           schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"},
		Versions:      []kube.CRDVersion{{Name: "v1", Served: true, Storage: true}},
		ClusterScoped: false,
		PrinterColumns: []kube.PrinterColumn{
			{Name: "Ready", Type: "string", JSONPath: `.status.conditions[?(@.type=="Ready")].status`},
			{Name: "Secret", Type: "string", JSONPath: ".spec.secretName"},
		},
		Established: true,
		CRDName:     "certificates.cert-manager.io",
	}
}

func certificateInstance(name, ns, secret, condStatus string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata":   map[string]any{"name": name, "namespace": ns, "creationTimestamp": metav1.Now().UTC().Format("2006-01-02T15:04:05Z")},
		"spec":       map[string]any{"secretName": secret, "issuerRef": map[string]any{"name": "letsencrypt-prod", "kind": "ClusterIssuer"}},
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "Ready", "status": condStatus, "message": "diagnosis text"},
			},
		},
	}}
}

func TestCustomDescriptorColumns(t *testing.T) {
	desc := CustomDescriptor(certificateDiscoveredKind())
	if desc.Kind != kube.ResourceKind("Certificate") {
		t.Fatalf("unexpected Kind: %s", desc.Kind)
	}
	if !desc.Custom || desc.APIGroup != "cert-manager.io" {
		t.Fatalf("expected Custom=true, APIGroup=cert-manager.io, got %+v", desc)
	}
	// docs/design README.md §14a: breadcrumb/pill text reads "Certificates"
	// (capitalized) + the dim "cert-manager.io/v1" API-version tag, not the
	// CRD's own raw lowercase plural.
	if desc.Display != "Certificates" {
		t.Fatalf("Display = %q, want %q", desc.Display, "Certificates")
	}
	if desc.APIVersion != "v1" {
		t.Fatalf("APIVersion = %q, want %q", desc.APIVersion, "v1")
	}
	want := []string{"Name", "Ready", "Secret", "Age"}
	if len(desc.Columns) != len(want) {
		t.Fatalf("Columns = %v, want %v", desc.Columns, want)
	}
	for i, w := range want {
		if desc.Columns[i] != w {
			t.Fatalf("Columns[%d] = %q, want %q", i, desc.Columns[i], w)
		}
	}
}

func TestProjectCustomResourceCellsAndStatus(t *testing.T) {
	desc := CustomDescriptor(certificateDiscoveredKind())
	row := desc.Project(certificateInstance("api-tls", "default", "api-tls-secret", "True"))
	if len(row.Cells) != len(desc.Columns) {
		t.Fatalf("%d cells, want %d columns (%v vs %v)", len(row.Cells), len(desc.Columns), row.Cells, desc.Columns)
	}
	if row.Name != "api-tls" || row.Namespace != "default" {
		t.Fatalf("unexpected identity: %+v", row)
	}
	if row.Cells[1] != "True" {
		t.Fatalf("Ready cell = %q, want True", row.Cells[1])
	}
	if row.Cells[2] != "api-tls-secret" {
		t.Fatalf("Secret cell = %q, want api-tls-secret", row.Cells[2])
	}
	if row.Status != StatusOK || row.Glyph != "●" {
		t.Fatalf("expected OK/●, got %s/%s", row.Status, row.Glyph)
	}
}

func TestProjectCustomResourceStatusFalseAndNoCondition(t *testing.T) {
	desc := CustomDescriptor(certificateDiscoveredKind())

	notReady := desc.Project(certificateInstance("staging-tls", "staging", "staging-secret", "False"))
	if notReady.Status != StatusFail || notReady.Glyph != "✕" {
		t.Fatalf("expected Fail/✕ for a False Ready condition, got %s/%s", notReady.Status, notReady.Glyph)
	}

	noConditions := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata":   map[string]any{"name": "bare", "namespace": "default"},
		"spec":       map[string]any{"secretName": "bare-secret"},
	}}
	row := desc.Project(noConditions)
	if row.Status != StatusNeutral || row.Glyph != "·" {
		t.Fatalf("expected the 14a no-conditions fallback (neutral/·), got %s/%s", row.Status, row.Glyph)
	}
}

func TestCRDDescriptorProjectsCountAndScope(t *testing.T) {
	counter := fakeCounter{"Certificate": 3}
	desc := CRDDescriptor(counter)
	crdObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "certificates.cert-manager.io"},
		"spec": map[string]any{
			"group": "cert-manager.io",
			"names": map[string]any{"kind": "Certificate", "plural": "certificates"},
			"scope": "Namespaced",
			"versions": []any{
				map[string]any{"name": "v1", "served": true, "storage": true},
				map[string]any{"name": "v1beta1", "served": false, "storage": false, "deprecated": true},
			},
		},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Established", "status": "True"}},
		},
	}}
	row := desc.Project(crdObj)
	if len(row.Cells) != len(desc.Columns) {
		t.Fatalf("%d cells, want %d columns", len(row.Cells), len(desc.Columns))
	}
	if row.Cells[4] != "3" {
		t.Fatalf("COUNT cell = %q, want 3 (from InstanceCounter)", row.Cells[4])
	}
	if row.Cells[3] != "Namespaced" {
		t.Fatalf("SCOPE cell = %q, want Namespaced", row.Cells[3])
	}
	if row.Cells[2] != "v1, v1beta1 (deprecated)" {
		t.Fatalf("VERSIONS cell = %q", row.Cells[2])
	}
	if row.Key != "Certificate" {
		t.Fatalf("Key = %q, want Certificate (browse's ↵ routing reads this)", row.Key)
	}
	if row.Status != StatusOK {
		t.Fatalf("expected Established=true to project OK, got %s", row.Status)
	}
}

// TestCRDDescriptorCountIsNeverFabricated: a kind whose instance count isn't
// known — cache still filling, Forbidden, failing, or no live cluster at all
// — renders COUNT as "–", never a 0 the cluster didn't report.
func TestCRDDescriptorCountIsNeverFabricated(t *testing.T) {
	crdObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "certificates.cert-manager.io"},
		"spec": map[string]any{
			"group":    "cert-manager.io",
			"names":    map[string]any{"kind": "Certificate", "plural": "certificates"},
			"scope":    "Namespaced",
			"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true}},
		},
	}}
	for name, counter := range map[string]InstanceCounter{
		"unknown to the counter": fakeCounter{},
		"no live cluster":        nil,
	} {
		if got := CRDDescriptor(counter).Project(crdObj).Cells[4]; got != "–" {
			t.Errorf("%s: COUNT cell = %q, want –", name, got)
		}
	}
	if got := CRDDescriptor(fakeCounter{"Certificate": 0}).Project(crdObj).Cells[4]; got != "0" {
		t.Errorf("a known empty kind: COUNT cell = %q, want 0", got)
	}
}

type fakeCounter map[kube.ResourceKind]int

func (f fakeCounter) CountInstances(kind kube.ResourceKind) (int, bool) {
	n, ok := f[kind]
	return n, ok
}

func httpRouteDiscoveredKind() kube.DiscoveredKind {
	return kube.DiscoveredKind{
		Kind: "HTTPRoute", Plural: "httproutes", Group: "gateway.networking.k8s.io",
		Versions:      []kube.CRDVersion{{Name: "v1", Served: true, Storage: true}},
		ClusterScoped: false,
		Established:   true,
		CRDName:       "httproutes.gateway.networking.k8s.io",
	}
}

func httpRouteInstance(name, ns, parentName, condStatus, message string) *unstructured.Unstructured {
	cond := map[string]any{"type": "Accepted", "status": condStatus}
	if message != "" {
		cond["message"] = message
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata":   map[string]any{"name": name, "namespace": ns, "creationTimestamp": metav1.Now().UTC().Format("2006-01-02T15:04:05Z")},
		"status": map[string]any{
			"parents": []any{
				map[string]any{"parentRef": map[string]any{"name": parentName}, "conditions": []any{cond}},
			},
		},
	}}
}

func TestHTTPRouteDescriptorAttachedColumn(t *testing.T) {
	desc := httpRouteDescriptor(httpRouteDiscoveredKind())
	if !desc.Custom {
		t.Fatalf("expected httpRouteDescriptor to stay Custom")
	}
	if desc.Columns[0] != "Name" || desc.Columns[1] != "Attached" || desc.Columns[len(desc.Columns)-1] != "Age" {
		t.Fatalf("unexpected columns: %v", desc.Columns)
	}

	accepted := desc.Project(httpRouteInstance("web-route", "prod", "public", "True", ""))
	if accepted.Status != StatusOK || accepted.Cells[1] != "✓ gw/public" {
		t.Fatalf("unexpected accepted row: %+v", accepted)
	}

	rejected := desc.Project(httpRouteInstance("orphan", "prod", "public", "False", "no matching listener hostname"))
	if rejected.Status != StatusFail || rejected.Cells[1] != "✕ not accepted: no matching listener hostname" {
		t.Fatalf("unexpected rejected row: %+v", rejected)
	}

	noStatus := desc.Project(&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
		"metadata": map[string]any{"name": "brand-new", "namespace": "prod"},
	}})
	if noStatus.Status != StatusFail || noStatus.Cells[1] != "✕ not accepted" {
		t.Fatalf("unexpected no-status row: %+v", noStatus)
	}
}

func TestBuildDiscoveredRegistryHTTPRouteGetsAttachedDescriptor(t *testing.T) {
	reg, _ := BuildDiscoveredRegistry([]kube.DiscoveredKind{httpRouteDiscoveredKind()}, nil)
	d, ok := reg.Descriptor(kube.KindHTTPRoute)
	if !ok || d.Columns[1] != "Attached" {
		t.Fatalf("expected HTTPRoute to get the bespoke ATTACHED descriptor, got %+v (ok=%v)", d, ok)
	}
}

func TestBuildDiscoveredRegistryIngressGetsLiveBackendResolution(t *testing.T) {
	sel := map[string]string{"app": "web"}
	reader := fakeClusterReader{lister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindService: {serviceWithSelector("web", "default", sel)},
		kube.KindPod:     {readyPod("web-1", "default", sel, true)},
	}}}

	reg, _ := BuildDiscoveredRegistry(nil, reader)
	d, ok := reg.Descriptor(kube.KindIngress)
	if !ok {
		t.Fatalf("expected Ingress to stay registered")
	}

	pathType := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: "web.local",
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{Path: "/", PathType: &pathType, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80}}}},
						},
					},
				},
			}},
		},
	}
	row := d.Project(ing)
	if row.Cells[4] != "1 ok" || row.Status != StatusOK {
		t.Fatalf("expected the live reader's backend resolution to reach through the registry, got %+v", row)
	}
}

// A defaultBackend-only Ingress is a catch-all whose one backend still
// counts, not "–" as if it routed nowhere.
func TestIngressBackendsCountsDefaultBackend(t *testing.T) {
	sel := map[string]string{"app": "web"}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindService: {serviceWithSelector("web", "default", sel)},
		kube.KindPod:     {readyPod("web-1", "default", sel, true)},
	}}
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "catch-all", Namespace: "default"},
		Spec: networkingv1.IngressSpec{DefaultBackend: &networkingv1.IngressBackend{
			Service: &networkingv1.IngressServiceBackend{Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80}},
		}},
	}
	if text, status, _ := ingressBackendsCell(lister, "default", ing); text != "1 ok" || status != StatusOK {
		t.Fatalf("BACKENDS = %q/%s, want 1 ok", text, status)
	}
}

// fakeClusterReader satisfies ClusterReader for
// TestBuildDiscoveredRegistryIngressGetsLiveBackendResolution — CountInstances
// is unused by that test, so it's a stub.
type fakeClusterReader struct{ lister fakeLister }

func (f fakeClusterReader) CountInstances(kube.ResourceKind) (int, bool) { return 0, true }
func (f fakeClusterReader) ListRaw(ctx context.Context, kind kube.ResourceKind, ns string) ([]runtime.Object, error) {
	return f.lister.ListRaw(ctx, kind, ns)
}

func TestBuildDiscoveredRegistryRegistersEveryDiscoveredKind(t *testing.T) {
	discovered := []kube.DiscoveredKind{certificateDiscoveredKind()}
	reg, groups := BuildDiscoveredRegistry(discovered, nil)

	d, ok := reg.Descriptor(kube.ResourceKind("Certificate"))
	if !ok || !d.Custom {
		t.Fatalf("expected a Custom descriptor for Certificate, got %+v (ok=%v)", d, ok)
	}
	if _, ok := reg.Descriptor(kube.KindCustomResourceDefinition); !ok {
		t.Fatalf("expected CustomResourceDefinition to stay registered")
	}
	if _, ok := reg.Descriptor(kube.KindPod); !ok {
		t.Fatalf("expected every built-in kind to still be registered")
	}

	var found bool
	for _, g := range groups {
		if g.ID == GroupCustomResources {
			found = true
			if len(g.Kinds) != 1 || g.Kinds[0] != kube.ResourceKind("Certificate") {
				t.Fatalf("unexpected GroupCustomResources kinds: %v", g.Kinds)
			}
		}
	}
	if !found {
		t.Fatalf("expected a GroupCustomResources group to be appended")
	}
}

func TestBuildDiscoveredRegistryNoDiscoveredKindsOmitsGroup(t *testing.T) {
	_, groups := BuildDiscoveredRegistry(nil, nil)
	for _, g := range groups {
		if g.ID == GroupCustomResources {
			t.Fatalf("expected no GroupCustomResources group when nothing was discovered")
		}
	}
}
