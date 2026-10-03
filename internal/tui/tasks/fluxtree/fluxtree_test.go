package fluxtree

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/kube/fake"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

func demoModel(t *testing.T) *Model {
	t.Helper()
	c := fake.NewDemo()
	reg, groups := resources.BuildDiscoveredRegistry(c.DiscoveredKinds(), c)
	sess := &tui.Session{
		Theme: tui.Dark(), Registry: reg, Groups: groups,
		Location: tui.Location{Context: "microk8s-cluster", Namespace: "flux-system"},
	}
	m := New(Config{Session: sess, Lister: c, Mutator: c})
	m.SetSize(120, 36)
	upd, _ := m.Update(m.load()())
	return upd.(*Model)
}

type syncLister struct {
	resources.RawLister
	synced bool
}

func (l syncLister) KindSynced(kube.ResourceKind, string) bool { return l.synced }

func TestLoadingStateShowsWhatFluxIsListing(t *testing.T) {
	c := fake.NewDemo()
	reg, groups := resources.BuildDiscoveredRegistry(c.DiscoveredKinds(), c)
	sess := &tui.Session{Theme: tui.Dark(), Registry: reg, Groups: groups}
	m := New(Config{Session: sess, Lister: syncLister{RawLister: c, synced: false}})
	m.SetSize(120, 36)

	view := plain(m.Render())
	for _, want := range []string{
		"loading Flux",
		"listing Flux sources and reconcilers…",
		"watch starts when the lists land",
		"SOURCE / RECONCILER",
		"KIND",
		"REVISION",
		"RECONCILED",
		"– of –",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("loading view missing %q:\n%s", want, view)
		}
	}
}

func TestPartialFluxTreeStaysLoadingUntilAllCachesSync(t *testing.T) {
	c := fake.NewDemo()
	reg, groups := resources.BuildDiscoveredRegistry(c.DiscoveredKinds(), c)
	sess := &tui.Session{Theme: tui.Dark(), Registry: reg, Groups: groups}
	lister := syncLister{RawLister: c, synced: false}
	m := New(Config{Session: sess, Lister: lister})
	m.SetSize(120, 36)

	loaded, cmd := m.Update(m.load()())
	if cmd == nil {
		t.Fatal("expected a cache-sync retry while Flux caches are filling")
	}
	got := loaded.(*Model)
	if got.state != tui.TaskStateLoading {
		t.Fatalf("state = %s, want loading", got.state)
	}
	if len(got.groups) != 0 {
		t.Fatalf("partial groups were published while loading: %d", len(got.groups))
	}

	lister.synced = true
	got.lister = lister
	loaded, cmd = got.Update(got.load()())
	if cmd != nil {
		t.Fatal("fully synced Flux caches should not schedule another retry")
	}
	if got = loaded.(*Model); got.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready after all caches sync", got.state)
	}
	if len(got.groups) == 0 {
		t.Fatal("expected the complete Flux tree after synchronization")
	}
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

// TestTreeNestsReconcilersUnderTheirSource is §30b's whole claim: the chain
// is on screen, not reconstructed in the reader's head from two lists.
func TestTreeNestsReconcilersUnderTheirSource(t *testing.T) {
	m := demoModel(t)
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready (feedback %q)", m.state, m.feedback)
	}
	if len(m.groups) == 0 {
		t.Fatal("expected at least one source group")
	}

	var found bool
	for _, g := range m.groups {
		if g.head.name != "flux-system" || g.head.kindLabel != "GitRepo" {
			continue
		}
		found = true
		if len(g.children) == 0 {
			t.Fatal("the GitRepository must carry the Kustomizations that reconcile from it")
		}
		for _, c := range g.children {
			if c.isSource {
				t.Errorf("%s is nested as a child but marked a source", c.name)
			}
			if c.sourceName != "flux-system" {
				t.Errorf("%s nested under the wrong source: sourceRef is %q", c.name, c.sourceName)
			}
		}
	}
	if !found {
		t.Fatalf("no GitRepo group in %+v", m.groups)
	}

	view := plain(m.Render())
	if !strings.Contains(view, "└─") {
		t.Errorf("expected the tree lead on nested rows:\n%s", view)
	}
	if !strings.Contains(view, "SOURCE / RECONCILER") {
		t.Errorf("expected §30b's own column header:\n%s", view)
	}
}

