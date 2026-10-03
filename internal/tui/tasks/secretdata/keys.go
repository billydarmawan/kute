package secretdata

import (
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references only, per the
// registry invariant — 27b's pill is DATA in navigation, ADD KEY while the
// line-insert row is showing, EDIT VALUE while an existing key's
// decode-then-edit is showing, CONFIRM while any of the three's y/N is
// deciding.
func (m Model) Keybar() tui.Keybar {
	if m.actions.Active() {
		note := ""
		if removing, key := m.pendingRemove(); removing {
			note = kube.SecretDataCommandString(m.namespace, m.name, key, true)
		} else if pc := m.pendingCommit; pc != nil && !pc.remove {
			note = kube.SecretDataCommandString(m.namespace, m.name, pc.key, false)
		}
		return tui.Keybar{
			Pill:      tui.ModeConfirm,
			PillText:  "CONFIRM",
			Groups:    [][]tui.KeyHint{m.actions.ConfirmHints()},
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
				verbs.Open.HintAs("apply"),
				verbs.SecretMaskToggle.HintAs("re-mask input"),
				verbs.SecretPaste.Hint(),
				verbs.Back.HintAs("discard"),
			}},
		}
	}
	if m.editing != nil {
		return tui.Keybar{
			Pill:     tui.ModeBrowse,
			PillText: "EDIT VALUE",
			Groups: [][]tui.KeyHint{{
				verbs.Open.HintAs("save"),
				verbs.SecretMaskToggle.Hint(),
				verbs.SecretPaste.Hint(),
				verbs.Back.HintAs("cancel"),
			}},
		}
	}

	groups := [][]tui.KeyHint{{verbs.Move.Hint()}}
	if m.mutator != nil && !verbs.AddSecretKey.HiddenWhileOffline(m.conn.Offline()) {
		groups = append(groups, []tui.KeyHint{
			verbs.Open.HintAs("edit"), verbs.AddSecretKey.Hint(), verbs.RemoveSecretKey.Hint(),
		})
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
