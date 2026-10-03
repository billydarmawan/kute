package routetable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

type fakeLister struct {
	objs map[kube.ResourceKind][]runtime.Object
}

func (f fakeLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	return f.objs[kind], nil
}

func plain(s string) string { return ansi.Strip(s) }

func newSession() *tui.Session {
	return &tui.Session{Theme: tui.Dark(), Location: tui.Location{Context: "test-cluster"}}
}

func step(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c != nil {
				m = step(t, m, c())
			}
		}
		return m
	}
	updated, cmd := m.Update(msg)
	next := *updated.(*Model)
	if cmd != nil {
		if msg := cmd(); msg != nil {
			return step(t, next, msg)
		}
	}
	return next
}

func readyPod(name, ns string, labels map[string]string, ready bool) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Ready: ready}}},
	}
}

func serviceWithSelector(name, ns string, selector map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.ServiceSpec{Selector: selector},
	}
}

func testIngress() *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: "web.local",
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{Path: "/", PathType: &pathType, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80}}}},
							{Path: "/admin", PathType: &pathType, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "missing", Port: networkingv1.ServiceBackendPort{Number: 80}}}},
						},
					},
				},
			}},
		},
	}
}

func TestLoadIngressResolvesBackendsAndTLS(t *testing.T) {
	sel := map[string]string{"app": "web"}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindIngress: {testIngress()},
		kube.KindService: {serviceWithSelector("web", "default", sel)},
		kube.KindPod:     {readyPod("web-1", "default", sel, true)},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "web"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready (feedback %q)", m.state, m.feedback)
	}
	if len(m.rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(m.rows))
	}
	if m.rows[0].glyph != "●" || m.rows[0].class != "ok" {
		t.Fatalf("expected the resolved backend row to be ●/ok, got %+v", m.rows[0])
	}
	if m.rows[1].glyph != "✕" || m.rows[1].class != "fail" {
		t.Fatalf("expected the missing-service row to be ✕/fail, got %+v", m.rows[1])
	}
	if m.rows[0].url != "http://web.local/" {
		t.Fatalf("unexpected url: %q", m.rows[0].url)
	}

	view := plain(m.Render())
	for _, want := range []string{"web.local /", "web:80", "missing:80"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

func TestLoadIngressTLSExpiry(t *testing.T) {
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: networkingv1.IngressSpec{
			TLS: []networkingv1.IngressTLS{{Hosts: []string{"web.local"}, SecretName: "web-tls"}},
		},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindIngress: {ing},
		kube.KindSecret:  {&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "web-tls", Namespace: "default"}}},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "web"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.tlsFacts) != 1 || m.tlsFacts[0].expiry != "no cert data" {
		t.Fatalf("unexpected tlsFacts: %+v", m.tlsFacts)
	}
}

// TestLoadIngressWildcardTLSCoversSubdomainRule pins that a "*.example.com"
// TLS block covers a single-label subdomain rule host (https, TLS cell),
// but neither the apex nor a two-label subdomain.
func TestLoadIngressWildcardTLSCoversSubdomainRule(t *testing.T) {
	backend := networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80}}}
	rule := func(host string) networkingv1.IngressRule {
		return networkingv1.IngressRule{Host: host, IngressRuleValue: networkingv1.IngressRuleValue{
			HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/", Backend: backend}}},
		}}
	}
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: networkingv1.IngressSpec{
			TLS:   []networkingv1.IngressTLS{{Hosts: []string{"*.example.com"}, SecretName: "wild-tls"}},
			Rules: []networkingv1.IngressRule{rule("a.example.com"), rule("example.com"), rule("a.b.example.com")},
		},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindIngress: {ing},
		kube.KindSecret:  {&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "wild-tls", Namespace: "default"}}},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "web"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(m.rows))
	}
	if m.rows[0].url != "https://a.example.com/" || m.rows[0].tlsText != "no cert data" {
		t.Fatalf("wildcard-covered row = url %q tls %q, want https + the block's TLS cell", m.rows[0].url, m.rows[0].tlsText)
	}
	for _, r := range m.rows[1:] {
		if !strings.HasPrefix(r.url, "http://") || r.tlsText != "" {
			t.Fatalf("uncovered row = url %q tls %q, want plain http and no TLS cell", r.url, r.tlsText)
		}
	}
}

// TestLoadIngressHostlessRuleHasNoURL pins that a rule with no host (or a
// wildcard host) offers no URL to copy rather than "http:///path", and the
// keybar drops "y copy url" on it.
func TestLoadIngressHostlessRuleHasNoURL(t *testing.T) {
	for _, host := range []string{"", "*.example.com"} {
		ing := testIngress()
		ing.Spec.Rules[0].Host = host
		lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindIngress: {ing}}}
		m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "web"})
		m.SetSize(120, 36)
		m = step(t, m, m.Init()())

		if len(m.rows) == 0 || m.rows[0].url != "" {
			t.Fatalf("host %q: rows = %+v, want a row with no url", host, m.rows)
		}
		if _, ok := m.copySelectedURL(); ok {
			t.Fatalf("host %q: expected no url copy", host)
		}
		for _, g := range m.Keybar().Groups {
			for _, h := range g {
				if h.Key == verbs.CopyRouteURL.Key {
					t.Fatalf("host %q: keybar still offers %q", host, h.Key)
				}
			}
		}
	}
}

