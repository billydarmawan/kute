package tui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

type staticTask struct {
	name string
	next tui.Task
}

func (t *staticTask) Init() tea.Cmd    { return nil }
func (t *staticTask) SetSize(int, int) {}
func (t *staticTask) View() tea.View   { return tea.NewView(t.name) }

func (t *staticTask) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok && key.String() == "open" && t.next != nil {
		return t.next, nil
	}
	return t, nil
}

func TestRootModelPreservesTaskUpdates(t *testing.T) {
	t.Parallel()

	task := &staticTask{name: "pods api"}
	model := tui.New(task)
	updated, _ := model.Update(tea.KeyPressMsg{Text: "noop"})
	view := updated.(tui.Model).View().Content
	if !strings.Contains(view, "pods api") {
		t.Fatalf("root model did not preserve task update:\n%s", view)
	}
}

func TestRootModelBackMessageRestoresPreviousTask(t *testing.T) {
	t.Parallel()

	logs := &staticTask{name: "Logs: api"}
	pods := &staticTask{name: "Pods api", next: logs}
	model := tui.New(pods)

	updated, _ := model.Update(tea.KeyPressMsg{Text: "open"})
	logsView := updated.(tui.Model).View().Content
	if !strings.Contains(logsView, "Logs: api") {
		t.Fatalf("logs view missing after open:\n%s", logsView)
	}

	updated, _ = updated.(tui.Model).Update(tui.BackMsg{})
	podsView := updated.(tui.Model).View().Content
	if !strings.Contains(podsView, "Pods api") {
		t.Fatalf("pods view missing after back:\n%s", podsView)
	}
}

// redirectingTask stands in for tasks/objectdetail's 14d redirect: it swaps
// itself out for another task from inside a plain (non-key) message and
// reports tui.Transient, asking the root to drop it rather than push it.
type redirectingTask struct {
	staticTask
	redirected bool
}

func (t *redirectingTask) Transient() bool { return t.redirected }

func (t *redirectingTask) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(tui.BackMsg); !ok && t.next != nil && !t.redirected {
		t.redirected = true
		return t.next, nil
	}
	return t, nil
}

// TestRootModelDropsTransientTaskFromStack pins the fix for an inescapable
// YAML view: a detail screen that redirects to tasks/yamlview during its own
// load must not sit under it, or esc pops back into the load that redirects
// again and Escape never walks back a level.
func TestRootModelDropsTransientTaskFromStack(t *testing.T) {
	t.Parallel()

	yamlTask := &staticTask{name: "YAML: order"}
	detail := &redirectingTask{staticTask: staticTask{name: "Order detail", next: yamlTask}}
	browse := &staticTask{name: "Orders list", next: detail}
	model := tui.New(browse)

	// browse pushes the detail screen, which immediately redirects.
	updated, _ := model.Update(tea.KeyPressMsg{Text: "open"})
	updated, _ = updated.(tui.Model).Update(kube.ResourceChangedMsg{})
	if view := updated.(tui.Model).View().Content; !strings.Contains(view, "YAML: order") {
		t.Fatalf("expected the redirect to land on the YAML view:\n%s", view)
	}

	updated, _ = updated.(tui.Model).Update(tui.BackMsg{})
	view := updated.(tui.Model).View().Content
	if !strings.Contains(view, "Orders list") {
		t.Fatalf("esc from a redirected YAML view must walk back to the list, got:\n%s", view)
	}
}

// reloadingTask is a staticTask that also implements tui.Reloader, so it
// exercises the root shell's BackMsg handling asking a just-restored task to
// refresh itself.
type reloadingTask struct {
	staticTask
	reloaded int
}

func (t *reloadingTask) Reload() tea.Cmd {
	t.reloaded++
	return nil
}

func TestRootModelBackMessageReloadsRestoredTask(t *testing.T) {
	t.Parallel()

	pods := &reloadingTask{staticTask: staticTask{name: "Pods api"}}
	logs := &staticTask{name: "Logs: api"}
	pods.next = logs
	model := tui.New(pods)

	updated, _ := model.Update(tea.KeyPressMsg{Text: "open"})
	if pods.reloaded != 0 {
		t.Fatalf("pushing a task must not reload it: got %d calls", pods.reloaded)
	}

	updated, _ = updated.(tui.Model).Update(tui.BackMsg{})
	podsView := updated.(tui.Model).View().Content
	if !strings.Contains(podsView, "Pods api") {
		t.Fatalf("pods view missing after back:\n%s", podsView)
	}
	if pods.reloaded != 1 {
		t.Fatalf("restoring a Reloader task via BackMsg must call Reload exactly once: got %d calls", pods.reloaded)
	}
}

