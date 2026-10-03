package jobattempts

import (
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/jobactions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references only, per the
// registry invariant — mirrors cronjobdetail's own; pill is ATTEMPTS.
func (m Model) Keybar() tui.Keybar {
	if m.pendingRerun != nil {
		return m.pendingRerun.Keybar()
	}
	if m.actions.Active() {
		if m.actions.Tier() == actions.TierModal {
			return tui.Keybar{Pill: tui.ModeConfirm, PillText: "CONFIRM"}
		}
		note := m.actions.Prompt()
		if pending := m.actions.Pending(); pending != nil {
			switch pending.Scope.Verb {
			case "job-retry":
				note = jobactions.RetryWillRunLine(pending.Scope)
			case "job-replace":
				note = jobactions.ReplaceWillRunLine(pending.Scope)
			}
		}
		return tui.Keybar{
			Pill:      tui.ModeConfirm,
			PillText:  "CONFIRM",
			Groups:    [][]tui.KeyHint{m.actions.ConfirmHints()},
			RightNote: note,
		}
	}
	if !m.found {
		return tui.Keybar{
			Pill:      tui.ModeBrowse,
			PillText:  "ATTEMPTS",
			RightNote: "job deleted · press any key to go back",
		}
	}
	if m.diffMode {
		return tui.Keybar{
			Pill:      tui.ModeBrowse,
			PillText:  "ATTEMPTS",
			Groups:    [][]tui.KeyHint{{verbs.Either("back", verbs.JobAttemptDiff, verbs.Back)}},
			RightNote: "comparing selected attempt vs attempt 1",
		}
	}

	groups := [][]tui.KeyHint{}
	attemptGroup := []tui.KeyHint{verbs.Logs.Hint()}
	if len(m.summary.Attempts) >= 2 {
		attemptGroup = append(attemptGroup, verbs.JobAttemptDiff.Hint())
	}
	groups = append(groups, attemptGroup)
	groups = append(groups, []tui.KeyHint{verbs.Events.Hint()})
	if m.mutator != nil && !m.conn.Offline() {
		groups = append(groups, []tui.KeyHint{verbs.JobRetry.Hint()})
	}
	if m.summary.Indexed {
		failedOnly := verbs.JobIndexFailedOnly.Hint()
		if m.failedOnly {
			failedOnly.Label = "all indexes"
		}
		groups = append(groups, []tui.KeyHint{failedOnly})
	}
	if len(m.siblings) > 1 {
		groups = append(groups, []tui.KeyHint{verbs.Siblings.HintAs("sibling job")})
	}

	pill, pillText, rightNote := tui.ModeBrowse, "ATTEMPTS", m.execFeedback
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
