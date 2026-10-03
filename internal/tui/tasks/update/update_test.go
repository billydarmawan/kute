package update

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/update"
)

var errBoom = errors.New("boom")

func testSession() *tui.Session {
	return &tui.Session{Theme: tui.Dark(), Styles: tui.NewStyles(tui.Dark()), Version: "0.2.0"}
}

// noopOpenBrowser is every test's default Config.OpenBrowser: New defaults
// to the real update.OpenBrowser (an OS process spawn against a real URL)
// when Config leaves it nil, so any test model that might press 'o' — now
// or after a future edit — must inject this instead, never the real thing.
func noopOpenBrowser(string) error { return nil }

func availableModel() Model {
	sess := testSession()
	sess.Update = &tui.UpdateInfo{
		Latest: update.Release{Version: "0.2.1", PublishedAt: time.Now().Add(-48 * time.Hour), HTMLURL: "https://github.com/kute-dev/kute/releases/tag/v0.2.1"},
		Changelog: []update.ChangelogEntry{
			{Type: "fix", Text: "rollout watch could miss the final ready event on slow clusters"},
			{Type: "new", Text: "resources editor accepts binary suffixes (Gi/Mi) case-insensitively"},
		},
		Install: update.InstallInfo{Manager: "homebrew", Command: "brew install kute-dev/tap/kute"},
	}
	m := New(Config{Session: sess, OpenBrowser: noopOpenBrowser})
	m.SetSize(120, 36)
	return m
}

func emptyModel() Model {
	sess := testSession()
	sess.Update = &tui.UpdateInfo{Latest: update.Release{Version: "0.2.0"}}
	sess.State.UpdateCheck.LastChecked = time.Now().Add(-3 * time.Hour)
	m := New(Config{Session: sess, OpenBrowser: noopOpenBrowser})
	m.SetSize(120, 36)
	return m
}

func loadingModel() Model {
	m := New(Config{Session: testSession(), OpenBrowser: noopOpenBrowser})
	m.SetSize(120, 36)
	return m
}

// checkFailedModel is a check that has already resolved, but with an error
// — ok=false on info() forever, indistinguishable from "still loading"
// unless Session.UpdateCheckErr is consulted.
func checkFailedModel() Model {
	sess := testSession()
	sess.UpdateCheckErr = errBoom
	m := New(Config{Session: sess, OpenBrowser: noopOpenBrowser})
	m.SetSize(120, 36)
	return m
}

// disabledModel has update.check: false in config — Session.Update and
// UpdateCheckErr both stay nil forever (updateCheckCmd itself never even
// tries), the third way info() can be permanently empty.
func disabledModel() Model {
	sess := testSession()
	disabled := false
	sess.Config.Update.Check = &disabled
	m := New(Config{Session: sess, OpenBrowser: noopOpenBrowser})
	m.SetSize(120, 36)
	return m
}

func TestStateDerivation(t *testing.T) {
	if got := availableModel().state(); got != tui.TaskStateReady {
		t.Fatalf("availableModel state = %v, want Ready", got)
	}
	if got := emptyModel().state(); got != tui.TaskStateEmpty {
		t.Fatalf("emptyModel state = %v, want Empty", got)
	}
	if got := loadingModel().state(); got != tui.TaskStateLoading {
		t.Fatalf("loadingModel state = %v, want Loading", got)
	}
	// A resolved-but-failed check must not be stuck in Loading forever —
	// Empty is what makes 'r' retry reachable (updateKey only fires Recheck
	// in the Empty state).
	if got := checkFailedModel().state(); got != tui.TaskStateEmpty {
		t.Fatalf("checkFailedModel state = %v, want Empty", got)
	}
	// Disabled must not read as Loading either — nothing is ever going to
	// resolve it.
	if got := disabledModel().state(); got != tui.TaskStateEmpty {
		t.Fatalf("disabledModel state = %v, want Empty", got)
	}
}

