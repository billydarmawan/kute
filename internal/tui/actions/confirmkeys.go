package actions

import (
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// ConfirmOutcome is what HandleConfirmKey did with a key press — what a
// screen with a real difference of its own keys a thin post-step on (26a's
// panel reverting its buffer on a cancel, 27a/27b reopening the buffer a
// commit came from), never a second router.
type ConfirmOutcome int

const (
	// ConfirmSwallowed: the key changed nothing. It is still the confirm's,
	// so nothing underneath (movement, filter, another verb) may act on it —
	// a type-the-name modal's ↵ before the name matches lands here too.
	ConfirmSwallowed ConfirmOutcome = iota
	// ConfirmExecuted: the pending action ran; the returned command carries
	// its write.
	ConfirmExecuted
	// ConfirmCancelled: the confirm was abandoned (Cancel).
	ConfirmCancelled
	// ConfirmEscalated: a pending delete became a force delete
	// (EscalateForceDelete) — the type-the-name modal now.
	ConfirmEscalated
	// ConfirmTyped: the key edited the type-the-name buffer.
	ConfirmTyped
)

// HandleConfirmKey is the one key router for an Active confirm — every
// controller-embedding screen hands it every key press while Active() and
// never matches a confirm key itself (guard_test.go). Its keys all come from
// the verbs registry:
//
//   - verbs.ForceDelete.Key escalates a pending single-target delete Begun
//     as verbs.Delete of a kind ForceDelete applies to, under that Begin's
//     own PROD tag, from an inline y/N or the type-the-name modal alike;
//   - in a type-the-name modal (TypeToConfirm), verbs.ConfirmCancel cancels,
//     verbs.ConfirmSubmit executes once the name matches (a marked set: the
//     count, whose buffer only takes digits), and anything else edits the
//     buffer — so y and n are typed, not answered;
//   - in a y/N (TierInline, or a TierModal y/N card such as Drain's),
//     verbs.ConfirmAccept executes, verbs.ConfirmDecline/ConfirmCancel
//     cancel, and anything else is swallowed.
//
// A no-op (ConfirmSwallowed) unless a confirm is showing.
func (c *Controller) HandleConfirmKey(msg tea.KeyPressMsg) (tea.Cmd, ConfirmOutcome) {
	if !c.Active() {
		return nil, ConfirmSwallowed
	}
	key := msg.String()
	if key == verbs.ForceDelete.Key && c.forceDeleteOffered() {
		c.EscalateForceDelete(c.prod)
		return nil, ConfirmEscalated
	}
	if c.TypeToConfirm() {
		switch key {
		case verbs.ConfirmCancel.Key:
			c.Cancel()
			return nil, ConfirmCancelled
		case verbs.ConfirmSubmit.Key:
			return c.confirm()
		}
		if len(c.pending.Scope.BulkTargets) > 0 && !digitsOnly(msg.Text) {
			return nil, ConfirmSwallowed
		}
		return c.HandleTypeKey(msg), ConfirmTyped
	}
	switch key {
	case verbs.ConfirmAccept.Key:
		return c.confirm()
	case verbs.ConfirmDecline.Key, verbs.ConfirmCancel.Key:
		c.Cancel()
		return nil, ConfirmCancelled
	}
	return nil, ConfirmSwallowed
}

// confirm runs Confirm and reports whether it executed.
func (c *Controller) confirm() (tea.Cmd, ConfirmOutcome) {
	cmd := c.Confirm()
	if c.Active() {
		return nil, ConfirmSwallowed
	}
	return cmd, ConfirmExecuted
}

// ConfirmHints is a y/N confirm's keybar hint group, from the verbs
// registry — accept and cancel, plus verbs.ForceDelete while
// HandleConfirmKey would escalate the pending delete — so the keys a
// screen's keybar advertises are exactly the keys its confirm answers to.
// A screen with a confirm-specific label (18a's rollback naming the
// revision) adjusts the returned slice rather than building its own.
func (c Controller) ConfirmHints() []tui.KeyHint {
	hints := verbs.ConfirmHints()
	if c.forceDeleteOffered() {
		hints = append(hints, verbs.ForceDelete.Hint())
	}
	return hints
}

// forceDeleteOffered reports whether verbs.ForceDelete.Key escalates the
// pending confirm: a Delete Begun as one (not, say, the debug panel's
// CLEAN UP, whose Scope is a "delete" too) that EscalateForceDelete would
// claim.
func (c Controller) forceDeleteOffered() bool {
	return c.requested == verbs.Delete.ID && c.deleteEscalates()
}

// digitsOnly reports whether text is safe for a marked set's count buffer —
// digits only. A non-text key (backspace, arrows, Home/End) carries no text
// and always is.
func digitsOnly(text string) bool {
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
