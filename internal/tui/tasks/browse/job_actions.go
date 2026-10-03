// §37c's staged 'R' rerun/replace preflight for browse's own Jobs list —
// mirrors cronjob_actions.go's run-now shape (0.8.0 plan §36b): the choice
// (create vs replace) and the generated rerun name are gathered into screen
// state *before* ever calling actions.Begin. Unlike run-now, staging alone
// only ever confirms the "create" branch (TierNone, staging-is-the-
// confirmation) — "replace" hands off to actions.Controller's ordinary
// tiered confirm instead, since it's genuinely destructive (see verbs.
// JobReplace's own doc comment). The pushed jobattempts screen (§37b/§37d,
// Layer 5) reaches the identical choice through its own copy of this shape —
// task packages can't import one another, the same reason cronjob_actions.go
// exists separately from cronjobdetail's own copy.
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
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// jobRerunTarget is the state pendingJobRerun gates on while §37c's staged
// choice is showing.
type jobRerunTarget struct {
	namespace, name string
	newName         string
	choice          resources.JobRerunChoice
}

// beginJobRerun stages §37c's preflight for the selected Job row — screen
// state only, no actions.Begin yet, mirroring beginCronJobRunNow's own
// "returns false (no-op) when nothing applies" contract.
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
	m.pendingJobRerun = &jobRerunTarget{
		namespace: row.Namespace,
		name:      row.Name,
		newName:   kube.NextRerunName(row.Name, func(n string) bool { return taken[n] }),
		choice:    resources.JobRerunCreate,
	}
	return true
}

// updateJobRerunKey routes keys while pendingJobRerun's preflight is
// showing: esc cancels, ↑↓ toggles create/replace, enter commits.
func (m *Model) updateJobRerunKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.pendingJobRerun = nil
	case "up", "down":
		t := m.pendingJobRerun
		if t.choice == resources.JobRerunCreate {
			t.choice = resources.JobRerunReplace
		} else {
			t.choice = resources.JobRerunCreate
		}
	case "enter":
		return m, m.commitJobRerun()
	}
	return m, nil
}

// commitJobRerun executes (create) or hands off to the ordinary Controller
// confirm (replace) — see this file's own doc comment for why the two
// diverge here rather than both executing at TierNone.
func (m *Model) commitJobRerun() tea.Cmd {
	t := m.pendingJobRerun
	m.pendingJobRerun = nil
	if t.choice == resources.JobRerunCreate {
		return m.actions.Begin(actions.Request{Verb: verbs.JobRetry, Prod: m.isProd()}, tui.TaskAction{
			ID:    "job-retry-" + t.namespace + "/" + t.name,
			Label: "Rerun " + t.name,
			Scope: tui.TaskScope{
				ResourceKind: string(kube.KindJob), ResourceName: t.name,
				Namespace: t.namespace, Verb: "job-retry", IsMutating: true,
				NewName: t.newName, TriggerCreator: m.jobRerunCreator(), StagedAt: m.now,
			},
		})
	}
	return m.actions.Begin(actions.Request{Verb: verbs.JobReplace, Prod: m.isProd()}, tui.TaskAction{
		ID:    "job-replace-" + t.namespace + "/" + t.name,
		Label: fmt.Sprintf("Replace %s?", t.name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindJob), ResourceName: t.name,
			Namespace: t.namespace, Verb: "job-replace", IsMutating: true,
		},
	})
}

// jobRerunCreator mirrors cronJobTriggerCreator's own fallback.
func (m Model) jobRerunCreator() string {
	if m.currentUser != "" {
		return m.currentUser
	}
	return "kute"
}

// jobRerunCommand renders the staged choice's own will-run line.
func jobRerunCommand(t jobRerunTarget) string {
	if t.choice == resources.JobRerunCreate {
		return kube.JobRetryCommandString(t.namespace, t.name, t.newName)
	}
	return kube.JobReplaceCommandString(t.namespace, t.name)
}

// jobRetryWillRunLine is job-retry's confirm "will run: ..." line — same
// idiom as jobSuspendWillRunLine/rolloutRestartWillRunLine. Only reached via
// keys.go's actions.Active() branch, which a job-retry action passes
// through immediately (TierNone) rather than lingering on — kept for the
// same defensive completeness every other verb's case has.
func jobRetryWillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.JobRetryCommandString(scope.Namespace, scope.ResourceName, scope.NewName)
}

// jobReplaceWillRunLine is job-replace's confirm "will run: ..." line —
// same idiom as jobRetryWillRunLine/jobSuspendWillRunLine.
func jobReplaceWillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.JobReplaceCommandString(scope.Namespace, scope.ResourceName)
}

// jobRerunKeybar is Keybar()'s rendering while pendingJobRerun is showing
// (§37c mockup: "↑↓ to choose · ↵ create/replace · esc cancel").
func (m Model) jobRerunKeybar() tui.Keybar {
	t := *m.pendingJobRerun
	action := "create"
	if t.choice == resources.JobRerunReplace {
		action = "replace"
	}
	return tui.Keybar{
		Pill:     tui.ModeBrowse,
		PillText: "RERUN",
		Groups: [][]tui.KeyHint{{
			{Key: "↑↓", Label: "create/replace"},
			{Key: "↵", Label: action},
			{Key: "esc", Label: "cancel"},
		}},
		RightNote: "will run: " + jobRerunCommand(t),
	}
}
