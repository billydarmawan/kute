package fluxdetail

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/kube/fake"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

func demoModel(t *testing.T, name string) Model {
	t.Helper()
	c := fake.NewDemo()
	reg, groups := resources.BuildDiscoveredRegistry(c.DiscoveredKinds(), c)
	sess := &tui.Session{Theme: tui.Dark(), Registry: reg, Groups: groups,
		Location: tui.Location{Context: "microk8s-cluster", Namespace: "flux-system"}}
	m := New(Config{Session: sess, Lister: c, Mutator: c,
		Kind: "Kustomization", Namespace: "flux-system", Name: name})
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

// TestFailureCardPromotesTheReasonAndTheCulprit is §31a's headline: the
// condition message verbatim, plus the object the health check was actually
// waiting on, without the user digging for it.
func TestFailureCardPromotesTheReasonAndTheCulprit(t *testing.T) {
	view := plain(demoModel(t, "nebula-workers").Render())

	if !strings.Contains(view, "RECONCILE FAILED") {
		t.Errorf("expected the failure card:\n%s", view)
	}
	if !strings.Contains(view, "health check failed after 2m0s") {
		t.Errorf("the condition message must render verbatim:\n%s", view)
	}
	if !strings.Contains(view, "Deployment/nebula-worker") {
		t.Errorf("expected the drill-through to name the failing workload:\n%s", view)
	}
	// The count is the fact an operator acts on, resolved live from the
	// workload rather than restated from the Flux message.
	if !strings.Contains(view, "3/4") {
		t.Errorf("expected the workload's live replica count:\n%s", view)
	}
}

// TestAppliedButUnhealthyIsDistinctFromDrift pins the one line §31a says
// kills the most common Flux misdiagnosis.
func TestAppliedButUnhealthyIsDistinctFromDrift(t *testing.T) {
	failing := plain(demoModel(t, "nebula-workers").Render())
	if !strings.Contains(failing, "in sync") || !strings.Contains(failing, "applied, failing health checks") {
		t.Errorf("a failing-but-applied reconciler must say so:\n%s", failing)
	}

	// A suspended one that has genuinely drifted reports the other family.
	drifted := plain(demoModel(t, "nebula-infra").Render())
	if !strings.Contains(drifted, "source ahead") {
		t.Errorf("a drifted reconciler must report drift, not health:\n%s", drifted)
	}
	if strings.Contains(drifted, "applied, failing health checks") {
		t.Errorf("a drifted reconciler must not claim a health failure:\n%s", drifted)
	}
}

// TestSuspendedShowsPausedNotFailed guards §30a's rule carried into detail:
// a paused reconciler is not a broken one.
func TestSuspendedShowsPausedNotFailed(t *testing.T) {
	view := plain(demoModel(t, "nebula-infra").Render())
	if !strings.Contains(view, "SUSPENDED") {
		t.Errorf("expected the suspended card:\n%s", view)
	}
	if strings.Contains(view, "RECONCILE FAILED") {
		t.Errorf("a suspended reconciler must not be reported as failed:\n%s", view)
	}
	if !strings.Contains(view, "resume") {
		t.Errorf("the keybar should offer resume on a suspended object:\n%s", view)
	}
}

// TestHealthyReconcilerHasNoCard: zero chrome when there is nothing wrong.
func TestHealthyReconcilerHasNoCard(t *testing.T) {
	view := plain(demoModel(t, "flux-system").Render())
	if strings.Contains(view, "RECONCILE FAILED") || strings.Contains(view, "SUSPENDED") {
		t.Errorf("a healthy reconciler should carry no card:\n%s", view)
	}
	if !strings.Contains(view, "in sync") {
		t.Errorf("expected the in-sync marker:\n%s", view)
	}
}

// TestInventoryFoldsHealthyTailAndOpensTheCulprit covers the third band:
// trouble first, the rest folded, and ↵ landing on the object under the
// cursor — which after the sort is the failing one.
func TestInventoryFoldsHealthyTailAndOpensTheCulprit(t *testing.T) {
	m := demoModel(t, "nebula-workers")
	view := plain(m.Render())
	if !strings.Contains(view, "1 NOT READY") {
		t.Errorf("expected the not-ready count in the inventory header:\n%s", view)
	}
	// The folded rows are kinds with no readiness signal: folded, but
	// never claimed ready.
	if !strings.Contains(view, "+ 2 no reading") {
		t.Errorf("expected the tail folded without claiming it ready:\n%s", view)
	}

	upd, cmd := m.Update(tea.KeyPressMsg{Text: "enter"})
	if cmd == nil {
		t.Fatal("expected ↵ to emit a goto cmd")
	}
	msg, ok := cmd().(tui.GotoResourceMsg)
	if !ok {
		t.Fatalf("expected GotoResourceMsg, got %T", cmd())
	}
	if msg.Kind != kube.KindDeployment || msg.Name != "nebula-worker" {
		t.Errorf("↵ should open the failing workload, got %+v", msg)
	}

	// tab reveals the rest.
	m2 := *upd.(*Model)
	m3, _ := m2.Update(tea.KeyPressMsg{Text: "tab"})
	if !strings.Contains(plain(m3.(*Model).Render()), "nebula-worker-config") {
		t.Errorf("tab should expand the healthy tail")
	}
}

func TestInventoryScrollsAndSupportsHalfPageKeys(t *testing.T) {
	m := demoModel(t, "nebula-workers")
	m.invExpanded = true
	m.inventory = make([]inventoryItem, 20)
	for i := range m.inventory {
		m.inventory[i] = inventoryItem{
			Kind:   kube.KindDeployment,
			Name:   fmt.Sprintf("managed-%02d", i),
			Ready:  "1/1",
			Status: resources.StatusOK,
			Glyph:  tui.GlyphRunning,
		}
	}
	m.selected, m.offset = 0, 0

	rows := m.inventoryViewportRows()
	if rows >= len(m.inventory) {
		t.Fatalf("inventory viewport rows = %d, want less than %d", rows, len(m.inventory))
	}
	if view := plain(m.Render()); !strings.Contains(view, fmt.Sprintf("1–%d of %d", rows, len(m.inventory))) {
		t.Fatalf("initial inventory page summary missing:\n%s", view)
	}

	for i := 0; i < len(m.inventory)-1; i++ {
		m.Update(tea.KeyPressMsg{Text: "j"})
	}
	if m.selected != len(m.inventory)-1 {
		t.Fatalf("selected = %d, want last inventory item", m.selected)
	}
	if m.offset == 0 {
		t.Fatal("offset stayed at zero while moving to the last inventory item")
	}
	if view := plain(m.Render()); !strings.Contains(view, "managed-19") {
		t.Fatalf("last inventory item is not rendered after scrolling:\n%s", view)
	}
	if view := plain(m.Render()); !strings.Contains(view, fmt.Sprintf("%d–%d of %d", len(m.inventory)-rows+1, len(m.inventory), len(m.inventory))) {
		t.Fatalf("scrolled inventory page summary missing:\n%s", view)
	}

	before := m.selected
	m.Update(tea.KeyPressMsg{Text: "ctrl+u"})
	if m.selected >= before {
		t.Fatalf("ctrl+u selected = %d, want less than %d", m.selected, before)
	}
	before = m.selected
	m.Update(tea.KeyPressMsg{Text: "ctrl+d"})
	if m.selected <= before {
		t.Fatalf("ctrl+d selected = %d, want greater than %d", m.selected, before)
	}
}

// TestHelmReleaseSaysWhyItHasNoInventory — a HelmRelease delegates to
// Helm's own storage, and saying so beats an empty list that reads as
// "this manages nothing".
func TestHelmReleaseSaysWhyItHasNoInventory(t *testing.T) {
	c := fake.NewDemo()
	reg, groups := resources.BuildDiscoveredRegistry(c.DiscoveredKinds(), c)
	sess := &tui.Session{Theme: tui.Dark(), Registry: reg, Groups: groups,
		Location: tui.Location{Context: "microk8s-cluster", Namespace: "flux-system"}}
	m := New(Config{Session: sess, Lister: c, Mutator: c,
		Kind: kube.KindFluxHelmRelease, Namespace: "flux-system", Name: "nebula-redis"})
	m.SetSize(120, 30)
	upd, _ := m.Update(m.load()())
	view := plain(upd.(*Model).Render())
	if !strings.Contains(view, "inventory not published by this kind") {
		t.Errorf("expected the explicit no-inventory note:\n%s", view)
	}
}

// TestInventoryStatusIsAGlyphNotJustAColour: every branch of
// objectReadiness used to answer "●" and let the foreground colour carry
// ready-vs-not, which the app's own terminal-degradation path defeats — a
// red dot and a green dot are the same character once the 256-colour or
// no-colour fallback flattens the palette (CLAUDE.md, terminal capability
// degradation). The shape has to carry the fact.
func TestInventoryStatusIsAGlyphNotJustAColour(t *testing.T) {
	four := int32(4)
	tests := []struct {
		name      string
		obj       runtime.Object
		wantReady string
		wantGlyph string
	}{
		{"deployment fully ready", &appsv1.Deployment{
			Spec:   appsv1.DeploymentSpec{Replicas: &four},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 4},
		}, "4/4", tui.GlyphRunning},
		{"deployment short of its replicas", &appsv1.Deployment{
			Spec:   appsv1.DeploymentSpec{Replicas: &four},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 3},
		}, "3/4", tui.GlyphFailed},
		{"running pod", &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, "running", tui.GlyphRunning},
		{"failed pod", &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}}, "failed", tui.GlyphFailed},
		// An object kute has no readiness reading for renders "·", never
		// the generic Neutral "○" — ○ means *completed* in the pod
		// vocabulary, and this object may be doing anything at all.
		{"no readiness to read", &corev1.ConfigMap{}, "–", "·"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ready, class := objectReadiness(tc.obj)
			if ready != tc.wantReady {
				t.Errorf("ready = %q, want %q", ready, tc.wantReady)
			}
			if got := inventoryGlyph(class); got != tc.wantGlyph {
				t.Errorf("glyph = %q, want %q", got, tc.wantGlyph)
			}
		})
	}

	// And the rendered band carries it, distinguishable with colour stripped.
	view := plain(demoModel(t, "nebula-workers").Render())
	if !strings.Contains(view, tui.GlyphFailed+" nebula-worker") {
		t.Errorf("expected the failing workload to render %s in the inventory:\n%s", tui.GlyphFailed, view)
	}
}

