package timeline

import (
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references only, per the
// registry invariant — 16a/16b's pill is TIMELINE (docs/design README.md
// §16a). Branches on the active confirm (16b's 'R' rollback, mirroring
// helmhistory.Keybar's own CONFIRM handling) and on objectScoped (16a's
// 'w'/'/' don't exist in 16b; 16b's rail-focused hints don't exist in 16a).
func (m Model) Keybar() tui.Keybar {
	if m.actionsCtl.Active() {
		if m.actionsCtl.Tier() == actions.TierInline {
			note := m.actionsCtl.Prompt()
			// 16b's rollback shows the real kubectl invocation (10a/13a/
			// 17b/18a's copyable-documentation idiom) rather than the
			// generic Prompt() text — mirrors helmhistory.Keybar's own
			// rollback-specific RightNote (kept bare, no "will run:"
			// prefix, deliberately short for the compact keybar strip).
			if pending := m.actionsCtl.Pending(); pending != nil && pending.Scope.Verb == "rollout-undo" {
				note = kube.RolloutUndoCommandString(pending.Scope.Namespace, pending.Scope.ResourceName, pending.Scope.Revision)
			}
			return tui.Keybar{
				Pill:      tui.ModeConfirm,
				PillText:  "CONFIRM",
				Groups:    [][]tui.KeyHint{{{Key: "y", Label: "confirm"}, {Key: "esc", Label: "cancel"}}},
				RightNote: note,
			}
		}
		return tui.Keybar{Pill: tui.ModeConfirm, PillText: "CONFIRM"}
	}

	if m.state == tui.TaskStateEmpty && len(m.rail) == 0 {
		return tui.Keybar{Pill: tui.ModeBrowse, PillText: "TIMELINE", RightNote: "0 changes · watching"}
	}
	if m.filterActive {
		return tui.Keybar{
			Pill:      tui.ModeFilter,
			PillText:  "FILTER",
			Groups:    [][]tui.KeyHint{{{Key: "esc", Label: "clear"}}},
			RightNote: "type to narrow",
		}
	}

	if m.objectScoped() {
		return m.objectKeybar()
	}
	return m.namespaceKeybar()
}

// namespaceKeybar is 16a's keybar: window/'w'/'/' always (mirrors
// tasks/events' own Keybar — 'w's label toggles with warningsOnly); 'tab'
// expand/collapse only once there's a normal group to fold/expand.
func (m Model) namespaceKeybar() tui.Keybar {
	warnLabel := "warnings only"
	if m.warningsOnly {
		warnLabel = "all events"
	}
	groups := [][]tui.KeyHint{
		{{Key: "w", Label: warnLabel}, {Key: "t", Label: "time window"}},
		{verbs.Filter.Hint()},
	}
	if m.normalPresent {
		groups = append(groups, []tui.KeyHint{{Key: "tab", Label: "expand/collapse normal"}})
	}
	if m.revisionSelected() {
		// §32a/§34a: only offered on a revision or sync row — there is
		// nothing to copy from an ordinary event, and a key that does
		// nothing is worse than an absent one.
		groups = append(groups, []tui.KeyHint{{Key: "v", Label: "copy revision"}})
	}
	return tui.Keybar{
		Pill:       tui.ModeBrowse,
		PillText:   "TIMELINE",
		Groups:     groups,
		RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint()),
	}
}

// objectKeybar is 16b's keybar: window always; once a revision
// rail resolved, 'tab' toggles rail focus between it and the feed. Moving
// the rail cursor live-syncs the feed's own cursor to that revision's
// ROLLOUT row (update.go's syncFeedToRailSelection) — no '↵' needed — so
// '↵' always means "open the selected feed row's object", regardless of
// which pane has focus. '↑↓ select revision' and 'R' rollback (hidden while
// OFFLINE, same 4a treatment helmhistory.Keybar's own Rollback hint gets)
// only apply while the rail has focus.
func (m Model) objectKeybar() tui.Keybar {
	groups := [][]tui.KeyHint{
		{{Key: "t", Label: "time window"}},
	}
	if len(m.rail) > 0 {
		groups = append(groups, []tui.KeyHint{{Key: "tab", Label: "focus rail/feed"}})
		if m.railFocused {
			rail := []tui.KeyHint{{Key: "↑↓", Label: "select revision"}}
			if m.mutator != nil && !verbs.RolloutUndo.HiddenWhileOffline(m.conn.Offline()) {
				hint := verbs.RolloutUndo.Hint()
				// rail[0], the current revision, is no rollback target
				// (update.go refuses it).
				hint.Disabled = m.railSelected == 0
				rail = append(rail, hint)
			}
			groups = append(groups, rail)
		}
	}

	pill, pillText, rightNote := tui.ModeBrowse, "TIMELINE", m.feedback
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
func (m Model) ActionResult() tui.ResultLine { return m.actionsCtl.Result() }