type escapeBackTask struct{ staticTask }

func (*escapeBackTask) BackOnEscape() bool { return true }

// TestRootModelEscapeBackIsSynchronous pins the open → Escape → immediate
// reopen race: the key after Escape must reach the restored task without
// waiting for an asynchronously returned BackMsg command.
func TestRootModelEscapeBackIsSynchronous(t *testing.T) {
	t.Parallel()

	panel := &escapeBackTask{staticTask: staticTask{name: "debug-panel"}}
	pods := &staticTask{name: "Pods api", next: panel}
	model := tui.New(pods)

	updated, _ := model.Update(tea.KeyPressMsg{Text: "open"})
	updated, cmd := updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd != nil {
		// A resumed task may legitimately return Reload; this test's static
		// parent does not, so any command here would reintroduce the race.
		t.Fatal("ready Escape back returned an unexpected asynchronous command")
	}
	if view := updated.(tui.Model).View().Content; !strings.Contains(view, "Pods api") {
		t.Fatalf("Escape did not restore the parent synchronously:\n%s", view)
	}

	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "open"})
	if view := updated.(tui.Model).View().Content; !strings.Contains(view, "debug-panel") {
		t.Fatalf("immediate reopen key was swallowed:\n%s", view)
	}
}

// screenTask implements both tui.Task and tui.Screen (Chrome v2), so it
// exercises the root shell's overlay/mode routing — staticTask above
// deliberately does not, to guard the legacy-screen passthrough path.
type screenTask struct {
	name    string
	pill    string
	updates []string
}

func (t *screenTask) Init() tea.Cmd    { return nil }
func (t *screenTask) SetSize(int, int) {}
func (t *screenTask) View() tea.View   { return tea.NewView(t.name) }
func (t *screenTask) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case kube.ConnStateMsg:
		t.updates = append(t.updates, "conn")
	case tui.GotoKindMsg:
		t.updates = append(t.updates, "goto")
	}
	return t, nil
}
func (t *screenTask) Theme() tui.Theme        { return tui.Dark() }
func (t *screenTask) Header() tui.HeaderState { return tui.HeaderState{} }
func (t *screenTask) Strips(int) []string     { return nil }
func (t *screenTask) Keybar() tui.Keybar      { return tui.Keybar{PillText: t.pill} }
func (t *screenTask) Body(int, int) string    { return t.name }

// capturingTask is a screenTask with an always-open free-text input (like
// browse's filter box), so it exercises the InputCapturer opt-out from
// shell-key interception.
type capturingTask struct {
	screenTask
	received []string
}

func (t *capturingTask) CapturingInput() bool { return true }
func (t *capturingTask) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		t.received = append(t.received, key.String())
	}
	return t, nil
}

func testSession() *tui.Session {
	return &tui.Session{Theme: tui.Dark(), Styles: tui.NewStyles(tui.Dark())}
}

func TestRootModelLegacyTaskUnaffectedByShellKeys(t *testing.T) {
	t.Parallel()

	task := &staticTask{name: "legacy"}
	model := tui.NewWithSession(task, testSession())
	updated, _ := model.Update(tea.KeyPressMsg{Text: "g"})
	m := updated.(tui.Model)
	if m.PaletteOpen() {
		t.Fatalf("a legacy (non-Screen) task must not have its 'g' key intercepted by the shell")
	}
}

func TestRootModelGOpensGotoPalette(t *testing.T) {
	t.Parallel()

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, testSession())
	updated, _ := model.Update(tea.KeyPressMsg{Text: "g"})
	m := updated.(tui.Model)
	if !m.PaletteOpen() {
		t.Fatalf("expected 'g' to open the palette")
	}
	if m.Mode() != tui.ModeGoto {
		t.Fatalf("Mode() = %v, want ModeGoto", m.Mode())
	}
}

