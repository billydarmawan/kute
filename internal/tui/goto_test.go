package tui_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

// gotoFakeLister is a minimal resources.RawLister fixture for the goto
// jump-palette data-building tests below — namespace-aware, since
// gotoCount/gotoResourceItems scope reads by namespace.
type gotoFakeLister struct {
	objs map[kube.ResourceKind][]runtime.Object
	// scoped backs tui.ScopedChecker for tests that need to simulate
	// --namespace-scoped mode; false (the zero value) for every test that
	// predates that mode.
	scoped bool
	// calls, when non-nil, records every ListRaw(kind, namespace) call — for
	// tests asserting a scoped-mode palette does *not* fan out one ListRaw
	// per namespace (docs/lazy-informers.md §5.6).
	calls *[]gotoListCall
}

type gotoListCall struct {
	kind      kube.ResourceKind
	namespace string
}

func (f gotoFakeLister) ListRaw(_ context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	if f.calls != nil {
		*f.calls = append(*f.calls, gotoListCall{kind: kind, namespace: namespace})
	}
	all := f.objs[kind]
	if namespace == "" {
		return all, nil
	}
	var out []runtime.Object
	for _, o := range all {
		if acc, ok := o.(metav1.Object); ok && acc.GetNamespace() == namespace {
			out = append(out, o)
		}
	}
	return out, nil
}

func (f gotoFakeLister) Scoped() bool { return f.scoped }

func gotoTestPod(ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Ready: true}}},
	}
}

func gotoTestSession(lister resources.RawLister) *tui.Session {
	return &tui.Session{
		Registry: resources.DefaultRegistry(),
		Groups:   resources.DefaultGroups(),
		Location: tui.Location{Context: "microk8s-cluster", Namespace: "default", Kind: kube.KindPod},
		Theme:    tui.Dark(),
		Lister:   lister,
	}
}

// TestRootModelGBrowseRankedListHasChipsCountsAndFooter drives the whole
// root-shell path ('g' → 12a ranked list) end to end: aliased kinds' labels
// and live per-kind counts render, and the static alias footer shows
// (docs/design README.md §12a).
func TestRootModelGBrowseRankedListHasChipsCountsAndFooter(t *testing.T) {
	t.Parallel()
	lister := gotoFakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {gotoTestPod("default", "api-1"), gotoTestPod("default", "api-2")},
	}}
	sess := gotoTestSession(lister)

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})
	// ansi.Strip: the aliased first letter renders as its own styled span,
	// so an un-stripped view would split "Pods"/"Deployments" across an
	// escape boundary between the highlighted letter and the rest.
	view := ansi.Strip(updated.(tui.Model).View().Content)

	for _, want := range []string{
		"Pods",        // kind row
		"Deployments", // another aliased daily kind
		"alias — colored first letter · typing it pins that kind to rank 1", // 12a's static footer
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected %q in the 12a ranked list:\n%s", want, view)
		}
	}
}

func TestRootModelGScrollsAllKindsWithinEqualMargins(t *testing.T) {
	t.Parallel()
	sess := gotoTestSession(gotoFakeLister{objs: map[kube.ResourceKind][]runtime.Object{}})
	model := tui.NewWithSession(&screenTask{name: "browse"}, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})
	view := ansi.Strip(updated.(tui.Model).View().Content)

	if strings.Contains(view, "more kinds") {
		t.Fatalf("did not expect an overflow trailer in the scrollable goto list:\n%s", view)
	}
	lines := strings.Split(view, "\n")
	bottom := -1
	for i, line := range lines {
		if strings.Contains(line, "╰") {
			bottom = i
			break
		}
	}
	if bottom != 21 {
		t.Fatalf("palette bottom row = %d, want 21 for two rows below a row-2 top in a 24-row terminal:\n%s", bottom, view)
	}

	for range 4 {
		updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "ctrl+d"})
	}
	view = ansi.Strip(updated.(tui.Model).View().Content)
	if !strings.Contains(view, "Helm Releases") {
		t.Fatalf("expected half-page scrolling to reach the final registered kind:\n%s", view)
	}
}

// TestRootModelGShowsGotoKeybarPill pins the cross-cutting fix (docs/design
// README.md §39: "Main keybar while open: GOTO mode pill + one-line
// explanation"): the underlying screen's own keybar pill must be replaced
// by a GOTO pill while the jump palette is open, not just dimmed to gray
// along with the rest of the background.
func TestRootModelGShowsGotoKeybarPill(t *testing.T) {
	t.Parallel()
	lister := gotoFakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {gotoTestPod("default", "api-1")},
	}}
	sess := gotoTestSession(lister)

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})
	view := updated.(tui.Model).View().Content

	if !strings.Contains(view, "GOTO") {
		t.Fatalf("expected the GOTO mode pill in the main keybar:\n%s", view)
	}
	if !strings.Contains(view, "jump to any kind, resource, namespace, or context") {
		t.Fatalf("expected the one-line explanation next to the pill:\n%s", view)
	}
}