// TestTabTogglesTLSFocusAndArrowsMoveWithinIt pins 23a's "tab" toggle
// (docs/design README.md:285: "a strip above the keybar names each secret —
// ↵ there jumps to it"): 'tab' moves focus onto the TLS strip instead of the
// main table, and up/down move the focused fact instead of the main table's
// own selection while focused.
func TestTabTogglesTLSFocusAndArrowsMoveWithinIt(t *testing.T) {
	m := Model{flavor: flavorIngress, tlsFacts: []tlsFact{{secretName: "a"}, {secretName: "b"}}}
	m.SetSize(120, 36)

	if m.tlsFocused {
		t.Fatal("expected the TLS strip to start unfocused")
	}
	updated, _ := m.Update(tea.KeyPressMsg{Text: "tab"})
	m = *updated.(*Model)
	if !m.tlsFocused {
		t.Fatal("expected 'tab' to focus the TLS strip")
	}

	updated, _ = m.Update(tea.KeyPressMsg{Text: "down"})
	m = *updated.(*Model)
	if m.tlsSelected != 1 {
		t.Fatalf("tlsSelected = %d, want 1 (down moved within the TLS strip, not the main table)", m.tlsSelected)
	}
	if m.selected != 0 {
		t.Fatalf("main table selected = %d, want unchanged at 0 while TLS-focused", m.selected)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Text: "tab"})
	m = *updated.(*Model)
	if m.tlsFocused {
		t.Fatal("expected a second 'tab' to unfocus the TLS strip")
	}
}

// TestOpenSelectedTLSSecretJumpsToReferencedSecret pins 23a's ↵ behavior
// once the TLS strip has focus, mirroring TestOpenSelectedEnterJumpsToBackendService's
// direct-Model style.
func TestOpenSelectedTLSSecretJumpsToReferencedSecret(t *testing.T) {
	m := Model{
		flavor: flavorIngress, namespace: "default",
		tlsFocused: true, tlsSelected: 0,
		tlsFacts: []tlsFact{{secretName: "web-tls"}},
	}
	cmd, ok := m.openSelectedTLSSecret()
	if !ok || cmd == nil {
		t.Fatalf("expected a jump cmd for a focused TLS fact with a secret name")
	}

	m = Model{flavor: flavorIngress, tlsFocused: true, tlsSelected: 0, tlsFacts: nil}
	if _, ok := m.openSelectedTLSSecret(); ok {
		t.Fatal("expected no jump with no TLS facts")
	}
}

func testHTTPRoute(name, parent, condStatus, message string, backends []map[string]any) *unstructured.Unstructured {
	cond := map[string]any{"type": "Accepted", "status": condStatus}
	if message != "" {
		cond["message"] = message
	}
	refs := make([]any, len(backends))
	for i, b := range backends {
		refs[i] = b
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec": map[string]any{
			"parentRefs": []any{map[string]any{"name": parent}},
			"rules": []any{
				map[string]any{
					"matches":     []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": "/"}}},
					"backendRefs": refs,
				},
			},
		},
		"status": map[string]any{
			"parents": []any{
				map[string]any{"parentRef": map[string]any{"name": parent, "sectionName": "https"}, "conditions": []any{cond}},
			},
		},
	}}
}

func TestLoadHTTPRouteWeightedSplitAndParent(t *testing.T) {
	sel := map[string]string{"app": "web"}
	route := testHTTPRoute("web-route", "public", "True", "", []map[string]any{
		{"name": "web", "port": int64(80), "weight": int64(90)},
		{"name": "web-canary", "port": int64(80), "weight": int64(10)},
	})
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHTTPRoute: {route},
		kube.KindService:   {serviceWithSelector("web", "default", sel)},
		kube.KindPod:       {readyPod("web-1", "default", sel, true)},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindHTTPRoute, Namespace: "default", Name: "web-route"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready (feedback %q)", m.state, m.feedback)
	}
	if len(m.rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(m.rows))
	}
	if m.rows[0].match == "" || m.rows[0].weightPct != "90%" {
		t.Fatalf("unexpected first row: %+v", m.rows[0])
	}
	if m.rows[1].match != "" || m.rows[1].weightPct != "10%" {
		t.Fatalf("expected the second row to be a same-match continuation, got %+v", m.rows[1])
	}
	if !m.parentAttached || m.parentGatewayName != "public" {
		t.Fatalf("expected an accepted parent pointing at 'public', got attached=%v name=%q", m.parentAttached, m.parentGatewayName)
	}

	view := plain(m.Render())
	if !strings.Contains(view, "same match") {
		t.Fatalf("view missing continuation row marker:\n%s", view)
	}
}

