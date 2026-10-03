package poddetail

import (
	"fmt"

	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/metapanel"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references only, per the
// registry invariant — 5a's pill is POD (docs/design README.md §5a).
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
	if m.gone {
		return tui.Keybar{
			Pill:      tui.ModeBrowse,
			PillText:  "POD",
			RightNote: "pod deleted · press any key to go back",
		}
	}

	maxBodyOffset := 0
	if m.found {
		maxBodyOffset = m.bodyMaxOffset()
	}
	narrowOverflow := maxBodyOffset > 0 && m.width < 100

	groups := [][]tui.KeyHint{}
	verbGroup := []tui.KeyHint{}
	if m.openLogs != nil {
		verbGroup = append(verbGroup, verbs.Logs.Hint())
	}
	if m.found && len(m.pod.ContainerInfos) > 0 && !verbs.Exec.HiddenWhileOffline(m.conn.Offline()) {
		verbGroup = append(verbGroup, verbs.Exec.Hint())
	}
	if m.found && m.openForward != nil {
		verbGroup = append(verbGroup, verbs.Forward.Hint())
	}
	// 'v' only when there's something to show — every real pod has at least
	// the injected service-account mount, so this is nearly always present
	// against a cluster.
	if m.found && len(m.sources) > 0 && (!narrowOverflow || m.bodyOffset == 0) {
		verbGroup = append(verbGroup, verbs.EnvMounts.Hint())
	}
	// RELATED's numbered entries (owner workload, fronting Ingress) share
	// one hint slot keyed to however many are actually resolved — spelling
	// each one out doesn't fit this band's width budget alongside everything
	// else already curated here (5a's kitchen-sink fixture already renders
	// at ~zero slack).
	if n := len(m.related); n > 0 && (!narrowOverflow || m.bodyOffset == 0) {
		key := "1"
		if n > 1 {
			key = fmt.Sprintf("1-%d", n)
		}
		verbGroup = append(verbGroup, tui.KeyHint{Key: key, Label: "related"})
	}
	if len(verbGroup) > 0 {
		groups = append(groups, verbGroup)
	}
	if m.mutator != nil && !verbs.Delete.HiddenWhileOffline(m.conn.Offline()) {
		groups = append(groups, []tui.KeyHint{verbs.Delete.Hint()})
	}
	if len(m.siblings) > 1 && !narrowOverflow {
		groups = append(groups, []tui.KeyHint{verbs.Siblings.Hint()})
	}
	// 4a's offline treatment (docs/design README.md §52, §301): mutating
	// verbs disappear from the keybar the same way browse's own list does,
	// not just at the actions.Controller gate.
	pill, pillText, rightNote := tui.ModeBrowse, "POD", m.execFeedback
	if m.conn.Offline() {
		pill, pillText, rightNote = tui.ModeOffline, "OFFLINE", "mutating actions disabled"
	}
	rightHints := tui.UpdateRightHints(m.session)
	if maxBodyOffset > 0 {
		if m.bodyOffset > 0 {
			rightHints = append(rightHints, verbs.PageUp.Hint())
		}
		if m.bodyOffset < maxBodyOffset {
			rightHints = append(rightHints, verbs.PageDown.Hint())
		}
	}
	rightHints = append(rightHints, verbs.Help.Hint())
	return tui.Keybar{
		Pill:       pill,
		PillText:   pillText,
		Groups:     groups,
		RightNote:  rightNote,
		RightHints: rightHints,
	}
}

// CapturingInput reports whether a confirm card is open, or the pod-gone
// state is showing (every key becomes "go back") — either way the root
// shell should let poddetail's own key handling see every keystroke instead
// of treating them as global g/n/c/? shortcuts.
func (m Model) CapturingInput() bool {
	return m.actions.Active() || m.gone || m.pendingEdit != nil || m.meta != nil
}

// ActionResult puts the actions controller's outcome line — a failed or
// successful delete, drain, cordon, restart, … — on the keybar
// (tui.ActionResulter, rendered by tui.Frame).
func (m Model) ActionResult() tui.ResultLine { return m.actions.Result() }