// TestRootModelGThenTypeSwitchesToFuzzyResults covers 12a→2b: typing a
// multi-character query after 'g' switches from the ranked list to fuzzy
// results, which include both matching kinds and the current kind's
// resource names.
func TestRootModelGThenTypeSwitchesToFuzzyResults(t *testing.T) {
	t.Parallel()
	lister := gotoFakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {gotoTestPod("default", "api-1")},
	}}
	sess := gotoTestSession(lister)

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "a"})
	// ansi.Strip: fuzzy-matched characters render as their own styled
	// spans, so an un-stripped view can split "api-1" across an escape
	// boundary.
	view := ansi.Strip(updated.(tui.Model).View().Content)

	if !strings.Contains(view, "api-1") {
		t.Fatalf("expected the current kind's resource name in fuzzy results:\n%s", view)
	}
	if strings.Contains(view, "alias — colored first letter") {
		t.Fatalf("expected the 12a footer to be gone once typing starts ('a' isn't an alias):\n%s", view)
	}
}

// TestRootModelGThenTabCompletesToSelectedLabel covers 2b's advertised "tab
// complete" key: pressing tab fills the query in to the highlighted result's
// own label, so the input row itself gains a second, independent occurrence
// of that label (the first comes from the still-matching result row).
func TestRootModelGThenTabCompletesToSelectedLabel(t *testing.T) {
	t.Parallel()
	lister := gotoFakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {},
	}}
	sess := gotoTestSession(lister)

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})
	for _, ch := range []string{"e", "p", "l"} {
		updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: ch})
	}
	before := updated.(tui.Model).View().Content
	beforeCount := strings.Count(before, "Deployments")

	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyTab})
	after := updated.(tui.Model).View().Content
	afterCount := strings.Count(after, "Deployments")

	if afterCount <= beforeCount {
		t.Fatalf("expected tab to complete the query to the selected label (Deployments occurrences %d -> %d):\nbefore:\n%s\nafter:\n%s", beforeCount, afterCount, before, after)
	}
}

// TestRootModelGThenAliasLetterPinsMatchAndConfirmsFooter covers 12b: typing
// a single alias character pins the aliased kind to rank 1 (highlighted
// first letter + "alias match" label) and the footer confirms the jump
// destination.
func TestRootModelGThenAliasLetterPinsMatchAndConfirmsFooter(t *testing.T) {
	t.Parallel()
	lister := gotoFakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {},
	}}
	sess := gotoTestSession(lister)

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "d"})
	// ansi.Strip: the pinned kind's aliased first letter renders as its
	// own styled span, so an un-stripped view would split "Deployments"
	// across an escape boundary.
	view := ansi.Strip(updated.(tui.Model).View().Content)

	if !strings.Contains(view, "alias match") {
		t.Fatalf("expected the pinned row's 'alias match' label:\n%s", view)
	}
	if !strings.Contains(view, "jumps to Deployments in default") {
		t.Fatalf("expected the 12b destination-confirmation footer:\n%s", view)
	}
}

// TestRootModelGAliasLettersMatchFirstLetterOfNodesAndConfigMaps pins the
// alias rework in docs/design README.md §12a/§12b: every alias is the
// kind's own first letter, so Nodes moved from 'o' to 'n' and ConfigMaps
// from 'm' to 'c' — 'o' and 'm' are plain fuzzy queries now, not aliases.
func TestRootModelGAliasLettersMatchFirstLetterOfNodesAndConfigMaps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		key  string
		want string
	}{
		{"n", "jumps to Nodes in default"},
		{"c", "jumps to ConfigMaps in default"},
	} {
		sess := gotoTestSession(gotoFakeLister{})

		task := &screenTask{name: "browse"}
		model := tui.NewWithSession(task, sess)
		updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
		updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})
		updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: tc.key})
		// ansi.Strip: the pinned kind's aliased first letter renders as
		// its own styled span, so an un-stripped view can split the
		// destination footer's kind name across an escape boundary.
		view := ansi.Strip(updated.(tui.Model).View().Content)

		if !strings.Contains(view, tc.want) {
			t.Fatalf("key %q: expected %q in the 12b destination footer:\n%s", tc.key, tc.want, view)
		}
	}

	// 'o' and 'm' are no longer aliases — they must not pin anything to rank
	// 1 or show the destination-confirmation footer.
	for _, key := range []string{"o", "m"} {
		sess := gotoTestSession(gotoFakeLister{})
		task := &screenTask{name: "browse"}
		model := tui.NewWithSession(task, sess)
		updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
		updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})
		updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: key})
		view := updated.(tui.Model).View().Content

		if strings.Contains(view, "alias match") {
			t.Fatalf("key %q: did not expect an 'alias match' pin (o/m are retired aliases):\n%s", key, view)
		}
	}
}

