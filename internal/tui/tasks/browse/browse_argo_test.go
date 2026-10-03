package browse

import (
	"context"
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/state"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
)

func applicationDK() kube.DiscoveredKind {
	return kube.DiscoveredKind{
		Kind: "Application", Plural: "applications", Group: kube.ArgoGroup,
		GVR:         schema.GroupVersionResource{Group: kube.ArgoGroup, Version: "v1alpha1", Resource: "applications"},
		Versions:    []kube.CRDVersion{{Name: "v1alpha1", Served: true, Storage: true}},
		Established: true,
		CRDName:     "applications." + kube.ArgoGroup,
	}
}

// application builds one Argo CD Application object.
func application(name, targetRevision, revision, sync, health string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kube.ArgoGroup + "/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": "argocd"},
		"spec":       map[string]any{"source": map[string]any{"targetRevision": targetRevision}},
		"status": map[string]any{
			"sync":   map[string]any{"status": sync, "revision": revision},
			"health": map[string]any{"status": health},
		},
	}}
}

func argoModel(t *testing.T, objs ...runtime.Object) Model {
	t.Helper()
	reg, groups := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{applicationDK()}, nil)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Application"): objs,
	}}
	session := newSession()
	session.Registry, session.Groups = reg, groups
	session.Location.Kind = kube.ResourceKind("Application")
	session.Location.Namespace = "argocd"
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	return step(t, m, m.Init()())
}

func argoModelWithMutator(t *testing.T, mut *fakeMutator, objs ...runtime.Object) Model {
	t.Helper()
	reg, groups := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{applicationDK()}, nil)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Application"): objs,
	}}
	session := newSession()
	session.Registry, session.Groups = reg, groups
	session.Location.Kind = kube.ResourceKind("Application")
	session.Location.Namespace = "argocd"
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	return step(t, m, m.Init()())
}

// TestArgoDegradedOutOfSyncProgressingHealthySortOrder pins §33a's stated
// order: Degraded → OutOfSync → Progressing → Healthy.
func TestArgoDegradedOutOfSyncProgressingHealthySortOrder(t *testing.T) {
	m := argoModel(t,
		application("api", "main", "abc0000", "Synced", "Healthy"),
		application("web", "main", "abc0000", "Synced", "Progressing"),
		application("worker", "main", "abc0000", "OutOfSync", "Healthy"),
		application("billing", "main", "abc0000", "Synced", "Degraded"),
	)
	var order []string
	for _, r := range m.rows {
		order = append(order, r.Name)
	}
	want := []string{"billing", "worker", "web", "api"}
	if len(order) != len(want) {
		t.Fatalf("row order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("row order = %v, want %v", order, want)
		}
	}
}

func TestArgoSyncAndHealthCellsUseIndependentColors(t *testing.T) {
	m := argoModel(t, application("billing", "main", "abc0000", "Synced", "Degraded"))
	theme := tui.Dark()
	styles := newRowCellStyles(theme, false, false, false)
	cols := browseColumns(m.desc)

	tests := []struct {
		name       string
		obj        runtime.Object
		wantSync   color.Color
		wantHealth color.Color
	}{
		{"synced degraded", application("billing", "main", "abc0000", "Synced", "Degraded"), theme.TextSecondary, theme.Bad},
		{"out of sync healthy", application("frontend", "main", "abc0000", "OutOfSync", "Healthy"), theme.Warn, theme.TextSecondary},
		{"syncing progressing", application("search", "main", "abc0000", "Syncing", "Progressing"), theme.TextSecondary, theme.TextSecondary},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := m.desc.Project(tt.obj)
			cells := m.rowCells(r, nil, cols, 120, styles, theme, 0, 0, "", false, false)
			if got := cells[2].Style.GetForeground(); got != tt.wantSync {
				t.Errorf("Sync foreground = %v, want %v", got, tt.wantSync)
			}
			if got := cells[3].Style.GetForeground(); got != tt.wantHealth {
				t.Errorf("Health foreground = %v, want %v", got, tt.wantHealth)
			}
		})
	}
}

