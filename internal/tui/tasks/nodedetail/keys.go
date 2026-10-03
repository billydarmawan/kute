package nodedetail

import (
	"fmt"

	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/metapanel"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references only, per the
// registry invariant — 11b's pill is NODE (docs/design README.md §11b).
func (m Model) Keybar() tui.Keybar {
	if m.pendingEdit != nil {
		return tui.Keybar{
			Pill:      tui.ModeConfirm,
			PillText:  "CONFIRM",
			Groups:    [][]tui.KeyHint{verbs.ConfirmHints()},
			RightNote: m.editConfirmPrompt(),
		}
	}
	if m.actions.Active() {
		if m.actions.Tier() == actions.TierInline {
			hints := m.actions.ConfirmHints()
			note := m.actions.Prompt()
			if pending := m.actions.Pending(); pending != nil && pending.Scope.Verb == "set-meta" {
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
			return tui.Keybar{
				Pill:      tui.ModeConfirm,
				PillText:  "CONFIRM",
				Groups:    [][]tui.KeyHint{hints},
				RightNote: note,
			}
		}
		return tui.Keybar{Pill: tui.ModeConfirm, PillText: "CONFIRM"}
	}
	if m.meta != nil {
		// The open 26a panel's own keybar (per the Global-verb rule, 'm'
		// itself is never listed while closed — the ? overlay's RESOURCE
		// column teaches it once, app-wide).
		return tui.Keybar{Pill: tui.ModeBrowse, PillText: "META", Groups: m.meta.KeybarHints()}
	}

	if m.state == tui.TaskStateLoading {
		// 15a applied to a detail screen: every row/node-scoped verb stays dark
		// (docs/design README.md §15a: "row actions enable when data
		// lands").
		return tui.Keybar{
			Pill:      tui.ModeBrowse,
			PillText:  "NODE",
			RightNote: "facts & pods enable when data lands",
		}
	}

	if m.filterActive {
		return tui.Keybar{
			Pill:      tui.ModeFilter,
			PillText:  "FILTER",
			Groups:    [][]tui.KeyHint{{verbs.Back.HintAs("clear")}},
			RightNote: "type to narrow",
		}
	}

	offline := m.conn.Offline()
	groups := [][]tui.KeyHint{}
	if len(m.pods) > 0 {
		podGroup := []tui.KeyHint{}
		if m.openLogs != nil {
			podGroup = append(podGroup, verbs.Logs.Hint())
		}
		if !verbs.Exec.HiddenWhileOffline(offline) {
			podGroup = append(podGroup, verbs.Exec.Hint())
		}
		if m.openForward != nil {
			podGroup = append(podGroup, verbs.Forward.Hint())
		}
		groups = append(groups, podGroup)
	}
	if !verbs.NodeDebugDetail.HiddenWhileOffline(offline) {
		groups = append(groups, []tui.KeyHint{verbs.NodeDebugDetail.Hint()})
	}
	if m.mutator != nil && !verbs.Cordon.HiddenWhileOffline(offline) && !verbs.Drain.HiddenWhileOffline(offline) {
		groups = append(groups, []tui.KeyHint{verbs.Cordon.Hint(), verbs.Drain.Hint()})
	}

	// 4a's offline treatment (docs/design README.md §52, §301): mutating
	// verbs disappear from the keybar the same way browse's own list does,
	// not just at the actions.Controller gate.
	pill, pillText, rightNote := tui.ModeBrowse, "NODE", m.execFeedback
	if m.conn.Offline() {
		pill, pillText, rightNote = tui.ModeOffline, "OFFLINE", "mutating actions disabled"
	}
	return tui.Keybar{
		Pill:      pill,
		PillText:  pillText,
		Groups:    groups,
		RightNote: rightNote,
	}
}

// CapturingInput reports whether a confirm card is open, so the root shell
// lets y/n reach nodedetail's own key handling instead of treating them as
// global shortcuts (mirrors browse.CapturingInput).
func (m Model) CapturingInput() bool {
	return m.actions.Active() || m.filterActive || m.pendingEdit != nil || m.meta != nil
}

// ActionResult puts the actions controller's outcome line — a failed or
// successful delete, drain, cordon, restart, … — on the keybar
// (tui.ActionResulter, rendered by tui.Frame).
func (m Model) ActionResult() tui.ResultLine { return m.actions.Result() }
