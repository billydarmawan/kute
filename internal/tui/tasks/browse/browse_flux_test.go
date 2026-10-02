package browse

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

func kustomizationDK() kube.DiscoveredKind {
	return kube.DiscoveredKind{
		Kind: "Kustomization", Plural: "kustomizations", Group: kube.FluxGroupKustomize,
		GVR:         schema.GroupVersionResource{Group: kube.FluxGroupKustomize, Version: "v1", Resource: "kustomizations"},
		Versions:    []kube.CRDVersion{{Name: "v1", Served: true, Storage: true}},
		Established: true,
		CRDName:     "kustomizations." + kube.FluxGroupKustomize,
	}
}

// kustomization builds one Flux object. ready "" means no conditions at all.
func kustomization(name string, suspend bool, ready, message string) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": kube.FluxGroupKustomize + "/v1",
		"kind":       "Kustomization",
		"metadata":   map[string]any{"name": name, "namespace": "flux-system"},
		"spec": map[string]any{
			"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"},
		},
	}
	if suspend {
		obj["spec"].(map[string]any)["suspend"] = true
	}
	status := map[string]any{"lastAppliedRevision": "master@sha1:efd398bed98a38348c7702355ecd98fc11ac2bef"}
	if ready != "" {
		status["conditions"] = []any{map[string]any{
			"type": "Ready", "status": ready, "message": message,
			"lastTransitionTime": "2026-08-01T09:00:00Z",
		}}
	}
	obj["status"] = status
	return &unstructured.Unstructured{Object: obj}
}

func fluxModel(t *testing.T, objs ...runtime.Object) Model {
	t.Helper()
	reg, groups := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{kustomizationDK()}, nil)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Kustomization"): objs,
	}}
	session := newSession()
	session.Registry, session.Groups = reg, groups
	session.Location.Kind = kube.ResourceKind("Kustomization")
	session.Location.Namespace = "flux-system"
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	return step(t, m, m.Init()())
}

// TestFluxSubLineRendersTheConditionMessageVerbatim is §30a's core claim:
// the failure reason is on the row, not one screen away.
func TestFluxSubLineRendersTheConditionMessageVerbatim(t *testing.T) {
	msg := "health check failed after 2m0s: Deployment/nebula-stage/nebula-worker status: 'InProgress'"
	m := fluxModel(t, kustomization("nebula-workers", false, "False", msg))

	view := plain(m.Render())
	if !strings.Contains(view, "health check failed after 2m0s") {
		t.Fatalf("expected the condition message on the row, got:\n%s", view)
	}
	// Verbatim: the object name from inside the message survives too, which
	// is what makes the message a diagnosis rather than a category.
	if !strings.Contains(view, "nebula-worker") {
		t.Errorf("the message must render verbatim, not paraphrased:\n%s", view)
	}
}

// TestFluxSubLineIsNotSelectable pins that the cursor skips a continuation
// line. Stopping on one would make every row-scoped verb ambiguous, since
// a sub-line has no object behind it.
func TestFluxSubLineIsNotSelectable(t *testing.T) {
	m := fluxModel(t,
		kustomization("nebula-apps", false, "False", "reconciliation failed"),
		kustomization("nebula-infra", false, "False", "another failure"),
	)

	// Both rows failed, so both carry sub-lines: display is row, sub, row, sub.
	if len(m.display) != 4 {
		t.Fatalf("expected 4 display lines (2 rows + 2 sub-lines), got %d", len(m.display))
	}
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		if row, ok := m.selectedRow(); ok {
			seen[row.Name] = true
		}
		if m.display[m.selected].kind == rowKindSubLine {
			t.Fatalf("cursor landed on a sub-line at display index %d", m.selected)
		}
		m.moveSelection(1)
	}
	if len(seen) != 2 {
		t.Errorf("expected to reach both data rows, saw %v", seen)
	}
}

