// 'E' edit machinery for the loaded pod: kubectl edit (kube.EditSpec)
// handed off via tea.ExecProcess, the same tty-suspend path openSelectedExec
// already uses. Edit never goes through kube.Mutator/actions.Controller, so
// this stays a bespoke gate (pendingEdit + updateEditConfirmKey) rather than
// routing through m.actions — mirrors browse/edit.go.
package poddetail

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// editTarget is the pod pendingEdit gates on while the PROD y/N line shows.
type editTarget struct {
	namespace string
	name      string
}

// beginEdit resolves 'E' for the loaded pod: verbs.Resolve(verbs.Edit, …)
// decides whether to launch kubectl edit immediately (TierNone, cmd is the
// tea.ExecProcess Cmd) or stage pendingEdit for one inline y/N line first
// (TierInline, PROD only). ok is false when no pod is loaded.
func (m *Model) beginEdit() (tea.Cmd, bool) {
	if !m.found {
		return nil, false
	}
	if verbs.Resolve(verbs.Edit, verbs.Target{}, m.isProd()).Tier == verbs.TierNone {
		return editCmd(m.session.CommandTarget(), m.namespace, m.name), true
	}
	m.pendingEdit = &editTarget{namespace: m.namespace, name: m.name}
	return nil, true
}

// editCmd suspends the program and hands the tty to kubectl edit
// (tea.ExecProcess over kube.EditSpec) — shared shape with browse's and
// nodedetail's own editCmd, duplicated per the repo's package-local-seam
// convention.
func editCmd(target kube.CommandTarget, namespace, name string) tea.Cmd {
	spec := kube.EditSpec(target, kube.KindPod, namespace, name)
	return tea.ExecProcess(spec, func(err error) tea.Msg {
		return editResultMsg{err: err}
	})
}

// updateEditConfirmKey routes keys while pendingEdit's PROD y/N line is
// showing: 'y' launches, 'n'/esc cancels back to normal detail view.
func (m *Model) updateEditConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y":
		target := *m.pendingEdit
		m.pendingEdit = nil
		return m, editCmd(m.session.CommandTarget(), target.namespace, target.name)
	case "n", "esc":
		m.pendingEdit = nil
	}
	return m, nil
}

// editConfirmPrompt renders pendingEdit's keybar RightNote — mirrors
// actions.Controller.Prompt()'s "<Verb> <target>? (y) confirm (n) cancel"
// shape for Edit's bespoke (non-Controller) gate.
func (m Model) editConfirmPrompt() string {
	t := m.pendingEdit
	return fmt.Sprintf("Edit %s %s/%s? (y) confirm  (n) cancel", kube.KindPod, t.namespace, t.name)
}
