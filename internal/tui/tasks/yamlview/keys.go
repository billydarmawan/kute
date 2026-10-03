package yamlview

import (
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references only — §8a's own
// search/fold/copy keys are screen-local registry entries.
func (m Model) Keybar() tui.Keybar {
	if m.searchActive {
		return tui.Keybar{
			Pill:      tui.ModeFilter,
			PillText:  "SEARCH",
			Groups:    [][]tui.KeyHint{{verbs.Back.HintAs("clear")}},
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
		{verbs.Move.HintAs("jk")},
		{verbs.YAMLFold.Hint(), verbs.YAMLUnfoldAll.Hint()},
		{verbs.Search.Hint(), verbs.YAMLCopy.Hint()},
	}
	pillText := "YAML"
	if m.isSecret {
		pillText = "SECRET"
		groups = append(groups, []tui.KeyHint{
			verbs.SecretRevealToggle.Hint(),
			verbs.SecretRevealAll.Hint(),
			verbs.SecretCopyValue.Hint(),
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
