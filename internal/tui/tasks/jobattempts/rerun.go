// §37c's staged 'R' create-vs-replace choice — the staging state, keys,
// will-run text and request live in internal/tui/jobactions, shared with
// browse's Jobs list. Unlike browse, this screen already holds the full
// resources.JobAttemptsSummary from its own load(), so the amber diagnostic
// strip (resources.ClassifyJobFailure) is real here, not omitted.
package jobattempts

import (
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/jobactions"
)

// beginRerun stages §37c's preflight for the loaded Job — screen state
// only, no actions.Begin yet.
func (m *Model) beginRerun() {
	if m.mutator == nil || m.state != tui.TaskStateReady || !m.found {
		return
	}
	// Every Job in the namespace, as browse checks every listed Job — not
	// just this one, or every rerun after the first proposes the -rerun-1
	// that already exists and the create fails AlreadyExists.
	taken := func(n string) bool { return n == m.name || m.jobNames[n] }
	m.pendingRerun = jobactions.StageRerun(m.namespace, m.name, taken, resources.ClassifyJobFailure(m.summary))
}

// updateRerunKey routes keys while pendingRerun's preflight is showing
// (jobactions.Rerun.Key: esc cancels, ↑↓ toggles, enter commits).
func (m *Model) updateRerunKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.pendingRerun.Key(msg.String()) {
	case jobactions.Cancel:
		m.pendingRerun = nil
	case jobactions.Commit:
		return m, m.commitRerun()
	}
	return m, nil
}

// commitRerun executes (create) or hands off to the ordinary Controller
// confirm (replace) — see jobactions.Rerun for why the two diverge.
func (m *Model) commitRerun() tea.Cmd {
	t := m.pendingRerun
	m.pendingRerun = nil
	return m.actions.Begin(t.Action(m.isProd(), m.rerunCreator(), m.now))
}

// IdentityReader is the connect-time SelfSubjectReview seam — satisfied by
// *kube.Cluster and *fake.Cluster's ReviewedIdentity.
type IdentityReader interface {
	ReviewedIdentity() (kube.ReviewedIdentity, bool)
}

// rerunCreator is the reviewed username, read now, else the kubeconfig
// name, else "kute" — the same answer browse stamps.
func (m Model) rerunCreator() string {
	return jobactions.Creator(m.identity, m.currentUser)
}