// TestRootModelGEnterOnKindSwitchesBrowseKind exercises the full round trip:
// selecting a kind result and pressing enter should switch the underlying
// browse task's kind (verified indirectly through the rendered view, since
// the task is a real browse.Model wired the way app.NewModel wires it).
func TestRootModelGEnterOnKindSwitchesBrowseKind(t *testing.T) {
	t.Parallel()
	lister := gotoFakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod:       {gotoTestPod("default", "api-1")},
		kube.KindConfigMap: {},
	}}
	sess := gotoTestSession(lister)

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	m := updated.(tui.Model)
	m2, _ := m.Update(tea.KeyPressMsg{Text: "g"})
	m = m2.(tui.Model)

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(tui.Model)
	if cmd == nil {
		t.Fatalf("expected enter on the pre-selected kind to return a navigation cmd")
	}
	msg := cmd()
	kindMsg, ok := msg.(tui.GotoKindMsg)
	if !ok {
		t.Fatalf("expected a GotoKindMsg, got %T", msg)
	}
	if kindMsg.Kind != kube.KindPod {
		t.Fatalf("expected the first grid entry (Pods) to be pre-selected, got %v", kindMsg.Kind)
	}
	if m.PaletteOpen() {
		t.Fatalf("expected enter to close the palette")
	}
}

// TestRootModelGotoDispatchSeedsOutgoingKind pins a companion bug to
// TestRootModelContextSwitchPreservesOutgoingRecentNamespaces
// (context_test.go): switching kind via the goto palette used to push
// only the destination kind into RecentKinds, never the kind being left —
// so a kind only ever entered RecentKinds by being switched *to*. The
// kind browse opened on (RecentKinds starts empty) had no "previous" to
// alt-tab back to on the very next "g <enter>". Confirms both the
// destination and the outgoing kind land in RecentKinds, most-recent-first.
func TestRootModelGotoDispatchSeedsOutgoingKind(t *testing.T) {
	t.Parallel()
	lister := gotoFakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod:        {gotoTestPod("default", "api-1")},
		kube.KindDeployment: {},
	}}
	sess := gotoTestSession(lister) // Location.Kind = KindPod, RecentKinds empty

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})
	for _, ch := range []string{"d", "e", "p"} {
		updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: ch})
	}
	_, cmd := updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("expected a navigation cmd for Deployments")
	}
	msg, ok := cmd().(tui.GotoKindMsg)
	if !ok || msg.Kind != kube.KindDeployment {
		t.Fatalf("expected a GotoKindMsg for Deployments, got %#v", msg)
	}

	want := []string{string(kube.KindDeployment), string(kube.KindPod)}
	got := sess.State.RecentKinds
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("RecentKinds = %v, want %v (must seed the outgoing kind too)", got, want)
	}
}

// TestRootModelGBrowseOpensWithLastOtherKindPreselected covers the alt-tab
// grammar (docs/design README.md §2b: "g ↵ returns to the last kind"):
// recentKinds[0] is always the current kind (only a completed jump ever
// pushes to it), so the 12a ranked list must pre-select recentKinds[1] — the
// kind you were on before — not the current one, or "g ↵" would be a no-op.
func TestRootModelGBrowseOpensWithLastOtherKindPreselected(t *testing.T) {
	t.Parallel()
	lister := gotoFakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {gotoTestPod("default", "api-1")},
	}}
	sess := gotoTestSession(lister)
	sess.State.RecentKinds = []string{string(kube.KindPod), string(kube.KindDeployment)}

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})
	updated, cmd := updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m := updated.(tui.Model)

	if m.PaletteOpen() {
		t.Fatalf("expected enter to close the palette")
	}
	if cmd == nil {
		t.Fatalf("expected a goto cmd since Deployments (not the current Pods) should be pre-selected")
	}
	msg := cmd()
	kindMsg, ok := msg.(tui.GotoKindMsg)
	if !ok || kindMsg.Kind != kube.KindDeployment {
		t.Fatalf("expected GotoKindMsg{Kind: Deployment}, got %#v", msg)
	}
}

// pushableScreenTask is a screenTask that transitions to next on the
// synthetic "open" key, letting tests simulate a detail screen (e.g.
// poddetail) pushed on top of browse via the root shell's own
// push-on-different-task-instance mechanism (model.go's sameTask check).
type pushableScreenTask struct {
	screenTask
	next tui.Task
}

func (t *pushableScreenTask) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok && key.String() == "open" && t.next != nil {
		return t.next, nil
	}
	// Must return t itself (not t.screenTask.Update(msg)'s result, which
	// would be &t.screenTask — a different pointer than t, tricking the
	// root shell's sameTask check into treating every message as a push).
	t.screenTask.Update(msg)
	return t, nil
}

// sessionAwareBrowseStub mimics the one piece of real browse.Model
// behavior that plain screenTask doesn't: browse.New seeds its *own*
// starting kind from Session.Location.Kind at construction time, and
// browse.goToResource only actually reloads when a Goto message's kind
// differs from what the instance already has — an unrelated jump to the
// kind it already shows is a no-op by design. If routeGoto ever mutates
// Session.Location *before* calling buildBrowse, the fresh instance is
// born already "at" the destination and this short-circuit swallows the
// load that should have happened — the bug behind "jump gets stuck on the
// loading screen" (real browse.Model's own doc comment on this exact
// tradeoff: goToResource's early return when !kindChanged && !namespaceChanged).
type sessionAwareBrowseStub struct {
	screenTask
	kind   kube.ResourceKind
	loaded bool
}

func newSessionAwareBrowseStub(sess *tui.Session) *sessionAwareBrowseStub {
	return &sessionAwareBrowseStub{screenTask: screenTask{name: "browse"}, kind: sess.Location.Kind}
}