// TestHelmReleaseNeverClaimsDrift guards the chain grid's own version of
// the comparison §30b's tree makes. A HelmRelease records a chart version
// ("6.5.4") while the HelmRepository behind it publishes the digest of the
// repo index — never equal, so a naive comparison reports "source ahead"
// on every healthy Helm release, forever. Latent until the demo grew a
// HelmRepository for its releases to point at.
func TestHelmReleaseNeverClaimsDrift(t *testing.T) {
	c := fake.NewDemo()
	reg, groups := resources.BuildDiscoveredRegistry(c.DiscoveredKinds(), c)
	sess := &tui.Session{Theme: tui.Dark(), Registry: reg, Groups: groups,
		Location: tui.Location{Context: "microk8s-cluster", Namespace: "flux-system"}}
	m := New(Config{Session: sess, Lister: c, Mutator: c,
		Kind: kube.KindFluxHelmRelease, Namespace: "flux-system", Name: "podinfo"})
	m.SetSize(120, 30)
	upd, _ := m.Update(m.load()())
	got := upd.(*Model)

	// The precondition: the source resolved, so a comparison was possible.
	if got.chn.SourceRevision == "" {
		t.Fatal("the demo must give this release a HelmRepository with an artifact")
	}
	if got.chn.DriftComparable {
		t.Error("a HelmRelease's applied revision is not in its source's vocabulary")
	}
	if note := got.driftNote(); note != "" {
		t.Errorf("driftNote = %q, want silence — not comparable is not a verdict", note)
	}
	if view := plain(got.Render()); strings.Contains(view, "source ahead") {
		t.Errorf("a healthy Helm release must not be reported as drifted:\n%s", view)
	}

	// And a Kustomization that genuinely is behind still reports it.
	drifted := plain(demoModel(t, "nebula-infra").Render())
	if !strings.Contains(drifted, "source ahead") {
		t.Errorf("a Kustomization behind its GitRepository must still report drift:\n%s", drifted)
	}
}

