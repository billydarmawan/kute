// §36b/§36c's staged CronJob action preflights, duplicated from
// browse/jobs.go+cronjob_actions.go per the repo's "task packages can't
// import one another" rule (0.8.0 plan §4.4: the neutral pure helpers this
// leans on — resources.ConcurrencyPolicyLabel/MissedRuns/SuspendedDuration/
// NextRunLabel — live in internal/resources/cronjob_actions.go specifically
// so both browse and this screen can share them). Single-target only: this
// screen has no marked-set bulk concept, so the bulk branches browse's own
// beginCronJobResumeBulk/beginCronJobSuspendBulk carry are simply absent
// here.
package cronjobdetail

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// ---------------------------------------------------------------------
// §36b run-now
// ---------------------------------------------------------------------

// cronJobRunTarget is the state pendingRun gates on while §36b's ctrl-r
// preflight is showing — mirrors browse's own cronJobRunTarget.
type cronJobRunTarget struct {
	namespace, name string
	newName         string
	concurrency     string
	activeRuns      []resources.JobSummary
	unknownOverlap  bool
}

// beginRunNow stages §36b's preflight — screen state only, no actions.Begin
// yet. Returns false (no-op) when nothing applies.
func (m *Model) beginRunNow() bool {
	if m.mutator == nil || m.state != tui.TaskStateReady || !m.found || m.summary.Object == nil {
		return false
	}
	taken := make(map[string]bool, len(m.summary.Runs))
	for _, r := range m.summary.Runs {
		taken[r.Name] = true
	}
	m.pendingRun = &cronJobRunTarget{
		namespace:      m.namespace,
		name:           m.name,
		newName:        kube.ManualJobName(m.name, m.now, func(n string) bool { return taken[n] }),
		concurrency:    resources.ConcurrencyPolicyLabel(m.summary.Object.Spec),
		activeRuns:     m.summary.ActiveRuns,
		unknownOverlap: m.jobsErr != nil,
	}
	return true
}

// updateRunKey routes keys while pendingRun's preflight is showing: esc
// cancels, y copies the will-run command, enter commits.
func (m *Model) updateRunKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.pendingRun = nil
	case "enter":
		return m, m.commitRunNow()
	case verbs.CronJobCopyCommand.Key:
		return m, tea.SetClipboard(cronJobRunCommand(*m.pendingRun))
	}
	return m, nil
}

// commitRunNow executes the staged run-now through actions.Controller at
// TierNone — mirrors browse's own commitCronJobRun.
func (m *Model) commitRunNow() tea.Cmd {
	t := m.pendingRun
	m.pendingRun = nil
	return m.actions.Begin(actions.Request{Verb: verbs.CronJobRunNow, Prod: m.isProd()}, tui.TaskAction{
		ID:    "cronjob-run-now-" + t.namespace + "/" + t.name,
		Label: fmt.Sprintf("Run %s now", t.name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: t.name,
			Namespace: t.namespace, Verb: "cronjob-run-now", IsMutating: true,
			NewName:        t.newName,
			TriggerCreator: m.cronJobTriggerCreator(),
			StagedAt:       m.now,
		},
	})
}

// cronJobRunCommand mirrors browse's own cronJobRunCommand.
func cronJobRunCommand(t cronJobRunTarget) string {
	return kube.CronJobTriggerCommandString(t.namespace, t.name, t.newName)
}

// cronJobOverlapNote mirrors browse's own cronJobOverlapNote (§36b).
func cronJobOverlapNote(t cronJobRunTarget) (note string, warn bool) {
	if t.unknownOverlap {
		return "overlap unknown — job history could not be listed", true
	}
	if len(t.activeRuns) == 0 {
		return "", false
	}
	names := make([]string, len(t.activeRuns))
	for i, r := range t.activeRuns {
		names[i] = r.Name
	}
	plural := ""
	if len(t.activeRuns) != 1 {
		plural = "s"
	}
	joined := strings.Join(names, ", ")
	switch t.concurrency {
	case "Forbid", "Replace":
		return fmt.Sprintf("%d job%s already active (%s) · manual run bypasses concurrencyPolicy: %s · next scheduled run unaffected",
			len(t.activeRuns), plural, joined, t.concurrency), true
	default: // Allow
		return fmt.Sprintf("%d job%s already active (%s) · concurrencyPolicy: Allow, no conflict",
			len(t.activeRuns), plural, joined), false
	}
}