func (t *sessionAwareBrowseStub) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if goto_, ok := msg.(tui.GotoResourceMsg); ok {
		if goto_.Kind != t.kind {
			t.kind = goto_.Kind
			t.loaded = true
		}
		return t, nil
	}
	t.screenTask.Update(msg)
	return t, nil
}

// TestRouteGotoBuildsFreshBrowseBeforeMutatingSessionLocation pins the fix
// for a jump landing on a stuck loading screen: routeGoto must build the
// fresh browse view (via buildBrowse, which reads Session.Location.Kind/
// Namespace to seed the new instance's starting kind — same as a real
// browse.New) before model.go's own Session.Location write, or the fresh
// instance starts already "at" the destination and never loads anything.
func TestRouteGotoBuildsFreshBrowseBeforeMutatingSessionLocation(t *testing.T) {
	t.Parallel()
	sess := &tui.Session{Theme: tui.Dark(), Location: tui.Location{Context: "c", Kind: kube.KindPod}}

	detail := &screenTask{name: "poddetail"}
	browseTask := &pushableScreenTask{screenTask: screenTask{name: "browse"}, next: detail}
	var fresh *sessionAwareBrowseStub
	buildBrowse := func() tui.Task {
		fresh = newSessionAwareBrowseStub(sess)
		return fresh
	}

	model := tui.NewWithSession(browseTask, sess).WithRootFactories(nil, buildBrowse)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "open"}) // push poddetail
	m := updated.(tui.Model)

	// Simulate poddetail's RELATED digit firing a pre-filled jump to a
	// different kind — the exact tui.GotoResourceMsg shape tui.GotoResource
	// produces (poddetail's own openRelated).
	updated, _ = m.Update(tui.GotoResourceMsg{Kind: kube.KindIngress, Namespace: "default", Name: "api"})
	_ = updated.(tui.Model)

	if fresh == nil {
		t.Fatal("expected routeGoto to build a fresh browse task")
	}
	if !fresh.loaded {
		t.Fatal("fresh browse view never loaded — Session.Location was mutated before it was built, so the fresh instance started already \"at\" the destination and its own change-detection saw nothing to do")
	}
}

// TestRootModelGotoFromPushedScreenPushesFreshBrowseAndPreservesHistory pins
// the fix for the goto palette discarding history from a pushed detail
// screen (e.g. poddetail): GotoKindMsg/GotoResourceMsg are only handled by
// the browse task, so picking a destination while a detail screen is active
// used to unwind the *entire* stack back to root before dispatching —
// discarding not just the detail screen but everything beneath it too.
// Now routeGoto builds a fresh browse view (via the WithRootFactories
// buildBrowse factory) and pushes the screen the palette was opened from
// onto the stack beneath it, so one Escape returns to exactly where 'g' was
// pressed, and a second Escape reaches the original browse task untouched
// — "one jump = one esc back," the same contract a live push already gives.
func TestRootModelGotoFromPushedScreenPushesFreshBrowseAndPreservesHistory(t *testing.T) {
	t.Parallel()
	lister := gotoFakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {gotoTestPod("default", "api-1")},
	}}
	sess := gotoTestSession(lister)

	detail := &screenTask{name: "poddetail"}
	browseTask := &pushableScreenTask{screenTask: screenTask{name: "browse"}, next: detail}
	freshBrowses := 0
	buildBrowse := func() tui.Task {
		freshBrowses++
		return &screenTask{name: "browse"}
	}

	model := tui.NewWithSession(browseTask, sess).WithRootFactories(nil, buildBrowse)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "open"})
	m := updated.(tui.Model)
	if !strings.Contains(m.View().Content, "poddetail") {
		t.Fatalf("expected poddetail pushed on top of browse:\n%s", m.View().Content)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Text: "g"})
	m = updated.(tui.Model)
	if !m.PaletteOpen() {
		t.Fatalf("expected 'g' to open the goto palette from a pushed screen")
	}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(tui.Model)
	if cmd == nil {
		t.Fatalf("expected enter on the pre-selected kind to return a navigation cmd")
	}
	// Picking a destination doesn't touch the stack by itself — only once
	// the dispatched message actually arrives (below) does routeGoto act,
	// same as any other tea.Cmd-driven navigation.
	if !strings.Contains(m.View().Content, "poddetail") {
		t.Fatalf("expected poddetail still active until the goto message arrives:\n%s", m.View().Content)
	}

	msg := cmd()
	if _, ok := msg.(tui.GotoKindMsg); !ok {
		t.Fatalf("expected a GotoKindMsg, got %T", msg)
	}
	updated, _ = m.Update(msg)
	m = updated.(tui.Model)
	if freshBrowses != 1 {
		t.Fatalf("expected routeGoto to build exactly one fresh browse task, got %d", freshBrowses)
	}
	if len(browseTask.updates) != 0 {
		t.Fatalf("expected the original browse task untouched by the jump, got %d forwards", len(browseTask.updates))
	}
	if !strings.Contains(m.View().Content, "browse") {
		t.Fatalf("expected the fresh browse view active after the jump:\n%s", m.View().Content)
	}

	// esc back once returns to poddetail — the screen the palette was
	// opened from, one level away exactly as a live push would give.
	updated, _ = m.Update(tui.BackMsg{})
	m = updated.(tui.Model)
	if !strings.Contains(m.View().Content, "poddetail") {
		t.Fatalf("expected the first esc back to land on poddetail:\n%s", m.View().Content)
	}

	// esc back again reaches the *original* browse task, still intact.
	updated, _ = m.Update(tui.BackMsg{})
	m = updated.(tui.Model)
	if !strings.Contains(m.View().Content, "browse") {
		t.Fatalf("expected the second esc back to land on the original browse task:\n%s", m.View().Content)
	}
}

