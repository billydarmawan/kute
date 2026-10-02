package helmhistory

import (
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references only, per the
// registry invariant — 18a's pill is HELM, same short form the Releases
// list itself uses.
func (m Model) Keybar() tui.Keybar {
	if m.actions.Active() {
		if m.actions.Tier() == actions.TierInline {
			note, shortNote := m.actions.Prompt(), ""
			if pending := m.actions.Pending(); pending != nil && pending.Scope.Verb == "rollback" {
				// 18a: "shell out to helm with a will run line" — kept short
				// deliberately, same reasoning as browse's own rollbackPrompt
				// (insetChromeLine drops the whole RightNote rather than
				// truncating it on overflow), with the --kubeconfig-elided
				// summary as the fallback for when even that doesn't fit.
				target := m.session.CommandTarget()
				note = kube.HelmRollbackCommandString(target, pending.Scope.Namespace, pending.Scope.ResourceName, pending.Scope.Revision)
				shortNote = kube.HelmRollbackCommandSummary(target, pending.Scope.Namespace, pending.Scope.ResourceName, pending.Scope.Revision)
			}
			return tui.Keybar{
				Pill:           tui.ModeConfirm,
				PillText:       "CONFIRM",
				Groups:         [][]tui.KeyHint{{{Key: "y", Label: "confirm"}, {Key: "esc", Label: "cancel"}}},
				RightNote:      note,
				RightNoteShort: shortNote,
			}
		}
		return tui.Keybar{Pill: tui.ModeConfirm, PillText: "CONFIRM"}
	}
	if m.state == tui.TaskStateLoading {
		// 15a applied to a detail screen: every revision-scoped verb stays dark
		// ("row actions enable when
		// data lands", docs/design README.md §15a).
		return tui.Keybar{
			Pill:       tui.ModeBrowse,
			PillText:   "HELM",
			RightNote:  "rollback enables when the revisions land",
			RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint()),
		}
	}
	if m.state != tui.TaskStateReady {
		return tui.Keybar{
			Pill:       tui.ModeBrowse,
			PillText:   "HELM",
			RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint()),
		}
	}

	groups := [][]tui.KeyHint{{{Key: "↑↓", Label: "move"}}}
	if m.mutator != nil && !verbs.Rollback.HiddenWhileOffline(m.conn.Offline()) {
		groups = append(groups, []tui.KeyHint{verbs.Rollback.Hint()})
	}

	// 4a's offline treatment (docs/design README.md §52, §301): mutating
	// verbs disappear from the keybar the same way browse's own list does,
	// not just at the actions.Controller gate.
	pill, pillText, rightNote := tui.ModeBrowse, "HELM", m.feedback
	if m.conn.Offline() {
		pill, pillText, rightNote = tui.ModeOffline, "OFFLINE", "mutating actions disabled"
	}
	return tui.Keybar{
		Pill:       pill,
		PillText:   pillText,
		Groups:     groups,
		RightNote:  rightNote,
		RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint()),
	}
}

// ActionResult puts the actions controller's outcome line — a failed or
// successful delete, drain, cordon, restart, … — on the keybar
// (tui.ActionResulter, rendered by tui.Frame).
func (m Model) ActionResult() tui.ResultLine { return m.actions.Result() }