func TestRootModelColonOpensGotoPalette(t *testing.T) {
	t.Parallel()

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, testSession())
	updated, _ := model.Update(tea.KeyPressMsg{Text: ":"})
	m := updated.(tui.Model)
	if !m.PaletteOpen() {
		t.Fatalf("expected ':' to open the palette")
	}
	if m.Mode() != tui.ModeGoto {
		t.Fatalf("Mode() = %v, want ModeGoto", m.Mode())
	}
}

func TestRootModelWithoutSessionDoesNotOpenPalette(t *testing.T) {
	t.Parallel()

	task := &screenTask{name: "browse"}
	model := tui.New(task) // no session
	updated, _ := model.Update(tea.KeyPressMsg{Text: "g"})
	if updated.(tui.Model).PaletteOpen() {
		t.Fatalf("expected no palette without a Session")
	}
}

func TestRootModelEscClosesPalette(t *testing.T) {
	t.Parallel()

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, testSession())
	updated, _ := model.Update(tea.KeyPressMsg{Text: "g"})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m := updated.(tui.Model)
	if m.PaletteOpen() {
		t.Fatalf("expected esc to close the palette")
	}
	if m.Mode() != tui.ModeBrowse {
		t.Fatalf("Mode() = %v, want ModeBrowse after closing", m.Mode())
	}
}

func TestRootModelQuestionMarkOpensHelp(t *testing.T) {
	t.Parallel()

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, testSession())
	updated, _ := model.Update(tea.KeyPressMsg{Text: "?"})
	m := updated.(tui.Model)
	if !m.HelpOpen() {
		t.Fatalf("expected '?' to open help")
	}
	updated, _ = m.Update(tea.KeyPressMsg{Text: "?"})
	if updated.(tui.Model).HelpOpen() {
		t.Fatalf("expected a second '?' to close help")
	}
}

// TestRootModelQOpensQuitConfirm covers the quit-confirmation overlay: 'q'
// must not quit immediately (unlike ctrl+q/ctrl+c, each task's own
// unchanged instant-quit binding) — it opens a confirm the root shell owns,
// the same way g/n/c/? open their own overlays.
func TestRootModelQOpensQuitConfirm(t *testing.T) {
	t.Parallel()

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, testSession())
	updated, cmd := model.Update(tea.KeyPressMsg{Text: "q"})
	m := updated.(tui.Model)
	if !m.QuitConfirmOpen() {
		t.Fatalf("expected 'q' to open the quit confirmation")
	}
	if m.Mode() != tui.ModeConfirm {
		t.Fatalf("Mode() = %v, want ModeConfirm", m.Mode())
	}
	if cmd != nil {
		t.Fatalf("expected no cmd from opening the confirm (must not quit yet)")
	}
}

// TestRootModelQuitConfirmNCancels covers 'n'/'esc' declining the quit
// confirmation: the overlay closes and the program keeps running.
func TestRootModelQuitConfirmNCancels(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"n", "esc"} {
		task := &screenTask{name: "browse"}
		model := tui.NewWithSession(task, testSession())
		updated, _ := model.Update(tea.KeyPressMsg{Text: "q"})
		var msg tea.KeyPressMsg
		if key == "esc" {
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		} else {
			msg = tea.KeyPressMsg{Text: key}
		}
		updated, cmd := updated.(tui.Model).Update(msg)
		m := updated.(tui.Model)
		if m.QuitConfirmOpen() {
			t.Fatalf("%q: expected the quit confirmation to close", key)
		}
		if m.Mode() != tui.ModeBrowse {
			t.Fatalf("%q: Mode() = %v, want ModeBrowse after cancelling", key, m.Mode())
		}
		if cmd != nil {
			t.Fatalf("%q: expected no cmd from cancelling (must not quit)", key)
		}
	}
}

// TestRootModelQuitConfirmYQuits covers 'y'/'enter' confirming the quit —
// the only path that actually returns tea.Quit for the 'q' key.
func TestRootModelQuitConfirmYQuits(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"y", "enter"} {
		task := &screenTask{name: "browse"}
		model := tui.NewWithSession(task, testSession())
		updated, _ := model.Update(tea.KeyPressMsg{Text: "q"})
		var msg tea.KeyPressMsg
		if key == "enter" {
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		} else {
			msg = tea.KeyPressMsg{Text: key}
		}
		_, cmd := updated.(tui.Model).Update(msg)
		if cmd == nil {
			t.Fatalf("%q: expected a quit cmd", key)
		}
	}
}

