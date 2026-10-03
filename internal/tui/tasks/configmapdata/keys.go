package configmapdata

import (
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references only, per the
// registry invariant — 27a's pill is DATA in navigation, ADD KEY while the
// line-insert row is showing, EDIT VALUE while the single-line in-place
// edit is showing, BUFFER EDITOR while the multi-line buffer editor is
// showing, CONFIRM while any of the three's y/N is deciding.
func (m Model) Keybar() tui.Keybar {
	if m.actions.TypeToConfirm() {
		// ctrl-r's PROD type-the-name modal carries its own keys and
		// will-run lines.
		return tui.Keybar{Pill: tui.ModeConfirm, PillText: "CONFIRM"}
	}
	if m.actions.Active() {
		note := ""
		if removing, key := m.pendingRemove(); removing {
			note = kube.ConfigMapDataCommandString(m.namespace, m.name, key, "", true)
		} else if pc := m.pendingCommit; pc != nil && !pc.remove {
			note = kube.ConfigMapDataCommandString(m.namespace, m.name, pc.key, pc.value, false)
		}
		return tui.Keybar{
			Pill:      tui.ModeConfirm,
			PillText:  "CONFIRM",
			Groups:    [][]tui.KeyHint{{{Key: "y", Label: "confirm"}, {Key: "esc", Label: "cancel"}}},
			RightNote: note,
		}
	}
	if m.state != tui.TaskStateReady {
		return tui.Keybar{
			Pill:       tui.ModeBrowse,
			PillText:   "DATA",
			RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint()),
		}
	}
	if m.adding != nil {
		return tui.Keybar{
			Pill:     tui.ModeBrowse,
			PillText: "ADD KEY",
			Groups: [][]tui.KeyHint{{
				{Key: "↵", Label: "apply"},
				verbs.RestartConfigMapConsumers.Hint(),
				{Key: "esc", Label: "discard"},
			}},
		}
	}
	if m.editing != nil {
		return tui.Keybar{
			Pill:     tui.ModeBrowse,
			PillText: "EDIT VALUE",
			Groups: [][]tui.KeyHint{{
				{Key: "↵", Label: "apply"},
				verbs.RestartConfigMapConsumers.Hint(),
				{Key: "esc", Label: "discard"},
			}},
		}
	}
	if m.multiline != nil {
		return tui.Keybar{
			Pill:     tui.ModeBrowse,
			PillText: "BUFFER EDITOR",
			Groups: [][]tui.KeyHint{{
				{Key: "alt+a", Label: "apply"},
				verbs.RestartConfigMapConsumers.Hint(),
				{Key: "esc", Label: "discard"},
			}},
		}
	}

	groups := [][]tui.KeyHint{{{Key: "↑↓", Label: "move"}}}
	if m.mutator != nil && !verbs.AddConfigMapKey.HiddenWhileOffline(m.conn.Offline()) {
		hints := []tui.KeyHint{{Key: "↵", Label: "edit"}, verbs.AddConfigMapKey.Hint(), verbs.RemoveConfigMapKey.Hint()}
		if row, ok := m.selectedKeyRow(); ok && row.multiline() {
			hints = append(hints, tui.KeyHint{Key: "e", Label: "buffer editor"})
		}
		groups = append(groups, hints)
	}

	// 4a's offline treatment (docs/design README.md §52, §301): mutating
	// verbs disappear from the keybar the same way browse's own list does.
	pill, pillText, rightNote := tui.ModeBrowse, "DATA", m.feedback
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