func TestLoadHTTPRouteNotAccepted(t *testing.T) {
	route := testHTTPRoute("orphan", "public", "False", "no matching listener hostname", []map[string]any{
		{"name": "web", "port": int64(80)},
	})
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindHTTPRoute: {route}}}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindHTTPRoute, Namespace: "default", Name: "orphan"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.parentAttached {
		t.Fatalf("expected a rejected route to report parentAttached=false")
	}
	view := plain(m.Render())
	if !strings.Contains(view, "not accepted") {
		t.Fatalf("view missing rejection text:\n%s", view)
	}
}

func testGateway(name string, attachedHTTPS int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec": map[string]any{
			"gatewayClassName": "nginx",
			"listeners": []any{
				map[string]any{"name": "https", "protocol": "HTTPS", "port": int64(443), "hostname": "*.demo.local"},
				map[string]any{"name": "http", "protocol": "HTTP", "port": int64(80)},
			},
		},
		"status": map[string]any{
			"listeners": []any{
				map[string]any{"name": "https", "attachedRoutes": attachedHTTPS},
				map[string]any{"name": "http", "attachedRoutes": int64(0)},
			},
		},
	}}
}

func TestLoadGatewayListeners(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindGateway: {testGateway("public", 3)},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindGateway, Namespace: "default", Name: "public"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready (feedback %q)", m.state, m.feedback)
	}
	if len(m.listeners) != 2 {
		t.Fatalf("listeners = %d, want 2", len(m.listeners))
	}
	if m.listeners[0].name != "https" || m.listeners[0].attached != 3 {
		t.Fatalf("unexpected https listener: %+v", m.listeners[0])
	}
	if m.listeners[0].hostname != "*.demo.local" || m.listeners[1].hostname != "" {
		t.Fatalf("unexpected hostnames: %+v / %+v", m.listeners[0], m.listeners[1])
	}

	// The hostname-less listener renders as "*" but, loaded through the
	// real loader, ↵ still filters on this Gateway rather than on "*".
	m.selected = 1
	if filter, ok := m.selectedListenerRouteFilter(); !ok || filter != "gw/public" {
		t.Fatalf("filter = %q, ok=%v, want \"gw/public\", true", filter, ok)
	}
	if view := plain(m.Render()); !strings.Contains(view, "HTTP:80") || !strings.Contains(view, " * ") {
		t.Fatalf("view should render the hostname-less listener as *:\n%s", view)
	}
}

// neverStartedBackendLister simulates Service/Pod informers that were never
// started because nothing on this screen reads them — the state a Gateway
// view leaves them in, since loadGateway never lists either kind.
// KindSynced reports false forever for both (mirrors *kube.Cluster's real
// answer for a typed kind whose informer was never registered — not
// stalled, just never asked for), while the viewed kind itself is synced.
type neverStartedBackendLister struct {
	lister fakeLister
}

func (l neverStartedBackendLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	return l.lister.ListRaw(ctx, kind, namespace)
}

func (l neverStartedBackendLister) KindSynced(kind kube.ResourceKind, _ string) bool {
	return kind != kube.KindService && kind != kube.KindPod
}

// TestZeroListenerGatewayDoesNotWaitOnUnreadCaches tests the zero-listener
// Gateway case. A Gateway with zero spec.listeners rows must settle into
// TaskStateEmpty. It must not retry forever, waiting on Service and Pod
// caches. loadGateway never reads Service or Pod, so those caches never
// start. The test calls applyLoaded directly instead of driving it through
// step(): step() would follow the retry command and spin for real, which is
// the exact hang this test must catch. Calling applyLoaded directly makes an
// unfixed regression fail fast instead of hanging the whole test suite.
func TestZeroListenerGatewayDoesNotWaitOnUnreadCaches(t *testing.T) {
	lister := neverStartedBackendLister{lister: fakeLister{}}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindGateway, Namespace: "default", Name: "public"})
	m.SetSize(120, 36)

	updated, cmd := m.applyLoaded(loadedMsg{flavor: flavorGateway, gatewayClass: "nginx"})
	m = *updated.(*Model)

	if m.state != tui.TaskStateEmpty {
		t.Fatalf("state = %s, want empty — a zero-listener Gateway must not wait on Service/Pod caches it never reads (feedback %q)", m.state, m.feedback)
	}
	if cmd != nil {
		t.Fatal("expected no retry scheduled once the Gateway's own cache has settled")
	}
}

func TestNotFoundRendersError(t *testing.T) {
	lister := fakeLister{}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "missing"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateError {
		t.Fatalf("state = %s, want error", m.state)
	}
	if !strings.Contains(m.feedback, "not found") {
		t.Fatalf("unexpected feedback: %q", m.feedback)
	}
}

// notYetSyncedIngressLister reports its Ingress cache as never synced —
// every read (found or not) looks like "still filling", the way the real
// cache does right after launch or mid a context switch.
type notYetSyncedIngressLister struct {
	lister fakeLister
}

func (l notYetSyncedIngressLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	return l.lister.ListRaw(ctx, kind, namespace)
}

func (l notYetSyncedIngressLister) KindSynced(kube.ResourceKind, string) bool { return false }

