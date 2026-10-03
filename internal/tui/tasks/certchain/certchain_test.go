package certchain

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/kube/fake"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

func demoModel(t *testing.T, namespace, name string) Model {
	t.Helper()
	c := fake.NewDemo()
	sess := &tui.Session{Theme: tui.Dark(), Location: tui.Location{Context: "microk8s-cluster", Namespace: namespace}}
	m := New(Config{Session: sess, Lister: c, Namespace: namespace, Name: name})
	m.SetSize(120, 30)
	upd, _ := m.Update(m.load()())
	return *upd.(*Model)
}

func plain(s string) string {
	var b strings.Builder
	skip := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			skip = true
		case skip && r == 'm':
			skip = false
		case !skip:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestFailingChainPromotesTheDeepestFailure is §35a's headline: the chain
// walks Certificate → CertificateRequest → Order → Challenge and promotes
// the Challenge's own message verbatim, the deepest real failure, not the
// Certificate's own "Ready=False" or the Order's "errored".
func TestFailingChainPromotesTheDeepestFailure(t *testing.T) {
	view := plain(demoModel(t, "default", "web-tls").Render())

	if !strings.Contains(view, "challenge failed") {
		t.Errorf("expected the failure card naming the Challenge:\n%s", view)
	}
	if !strings.Contains(view, "propagation check failed: NXDOMAIN looking up TXT _acme-challenge.app.nva.dev") {
		t.Errorf("the Challenge's status.reason must render verbatim:\n%s", view)
	}
	if !strings.Contains(view, "dns-01 · app.nva.dev") {
		t.Errorf("expected the Challenge's own type/dnsName detail line:\n%s", view)
	}
	if !strings.Contains(view, "order/web-tls-1-2847563921") {
		t.Errorf("expected the failure card to reference its parent Order:\n%s", view)
	}
	if !strings.Contains(view, "attempt 4") {
		t.Errorf("expected the honest attempt count (4 seeded CertificateRequests):\n%s", view)
	}
	if !strings.Contains(view, "message verbatim from challenge status · deepest failure in the chain, found for you") {
		t.Errorf("expected the caption naming the failing kind:\n%s", view)
	}
	if strings.Contains(view, "next retry") {
		t.Errorf("cert-manager exposes no retry/backoff timing on any chain kind — a countdown here would be invented:\n%s", view)
	}
	// The four chain rows, in order.
	for _, want := range []string{"certificate/web-tls", "certificaterequest/web-tls-1", "order/web-tls-1-2847563921", "challenge/web-tls-1-2847563921-0"} {
		if !strings.Contains(view, want) {
			t.Errorf("expected chain row %q:\n%s", want, view)
		}
	}
}

// TestFailingChainCursorDefaultsToTheFailure: "zero digs" — ↵ with no
// navigation should land on the object the failure card already named.
func TestFailingChainCursorDefaultsToTheFailure(t *testing.T) {
	m := demoModel(t, "default", "web-tls")
	cmd := m.updateKeyForTest(t, "enter")
	msg, ok := cmd().(tui.GotoResourceMsg)
	if !ok {
		t.Fatalf("expected GotoResourceMsg, got %T", cmd())
	}
	if msg.Kind != kube.KindChallenge || msg.Name != "web-tls-1-2847563921-0" {
		t.Errorf("↵ should default to the failing Challenge, got %+v", msg)
	}
}

// updateKeyForTest is a small test-only helper so the enter-key assertions
// above don't need tea.KeyPressMsg boilerplate repeated at each call site.
func (m Model) updateKeyForTest(t *testing.T, key string) tea.Cmd {
	t.Helper()
	_, cmd := (&m).updateKey(tea.KeyPressMsg{Text: key})
	if cmd == nil {
		t.Fatalf("key %q produced no cmd", key)
	}
	return cmd
}

// TestHealthyCertificateHasNoChrome: "zero chrome until earned" — a Ready
// Certificate with a Ready CertificateRequest gets no failure banner.
func TestHealthyCertificateHasNoChrome(t *testing.T) {
	view := plain(demoModel(t, "default", "api-tls").Render())
	if strings.Contains(view, tui.GlyphFailed+" ") && strings.Contains(view, "failed") {
		t.Errorf("a healthy chain must carry no failure banner:\n%s", view)
	}
	if !strings.Contains(view, "certificate/api-tls") || !strings.Contains(view, "certificaterequest/api-tls-abcd1") {
		t.Errorf("expected both healthy chain rows:\n%s", view)
	}
}

// TestDegradedChainStopsAtCertificate covers §35a's "for non-ACME issuers
// (or before any CertificateRequest exists yet) the chain is just two rows —
// the recipe degrades, no special casing" — here it degrades to one, since
// staging-tls has no CertificateRequest fixture at all. A bare Ready=False
// with no reason is in-flight progress, not failure, so it still earns no
// card.
func TestDegradedChainStopsAtCertificate(t *testing.T) {
	m := demoModel(t, "staging", "staging-tls")
	if len(m.chain) != 1 {
		t.Fatalf("expected a 1-row chain (Certificate only), got %d: %+v", len(m.chain), m.chain)
	}
	if m.fail != nil {
		t.Errorf("a bare Ready=False with no reason must not be reported as a failure, got %+v", m.fail)
	}
}

// TestRefsStripReportsMissingSecretAndReadyIssuer exercises §35a's bottom
// band: the target Secret was deliberately never seeded (missing, red) and
// the ClusterIssuer it names is Ready (green).
func TestRefsStripReportsMissingSecretAndReadyIssuer(t *testing.T) {
	m := demoModel(t, "default", "web-tls")
	if !m.haveSecret || m.secretRef.Exists {
		t.Errorf("expected the web-tls-cert Secret to be reported missing, got %+v", m.secretRef)
	}
	if !m.haveIssuer || !m.issuerRef.Exists || !m.issuerRef.Ready {
		t.Errorf("expected letsencrypt-prod to resolve Ready, got %+v", m.issuerRef)
	}
	view := plain(m.Render())
	if !strings.Contains(view, "secret/web-tls-cert") || !strings.Contains(view, "missing") {
		t.Errorf("expected the missing-secret ref line:\n%s", view)
	}
	if !strings.Contains(view, "clusterissuer/letsencrypt-prod") || !strings.Contains(view, "Ready") {
		t.Errorf("expected the ready-issuer ref line:\n%s", view)
	}
}

// TestReloadsOnIsNarrow guards the lazy-informer rule: a change to a kind
// this chain never resolved must not trigger a reload.
func TestReloadsOnIsNarrow(t *testing.T) {
	m := demoModel(t, "default", "web-tls")
	for _, kind := range []kube.ResourceKind{kube.KindCertificate, kube.KindCertificateRequest, kube.KindOrder, kube.KindChallenge, kube.KindSecret, kube.KindClusterIssuer} {
		if !m.reloadsOn(kind) {
			t.Errorf("expected reloadsOn(%s) = true for a kind in this chain", kind)
		}
	}
	for _, kind := range []kube.ResourceKind{kube.KindPod, kube.KindDeployment, kube.KindIssuer} {
		if m.reloadsOn(kind) {
			t.Errorf("expected reloadsOn(%s) = false — not part of this chain", kind)
		}
	}
}

// recordingLister answers ListRaw from objs and records which kind/namespace
// pairs were read.
type recordingLister struct {
	objs  map[kube.ResourceKind][]runtime.Object
	reads []string
}

func (l *recordingLister) ListRaw(_ context.Context, kind kube.ResourceKind, ns string) ([]runtime.Object, error) {
	l.reads = append(l.reads, string(kind)+"/"+ns)
	return l.objs[kind], nil
}

func certWithIssuerRef(ref map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "web-tls", "namespace": "shop"},
		"spec":     map[string]any{"issuerRef": ref},
	}}
}