// TestRootModelInputCapturingTaskGetsQ covers 'q' alongside g/n/c/?:
// browse's own filter box (and every other CapturingInput task) must still
// be able to type the letter q rather than have the shell steal it for the
// quit confirmation.
func TestRootModelInputCapturingTaskGetsQ(t *testing.T) {
	t.Parallel()

	task := &capturingTask{screenTask: screenTask{name: "browse-filter"}}
	model := tui.NewWithSession(task, testSession())
	updated, _ := model.Update(tea.KeyPressMsg{Text: "q"})
	m := updated.(tui.Model)
	if m.QuitConfirmOpen() {
		t.Fatalf("shell must not intercept 'q' while the task is capturing input")
	}
	if got := task.received; len(got) != 1 || got[0] != "q" {
		t.Fatalf("expected the task to receive 'q' verbatim, got %v", got)
	}
}

// TestRootModelHelpOverlayRendersFourColumnsAndMisc covers 7b: the active
// screen's own Keybar() groups become the current-view column, and the
// fixed SCOPE/LIST/RESOURCE columns plus the MISC footer row come from
// Session.HelpScope/HelpList/HelpResource/HelpMisc (built at the
// composition root from the verbs registry — session.go).
func TestRootModelHelpOverlayRendersFourColumnsAndMisc(t *testing.T) {
	t.Parallel()

	task := &screenTask{name: "browse"}
	sess := testSession()
	sess.HelpScope = []tui.KeyHint{{Key: "g", Label: "jump anywhere"}}
	sess.HelpList = []tui.KeyHint{{Key: "/", Label: "filter"}}
	sess.HelpResource = []tui.KeyHint{{Key: "y", Label: "yaml"}}
	sess.HelpMisc = []tui.KeyHint{{Key: "ctrl+q", Label: "quit"}}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "?"})
	view := updated.(tui.Model).View().Content

	for _, want := range []string{
		"? help", "SCOPE", "LIST", "RESOURCE", "MISC",
		"jump anywhere", "filter", "yaml", "quit", "esc", "close",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected %q in the help overlay:\n%s", want, view)
		}
	}
}

// TestRootModelQuestionMarkShowsHelpKeybarPill pins 7b (docs/design
// README.md:114: "Keybar pill HELP"): the underlying screen's own keybar
// pill (e.g. "PODS") must be replaced by a HELP pill while the help overlay
// is open, the same undimmed-splice treatment goto's own GOTO pill gets
// (TestRootModelGShowsGotoKeybarPill, goto_test.go).
func TestRootModelQuestionMarkShowsHelpKeybarPill(t *testing.T) {
	t.Parallel()

	task := &screenTask{name: "browse", pill: "PODS"}
	model := tui.NewWithSession(task, testSession())
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "?"})
	view := updated.(tui.Model).View().Content

	lines := strings.Split(view, "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "HELP") {
		t.Fatalf("expected the HELP mode pill in the main keybar's last line, got:\n%s", last)
	}
	if strings.Contains(last, "PODS") {
		t.Fatalf("expected the underlying screen's own pill replaced, still saw it in:\n%s", last)
	}
}

func TestRootModelPaletteTypingAccumulatesQuery(t *testing.T) {
	t.Parallel()

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, testSession())
	updated, _ := model.Update(tea.KeyPressMsg{Text: "g"})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "p"})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "o"})
	view := updated.(tui.Model).View().Content
	if !strings.Contains(view, "po") {
		t.Fatalf("expected typed query 'po' reflected in the palette overlay:\n%s", view)
	}
}

func TestRootModelInputCapturingTaskGetsGNCQ(t *testing.T) {
	t.Parallel()

	task := &capturingTask{screenTask: screenTask{name: "browse-filter"}}
	model := tui.NewWithSession(task, testSession())
	for _, key := range []string{"g", "n", "c", "?"} {
		updated, _ := model.Update(tea.KeyPressMsg{Text: key})
		model = updated.(tui.Model)
	}
	if m := model; m.PaletteOpen() || m.HelpOpen() {
		t.Fatalf("shell must not intercept g/n/c/? while the task is capturing input (palette=%v help=%v)", m.PaletteOpen(), m.HelpOpen())
	}
	if got := task.received; len(got) != 4 || got[0] != "g" || got[1] != "n" || got[2] != "c" || got[3] != "?" {
		t.Fatalf("expected the task to receive g/n/c/? verbatim, got %v", got)
	}
}