// TestNotFoundStaysLoadingWhileCacheSyncing pins the fix for §5 of
// docs/lazy-informers.md §5.6: findUnstructured's own "not
// found" (a plain fmt.Errorf) is indistinguishable on its face from the
// object's own cache not having filled yet — without asking KindSynced
// first, this used to render TaskStateError ("not found") for an object
// that's actually just seconds away.
func TestNotFoundStaysLoadingWhileCacheSyncing(t *testing.T) {
	lister := notYetSyncedIngressLister{lister: fakeLister{}}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "web"})
	m.SetSize(120, 36)

	updated, cmd := m.applyLoaded(loadedMsg{err: fmt.Errorf("%s %q not found", kube.KindIngress, "web")})
	m = *updated.(*Model)

	if m.state != tui.TaskStateLoading {
		t.Fatalf("state = %s, want loading — the Ingress cache hasn't synced yet", m.state)
	}
	if cmd == nil {
		t.Fatal("expected a retry command to be scheduled while the cache is still syncing")
	}
}

// forbiddenIngressLister simulates the Ingress reflector coming back
// Forbidden: reads still succeed empty (no error), KindSynced reports
// settled (the anti-hang rule), and KindForbidden carries the reason.
type forbiddenIngressLister struct {
	lister fakeLister
	err    error
}

func (l forbiddenIngressLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	return l.lister.ListRaw(ctx, kind, namespace)
}

func (l forbiddenIngressLister) KindSynced(kube.ResourceKind, string) bool { return true }

func (l forbiddenIngressLister) KindForbidden(kind kube.ResourceKind, _ string) error {
	if kind == kube.KindIngress {
		return l.err
	}
	return nil
}

// TestNotFoundRendersPermissionDeniedWhenKindIsForbidden is
// TestNotFoundStaysLoadingWhileCacheSyncing's Forbidden-path counterpart:
// a denied Ingress cache must render the permission card, not "not found".
func TestNotFoundRendersPermissionDeniedWhenKindIsForbidden(t *testing.T) {
	lister := forbiddenIngressLister{
		lister: fakeLister{},
		err:    apierrors.NewForbidden(schema.GroupResource{Resource: "ingresses"}, "", errors.New("nope")),
	}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "web"})
	m.SetSize(120, 36)

	updated, _ := m.applyLoaded(loadedMsg{err: fmt.Errorf("%s %q not found", kube.KindIngress, "web")})
	m = *updated.(*Model)

	if m.state != tui.TaskStatePermissionDenied {
		t.Fatalf("state = %s, want permission-denied — the Ingress cache is Forbidden, not empty", m.state)
	}
}

// forbiddenBackendLister simulates the Service/Pod reflectors coming back
// Forbidden while the Ingress itself resolves normally — reads still
// succeed empty (no error), KindSynced reports settled, and KindForbidden
// carries the reason for Service/Pod only.
type forbiddenBackendLister struct {
	lister fakeLister
	err    error
}

func (l forbiddenBackendLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	return l.lister.ListRaw(ctx, kind, namespace)
}

func (l forbiddenBackendLister) KindSynced(kube.ResourceKind, string) bool { return true }

func (l forbiddenBackendLister) KindForbidden(kind kube.ResourceKind, _ string) error {
	if kind == kube.KindService || kind == kube.KindPod {
		return l.err
	}
	return nil
}

// TestBackendDeniedShowsNoteWhenRowsAlreadyPresent pins §5: the parent
// Ingress's own rows render regardless of backend-resolution success
// (routeRowsFromRoute/loadIngress emit a row per rule), so rowCount() > 0
// took the applyLoaded branch that previously checked Service/Pod's own
// sync/error state not at all — every backend read the same false ✕ "not
// found" a genuinely-absent Service gets, with no signal the cache itself
// was denied.
func TestBackendDeniedShowsNoteWhenRowsAlreadyPresent(t *testing.T) {
	lister := forbiddenBackendLister{
		lister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
			kube.KindIngress: {testIngress()},
		}},
		err: apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "", errors.New("nope")),
	}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "web"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready — the Ingress itself resolved fine, only the backend caches are denied", m.state)
	}
	if len(m.rows) == 0 {
		t.Fatal("expected rows from the Ingress's own rules regardless of backend resolution")
	}
	if m.backendDeniedNote == "" {
		t.Fatal("expected backendDeniedNote to be set")
	}
	view := plain(m.Render())
	if !strings.Contains(view, "permission denied") {
		t.Fatalf("expected an inline note naming the denied backend cache:\n%s", view)
	}
}

func TestOpenSelectedEnterJumpsToBackendService(t *testing.T) {
	m := Model{flavor: flavorIngress, rows: []routeRow{{backendNS: "default", backendName: "web"}}}
	cmd, ok := m.openSelectedEnter()
	if !ok || cmd == nil {
		t.Fatalf("expected an enter jump for a resolved backend row")
	}

	m = Model{flavor: flavorIngress, rows: nil}
	if _, ok := m.openSelectedEnter(); ok {
		t.Fatalf("expected no jump with no rows")
	}
}

