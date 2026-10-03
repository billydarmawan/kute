package fluxtree

import (
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references only, per the
// registry invariant. Pill is FLUX (docs/design README.md §30b).
func (m Model) Keybar() tui.Keybar {
	const pillText = "FLUX"

	if m.state == tui.TaskStateLoading {
		return tui.Keybar{
			Pill:      tui.ModeBrowse,
			PillText:  pillText,
			RightNote: "rows enable when the Flux caches fill",
		}
	}
	groups := [][]tui.KeyHint{}
	row, hasRow := m.selectedRow()
	if hasRow {
		// ↵ reads differently on the two halves of the chain and the label
		// says which: a reconciler has an inventory to open (§31a), a source
		// has an artifact and a list row.
		if !row.isSource {
			open := verbs.Open
			open.Label = "inventory"
			groups = append(groups, []tui.KeyHint{{Key: open.Key, Label: open.Label}})
		}
	}
	if m.verbsApply() && hasRow {
		// One key, scope following the cursor: on a reconciler 'r' reconciles
		// with-source, which the will-run line spells out in full.
		reconcile := verbs.FluxReconcile
		if !row.isSource && row.sourceName != "" {
			reconcile.Label = "reconcile"
		}
		suspend := verbs.FluxSuspend
		if row.suspended {
			suspend.Label = "resume"
		}
		hints := []tui.KeyHint{
			{Key: reconcile.Key, Label: reconcile.Label},
			{Key: suspend.Key, Label: suspend.Label},
		}
		if !row.isSource && row.sourceName != "" {
			hints = append(hints, verbs.FluxSource.Hint())
		}
		groups = append(groups, hints)
	}
	if m.foldedSources > 0 || m.expanded {
		groups = append(groups, []tui.KeyHint{verbs.ToggleGroup.HintAs("expand/collapse ready")})
	}

	return tui.Keybar{
		Pill:     tui.ModeBrowse,
		PillText: pillText,
		Groups:   groups,
	}
}

// CapturingInput reports whether fluxtree has an open free-text input — it
// never does (no filter, and its two mutating verbs are TierNone), so the
// root shell's global shortcuts always reach it as-is.
func (m Model) CapturingInput() bool { return false }

// ActionResult puts the actions controller's outcome line — a failed or
// successful delete, drain, cordon, restart, … — on the keybar
// (tui.ActionResulter, rendered by tui.Frame).
func (m Model) ActionResult() tui.ResultLine { return m.actionsCtl.Result() }
