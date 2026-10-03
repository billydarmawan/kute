package overview

import (
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references only. Timeline/Events (t/e) are
// global grammar as of v.0.3.0.dc.html §29a, so they no longer render here —
// they're taught once in the ? overlay's GLOBAL column.
func (m Model) Keybar() tui.Keybar {
	if m.state != tui.TaskStateReady {
		return tui.Keybar{Pill: tui.ModeBrowse, PillText: "OVERVIEW"}
	}

	groups := [][]tui.KeyHint{
		{verbs.NextPanel.Hint()},
	}

	return tui.Keybar{
		Pill:       tui.ModeBrowse,
		PillText:   "OVERVIEW",
		Groups:     groups,
		RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint()),
	}
}