// TestSelectedListenerRouteFilterUsesHostnameOrFallsBackToGateway pins 23b
// (docs/design README.md:292: "↵ on a listener filters to attached routes"):
// a listener with its own hostname filters by that hostname; a wildcard
// listener (no hostname) falls back to this Gateway's own ATTACHED-cell
// text so it still narrows to this Gateway's routes rather than showing
// every HTTPRoute in the namespace.
func TestSelectedListenerRouteFilterUsesHostnameOrFallsBackToGateway(t *testing.T) {
	m := Model{flavor: flavorGateway, name: "public", listeners: []listenerRow{{name: "https", hostname: "api.example.com"}}}
	filter, ok := m.selectedListenerRouteFilter()
	if !ok || filter != "api.example.com" {
		t.Fatalf("filter = %q, ok=%v, want \"api.example.com\", true", filter, ok)
	}

	m = Model{flavor: flavorGateway, name: "public", listeners: []listenerRow{{name: "http", hostname: ""}}}
	filter, ok = m.selectedListenerRouteFilter()
	if !ok || filter != "gw/public" {
		t.Fatalf("filter = %q, ok=%v, want \"gw/public\", true (wildcard listener fallback)", filter, ok)
	}

	m = Model{flavor: flavorGateway}
	if _, ok := m.selectedListenerRouteFilter(); ok {
		t.Fatal("expected no filter with no selected listener")
	}
}

func TestOpenSelectedEnterGatewayJumpsToHTTPRouteKind(t *testing.T) {
	m := Model{flavor: flavorGateway, listeners: []listenerRow{{name: "https"}}}
	cmd, ok := m.openSelectedEnter()
	if !ok || cmd == nil {
		t.Fatalf("expected a gateway enter jump when listeners exist")
	}

	m = Model{flavor: flavorGateway}
	if _, ok := m.openSelectedEnter(); ok {
		t.Fatalf("expected no jump with no listeners")
	}
}

func TestOpenParentGatewayOnlyForRouteFlavor(t *testing.T) {
	m := Model{flavor: flavorRoute, parentGatewayNS: "default", parentGatewayName: "public"}
	if _, ok := m.openParentGateway(); !ok {
		t.Fatalf("expected a parent-gateway jump")
	}

	m = Model{flavor: flavorIngress, parentGatewayNS: "default", parentGatewayName: "public"}
	if _, ok := m.openParentGateway(); ok {
		t.Fatalf("expected no parent-gateway jump outside the route flavor")
	}

	m = Model{flavor: flavorRoute}
	if _, ok := m.openParentGateway(); ok {
		t.Fatalf("expected no parent-gateway jump with no resolved parent")
	}
}

func TestCopySelectedURLOnlyForIngressFlavor(t *testing.T) {
	m := Model{flavor: flavorIngress, rows: []routeRow{{url: "https://web.local/"}}}
	if _, ok := m.copySelectedURL(); !ok {
		t.Fatalf("expected a copy for an ingress row with a url")
	}

	m = Model{flavor: flavorRoute, rows: []routeRow{{url: ""}}}
	if _, ok := m.copySelectedURL(); ok {
		t.Fatalf("expected no copy outside the ingress flavor")
	}
}

func TestKeybarPillAndHints(t *testing.T) {
	m := Model{state: tui.TaskStateReady, flavor: flavorIngress, rows: []routeRow{{}}}
	kb := m.Keybar()
	if kb.PillText != "ROUTES" {
		t.Fatalf("PillText = %q, want ROUTES", kb.PillText)
	}

	m = Model{state: tui.TaskStateReady, flavor: flavorGateway, listeners: []listenerRow{{}}}
	kb = m.Keybar()
	if kb.PillText != "GATEWAY" {
		t.Fatalf("PillText = %q, want GATEWAY", kb.PillText)
	}
}

// TestReloadsWhenBackendCachesChange: this screen resolves every row's
// backend live through the Service and Pod caches, but handled no change
// events at all — it loaded once on Init and never again. A backend that
// changed, or a cache that only finished filling after the screen opened,
// left BACKENDS wrong until the user backed out and came back in.
func TestReloadsWhenBackendCachesChange(t *testing.T) {
	sel := map[string]string{"app": "web"}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindIngress: {testIngress()},
		kube.KindService: {serviceWithSelector("web", "default", sel)},
		kube.KindPod:     {readyPod("web-1", "default", sel, true)},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "web"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	for _, kind := range []kube.ResourceKind{kube.KindIngress, kube.KindService, kube.KindPod, kube.KindSecret, kube.KindTLSSecret} {
		_, cmd := m.Update(kube.ResourceChangedMsg{Kind: kind})
		if cmd == nil {
			t.Errorf("a %s change must reload the routing table — it resolves backends through that cache", kind)
		}
	}

	// Something unrelated must not.
	if _, cmd := m.Update(kube.ResourceChangedMsg{Kind: kube.KindCronJob}); cmd != nil {
		t.Error("a CronJob change has nothing to do with the routing table")
	}
}