// TestArgoDegradedAndOutOfSyncStayVisibleProgressingAndHealthyFold pins the
// fold-visibility half of §33a: only Degraded/OutOfSync rows are pinned
// above a healthy-tail fold — Progressing and Healthy both fold away.
func TestArgoDegradedAndOutOfSyncStayVisibleProgressingAndHealthyFold(t *testing.T) {
	m := argoModel(t,
		application("billing", "main", "abc0000", "Synced", "Degraded"),
		application("worker", "main", "abc0000", "OutOfSync", "Healthy"),
		application("web", "main", "abc0000", "Synced", "Progressing"),
		application("api", "main", "abc0000", "Synced", "Healthy"),
	)
	view := plain(m.Render())
	if !strings.Contains(view, "billing") || !strings.Contains(view, "worker") {
		t.Errorf("Degraded/OutOfSync rows must stay visible:\n%s", view)
	}
	if strings.Contains(view, "web") || strings.Contains(view, "api") {
		t.Errorf("Progressing/Healthy rows should be folded away:\n%s", view)
	}
	if !strings.Contains(view, "+ 2") {
		t.Errorf("expected a fold line for the two folded rows:\n%s", view)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	view = plain(m.Render())
	if !strings.Contains(view, "web") || !strings.Contains(view, "api") {
		t.Errorf("tab should expand the fold and reveal every row:\n%s", view)
	}
}

// TestArgoSubLineRendersTheResourceHealthMessageVerbatim is §33a's core
// claim, mirroring TestFluxSubLineRendersTheConditionMessageVerbatim.
func TestArgoSubLineRendersTheResourceHealthMessageVerbatim(t *testing.T) {
	obj := application("billing", "main", "abc0000", "Synced", "Degraded")
	obj.Object["status"].(map[string]any)["resources"] = []any{
		map[string]any{
			"kind": "Deployment", "name": "billing-api",
			"health": map[string]any{"status": "Degraded", "message": `container "api" is in CrashLoopBackOff — exit 1, 2m ago`},
		},
	}
	m := argoModel(t, obj)
	view := plain(m.Render())
	if !strings.Contains(view, "CrashLoopBackOff") {
		t.Fatalf("expected the resource health message on the row, got:\n%s", view)
	}
	if !strings.Contains(view, "billing-api") {
		t.Errorf("the message must render verbatim, not paraphrased:\n%s", view)
	}
}

// TestArgoRefreshVerbAnnotates covers §33a's 'r' — TierNone, fires
// immediately, mirroring TestFluxReconcileVerbAnnotates.
func TestArgoRefreshVerbAnnotates(t *testing.T) {
	mut := &fakeMutator{}
	m := argoModelWithMutator(t, mut, application("api", "main", "abc0000", "Synced", "Healthy"))
	m = step(t, m, tea.KeyPressMsg{Text: "r"})
	if len(mut.argoRefreshes) != 1 || mut.argoRefreshes[0] != "argocd/api" {
		t.Fatalf("expected a refresh call, got %v", mut.argoRefreshes)
	}
	if !strings.Contains(m.execFeedback, "kubectl annotate") {
		t.Errorf("will-run line = %q, want a kubectl annotate", m.execFeedback)
	}
	if strings.Contains(m.execFeedback, "argocd app get") {
		t.Errorf("will-run line must not name the argocd binary: %q", m.execFeedback)
	}
	if !strings.Contains(m.execFeedback, kube.ArgoRefreshAnnotation) {
		t.Errorf("will-run line should name the annotation: %q", m.execFeedback)
	}
}

// TestArgoSyncShowsConfirmThenSyncsOnY covers §33a's 'S' — TierInline, so it
// waits for 'y', mirroring TestCtrlRShowsConfirmThenRestartsRolloutOnY.
func TestArgoSyncShowsConfirmThenSyncsOnY(t *testing.T) {
	mut := &fakeMutator{}
	m := argoModelWithMutator(t, mut, application("worker", "main", "e41b90c1f2a3b4c5d6e7f8091a2b3c4d5e6f7081", "OutOfSync", "Healthy"))

	m = step(t, m, tea.KeyPressMsg{Text: "S"})
	if !m.actions.Active() || m.actions.Tier() != actions.TierInline {
		t.Fatalf("expected 'S' to open the inline prompt, tier=%v", m.actions.Tier())
	}
	kb := m.Keybar()
	if !strings.Contains(kb.RightNote, "kubectl patch application/worker") {
		t.Fatalf("expected the will-run line in the confirm, got %q", kb.RightNote)
	}
	if !strings.Contains(kb.RightNote, `"revision":"main"`) {
		t.Fatalf("expected the app's own target revision in the patch, got %q", kb.RightNote)
	}
	if len(mut.argoSyncs) != 0 {
		t.Fatalf("expected no sync before 'y', got %v", mut.argoSyncs)
	}

	if !strings.Contains(kb.RightNote, `"initiatedBy":{"automated":false,"username":"kute"}`) {
		t.Fatalf("expected an unreviewed sync credited to kute in the patch, got %q", kb.RightNote)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.argoSyncs) != 1 || mut.argoSyncs[0] != "argocd/worker@main" {
		t.Fatalf("argoSyncs = %v, want [argocd/worker@main]", mut.argoSyncs)
	}
	if mut.argoSyncInitiators[0] != "kute" {
		t.Fatalf("initiator = %q, want kute", mut.argoSyncInitiators[0])
	}
}

type fakeIdentity struct {
	id kube.ReviewedIdentity
	ok bool
}

func (f *fakeIdentity) ReviewedIdentity() (kube.ReviewedIdentity, bool) { return f.id, f.ok }

// TestArgoSyncCreditsTheReviewedIdentity: with a SelfSubjectReview answer,
// the will-run line and the executed sync both name that user — read at
// Begin time, so a review landing after browse was built still counts.
func TestArgoSyncCreditsTheReviewedIdentity(t *testing.T) {
	mut := &fakeMutator{}
	identity := &fakeIdentity{}
	reg, groups := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{applicationDK()}, nil)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Application"): {application("worker", "main", "abc0000", "OutOfSync", "Healthy")},
	}}
	session := newSession()
	session.Registry, session.Groups = reg, groups
	session.Location.Kind = kube.ResourceKind("Application")
	session.Location.Namespace = "argocd"
	m := New(Config{Session: session, Lister: lister, Mutator: mut, Identity: identity})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	identity.id, identity.ok = kube.ReviewedIdentity{Username: "alice@example.com"}, true

	m = step(t, m, tea.KeyPressMsg{Text: "S"})
	if kb := m.Keybar(); !strings.Contains(kb.RightNote, `"username":"alice@example.com"`) {
		t.Fatalf("will-run line should credit the reviewed user, got %q", kb.RightNote)
	}
	step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.argoSyncInitiators) != 1 || mut.argoSyncInitiators[0] != "alice@example.com" {
		t.Fatalf("initiators = %v, want [alice@example.com]", mut.argoSyncInitiators)
	}
}

