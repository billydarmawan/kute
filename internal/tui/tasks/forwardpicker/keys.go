package forwardpicker

import (
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from the panel's own key list (docs/design
// README.md §13a: "↵ start · type local port · ↑↓ port · esc cancel"). The
// digit hint says which half of the pair the selected row's buffer types: a
// declared row already knows its remote port, the free-port row doesn't.
func (m Model) Keybar() tui.Keybar {
	typeHint := verbs.ForwardLocalPort.Hint()
	if m.row().manual {
		typeHint = verbs.ForwardLocalPort.HintAs("type port")
	}
	kb := tui.Keybar{
		Pill:     tui.ModeBrowse,
		PillText: "FORWARD",
		Groups: [][]tui.KeyHint{{
			verbs.Open.HintAs("start"),
			typeHint,
			verbs.Move.HintAs("port"),
		}},
		RightHints: []tui.KeyHint{verbs.Back.HintAs("cancel")},
	}
	if m.feedback != "" {
		kb.RightNote = m.feedback
	}
	return kb
}

// CapturingInput reports true while a row's local port is being edited, so
// digit keys reach the buffer instead of the root shell's global shortcuts.
func (m Model) CapturingInput() bool { return m.editing() }
