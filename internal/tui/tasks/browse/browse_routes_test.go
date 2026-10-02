package browse

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
)

func testIngressRow(name, ns string) *networkingv1.Ingress {
	return &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
}

// TestIngressSortsUnhealthyFirst pins 23a's explicit requirement: Ingress
// rows sort unhealthy-first like every other workload kind, not the plain
// alphabetical-by-name order Ingress previously fell back to (it was missing
// from sort.go's workloadKinds set).
func TestIngressSortsUnhealthyFirst(t *testing.T) {
	rows := []resources.Row{
		{Name: "aaa-healthy", Status: resources.StatusOK},
		{Name: "zzz-broken", Status: resources.StatusFail},
	}
	sortForDisplay(kube.KindIngress, "default", rows, false, false, false, false)
	if rows[0].Name != "zzz-broken" || rows[1].Name != "aaa-healthy" {
		t.Fatalf("expected unhealthy-first order [zzz-broken, aaa-healthy], got %v", []string{rows[0].Name, rows[1].Name})
	}
}

// TestEnterOnIngressRowOpensRouteTable exercises 23a: ↵ on an Ingress row
// (which desc.Custom never covers — Ingress is a built-in kind) pushes
// tasks/routetable via OpenRouteTable.
func TestEnterOnIngressRowOpensRouteTable(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindIngress: {testIngressRow("web", "default")},
	}}
	session := newSession()
	session.Location.Kind = kube.KindIngress

	var openedKind kube.ResourceKind
	var openedNS, openedName string
	m := New(Config{
		Session: session, Lister: lister,
		OpenRouteTable: func(kind kube.ResourceKind, ns, name string, w, h int) (tea.Model, tea.Cmd) {
			openedKind, openedNS, openedName = kind, ns, name
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "enter"})
	if openedKind != kube.KindIngress || openedNS != "default" || openedName != "web" {
		t.Fatalf("expected Ingress default/web opened, got kind=%s ns=%s name=%s", openedKind, openedNS, openedName)
	}
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected Update to return the pushed stub task, got %T", updated)
	}
}

// TestEnterOnDiscoveredHTTPRouteOpensRouteTableNotObjectDetail exercises
// 23b's ordering requirement: a discovered "HTTPRoute" kind is Custom (like
// any CRD), so openSelectedRouteTable must be checked ahead of the generic
// object-detail branch — otherwise ↵ would silently fall through to 14d
// instead of the bespoke routing table.
func TestEnterOnDiscoveredHTTPRouteOpensRouteTableNotObjectDetail(t *testing.T) {
	dk := kube.DiscoveredKind{
		Kind: "HTTPRoute", Plural: "httproutes", Group: "gateway.networking.k8s.io",
		Versions: []kube.CRDVersion{{Name: "v1", Served: true, Storage: true}}, Established: true,
	}
	reg, _ := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{dk}, nil)
	route := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata":   map[string]any{"name": "web-route", "namespace": "default"},
	}}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHTTPRoute: {route},
	}}
	session := newSession()
	session.Registry = reg
	session.Location.Kind = kube.KindHTTPRoute

	var openedRouteTable, openedObjectDetail bool
	m := New(Config{
		Session: session, Lister: lister,
		OpenRouteTable: func(kube.ResourceKind, string, string, int, int) (tea.Model, tea.Cmd) {
			openedRouteTable = true
			return stubTask{}, nil
		},
		OpenObjectDetail: func(kube.ResourceKind, string, string, []ObjectSiblingRef, int, int, int) (tea.Model, tea.Cmd) {
			openedObjectDetail = true
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m.Update(tea.KeyPressMsg{Text: "enter"})
	if !openedRouteTable {
		t.Fatalf("expected OpenRouteTable to be called for a discovered HTTPRoute row")
	}
	if openedObjectDetail {
		t.Fatalf("expected OpenObjectDetail NOT to be called once OpenRouteTable handled the kind")
	}
}

// TestIngressKeybarOmitsGenericOpenHint confirms Enter still routes through
// routes.go without repeating the global open operation in the keybar.
func TestIngressKeybarOmitsGenericOpenHint(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindIngress: {testIngressRow("web", "default")},
	}}
	session := newSession()
	session.Location.Kind = kube.KindIngress
	m := New(Config{
		Session: session, Lister: lister,
		OpenRouteTable: func(kube.ResourceKind, string, string, int, int) (tea.Model, tea.Cmd) {
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	hasOpenHint := false
	for _, g := range m.Keybar().Groups {
		for _, h := range g {
			if h.Key == "↵" {
				hasOpenHint = true
			}
		}
	}
	if hasOpenHint {
		t.Fatalf("expected no generic ↵ open in the Ingress keybar, got %+v", m.Keybar().Groups)
	}
}