// TestDriftIsABooleanNotACount pins the design's one unbuildable headline.
// The mockup draws "−9" (the source is 9 commits ahead); counting commits
// needs git log, which kute never has (docs/flux-plan.md G1), so the honest
// rendering is the boolean — and it must never grow a number.
func TestDriftIsABooleanNotACount(t *testing.T) {
	m := demoModel(t)
	var drifted []treeRow
	for _, g := range m.groups {
		for _, c := range g.children {
			if strings.Contains(c.revision, "source ahead") {
				drifted = append(drifted, c)
			}
		}
	}
	if len(drifted) == 0 {
		t.Fatal("the demo fixtures must include a reconciler whose applied revision is behind its source")
	}
	for _, r := range drifted {
		if strings.ContainsAny(r.revision, "−-+") && !strings.HasPrefix(r.revision, "-") {
			// A "−N commits" style delta would have to come from somewhere,
			// and there is nowhere for it to come from.
			if strings.Contains(r.revision, "−") {
				t.Errorf("%s renders a commit delta %q — unbuildable without git log", r.name, r.revision)
			}
		}
	}
}

// TestHealthyChainsFoldAwayBehindTheTrouble is §30b's triage order: a
// working chain is one line of summary, not fifteen rows of noise.
func TestHealthyChainsFoldAwayBehindTheTrouble(t *testing.T) {
	m := demoModel(t)
	if m.foldedSources == 0 {
		t.Skip("no fully-healthy chain in the demo fixtures to fold")
	}
	view := plain(m.Render())
	if !strings.Contains(view, "ready · ↹ expand") {
		t.Errorf("expected the fold summary line:\n%s", view)
	}

	m2, _ := m.Update(tea.KeyPressMsg{Text: "tab"})
	expanded := plain(m2.(*Model).Render())
	if strings.Contains(expanded, "ready · ↹ expand") {
		t.Errorf("tab should expand the folded chains:\n%s", expanded)
	}
}

// TestSubLinesAreNotSelectable: a condition message is a continuation of
// the row above it, never a stop of its own — §30a's rule, and what keeps
// 'r'/'s' from landing on a line with no object behind it.
func TestSubLinesAreNotSelectable(t *testing.T) {
	m := demoModel(t)
	var sawSub bool
	for _, l := range m.lines {
		if l.kind == lineSub {
			sawSub = true
		}
	}
	if !sawSub {
		t.Fatal("the demo fixtures must include a failing reconciler with a condition message")
	}
	for range m.lines {
		if l := m.lines[m.selected]; l.kind != lineRow {
			t.Fatalf("cursor landed on a non-object line (%v) at index %d", l.kind, m.selected)
		}
		if _, ok := m.selectedRow(); !ok {
			t.Fatalf("selected index %d resolves to no row", m.selected)
		}
		m.moveSelection(1)
	}
}

