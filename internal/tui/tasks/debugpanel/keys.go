package debugpanel

import (
	"fmt"

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
		hints := []tui.KeyHint{{Key: "↵", Label: "apply"}, {Key: "esc", Label: "cancel"}}
		if m.editingField == fieldImage && len(m.recentImages()) > 0 {
			hints = append(hints, tui.KeyHint{Key: "tab", Label: "recents"})
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
				RightHints: []tui.KeyHint{{Key: "type name", Label: "then ↵"}, {Key: "esc", Label: "cancel"}},
			}
		}
		return tui.Keybar{
			Pill:      tui.ModeConfirm,
			PillText:  "CONFIRM",
			Groups:    [][]tui.KeyHint{{{Key: "y", Label: "confirm"}, {Key: "n", Label: "cancel"}}},
			RightNote: m.actions.Prompt(),
		}
	}
	if m.launchPending {
		return tui.Keybar{
			Pill:      tui.ModeConfirm,
			PillText:  "CONFIRM",
			Groups:    [][]tui.KeyHint{{{Key: "y", Label: "confirm"}, {Key: "n", Label: "cancel"}}},
			RightNote: m.launchPrompt(),
		}
	}
	if m.cleanup != nil {
		return tui.Keybar{
			Pill:      tui.ModeBrowse,
			PillText:  "CLEAN UP",
			RightNote: m.cleanup.name + " still running",
			RightHints: []tui.KeyHint{
				{Key: "ctrl-d", Label: "delete it"},
				{Key: "esc", Label: "keep"},
			},
		}
	}
	return m.readyKeybar()
}

// launchPrompt mirrors actions.Controller.Prompt()'s "<Verb> <target>? (y)
// confirm (esc) cancel" shape for the launch's own bespoke, non-Controller
// gate.
func (m Model) launchPrompt() string {
	name := m.podName
	if m.tgt == targetNode {
		name = "node/" + m.nodeName
	}
	return fmt.Sprintf("Launch kubectl debug against %s? (y) confirm  (esc) cancel", name)
}

func (m Model) readyKeybar() tui.Keybar {
	if m.accessState == accessChecking {
		return tui.Keybar{
			Pill:       tui.ModeBrowse,
			PillText:   "DEBUG",
			RightNote:  m.accessFeedback(),
			RightHints: []tui.KeyHint{{Key: "esc", Label: "cancel"}},
		}
	}
	if m.accessState == accessDenied {
		return tui.Keybar{
			Pill:       tui.ModeBrowse,
			PillText:   "DEBUG",
			Groups:     [][]tui.KeyHint{{verbs.WhoCan.Hint()}},
			RightNote:  m.accessFeedback(),
			RightHints: []tui.KeyHint{{Key: "esc", Label: "cancel"}},
		}
	}
	group := []tui.KeyHint{verbs.PodDebug.Hint()}
	switch {
	case m.tgt == targetNode:
		group = []tui.KeyHint{{Key: "↵", Label: "start"}, {Key: "i", Label: "image"}, {Key: "p", Label: "profile"}}
	case m.mode == modeAttach:
		group = append(group,
			tui.KeyHint{Key: "m", Label: "mode"},
			tui.KeyHint{Key: "i", Label: "image"},
			tui.KeyHint{Key: "t", Label: "target"},
			tui.KeyHint{Key: "p", Label: "profile"},
		)
	default:
		group = append(group,
			tui.KeyHint{Key: "m", Label: "mode"},
			tui.KeyHint{Key: "i", Label: "copy name"},
			tui.KeyHint{Key: "t", Label: "container"},
			tui.KeyHint{Key: "p", Label: "profile"},
			tui.KeyHint{Key: "e", Label: "entrypoint"},
			tui.KeyHint{Key: "s", Label: "processes"},
		)
	}
	return tui.Keybar{
		Pill:       tui.ModeBrowse,
		PillText:   "DEBUG",
		Groups:     [][]tui.KeyHint{group},
		RightNote:  m.accessFeedback(),
		RightHints: []tui.KeyHint{{Key: "esc", Label: "cancel"}},
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