// TestFluxHealthyRowsFoldAwayBehindTheUnhealthy is §30a's "unhealthy first"
// triage shape in a single-namespace list, where browse previously had no
// fold at all.
func TestFluxHealthyRowsFoldAwayBehindTheUnhealthy(t *testing.T) {
	m := fluxModel(t,
		kustomization("nebula-apps", false, "True", "Applied revision: master@sha1:efd398b"),
		kustomization("nebula-workers", false, "False", "health check failed"),
		kustomization("flux-system", false, "True", "Applied revision: master@sha1:efd398b"),
		kustomization("observability", false, "True", "Applied revision: master@sha1:efd398b"),
	)

	view := plain(m.Render())
	if !strings.Contains(view, "nebula-workers") {
		t.Errorf("the failing row must stay visible:\n%s", view)
	}
	if !strings.Contains(view, "+ 3 ready") {
		t.Errorf("expected a '+ 3 ready' fold line for the healthy tail:\n%s", view)
	}
	if strings.Contains(view, "observability") {
		t.Errorf("healthy rows should be folded away, but one rendered:\n%s", view)
	}

	// ↹ expands the fold, and every row comes back.
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	if !strings.Contains(plain(m.Render()), "observability") {
		t.Errorf("tab should expand the fold and reveal the healthy tail")
	}
}

// TestFluxAllSuspendedListKeepsItsStatusSemantics guards the StatusSemantics
// gate. Every row suspended means every row is Neutral, which for a plain
// CRD kind is 14a's "no status semantics" case — but a Flux list emphatically
// has them, and saying otherwise would be a lie about the one kind whose
// status derivation is the point of the feature.
func TestFluxAllSuspendedListKeepsItsStatusSemantics(t *testing.T) {
	m := fluxModel(t,
		kustomization("nebula-infra", true, "True", "Applied revision: master@sha1:efd398b"),
		kustomization("nebula-apps", true, "True", "Applied revision: master@sha1:efd398b"),
	)

	view := plain(m.Render())
	if strings.Contains(view, "no status semantics") {
		t.Errorf("a Flux list must never claim it has no status semantics:\n%s", view)
	}
	if !strings.Contains(view, "suspended") {
		t.Errorf("expected the strip to count suspended rows:\n%s", view)
	}
}

// TestFluxSuspendedRowStaysVisibleAndSortsAboveHealthy is §30a's "suspended
// is a state, not a footnote". Suspended is StatusNeutral, which for every
// other kind means finished-and-uninteresting and sinks to the bottom of a
// list — folding a paused Kustomization away would hide exactly the object
// that is silently drifting from git.
func TestFluxSuspendedRowStaysVisibleAndSortsAboveHealthy(t *testing.T) {
	m := fluxModel(t,
		kustomization("nebula-apps", false, "True", "ok"),
		kustomization("nebula-infra", true, "True", "Applied revision: master@sha1:efd398b"),
		kustomization("flux-system", false, "True", "ok"),
		kustomization("observability", false, "True", "ok"),
	)

	view := plain(m.Render())
	if !strings.Contains(view, "nebula-infra") {
		t.Errorf("the suspended row must survive the healthy-tail fold:\n%s", view)
	}
	if !strings.Contains(view, "+ 3 ready") {
		t.Errorf("expected the three healthy rows to fold, leaving the suspended one:\n%s", view)
	}
	if got := m.rows[0].Name; got != "nebula-infra" {
		t.Errorf("suspended row should sort first among otherwise-healthy rows, got %q", got)
	}
	// It must not be double-reported: the READY cell already says the word.
	if strings.Count(view, "suspended") != 2 { // strip segment + READY cell
		t.Errorf("expected 'suspended' exactly twice (strip + cell), got %d:\n%s",
			strings.Count(view, "suspended"), view)
	}
}

// fluxModelWithMutator is fluxModel plus a wired write seam.
func fluxModelWithMutator(t *testing.T, mut *fakeMutator, objs ...runtime.Object) Model {
	t.Helper()
	reg, groups := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{kustomizationDK()}, nil)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Kustomization"): objs,
	}}
	session := newSession()
	session.Registry, session.Groups = reg, groups
	session.Location.Kind = kube.ResourceKind("Kustomization")
	session.Location.Namespace = "flux-system"
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	return step(t, m, m.Init()())
}

