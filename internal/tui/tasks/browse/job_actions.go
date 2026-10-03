// §37c's staged 'R' rerun/replace preflight for browse's own Jobs list —
// mirrors cronjob_actions.go's run-now shape (0.8.0 plan §36b): the choice
// (create vs replace) and the generated rerun name are gathered into screen
// state *before* ever calling actions.Begin. Unlike run-now, staging alone
// only ever confirms the "create" branch (TierNone, staging-is-the-
// confirmation) — "replace" hands off to actions.Controller's ordinary
// tiered confirm instead, since it's genuinely destructive (see verbs.
// JobReplace's own doc comment). The pushed jobattempts screen (§37b/§37d,
// Layer 5) reaches the identical choice: the staging state, keys, will-run
// text and request all live in internal/tui/jobactions, so the two entry
// points can't drift; only where it renders is this screen's own.
//
// Scope simplification (documented, not an oversight): §37c's amber
// diagnostic strip (resources.ClassifyJobFailure) needs a full
// resources.JobAttemptsSummary — BackoffLimit, ActiveDeadlineSeconds,
// per-attempt exit codes — which requires the raw Pod objects browse's own
// Jobs list never retains (only the curated kube.Pod map, for the 'l' key).
// Browse's own 'R' preflight below never shows the diagnostic strip; the
// pushed jobattempts screen, which already holds the full aggregation from
// its own load(), does.
package browse

import (
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/jobactions"
)

// beginJobRerun stages §37c's preflight for the selected Job row — screen
// state only, no actions.Begin yet, mirroring beginCronJobRunNow's own
// "returns false (no-op) when nothing applies" contract. No diagnostic: see
// this file's own doc comment.
func (m *Model) beginJobRerun(row resources.Row) bool {
	if m.kind != kube.KindJob || m.mutator == nil || m.state != tui.TaskStateReady {
		return false
	}
	taken := make(map[string]bool, len(m.jobListSummaries))
	for _, s := range m.jobListSummaries {
		if s.Object != nil {
			taken[s.Object.Name] = true
		}
	}
	m.pendingJobRerun = jobactions.StageRerun(row.Namespace, row.Name, func(n string) bool { return taken[n] }, "")
	return true
}

// updateJobRerunKey routes keys while pendingJobRerun's preflight is
// showing (jobactions.Rerun.Key: esc cancels, ↑↓ toggles, enter commits).
func (m *Model) updateJobRerunKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.pendingJobRerun.Key(msg.String()) {
	case jobactions.Cancel:
		m.pendingJobRerun = nil
	case jobactions.Commit:
		return m, m.commitJobRerun()
	}
	return m, nil
}

// commitJobRerun executes (create) or hands off to the ordinary Controller
// confirm (replace) — see jobactions.Rerun for why the two diverge.
func (m *Model) commitJobRerun() tea.Cmd {
	t := m.pendingJobRerun
	m.pendingJobRerun = nil
	return m.actions.Begin(t.Action(m.isProd(), m.jobRerunCreator(), m.now))
}

// jobRerunCreator mirrors cronJobTriggerCreator's own fallback.
func (m Model) jobRerunCreator() string {
	return jobactions.Creator(m.identity, m.currentUser)
}