// nsLister is fakeLister honouring the namespace argument, so a lookup in
// the wrong namespace misses the way a real scoped read would.
type nsLister struct {
	objs map[kube.ResourceKind][]runtime.Object
}

func (f nsLister) ListRaw(_ context.Context, kind kube.ResourceKind, ns string) ([]runtime.Object, error) {
	var out []runtime.Object
	for _, obj := range f.objs[kind] {
		if m, err := meta.Accessor(obj); err == nil && (ns == "" || m.GetNamespace() == ns) {
			out = append(out, obj)
		}
	}
	return out, nil
}

// A backendRef naming another namespace resolves there (where a
// ReferenceGrant permits it), and ↵ jumps to that namespace's Service.
func TestLoadHTTPRouteCrossNamespaceBackend(t *testing.T) {
	sel := map[string]string{"app": "api"}
	route := testHTTPRoute("shared", "public", "True", "", []map[string]any{
		{"name": "api", "namespace": "apps", "port": int64(8080)},
	})
	lister := nsLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHTTPRoute: {route},
		kube.KindService:   {serviceWithSelector("api", "apps", sel)},
		kube.KindPod:       {readyPod("api-1", "apps", sel, true)},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindHTTPRoute, Namespace: "default", Name: "shared"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(m.rows))
	}
	r := m.rows[0]
	if r.glyph != "●" || r.backendNS != "apps" || r.backendText != "apps/api:8080" || r.endpointsText != "1 ready" {
		t.Fatalf("cross-namespace backend = %+v, want ● apps/api:8080 with 1 ready", r)
	}
}

// ResolvedRefs=False/RefNotPermitted is the controller's verdict that a
// cross-namespace ref has no ReferenceGrant: the row says so even when a
// same-named Service exists in the route's own namespace.
func TestLoadHTTPRouteRefNotPermitted(t *testing.T) {
	sel := map[string]string{"app": "api"}
	route := testHTTPRoute("shared", "public", "True", "", []map[string]any{
		{"name": "api", "namespace": "apps", "port": int64(8080)},
	})
	parents, _, _ := unstructured.NestedSlice(route.Object, "status", "parents")
	parent := parents[0].(map[string]any)
	parent["conditions"] = append(parent["conditions"].([]any), map[string]any{
		"type": "ResolvedRefs", "status": "False", "reason": "RefNotPermitted",
	})
	_ = unstructured.SetNestedSlice(route.Object, parents, "status", "parents")
	lister := nsLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHTTPRoute: {route},
		kube.KindService:   {serviceWithSelector("api", "default", sel), serviceWithSelector("api", "apps", sel)},
		kube.KindPod:       {readyPod("api-1", "default", sel, true), readyPod("api-1", "apps", sel, true)},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindHTTPRoute, Namespace: "default", Name: "shared"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) != 1 || m.rows[0].glyph != "✕" || m.rows[0].endpointsText != "not permitted" {
		t.Fatalf("rows = %+v, want one ✕ not-permitted row", m.rows)
	}
}

// A non-Service backendRef is shown as-is, never looked up as a same-named
// Service, and isn't a jump target.
func TestLoadHTTPRouteNonServiceBackendIsNeutral(t *testing.T) {
	route := testHTTPRoute("mc", "public", "True", "", []map[string]any{
		{"name": "api", "kind": "ServiceImport", "group": "multicluster.x-k8s.io", "port": int64(8080)},
	})
	lister := nsLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHTTPRoute: {route},
		kube.KindService:   {serviceWithSelector("api", "default", map[string]string{"app": "api"})},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindHTTPRoute, Namespace: "default", Name: "mc"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(m.rows))
	}
	r := m.rows[0]
	if r.glyph != "·" || r.class != resources.StatusNeutral || r.backendText != "serviceimport/api:8080" {
		t.Fatalf("non-Service backend = %+v, want a neutral serviceimport/api:8080", r)
	}
	if _, ok := m.openSelectedEnter(); ok {
		t.Fatal("a non-Service backend must not jump to a Service")
	}
}

// A listener certificateRef naming another namespace reads its Secret there.
func TestLoadGatewayCrossNamespaceCertificateRef(t *testing.T) {
	gw := testGateway("public", 1)
	listeners, _, _ := unstructured.NestedSlice(gw.Object, "spec", "listeners")
	listeners[0].(map[string]any)["tls"] = map[string]any{
		"certificateRefs": []any{map[string]any{"name": "wild-tls", "namespace": "certs"}},
	}
	_ = unstructured.SetNestedSlice(gw.Object, listeners, "spec", "listeners")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wild-tls", Namespace: "certs"},
		Data:       map[string][]byte{"tls.crt": demoCertPEM(t, time.Now().Add(61*24*time.Hour+time.Hour))},
	}
	lister := nsLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindGateway: {gw},
		kube.KindSecret:  {secret},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindGateway, Namespace: "default", Name: "public"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.listeners) == 0 || m.listeners[0].tlsText != "expires in 61d" {
		t.Fatalf("listeners = %+v, want the https listener's cert read from certs/wild-tls", m.listeners)
	}
}