// TestReconcileFollowsTheCursor is the design's "one key, scope follows the
// cursor": on a source 'r' reconciles that source; on a reconciler it
// reconciles with-source, and the will-run line shows both commands.
func TestReconcileFollowsTheCursor(t *testing.T) {
	m := demoModel(t)

	sourceIdx, reconcilerIdx := -1, -1
	for i, l := range m.lines {
		if l.kind != lineRow {
			continue
		}
		if m.rows[l.row].isSource && sourceIdx < 0 {
			sourceIdx = i
		}
		if !m.rows[l.row].isSource && m.rows[l.row].sourceName != "" && reconcilerIdx < 0 {
			reconcilerIdx = i
		}
	}
	if sourceIdx < 0 || reconcilerIdx < 0 {
		t.Fatalf("need both a source and a reconciler row, got %d/%d", sourceIdx, reconcilerIdx)
	}

	m.selected = sourceIdx
	m.Update(tea.KeyPressMsg{Text: "r"})
	if strings.Contains(m.execFeedback, "&&") {
		t.Errorf("a source reconcile is one command, got %q", m.execFeedback)
	}
	if !strings.Contains(m.execFeedback, "kubectl annotate") {
		t.Errorf("expected the will-run line to name the command, got %q", m.execFeedback)
	}

	m.selected = reconcilerIdx
	row, _ := m.selectedRow()
	m.Update(tea.KeyPressMsg{Text: "r"})
	if !strings.Contains(m.execFeedback, "&&") {
		t.Errorf("a reconciler reconcile is with-source — two commands, got %q", m.execFeedback)
	}
	if !strings.Contains(m.execFeedback, row.sourceName) {
		t.Errorf("the with-source half must name the source %q, got %q", row.sourceName, m.execFeedback)
	}
	// The source is annotated first: syncing a reconciler against a stale
	// artifact re-applies what it already has.
	if got := strings.Index(m.execFeedback, row.sourceName); got > strings.Index(m.execFeedback, "&&") {
		t.Errorf("source must be reconciled first, got %q", m.execFeedback)
	}
}

