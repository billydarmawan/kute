package podlogs

import (
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band — 5b's pill is LOGS. The warning and error
// jumps are deliberately separate hints: both search forward (and wrap at
// the end), so the former combined "prev/next warn/err" label described
// behavior the screen never had. Uppercase W exposes the display-only wrap
// toggle without taking lowercase w away from next-warning navigation.
func (m Model) Keybar() tui.Keybar {
	if m.filterActive {
		return tui.Keybar{
			Pill:      tui.ModeFilter,
			PillText:  "FILTER",
			Groups:    [][]tui.KeyHint{{verbs.Back.HintAs("clear")}},
			RightNote: "type to narrow",
		}
	}

	follow := verbs.LogFollow
	if m.view.AutoScroll {
		follow = verbs.LogPause
	}
	groups := [][]tui.KeyHint{{
		follow.Hint(),
		verbs.LogNextWarning.Hint(),
		verbs.LogNextError.Hint(),
		verbs.LogToggleWrap.Hint(),
	}}
	nav := []tui.KeyHint{verbs.LogCycleSince.Hint()}
	if len(m.pod.Containers) > 1 {
		nav = append(nav, verbs.LogCycleContainer.Hint())
	}
	nav = append(nav, verbs.LogCopyView.Hint())
	groups = append(groups, nav)

	return tui.Keybar{
		Pill:       tui.ModeBrowse,
		PillText:   "LOGS",
		Groups:     groups,
		RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint()),
	}
}
