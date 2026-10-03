// 'E' edit machinery: any row, any kind (verbs.Edit.Kinds is nil — "all
// kinds"), kubectl edit (kube.EditSpec) handed off via tea.ExecProcess, the
// same tty-suspend path openSelectedExec/selectedNodeShell already use.
// Edit never goes through kube.Mutator/actions.Controller, so this stays a
// bespoke gate (pendingEdit + updateEditConfirmKey) rather than routing
// through m.actions — kept in its own file per browse's per-concern split
// convention (like delete.go/nodes.go).
package browse

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// editTarget is the row pendingEdit gates on while the PROD y/N line shows.
type editTarget struct {
	kind      kube.ResourceKind
	namespace string
	name      string
}

// beginEdit resolves 'E' for the selected row: verbs.Resolve(verbs.Edit, …)
// decides whether to launch kubectl edit immediately (TierNone, task nil,
// cmd is the tea.ExecProcess Cmd — same return shape openSelectedExec uses)
// or stage pendingEdit for one inline y/N line first (TierInline, PROD
// only). ok is false when nothing applies (not ready, no row selected).
func (m *Model) beginEdit() (tea.Cmd, bool) {
	if m.state != tui.TaskStateReady || m.kind == kube.KindHelmRelease {
		// A Helm release isn't a real kubectl-editable object (18a has no
		// 'E' — see keys.go's baseGroup carve-out).
		return nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, false
	}
	if verbs.Resolve(verbs.Edit, verbs.Target{}, m.isProd()).Tier == verbs.TierNone {
		return editCmd(m.session.CommandTarget(), m.kind, row.Namespace, row.Name), true
	}
	m.pendingEdit = &editTarget{kind: m.kind, namespace: row.Namespace, name: row.Name}
	return nil, true
}

// editCmd suspends the program and hands the tty to kubectl edit
// (tea.ExecProcess over kube.EditSpec) — shared shape with poddetail's and
// nodedetail's own editCmd, duplicated per the repo's package-local-seam
// convention (execCmd already does the same across these three packages).
func editCmd(target kube.CommandTarget, kind kube.ResourceKind, namespace, name string) tea.Cmd {
	spec := kube.EditSpec(target, kind, namespace, name)
	return tea.ExecProcess(spec, func(err error) tea.Msg {
		return editResultMsg{err: err}
	})
}

// updateEditConfirmKey routes keys while pendingEdit's PROD y/N line is
// showing: 'y' launches, 'n'/esc cancels back to normal browsing.
func (m *Model) updateEditConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case verbs.ConfirmAccept.Key:
		target := *m.pendingEdit
		m.pendingEdit = nil
		return m, editCmd(m.session.CommandTarget(), target.kind, target.namespace, target.name)
	case verbs.ConfirmDecline.Key, verbs.ConfirmCancel.Key:
		m.pendingEdit = nil
	}
	return m, nil
}

// editConfirmPrompt renders pendingEdit's keybar RightNote — mirrors
// actions.Controller.Prompt()'s "<Verb> <target>? (y) confirm (n) cancel"
// shape for Edit's bespoke (non-Controller) gate.
func (m Model) editConfirmPrompt() string {
	t := m.pendingEdit
	target := string(t.kind)
	if t.namespace != "" {
		target += " " + t.namespace + "/" + t.name
	} else {
		target += " " + t.name
	}
	return fmt.Sprintf("Edit %s? (y) confirm  (n) cancel", target)
}