// TestSuspendFlipsDirectionWithTheRow — one verb, two directions, §30a's
// own shape carried onto this screen.
func TestSuspendFlipsDirectionWithTheRow(t *testing.T) {
	m := demoModel(t)
	idx := -1
	for i, l := range m.lines {
		if l.kind == lineRow && m.rows[l.row].suspended {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("the demo fixtures must include a suspended reconciler")
	}
	m.selected = idx
	hasResume := false
	for _, group := range m.Keybar().Groups {
		for _, hint := range group {
			hasResume = hasResume || strings.Contains(plain(hint.Label), "resume")
		}
	}
	if !hasResume {
		t.Errorf("the keybar must offer resume on a suspended row, got %+v", m.Keybar().Groups)
	}
	m.Update(tea.KeyPressMsg{Text: "s"})
	if !strings.Contains(m.execFeedback, "suspend=false") && !strings.Contains(m.execFeedback, "false") {
		t.Errorf("resuming must patch suspend to false, got %q", m.execFeedback)
	}
}

// TestVerbsAreInertWithoutAMutator mirrors browse's own guard: a read-only
// wiring must advertise nothing it can't do.
func TestVerbsAreInertWithoutAMutator(t *testing.T) {
	c := fake.NewDemo()
	reg, groups := resources.BuildDiscoveredRegistry(c.DiscoveredKinds(), c)
	sess := &tui.Session{Theme: tui.Dark(), Registry: reg, Groups: groups}
	m := New(Config{Session: sess, Lister: c})
	m.SetSize(120, 36)
	upd, _ := m.Update(m.load()())
	got := upd.(*Model)

	got.Update(tea.KeyPressMsg{Text: "r"})
	if got.execFeedback != "" {
		t.Errorf("reconcile must be inert without a mutator, got %q", got.execFeedback)
	}
	for _, group := range got.Keybar().Groups {
		for _, hint := range group {
			if hint.Label == "reconcile" || hint.Label == "suspend" {
				t.Errorf("keybar advertises %q with no mutator wired", hint.Label)
			}
		}
	}
}

// TestOnlyFluxKindsTriggerAReload: the join is expensive enough that a Pod
// event must not re-run it.
func TestOnlyFluxKindsTriggerAReload(t *testing.T) {
	m := demoModel(t)
	if m.isFluxKind(kube.KindPod) {
		t.Error("Pod is not a Flux kind")
	}
	if !m.isFluxKind(kube.ResourceKind("Kustomization")) {
		t.Error("Kustomization must trigger a reload")
	}
	if !m.isFluxKind(kube.KindFluxHelmRelease) {
		t.Error("Flux's HelmRelease must trigger a reload — it's half the tree")
	}
	if m.isFluxKind(kube.KindHelmRelease) {
		t.Error("§18a's Helm-3 release kind is not Flux's and must not reload this screen")
	}
}

// TestHelmChainsNeverClaimDrift is the join's sharpest edge. A
// Kustomization's applied revision and its GitRepository's artifact
// revision are the same kind of string, so an inequality is real drift. A
// HelmRelease's is a chart version ("19.6.1") while its HelmRepository
// publishes an index digest — never equal, so a naive comparison brands
// every healthy Helm chain "source ahead" forever.
func TestHelmChainsNeverClaimDrift(t *testing.T) {
	m := demoModel(t)
	var sawHelmChain bool
	for _, g := range m.groups {
		for _, c := range g.children {
			if c.kindLabel != "HelmRelease" {
				continue
			}
			sawHelmChain = true
			if strings.Contains(c.revision, "source ahead") {
				t.Errorf("%s claims drift against an index digest: %q", c.name, c.revision)
			}
		}
	}
	if !sawHelmChain {
		t.Fatal("the demo fixtures must include a HelmRelease under a HelmRepository")
	}

	// And the comparison still fires where it is meaningful.
	var sawGitDrift bool
	for _, g := range m.groups {
		if g.head.kindLabel != "GitRepo" {
			continue
		}
		for _, c := range g.children {
			if strings.Contains(c.revision, "source ahead") {
				sawGitDrift = true
			}
		}
	}
	if !sawGitDrift {
		t.Error("a Kustomization behind its GitRepository must still report drift")
	}
}

// TestSourceRevisionReadsIndexForAChartRepo: a HelmRepository's artifact is
// the repo index, whose digest names nothing a human compares.
func TestSourceRevisionReadsIndexForAChartRepo(t *testing.T) {
	m := demoModel(t)
	for _, g := range m.groups {
		if g.head.kindLabel != "HelmRepo" {
			continue
		}
		if !strings.HasPrefix(g.head.revision, "index") {
			t.Errorf("%s renders %q, want an \"index · <age>\" cell", g.head.name, g.head.revision)
		}
	}
}

// TestEnterPushesSoEscapeComesBack is the navigation contract for this
// screen: whatever ↵ opens, one esc returns to the tree.
//
// tui.Model pushes the current task onto its back-stack precisely when a
// task's Update returns a *different* task instance, and a tui.BackMsg pops
// one level. So "↵ returns a new task, and never a tui.GotoResource cmd" is
// exactly the property that makes esc come back to the tree itself rather
// than a browse list — the jump tasks/events and tasks/timeline make on ↵
// (see fluxtree/update.go's openSelectedDetail doc comment for why that
// shape is right for them and wrong here).
func TestEnterPushesSoEscapeComesBack(t *testing.T) {
	opened := map[string]bool{}
	newTree := func(t *testing.T) *Model {
		t.Helper()
		c := fake.NewDemo()
		reg, groups := resources.BuildDiscoveredRegistry(c.DiscoveredKinds(), c)
		sess := &tui.Session{
			Theme: tui.Dark(), Registry: reg, Groups: groups,
			Location: tui.Location{Context: "microk8s-cluster", Namespace: "flux-system"},
		}
		m := New(Config{
			Session: sess, Lister: c, Mutator: c,
			OpenFluxDetail: func(_ kube.ResourceKind, _, name string, _, _ int) (tea.Model, tea.Cmd) {
				opened["flux-detail/"+name] = true
				return &pushedTask{}, nil
			},
			OpenObjectDetail: func(_ kube.ResourceKind, _, name string, _, _ int) (tea.Model, tea.Cmd) {
				opened["object-detail/"+name] = true
				return &pushedTask{}, nil
			},
		})
		m.SetSize(120, 36)
		upd, _ := m.Update(m.load()())
		return upd.(*Model)
	}

	for _, tc := range []struct {
		name     string
		wantKind string
		source   bool
	}{
		{"reconciler opens its inventory", "flux-detail/", false},
		{"source opens its object detail", "object-detail/", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clear(opened)
			m := newTree(t)
			idx := -1
			for i, l := range m.lines {
				if l.kind == lineRow && m.rows[l.row].isSource == tc.source {
					idx = i
					break
				}
			}
			if idx < 0 {
				t.Fatalf("no %s row in the tree", tc.name)
			}
			m.selected = idx
			row, _ := m.selectedRow()

			next, cmd := m.Update(tea.KeyPressMsg{Text: "enter"})
			if next == tea.Model(m) {
				t.Fatalf("↵ on %s returned the same task — nothing was pushed, so esc has nothing to come back to", row.name)
			}
			if !opened[tc.wantKind+row.name] {
				t.Errorf("↵ opened %v, want %s%s", opened, tc.wantKind, row.name)
			}
			// A BackMsg anywhere in the returned command would pop the tree
			// off the stack on the way out.
			if cmd != nil {
				if _, isBack := cmd().(tui.BackMsg); isBack {
					t.Error("↵ emitted a BackMsg — the tree is popped and esc can't return to it")
				}
			}
		})
	}
}