// TestInitFetchesWhenNothingResolvedYet pins the actual reported bug: the
// app-level 24h cache (keyed off the *persisted* Session.State.UpdateCheck.
// LastChecked, which survives process restarts) makes updateCheckCmd skip
// the ambient check outright on most launches — Session.Update never gets
// populated this process at all, not just "not yet". Before Init bypassed
// that cache, opening the panel in this state rendered "checking for
// updates…" forever: state() had no signal to tell "genuinely in flight"
// from "nothing is ever going to happen here".
func TestInitFetchesWhenNothingResolvedYet(t *testing.T) {
	calls := 0
	m := loadingModel()
	m.recheck = func() tea.Cmd { calls++; return func() tea.Msg { return tui.UpdateCheckedMsg{} } }

	cmd := m.Init()
	if cmd == nil || calls != 1 {
		t.Fatalf("expected Init to call Recheck when info is unresolved, calls=%d", calls)
	}
}

// TestInitNoopWhenInfoAlreadyResolved covers both terminal info() outcomes
// (an available update, and a genuinely-current one) — Init must not
// re-fetch once a real result already exists in memory.
func TestInitNoopWhenInfoAlreadyResolved(t *testing.T) {
	calls := 0
	fakeRecheck := func() tea.Cmd { calls++; return nil }

	for name, m := range map[string]Model{"available": availableModel(), "empty": emptyModel()} {
		m.recheck = fakeRecheck
		if cmd := m.Init(); cmd != nil {
			t.Errorf("%s: expected Init to no-op once info is resolved", name)
		}
	}
	if calls != 0 {
		t.Fatalf("expected Recheck never called, calls=%d", calls)
	}
}

// TestInitNoopWhileAmbientCheckInFlight: opening 28b before the first-of-
// day ambient check returns waits for that check rather than firing a
// second, concurrent one (14 L11b).
func TestInitNoopWhileAmbientCheckInFlight(t *testing.T) {
	calls := 0
	m := loadingModel()
	m.session.UpdateChecking = true
	m.recheck = func() tea.Cmd { calls++; return nil }
	if cmd := m.Init(); cmd != nil || calls != 0 {
		t.Fatalf("expected Init to wait for the in-flight check, calls=%d", calls)
	}
	if m.state() != tui.TaskStateLoading {
		t.Fatalf("state() = %v, want loading while the ambient check is in flight", m.state())
	}
}

// TestInitNoopWhenCheckAlreadyFailed guards against a refetch loop: once a
// check has resolved with an error this session, Init must leave it to 'r'
// rather than silently retrying every time the panel is reopened.
func TestInitNoopWhenCheckAlreadyFailed(t *testing.T) {
	calls := 0
	m := checkFailedModel()
	m.recheck = func() tea.Cmd { calls++; return nil }
	if cmd := m.Init(); cmd != nil || calls != 0 {
		t.Fatalf("expected Init to no-op after a prior failed check, calls=%d", calls)
	}
}

// TestInitNoopWhenDisabled guards the other hazard: Init must not fire a
// Cmd that updateCheckCmd would just discard as nil (update.check: false)
// — state() would then read a bare "checking" flag Init never gets to set
// with no Cmd ever resolving it.
func TestInitNoopWhenDisabled(t *testing.T) {
	calls := 0
	m := disabledModel()
	m.recheck = func() tea.Cmd { calls++; return nil }
	if cmd := m.Init(); cmd != nil || calls != 0 {
		t.Fatalf("expected Init to no-op when checks are disabled, calls=%d", calls)
	}
}

// TestRNoopWhenDisabled is the same hazard TestInitNoopWhenDisabled guards,
// via the other entry point: 'r' must not set checking=true off a Cmd that
// never resolves.
func TestRNoopWhenDisabled(t *testing.T) {
	calls := 0
	m := disabledModel()
	m.recheck = func() tea.Cmd { calls++; return nil }
	updated, cmd := m.Update(tea.KeyPressMsg{Text: "r"})
	if cmd != nil || calls != 0 {
		t.Fatal("expected 'r' to be a no-op when checks are disabled")
	}
	if updated.(*Model).checking {
		t.Fatal("expected checking to stay false when 'r' no-ops")
	}
}

