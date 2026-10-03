package tui_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/update"
)

var errBoom = errors.New("boom")

func TestSessionUpdateChipNoCachedVersion(t *testing.T) {
	t.Parallel()
	sess := testSession()
	sess.Version = "0.2.0"
	if _, ok := sess.UpdateChip(); ok {
		t.Fatal("expected no chip with no cached latest version")
	}
}

func TestSessionUpdateChipNotNewer(t *testing.T) {
	t.Parallel()
	sess := testSession()
	sess.Version = "0.2.0"
	sess.State.UpdateCheck.LatestVersion = "0.2.0"
	if _, ok := sess.UpdateChip(); ok {
		t.Fatal("expected no chip when the cached latest isn't newer")
	}
}

func TestSessionUpdateChipAvailable(t *testing.T) {
	t.Parallel()
	sess := testSession()
	sess.Version = "0.2.0"
	sess.State.UpdateCheck.LatestVersion = "0.2.1"
	v, ok := sess.UpdateChip()
	if !ok || v != "0.2.1" {
		t.Fatalf("UpdateChip() = (%q, %v), want (0.2.1, true)", v, ok)
	}
}

// TestSessionUpdateChipHiddenWhenChecksAreOff: with update.check: false or
// --no-update-check the persisted cache is inert, so a stale "↑ x.y.z"
// from an earlier session must not show (14 L10).
func TestSessionUpdateChipHiddenWhenChecksAreOff(t *testing.T) {
	t.Parallel()
	off := false
	for name, cfg := range map[string]config.Config{
		"update.check: false": {Update: config.UpdateConfig{Check: &off}},
		"--no-update-check":   func() config.Config { var c config.Config; c.DisableUpdateCheck(); return c }(),
	} {
		sess := testSession()
		sess.Version = "0.2.0"
		sess.Config = cfg
		sess.State.UpdateCheck.LatestVersion = "0.2.1"
		if v, ok := sess.UpdateChip(); ok {
			t.Errorf("%s: UpdateChip() = (%q, true), want no chip", name, v)
		}
		if hints := tui.UpdateRightHints(sess); hints != nil {
			t.Errorf("%s: UpdateRightHints() = %v, want nil", name, hints)
		}
	}
}

func TestSessionUpdateChipHiddenOnceSeen(t *testing.T) {
	t.Parallel()
	sess := testSession()
	sess.Version = "0.2.0"
	sess.State.UpdateCheck.LatestVersion = "0.2.1"
	sess.State.MarkUpdateSeen("0.2.1")
	if _, ok := sess.UpdateChip(); ok {
		t.Fatal("expected no chip once the version has been marked seen")
	}
}

func TestBuildUpdateChipRendersNothingWhenInert(t *testing.T) {
	t.Parallel()
	sess := testSession()
	badge := tui.BuildUpdateChip(sess.Theme, sess)
	if badge.Text != "" {
		t.Fatalf("expected an empty chip, got %q", badge.Text)
	}
}

func TestBuildUpdateChipRendersArrowAndVersion(t *testing.T) {
	t.Parallel()
	sess := testSession()
	sess.Version = "0.2.0"
	sess.State.UpdateCheck.LatestVersion = "0.2.1"
	badge := tui.BuildUpdateChip(sess.Theme, sess)
	if badge.Text != "↑ 0.2.1" {
		t.Fatalf("chip text = %q, want %q", badge.Text, "↑ 0.2.1")
	}
}

func TestUpdateRightHintsNilWhenInert(t *testing.T) {
	t.Parallel()
	sess := testSession()
	if hints := tui.UpdateRightHints(sess); hints != nil {
		t.Fatalf("expected nil hints, got %v", hints)
	}
}

func TestUpdateRightHintsNamesTheKeyWhenLive(t *testing.T) {
	t.Parallel()
	sess := testSession()
	sess.Version = "0.2.0"
	sess.State.UpdateCheck.LatestVersion = "0.2.1"
	hints := tui.UpdateRightHints(sess)
	if len(hints) != 1 || hints[0].Key != "U" || !strings.Contains(hints[0].Label, "0.2.1") {
		t.Fatalf("UpdateRightHints = %+v", hints)
	}
}

// TestRootModelUOpensUpdatePanel pins 'U' as a root-shell shortcut (like
// g/n/c/?): it pushes the current task and swaps in whatever
// WithUpdatePanel installed, and BackMsg returns to the task it came from.
func TestRootModelUOpensUpdatePanel(t *testing.T) {
	t.Parallel()

	browseTask := &screenTask{name: "browse-view"}
	panelTask := &screenTask{name: "update-panel"}
	sess := testSession()
	model := tui.NewWithSession(browseTask, sess).WithUpdatePanel(func() tui.Task { return panelTask })

	updated, _ := model.Update(tea.KeyPressMsg{Text: "U"})
	m := updated.(tui.Model)
	if !strings.Contains(m.View().Content, "update-panel") {
		t.Fatalf("expected the update panel active after 'U':\n%s", m.View().Content)
	}

	updated, _ = m.Update(tui.BackMsg{})
	m = updated.(tui.Model)
	if !strings.Contains(m.View().Content, "browse-view") {
		t.Fatalf("expected browse active again after back:\n%s", m.View().Content)
	}
}

