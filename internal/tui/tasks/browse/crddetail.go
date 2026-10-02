// CRD-specific browse machinery for 14b/14d (docs/design README.md): the
// CustomResourceDefinitions list's ↵ "open this kind's instances" routing
// jump, and any discovered kind's ↵ into the 14d generic detail screen.
// Kept in its own file, browse's per-concern split convention (like
// deployments.go/nodes.go/delete.go) rather than sprinkled through
// model.go/update.go.
package browse

import (
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// openCRDInstances resolves ↵ on a CustomResourceDefinitions row: jumps
// straight to that kind's instance list (14a) via the same GotoKindMsg the
// goto palette's own kind-switch emits — "CRDs are a routing layer, like
// events" (docs/design README.md §14b). row.Key carries the discovered
// instance kind's ResourceKind string (projectCRD's doc comment) — the same
// opaque-id convention Forward's session ID already uses. ok is false when
// nothing's selected or the row has no kind to jump to.
func (m Model) openCRDInstances() (tea.Cmd, bool) {
	row, ok := m.selectedRow()
	if !ok || row.Key == "" {
		return nil, false
	}
	kind := kube.ResourceKind(row.Key)
	return func() tea.Msg { return tui.GotoKindMsg{Kind: kind} }, true
}

// openSelectedObjectDetail pushes 14d for the selected row of any Custom
// (discovered CRD) kind — the one generic branch that scales to every
// discovered kind without per-kind code, per CLAUDE.md's "CRD support is
// data, not code" invariant. ok is false when the current kind isn't Custom,
// the hook isn't wired, or nothing's selected.
func (m Model) openSelectedObjectDetail() (tea.Model, tea.Cmd, bool) {
	if !m.desc.Custom || m.openObjectDetail == nil {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	siblings := make([]ObjectSiblingRef, len(m.visible))
	index := 0
	for i, fm := range m.visible {
		siblings[i] = ObjectSiblingRef{Namespace: fm.row.Namespace, Name: fm.row.Name}
		if fm.row.Namespace == row.Namespace && fm.row.Name == row.Name {
			index = i
		}
	}
	task, cmd := m.openObjectDetail(m.kind, row.Namespace, row.Name, siblings, index, m.width, m.height)
	return task, cmd, task != nil
}