// TestRRechecksAfterFailedAmbientCheck pins the actual fix: opening 28b
// after the ambient check already failed must still let 'r' retry — before
// Session.UpdateCheckErr existed, info() never resolved, state() never left
// Loading, and 'r' (gated on the Empty state) could never fire.
func TestRRechecksAfterFailedAmbientCheck(t *testing.T) {
	calls := 0
	m := checkFailedModel()
	m.recheck = func() tea.Cmd { calls++; return func() tea.Msg { return tui.UpdateCheckedMsg{} } }

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "r"})
	if cmd == nil || calls != 1 {
		t.Fatalf("expected 'r' to call Recheck after a failed ambient check, calls=%d", calls)
	}
	m2 := updated.(*Model)
	if !m2.checking {
		t.Fatal("expected checking=true after 'r'")
	}
}

func TestEscSendsBackMsg(t *testing.T) {
	m := availableModel()
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("expected a Cmd for esc")
	}
	if _, ok := cmd().(tui.BackMsg); !ok {
		t.Fatalf("expected tui.BackMsg, got %T", cmd())
	}
}

func TestYCopiesInstallCommand(t *testing.T) {
	m := availableModel()
	updated, cmd := m.Update(tea.KeyPressMsg{Text: "y"})
	if cmd == nil {
		t.Fatal("expected a Cmd (tea.SetClipboard) from 'y'")
	}
	m2 := updated.(*Model)
	if !strings.Contains(m2.feedback, "copied") {
		t.Fatalf("feedback = %q, want it to mention copied", m2.feedback)
	}
}

func TestYNoopInEmptyState(t *testing.T) {
	m := emptyModel()
	_, cmd := m.Update(tea.KeyPressMsg{Text: "y"})
	if cmd != nil {
		t.Fatal("expected no Cmd for 'y' with no update available")
	}
}

func TestXMarksSeenAndSetsFeedback(t *testing.T) {
	m := availableModel()
	updated, _ := m.Update(tea.KeyPressMsg{Text: "x"})
	m2 := updated.(*Model)
	if !m2.session.State.UpdateSeen("0.2.1") {
		t.Fatal("expected 0.2.1 marked seen after 'x'")
	}
	if !strings.Contains(m2.feedback, "0.2.1") || !strings.Contains(m2.feedback, "skipped") {
		t.Fatalf("feedback = %q", m2.feedback)
	}
}

// TestOOpensBrowserAndSetsFeedback pins 'o' calling Config.OpenBrowser with
// the release's HTMLURL — never the real update.OpenBrowser (which would
// spawn a real OS "open a URL" process against a real GitHub URL; a
// previous version of this test did exactly that, popping a real browser
// tab on every `go test` run).
func TestOOpensBrowserAndSetsFeedback(t *testing.T) {
	var gotURL string
	sess := testSession()
	sess.Update = &tui.UpdateInfo{Latest: update.Release{Version: "0.2.1", HTMLURL: "https://github.com/kute-dev/kute/releases/tag/v0.2.1"}}
	m := New(Config{Session: sess, OpenBrowser: func(url string) error { gotURL = url; return nil }})
	m.SetSize(120, 36)

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "o"})
	if cmd == nil {
		t.Fatal("expected a Cmd from 'o'")
	}
	// Drive the Cmd's result back through Update, same as the real runtime
	// would.
	updated, _ = updated.(*Model).Update(cmd())
	m2 := updated.(*Model)

	if gotURL != "https://github.com/kute-dev/kute/releases/tag/v0.2.1" {
		t.Fatalf("OpenBrowser called with %q, want the release's HTMLURL", gotURL)
	}
	if m2.feedback != "opened in browser" {
		t.Fatalf("feedback = %q, want \"opened in browser\"", m2.feedback)
	}
}