func TestRootModelConnStateMsgUpdatesConnAndForwardsToTask(t *testing.T) {
	t.Parallel()

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, testSession())
	updated, _ := model.Update(kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "dial timeout"})
	m := updated.(tui.Model)
	if m.Conn().Phase != kube.ConnReconnecting {
		t.Fatalf("Conn().Phase = %v, want Reconnecting", m.Conn().Phase)
	}
	if len(task.updates) != 1 {
		t.Fatalf("expected ConnStateMsg forwarded to the task, got %d forwards", len(task.updates))
	}
}

func TestRootModelViewComposesPaletteOverBase(t *testing.T) {
	t.Parallel()

	task := &screenTask{name: "browse"}
	model := tui.NewWithSession(task, testSession())
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "g"})
	view := updated.(tui.Model).View().Content
	// 'g' with no query opens the 12a ranked list (bare-cursor input, per
	// the mockup — no placeholder text) — testSession has no Groups/Lister,
	// so the list is empty but the input row's hint still renders.
	if !strings.Contains(view, "jump anywhere") {
		t.Fatalf("expected the palette panel spliced into the view:\n%s", view)
	}
	lines := strings.Split(view, "\n")
	if len(lines) != 30 {
		t.Fatalf("got %d lines, want 30 (composed view keeps the base size)", len(lines))
	}
}

// TestNeverConnectedSwapsToSetupThenBackOnConnect pins 4c (mvp-plan.md
// Phase 4): a Session with a live-but-unconfirmed cluster swaps the root
// task to buildSetup's result on the first Reconnecting/Failed
// ConnStateMsg, and back to buildBrowse's result on the first Connected.
func TestNeverConnectedSwapsToSetupThenBackOnConnect(t *testing.T) {
	t.Parallel()

	browseTask := &screenTask{name: "browse-view"}
	setupTask := &screenTask{name: "setup-view"}
	sess := testSession()
	sess.Cluster = &kube.Cluster{}

	var builtSetup, builtBrowse int
	model := tui.NewWithSession(browseTask, sess).WithRootFactories(
		func(kube.ConnState) tui.Task { builtSetup++; return setupTask },
		func() tui.Task { builtBrowse++; return browseTask },
	)

	updated, _ := model.Update(kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "boom"})
	m := updated.(tui.Model)
	if builtSetup != 1 {
		t.Fatalf("buildSetup calls = %d, want 1", builtSetup)
	}
	if !strings.Contains(m.View().Content, "setup-view") {
		t.Fatalf("expected setup active after Reconnecting:\n%s", m.View().Content)
	}

	updated, _ = m.Update(kube.ConnStateMsg{Phase: kube.ConnConnected})
	m = updated.(tui.Model)
	if builtBrowse != 1 {
		t.Fatalf("buildBrowse calls = %d, want 1", builtBrowse)
	}
	if !strings.Contains(m.View().Content, "browse-view") {
		t.Fatalf("expected browse active again after Connected:\n%s", m.View().Content)
	}
}

// sizedTask is a screenTask that records SetSize calls, for pinning that
// root-level task swaps propagate the terminal size.
type sizedTask struct {
	screenTask
	sizes [][2]int
}

func (t *sizedTask) SetSize(w, h int) { t.sizes = append(t.sizes, [2]int{w, h}) }

func (t *sizedTask) lastSize() [2]int {
	if len(t.sizes) == 0 {
		return [2]int{0, 0}
	}
	return t.sizes[len(t.sizes)-1]
}