// TestRootModelGCtrlJKNavigateRankedListWithoutTyping covers the ctrl-modified
// vim keys added alongside the arrow keys: since a ctrl-modified press never
// carries Key.Text, it can move the 12a ranked list's selection without also
// being typeable into the "type to narrow" query (unlike plain j/k, which
// must stay reserved for typing — see handlePaletteKey's comment in
// model.go). Round-trips ctrl+j/ctrl+k down the ranked list (Pods ->
// Deployments -> back to Pods).
func TestRootModelGCtrlJKNavigateRankedListWithoutTyping(t *testing.T) {
	t.Parallel()
	sess := gotoTestSession(gotoFakeLister{})

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})

	// ansi.Strip: the aliased first letter renders as its own styled
	// span, so an un-stripped view would split "Pods"/"Deployments"
	// across an escape boundary.
	view := ansi.Strip(updated.(tui.Model).View().Content)
	if !isSelectedLine(view, "Pods") {
		t.Fatalf("expected Pods pre-selected after 'g':\n%s", view)
	}

	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	m := updated.(tui.Model)
	view = ansi.Strip(m.View().Content)
	if !isSelectedLine(view, "Deployments") {
		t.Fatalf("expected ctrl+j to move selection to Deployments:\n%s", view)
	}
	if !m.PaletteOpen() {
		t.Fatalf("palette should still be open — ctrl+j must not have been typed into the query")
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	m = updated.(tui.Model)
	view = ansi.Strip(m.View().Content)
	if !isSelectedLine(view, "Pods") {
		t.Fatalf("expected ctrl+k to move selection back to Pods:\n%s", view)
	}
}

// isSelectedLine reports whether view has a line carrying both label and
// the palette's 1-cell selection-bar glyph — i.e. label is the currently
// selected row, not just present somewhere in the panel (the input row's
// block cursor uses the same glyph, so a bare "▎" search isn't enough).
func isSelectedLine(view, label string) bool {
	for l := range strings.SplitSeq(view, "\n") {
		if strings.Contains(l, label) && strings.Contains(l, "▎") {
			return true
		}
	}
	return false
}

// countingLister records every cache read and answers counts server-side,
// the shape *kube.Cluster has once informers start lazily.
type countingLister struct {
	mu        sync.Mutex
	listCalls []kube.ResourceKind
	counts    map[kube.ResourceKind]int
}

func (l *countingLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	l.mu.Lock()
	l.listCalls = append(l.listCalls, kind)
	l.mu.Unlock()
	return nil, nil
}

func (l *countingLister) CountLive(_ context.Context, kind kube.ResourceKind, _ string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.counts[kind]
	if !ok {
		return 0, fmt.Errorf("no count for %s", kind)
	}
	return n, nil
}

func (l *countingLister) cacheReads() []kube.ResourceKind {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]kube.ResourceKind(nil), l.listCalls...)
}

// TestGotoPaletteCountsWithoutReadingCaches is the regression test for
// Phase 3: with informers starting on demand, counting every kind from its
// cache would start a watch per kind the instant 'g' was pressed — the
// launch stampede, relocated. The palette must count server-side instead.
func TestGotoPaletteCountsWithoutReadingCaches(t *testing.T) {
	lister := &countingLister{counts: map[kube.ResourceKind]int{
		kube.KindPod:        42,
		kube.KindDeployment: 7,
	}}
	sess := gotoTestSession(lister)

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, cmd := updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})

	// Opening the palette must not have read a single cache for counts.
	for _, kind := range lister.cacheReads() {
		t.Fatalf("opening the jump palette read the %s cache; counts must not touch informers", kind)
	}
	if cmd == nil {
		t.Fatal("expected a background count fetch to be scheduled")
	}

	// Counts land asynchronously and the palette re-renders with them.
	updated, _ = updated.(tui.Model).Update(cmd())
	view := updated.(tui.Model).View().Content
	if !strings.Contains(view, "42") {
		t.Fatalf("expected the fetched Pod count in the palette:\n%s", view)
	}
	// A kind the server wouldn't count reads as unknown, not as zero.
	if !strings.Contains(view, "–") {
		t.Fatalf("expected an en dash for a kind with no count available:\n%s", view)
	}
}

// countOnlyLister is what the real seam stack looks like once CountLive is
// forwarded: it counts server-side and records any cache read, so a test can
// prove the palette never falls back to measuring informer caches.
type countOnlyLister struct {
	mu        sync.Mutex
	listCalls []kube.ResourceKind
	synced    map[kube.ResourceKind]bool
}