// secretGateLister answers every read from lister, with the Secret cache's
// own sync/error state set per test.
type secretGateLister struct {
	lister       fakeLister
	secretSynced bool
	secretErr    error
}

func (l secretGateLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	return l.lister.ListRaw(ctx, kind, namespace)
}

func (l secretGateLister) KindSynced(kind kube.ResourceKind, _ string) bool {
	return kind != kube.KindSecret || l.secretSynced
}

func (l secretGateLister) KindError(kind kube.ResourceKind, _ string) error {
	if kind == kube.KindSecret {
		return l.secretErr
	}
	return nil
}

func tlsIngress() *networkingv1.Ingress {
	ing := testIngress()
	ing.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{"web.local"}, SecretName: "web-tls"}}
	return ing
}

// A TLS Secret missing from a Forbidden Secret cache proves nothing about
// the cluster: the fact says the Secret can't be read, neutral, never a red
// "secret not found".
func TestIngressTLSFactOnForbiddenSecretsIsNotNotFound(t *testing.T) {
	lister := secretGateLister{
		lister:       fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindIngress: {tlsIngress()}}},
		secretSynced: true,
		secretErr:    apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("nope")),
	}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "web"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.tlsFacts) == 0 {
		t.Fatal("expected a TLS fact")
	}
	if f := m.tlsFacts[0]; f.expiry != "secrets: permission denied" || f.class != resources.StatusNeutral {
		t.Fatalf("tls fact = %+v, want a neutral permission-denied fact", f)
	}
}

// A Secret cache still filling reads as loading and schedules a retry —
// a Secret that genuinely isn't there syncs without a change event.
func TestIngressTLSFactWhileSecretsFillAsksForRetry(t *testing.T) {
	lister := secretGateLister{
		lister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindIngress: {tlsIngress()}}},
	}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "web"})
	m.SetSize(120, 36)
	msg := m.load()()
	updated, cmd := m.Update(msg)
	m = *updated.(*Model)

	if len(m.tlsFacts) == 0 || m.tlsFacts[0].expiry != "loading…" {
		t.Fatalf("tls facts = %+v, want a loading fact", m.tlsFacts)
	}
	if cmd == nil {
		t.Fatal("expected a cache-sync retry while the Secret cache fills")
	}
}

// A defaultBackend-only Ingress routes everything to one backend: a
// "* (default)" row, never the "no routes" empty state.
func TestLoadIngressDefaultBackendOnly(t *testing.T) {
	sel := map[string]string{"app": "web"}
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "catch-all", Namespace: "default"},
		Spec: networkingv1.IngressSpec{DefaultBackend: &networkingv1.IngressBackend{
			Service: &networkingv1.IngressServiceBackend{Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80}},
		}},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindIngress: {ing},
		kube.KindService: {serviceWithSelector("web", "default", sel)},
		kube.KindPod:     {readyPod("web-1", "default", sel, true)},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "catch-all"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady || len(m.rows) != 1 {
		t.Fatalf("state = %s, rows = %+v, want one ready row", m.state, m.rows)
	}
	if r := m.rows[0]; r.match != "* (default)" || r.glyph != "●" || r.backendText != "web:80" {
		t.Fatalf("default backend row = %+v", r)
	}
}

// A resource backend is shown as-is, neutral, and never looked up as a
// Service.
func TestLoadIngressResourceBackendIsNeutral(t *testing.T) {
	apiGroup := "k8s.example.com"
	pathType := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "static", Namespace: "default"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: "static.local",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &pathType, Backend: networkingv1.IngressBackend{
					Resource: &corev1.TypedLocalObjectReference{APIGroup: &apiGroup, Kind: "StorageBucket", Name: "assets"},
				}}},
			}},
		}}},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindIngress: {ing}}}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "static"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) != 1 {
		t.Fatalf("rows = %+v, want one", m.rows)
	}
	if r := m.rows[0]; r.glyph != "·" || r.backendText != "storagebucket/assets" || r.backendName != "" {
		t.Fatalf("resource backend row = %+v", r)
	}
}

// tlsCacheLister is a lister with the narrow TLS Secret seam: Secrets come
// from ListTLSSecrets, and any read of the shared Secret cache is recorded —
// on a real cluster that read starts the cluster-wide, full-payload Secret
// informer (11 M8). The cache gates record which (kind, namespace) they were
// asked about.
type tlsCacheLister struct {
	lister    fakeLister
	tls       []runtime.Object
	tlsSynced bool
	log       *tlsCacheLog
}

type tlsCacheLog struct {
	sharedSecretReads int
	tlsReads          []string
	synced            []string
}

func (l tlsCacheLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	if kind == kube.KindSecret {
		l.log.sharedSecretReads++
	}
	return l.lister.ListRaw(ctx, kind, namespace)
}

func (l tlsCacheLister) ListTLSSecrets(_ context.Context, namespace string) ([]runtime.Object, error) {
	l.log.tlsReads = append(l.log.tlsReads, namespace)
	return l.tls, nil
}