// TestSourceVerbStaysOnTheScreen: on a list, 'o' has to leave (the source is
// a different kind, so a different list). On the tree the source is the
// row's own parent, already on screen — so 'o' moves the cursor to it and
// navigates nowhere, which is both more useful and impossible to get lost
// from.
func TestSourceVerbStaysOnTheScreen(t *testing.T) {
	m := demoModel(t)
	idx := -1
	for i, l := range m.lines {
		if l.kind == lineRow && !m.rows[l.row].isSource && m.rows[l.row].sourceName != "" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("no reconciler row with a resolved source")
	}
	m.selected = idx
	row, _ := m.selectedRow()

	next, cmd := m.Update(tea.KeyPressMsg{Text: "o"})
	if next != tea.Model(m) {
		t.Error("'o' must not push or replace the screen")
	}
	if cmd != nil {
		t.Errorf("'o' must not navigate, got a command returning %T", cmd())
	}
	landed, ok := m.selectedRow()
	if !ok || !landed.isSource || landed.name != row.sourceName {
		t.Errorf("'o' left the cursor on %+v, want the source %q", landed, row.sourceName)
	}
}

// TestEscapeReturnsToTheTreeThroughTheRootShell drives the actual thing the
// user does: open the tree, press ↵ on a row, press esc. The two halves of
// the contract live in different places — this screen has to *push* (return
// a new task) and the root shell has to pop on tui.BackMsg — so only a test
// that runs both proves the round trip.
func TestEscapeReturnsToTheTreeThroughTheRootShell(t *testing.T) {
	c := fake.NewDemo()
	reg, groups := resources.BuildDiscoveredRegistry(c.DiscoveredKinds(), c)
	sess := &tui.Session{
		Theme: tui.Dark(), Registry: reg, Groups: groups,
		Location: tui.Location{Context: "microk8s-cluster", Namespace: "flux-system"},
	}
	tree := New(Config{
		Session: sess, Lister: c, Mutator: c,
		OpenFluxDetail: func(kube.ResourceKind, string, string, int, int) (tea.Model, tea.Cmd) {
			return &pushedTask{}, nil
		},
		OpenObjectDetail: func(kube.ResourceKind, string, string, int, int) (tea.Model, tea.Cmd) {
			return &pushedTask{}, nil
		},
	})
	tree.SetSize(120, 36)
	loaded, _ := tree.Update(tree.load()())

	root := tui.NewWithSession(loaded.(*Model), sess)
	updated, _ := root.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	if view := plain(updated.(tui.Model).View().Content); !strings.Contains(view, "SOURCE / RECONCILER") {
		t.Fatalf("expected to start on the tree:\n%s", view)
	}

	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "enter"})
	if view := plain(updated.(tui.Model).View().Content); strings.Contains(view, "SOURCE / RECONCILER") {
		t.Fatalf("↵ did not open anything:\n%s", view)
	}

	// esc from the pushed screen — the real screens all answer it with a
	// BackMsg, which is what pushedTask models here.
	next, cmd := updated.(tui.Model).Update(tea.KeyPressMsg{Text: "esc"})
	if cmd != nil {
		next, _ = next.(tui.Model).Update(cmd())
	}
	view := plain(next.(tui.Model).View().Content)
	if !strings.Contains(view, "SOURCE / RECONCILER") {
		t.Fatalf("esc did not return to the Flux tree:\n%s", view)
	}
}

