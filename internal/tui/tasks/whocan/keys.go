package whocan

import (
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references only, per the
// registry invariant — 22a's pill is WHO CAN (docs/design README.md §22a).
// v/K are screen-local (there's no cross-screen verb to register for them).
// K is capital — lowercase k stays movement (CLAUDE.md's "j/k ≡ ↑↓
// everywhere"), which whocan itself used to break: the root shell (model.go)
// intercepted lowercase 'k' for this screen's own resource-kind palette
// before whocan's own updateKey ever saw it, so only the literal Up arrow
// ever moved the cursor up here. Namespace (n) is global grammar as of
// v.0.3.0.dc.html §29a, so it no longer renders here — it's taught once in
// the ? overlay's SCOPE column.
func (m Model) Keybar() tui.Keybar {
	if m.state != tui.TaskStateReady && m.state != tui.TaskStateEmpty {
		return tui.Keybar{Pill: tui.ModeBrowse, PillText: "WHO CAN"}
	}
	if m.state == tui.TaskStateEmpty {
		return tui.Keybar{Pill: tui.ModeBrowse, PillText: "WHO CAN", RightNote: "0 subjects"}
	}

	groups := [][]tui.KeyHint{
		{verbs.WhoCanVerb.Hint(), verbs.WhoCanResource.Hint()},
	}
	if row, ok := m.selectedRow(); ok && row.subject.BindingName != "" {
		groups = append(groups, []tui.KeyHint{verbs.Open.HintAs("open binding yaml")})
	}

	return tui.Keybar{
		Pill:       tui.ModeBrowse,
		PillText:   "WHO CAN",
		Groups:     groups,
		RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint()),
	}
}
