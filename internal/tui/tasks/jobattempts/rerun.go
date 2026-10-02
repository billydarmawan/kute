// §37c's staged 'R' rerun/replace choice — this screen's own copy of
// browse's job_actions.go (task packages can't import one another). Unlike
// browse's copy, this screen already holds the full
// resources.JobAttemptsSummary from its own load(), so the amber diagnostic
// strip (resources.ClassifyJobFailure) is real here, not omitted.
package jobattempts

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// jobRerunTarget is the state pendingRerun gates on — mirrors browse's own.
type jobRerunTarget struct {
	newName    string
	choice     resources.JobRerunChoice
	diagnostic string
}

// beginRerun stages §37c's preflight for the loaded Job — screen state
// only, no actions.Begin yet, mirroring browse's own beginJobRerun.
func (m *Model) beginRerun() {
	if m.mutator == nil || m.state != tui.TaskStateReady || !m.found {
		return
	}
	// Every Job in the namespace, as browse's copy checks every listed Job —
	// not just this one, or every rerun after the first proposes the
	// -rerun-1 that already exists and the create fails AlreadyExists.
	taken := func(n string) bool { return n == m.name || m.jobNames[n] }
	m.pendingRerun = &jobRerunTarget{
		newName:    kube.NextRerunName(m.name, taken),
		choice:     resources.JobRerunCreate,
		diagnostic: resources.ClassifyJobFailure(m.summary),
	}
}

// updateRerunKey routes keys while pendingRerun's preflight is showing —
// mirrors browse's own updateJobRerunKey.
func (m *Model) updateRerunKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.pendingRerun = nil
	case "up", "down":
		t := m.pendingRerun
		if t.choice == resources.JobRerunCreate {
			t.choice = resources.JobRerunReplace
		} else {
			t.choice = resources.JobRerunCreate
		}
	case "enter":
		return m, m.commitRerun()
	}
	return m, nil
}

// commitRerun executes (create) or hands off to the ordinary Controller
// confirm (replace) — see this screen's own doc comment and browse's
// job_actions.go for why the two diverge.
func (m *Model) commitRerun() tea.Cmd {
	t := m.pendingRerun
	m.pendingRerun = nil
	if t.choice == resources.JobRerunCreate {
		return m.actions.Begin(verbs.JobRetry.Tier, tui.TaskAction{
			ID:    "job-retry-" + m.namespace + "/" + m.name,
			Label: "Rerun " + m.name,
			Scope: tui.TaskScope{
				ResourceKind: string(kube.KindJob), ResourceName: m.name,
				Namespace: m.namespace, Verb: "job-retry", IsMutating: true,
				NewName: t.newName, TriggerCreator: m.rerunCreator(), StagedAt: m.now,
			},
		})
	}
	tier := verbs.TierFor(verbs.JobReplace, m.isProd())
	return m.actions.Begin(tier, tui.TaskAction{
		ID:    "job-replace-" + m.namespace + "/" + m.name,
		Label: fmt.Sprintf("Replace %s?", m.name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindJob), ResourceName: m.name,
			Namespace: m.namespace, Verb: "job-replace", IsMutating: true,
		},
	})
}

// rerunCreator mirrors browse's own jobRerunCreator.
func (m Model) rerunCreator() string {
	if m.currentUser != "" {
		return m.currentUser
	}
	return "kute"
}

// rerunCommand renders the staged choice's own will-run line.
func rerunCommand(t jobRerunTarget, namespace, name string) string {
	if t.choice == resources.JobRerunCreate {
		return kube.JobRetryCommandString(namespace, name, t.newName)
	}
	return kube.JobReplaceCommandString(namespace, name)
}

// jobRetryWillRunLine/jobReplaceWillRunLine are this screen's own copies of
// browse's — used by keys.go's confirm-note dispatch once a job-replace
// action reaches the ordinary Controller confirm.
func jobRetryWillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.JobRetryCommandString(scope.Namespace, scope.ResourceName, scope.NewName)
}

func jobReplaceWillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.JobReplaceCommandString(scope.Namespace, scope.ResourceName)
}

// rerunKeybar is Keybar()'s rendering while pendingRerun is showing —
// mirrors browse's own jobRerunKeybar, plus the diagnostic strip when
// classifiable (§37c: "only appears when the prior failure is one kute can
// classify").
func (m Model) rerunKeybar() tui.Keybar {
	t := *m.pendingRerun
	action := "create"
	if t.choice == resources.JobRerunReplace {
		action = "replace"
	}
	kb := tui.Keybar{
		Pill:     tui.ModeBrowse,
		PillText: "RERUN",
		Groups: [][]tui.KeyHint{{
			{Key: "↑↓", Label: "create/replace"},
			{Key: "↵", Label: action},
			{Key: "esc", Label: "cancel"},
		}},
		RightNote: "will run: " + rerunCommand(t, m.namespace, m.name),
	}
	if t.diagnostic != "" {
		kb.RightWarnNote = t.diagnostic
	}
	return kb
}
