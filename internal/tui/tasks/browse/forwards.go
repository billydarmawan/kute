// Forward-specific browse machinery for 13a/13c (docs/design README.md):
// pushing tasks/forwardpicker from a Pod/Service/Deployment row, and the
// Forwards-kind verbs (x stop, r restart, X stop all, y copy url) that act
// directly on the shared kube.ForwardManager. Kept in its own file, browse's
// per-concern split convention (like nodes.go/deployments.go/edit.go).
package browse

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// openSelectedForward pushes 13a for the selected Pod/Service/Deployment
// row. ok is false when forwarding isn't wired, the kind doesn't support
// it, or nothing's selected — 'f' stays a no-op rather than pushing a
// broken screen, mirroring openSelectedExec's contract.
func (m Model) openSelectedForward() (tea.Model, tea.Cmd, bool) {
	if m.openForward == nil {
		return nil, nil, false
	}
	switch m.kind {
	case kube.KindPod, kube.KindService, kube.KindDeployment:
	default:
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	target := kube.ForwardTarget{Kind: m.kind, Namespace: row.Namespace, Name: row.Name}
	task, cmd := m.openForward(target, m.width, m.height)
	return task, cmd, task != nil
}

// stopSelectedForward ends the selected Forwards row's session immediately
// (docs/design README.md §13c: "x executes immediately (reversible)").
func (m Model) stopSelectedForward() tea.Cmd {
	if m.forwards == nil {
		return nil
	}
	row, ok := m.selectedRow()
	if !ok || row.Key == "" {
		return nil
	}
	m.forwards.Stop(row.Key)
	return nil
}

// restartSelectedForward force-reconnects the selected Forwards row,
// bypassing any pending backoff.
func (m Model) restartSelectedForward() tea.Cmd {
	if m.forwards == nil {
		return nil
	}
	row, ok := m.selectedRow()
	if !ok || row.Key == "" {
		return nil
	}
	m.forwards.Restart(row.Key)
	return nil
}

// copySelectedForwardURL copies the selected Forwards row's local address —
// row.Cells[0] is the LOCAL cell ("localhost:8080", projectForward's
// layout) — as a fetchable URL.
func (m Model) copySelectedForwardURL() tea.Cmd {
	row, ok := m.selectedRow()
	if !ok || len(row.Cells) == 0 {
		return nil
	}
	return tea.SetClipboard("http://" + row.Cells[0])
}

// beginStopAllForwards arms 13c's one confirmed forward verb (X, TierInline
// — docs/design README.md §13c: "only X stop all gets the inline y/N").
func (m *Model) beginStopAllForwards() {
	if m.forwards == nil || len(m.rows) == 0 {
		return
	}
	m.pendingStopAllForwards = true
}

// updateStopAllForwardsKey routes keys while pendingStopAllForwards' y/N
// line is showing — mirrors updateEditConfirmKey's shape for the same
// reason (not a kube.Mutator/actions.Controller operation).
func (m *Model) updateStopAllForwardsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case verbs.ConfirmAccept.Key:
		m.pendingStopAllForwards = false
		if m.forwards != nil {
			m.forwards.StopAll()
		}
	case verbs.ConfirmDecline.Key, verbs.ConfirmCancel.Key:
		m.pendingStopAllForwards = false
	}
	return m, nil
}

// stopAllForwardsPrompt renders pendingStopAllForwards' keybar RightNote.
func (m Model) stopAllForwardsPrompt() string {
	return fmt.Sprintf("Stop all %d forwards? (y) confirm  (n) cancel", len(m.rows))
}

// forwardSummaryText is 13c's health-strip right side: "4 forwards · 2
// namespaces · forwards end when kute exits".
func (m Model) forwardSummaryText() string {
	return fmt.Sprintf("%d forwards · %d namespaces · forwards end when kute exits", len(m.rows), distinctNamespaces(m.rows))
}
