package tui_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/state"
	"github.com/kute-dev/kute/internal/testutil/testenv"
	"github.com/kute-dev/kute/internal/tui"
)

const contextTestKubeconfig = `
apiVersion: v1
kind: Config
current-context: dev
contexts:
- name: dev
  context: {cluster: dev, namespace: default}
- name: prod-eks
  context: {cluster: prod, namespace: prod}
clusters:
- name: dev
  cluster: {server: https://dev.example.invalid}
- name: prod
  cluster: {server: https://prod.example.invalid}
users: []
`

// writeContextTestKubeconfig points $KUBECONFIG at a fixture with two named
// contexts, so kube.AvailableContexts() (which context.go reads directly —
// there's no injectable seam for it) is deterministic in tests. Not safe to
// run with t.Parallel() before this, per testing.T.Setenv's restriction.
func writeContextTestKubeconfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(contextTestKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
}

// threeContextTestKubeconfig adds a third context (stage-eks) beyond
// contextTestKubeconfig's two — needed to exercise digit "1" meaningfully
// now that current and previous are both excluded from the numbered pick
// (TestRootModelContextDigitPicksRecent needs a *third* context to land a
// digit on).
const threeContextTestKubeconfig = `
apiVersion: v1
kind: Config
current-context: dev
contexts:
- name: dev
  context: {cluster: dev, namespace: default}
- name: prod-eks
  context: {cluster: prod, namespace: prod}
- name: stage-eks
  context: {cluster: stage, namespace: stage}
clusters:
- name: dev
  cluster: {server: https://dev.example.invalid}
- name: prod
  cluster: {server: https://prod.example.invalid}
- name: stage
  cluster: {server: https://stage.example.invalid}
users: []
`

func writeThreeContextTestKubeconfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(threeContextTestKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
}

// TestRootModelCOpensContextPaletteWithProdTagAndCurrent drives 'c' end to
// end (docs/design README.md §7a): both kubeconfig contexts list, the
// current one tagged, the configured PROD context tagged, and reachability
// starts "probing…" before any result has streamed back.
func TestRootModelCOpensContextPaletteWithProdTagAndCurrent(t *testing.T) {
	writeContextTestKubeconfig(t)

	sess := &tui.Session{
		Theme:    tui.Dark(),
		Location: tui.Location{Context: "dev"},
		Config:   config.Config{ProdContexts: []string{"prod-eks"}},
		State:    state.State{PerContext: map[string]state.PerContext{}},
	}
	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	m := updated.(tui.Model)
	view := m.View().Content

	for _, want := range []string{"dev", "prod-eks", "current", "PROD", "probing"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected %q in the context palette:\n%s", want, view)
		}
	}
	if m.Mode() != tui.ModeGoto {
		t.Fatalf("Mode() = %v, want ModeGoto while the context palette is open", m.Mode())
	}
}

// TestRootModelContextCtrlPTogglesProdAndPersists drives ctrl+p end to end
// (docs/design README.md §7a): toggling a non-prod context on shows PROD in
// the view and persists it via config.SetProd (so a reload of the same
// config.Path() sees it), and a second ctrl+p removes it again. The palette
// must stay open and the toggled row selected throughout — unlike "r"
// re-probe/refreshContextPalette, which resets Sel to the alt-tab target.
func TestRootModelContextCtrlPTogglesProdAndPersists(t *testing.T) {
	writeContextTestKubeconfig(t)
	testenv.SetHome(t, t.TempDir())

	sess := &tui.Session{
		Theme:    tui.Dark(),
		Location: tui.Location{Context: "dev"},
		State:    state.State{PerContext: map[string]state.PerContext{}},
	}
	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyDown}) // land on prod-eks
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	m := updated.(tui.Model)

	if !m.PaletteOpen() {
		t.Fatalf("ctrl+p must not close the palette")
	}
	if view := m.View().Content; !strings.Contains(view, "PROD") {
		t.Fatalf("expected PROD tag in the palette after ctrl+p:\n%s", view)
	}
	if reloaded := config.Load(); !reloaded.IsProd("prod-eks") {
		t.Fatalf("expected prod-eks persisted via config.SetProd, got %+v", reloaded)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	m = updated.(tui.Model)
	if view := m.View().Content; strings.Contains(view, "PROD") {
		t.Fatalf("expected PROD tag gone after a second ctrl+p:\n%s", view)
	}
	if config.Load().IsProd("prod-eks") {
		t.Fatalf("expected prod-eks unmarked in config.Path() after a second ctrl+p")
	}
}