// TestRootModelUMarksLatestVersionSeen pins docs/design README.md §28b:
// "opening the panel at all also counts as seen for the ambient chip".
func TestRootModelUMarksLatestVersionSeen(t *testing.T) {
	t.Parallel()

	sess := testSession()
	sess.Version = "0.2.0"
	sess.State.UpdateCheck.LatestVersion = "0.2.1"
	model := tui.NewWithSession(&screenTask{name: "browse"}, sess).
		WithUpdatePanel(func() tui.Task { return &screenTask{name: "panel"} })

	model.Update(tea.KeyPressMsg{Text: "U"})
	if !sess.State.UpdateSeen("0.2.1") {
		t.Fatal("expected 0.2.1 marked seen after opening the panel")
	}
}

// TestRootModelUNoopWithoutFactory guards the nil-factory case (no cluster
// wiring ever called WithUpdatePanel) — the key is swallowed (handled by
// the shell) but nothing pushes.
func TestRootModelUNoopWithoutFactory(t *testing.T) {
	t.Parallel()

	task := &screenTask{name: "browse-view"}
	model := tui.NewWithSession(task, testSession())
	updated, _ := model.Update(tea.KeyPressMsg{Text: "U"})
	m := updated.(tui.Model)
	if !strings.Contains(m.View().Content, "browse-view") {
		t.Fatalf("expected browse to stay active with no update-panel factory:\n%s", m.View().Content)
	}
}

// TestGotoUpdateItemOpensPanelWithoutUnwindingStack pins ":update"
// (docs/design README.md §28b) opening 28b from two levels deep in the
// stack and popping back to the level right below it on esc, not all the
// way to root — the same "one jump = one esc back" contract a real kind/
// resource jump now gets too (routeGoto, model.go), just via a different
// message (OpenUpdatePanelMsg pushes directly onto whatever's active,
// rather than needing a fresh browse view built for it).
func TestGotoUpdateItemOpensPanelWithoutUnwindingStack(t *testing.T) {
	t.Parallel()

	root := &screenTask{name: "root-browse"}
	sess := testSession()
	panelN := 0
	model := tui.NewWithSession(root, sess).WithUpdatePanel(func() tui.Task {
		panelN++
		return &screenTask{name: strings.Repeat("panel", panelN)}
	})

	// First 'U' pushes root, stack = [root-browse].
	updated, _ := model.Update(tea.KeyPressMsg{Text: "U"})
	m := updated.(tui.Model)

	// From two levels deep, ":update" (via the goto palette's synthetic
	// item) must push onto the *current* stack, not unwind to root first.
	updated, _ = m.Update(tea.KeyPressMsg{Text: "g"})
	m = updated.(tui.Model)
	for _, r := range "update" {
		updated, _ = m.Update(tea.KeyPressMsg{Text: string(r)})
		m = updated.(tui.Model)
	}
	// handleShellKey's Enter returns a tea.Cmd (gotoDispatch's
	// OpenUpdatePanelMsg-producing closure) rather than pushing directly —
	// the real tea.Program runtime executes it and feeds the result back
	// through Update; drive that by hand here, the same way
	// TestRootModelGotoFromPushedScreenPushesFreshBrowseAndPreservesHistory
	// does for GotoKindMsg.
	var cmd tea.Cmd
	updated, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(tui.Model)
	if cmd == nil {
		t.Fatal("expected a Cmd from selecting the :update item")
	}
	updated, _ = m.Update(cmd())
	m = updated.(tui.Model)
	if !strings.Contains(m.View().Content, "panelpanel") {
		t.Fatalf("expected the second update panel active after :update:\n%s", m.View().Content)
	}

	updated, _ = m.Update(tui.BackMsg{})
	m = updated.(tui.Model)
	if strings.Contains(m.View().Content, "root-browse") {
		t.Fatalf("expected back to land on the first panel, not unwind to root:\n%s", m.View().Content)
	}
	if !strings.Contains(m.View().Content, "panel") {
		t.Fatalf("expected the first update panel active after back:\n%s", m.View().Content)
	}
}

func TestUpdateCheckedMsgPopulatesSession(t *testing.T) {
	t.Parallel()

	sess := testSession()
	model := tui.NewWithSession(&screenTask{name: "browse"}, sess)
	checkedAt := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	info := tui.UpdateInfo{Latest: update.Release{Version: "0.2.1"}}
	model.Update(tui.UpdateCheckedMsg{Info: info, LatestVersion: "0.2.1", CheckedAt: checkedAt})

	if sess.Update == nil || sess.Update.Latest.Version != "0.2.1" {
		t.Fatalf("Session.Update = %+v, want Latest.Version 0.2.1", sess.Update)
	}
	if sess.State.UpdateCheck.LatestVersion != "0.2.1" {
		t.Fatalf("State.UpdateCheck.LatestVersion = %q, want 0.2.1", sess.State.UpdateCheck.LatestVersion)
	}
	if !sess.State.UpdateCheck.LastChecked.Equal(checkedAt) {
		t.Fatalf("State.UpdateCheck.LastChecked = %v, want %v", sess.State.UpdateCheck.LastChecked, checkedAt)
	}
}

func TestUpdateCheckedMsgErrIsIgnored(t *testing.T) {
	t.Parallel()

	sess := testSession()
	sess.UpdateChecking = true
	model := tui.NewWithSession(&screenTask{name: "browse"}, sess)
	model.Update(tui.UpdateCheckedMsg{Err: errBoom})

	if sess.UpdateChecking {
		t.Fatal("UpdateChecking still set after the check resolved")
	}

	if sess.Update != nil {
		t.Fatalf("Session.Update = %+v, want nil after a failed check", sess.Update)
	}
	if sess.State.UpdateCheck.LatestVersion != "" {
		t.Fatalf("State.UpdateCheck.LatestVersion = %q, want empty after a failed check", sess.State.UpdateCheck.LatestVersion)
	}
}
