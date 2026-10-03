package execpicker

import (
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from the panel's own key list (docs/design
// README.md §10a: "↵ open shell · ↑↓ move · esc cancel"). The ↵ label
// follows the cursor (docs/design v.0.11.0.dc.html §41a: "one container has
// a shell, one doesn't — the ↵ label follows the cursor") — "debug
// <container>" on a shell-less row, "open shell" everywhere else.
func (m Model) Keybar() tui.Keybar {
	enterLabel := "open shell"
	if m.selectedShellless() {
		enterLabel = "debug " + m.selectedContainerName()
	}
	kb := tui.Keybar{
		Pill:     tui.ModeBrowse,
		PillText: "EXEC",
		Groups: [][]tui.KeyHint{{
			verbs.Open.HintAs(enterLabel),
			verbs.Move.Hint(),
		}},
		RightHints: []tui.KeyHint{verbs.Back.HintAs("cancel")},
	}
	if m.feedback != "" {
		kb.RightNote = m.feedback
	}
	return kb
}

// CapturingInput reports false: the picker has no free-text input, every
// key is a fixed binding.
func (m Model) CapturingInput() bool { return false }

// BackOnEscape opts the picker into the root's synchronous task pop. Without
// it, Escape returns BackMsg from a Cmd and an immediately repeated x can land
// on the picker before that message is processed, swallowing the reopen key.
// The picker has no editor or confirmation state that needs to claim Escape
// first, so it is always ready to close.
func (m Model) BackOnEscape() bool { return true }