// runKeybar mirrors browse's own cronJobRunKeybar.
func (m Model) runKeybar() tui.Keybar {
	t := *m.pendingRun
	kb := tui.Keybar{
		Pill: tui.ModeBrowse, PillText: "RUN NOW",
		Groups: [][]tui.KeyHint{{
			verbs.Open.HintAs("run"),
			verbs.CronJobCopyCommand.Hint(),
			verbs.Back.HintAs("cancel"),
		}},
		RightNote: "will run: " + cronJobRunCommand(t),
	}
	if note, warn := cronJobOverlapNote(t); note != "" {
		if warn {
			kb.RightWarnNote = note
		} else {
			kb.RightNote += "   " + note
		}
	}
	return kb
}

// ---------------------------------------------------------------------
// §36c resume
// ---------------------------------------------------------------------

// cronJobResumeEntry mirrors browse's own cronJobResumeEntry.
type cronJobResumeEntry struct {
	namespace, name, resourceVersion string
	generation                       int64
	suspendedLabel                   string
	suspendedKnown                   bool
	missedCount                      int
	missedTrunc                      bool
	missedEligible                   bool
	missedReason                     string
	nextRun                          string
}

// cronJobResumeTarget mirrors browse's own cronJobResumeTarget, minus the
// marked-set slice (single entry only — this screen has no bulk concept).
type cronJobResumeTarget struct {
	entry cronJobResumeEntry
}

// resumeEntry computes this CronJob's own resume preview from m.summary —
// mirrors browse's own cronJobResumeEntryFor.
func (m Model) resumeEntry() (cronJobResumeEntry, bool) {
	if !m.found || m.summary.Object == nil {
		return cronJobResumeEntry{}, false
	}
	spec := m.summary.Object.Spec
	suspendedLabel, suspendedKnown := resources.SuspendedDuration(m.summary.SuspendedAt, m.now)

	var missedCount int
	var missedTrunc, missedEligible bool
	var missedReason string
	if m.summary.SuspendedAt != nil {
		missedCount, missedTrunc, missedEligible, missedReason = resources.MissedRuns(spec, *m.summary.SuspendedAt, m.now)
	} else {
		missedReason = "suspension start unknown"
	}

	return cronJobResumeEntry{
		namespace: m.namespace, name: m.name,
		resourceVersion: m.summary.Object.ResourceVersion, generation: m.summary.Object.Generation,
		suspendedLabel: suspendedLabel, suspendedKnown: suspendedKnown,
		missedCount: missedCount, missedTrunc: missedTrunc, missedEligible: missedEligible, missedReason: missedReason,
		nextRun: resources.NextRunLabel(spec, m.now),
	}, true
}

// beginResume stages §36c's resume preflight — mirrors browse's own
// beginCronJobResume.
func (m *Model) beginResume() bool {
	if m.mutator == nil || m.state != tui.TaskStateReady {
		return false
	}
	entry, ok := m.resumeEntry()
	if !ok {
		return false
	}
	m.pendingResume = &cronJobResumeTarget{entry: entry}
	return true
}

// updateResumeKey routes keys while pendingResume's preflight is showing:
// esc cancels, enter commits.
func (m *Model) updateResumeKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.pendingResume = nil
	case "enter":
		return m, m.commitResume()
	}
	return m, nil
}

// commitResume executes the staged resume through actions.Controller at
// TierNone — mirrors browse's own commitCronJobResume single-target branch.
func (m *Model) commitResume() tea.Cmd {
	e := m.pendingResume.entry
	m.pendingResume = nil
	return m.actions.Begin(actions.Request{Verb: verbs.CronJobSuspend, Prod: m.isProd()}, tui.TaskAction{
		ID:    "cronjob-resume-" + e.namespace + "/" + e.name,
		Label: "Resume " + e.name,
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: e.name,
			Namespace: e.namespace, Verb: "cronjob-resume", IsMutating: true,
			CronJobResourceVersion: e.resourceVersion,
			CronJobGeneration:      e.generation,
			StagedAt:               m.now,
		},
	})
}

// missedRunsCopy mirrors browse's own missedRunsCopy (§3.3).
func missedRunsCopy(count int, truncated, eligible bool, reason string) string {
	if reason != "" {
		return reason
	}
	if count == 0 {
		return "no schedules missed while suspended"
	}
	label := strconv.Itoa(count)
	if truncated {
		label = "100+"
	}
	plural := ""
	if count != 1 {
		plural = "s"
	}
	if eligible {
		return fmt.Sprintf("%s schedule%s missed while suspended · Kubernetes may start one immediately on resume", label, plural)
	}
	return fmt.Sprintf("%s schedule%s missed while suspended · none eligible to backfill (starting deadline elapsed)", label, plural)
}