// pushedTask is a minimal tui.Task stand-in for the openers above — it
// answers esc with a tui.BackMsg, which is what every real pushed screen in
// the app does.
type pushedTask struct{}

func (pushedTask) Init() tea.Cmd { return nil }
func (p *pushedTask) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok && key.String() == "esc" {
		return p, func() tea.Msg { return tui.BackMsg{} }
	}
	return p, nil
}
func (pushedTask) View() tea.View      { return tea.NewView("pushed screen") }
func (p *pushedTask) SetSize(int, int) {}

func fluxObj(apiVersion, kind, ns, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     spec,
		"status": map[string]any{"conditions": []any{map[string]any{
			"type": "Ready", "status": "True", "reason": "Succeeded", "message": "ok",
		}}},
	}}
}

func seedSourceKind(c *fake.Cluster, kind, plural string) {
	c.SeedDiscovered(kube.DiscoveredKind{
		GVR:  schema.GroupVersionResource{Group: kube.FluxGroupSource, Version: "v1", Resource: plural},
		Kind: kind, Plural: plural, Group: kube.FluxGroupSource,
		Versions:    []kube.CRDVersion{{Name: "v1", Served: true, Storage: true}},
		Established: true, CRDName: plural + "." + kube.FluxGroupSource,
	})
}

// recordingLister notes every kind read, so a test can assert what the
// screen did *not* start.
type recordingLister struct {
	resources.RawLister
	read map[kube.ResourceKind]int
}

func (l *recordingLister) ListRaw(ctx context.Context, kind kube.ResourceKind, ns string) ([]runtime.Object, error) {
	l.read[kind]++
	return l.RawLister.ListRaw(ctx, kind, ns)
}

// TestChartRefReleasesJoinTheirSource: a HelmRelease that installs from
// spec.chartRef (an OCI chart, or a HelmChart) must nest under the source it
// really comes from — never under a "source not found" row that claims a
// healthy chain is broken.
func TestChartRefReleasesJoinTheirSource(t *testing.T) {
	c := fake.NewDemo()
	seedSourceKind(c, "OCIRepository", "ocirepositories")
	seedSourceKind(c, "HelmChart", "helmcharts")
	c.Seed(kube.ResourceKind("OCIRepository"),
		fluxObj(kube.FluxGroupSource+"/v1", "OCIRepository", "flux-system", "gitlab-chart",
			map[string]any{"url": "oci://registry.gitlab.com/gitlab-org/charts/gitlab"}))
	c.Seed(kube.ResourceKind("HelmChart"),
		fluxObj(kube.FluxGroupSource+"/v1", "HelmChart", "flux-system", "runner",
			map[string]any{"chart": "gitlab-runner", "sourceRef": map[string]any{"kind": "HelmRepository", "name": "bitnami"}}))
	c.Seed(kube.KindFluxHelmRelease,
		fluxObj(kube.FluxGroupHelm+"/v2", "HelmRelease", "gitlab", "gitlab",
			map[string]any{"chartRef": map[string]any{"kind": "OCIRepository", "name": "gitlab-chart", "namespace": "flux-system"}}),
		fluxObj(kube.FluxGroupHelm+"/v2", "HelmRelease", "gitlab", "gitlab-runner",
			map[string]any{"chartRef": map[string]any{"kind": "HelmChart", "name": "runner", "namespace": "flux-system"}}),
	)
	reg, groups := resources.BuildDiscoveredRegistry(c.DiscoveredKinds(), c)
	sess := &tui.Session{Theme: tui.Dark(), Registry: reg, Groups: groups}
	m := New(Config{Session: sess, Lister: c})
	m.SetSize(120, 36)
	upd, _ := m.Update(m.load()())
	got := upd.(*Model)

	parent := map[string]string{}
	for _, g := range got.groups {
		for _, ch := range g.children {
			parent[ch.namespace+"/"+ch.name] = g.head.kindLabel + "/" + g.head.name
			if g.head.missing && ch.namespace == "gitlab" {
				t.Errorf("%s landed under a missing source: %s", ch.name, g.head.subLine)
			}
		}
	}
	if p := parent["gitlab/gitlab"]; p != "OCIRepo/gitlab-chart" {
		t.Errorf("chartRef OCIRepository release nested under %q", p)
	}
	if p := parent["gitlab/gitlab-runner"]; p != "HelmRepo/bitnami" {
		t.Errorf("chartRef HelmChart release nested under %q, want the HelmChart's own repository", p)
	}
}