// TestArgoSyncRevisionFallsBackToHEAD covers argoSyncRevisionFromRow's
// fallback for an Application that has never synced (bare "HEAD" cell, no
// SHA to strip).
func TestArgoSyncRevisionFallsBackToHEAD(t *testing.T) {
	mut := &fakeMutator{}
	m := argoModelWithMutator(t, mut, application("web", "", "", "Unknown", "Unknown"))
	m = step(t, m, tea.KeyPressMsg{Text: "S"})
	step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.argoSyncs) != 1 || mut.argoSyncs[0] != "argocd/web@HEAD" {
		t.Fatalf("argoSyncs = %v, want [argocd/web@HEAD]", mut.argoSyncs)
	}
}

// TestArgoDashboardURLCopiesTheConfiguredLink covers §33a's 'u'.
func TestArgoDashboardURLCopiesTheConfiguredLink(t *testing.T) {
	t.Setenv("KUTE_TEST_NO_CLIPBOARD", "1") // no-op; documents intent only
	reg, groups := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{applicationDK()}, nil)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Application"): {application("worker", "main", "abc0000", "OutOfSync", "Healthy")},
		kube.KindConfigMap: {&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "argocd-cm", Namespace: "argocd"},
			Data:       map[string]string{"url": "https://argocd.demo.local"},
		}},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Registry, session.Groups = reg, groups
	session.Location.Kind = kube.ResourceKind("Application")
	session.Location.Namespace = "argocd"
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	_, cmd := m.Update(tea.KeyPressMsg{Text: "u"})
	if cmd == nil {
		t.Fatal("expected a clipboard cmd from 'u'")
	}
}

// TestArgoDashboardURLMissingConfigMapSetsFeedback covers the graceful
// fallback when argocd-cm isn't found.
func TestArgoDashboardURLMissingConfigMapSetsFeedback(t *testing.T) {
	mut := &fakeMutator{}
	m := argoModelWithMutator(t, mut, application("worker", "main", "abc0000", "OutOfSync", "Healthy"))
	m = step(t, m, tea.KeyPressMsg{Text: "u"})
	if !strings.Contains(m.execFeedback, "not configured") {
		t.Errorf("expected a not-configured feedback message, got %q", m.execFeedback)
	}
}

// configMapCountingLister counts ConfigMap reads and can hold the
// ConfigMap cache unsynced, for the u key's lookup.
type configMapCountingLister struct {
	fakeLister
	reads    *int
	unsynced *bool
}

func (l configMapCountingLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	if kind == kube.KindConfigMap {
		*l.reads++
	}
	return l.fakeLister.ListRaw(ctx, kind, namespace)
}

func (l configMapCountingLister) KindSynced(kind kube.ResourceKind, _ string) bool {
	return kind != kube.KindConfigMap || l.unsynced == nil || !*l.unsynced
}

