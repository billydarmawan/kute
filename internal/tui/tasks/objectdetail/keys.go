package objectdetail

import (
	"fmt"

	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/metapanel"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references only, per the
// registry invariant — 14d's pill is DETAIL (docs/design README.md §14d).
func (m Model) Keybar() tui.Keybar {
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
	if m.gone {
		return tui.Keybar{
			Pill:      tui.ModeBrowse,
			PillText:  "DETAIL",
			RightNote: m.desc.Display + " deleted · press any key to go back",
		}
	}

	groups := [][]tui.KeyHint{}
	if len(m.siblings) > 1 {
		groups = append(groups, []tui.KeyHint{{Key: "j/k", Label: "next/prev"}})
	}
	if firstOwnerRef(m.obj) != "" {
		groups = append(groups, []tui.KeyHint{{Key: "1", Label: "owner"}})
	}
	if m.mutator != nil && !verbs.Delete.HiddenWhileOffline(m.conn.Offline()) {
		groups = append(groups, []tui.KeyHint{verbs.Delete.Hint()})
	}

	// 4a's offline treatment (docs/design README.md §52, §301): mutating
	// verbs disappear from the keybar the same way browse's own list does,
	// not just at the actions.Controller gate.
	pill, pillText, rightNote := tui.ModeBrowse, "DETAIL", ""
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

// CapturingInput reports whether a confirm is active or the object-gone
// state is showing — mirrors poddetail's own reasoning.
func (m Model) CapturingInput() bool {
	return m.actions.Active() || m.gone || m.meta != nil
}

// ActionResult puts the actions controller's outcome line — a failed or
// successful delete, drain, cordon, restart, … — on the keybar
// (tui.ActionResulter, rendered by tui.Frame).
func (m Model) ActionResult() tui.ResultLine { return m.actions.Result() }