// TestRootModelContextCapitalPIsQueryText is the regression for mark-prod
// being a bare 'P': typing a context name that starts with a capital P
// rewrote config.yaml's prodContexts for whichever row was selected instead
// of narrowing the fuzzy query.
func TestRootModelContextCapitalPIsQueryText(t *testing.T) {
	writeContextTestKubeconfig(t)
	testenv.SetHome(t, t.TempDir())

	sess := &tui.Session{
		Theme:    tui.Dark(),
		Location: tui.Location{Context: "dev"},
		State:    state.State{PerContext: map[string]state.PerContext{}},
	}
	model := tui.NewWithSession(&screenTask{name: "browse"}, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyDown}) // land on prod-eks
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Code: 'p', ShiftedCode: 'P', Mod: tea.ModShift, Text: "P"})
	m := updated.(tui.Model)

	if !m.PaletteOpen() {
		t.Fatalf("typing P must not close the palette")
	}
	if len(config.Load().ProdContexts) != 0 {
		t.Fatalf("typing P marked a context PROD: %+v", config.Load().ProdContexts)
	}
	// The query narrowed to prod-eks (stage-eks has no 'p'), proving the
	// key reached the fuzzy input rather than a verb.
	if view := m.View().Content; strings.Contains(view, "PROD") || strings.Contains(view, "stage-eks") {
		t.Fatalf("expected P typed into the query and no PROD tag:\n%s", view)
	}
}

// TestRootModelContextEnterWithoutLiveClusterIsNoop covers --demo/no-cluster
// sessions (Session.Cluster nil): selecting a context still closes the
// palette but must not panic or return a cmd — there is nothing to rebuild.
func TestRootModelContextEnterWithoutLiveClusterIsNoop(t *testing.T) {
	writeContextTestKubeconfig(t)

	sess := &tui.Session{
		Theme:    tui.Dark(),
		Location: tui.Location{Context: "dev"},
		State:    state.State{PerContext: map[string]state.PerContext{}},
	}
	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	// Move off the current context ("dev", pre-selected) onto "prod-eks" so
	// Enter exercises the no-live-cluster branch rather than the
	// already-current no-op.
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyDown})
	updated, cmd := updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m := updated.(tui.Model)

	if m.PaletteOpen() {
		t.Fatalf("expected enter to close the palette even with no live cluster")
	}
	if cmd != nil {
		t.Fatalf("expected no cmd from selecting a context with Session.Cluster == nil")
	}
}

// TestRootModelContextPaletteOpensWithLastOtherPreselected covers the
// alt-tab grammar that replaced the old "c c" double-tap (docs/design
// README.md §7a): opening the context palette pre-selects the
// second-most-recent context (recentContexts[0] is always current — see
// mostRecentOther), so a bare "c" + enter toggles straight to it.
// switchContextCmd records the target in Session.State.RecentContexts
// synchronously (before the returned tea.Cmd's blocking SwitchContext ever
// runs), so that's checked directly rather than invoking the cmd.
func TestRootModelContextPaletteOpensWithLastOtherPreselected(t *testing.T) {
	writeContextTestKubeconfig(t)

	sess := &tui.Session{
		Theme:    tui.Dark(),
		Location: tui.Location{Context: "dev"},
		State:    state.State{PerContext: map[string]state.PerContext{}, RecentContexts: []string{"dev", "prod-eks"}},
	}
	sess.Cluster = &kube.Cluster{}
	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	updated, cmd := updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m := updated.(tui.Model)

	if m.PaletteOpen() {
		t.Fatalf("expected enter to close the palette")
	}
	if cmd == nil {
		t.Fatalf("expected a switch-context cmd since prod-eks (not the current dev) should be pre-selected")
	}
	if got := sess.State.RecentContexts[0]; got != "prod-eks" {
		t.Fatalf("RecentContexts[0] = %q, want prod-eks (the alt-tab target committed on enter)", got)
	}
}

// TestRootModelContextSwitchPreservesOutgoingRecentNamespaces pins a clobber
// bug: switchContextCmd used to persist the outgoing context's
// Namespace/Kind/Filter with a fresh state.PerContext{} literal, silently
// wiping that context's RecentNamespaces (namespaces' recents live per
// context, unlike kinds/contexts — see state.State's doc comment) every time
// you switched away from it. Mirrors
// TestRootModelContextPaletteOpensWithLastOtherPreselected's pattern:
// switchContextCmd's Session.State writes happen synchronously before the
// returned tea.Cmd's blocking SwitchContext ever runs, so they're checked
// directly without invoking cmd().
func TestRootModelContextSwitchPreservesOutgoingRecentNamespaces(t *testing.T) {
	writeContextTestKubeconfig(t)

	sess := &tui.Session{
		Theme:    tui.Dark(),
		Location: tui.Location{Context: "dev", Namespace: "default"},
		State: state.State{PerContext: map[string]state.PerContext{
			"dev": {RecentNamespaces: []string{"default", "kube-system"}},
		}},
	}
	sess.Cluster = &kube.Cluster{}
	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyDown})
	updated, cmd := updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m := updated.(tui.Model)

	if m.PaletteOpen() {
		t.Fatalf("expected enter to close the palette")
	}
	if cmd == nil {
		t.Fatalf("expected a switch-context cmd for prod-eks")
	}
	want := []string{"default", "kube-system"}
	got := sess.State.PerContext["dev"].RecentNamespaces
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("PerContext[dev].RecentNamespaces = %v, want %v (must survive switching away from dev)", got, want)
	}
}