// TestFluxSuspendVerbFlipsDirectionWithTheRow is Cordon's shape: one key,
// two directions, chosen by the row's own state rather than by a mode.
func TestFluxSuspendVerbFlipsDirectionWithTheRow(t *testing.T) {
	mut := &fakeMutator{}
	m := fluxModelWithMutator(t, mut, kustomization("nebula-apps", false, "True", "ok"))
	m = step(t, m, tea.KeyPressMsg{Text: "s"})
	if len(mut.fluxSuspends) != 1 || mut.fluxSuspends[0] != "flux-system/nebula-apps=true" {
		t.Fatalf("expected a suspend call, got %v", mut.fluxSuspends)
	}

	mut2 := &fakeMutator{}
	m2 := fluxModelWithMutator(t, mut2, kustomization("nebula-infra", true, "True", "ok"))
	m2 = step(t, m2, tea.KeyPressMsg{Text: "s"})
	if len(mut2.fluxSuspends) != 1 || mut2.fluxSuspends[0] != "flux-system/nebula-infra=false" {
		t.Fatalf("expected a resume call on a suspended row, got %v", mut2.fluxSuspends)
	}
	// The keybar has to say which direction it will go, or the single key is
	// a coin flip.
	if !strings.Contains(plain(m2.Render()), "resume") {
		t.Errorf("keybar should read 'resume' on a suspended row:\n%s", plain(m2.Render()))
	}
	if !strings.Contains(plain(m.Render()), "suspend") {
		t.Errorf("keybar should read 'suspend' on an active row")
	}
}

// TestFluxReconcileVerbAnnotates covers §30a's 'r' — and that it does not
// collide with the retry meaning 'r' carries in browse's error states,
// since a ready Flux list is not in one.
func TestFluxReconcileVerbAnnotates(t *testing.T) {
	mut := &fakeMutator{}
	m := fluxModelWithMutator(t, mut, kustomization("nebula-apps", false, "True", "ok"))
	m = step(t, m, tea.KeyPressMsg{Text: "r"})
	if len(mut.fluxReconciles) != 1 || mut.fluxReconciles[0] != "flux-system/nebula-apps" {
		t.Fatalf("expected a reconcile call, got %v", mut.fluxReconciles)
	}
	// The will-run line names the kubectl kute actually issues, never the
	// `flux reconcile` it is equivalent to — §27a's rule. It rides the
	// keybar's RightNote, the same channel scale and bulk-delete use, so it
	// sheds at narrow widths; assert the content, then that it reaches the
	// bar when there is room for it.
	if !strings.Contains(m.execFeedback, "kubectl annotate") {
		t.Errorf("will-run line = %q, want a kubectl annotate", m.execFeedback)
	}
	if strings.Contains(m.execFeedback, "flux reconcile") {
		t.Errorf("will-run line must not name a binary kute never runs: %q", m.execFeedback)
	}
	if !strings.Contains(m.execFeedback, kube.FluxReconcileAnnotation) {
		t.Errorf("will-run line should name the annotation: %q", m.execFeedback)
	}
	m.SetSize(240, 36)
	if !strings.Contains(plain(m.Render()), "kubectl annotate") {
		t.Errorf("expected the will-run line in the keybar at a wide width:\n%s", plain(m.Render()))
	}
}

// TestFluxSourceVerbJumpsToTheSource covers 'o'.
func TestFluxSourceVerbJumpsToTheSource(t *testing.T) {
	mut := &fakeMutator{}
	m := fluxModelWithMutator(t, mut, kustomization("nebula-apps", false, "True", "ok"))

	_, cmd := m.Update(tea.KeyPressMsg{Text: "o"})
	if cmd == nil {
		t.Fatal("expected a goto cmd from 'o'")
	}
	msg, ok := cmd().(tui.GotoResourceMsg)
	if !ok {
		t.Fatalf("expected GotoResourceMsg, got %T", cmd())
	}
	if msg.Kind != kube.ResourceKind("GitRepository") || msg.Name != "flux-system" {
		t.Errorf("expected a jump to GitRepository/flux-system, got %+v", msg)
	}
}

