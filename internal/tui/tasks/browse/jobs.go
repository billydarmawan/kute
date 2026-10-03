// Job- and CronJob-specific browse machinery: the ↵ "open this workload's
// own pods" recipe deployments.go established for Deployment/StatefulSet/
// DaemonSet (and helm.go for HelmRelease), extended here to a different
// resource family — run-to-completion workloads, with neither a
// rollout-restart verb nor any other machinery to share with
// deployments.go, hence their own file per browse's per-concern split
// convention (nodes.go/sort.go/grouping.go/delete.go/helm.go). Also holds
// Job's own two mutating verbs ('R' rerun, 's' suspend/resume) and
// CronJob's own ctrl-r/'s' siblings (run now, suspend/resume — one level up:
// a CronJob's ctrl-r triggers a Job rather than cloning itself) — kept here
// alongside the navigation above rather than split out, matching
// deployments.go's own precedent of keeping a kind's navigation and its
// mutating verb(s) in one file. CronJob's third verb, 'S' edit-schedule, is
// sizable enough (its own typed-buffer panel, validation, keep-open result
// handling) to live in its own file, cronjobschedule.go.
package browse

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// openSelectedJobAttempts pushes §37b/§37d (tasks/jobattempts) for the
// selected Job row — §37a's replacement for the pre-v0.9.0 openJobPods
// jump-straight-to-Pods shortcut ("↵ opens the attempt ledger, not a
// describe page"). siblings/index mirror openSelectedCronJobDetail's own
// shape exactly. ok is false — and `enter` a no-op — until app.go wires
// OpenJobAttempts.
func (m Model) openSelectedJobAttempts() (tea.Model, tea.Cmd, bool) {
	if m.openJobAttempts == nil || m.kind != kube.KindJob {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	siblings := make([]JobSiblingRef, len(m.visible))
	index := 0
	for i, fm := range m.visible {
		siblings[i] = JobSiblingRef{Namespace: fm.row.Namespace, Name: fm.row.Name}
		if fm.row.Namespace == row.Namespace && fm.row.Name == row.Name {
			index = i
		}
	}
	task, cmd := m.openJobAttempts(row.Namespace, row.Name, siblings, index, m.width, m.height)
	return task, cmd, task != nil
}

// jobListSummaryFor resolves any row's full aggregated
// resources.JobListSummary by namespace/name against m.jobListSummaries —
// mirrors cronJobSummaryFor's own lookup/reasoning.
func (m Model) jobListSummaryFor(namespace, name string) (resources.JobListSummary, bool) {
	for i := range m.jobListSummaries {
		obj := m.jobListSummaries[i].Object
		if obj != nil && obj.Namespace == namespace && obj.Name == name {
			return m.jobListSummaries[i], true
		}
	}
	return resources.JobListSummary{}, false
}

// jobLogsTarget resolves §37a's 'l' (update.go's own case, mirrors
// cronJobLogsTarget's shape): the newest pod this Job has created,
// regardless of state (JobListSummary.NewestPodName, resolved once at load
// time). reason names why no pod is available when ok is false.
func (m Model) jobLogsTarget(row resources.Row) (pod kube.Pod, reason string, ok bool) {
	summary, sumOK := m.jobListSummaryFor(row.Namespace, row.Name)
	if !sumOK || summary.NewestPodName == "" {
		return kube.Pod{}, row.Name + ": no pod collected for this job yet", false
	}
	pod, ok = m.pods[kube.PodKey(row.Namespace, summary.NewestPodName)]
	if !ok {
		pod = kube.Pod{Namespace: row.Namespace, Name: summary.NewestPodName}
	}
	return pod, "", true
}

// openSelectedCronJobDetail pushes §36e (tasks/cronjobdetail, wired once
// Phase 7/8 land) for the selected CronJob row — 0.8.0 plan Phase 4 task
// 14's replacement for the old openCronJobPods jump-straight-to-Pods
// shortcut. siblings are every currently visible row's namespace-qualified
// ref, in order, the same "current list + this row's index" shape
// openSelectedPodDetail/openSelectedObjectDetail already use for their own
// `[`/`]` sibling movement — except namespace-qualified, since all-
// namespaces mode can list two different CronJobs sharing a name (Phase 4
// test 11). ok is false — and `enter` a no-op — until app.go wires
// OpenCronJobDetail; nothing regresses, there is simply nowhere to push
// yet.
func (m Model) openSelectedCronJobDetail() (tea.Model, tea.Cmd, bool) {
	if m.openCronJobDetail == nil || m.kind != kube.KindCronJob {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	siblings := make([]CronJobSiblingRef, len(m.visible))
	index := 0
	for i, fm := range m.visible {
		siblings[i] = CronJobSiblingRef{Namespace: fm.row.Namespace, Name: fm.row.Name}
		if fm.row.Namespace == row.Namespace && fm.row.Name == row.Name {
			index = i
		}
	}
	task, cmd := m.openCronJobDetail(row.Namespace, row.Name, siblings, index, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedCronJobSchedule pushes §36d (tasks/cronjobschedule) for the
// selected CronJob row's own schedule editor — Phase 6's replacement for
// the pre-Phase-6 inline beginCronJobSetSchedule buffer (the deleted
// cronjobschedule.go). Same "ok is false until app.go wires the opener"
// contract openSelectedCronJobDetail's own doc comment describes.
func (m Model) openSelectedCronJobSchedule() (tea.Model, tea.Cmd, bool) {
	if m.openCronJobSchedule == nil || m.kind != kube.KindCronJob {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	task, cmd := m.openCronJobSchedule(row.Namespace, row.Name, m.width, m.height)
	return task, cmd, task != nil
}

// selectedCronJobSummary returns the full aggregated resources.CronJobSummary
// behind the selected row — view.go's behavior strip and cronJobLogsTarget
// below both need spec/Job-history fields Row's own display Cells don't
// carry.
func (m Model) selectedCronJobSummary() (resources.CronJobSummary, bool) {
	row, ok := m.selectedRow()
	if !ok {
		return resources.CronJobSummary{}, false
	}
	return m.cronJobSummaryFor(row.Namespace, row.Name)
}

// cronJobSummaryFor resolves any row's full aggregated
// resources.CronJobSummary by namespace/name against m.cronJobSummaries —
// selectedCronJobSummary's own lookup, generalized for cronjob_actions.go's
// preflights, which (for a marked-set bulk action) need every marked row's
// summary, not just the selected one. Resolved by namespace/name rather than
// assumed position-aligned with m.visible/m.display: sort/filter never
// reorder m.rows for this kind (sort.go), but a defensive lookup costs
// nothing at this list's realistic size and stays correct regardless.
func (m Model) cronJobSummaryFor(namespace, name string) (resources.CronJobSummary, bool) {
	for i := range m.cronJobSummaries {
		obj := m.cronJobSummaries[i].Object
		if obj != nil && obj.Namespace == namespace && obj.Name == name {
			return m.cronJobSummaries[i], true
		}
	}
	return resources.CronJobSummary{}, false
}

// cronJobLogsTarget resolves §36a's 'l' (update.go's own case, since it
// needs a pointer receiver to set m.execFeedback on the "unavailable"
// path): the newest useful Pod of the selected CronJob's most recent
// associated Job, active or terminal, succeeded or failed — Runs is
// newest-first by runTimestamp and already includes every run regardless of
// state (resources.CronJobSummary's own doc comment), so index 0 is exactly
// "the latest job run" with no success/failure filtering (verbs.Logs' doc
// comment). reason names why no pod is available when ok is false; both
// empty means nothing is selected at all, a silent no-op like every other
// row-scoped verb.
func (m Model) cronJobLogsTarget() (pod kube.Pod, reason string, ok bool) {
	row, rowOK := m.selectedRow()
	if !rowOK {
		return kube.Pod{}, "", false
	}
	summary, sumOK := m.selectedCronJobSummary()
	if !sumOK {
		return kube.Pod{}, "", false
	}
	if len(summary.Runs) == 0 {
		return kube.Pod{}, row.Name + " has no runs to show logs for", false
	}
	run := summary.Runs[0]
	if run.PodName == "" {
		return kube.Pod{}, row.Name + ": no pod collected for this run yet", false
	}
	pod, ok = m.pods[kube.PodKey(row.Namespace, run.PodName)]
	if !ok {
		pod = kube.Pod{Namespace: row.Namespace, Name: run.PodName}
	}
	return pod, "", true
}

// beginJobSuspend starts verbs.JobSuspend ('s'): one verb, two directions,
// exactly beginFluxSuspend's (flux.go) shape — Scope.Verb flips between
// "job-suspend"/"job-resume" based on row.Suspended. v0.9.0 §37a routes this
// through verbs.Resolve's JobSuspend row, not the flat verbs.Resolve: resume is
// always TierNone (reversible, immediate — mirrors CronJob's own resume),
// suspend is TierInline outside PROD, TierModal (the type-the-name modal,
// verbs.Resolve's typed-name policy for "job-suspend") inside one — tearing
// down a Job's active pods immediately is a real destructive side effect
// Retry's clone-only semantics don't have.
func (m *Model) beginJobSuspend(row resources.Row) tea.Cmd {
	verb, label := "job-suspend", fmt.Sprintf("Suspend %s?", row.Name)
	suspending := !row.Suspended
	if !suspending {
		verb, label = "job-resume", fmt.Sprintf("Resume %s?", row.Name)
	}
	return m.actions.Begin(actions.Request{Verb: verbs.JobSuspend, Prod: m.isProd()}, tui.TaskAction{
		ID:    verb + "-" + row.Name,
		Label: label,
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindJob), ResourceName: row.Name,
			Namespace: row.Namespace, Verb: verb, IsMutating: true,
		},
	})
}

// jobSuspendWillRunLine is beginJobSuspend's confirm "will run: ..." line.
func jobSuspendWillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.JobSuspendCommandString(scope.Namespace, scope.ResourceName, scope.Verb == "job-suspend")
}

// jobKeybarGroup mirrors fluxKeybarGroup (flux.go): copies the registry
// Verb, flips its Label to "resume" when the selected row is already
// suspended, so the keybar always shows the direction pressing 's' would
// actually take. 'R' has no dedicated Hint() rendering here beyond the
// bare key/label — job_actions.go's staged choice list takes over the
// keybar entirely once pressed (job_actions.go's jobRerunKeybar).
func (m Model) jobKeybarGroup() []tui.KeyHint {
	suspend := verbs.JobSuspend
	if row, ok := m.selectedRow(); ok && row.Suspended {
		suspend.Label = "resume"
	}
	hints := []tui.KeyHint{verbs.JobRetry.Hint()}
	if m.openLogs != nil {
		hints = append(hints, verbs.Logs.Hint())
	}
	hints = append(hints, tui.KeyHint{Key: suspend.Key, Label: suspend.Label})
	return hints
}

// CronJobRunNow (ctrl-r) and CronJobSuspend's resume direction both stage a
// screen-owned preview before ever calling actions.Begin on a TierNone verb —
// 0.8.0 plan §36b/§36c/Phase 5. Their staging/commit/render logic lives in
// cronjob_actions.go, big enough on its own (overlap detection, missed-run
// math, bulk marked-set support) to outgrow this file's "navigation plus a
// kind's mutating verbs" scope. beginCronJobSuspend below now only ever
// handles the *suspend* direction — the dangerous half that still goes
// through actions.Controller's ordinary TierInline/TierModal confirm — with
// direction/marked-set routing itself living in
// cronjob_actions.go's beginCronJobSuspendOrResume, update.go's 's' dispatch
// target.

// beginCronJobSuspend starts verbs.CronJobSuspend's suspend direction only:
// TierInline outside PROD, TierModal (the type-the-name modal,
// verbs.Resolve's typed-name policy for "cronjob-suspend") inside one — the
// ordinary Controller-driven confirm every other tiered verb gets, rendered
// generically (keys.go's "cronjob-suspend" case, delete.go's
// typeNameConfirmModal). CronJobResourceVersion/CronJobGeneration are the
// precondition/expected-generation values SetCronJobSuspend needs (§3.3) —
// resolved from the cache-local summary now that Phase 4's aggregation
// exists, closing out the TODO this function used to carry.
func (m *Model) beginCronJobSuspend(row resources.Row) tea.Cmd {
	summary, _ := m.cronJobSummaryFor(row.Namespace, row.Name)
	var resourceVersion string
	var generation int64
	if summary.Object != nil {
		resourceVersion = summary.Object.ResourceVersion
		generation = summary.Object.Generation
	}
	return m.actions.Begin(actions.Request{Verb: verbs.CronJobSuspend, Prod: m.isProd()}, tui.TaskAction{
		ID:    "cronjob-suspend-" + row.Namespace + "/" + row.Name,
		Label: fmt.Sprintf("Suspend %s?", row.Name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: row.Name,
			Namespace: row.Namespace, Verb: "cronjob-suspend", IsMutating: true,
			CronJobResourceVersion: resourceVersion,
			CronJobGeneration:      generation,
			StagedAt:               m.now,
		},
	})
}

// cronJobSuspendWillRunLine is beginCronJobSuspend's confirm "will run: ..."
// line — same idiom as jobSuspendWillRunLine.
func cronJobSuspendWillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.CronJobSuspendCommandString(scope.Namespace, scope.ResourceName, scope.Verb == "cronjob-suspend")
}

// cronJobSuspendDangerNote supplements beginCronJobSuspend's confirm (docs/
// design README.md §36c: "the card names any currently-active Jobs and
// states plainly that they are unaffected: suspend only stops *future*
// scheduling"). "" when summary is unavailable (falls back to the plain
// will-run line with no supplement). now drives the same NextRunLabel
// computation §36a's own NEXT column uses, so the "stops happening" claim
// names the actual next occurrence rather than a vague "future runs".
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

// cronJobKeybarGroup mirrors jobKeybarGroup/fluxKeybarGroup: copies the
// registry Verb, flips CronJobSuspend's label to "resume" when the selected
// row is already suspended.
func (m Model) cronJobKeybarGroup() []tui.KeyHint {
	suspend := verbs.CronJobSuspend
	if row, ok := m.selectedRow(); ok && row.Suspended {
		suspend.Label = "resume"
	}
	hints := []tui.KeyHint{
		{Key: verbs.CronJobRunNow.Key, Label: verbs.CronJobRunNow.Label},
		{Key: suspend.Key, Label: suspend.Label},
		{Key: verbs.CronJobSetSchedule.Key, Label: verbs.CronJobSetSchedule.Label},
		verbs.SetImage.Hint(),
	}
	if m.openLogs != nil {
		// §36a's keybar: "l logs" — verbs.Logs' Kinds already includes
		// CronJob (verbs.go), same conditional-on-wiring guard Pod's own
		// keybar group uses for the identical hint.
		hints = append(hints, verbs.Logs.Hint())
	}
	return hints
}