func (l *countOnlyLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	l.mu.Lock()
	l.listCalls = append(l.listCalls, kind)
	l.mu.Unlock()
	return nil, nil
}

func (l *countOnlyLister) CountLive(_ context.Context, _ kube.ResourceKind, _ string) (int, error) {
	return 7, nil
}

// KindSynced answers false for anything not explicitly marked, standing in
// for a cluster where only the kinds you've visited have running informers.
func (l *countOnlyLister) KindSynced(kind kube.ResourceKind, _ string) bool { return l.synced[kind] }

func (l *countOnlyLister) reads() []kube.ResourceKind {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]kube.ResourceKind(nil), l.listCalls...)
}

// TestGotoPaletteOpensWithoutReadingAnyCache is the regression test for the
// jump palette hanging the app for a minute on a real cluster. Opening it
// built one row per registered kind, and each row's count fell back to
// counting an informer cache — which started that kind's informer, and for
// custom kinds also fetched its CRD synchronously on the update loop.
// Opening the palette must touch no cache at all.
func TestGotoPaletteOpensWithoutReadingAnyCache(t *testing.T) {
	lister := &countOnlyLister{synced: map[kube.ResourceKind]bool{}}
	sess := gotoTestSession(lister)

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	_, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})

	if reads := lister.reads(); len(reads) != 0 {
		t.Fatalf("opening the jump palette read %v; counts must come from CountLive, never a cache", reads)
	}
}

// TestGotoFuzzySearchSkipsUnstartedKinds: typing builds a corpus of object
// names, and listing a kind whose informer hasn't started would start it —
// the same stampede, one keystroke later.
func TestGotoFuzzySearchSkipsUnstartedKinds(t *testing.T) {
	// Pods plus the eager set, which is always started on a real cluster.
	lister := &countOnlyLister{synced: map[kube.ResourceKind]bool{
		kube.KindPod: true, kube.KindNamespace: true, kube.KindNode: true,
	}}
	sess := gotoTestSession(lister)

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "a"})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "p"})
	_, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "i"})

	for _, kind := range lister.reads() {
		switch kind {
		case kube.KindPod, kube.KindNamespace, kube.KindNode, kube.KindForward:
		default:
			t.Fatalf("fuzzy search read the %s cache, whose informer has not started", kind)
		}
	}
}

// unstartedCRDLister answers the sync seams the way *kube.Cluster does for a
// kind whose informer nothing has started: KindSynced says "settled" (the
// anti-hang rule — there is nothing to wait for), KindStarted says a read
// would start one. Only the eager set is started.
type unstartedCRDLister struct{ countOnlyLister }

func (l *unstartedCRDLister) KindSynced(kube.ResourceKind, string) bool { return true }

func (l *unstartedCRDLister) KindStarted(kind kube.ResourceKind, _ string) bool {
	switch kind {
	case kube.KindPod, kube.KindNamespace, kube.KindNode:
		return true
	}
	return false
}

// TestGotoFuzzySearchDoesNotStartTheCRDInformer is review 01 H1: before 14b
// had been opened, KindSynced vouched for KindCustomResourceDefinition, so
// the first fuzzy keystroke listed it and started the full-CRD informer.
// The corpus must ask whether a read is free (KindStarted), not only whether
// a cache is settled.
func TestGotoFuzzySearchDoesNotStartTheCRDInformer(t *testing.T) {
	lister := &unstartedCRDLister{}
	sess := gotoTestSession(lister)

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})
	_, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})

	for _, kind := range lister.reads() {
		if !lister.KindStarted(kind, "") {
			t.Fatalf("fuzzy search read the %s cache, whose informer has not started", kind)
		}
	}
}

// TestGotoOffersTheFluxTreeOnlyOnAFluxCluster is §30a/§30b's "zero chrome on
// a non-Flux cluster" applied to the one place §30b is reachable from. The
// entry has to appear the moment Flux CRDs are discovered and never before —
// a palette result that opens a screen with nothing to show is worse than no
// result at all.
func TestGotoOffersTheFluxTreeOnlyOnAFluxCluster(t *testing.T) {
	t.Parallel()
	// Matched on the item's own detail text, not the label: the palette
	// echoes the typed query, so "flux" is on screen either way.
	const fluxTreeDetail = "tree · sources"

	plain := gotoTestSession(gotoFakeLister{})
	if strings.Contains(gotoFuzzyLabels(t, plain, "flux"), fluxTreeDetail) {
		t.Error("a cluster with no Flux CRDs must not offer the Flux tree")
	}

	fluxy := gotoTestSession(gotoFakeLister{})
	reg, groups := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{{
		Kind: "Kustomization", Plural: "kustomizations", Group: kube.FluxGroupKustomize,
		GVR:         schema.GroupVersionResource{Group: kube.FluxGroupKustomize, Version: "v1", Resource: "kustomizations"},
		Versions:    []kube.CRDVersion{{Name: "v1", Served: true, Storage: true}},
		Established: true,
		CRDName:     "kustomizations." + kube.FluxGroupKustomize,
	}}, nil)
	fluxy.Registry, fluxy.Groups = reg, groups
	if !strings.Contains(gotoFuzzyLabels(t, fluxy, "flux"), fluxTreeDetail) {
		t.Error("a Flux cluster must offer the Flux tree in the goto palette")
	}
}