// TestFluxSourceVerbFollowsTheRefsOwnNamespace is M5: 'o' re-parsed the
// SOURCE cell, which never carried sourceRef.namespace, and so jumped to
// the source in the row's own namespace — an empty list in the common
// layout where the source lives in flux-system.
func TestFluxSourceVerbFollowsTheRefsOwnNamespace(t *testing.T) {
	ks := kustomization("app", false, "True", "ok")
	ks.SetNamespace("apps")
	ks.Object["spec"].(map[string]any)["sourceRef"] = map[string]any{
		"kind": "GitRepository", "name": "fleet", "namespace": "flux-system",
	}
	reg, groups := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{kustomizationDK()}, nil)
	session := newSession()
	session.Registry, session.Groups = reg, groups
	session.Location.Kind = kube.ResourceKind("Kustomization")
	session.Location.Namespace = "apps"
	m := New(Config{Session: session, Mutator: &fakeMutator{}, Lister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Kustomization"): {ks},
	}}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if view := plain(m.Render()); !strings.Contains(view, "git/fleet in flux-system") {
		t.Errorf("SOURCE should name the source's namespace when it differs:\n%s", view)
	}
	_, cmd := m.Update(tea.KeyPressMsg{Text: "o"})
	if cmd == nil {
		t.Fatal("expected a goto cmd from 'o'")
	}
	msg, ok := cmd().(tui.GotoResourceMsg)
	if !ok || msg.Kind != "GitRepository" || msg.Namespace != "flux-system" || msg.Name != "fleet" {
		t.Errorf("expected a jump to GitRepository flux-system/fleet, got %#v", cmd())
	}
}

// TestFluxVerbsAreInertWithoutAMutator guards the read-only case: no write
// seam wired means the keys do nothing rather than panic.
func TestFluxVerbsAreInertWithoutAMutator(t *testing.T) {
	m := fluxModel(t, kustomization("nebula-apps", false, "True", "ok"))
	for _, key := range []string{"s", "r"} {
		m = step(t, m, tea.KeyPressMsg{Text: key})
	}
	if strings.Contains(plain(m.Render()), "suspend") {
		t.Errorf("keybar should not offer suspend without a mutator")
	}
}

// TestFluxEnterOpensReconcilerDetailOnlyForReconcilers is M9: ↵ sent every
// Flux kind to §31a, where a GitRepository has no chain and no inventory,
// instead of to 14d's CONDITIONS and EVENTS as §30a specifies.
func TestFluxEnterOpensReconcilerDetailOnlyForReconcilers(t *testing.T) {
	gitDK := kube.DiscoveredKind{
		Kind: "GitRepository", Plural: "gitrepositories", Group: kube.FluxGroupSource,
		GVR:         schema.GroupVersionResource{Group: kube.FluxGroupSource, Version: "v1", Resource: "gitrepositories"},
		Versions:    []kube.CRDVersion{{Name: "v1", Served: true, Storage: true}},
		Established: true, CRDName: "gitrepositories." + kube.FluxGroupSource,
	}
	repo := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kube.FluxGroupSource + "/v1", "kind": "GitRepository",
		"metadata": map[string]any{"name": "fleet", "namespace": "flux-system"},
	}}
	reg, groups := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{kustomizationDK(), gitDK}, nil)

	for _, tc := range []struct {
		kind     kube.ResourceKind
		obj      runtime.Object
		wantFlux bool
	}{
		{"Kustomization", kustomization("apps", false, "True", "ok"), true},
		{"GitRepository", repo, false},
	} {
		var opened string
		session := newSession()
		session.Registry, session.Groups = reg, groups
		session.Location.Kind, session.Location.Namespace = tc.kind, "flux-system"
		m := New(Config{
			Session: session,
			Lister:  fakeLister{objs: map[kube.ResourceKind][]runtime.Object{tc.kind: {tc.obj}}},
			OpenFluxDetail: func(kube.ResourceKind, string, string, int, int) (tea.Model, tea.Cmd) {
				opened = "flux"
				return stubTask{}, nil
			},
			OpenObjectDetail: func(kube.ResourceKind, string, string, []ObjectSiblingRef, int, int, int) (tea.Model, tea.Cmd) {
				opened = "14d"
				return stubTask{}, nil
			},
		})
		m.SetSize(120, 36)
		m = step(t, m, m.Init()())
		m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if want := map[bool]string{true: "flux", false: "14d"}[tc.wantFlux]; opened != want {
			t.Errorf("%s: ↵ opened %q, want %q", tc.kind, opened, want)
		}
	}
}