// TestHelmChartIsReadOnlyWhenAReleaseNamesOne: HelmChart is a hop, not a
// source; the common tree must not start its informer.
func TestHelmChartIsReadOnlyWhenAReleaseNamesOne(t *testing.T) {
	c := fake.NewDemo()
	seedSourceKind(c, "HelmChart", "helmcharts")
	reg, groups := resources.BuildDiscoveredRegistry(c.DiscoveredKinds(), c)
	sess := &tui.Session{Theme: tui.Dark(), Registry: reg, Groups: groups}
	l := &recordingLister{RawLister: c, read: map[kube.ResourceKind]int{}}
	m := New(Config{Session: sess, Lister: l})
	m.Update(m.load()())
	if n := l.read["HelmChart"]; n != 0 {
		t.Errorf("HelmChart read %d times with no chartRef release in the cluster", n)
	}
}

func TestAppliesLineSummarisesTheInventoryByNamespace(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []string
		want string
	}{
		{"none", nil, ""},
		{"one", []string{"gitlab_web_apps_Deployment"}, "applies 1 object · gitlab 1"},
		{
			"namespace object counts toward the namespace it creates",
			[]string{"_gitlab__Namespace", "gitlab_web_apps_Deployment", "_view_rbac.authorization.k8s.io_ClusterRole"},
			"applies 3 objects · gitlab 2",
		},
		{
			"largest first, rest folded",
			[]string{"a_x__ConfigMap", "b_x__ConfigMap", "b_y__ConfigMap", "c_x__ConfigMap", "c_y__ConfigMap", "c_z__ConfigMap", "d_x__ConfigMap", "e_x__ConfigMap"},
			"applies 8 objects · c 3 · b 2 · a 1 · +2 namespaces",
		},
	} {
		if got := appliesLine(tc.ids); got != tc.want {
			t.Errorf("%s: appliesLine = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// kindErrLister reports a fixed error for some kinds, the way *kube.Cluster
// does for a kind whose cache can't fill.
type kindErrLister struct {
	resources.RawLister
	errs map[kube.ResourceKind]error
}

func (l kindErrLister) KindError(kind kube.ResourceKind, _ string) error { return l.errs[kind] }

// ListRaw answers an erroring kind with the empty cache it really has.
func (l kindErrLister) ListRaw(ctx context.Context, kind kube.ResourceKind, ns string) ([]runtime.Object, error) {
	if _, failing := l.errs[kind]; failing {
		return []runtime.Object{}, nil
	}
	return l.RawLister.ListRaw(ctx, kind, ns)
}

func kindErrTree(t *testing.T, errs func(reg resources.Registry) map[kube.ResourceKind]error) (*Model, tea.Cmd) {
	t.Helper()
	c := fake.NewDemo()
	reg, groups := resources.BuildDiscoveredRegistry(c.DiscoveredKinds(), c)
	sess := &tui.Session{Theme: tui.Dark(), Registry: reg, Groups: groups}
	m := New(Config{Session: sess, Lister: kindErrLister{RawLister: c, errs: errs(reg)}})
	m.SetSize(160, 36)
	upd, cmd := m.Update(m.load()())
	return upd.(*Model), cmd
}

// TestOneUnreadableFluxKindDegradesTheTree is M6: one kind this identity
// may not list turned the whole tree into an error that promised a retry
// nothing scheduled. It now drops out of the join, the strip names it, and
// a reconciler whose source it hides isn't called broken.
func TestOneUnreadableFluxKindDegradesTheTree(t *testing.T) {
	forbidden := errors.New(`helmrepositories.source.toolkit.fluxcd.io is forbidden: User "u" cannot list resource`)
	got, cmd := kindErrTree(t, func(resources.Registry) map[kube.ResourceKind]error {
		return map[kube.ResourceKind]error{"HelmRepository": forbidden}
	})
	if got.state != tui.TaskStateReady {
		t.Fatalf("state = %s (%s), want ready", got.state, got.feedback)
	}
	if cmd != nil {
		t.Error("a denial alone must not schedule a retry")
	}
	got.expanded = true
	got.recomputeVisible()
	view := plain(got.Render())
	if !strings.Contains(view, "not shown") || !strings.Contains(view, "forbidden") {
		t.Errorf("the strip should name the unreadable kind:\n%s", view)
	}
	if !strings.Contains(view, "HelmRepository: forbidden") || !strings.Contains(view, "source unreadable: helmrepository/bitnami") {
		t.Errorf("a source kute can't read should say so:\n%s", view)
	}
	if !strings.Contains(view, "nebula-workers") {
		t.Errorf("the readable kinds should still render:\n%s", view)
	}
	if strings.Contains(view, "source not found: helmrepository") {
		t.Errorf("a source kute can't read must not be reported missing:\n%s", view)
	}

	// A stalled kind — not a denial — is what "retrying" can honestly
	// promise, so that is when a retry is scheduled.
	_, cmd = kindErrTree(t, func(resources.Registry) map[kube.ResourceKind]error {
		return map[kube.ResourceKind]error{"HelmRepository": errors.New("the server is currently unable to handle the request")}
	})
	if cmd == nil {
		t.Error("a stalled kind should schedule a retry")
	}

	// Every kind failing is still the error state.
	all, _ := kindErrTree(t, func(reg resources.Registry) map[kube.ResourceKind]error {
		out := map[kube.ResourceKind]error{}
		for _, k := range reg.Kinds() {
			if d, _ := reg.Descriptor(k); d.Flux {
				out[k] = forbidden
			}
		}
		return out
	})
	if all.state != tui.TaskStateError {
		t.Errorf("state = %s, want error when no Flux kind can be read", all.state)
	}
}

// TestReconcileUnderAMissingSourceSkipsWithSource: a reconciler whose
// sourceRef resolved to nothing has no source to stamp — doing so would
// only fail — so its 'r' is the plain one-command reconcile.
func TestReconcileUnderAMissingSourceSkipsWithSource(t *testing.T) {
	m := demoModel(t)
	gi := -1
	for i, g := range m.groups {
		if len(g.children) > 0 && g.children[0].sourceName != "" {
			gi = i
			break
		}
	}
	if gi < 0 {
		t.Fatal("need a group with a reconciler")
	}
	child := m.groups[gi].children[0]
	m.groups[gi].head = missingSourceRow(child, nil)
	m.expanded = true
	m.recomputeVisible()
	for i, l := range m.lines {
		if l.kind == lineRow && m.rows[l.row].name == child.name && m.rows[l.row].namespace == child.namespace && !m.rows[l.row].isSource {
			m.selected = i
		}
	}
	if row, _ := m.selectedRow(); row.name != child.name {
		t.Fatalf("could not select %s", child.name)
	}
	m.Update(tea.KeyPressMsg{Text: "r"})
	if strings.Contains(m.execFeedback, "&&") || !strings.Contains(m.execFeedback, child.name) {
		t.Errorf("a reconciler under a missing source reconciles alone, got %q", m.execFeedback)
	}
}

// TestStaleLoadNeverOverwritesANewerTree: watch-triggered joins race, and
// the slowest reply isn't necessarily the oldest — one issued before the
// tree on screen must not put its older join back.
func TestStaleLoadNeverOverwritesANewerTree(t *testing.T) {
	m := demoModel(t)
	stale := m.load()()
	m.startLoad()
	fresh := m.load()()
	m.Update(fresh)
	want := len(m.groups)
	if want == 0 {
		t.Fatal("demo should have groups")
	}
	staleMsg := stale.(loadedMsg)
	staleMsg.groups = nil
	m.Update(staleMsg)
	if len(m.groups) != want || m.state != tui.TaskStateReady {
		t.Fatalf("a stale load replaced the tree: %d groups, state %v", len(m.groups), m.state)
	}
}