// cert-manager defaults an omitted issuerRef.kind to Issuer in the
// Certificate's own namespace, not a ClusterIssuer.
func TestIssuerRefKindDefaultsToNamespacedIssuer(t *testing.T) {
	issuer := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "local-ca", "namespace": "shop"},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "True"},
		}},
	}}
	lister := &recordingLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindIssuer: {issuer}}}
	ref, ok := resolveIssuerRef(t.Context(), lister, "shop", certWithIssuerRef(map[string]any{"name": "local-ca"}))
	if !ok || ref.Kind != kube.KindIssuer || !ref.Exists || !ref.Ready || ref.Label != "issuer/local-ca" {
		t.Fatalf("resolveIssuerRef = %+v, want a Ready issuer/local-ca", ref)
	}
	if len(lister.reads) != 1 || lister.reads[0] != "Issuer/shop" {
		t.Fatalf("reads = %v, want only Issuer/shop", lister.reads)
	}
}

// An issuerRef into an external issuer's group is shown as-is and never
// read as a ClusterIssuer, which it isn't.
func TestIssuerRefExternalGroupIsNeutralAndUnread(t *testing.T) {
	lister := &recordingLister{}
	ref, ok := resolveIssuerRef(t.Context(), lister, "shop", certWithIssuerRef(map[string]any{
		"name": "pca", "kind": "AWSPCAClusterIssuer", "group": "awspca.cert-manager.io",
	}))
	if !ok || !ref.External || ref.Kind != "" || ref.Label != "awspcaclusterissuer/pca" {
		t.Fatalf("resolveIssuerRef = %+v, want an external awspcaclusterissuer/pca", ref)
	}
	if len(lister.reads) != 0 {
		t.Fatalf("reads = %v, want none for an external issuer", lister.reads)
	}
	m := Model{issuerRef: ref, haveIssuer: true}
	if _, _, ok := m.selectedTarget(); ok {
		t.Fatal("an external issuer ref must not be a jump target")
	}
}