// TestOSetsFeedbackOnOpenBrowserError pins the failure path — a browser
// that couldn't be launched (no xdg-open, headless box, …) surfaces a
// message instead of silently doing nothing.
func TestOSetsFeedbackOnOpenBrowserError(t *testing.T) {
	sess := testSession()
	sess.Update = &tui.UpdateInfo{Latest: update.Release{Version: "0.2.1", HTMLURL: "https://github.com/kute-dev/kute/releases/tag/v0.2.1"}}
	m := New(Config{Session: sess, OpenBrowser: func(string) error { return errBoom }})
	m.SetSize(120, 36)

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "o"})
	updated, _ = updated.(*Model).Update(cmd())
	m2 := updated.(*Model)
	if m2.feedback != "couldn't open a browser" {
		t.Fatalf("feedback = %q, want the failure note", m2.feedback)
	}
}

func TestROnlyRechecksInEmptyState(t *testing.T) {
	calls := 0
	sess := testSession()
	sess.Update = &tui.UpdateInfo{Latest: update.Release{Version: "0.2.0"}}
	m := New(Config{Session: sess, OpenBrowser: noopOpenBrowser, Recheck: func() tea.Cmd { calls++; return func() tea.Msg { return tui.UpdateCheckedMsg{} } }})
	m.SetSize(120, 36)

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "r"})
	if cmd == nil || calls != 1 {
		t.Fatalf("expected 'r' to call Recheck in the empty state, calls=%d", calls)
	}
	m2 := updated.(*Model)
	if !m2.checking {
		t.Fatal("expected checking=true after 'r'")
	}

	// UpdateCheckedMsg clears checking regardless of outcome.
	updated, _ = m2.Update(tui.UpdateCheckedMsg{})
	m3 := updated.(*Model)
	if m3.checking {
		t.Fatal("expected checking cleared after UpdateCheckedMsg")
	}
}

func TestRNoopInReadyState(t *testing.T) {
	calls := 0
	m := availableModel()
	m.recheck = func() tea.Cmd { calls++; return nil }
	_, cmd := m.Update(tea.KeyPressMsg{Text: "r"})
	if cmd != nil || calls != 0 {
		t.Fatal("expected 'r' to be a no-op while an update is available (ready state)")
	}
}

func TestKeybarGroupsAreStateConditional(t *testing.T) {
	ready := availableModel().Keybar()
	if !containsKey(ready.Groups, "y") || !containsKey(ready.Groups, "x") || containsKey(ready.Groups, "r") {
		t.Fatalf("ready keybar groups = %+v", ready.Groups)
	}
	empty := emptyModel().Keybar()
	if !containsKey(empty.Groups, "r") || containsKey(empty.Groups, "y") {
		t.Fatalf("empty keybar groups = %+v", empty.Groups)
	}
	disabled := disabledModel().Keybar()
	if containsKey(disabled.Groups, "r") {
		t.Fatalf("disabled keybar groups = %+v, want no 'r' hint (retrying would just no-op)", disabled.Groups)
	}
}

func containsKey(groups [][]tui.KeyHint, key string) bool {
	for _, g := range groups {
		for _, h := range g {
			if h.Key == key {
				return true
			}
		}
	}
	return false
}

// TestStateSurvivesRootRouting drives 28b through the real root shell the
// way app's factory installs it (a *Model): the root keeps whatever Update
// returns only if it's a tui.Task, so a value-receiver Update silently
// dropped every feedback line and the checking flag.
func TestStateSurvivesRootRouting(t *testing.T) {
	m := availableModel()
	root := tui.NewWithSession(&m, m.session)
	updated, _ := root.Update(tea.KeyPressMsg{Text: "y"})
	if got := updated.(tui.Model).View().Content; !strings.Contains(got, "command copied") {
		t.Fatalf("expected the root-routed 'y' to show command copied, got:\n%s", got)
	}

	f := checkFailedModel()
	root = tui.NewWithSession(&f, f.session)
	updated, _ = root.Update(tui.UpdateCheckedMsg{Err: errBoom})
	if got := updated.(tui.Model).View().Content; !strings.Contains(got, "check failed") {
		t.Fatalf("expected the root-routed failed check to show check failed, got:\n%s", got)
	}
}