// gotoFuzzyLabels drives the real palette and returns its rendered results
// for query — the corpus is unexported, so this asserts through the same
// surface a user sees.
func gotoFuzzyLabels(t *testing.T, sess *tui.Session, query string) string {
	t.Helper()
	model := tui.NewWithSession(&screenTask{name: "browse"}, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})
	for _, r := range query {
		updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: string(r)})
	}
	return ansi.Strip(updated.(tui.Model).View().Content)
}

// inPlaceNamespaceTask is a pushed screen that owns its namespace (events,
// whocan) and so must receive SwitchNamespaceMsg itself.
type inPlaceNamespaceTask struct{ screenTask }

func (t *inPlaceNamespaceTask) SwitchesNamespaceInPlace() bool { return true }

// TestRootModelSwitchNamespaceFromObjectScreenPushesFreshBrowse pins the fix
// for the namespace palette doing nothing on an object-bound pushed screen
// (a Secret's Data view, pod detail): those screens ignore
// SwitchNamespaceMsg, so the root now routes it like a goto — a fresh browse
// pushed on top, the object screen one esc beneath it.
func TestRootModelSwitchNamespaceFromObjectScreenPushesFreshBrowse(t *testing.T) {
	t.Parallel()
	sess := gotoTestSession(gotoFakeLister{})
	detail := &screenTask{name: "secretdata"}
	browseTask := &pushableScreenTask{screenTask: screenTask{name: "browse"}, next: detail}
	var fresh *screenTask
	buildBrowse := func() tui.Task {
		fresh = &screenTask{name: "fresh-browse"}
		return fresh
	}

	model := tui.NewWithSession(browseTask, sess).WithRootFactories(nil, buildBrowse)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "open"})
	updated, _ = updated.(tui.Model).Update(tui.SwitchNamespaceMsg{Namespace: "other"})
	m := updated.(tui.Model)

	if fresh == nil || !strings.Contains(m.View().Content, "fresh-browse") {
		t.Fatalf("expected a fresh browse pushed over the object screen:\n%s", m.View().Content)
	}
	if sess.Location.Namespace != "other" {
		t.Fatalf("Session.Location.Namespace = %q, want other", sess.Location.Namespace)
	}
	updated, _ = m.Update(tui.BackMsg{})
	if !strings.Contains(updated.(tui.Model).View().Content, "secretdata") {
		t.Fatalf("expected esc to return to the object screen:\n%s", updated.(tui.Model).View().Content)
	}
}

func TestRootModelSwitchNamespaceStaysInPlaceOnNamespaceSwitcher(t *testing.T) {
	t.Parallel()
	sess := gotoTestSession(gotoFakeLister{})
	events := &inPlaceNamespaceTask{screenTask{name: "events"}}
	browseTask := &pushableScreenTask{screenTask: screenTask{name: "browse"}, next: events}
	built := 0
	buildBrowse := func() tui.Task { built++; return &screenTask{name: "fresh-browse"} }

	model := tui.NewWithSession(browseTask, sess).WithRootFactories(nil, buildBrowse)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "open"})
	updated, _ = updated.(tui.Model).Update(tui.SwitchNamespaceMsg{Namespace: "other"})
	m := updated.(tui.Model)

	if built != 0 || !strings.Contains(m.View().Content, "events") {
		t.Fatalf("expected events to keep the namespace switch in place (built=%d):\n%s", built, m.View().Content)
	}
}

// objectScreenTask is a pushed object-bound screen (secretdata, poddetail).
type objectScreenTask struct {
	screenTask
	namespace string
}

func (t *objectScreenTask) ScreenObject() (kube.ResourceKind, string, string) {
	return kube.KindSecret, t.namespace, "db"
}

// followingBrowseStub records the navigation message routeGoto hands it and,
// on "open", swaps itself for the followed object the way browse's
// resolveFollow does once rows land — reporting Transient from then on.
type followingBrowseStub struct {
	screenTask
	got      tea.Msg
	followed tui.Task
	away     bool
}

func (t *followingBrowseStub) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tui.FollowObjectMsg, tui.SwitchNamespaceMsg:
		t.got = msg
	case tea.KeyPressMsg:
		if msg.String() == "open" && t.followed != nil {
			t.away = true
			return t.followed, nil
		}
	}
	return t, nil
}

func (t *followingBrowseStub) Transient() bool { return t.away }