func (l tlsCacheLister) KindSynced(kind kube.ResourceKind, namespace string) bool {
	if kind == kube.KindSecret || kind == kube.KindTLSSecret {
		l.log.synced = append(l.log.synced, string(kind)+"@"+namespace)
	}
	return kind != kube.KindTLSSecret || l.tlsSynced
}

// Opening a TLS Ingress resolves its cert expiry through the filtered TLS
// Secret cache, in the Ingress's own namespace, and never reads the shared
// Secret cache.
func TestIngressTLSExpiryReadsTheTLSSecretCacheNotEverySecret(t *testing.T) {
	log := &tlsCacheLog{}
	lister := tlsCacheLister{
		lister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindIngress: {tlsIngress()}}},
		tls: []runtime.Object{&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "web-tls", Namespace: "default"},
			Type:       corev1.SecretTypeTLS,
		}},
		tlsSynced: true,
		log:       log,
	}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "web"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if log.sharedSecretReads != 0 {
		t.Fatalf("read the shared Secret cache %d time(s); a TLS cell must use ListTLSSecrets", log.sharedSecretReads)
	}
	if len(log.tlsReads) == 0 || log.tlsReads[0] != "default" {
		t.Fatalf("TLS reads = %v, want reads of \"default\"", log.tlsReads)
	}
	if len(m.tlsFacts) != 1 || m.tlsFacts[0].expiry != "no cert data" {
		t.Fatalf("tls facts = %+v, want the Secret found through the TLS cache", m.tlsFacts)
	}
}

// A miss in the TLS cache is gated on that cache, at the namespace read —
// never on KindSecret, which answers for a cache this path leaves unstarted.
func TestIngressTLSMissGatesOnTheTLSCacheItRead(t *testing.T) {
	log := &tlsCacheLog{}
	lister := tlsCacheLister{
		lister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindIngress: {tlsIngress()}}},
		log:    log,
	}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "web"})
	m.SetSize(120, 36)
	updated, _ := m.Update(m.load()())
	m = *updated.(*Model)

	if len(m.tlsFacts) == 0 || m.tlsFacts[0].expiry != "loading…" {
		t.Fatalf("tls facts = %+v, want loading while the TLS cache fills", m.tlsFacts)
	}
	for _, asked := range log.synced {
		if asked != string(kube.KindTLSSecret)+"@default" {
			t.Fatalf("cache gate asked about %s, want only %s@default", asked, kube.KindTLSSecret)
		}
	}
	if len(log.synced) == 0 {
		t.Fatal("the TLS miss was not gated on any cache")
	}
}

// startedTLSCacheLister is tlsCacheLister that can also say whether the
// shared Secret cache is already running (tui.KindStartedChecker).
type startedTLSCacheLister struct {
	tlsCacheLister
	sharedStarted bool
}

func (l startedTLSCacheLister) KindStarted(kind kube.ResourceKind, _ string) bool {
	return kind != kube.KindSecret || l.sharedStarted
}

func opaqueCertSecret(t *testing.T) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "web-tls", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"tls.crt": demoCertPEM(t, time.Now().Add(90*24*time.Hour))},
	}
}

// An Opaque Secret carrying tls.crt, invisible to the kubernetes.io/tls
// cache, is still found when the shared Secret cache is already running.
func TestIngressTLSFindsOpaqueSecretInStartedSharedCache(t *testing.T) {
	log := &tlsCacheLog{}
	lister := startedTLSCacheLister{
		tlsCacheLister: tlsCacheLister{
			lister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
				kube.KindIngress: {tlsIngress()},
				kube.KindSecret:  {opaqueCertSecret(t)},
			}},
			tlsSynced: true,
			log:       log,
		},
		sharedStarted: true,
	}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "web"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.tlsFacts) != 1 || !strings.HasPrefix(m.tlsFacts[0].expiry, "expires in ") {
		t.Fatalf("tls facts = %+v, want the Opaque Secret's cert expiry", m.tlsFacts)
	}
}

// When the shared Secret cache isn't running, a TLS-cache miss never reads
// it, and says only kubernetes.io/tls Secrets were checked.
func TestIngressTLSMissNeverStartsSharedSecretCache(t *testing.T) {
	log := &tlsCacheLog{}
	lister := startedTLSCacheLister{
		tlsCacheLister: tlsCacheLister{
			lister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
				kube.KindIngress: {tlsIngress()},
				kube.KindSecret:  {opaqueCertSecret(t)},
			}},
			tlsSynced: true,
			log:       log,
		},
	}
	m := New(Config{Session: newSession(), Lister: lister, Kind: kube.KindIngress, Namespace: "default", Name: "web"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if log.sharedSecretReads != 0 {
		t.Fatalf("read the shared Secret cache %d time(s) while it was not started", log.sharedSecretReads)
	}
	if len(m.tlsFacts) != 1 || m.tlsFacts[0].expiry != "no kubernetes.io/tls secret" {
		t.Fatalf("tls facts = %+v, want a miss naming the type checked", m.tlsFacts)
	}
}