// TestRootModelContextSwitchSeedsOutgoingContext pins a companion bug to
// TestRootModelContextSwitchPreservesOutgoingRecentNamespaces above:
// switchContextCmd used to push only the destination context into
// RecentContexts, never the context being left — so a context only ever
// entered RecentContexts by being switched *to*. The context the app
// launched into (RecentContexts starts empty) had no "previous" to
// alt-tab back to on the very next "c <enter>". Confirms both the
// destination and the outgoing context land in RecentContexts,
// most-recent-first.
func TestRootModelContextSwitchSeedsOutgoingContext(t *testing.T) {
	writeContextTestKubeconfig(t)

	sess := &tui.Session{
		Theme:    tui.Dark(),
		Location: tui.Location{Context: "dev", Namespace: "default"},
		State:    state.State{PerContext: map[string]state.PerContext{}},
	}
	sess.Cluster = &kube.Cluster{}
	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, cmd := updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd == nil {
		t.Fatalf("expected a switch-context cmd for prod-eks")
	}
	want := []string{"prod-eks", "dev"}
	got := sess.State.RecentContexts
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("RecentContexts = %v, want %v (must seed the outgoing context too)", got, want)
	}
}

// TestRootModelContextDigitPicksRecent covers 7a's numbered RECENT-row
// shortcut (digitRecentTarget/recentNumbers): current and the
// immediately-previous context are both excluded from the numbering (each
// is already visible tagged "current"/"previous" on its own row — a digit
// for either would be redundant), so with
// RecentContexts = [dev(current), prod-eks(previous), stage-eks],
// "stage-eks" is digit 1 — typing a bare '1' jumps Sel straight to it and
// the footer names it before commit.
func TestRootModelContextDigitPicksRecent(t *testing.T) {
	writeThreeContextTestKubeconfig(t)

	sess := &tui.Session{
		Theme:    tui.Dark(),
		Location: tui.Location{Context: "dev"},
		State:    state.State{PerContext: map[string]state.PerContext{}, RecentContexts: []string{"dev", "prod-eks", "stage-eks"}},
	}
	sess.Cluster = &kube.Cluster{}
	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "1"})
	view := updated.(tui.Model).View().Content
	if !strings.Contains(view, "switches to") || !strings.Contains(view, "stage-eks") {
		t.Fatalf("expected a digit-select footer naming stage-eks:\n%s", view)
	}

	updated, cmd := updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m := updated.(tui.Model)
	if m.PaletteOpen() {
		t.Fatalf("expected enter to close the palette")
	}
	if cmd == nil {
		t.Fatalf("expected a switch-context cmd from the digit-picked recent")
	}
	if got := sess.State.RecentContexts[0]; got != "stage-eks" {
		t.Fatalf("RecentContexts[0] = %q, want stage-eks (the digit-picked target committed on enter)", got)
	}
}

// TestRootModelContextCtrlPRefusesToOverwriteBrokenConfig: ctrl+p against a
// config.yaml that failed to parse must leave the file byte-for-byte alone
// and say why in the palette, rather than writing back the parse remnant.
func TestRootModelContextCtrlPRefusesToOverwriteBrokenConfig(t *testing.T) {
	writeContextTestKubeconfig(t)
	testenv.SetHome(t, t.TempDir())
	const body = "# hand-edited\nprodContexts: [prod-eks\n"
	if err := os.MkdirAll(filepath.Dir(config.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	sess := &tui.Session{
		Theme:    tui.Dark(),
		Location: tui.Location{Context: "dev"},
		Config:   config.Load(),
		State:    state.State{PerContext: map[string]state.PerContext{}},
	}
	model := tui.NewWithSession(&screenTask{name: "browse"}, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	m := updated.(tui.Model)

	if view := m.View().Content; !strings.Contains(view, "config.yaml has errors") {
		t.Fatalf("expected the refusal in the palette footer:\n%s", view)
	}
	got, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("ctrl+p rewrote a config.yaml that failed to parse:\n%s", got)
	}
}

// TestRootModelContextCtrlPReportsWriteFailure: a ctrl+p whose write fails
// (here ~/.config/kute is a regular file, so nothing can be created under
// it) must leave the row's tag untouched and say why in the palette footer,
// rather than failing in silence (14 L1).
func TestRootModelContextCtrlPReportsWriteFailure(t *testing.T) {
	writeContextTestKubeconfig(t)
	testenv.SetHome(t, t.TempDir())
	dir := filepath.Dir(config.Path())
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	sess := &tui.Session{
		Theme:    tui.Dark(),
		Location: tui.Location{Context: "dev"},
		State:    state.State{PerContext: map[string]state.PerContext{}},
	}
	model := tui.NewWithSession(&screenTask{name: "browse"}, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyDown})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	m := updated.(tui.Model)

	view := m.View().Content
	if !strings.Contains(view, "couldn't save config.yaml") {
		t.Fatalf("expected the write failure in the palette footer:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "prod-eks") && strings.Contains(line, "PROD") {
			t.Fatalf("a failed write must not show a PROD tag:\n%s", view)
		}
	}
	if sess.Config.IsProd("prod-eks") {
		t.Fatalf("a failed write must leave the in-memory config untouched")
	}
}