// TestRootModelSwitchNamespaceFromObjectScreenFollowsObject pins the
// follow-the-object contract: the fresh browse gets a FollowObjectMsg for the
// same name in the new namespace, and once it hands off to the followed
// object's screen, esc goes straight back to the original object rather than
// through the intermediate list.
func TestRootModelSwitchNamespaceFromObjectScreenFollowsObject(t *testing.T) {
	t.Parallel()
	sess := gotoTestSession(gotoFakeLister{})
	original := &objectScreenTask{screenTask: screenTask{name: "secretdata-qa"}, namespace: "qa"}
	browseTask := &pushableScreenTask{screenTask: screenTask{name: "browse"}, next: original}
	fresh := &followingBrowseStub{screenTask: screenTask{name: "fresh-browse"}, followed: &screenTask{name: "secretdata-stage"}}
	buildBrowse := func() tui.Task { return fresh }

	model := tui.NewWithSession(browseTask, sess).WithRootFactories(nil, buildBrowse)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "open"})
	updated, _ = updated.(tui.Model).Update(tui.SwitchNamespaceMsg{Namespace: "stage"})
	m := updated.(tui.Model)

	want := tui.FollowObjectMsg{Kind: kube.KindSecret, Namespace: "stage", Name: "db"}
	if fresh.got != want {
		t.Fatalf("fresh browse got %#v, want %#v", fresh.got, want)
	}
	if sess.Location.Namespace != "stage" || sess.Location.Kind != kube.KindSecret || sess.Location.Resource != "db" {
		t.Fatalf("Session.Location = %+v, want Secret db in stage", sess.Location)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Text: "open"})
	m = updated.(tui.Model)
	if !strings.Contains(m.View().Content, "secretdata-stage") {
		t.Fatalf("expected the followed object active:\n%s", m.View().Content)
	}
	updated, _ = m.Update(tui.BackMsg{})
	if !strings.Contains(updated.(tui.Model).View().Content, "secretdata-qa") {
		t.Fatalf("expected esc to skip the transient list and land on the original object:\n%s", updated.(tui.Model).View().Content)
	}
}

func TestRootModelSwitchNamespaceFromObjectScreenEdgeCases(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, target string
		wantBuilt    bool
		wantMsg      tea.Msg
	}{
		{"same namespace stays put", "qa", false, nil},
		{"all namespaces lands on the list", "", true, tui.SwitchNamespaceMsg{Namespace: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := gotoTestSession(gotoFakeLister{})
			original := &objectScreenTask{screenTask: screenTask{name: "secretdata-qa"}, namespace: "qa"}
			browseTask := &pushableScreenTask{screenTask: screenTask{name: "browse"}, next: original}
			var fresh *followingBrowseStub
			buildBrowse := func() tui.Task {
				fresh = &followingBrowseStub{screenTask: screenTask{name: "fresh-browse"}}
				return fresh
			}
			model := tui.NewWithSession(browseTask, sess).WithRootFactories(nil, buildBrowse)
			updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
			updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "open"})
			updated, _ = updated.(tui.Model).Update(tui.SwitchNamespaceMsg{Namespace: tc.target})
			if (fresh != nil) != tc.wantBuilt {
				t.Fatalf("fresh browse built = %v, want %v", fresh != nil, tc.wantBuilt)
			}
			if fresh != nil && fresh.got != tc.wantMsg {
				t.Fatalf("fresh browse got %#v, want %#v", fresh.got, tc.wantMsg)
			}
			if !tc.wantBuilt && !strings.Contains(updated.(tui.Model).View().Content, "secretdata-qa") {
				t.Fatalf("expected to stay on the object:\n%s", updated.(tui.Model).View().Content)
			}
		})
	}
}

// initRecordingTask records Init alongside screenTask's own conn/goto log,
// so a test can see the order the root drives a freshly built task in.
type initRecordingTask struct {
	screenTask
}

func (t *initRecordingTask) Init() tea.Cmd {
	t.updates = append(t.updates, "init")
	return nil
}

// Update returns t itself, not the promoted screenTask's &t.screenTask, so
// the root keeps holding this type (and its Init) after the conn replay.
func (t *initRecordingTask) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	t.screenTask.Update(msg)
	return t, nil
}

// TestRouteGotoInitsTheFreshTaskAfterConnReplay pins 13 H3: routeGoto
// builds a brand-new browse and must Init it — nothing else ever starts a
// fresh instance's load, so a jump to the kind the Session already names
// stayed on "Loading …" forever. Init runs after the connection replay
// (an offline screen must know it's offline before deciding to arm polls)
// and before the jump itself (so a real retarget supersedes Init's load).
func TestRouteGotoInitsTheFreshTaskAfterConnReplay(t *testing.T) {
	t.Parallel()
	sess := &tui.Session{Theme: tui.Dark(), Location: tui.Location{Context: "c", Kind: kube.KindPod}}

	detail := &screenTask{name: "poddetail"}
	browseTask := &pushableScreenTask{screenTask: screenTask{name: "browse"}, next: detail}
	var fresh *initRecordingTask
	buildBrowse := func() tui.Task {
		fresh = &initRecordingTask{screenTask: screenTask{name: "fresh-browse"}}
		return fresh
	}

	model := tui.NewWithSession(browseTask, sess).WithRootFactories(nil, buildBrowse)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(kube.ConnStateMsg{Phase: kube.ConnConnected})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "open"}) // push poddetail
	updated, _ = updated.(tui.Model).Update(tui.GotoKindMsg{Kind: kube.KindPod})
	_ = updated.(tui.Model)

	if fresh == nil {
		t.Fatal("expected routeGoto to build a fresh browse task")
	}
	want := []string{"conn", "init", "goto"}
	if strings.Join(fresh.updates, ",") != strings.Join(want, ",") {
		t.Fatalf("fresh task saw %v, want %v (Init after the conn replay, before the jump)", fresh.updates, want)
	}
}