// TestRootTaskSwapsPropagateTerminalSize pins the 4c swap regression where a
// task built by buildSetup/buildBrowse/ReplaceRootMsg rendered at
// tui.Default* dimensions until the user resized the terminal: every swap
// path must push the last known WindowSizeMsg size onto the new task.
func TestRootTaskSwapsPropagateTerminalSize(t *testing.T) {
	t.Parallel()

	initial := &sizedTask{screenTask: screenTask{name: "initial-browse"}}
	setupTask := &sizedTask{screenTask: screenTask{name: "setup-view"}}
	browseTask := &sizedTask{screenTask: screenTask{name: "browse-view"}}
	sess := testSession()
	sess.Cluster = &kube.Cluster{}

	model := tui.NewWithSession(initial, sess).WithRootFactories(
		func(kube.ConnState) tui.Task { return setupTask },
		func() tui.Task { return browseTask },
	)

	updated, _ := model.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	updated, _ = updated.(tui.Model).Update(kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "boom"})
	if got := setupTask.lastSize(); got != [2]int{200, 50} {
		t.Fatalf("setup task size after swap = %v, want [200 50]", got)
	}

	updated, _ = updated.(tui.Model).Update(kube.ConnStateMsg{Phase: kube.ConnConnected})
	if got := browseTask.lastSize(); got != [2]int{200, 50} {
		t.Fatalf("browse task size after reconnect swap = %v, want [200 50]", got)
	}

	replacement := &sizedTask{screenTask: screenTask{name: "replacement"}}
	updated, _ = updated.(tui.Model).Update(tui.ReplaceRootMsg{Task: replacement})
	if got := replacement.lastSize(); got != [2]int{200, 50} {
		t.Fatalf("replacement task size = %v, want [200 50]", got)
	}
	_ = updated
}

// TestWithInitialPushSeedsStackAndResizesBothTasks pins a persisted
// Location.Kind of "Event" restoring straight to 9b (tasks/events) instead
// of browse's own stock Events list (internal/app's NewModel calls
// WithInitialPush for exactly this): the pushed task is immediately active,
// esc/BackMsg pops back to the seeded task underneath, and — the fix this
// regression actually pins — a live task's child normally inherits its
// parent's already-correct width/height at construction time, but
// WithInitialPush seeds the stack before the runtime has delivered any real
// terminal size, so the first tea.WindowSizeMsg must resize the seeded
// stack task too, not just the active one, or it renders letterboxed at
// tui.Default* dimensions the moment esc pops back to it.
func TestWithInitialPushSeedsStackAndResizesBothTasks(t *testing.T) {
	t.Parallel()

	browseTask := &sizedTask{screenTask: screenTask{name: "browse-pods"}}
	eventsTask := &sizedTask{screenTask: screenTask{name: "events-view"}}
	model := tui.NewWithSession(browseTask, testSession()).WithInitialPush(eventsTask)

	if view := model.View().Content; !strings.Contains(view, "events-view") {
		t.Fatalf("expected the pushed task active immediately, got:\n%s", view)
	}

	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if got := eventsTask.lastSize(); got != [2]int{120, 40} {
		t.Fatalf("active (pushed) task size = %v, want [120 40]", got)
	}
	if got := browseTask.lastSize(); got != [2]int{120, 40} {
		t.Fatalf("seeded stack task size = %v, want [120 40] — must not be stuck at tui.Default* dimensions", got)
	}

	updated, _ = updated.(tui.Model).Update(tui.BackMsg{})
	if view := updated.(tui.Model).View().Content; !strings.Contains(view, "browse-pods") {
		t.Fatalf("expected esc/BackMsg to pop back to the seeded browse task, got:\n%s", view)
	}
}

func TestContextSwitchFromPushedTaskReturnsToFreshBrowse(t *testing.T) {
	t.Parallel()

	sess := testSession()
	sess.Location = tui.Location{Context: "one", Namespace: "old", Kind: kube.KindPod}
	oldBrowse := &screenTask{name: "old-browse"}
	detail := &screenTask{name: "old-detail"}
	freshBrowse := &screenTask{name: "fresh-browse"}
	model := tui.NewWithSession(oldBrowse, sess).
		WithRootFactories(nil, func() tui.Task { return freshBrowse }).
		WithInitialPush(detail)

	updated, _ := model.Update(tui.SwitchContextMsg{
		Context: "two", Namespace: "new", Kind: kube.KindConfigMap, Filter: "marker",
	})
	m := updated.(tui.Model)
	if view := m.View().Content; !strings.Contains(view, "fresh-browse") || strings.Contains(view, "old-detail") {
		t.Fatalf("context switch did not replace the pushed task with browse:\n%s", view)
	}
	if got := sess.Location; got.Context != "two" || got.Namespace != "new" || got.Kind != kube.KindConfigMap || got.Filter != "marker" {
		t.Fatalf("Session.Location = %+v, want destination context location", got)
	}

	// The old stack was discarded, so Back cannot resurrect an object task
	// built against the previous context.
	updated, _ = m.Update(tui.BackMsg{})
	if view := updated.(tui.Model).View().Content; !strings.Contains(view, "fresh-browse") {
		t.Fatalf("Back resurrected old-context stack after switch:\n%s", view)
	}
}