func argoDashboardModel(t *testing.T, appNamespace string, lister configMapCountingLister) Model {
	t.Helper()
	reg, groups := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{applicationDK()}, nil)
	session := newSession()
	session.Registry, session.Groups = reg, groups
	session.Location.Kind = kube.ResourceKind("Application")
	session.Location.Namespace = appNamespace
	m := New(Config{Session: session, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	return step(t, m, m.Init()())
}

// TestArgoDashboardURLReadsOffTheUpdateLoopFromTheControlPlane pins two
// things about 'u': the argocd-cm read happens in the returned tea.Cmd, not
// inside Update, and an Application outside Argo's own namespace (apps in
// any namespace) still finds argocd-cm in the control-plane namespace.
func TestArgoDashboardURLReadsOffTheUpdateLoopFromTheControlPlane(t *testing.T) {
	app := application("worker", "main", "abc0000", "OutOfSync", "Healthy")
	app.SetNamespace("team-a")
	reads := 0
	lister := configMapCountingLister{reads: &reads, fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Application"): {app},
		kube.KindConfigMap: {&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "argocd-cm", Namespace: "argocd"},
			Data:       map[string]string{"url": "https://argocd.demo.local"},
		}},
	}}}
	m := argoDashboardModel(t, "team-a", lister)
	reads = 0

	_, cmd := m.Update(tea.KeyPressMsg{Text: "u"})
	if reads != 0 {
		t.Fatalf("'u' read ConfigMaps %d time(s) inside Update", reads)
	}
	if cmd == nil {
		t.Fatal("expected a lookup cmd from 'u'")
	}
	msg, ok := cmd().(argoDashboardURLMsg)
	if !ok || msg.url != "https://argocd.demo.local/applications/team-a/worker" {
		t.Fatalf("lookup = %+v, want the team-a/worker deep link built from argocd/argocd-cm", msg)
	}
}

// TestArgoDashboardURLSaysLoadingOnAColdCache pins that the first 'u' —
// which starts the ConfigMap informer — reports a still-filling cache as
// loading, not as argocd-cm having no url.
func TestArgoDashboardURLSaysLoadingOnAColdCache(t *testing.T) {
	reads, unsynced := 0, false
	lister := configMapCountingLister{reads: &reads, unsynced: &unsynced, fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Application"): {application("worker", "main", "abc0000", "OutOfSync", "Healthy")},
	}}}
	m := argoDashboardModel(t, "argocd", lister)
	unsynced = true // only once the list itself has loaded
	m = step(t, m, tea.KeyPressMsg{Text: "u"})
	if !strings.Contains(m.execFeedback, "still loading") || strings.Contains(m.execFeedback, "not configured") {
		t.Fatalf("feedback = %q, want a loading message", m.execFeedback)
	}
}

// TestArgoVerbsAreInertWithoutAMutator guards the read-only case, mirroring
// TestFluxVerbsAreInertWithoutAMutator.
func TestArgoVerbsAreInertWithoutAMutator(t *testing.T) {
	m := argoModel(t, application("api", "main", "abc0000", "Synced", "Healthy"))
	for _, key := range []string{"r", "S"} {
		m = step(t, m, tea.KeyPressMsg{Text: key})
	}
	if m.actions.Active() {
		t.Error("no key should open a confirm without a mutator")
	}
	if strings.Contains(plain(m.Render()), "refresh") {
		t.Errorf("keybar should not offer refresh without a mutator")
	}
}

// TestLaunchKeepsAPersistedCRDKindUntilDiscovery pins M5: at launch the
// registry is the built-ins only until discovery lands, so a persisted CRD
// kind opens on Pods — but it must be held, not lost. Quitting before
// discovery persists it rather than Pod, and once the kind resolves the
// root's jump to it opens it and drops the hold.
func TestLaunchKeepsAPersistedCRDKindUntilDiscovery(t *testing.T) {
	session := newSession()
	session.Location.Kind = kube.ResourceKind("Application")
	session.Location.Namespace = "argocd"
	session.State.PerContext = map[string]state.PerContext{}
	m := New(Config{Session: session, Lister: fakeLister{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.kind != kube.KindPod || session.Location.Kind != kube.KindPod {
		t.Fatalf("an unresolvable launch kind opened %q (Location %q), want the Pod fallback", m.kind, session.Location.Kind)
	}
	if got := session.PendingKind(); got != "Application" {
		t.Fatalf("PendingKind = %q, want the persisted Application held for discovery", got)
	}
	session.SyncLocationToPerContext()
	if got := session.State.PerContext[session.Location.Context].Kind; got != "Application" {
		t.Fatalf("quitting before discovery persisted kind %q, want Application", got)
	}

	session.Registry, session.Groups = resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{applicationDK()}, nil)
	m = step(t, m, tui.GotoKindMsg{Kind: kube.ResourceKind("Application")})
	if m.kind != "Application" || session.Location.Kind != "Application" {
		t.Fatalf("after discovery the jump opened %q (Location %q), want Application", m.kind, session.Location.Kind)
	}
	if got := session.PendingKind(); got != "" {
		t.Fatalf("PendingKind = %q after the kind opened, want it dropped", got)
	}
}