// TestCrossNamespaceSourceResolves: a HelmRelease in an app namespace
// pointing at a HelmRepository in flux-system is Flux's common layout. The
// source must be read — and `o` must go — where the ref says, not in the
// release's own namespace.
func TestCrossNamespaceSourceResolves(t *testing.T) {
	c := fake.NewDemo()
	c.Seed(kube.KindFluxHelmRelease, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kube.FluxGroupHelm + "/v2", "kind": "HelmRelease",
		"metadata": map[string]any{"name": "gitlab", "namespace": "gitlab"},
		"spec": map[string]any{"chart": map[string]any{"spec": map[string]any{
			"chart": "gitlab", "sourceRef": map[string]any{"kind": "HelmRepository", "name": "podinfo", "namespace": "flux-system"},
		}}},
	}})
	reg, groups := resources.BuildDiscoveredRegistry(c.DiscoveredKinds(), c)
	sess := &tui.Session{Theme: tui.Dark(), Registry: reg, Groups: groups,
		Location: tui.Location{Context: "microk8s-cluster", Namespace: "gitlab"}}
	m := New(Config{Session: sess, Lister: c, Mutator: c,
		Kind: kube.KindFluxHelmRelease, Namespace: "gitlab", Name: "gitlab"})
	m.SetSize(120, 30)
	upd, _ := m.Update(m.load()())
	got := upd.(*Model)

	if got.chn.SourceRevision == "" {
		t.Error("source in flux-system was not read: no source revision")
	}
	if want := "helm/podinfo in flux-system"; got.chn.SourceDisplay != want {
		t.Errorf("SourceDisplay = %q, want %q", got.chn.SourceDisplay, want)
	}
	_, cmd := got.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	if cmd == nil {
		t.Fatal("o produced no navigation")
	}
	msg, ok := cmd().(tui.GotoResourceMsg)
	if !ok || msg.Namespace != "flux-system" {
		t.Errorf("o navigated to %#v, want the source's own namespace", msg)
	}
}