// TestNeverConnectedLatchesOffAfterFirstConnect pins the "latches false for
// good" half of the same doc comment: once any Connected state has been
// observed, a later mid-session drop is 4a (browse's own job), not another
// 4c swap.
func TestNeverConnectedLatchesOffAfterFirstConnect(t *testing.T) {
	t.Parallel()

	browseTask := &screenTask{name: "browse-view"}
	sess := testSession()
	sess.Cluster = &kube.Cluster{}
	builtSetup := 0
	model := tui.NewWithSession(browseTask, sess).WithRootFactories(
		func(kube.ConnState) tui.Task { builtSetup++; return &screenTask{name: "setup-view"} },
		func() tui.Task { return browseTask },
	)

	updated, _ := model.Update(kube.ConnStateMsg{Phase: kube.ConnConnected})
	updated, _ = updated.(tui.Model).Update(kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "boom"})
	m := updated.(tui.Model)

	if builtSetup != 0 {
		t.Fatalf("buildSetup fired %d times after a Connected state was already observed, want 0", builtSetup)
	}
	if !strings.Contains(m.View().Content, "browse-view") {
		t.Fatalf("expected browse to stay active:\n%s", m.View().Content)
	}
}

// Informer events can arrive before health's first periodic /livez probe.
// They prove startup connected just as conclusively as ConnConnected, so a
// subsequent mid-session outage must stay on browse instead of swapping to
// the unreachable-at-launch setup screen.
func TestNeverConnectedLatchesOffAfterFirstResourceEvent(t *testing.T) {
	t.Parallel()

	browseTask := &screenTask{name: "browse-view"}
	sess := testSession()
	sess.Cluster = &kube.Cluster{}
	builtSetup := 0
	model := tui.NewWithSession(browseTask, sess).WithRootFactories(
		func(kube.ConnState) tui.Task { builtSetup++; return &screenTask{name: "setup-view"} },
		func() tui.Task { return browseTask },
	)

	updated, _ := model.Update(kube.ResourceChangedMsg{Kind: kube.KindPod})
	updated, _ = updated.(tui.Model).Update(kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "boom"})
	m := updated.(tui.Model)

	if builtSetup != 0 {
		t.Fatalf("buildSetup fired %d times after a resource event was observed, want 0", builtSetup)
	}
	if !strings.Contains(m.View().Content, "browse-view") {
		t.Fatalf("expected browse to stay active:\n%s", m.View().Content)
	}
}

// TestWithRootFactoriesNoopWithoutCluster pins the --demo/no-cluster guard:
// WithRootFactories only arms neverConnected when Session carries a live
// Cluster — a demo session must never swap to setup.
func TestWithRootFactoriesNoopWithoutCluster(t *testing.T) {
	t.Parallel()

	task := &screenTask{name: "browse-view"}
	builtSetup := 0
	model := tui.NewWithSession(task, testSession()).WithRootFactories(
		func(kube.ConnState) tui.Task { builtSetup++; return &screenTask{name: "setup-view"} },
		func() tui.Task { return task },
	)

	updated, _ := model.Update(kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "boom"})
	if builtSetup != 0 {
		t.Fatalf("buildSetup fired without a Session.Cluster, want 0 calls")
	}
	if !strings.Contains(updated.(tui.Model).View().Content, "browse-view") {
		t.Fatalf("expected browse to stay active without a Cluster")
	}
}

