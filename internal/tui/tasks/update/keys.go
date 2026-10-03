package update

import (
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references, this screen's own
// y/o/x/r included — 28b's pill is UPDATE (docs/design README.md §28b). The key
// group shown is state-conditional: "y copy · o release notes · x skip"
// while an update is available, "r re-check now"
// in the empty (already-current) state — 'r' only exists there, per the
// §28b keys line ("the only place a manual check exists").
func (m Model) Keybar() tui.Keybar {
	kb := tui.Keybar{
		Pill:       tui.ModeUpdate,
		PillText:   "UPDATE",
		RightNote:  m.feedback,
		RightHints: []tui.KeyHint{verbs.Help.Hint()},
	}

	switch m.state() {
	case tui.TaskStateReady:
		kb.Groups = [][]tui.KeyHint{{
			verbs.UpdateCopyCommand.Hint(),
			verbs.UpdateNotes.Hint(),
			verbs.UpdateSkip.Hint(),
		}}
	case tui.TaskStateEmpty:
		if m.checkDisabled() {
			break
		}
		kb.Groups = [][]tui.KeyHint{{
			verbs.UpdateRecheck.Hint(),
		}}
	}
	return kb
}
