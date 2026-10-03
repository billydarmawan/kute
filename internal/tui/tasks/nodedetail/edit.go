// 'E' edit machinery for the loaded node: kubectl edit (kube.EditSpec)
// handed off via tea.ExecProcess, the same tty-suspend path openSelectedExec
// already uses for the node's pods. Edit never goes through kube.Mutator/
// actions.Controller, so this stays a bespoke gate (pendingEdit +
// updateEditConfirmKey) rather than routing through m.actions — mirrors
// browse/edit.go and poddetail/edit.go.
package nodedetail

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// editTarget is the node pendingEdit gates on while the PROD y/N line shows.
type editTarget struct{ name string }

// isProd reports whether the active session's current context is tagged
// prod in ~/.config/kute/config.yaml — mirrors browse's and poddetail's
// own isProd().
func (m Model) isProd() bool {
	if m.session == nil {
		return false
	}
	return m.session.Config.IsProd(m.session.Location.Context)
}

// beginEdit resolves 'E' for the loaded node: verbs.Resolve(verbs.Edit, …)
// decides whether to launch kubectl edit immediately (TierNone, cmd is the
// tea.ExecProcess Cmd) or stage pendingEdit for one inline y/N line first
// (TierInline, PROD only). ok is false when no node is loaded.
func (m *Model) beginEdit() (tea.Cmd, bool) {
	if m.node == nil {
		return nil, false
	}
	if verbs.Resolve(verbs.Edit, verbs.Target{}, m.isProd()).Tier == verbs.TierNone {
		return editCmd(m.session.CommandTarget(), m.nodeName), true
	}
	m.pendingEdit = &editTarget{name: m.nodeName}
	return nil, true
}

// editCmd suspends the program and hands the tty to kubectl edit
// (tea.ExecProcess over kube.EditSpec) — shared shape with browse's and
// poddetail's own editCmd, duplicated per the repo's package-local-seam
// convention.
func editCmd(target kube.CommandTarget, name string) tea.Cmd {
	spec := kube.EditSpec(target, kube.KindNode, "", name)
	return tea.ExecProcess(spec, func(err error) tea.Msg {
		return editResultMsg{err: err}
	})
}

// updateEditConfirmKey routes keys while pendingEdit's PROD y/N line is
// showing: 'y' launches, 'n'/esc cancels back to normal detail view.
func (m *Model) updateEditConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case verbs.ConfirmAccept.Key:
		target := *m.pendingEdit
		m.pendingEdit = nil
		return m, editCmd(m.session.CommandTarget(), target.name)
	case verbs.ConfirmDecline.Key, verbs.ConfirmCancel.Key:
		m.pendingEdit = nil
	}
	return m, nil
}

// editConfirmPrompt renders pendingEdit's keybar RightNote — mirrors
// actions.Controller.Prompt()'s "<Verb> <target>? (y) confirm (n) cancel"
// shape for Edit's bespoke (non-Controller) gate.
func (m Model) editConfirmPrompt() string {
	return fmt.Sprintf("Edit %s %s? (y) confirm  (n) cancel", kube.KindNode, m.pendingEdit.name)
}