// TestCRDsDiscoveredOpensTheHeldLaunchKind pins M5's root half: a persisted
// CRD kind browse had to fall back from at launch is opened once discovery
// makes it resolvable — but not over a screen the user has since opened.
func TestCRDsDiscoveredOpensTheHeldLaunchKind(t *testing.T) {
	t.Parallel()
	widget := kube.ResourceKind("Widget")
	newSess := func() *tui.Session {
		sess := testSession()
		sess.Location.Context = "dev"
		sess.Registry, sess.Groups = resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{{
			Kind: "Widget", Plural: "widgets", Group: "example.com",
			GVR:         schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"},
			Established: true, CRDName: "widgets.example.com",
		}}, nil)
		sess.HoldPendingKind(widget)
		return sess
	}

	model := tui.NewWithSession(&staticTask{name: "browse"}, newSess())
	_, cmd := model.Update(kube.CRDsDiscoveredMsg{})
	if cmd == nil {
		t.Fatal("discovery resolving the held launch kind dispatched nothing")
	}
	if got, ok := cmd().(tui.GotoKindMsg); !ok || got.Kind != widget {
		t.Fatalf("discovery dispatched %#v, want GotoKindMsg{Kind: Widget}", cmd())
	}

	sess := newSess()
	model = tui.NewWithSession(&staticTask{name: "browse", next: &staticTask{name: "detail"}}, sess)
	updated, _ := model.Update(tea.KeyPressMsg{Text: "open"})
	if _, cmd := updated.(tui.Model).Update(kube.CRDsDiscoveredMsg{}); cmd != nil {
		t.Fatalf("discovery jumped away from a screen the user opened: %#v", cmd())
	}
}

// confirmingTask is a screenTask whose keybar shows an inline y/N confirm
// (helmhistory's rollback, secretdata's key removal) without implementing
// InputCapturer.
type confirmingTask struct {
	capturingTask
}

func (t *confirmingTask) CapturingInput() bool { return false }
func (t *confirmingTask) Keybar() tui.Keybar {
	return tui.Keybar{Pill: tui.ModeConfirm, PillText: "CONFIRM"}
}

// An inline confirm advertises n cancel; the shell must not take n (or q)
// for its own palette/quit confirm and leave the confirm pending behind it.
func TestRootModelConfirmingTaskGetsNAndQ(t *testing.T) {
	t.Parallel()

	task := &confirmingTask{capturingTask{screenTask: screenTask{name: "secretdata"}}}
	model := tui.NewWithSession(task, testSession())
	for _, key := range []string{"n", "q"} {
		updated, _ := model.Update(tea.KeyPressMsg{Text: key})
		model = updated.(tui.Model)
	}
	if model.PaletteOpen() || model.QuitConfirmOpen() {
		t.Fatalf("shell must not intercept n/q while the task's confirm is pending (palette=%v quit=%v)", model.PaletteOpen(), model.QuitConfirmOpen())
	}
	if got := task.received; len(got) != 2 || got[0] != "n" || got[1] != "q" {
		t.Fatalf("expected the confirm to receive n/q, got %v", got)
	}
}

// TestRegistryRebuildsOnlyWhenColumnsArrive: 14b's CRD informer delivers one
// add per CRD on its initial sync, none of which changes anything the kind
// registry is built from. Only the printer-columns signal may rebuild it.
func TestRegistryRebuildsOnlyWhenColumnsArrive(t *testing.T) {
	t.Parallel()
	sess := testSession()
	sess.Cluster = &kube.Cluster{}
	sess.Registry = resources.DefaultRegistry()
	sentinel := kube.ResourceKind("Sentinel")
	sess.Registry.Register(resources.Descriptor{Kind: sentinel})
	model := tui.NewWithSession(&screenTask{name: "browse-view"}, sess)

	updated, _ := model.Update(kube.ResourceChangedMsg{Kind: kube.KindCustomResourceDefinition})
	if _, ok := sess.Registry.Descriptor(sentinel); !ok {
		t.Fatal("an ordinary CRD object event rebuilt the kind registry")
	}

	updated.(tui.Model).Update(kube.ResourceChangedMsg{Kind: kube.KindCustomResourceDefinition, ColumnsArrived: true})
	if _, ok := sess.Registry.Descriptor(sentinel); ok {
		t.Fatal("printer columns arriving did not rebuild the kind registry")
	}
}

// TestModelScreenNamesTheTaskTypeWithoutThePointer: tasks are pointers, and
// the crash report's screen row reads "tui_test.screenTask", not
// "*tui_test.screenTask" (14 L8).
func TestModelScreenNamesTheTaskTypeWithoutThePointer(t *testing.T) {
	m := tui.NewWithSession(&screenTask{name: "browse"}, &tui.Session{Theme: tui.Dark()})
	if got := m.Screen(); got != "tui_test.screenTask" {
		t.Fatalf("Screen() = %q, want %q", got, "tui_test.screenTask")
	}
}
