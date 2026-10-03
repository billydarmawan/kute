package cronjobdetail

import (
	"fmt"

	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/jobactions"
	"github.com/kute-dev/kute/internal/tui/metapanel"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references only, per the
// registry invariant — 36e's pill is DETAIL (docs/design README.md §36e).
func (m Model) Keybar() tui.Keybar {
	if m.pendingRun != nil {
		return m.pendingRun.Keybar()
	}
	if m.pendingResume != nil {
		return m.pendingResume.Keybar()
	}
	if m.actions.Active() {
		if m.actions.Tier() == actions.TierModal {
			return tui.Keybar{Pill: tui.ModeConfirm, PillText: "CONFIRM"}
		}
		note := m.actions.Prompt()
		if pending := m.actions.Pending(); pending != nil {
			switch pending.Scope.Verb {
			case "cronjob-suspend":
				// Only reached outside PROD — verbs.Resolve's CronJobSuspend row
				// escalates to TierModal in PROD instead (this screen's own
				// suspendConfirmModal). cronjob-resume never appears here,
				// fixed TierNone (staged, reversible, immediate) the same way
				// cronjob-run-now is.
				note = jobactions.SuspendWillRunLine(pending.Scope)
				if m.summary.Object != nil {
					note += "   " + jobactions.SuspendDangerNote(m.summary, m.now)
				}
			case "set-meta":
				// 26a: the panel stays open under this confirm and already
				// renders the full will-run line + join warning in its own
				// strip, so this note keeps to the keybar-safe short form
				// (browse's own set-meta case, same reasoning).
				note = metapanel.WillRunLine(pending.Scope)
				if pending.Scope.MetaJoinService != "" {
					note = fmt.Sprintf("detaches %d pods from svc/%s",
						pending.Scope.MetaJoinPodCount, pending.Scope.MetaJoinService)
				}
			}
		}
		return tui.Keybar{
			Pill:      tui.ModeConfirm,
			PillText:  "CONFIRM",
			Groups:    [][]tui.KeyHint{m.actions.ConfirmHints()},
			RightNote: note,
		}
	}
	if m.meta != nil {
		// The open 26a panel's own keybar (per the Global-verb rule, 'm'
		// itself is never listed while closed — the ? overlay's RESOURCE
		// column teaches it once, app-wide).
		return tui.Keybar{Pill: tui.ModeBrowse, PillText: "META", Groups: m.meta.KeybarHints()}
	}
	if !m.found {
		return tui.Keybar{
			Pill:      tui.ModeBrowse,
			PillText:  "DETAIL",
			RightNote: "cronjob deleted · press any key to go back",
		}
	}

	groups := [][]tui.KeyHint{}
	if len(m.summary.Runs) > 0 {
		jobGroup := []tui.KeyHint{verbs.Open.HintAs("open job")}
		if m.openLogs != nil {
			jobGroup = append(jobGroup, verbs.Logs.Hint())
		}
		groups = append(groups, jobGroup)
	}
	if m.mutator != nil && !m.conn.Offline() {
		groups = append(groups, m.cronJobKeybarGroup())
	}
	if len(m.siblings) > 1 {
		groups = append(groups, []tui.KeyHint{verbs.Siblings.HintAs("sibling cronjob")})
	}

	pill, pillText, rightNote := tui.ModeBrowse, "DETAIL", m.execFeedback
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
