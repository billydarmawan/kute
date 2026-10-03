package debugpanel

import (
	"fmt"
	"time"

	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band, in priority order: an open field edit,
// the cleanup delete confirm (actions.Controller), the debug-launch confirm
// (launchPending — PROD only), §41c's CLEAN UP prompt, then the ordinary
// per-target/mode hints.
func (m Model) Keybar() tui.Keybar {
	if m.editingField != fieldNone {
		hints := []tui.KeyHint{verbs.Open.HintAs("apply"), verbs.Back.HintAs("cancel")}
		if m.editingField == fieldImage && len(m.recentImages()) > 0 {
			hints = append(hints, verbs.DebugRecents.Hint())
		}
		return tui.Keybar{
			Pill:       tui.ModeConfirm,
			PillText:   "EDIT",
			RightHints: hints,
		}
	}
	if m.actions.Active() {
		if m.actions.Tier() == actions.TierModal {
			return tui.Keybar{
				Pill:       tui.ModeConfirm,
				PillText:   "DELETE",
				RightHints: []tui.KeyHint{{Key: "type name", Label: "then ↵"}, verbs.ConfirmCancel.Hint()},
			}
		}
		return tui.Keybar{
			Pill:      tui.ModeConfirm,
			PillText:  "CONFIRM",
			Groups:    [][]tui.KeyHint{m.actions.ConfirmHints()},
			RightNote: m.actions.Prompt(),
		}
	}
	if m.launchPending {
		return tui.Keybar{
			Pill:      tui.ModeConfirm,
			PillText:  "CONFIRM",
			Groups:    [][]tui.KeyHint{verbs.ConfirmHints()},
			RightNote: m.launchPrompt(),
		}
	}
	if m.cleanup != nil {
		hints := []tui.KeyHint{verbs.DebugCleanup.Hint(), verbs.Back.HintAs("keep")}
		if verbs.DebugCleanup.HiddenWhileOffline(m.conn.Offline()) {
			hints = hints[1:]
		}
		note := m.cleanup.name + " still running"
		if !m.cleanup.startedAt.IsZero() && !m.now.IsZero() {
			note += " · " + shortDur(m.now.Sub(m.cleanup.startedAt))
		}
		return tui.Keybar{
			Pill:       tui.ModeBrowse,
			PillText:   "CLEAN UP",
			RightNote:  note,
			RightHints: hints,
		}
	}
	return m.readyKeybar()
}

// launchPrompt mirrors actions.Controller.Prompt()'s verbs.ConfirmPrompt
// shape for the launch's own bespoke, non-Controller
// gate.
func (m Model) launchPrompt() string {
	name := m.podName
	if m.tgt == targetNode {
		name = "node/" + m.nodeName
	}
	return verbs.ConfirmPrompt("Launch kubectl debug against " + name + "?")
}

func (m Model) readyKeybar() tui.Keybar {
	if m.accessState == accessChecking {
		return tui.Keybar{
			Pill:       tui.ModeBrowse,
			PillText:   "DEBUG",
			RightNote:  m.accessFeedback(),
			RightHints: []tui.KeyHint{verbs.Back.HintAs("cancel")},
		}
	}
	if m.accessState == accessDenied {
		return tui.Keybar{
			Pill:       tui.ModeBrowse,
			PillText:   "DEBUG",
			Groups:     [][]tui.KeyHint{{verbs.WhoCan.Hint()}},
			RightNote:  m.accessFeedback(),
			RightHints: []tui.KeyHint{verbs.Back.HintAs("cancel")},
		}
	}
	group := []tui.KeyHint{verbs.PodDebug.Hint()}
	switch {
	case m.tgt == targetNode:
		group = []tui.KeyHint{verbs.Open.HintAs("start"), verbs.DebugImage.Hint(), verbs.DebugProfile.Hint()}
	case m.mode == modeAttach:
		group = append(group,
			verbs.DebugMode.Hint(),
			verbs.DebugImage.Hint(),
			verbs.DebugTarget.Hint(),
			verbs.DebugProfile.Hint(),
		)
	default:
		group = append(group,
			verbs.DebugMode.Hint(),
			verbs.DebugCopyName.Hint(),
			verbs.DebugContainer.Hint(),
			verbs.DebugProfile.Hint(),
			verbs.DebugEntrypoint.Hint(),
			verbs.DebugProcesses.Hint(),
		)
	}
	pill, pillText, rightNote := tui.ModeBrowse, "DEBUG", m.accessFeedback()
	if m.launchVerb().HiddenWhileOffline(m.conn.Offline()) {
		// The launch is refused while offline (updateKey) — drop its hint
		// and say why, the same OFFLINE band every other screen shows.
		group = group[1:]
		pill, pillText, rightNote = tui.ModeOffline, "OFFLINE", "mutating actions disabled"
	}
	return tui.Keybar{
		Pill:       pill,
		PillText:   pillText,
		Groups:     [][]tui.KeyHint{group},
		RightNote:  rightNote,
		RightHints: []tui.KeyHint{verbs.Back.HintAs("cancel")},
	}
}

// CapturingInput reports true while a field is being edited or a confirm is
// showing, so digit/letter keys reach the buffer instead of the root
// shell's global shortcuts — mirrors forwardpicker's own contract.
func (m Model) CapturingInput() bool {
	return m.editingField != fieldNone || m.actions.Active() || m.launchPending
}

// BackOnEscape opts the ready panel into the root's synchronous task pop, so
// an immediately repeated x is handled by the restored caller instead of
// racing the BackMsg command and being swallowed here. Editors and confirms
// keep their existing first-Escape-means-cancel behavior.
func (m Model) BackOnEscape() bool {
	return m.editingField == fieldNone && !m.actions.Active() && !m.launchPending
}

// ActionResult puts the actions controller's outcome line — a failed or
// successful delete, drain, cordon, restart, … — on the keybar
// (tui.ActionResulter, rendered by tui.Frame).
func (m Model) ActionResult() tui.ResultLine { return m.actions.Result() }

// shortDur renders CLEAN UP's age in the app's compact "45s"/"4m"/"2h"/"3d"
// shape — duplicated per the repo's package-local-seam convention.
func shortDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
