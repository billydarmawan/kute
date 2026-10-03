package certchain

import (
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar is §35a's chain-specific y/e hints, plus §35c's 'r' force-renew
// when a mutator is wired. Generic back/open navigation lives in help.
func (m Model) Keybar() tui.Keybar {
	if m.actions.Active() {
		if m.actions.Tier() == actions.TierInline {
			note := m.actions.Prompt()
			if pending := m.actions.Pending(); pending != nil && pending.Scope.Verb == "cert-renew" {
				// 35c: the exact "will run: kubectl patch certificate/...
				// --subresource=status ..." line, same idiom as helmhistory's
				// own rollback confirm.
				note = certRenewWillRunLine(pending.Scope)
			}
			return tui.Keybar{
				Pill:      tui.ModeConfirm,
				PillText:  "CONFIRM",
				Groups:    [][]tui.KeyHint{m.actions.ConfirmHints()},
				RightNote: note,
			}
		}
		return tui.Keybar{Pill: tui.ModeConfirm, PillText: "CONFIRM"}
	}
	if m.state != tui.TaskStateReady {
		return tui.Keybar{Pill: tui.ModeBrowse, PillText: "CHAIN"}
	}
	verbGroup := []tui.KeyHint{verbs.YAML.Hint(), verbs.Events.Hint()}
	// 4a's offline treatment (docs/design README.md §52, §301): the renew
	// hint disappears from the keybar the same way browse's own list does,
	// not just at the actions.Controller gate.
	if m.mutator != nil && !verbs.CertRenew.HiddenWhileOffline(m.conn.Offline()) {
		verbGroup = append(verbGroup, verbs.CertRenew.Hint())
	}
	return tui.Keybar{
		Pill:       tui.ModeBrowse,
		PillText:   "CHAIN",
		Groups:     [][]tui.KeyHint{verbGroup},
		RightNote:  m.feedback,
		RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint()),
	}
}

// ActionResult puts the actions controller's outcome line — a failed or
// successful delete, drain, cordon, restart, … — on the keybar
// (tui.ActionResulter, rendered by tui.Frame).
func (m Model) ActionResult() tui.ResultLine { return m.actions.Result() }