// resumeSummaryLine mirrors browse's own cronJobResumeSummaryLine.
func resumeSummaryLine(e cronJobResumeEntry) string {
	suspended := "suspended · start unknown"
	if e.suspendedKnown {
		suspended = "suspended " + e.suspendedLabel
	}
	return fmt.Sprintf("%s · %s · next %s", suspended,
		missedRunsCopy(e.missedCount, e.missedTrunc, e.missedEligible, e.missedReason), e.nextRun)
}

// resumeKeybar mirrors browse's own cronJobResumeKeybar single-target
// branch.
func (m Model) resumeKeybar() tui.Keybar {
	e := m.pendingResume.entry
	return tui.Keybar{
		Pill: tui.ModeBrowse, PillText: "RESUME",
		Groups: [][]tui.KeyHint{{
			verbs.Open.HintAs("resume"),
			verbs.Back.HintAs("cancel"),
		}},
		RightNote: fmt.Sprintf("resume %s? %s", e.name, resumeSummaryLine(e)),
	}
}

// ---------------------------------------------------------------------
// 's' direction dispatch and suspend (the dangerous, Controller-driven half)
// ---------------------------------------------------------------------

// beginSuspendOrResume is 's' (update.go's dispatch): chooses direction from
// the loaded CronJob's own suspend state — mirrors browse's own
// beginCronJobSuspendOrResume, minus the marked-set branch.
func (m *Model) beginSuspendOrResume() tea.Cmd {
	if !m.found || m.summary.Object == nil {
		return nil
	}
	if m.summary.Object.Spec.Suspend != nil && *m.summary.Object.Spec.Suspend {
		m.beginResume()
		return nil
	}
	return m.beginSuspend()
}

// beginSuspend starts verbs.CronJobSuspend's suspend direction — mirrors
// browse's own beginCronJobSuspend, routed through the ordinary
// actions.Controller TierInline/TierModal confirm.
func (m *Model) beginSuspend() tea.Cmd {
	var resourceVersion string
	var generation int64
	if m.summary.Object != nil {
		resourceVersion = m.summary.Object.ResourceVersion
		generation = m.summary.Object.Generation
	}
	return m.actions.Begin(actions.Request{Verb: verbs.CronJobSuspend, Prod: m.isProd()}, tui.TaskAction{
		ID:    "cronjob-suspend-" + m.namespace + "/" + m.name,
		Label: fmt.Sprintf("Suspend %s?", m.name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: m.name,
			Namespace: m.namespace, Verb: "cronjob-suspend", IsMutating: true,
			CronJobResourceVersion: resourceVersion,
			CronJobGeneration:      generation,
			StagedAt:               m.now,
		},
	})
}

// cronJobSuspendWillRunLine mirrors browse's own cronJobSuspendWillRunLine.
func cronJobSuspendWillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.CronJobSuspendCommandString(scope.Namespace, scope.ResourceName, scope.Verb == "cronjob-suspend")
}

// cronJobSuspendDangerNote mirrors browse's own cronJobSuspendDangerNote
// (§36c: "the card names any currently-active Jobs and states plainly that
// they are unaffected").
func cronJobSuspendDangerNote(summary resources.CronJobSummary, now time.Time) string {
	if summary.Object == nil {
		return ""
	}
	next := resources.NextRunLabel(summary.Object.Spec, now)
	consequence := "future scheduled runs stop"
	if next != "—" && next != "controller local" {
		consequence = fmt.Sprintf("the next scheduled run (%s) stops happening", next)
	}
	if len(summary.ActiveRuns) == 0 {
		return "no active jobs right now · " + consequence
	}
	names := make([]string, len(summary.ActiveRuns))
	for i, r := range summary.ActiveRuns {
		names[i] = r.Name
	}
	return "running jobs unaffected · " + strings.Join(names, ", ") + " · " + consequence
}

// cronJobKeybarGroup mirrors browse's own cronJobKeybarGroup: copies the
// registry Verb, flips CronJobSuspend's label to "resume" when the loaded
// CronJob is currently suspended.
func (m Model) cronJobKeybarGroup() []tui.KeyHint {
	suspend := verbs.CronJobSuspend
	if m.summary.Object != nil && m.summary.Object.Spec.Suspend != nil && *m.summary.Object.Spec.Suspend {
		suspend.Label = "resume"
	}
	hints := []tui.KeyHint{
		{Key: verbs.CronJobRunNow.Key, Label: verbs.CronJobRunNow.Label},
		{Key: suspend.Key, Label: suspend.Label},
	}
	if m.openSchedule != nil {
		hints = append(hints, tui.KeyHint{Key: verbs.CronJobSetSchedule.Key, Label: verbs.CronJobSetSchedule.Label})
	}
	return hints
}
