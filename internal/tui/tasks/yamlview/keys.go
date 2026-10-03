package yamlview

import (
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references where one exists,
// plus a handful of local literals for keys that have no registry entry
// (docs/design README.md §8a's own vocabulary — "search"/"fold"/"copy"
// aren't verbs any other screen shares).
func (m Model) Keybar() tui.Keybar {
	if m.searchActive {
		return tui.Keybar{
			Pill:      tui.ModeFilter,
			PillText:  "SEARCH",
			Groups:    [][]tui.KeyHint{{{Key: "esc", Label: "clear"}}},
			RightNote: "type to search",
		}
	}
	if m.revealAllConfirm {
		return tui.Keybar{
			Pill:      tui.ModeConfirm,
			PillText:  "CONFIRM",
			Groups:    [][]tui.KeyHint{verbs.ConfirmHints()},
			RightNote: m.revealAllConfirmPrompt(),
		}
	}

	groups := [][]tui.KeyHint{
		{{Key: "↑↓", Label: "jk"}},
		{{Key: "tab", Label: "fold/unfold"}, {Key: "f", Label: "unfold all"}},
		{{Key: "/", Label: "search"}, {Key: "Y", Label: "copy"}},
	}
	pillText := "YAML"
	if m.isSecret {
		pillText = "SECRET"
		groups = append(groups, []tui.KeyHint{
			{Key: "x", Label: "reveal/mask"},
			{Key: "X", Label: "reveal all"},
			{Key: "y", Label: "copy value"},
		})
	}

	return tui.Keybar{
		Pill:       tui.ModeBrowse,
		PillText:   pillText,
		Groups:     groups,
		RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint()),
	}
}

// CapturingInput reports whether search input is open, so the root shell
// lets every keystroke reach yamlview's own key handling instead of
// treating them as global g/n/c/? shortcuts.
func (m Model) CapturingInput() bool {
	return m.searchActive || m.revealAllConfirm
}