// gatedLister wraps the demo cluster with per-kind cache state: kinds in
// unsynced haven't filled, kinds in errs stopped with that error.
type gatedLister struct {
	resources.RawLister
	unsynced map[kube.ResourceKind]bool
	errs     map[kube.ResourceKind]error
}

func (l gatedLister) KindSynced(kind kube.ResourceKind, _ string) bool { return !l.unsynced[kind] }
func (l gatedLister) KindError(kind kube.ResourceKind, _ string) error { return l.errs[kind] }

// gatedModel loads web-tls from the demo cluster through those gates, empty
// lists for every gated kind (what a Forbidden or unfilled cache returns).
func gatedModel(t *testing.T, unsynced map[kube.ResourceKind]bool, errs map[kube.ResourceKind]error) (Model, tea.Cmd) {
	t.Helper()
	base := fake.NewDemo()
	lister := gatedLister{RawLister: emptyFor{base, unsynced, errs}, unsynced: unsynced, errs: errs}
	sess := &tui.Session{Theme: tui.Dark(), Location: tui.Location{Context: "microk8s-cluster", Namespace: "default"}}
	m := New(Config{Session: sess, Lister: lister, Namespace: "default", Name: "web-tls"})
	m.SetSize(120, 30)
	upd, cmd := m.Update(m.load()())
	return *upd.(*Model), cmd
}

type emptyFor struct {
	base     resources.RawLister
	unsynced map[kube.ResourceKind]bool
	errs     map[kube.ResourceKind]error
}

func (e emptyFor) ListRaw(ctx context.Context, kind kube.ResourceKind, ns string) ([]runtime.Object, error) {
	if _, gated := e.errs[kind]; gated || e.unsynced[kind] {
		return nil, nil // a gated cache reads empty, error-free, like the real one
	}
	return e.base.ListRaw(ctx, kind, ns)
}

// A Forbidden CertificateRequest cache must not pass for a healthy
// single-node chain: the screen says the chain may be incomplete, and why.
func TestForbiddenChainKindIsSurfaced(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "certificaterequests"}, "", errors.New("nope"))
	m, _ := gatedModel(t, nil, map[kube.ResourceKind]error{kube.KindCertificateRequest: forbidden})
	if !strings.Contains(m.chainNote, "permission denied") {
		t.Fatalf("chainNote = %q, want a permission-denied note", m.chainNote)
	}
	if view := plain(m.Render()); !strings.Contains(view, "chain may be incomplete") {
		t.Fatalf("view missing the incomplete-chain note:\n%s", view)
	}
}

