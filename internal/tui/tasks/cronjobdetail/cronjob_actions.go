// §36b/§36c's staged CronJob action preflights. The staging state, keys,
// will-run text and requests live in internal/tui/jobactions, shared with
// browse's CronJobs list (36e: "every mutating verb from the list works
// here unchanged"). Single-target only: this screen has no marked-set bulk
// concept, so it always stages a one-entry resume and never a bulk
// suspend.
package cronjobdetail

import (
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/jobactions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// ---------------------------------------------------------------------
// §36b run-now
// ---------------------------------------------------------------------

// beginRunNow stages §36b's preflight — screen state only, no actions.Begin
// yet. Returns false (no-op) when nothing applies.
func (m *Model) beginRunNow() bool {
	if m.mutator == nil || m.state != tui.TaskStateReady || !m.found {
		return false
	}
	t, ok := jobactions.StageRunNow(m.namespace, m.name, m.summary, m.jobsErr, m.now)
	if !ok {
		return false
	}
	m.pendingRun = t
	return true
}

// updateRunKey routes keys while pendingRun's preflight is showing
// (jobactions.RunNow.Key: esc cancels, y copies the will-run command,
// enter commits).
func (m *Model) updateRunKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	out, cmd := m.pendingRun.Key(msg.String())
	switch out {
	case jobactions.Cancel:
		m.pendingRun = nil
	case jobactions.Commit:
		return m, m.commitRunNow()
	}
	return m, cmd
}

// commitRunNow executes the staged run-now through actions.Controller at
// TierNone.
func (m *Model) commitRunNow() tea.Cmd {
	t := m.pendingRun
	m.pendingRun = nil
	m.lastRunName = t.NewName
	return m.actions.Begin(t.Action(m.isProd(), m.cronJobTriggerCreator(), m.now))
}

// ---------------------------------------------------------------------
// §36c resume
// ---------------------------------------------------------------------

// beginResume stages §36c's resume preflight for the loaded CronJob.
func (m *Model) beginResume() bool {
	if m.mutator == nil || m.state != tui.TaskStateReady || !m.found {
		return false
	}
	entry, ok := jobactions.ResumeEntryFor(m.namespace, m.name, m.summary, m.now)
	if !ok {
		return false
	}
	m.pendingResume = &jobactions.Resume{Entries: []jobactions.ResumeEntry{entry}}
	return true
}

// updateResumeKey routes keys while pendingResume's preflight is showing:
// esc cancels, enter commits.
func (m *Model) updateResumeKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.pendingResume.Key(msg.String()) {
	case jobactions.Cancel:
		m.pendingResume = nil
	case jobactions.Commit:
		return m, m.commitResume()
	}
	return m, nil
}

// commitResume executes the staged resume through actions.Controller at
// TierNone.
func (m *Model) commitResume() tea.Cmd {
	t := m.pendingResume
	m.pendingResume = nil
	return m.actions.Begin(t.Action(m.isProd(), m.now))
}

// ---------------------------------------------------------------------
// 's' direction dispatch and suspend (the dangerous, Controller-driven half)
// ---------------------------------------------------------------------

// suspended reports whether the loaded CronJob is currently suspended.
func (m Model) suspended() bool {
	return m.summary.Object != nil && m.summary.Object.Spec.Suspend != nil && *m.summary.Object.Spec.Suspend
}

// beginSuspendOrResume is 's' (update.go's dispatch): chooses direction from
// the loaded CronJob's own suspend state — browse's own
// beginCronJobSuspendOrResume, minus the marked-set branch.
func (m *Model) beginSuspendOrResume() tea.Cmd {
	if !m.found || m.summary.Object == nil {
		return nil
	}
	if m.suspended() {
		m.beginResume()
		return nil
	}
	return m.beginSuspend()
}

// beginSuspend starts verbs.CronJobSuspend's suspend direction, routed
// through the ordinary actions.Controller TierInline/TierModal confirm.
func (m *Model) beginSuspend() tea.Cmd {
	return m.actions.Begin(jobactions.SuspendAction(m.namespace, m.name, m.summary, m.isProd(), m.now))
}

// cronJobKeybarGroup mirrors browse's own cronJobKeybarGroup: 's' is
// labelled "resume" when the loaded CronJob is currently suspended.
func (m Model) cronJobKeybarGroup() []tui.KeyHint {
	hints := []tui.KeyHint{
		{Key: verbs.CronJobRunNow.Key, Label: verbs.CronJobRunNow.Label},
		jobactions.SuspendHint(m.suspended()),
	}
	if m.openSchedule != nil {
		hints = append(hints, tui.KeyHint{Key: verbs.CronJobSetSchedule.Key, Label: verbs.CronJobSetSchedule.Label})
	}
	return hints
}