// A Secret or issuer missing from an unreadable cache is unknown, not
// "missing"; one missing from a cache still filling retries.
func TestRefsOnUnreadableCachesAreNotMissing(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("nope"))
	m, _ := gatedModel(t, nil, map[kube.ResourceKind]error{kube.KindSecret: forbidden})
	if !m.secretRef.Unknown || m.secretRef.StatusText != "permission denied" {
		t.Fatalf("secretRef = %+v, want an unknown permission-denied ref", m.secretRef)
	}

	m, cmd := gatedModel(t, map[kube.ResourceKind]bool{kube.KindClusterIssuer: true}, nil)
	if !m.issuerRef.Unknown || m.issuerRef.StatusText != "loading…" {
		t.Fatalf("issuerRef = %+v, want an unknown loading ref", m.issuerRef)
	}
	if cmd == nil {
		t.Fatal("expected a cache-sync retry while the ClusterIssuer cache fills")
	}
}

// tlsRecordingLister is recordingLister with the narrow TLS Secret seam, and
// records the cache gates it is asked.
type tlsRecordingLister struct {
	recordingLister
	tls       []runtime.Object
	tlsSynced bool
	tlsReads  []string
	gates     []string
}

func (l *tlsRecordingLister) ListTLSSecrets(_ context.Context, ns string) ([]runtime.Object, error) {
	l.tlsReads = append(l.tlsReads, ns)
	return l.tls, nil
}

func (l *tlsRecordingLister) KindSynced(kind kube.ResourceKind, ns string) bool {
	l.gates = append(l.gates, string(kind)+"/"+ns)
	return kind != kube.KindTLSSecret || l.tlsSynced
}

func certWithSecretName(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "web-tls", "namespace": "shop"},
		"spec":     map[string]any{"secretName": name},
	}}
}

// The Secret ref is resolved through the filtered TLS Secret cache in the
// Certificate's namespace, never the shared Secret cache (11 M8), and still
// jumps to a Secret.
func TestSecretRefReadsTheTLSSecretCacheNotEverySecret(t *testing.T) {
	secret := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "web-tls-cert", "namespace": "shop"},
	}}
	lister := &tlsRecordingLister{tls: []runtime.Object{secret}, tlsSynced: true}
	ref, ok := resolveSecretRef(t.Context(), lister, "shop", certWithSecretName("web-tls-cert"))
	if !ok || !ref.Exists || ref.Kind != kube.KindSecret {
		t.Fatalf("resolveSecretRef = %+v, want an existing Secret ref", ref)
	}
	if len(lister.reads) != 0 {
		t.Fatalf("ListRaw reads = %v, want none — the shared Secret cache must stay unstarted", lister.reads)
	}
	if len(lister.tlsReads) != 1 || lister.tlsReads[0] != "shop" {
		t.Fatalf("TLS reads = %v, want one read of shop", lister.tlsReads)
	}
}

// A miss is gated on the TLS cache at the namespace read, not on KindSecret.
func TestSecretRefMissGatesOnTheTLSCacheItRead(t *testing.T) {
	lister := &tlsRecordingLister{}
	ref, _ := resolveSecretRef(t.Context(), lister, "shop", certWithSecretName("web-tls-cert"))
	if !ref.Unknown || ref.StatusText != "loading…" {
		t.Fatalf("resolveSecretRef = %+v, want loading while the TLS cache fills", ref)
	}
	if len(lister.gates) != 1 || lister.gates[0] != string(kube.KindTLSSecret)+"/shop" {
		t.Fatalf("cache gates = %v, want only %s/shop", lister.gates, kube.KindTLSSecret)
	}
	m := Model{secretRef: ref, haveSecret: true}
	if !m.reloadsOn(kube.KindTLSSecret) {
		t.Fatal("a TLS Secret cache change must reload the chain")
	}
}
