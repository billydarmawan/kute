// Package resources: this file holds v0.8.0's CronJob/Job aggregation. It is pure data: every
// exported function here takes its inputs (raw API objects, a caller-
// supplied clock) as parameters and returns a value. Nothing in this file
// performs cluster I/O or reads the wall clock — see BuildCronJobSummaries'
// and ProjectCronJob's own doc comments for the one deliberate boundary
// (the CronJob descriptor's single-object Project fallback in
// registry.go/projections.go, which is Job-unaware by design and does read
// the clock, precisely because it has nowhere else to get one).
//
// The shape: BuildCronJobSummaries joins CronJob/Job/Pod snapshots (already
// read from the informer cache by a caller — browse's Phase 4 load()) into
// one CronJobSummary per CronJob, holding absolute timestamps only.
// ProjectCronJob/ProjectAssociatedJob turn a summary into a display Row
// given an explicit `now`, so the same summary renders identically whether
// it's rendered once or on every second of a UI clock tick.
//
// Deliberately not done here: internal/tui/tasks/browse/sort.go's
// workloadKinds map (unhealthy-first sort) does not gain a KindCronJob
// entry. §36a's default keeps CronJobs in namespace/name order — a
// suspended CronJob is usually suspended by the operator that morning and
// needs to be found where it was left, not sunk to the bottom by a
// health-first default — but, like every other kind, a manual 1-9 column
// pick (docs/design README.md §36a) overrides that default. That's a
// browse concern outside Phase 1's scope, noted here only so the omission
// reads as intentional rather than forgotten.
package resources

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kute-dev/kute/internal/kube"
)

// JobSource distinguishes a controller-scheduled Job from a Kute-triggered
// manual one (§36b) — the two association rules in cronJobAssociation.
type JobSource int

const (
	JobSourceScheduled JobSource = iota
	JobSourceManual
)

// JobSummary is one Job's outcome, association, and best-available pod
// target — pure display data, deep-copied out of the Job/Pod objects it was
// built from (see buildJobSummary). Absolute timestamps only; a projector
// turns these into relative strings against a caller-supplied `now`.
type JobSummary struct {
	Namespace string
	Name      string
	UID       types.UID

	Source  JobSource
	Creator string // AnnotationTriggeredBy's value, JobSourceManual only.

	ScheduledAt time.Time // Zero for a manual run — there was no schedule tick.
	StartedAt   time.Time // Zero until the Job's pod(s) actually start.
	CompletedAt time.Time // Zero while active; see jobTerminalTime for how a Failed Job without Status.CompletionTime gets one.

	Active    bool
	Succeeded bool
	Failed    bool

	Completions    string // "N/M", Job.Status.Succeeded over Spec.Completions.
	FailedAttempts int32  // Job.Status.Failed — the retry count, not a bool.

	// Reason/Message come from the Job's own Complete/Failed condition
	// (§3.5): the stable, authoritative outcome. A representative Pod never
	// overrides these — only ExitCode/PodName below come from a Pod, and
	// only when one was found.
	Reason  string
	Message string

	// ExitCode/PodName are Pod-sourced additions (§3.5 point 2): PodName is
	// the newest associated Pod regardless of its state (a live logs
	// target even for a still-running Job); ExitCode is set only when that
	// Pod has a terminated container to read one from. Both stay
	// unset/nil when no Pod was collected — a missing Pod must never erase
	// a valid Job-level failure (§3.5 point 3, Phase 1 test 8).
	ExitCode *int32
	PodName  string
}

// CronJobSummary is one CronJob's own snapshot plus its associated Jobs —
// the unit both browse (Phase 4) and cronjobdetail (Phase 7) will project
// from. Object is a deep copy (BuildCronJobSummaries), so it's safe to hold
// across a UI clock tick without racing an informer cache update.
type CronJobSummary struct {
	Object *batchv1.CronJob

	// Runs is every associated Job (scheduled + manual), newest-first by
	// runTimestamp. Includes both terminal and active runs.
	Runs []JobSummary

	// LastTerminal is the newest *terminal* associated run — Succeeded or
	// Failed, never one still Active — or nil when no retained run has
	// finished yet. §4.3: this is what LAST RUN renders, deliberately
	// independent of ActiveRuns below, so an active CronJob can read
	// "2m ago ✓" in LAST RUN while ACT reads 1.
	LastTerminal *JobSummary

	// ActiveRuns is every associated Job currently Active (Job.Status.Active
	// > 0) — the ACT column's count and Row.Active's source.
	ActiveRuns []JobSummary

	// SuspendedAt is Kute's own suspend timestamp (§3.3), trusted only when
	// AnnotationSuspendedGeneration still matches the CronJob's current
	// generation. nil whenever the CronJob isn't suspended, was suspended
	// externally (no annotation), or the annotation's generation is stale —
	// never a guess; a nil value here means "start unknown", not "not
	// suspended" (check Object.Spec.Suspend for that).
	SuspendedAt *time.Time
}

// JobColumns names the columns ProjectAssociatedJob fills, in order, so a
// caller doesn't have to accept a fixed cell layout it doesn't need. Phase 1
// ships exactly one value, AssociatedJobColumns — the §36e "JOBS table"
// column subset (docs/design README.md §36e: "same columns, same SOURCE
// column distinguishing schedule HH:MM runs from manual · ctrl-r ones").
// cronjobdetail (Phase 7) is the only caller planned today; a narrower or
// differently-ordered subset later is a new JobColumns value, not a change
// to ProjectAssociatedJob's cell-building switch.
type JobColumns []string

// AssociatedJobColumns is the §36e column set: NAME, SOURCE, START,
// DURATION, OUTCOME.
var AssociatedJobColumns = JobColumns{"Name", "Source", "Start", "Duration", "Outcome"}

// ErrTimeZoneUnset is NextCronRuns/CountCronRuns' sentinel for a CronJob
// with no explicit spec.timeZone (§3.9). Unlike a genuinely invalid
// schedule or timezone string, this isn't an error in the CronJob — it's a
// question Kute cannot answer, because the schedule is evaluated in the
// kube-controller-manager process's own local timezone, which no interface
// exposes. Callers (ProjectCronJob's NEXT cell) render "controller local"
// for this case and "—" for every other error.
var ErrTimeZoneUnset = errors.New("cronjob has no explicit spec.timeZone; next run depends on the kube-controller-manager process's own local timezone, which kute cannot discover")

// parseCronSchedule parses schedule the same way the rest of the codebase
// does (projections.go's old cronNextRunCell, internal/kube/mutate.go's
// schedule validation): cron.ParseStandard, five fields plus '@' macros
// (Descriptor is one of ParseStandard's built-in options — @daily,
// @every 1h30m, etc. all parse), matching what the real CronJob controller
// accepts. No seconds field — Kubernetes CronJobs don't have one either.
func parseCronSchedule(schedule string) (cron.Schedule, error) {
	return cron.ParseStandard(schedule)
}

// NextCronRuns returns the next count absolute firing times for schedule,
// evaluated in timezone, strictly after from. An empty timezone returns
// ErrTimeZoneUnset (§3.9) rather than guessing UTC or the workstation's own
// zone. count <= 0 returns (nil, nil).
func NextCronRuns(schedule, timezone string, from time.Time, count int) ([]time.Time, error) {
	if count <= 0 {
		return nil, nil
	}
	sched, err := parseCronSchedule(schedule)
	if err != nil {
		return nil, fmt.Errorf("invalid schedule %q: %w", schedule, err)
	}
	if timezone == "" {
		return nil, ErrTimeZoneUnset
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone %q: %w", timezone, err)
	}
	at := from.In(loc)
	out := make([]time.Time, 0, count)
	for range count {
		at = sched.Next(at)
		if at.IsZero() {
			break
		}
		out = append(out, at)
	}
	return out, nil
}

// CountCronRuns counts schedule's firings in timezone strictly after from
// and up to and including to, capped at limit (limit <= 0 means unbounded —
// callers driving this off a UI action should always pass a real limit;
// nothing in this package calls it unbounded). truncated reports whether
// the count hit limit before reaching to, so a caller can render "100+"
// rather than a false-precision exact number (§3.3's missed-schedule
// count). Same ErrTimeZoneUnset/parse-error behavior as NextCronRuns.
func CountCronRuns(schedule, timezone string, from, to time.Time, limit int) (count int, truncated bool, err error) {
	sched, err := parseCronSchedule(schedule)
	if err != nil {
		return 0, false, fmt.Errorf("invalid schedule %q: %w", schedule, err)
	}
	if timezone == "" {
		return 0, false, ErrTimeZoneUnset
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return 0, false, fmt.Errorf("invalid timezone %q: %w", timezone, err)
	}
	at := from.In(loc)
	toIn := to.In(loc)
	for {
		if limit > 0 && count >= limit {
			return count, true, nil
		}
		at = sched.Next(at)
		if at.IsZero() || at.After(toIn) {
			return count, false, nil
		}
		count++
	}
}

// controllerOwnerRef returns the single owner reference in refs with
// Controller set true, checking every index rather than assuming refs[0] —
// a sparse test fixture (or a real object with an unrelated non-controller
// owner listed first) can put it anywhere in the slice. Kubernetes permits
// at most one controller=true owner reference per object, so the first
// match found is the only one that matters.
func controllerOwnerRef(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	return nil
}

// ownerKey builds the join key BuildCronJobSummaries indexes Jobs and Pods
// by: a UID when one is known (the authoritative case), namespace+name only
// when it's absent on the side being keyed (the sparse-fake-object
// fallback). Namespace is always included because owner references never
// cross namespaces and Kute's own association annotations are validated
// against the Job's namespace before this is ever called with them.
//
// This single scheme is what makes the four association rules (§4.2, Phase
// 1 tests 1-4) fall out of a plain map lookup instead of an explicit
// UID-then-name branch at every call site: two keys built with a UID
// collide only on equal UIDs; two keys built with an absent UID collide
// only on equal namespace+name; a key built from a present UID never
// collides with one built from an absent UID even if the names match, which
// is exactly the "reject a stale-UID owner even when the name still
// matches" rule.
func ownerKey(namespace string, uid types.UID, name string) string {
	if uid != "" {
		return namespace + "/uid:" + string(uid)
	}
	return namespace + "/name:" + name
}

// indexPodsByJob groups pods by their controller-owning Job's ownerKey, for
// buildJobSummary's best-available Pod lookup. A Pod without a Job
// controller owner reference is ignored — it isn't part of any Job's run
// history.
func indexPodsByJob(pods []runtime.Object) map[string][]*corev1.Pod {
	out := map[string][]*corev1.Pod{}
	for _, obj := range pods {
		p, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}
		ref := controllerOwnerRef(p.OwnerReferences)
		if ref == nil || ref.Kind != kube.KindJob.APIKind() {
			continue
		}
		key := ownerKey(p.Namespace, ref.UID, ref.Name)
		out[key] = append(out[key], p.DeepCopy())
	}
	return out
}

// cronJobAssociation determines whether j associates with a CronJob at all,
// and how (§4.2):
//
//  1. A controller owner reference whose Kind is CronJob (kube.KindCronJob's
//     own APIKind, never a bare string compare) — JobSourceScheduled.
//  2. Failing that, Kute's own AnnotationCronJobUID/AnnotationCronJobName
//     annotations — JobSourceManual.
//
// Either way this returns the *candidate* join key built from whatever the
// owner reference/annotations say, not proof that a real CronJob with that
// key exists — indexJobsByCronJob's map lookup against each real CronJob's
// own key is what actually resolves or rejects it (including the "same
// name, stale UID" rejection: a stale-UID owner reference builds a key that
// simply doesn't match the real CronJob's current-UID key). A Job with
// neither a CronJob-kind controller owner nor either annotation returns
// ok=false and is excluded outright — this is what keeps a same-name-prefix
// Job out (§4.2: "never associate by name prefix").
func cronJobAssociation(j *batchv1.Job) (key string, source JobSource, ok bool) {
	if ref := controllerOwnerRef(j.OwnerReferences); ref != nil && ref.Kind == kube.KindCronJob.APIKind() {
		return ownerKey(j.Namespace, ref.UID, ref.Name), JobSourceScheduled, true
	}
	uid := types.UID(j.Annotations[kube.AnnotationCronJobUID])
	name := j.Annotations[kube.AnnotationCronJobName]
	if uid == "" && name == "" {
		return "", 0, false
	}
	return ownerKey(j.Namespace, uid, name), JobSourceManual, true
}

// jobTerminalOutcome reads a Job's own Complete/Failed condition (§3.5
// point 1) — the stable, authoritative source for succeeded/failed plus the
// reason/message a Pod's termination must never override. Falls back to
// Status.CompletionTime alone (no matching condition set) only for minimal
// fixtures/older objects that reached a terminal state before the condition
// caught up; Failed wins if both conditions are somehow simultaneously True
// (shouldn't happen, but a Job can't sensibly be both).
func jobTerminalOutcome(j *batchv1.Job) (succeeded, failed bool, reason, message string) {
	var completeCond, failedCond *batchv1.JobCondition
	for i := range j.Status.Conditions {
		c := &j.Status.Conditions[i]
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			completeCond = c
		case batchv1.JobFailed:
			failedCond = c
		}
	}
	switch {
	case failedCond != nil:
		return false, true, failedCond.Reason, failedCond.Message
	case completeCond != nil:
		return true, false, completeCond.Reason, completeCond.Message
	case j.Status.CompletionTime != nil:
		return true, false, "", ""
	default:
		return false, false, "", ""
	}
}

// jobTerminalTime is CompletedAt's fallback when Status.CompletionTime is
// unset — true for every Failed Job (the API only ever sets CompletionTime
// on success) and for a Complete Job read before that field landed. Reads
// the same condition jobTerminalOutcome found, so the two never disagree
// about which condition is authoritative.
func jobTerminalTime(j *batchv1.Job) time.Time {
	for i := range j.Status.Conditions {
		c := j.Status.Conditions[i]
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type == batchv1.JobFailed || c.Type == batchv1.JobComplete {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

// representativePod picks one Pod from an associated set as the logs
// target / exit-code source: the newest by creation time, name as a
// deterministic tiebreaker. pods must be non-empty.
func representativePod(pods []*corev1.Pod) *corev1.Pod {
	best := pods[0]
	for _, p := range pods[1:] {
		switch {
		case p.CreationTimestamp.After(best.CreationTimestamp.Time):
			best = p
		case p.CreationTimestamp.Equal(&best.CreationTimestamp) && p.Name < best.Name:
			best = p
		}
	}
	return best
}

// terminatedExitCode returns the first terminated container's exit code
// found on p, mirroring projections.go's podTerminatedReason. ok is false
// when no container has terminated yet (still running/waiting), which
// callers must treat as "no exit code available", never as 0.
func terminatedExitCode(p *corev1.Pod) (code int32, ok bool) {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			return cs.State.Terminated.ExitCode, true
		}
	}
	return 0, false
}

// buildJobSummary projects one deep-copied Job (plus its already-indexed
// associated Pods) into a JobSummary. j is deep-copied here (task 12) so
// the summary never holds a pointer into a cache object a concurrent watch
// update could replace mid-read.
func buildJobSummary(job *batchv1.Job, source JobSource, pods []*corev1.Pod) JobSummary {
	job = job.DeepCopy()
	succeeded, failed, reason, message := jobTerminalOutcome(job)
	active := job.Status.Active > 0

	var completedAt time.Time
	switch {
	case job.Status.CompletionTime != nil:
		completedAt = job.Status.CompletionTime.Time
	case succeeded || failed:
		completedAt = jobTerminalTime(job)
	}
	var startedAt time.Time
	if job.Status.StartTime != nil {
		startedAt = job.Status.StartTime.Time
	}
	var scheduledAt time.Time
	if source == JobSourceScheduled {
		// The CronJob controller creates a scheduled Job at (or very near)
		// its firing tick, so the Job's own creationTimestamp is the best
		// available stand-in for "when this run was scheduled" — Kubernetes
		// doesn't otherwise expose the tick a specific Job answers.
		scheduledAt = job.CreationTimestamp.Time
	}

	var creator string
	if source == JobSourceManual {
		creator = job.Annotations[kube.AnnotationTriggeredBy]
	}

	var podName string
	var exitCode *int32
	if len(pods) > 0 {
		best := representativePod(pods)
		podName = best.Name
		if code, ok := terminatedExitCode(best); ok {
			c := code
			exitCode = &c
		}
	}

	return JobSummary{
		Namespace:      job.Namespace,
		Name:           job.Name,
		UID:            job.UID,
		Source:         source,
		Creator:        creator,
		ScheduledAt:    scheduledAt,
		StartedAt:      startedAt,
		CompletedAt:    completedAt,
		Active:         active,
		Succeeded:      succeeded,
		Failed:         failed,
		Completions:    fmt.Sprintf("%d/%d", job.Status.Succeeded, int32ptr(job.Spec.Completions)),
		FailedAttempts: job.Status.Failed,
		Reason:         reason,
		Message:        message,
		ExitCode:       exitCode,
		PodName:        podName,
	}
}

// indexJobsByCronJob groups every associated Job's JobSummary by its
// candidate CronJob ownerKey (task 2's "one Job index keyed by source
// CronJob UID/name"). Unassociated Jobs (cronJobAssociation's ok=false) are
// dropped here, before any per-CronJob lookup happens.
func indexJobsByCronJob(jobs []runtime.Object, podsByJobKey map[string][]*corev1.Pod) map[string][]JobSummary {
	out := map[string][]JobSummary{}
	for _, obj := range jobs {
		j, ok := obj.(*batchv1.Job)
		if !ok {
			continue
		}
		key, source, ok := cronJobAssociation(j)
		if !ok {
			continue
		}
		jobKey := ownerKey(j.Namespace, j.UID, j.Name)
		summary := buildJobSummary(j, source, podsByJobKey[jobKey])
		out[key] = append(out[key], summary)
	}
	return out
}

// runTimestamp is the instant a run is sorted/compared by: the newest
// meaningful timestamp it has, preferring an actual completion over a start
// over a bare schedule tick.
func runTimestamp(s JobSummary) time.Time {
	switch {
	case !s.CompletedAt.IsZero():
		return s.CompletedAt
	case !s.StartedAt.IsZero():
		return s.StartedAt
	default:
		return s.ScheduledAt
	}
}

// suspendedAt derives CronJobSummary.SuspendedAt (§3.3): nil unless the
// CronJob is currently suspended *and* carries both Kute annotations *and*
// AnnotationSuspendedGeneration still equals the object's current
// generation — an external spec change since the suspend (or an external
// resume/resuspend cycle) invalidates the marker, and this deliberately
// returns nil rather than a stale guess.
func suspendedAt(cj *batchv1.CronJob) *time.Time {
	if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
		return nil
	}
	ts := cj.Annotations[kube.AnnotationSuspendedAt]
	genStr := cj.Annotations[kube.AnnotationSuspendedGeneration]
	if ts == "" || genStr == "" {
		return nil
	}
	gen, err := strconv.ParseInt(genStr, 10, 64)
	if err != nil || gen != cj.Generation {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return nil
	}
	return &parsed
}

// BuildCronJobSummaries joins cronJobs/jobs/pods (already read once from the
// informer cache by a caller — never called per-row) into one
// CronJobSummary per CronJob object in cronJobs, in the same order. Every
// CronJob is deep-copied into its summary's Object (task 12), so the
// returned slice is safe to hold across a UI clock tick without racing a
// concurrent cache update.
//
// This is the aggregation browse's Phase 4 CronJob branch and cronjobdetail
// (Phase 7) both call directly, bypassing resources.List/Descriptor.Project
// entirely — see registry.go's CronJob descriptor and
// projections.go's projectCronJobFallback for why Descriptor.Project alone
// must never be assumed Job-aware.
func BuildCronJobSummaries(cronJobs, jobs, pods []runtime.Object) []CronJobSummary {
	podsByJobKey := indexPodsByJob(pods)
	jobsByCronJobKey := indexJobsByCronJob(jobs, podsByJobKey)

	out := make([]CronJobSummary, 0, len(cronJobs))
	for _, obj := range cronJobs {
		cj, ok := obj.(*batchv1.CronJob)
		if !ok {
			continue
		}
		cj = cj.DeepCopy()

		runs := append([]JobSummary(nil), jobsByCronJobKey[ownerKey(cj.Namespace, cj.UID, cj.Name)]...)
		slices.SortStableFunc(runs, func(a, b JobSummary) int {
			return runTimestamp(b).Compare(runTimestamp(a))
		})

		summary := CronJobSummary{Object: cj, Runs: runs, SuspendedAt: suspendedAt(cj)}
		for i := range runs {
			if runs[i].Active {
				// An active run is never a candidate for LastTerminal, even
				// if it happens to be the newest by runTimestamp (§4.3,
				// Phase 1 test 5) — LAST RUN must keep reporting the
				// previous terminal outcome while ACT shows the run in
				// flight.
				summary.ActiveRuns = append(summary.ActiveRuns, runs[i])
				continue
			}
			if !runs[i].Succeeded && !runs[i].Failed {
				continue
			}
			if summary.LastTerminal == nil || runTimestamp(runs[i]).After(runTimestamp(*summary.LastTerminal)) {
				r := runs[i]
				summary.LastTerminal = &r
			}
		}
		out = append(out, summary)
	}
	return out
}

// terminalMark renders a terminal JobSummary's outcome glyph: ✓ succeeded,
// ✕ otherwise (failed — s is only ever passed here when one of the two is
// true, see ProjectCronJob).
func terminalMark(s JobSummary) string {
	if s.Succeeded {
		return "✓" // ✓
	}
	return "✕" // ✕
}

// cronJobStatusGlyph picks ProjectCronJob's leading status-column glyph and
// its color class, per docs/design README.md §4.3's priority order:
// suspended beats active beats the last terminal outcome beats "no history
// yet". This is the row's Glyph/GlyphClass, deliberately independent of
// Status (which stays keyed to the last terminal outcome alone, per §4.3,
// so the health strip's OK/Fail/Neutral tally means "last run succeeded/
// failed/unknown" and never silently absorbs "currently running" or
// "paused") — the same Glyph-vs-Status split Row's own doc comment already
// describes for 18a's outdated release and §35b's expiring certificate.
//
// GlyphCronActive/GlyphCronPaused mirror internal/tui/glyphs.go's constants
// of the same rune (this package can't import tui — see resources.go).
func cronJobStatusGlyph(suspended, active bool, status StatusClass) (glyph string, class StatusClass) {
	switch {
	case suspended:
		return "⏸", StatusNeutral // ⏸ — GlyphCronPaused
	case active:
		return "◐", StatusWarn // ◐ — GlyphCronActive
	case status == StatusFail:
		return "✕", StatusFail // ✕
	case status == StatusOK:
		return "●", StatusOK // ●
	default:
		return "○", StatusNeutral // ○
	}
}

// NextRunLabel renders the same "in 4m"/"controller local"/"—" text
// ProjectCronJob's NEXT cell shows for a running (non-suspended) CronJob —
// factored out of cronJobNextCell so §36c's resume preview (0.8.0 plan
// Phase 5, cronjob_actions.go) can compute the *post-resume* NEXT off a
// still-suspended CronJob's own spec without duplicating NextCronRuns/
// shortEta's error handling. "—" for a schedule that fails to parse or a
// timezone string that's invalid, "controller local" for one with no
// explicit spec.timeZone (§3.9 — Kute cannot discover the
// kube-controller-manager process's own local zone), otherwise shortEta's
// "in 4m"/"in 2h" reading.
func NextRunLabel(spec batchv1.CronJobSpec, now time.Time) string {
	tz := ""
	if spec.TimeZone != nil {
		tz = *spec.TimeZone
	}
	runs, err := NextCronRuns(spec.Schedule, tz, now, 1)
	switch {
	case errors.Is(err, ErrTimeZoneUnset):
		return "controller local"
	case err != nil, len(runs) == 0:
		return "—"
	default:
		return shortEta(runs[0].Sub(now))
	}
}

// cronJobNextCell renders ProjectCronJob's NEXT cell: "—" for a suspended
// CronJob, otherwise NextRunLabel.
func cronJobNextCell(cj *batchv1.CronJob, suspended bool, now time.Time) string {
	if suspended {
		return "—"
	}
	return NextRunLabel(cj.Spec, now)
}

// ProjectCronJob turns summary into 36a's list Row — NAME · SCHEDULE ·
// SUSP · ACT · LAST RUN · NEXT (registry.go's CronJob Columns) — using now
// for every relative string. Never reads the clock itself: the same summary
// projected at two different `now` values is the entire contract that lets
// a UI clock tick rebuild cells without a fresh cache read.
func ProjectCronJob(summary CronJobSummary, now time.Time) Row {
	cj := summary.Object
	if cj == nil {
		return Row{}
	}
	suspended := cj.Spec.Suspend != nil && *cj.Spec.Suspend
	active := len(summary.ActiveRuns) > 0

	// §4.3: Status is keyed to the last *terminal* outcome only — Active/
	// Suspended ride on Row.Active/Row.Suspended instead, exactly the way
	// Outdated/ExpiringSoon cross-cut Status for other kinds.
	status := StatusNeutral
	switch {
	case summary.LastTerminal != nil && summary.LastTerminal.Succeeded:
		status = StatusOK
	case summary.LastTerminal != nil && summary.LastTerminal.Failed:
		status = StatusFail
	}

	glyph, glyphClass := cronJobStatusGlyph(suspended, active, status)

	suspCell := "no"
	if suspended {
		suspCell = "yes"
	}

	lastRunCell := "no retained runs"
	if summary.LastTerminal != nil {
		lastRunCell = shortAge(now.Sub(runTimestamp(*summary.LastTerminal))) + " ago " + terminalMark(*summary.LastTerminal)
	}

	subLine := ""
	if summary.LastTerminal != nil && summary.LastTerminal.Failed {
		subLine = cronJobFailureSubLine(*summary.LastTerminal)
	}

	return Row{
		Namespace: cj.Namespace,
		Name:      cj.Name,
		Cells: []string{
			cj.Name,
			cj.Spec.Schedule,
			suspCell,
			strconv.Itoa(len(summary.ActiveRuns)),
			lastRunCell,
			cronJobNextCell(cj, suspended, now),
		},
		Status:     status,
		Glyph:      glyph,
		GlyphClass: glyphClass,
		Suspended:  suspended,
		Active:     active,
		SubLine:    subLine,
	}
}

// cronJobFailureSubLine formats §36a's failure continuation line: the newest
// terminal run's own Job condition reason/message, verbatim — the same
// "prose renders as a continuation line, never paraphrased" rule
// fluxSubLine/argoSubLine follow (resources/flux.go, resources/argo.go).
// Unlike those two curated kinds, this one is not gated by row selection —
// every failed CronJob shows its own line, all the time (docs/design
// README.md §36a).
func cronJobFailureSubLine(s JobSummary) string {
	switch {
	case s.Reason != "" && s.Message != "":
		return s.Reason + " — " + s.Message
	case s.Reason != "":
		return s.Reason
	case s.Message != "":
		return s.Message
	default:
		return "failed — no message retained"
	}
}

// associatedJobDuration renders ProjectAssociatedJob's DURATION cell: the
// elapsed time between start and completion for a terminal run, between
// start and now for one still active, or "–" when the run hasn't started
// yet (still queued/waiting on concurrency policy).
func associatedJobDuration(s JobSummary, now time.Time) string {
	switch {
	case s.StartedAt.IsZero():
		return "–"
	case !s.CompletedAt.IsZero():
		return shortAge(s.CompletedAt.Sub(s.StartedAt))
	case s.Active:
		return shortAge(now.Sub(s.StartedAt))
	default:
		return "–"
	}
}

// associatedJobOutcome renders ProjectAssociatedJob's OUTCOME cell.
func associatedJobOutcome(s JobSummary) string {
	switch {
	case s.Succeeded:
		return "✓ " + s.Completions // ✓ N/M
	case s.Failed:
		if s.Reason != "" {
			return "✕ " + s.Reason // ✕ Reason
		}
		return "✕" // ✕
	case s.Active:
		return "running"
	default:
		return "–"
	}
}

// associatedJobSource renders ProjectAssociatedJob's SOURCE cell (docs/design
// README.md §36e: "same SOURCE column distinguishing schedule HH:MM runs
// from manual · ctrl-r ones"). Formats ScheduledAt in its own stored
// location (UTC unless a caller populated it otherwise) — this is display
// text, not a value ProjectCronJob's own NEXT-cell timezone handling needs
// to agree with.
func associatedJobSource(s JobSummary) string {
	if s.Source == JobSourceManual {
		return "manual · ctrl+r" // "manual · ctrl+r"
	}
	if s.ScheduledAt.IsZero() {
		return "schedule"
	}
	return "schedule " + s.ScheduledAt.Format("15:04")
}

// associatedJobCell resolves one named column's cell text for
// ProjectAssociatedJob. Unknown column names render "–" rather than
// panicking, so a future JobColumns value with a typo fails visibly (an
// obviously-wrong "–" cell) instead of crashing a detail screen.
func associatedJobCell(column string, s JobSummary, now time.Time) string {
	switch column {
	case "Name":
		return s.Name
	case "Source":
		return associatedJobSource(s)
	case "Start":
		if s.StartedAt.IsZero() {
			return "–"
		}
		return shortAge(now.Sub(s.StartedAt)) + " ago"
	case "Duration":
		return associatedJobDuration(s, now)
	case "Outcome":
		return associatedJobOutcome(s)
	default:
		return "–"
	}
}

// ProjectAssociatedJob turns one JobSummary into a Row for columns (§36e's
// JOBS table — cronjobdetail, Phase 7). Like ProjectCronJob, now is a
// parameter and nothing here reads the clock.
func ProjectAssociatedJob(summary JobSummary, now time.Time, columns JobColumns) Row {
	cells := make([]string, len(columns))
	for i, col := range columns {
		cells[i] = associatedJobCell(col, summary, now)
	}
	status := StatusNeutral
	switch {
	case summary.Succeeded:
		status = StatusOK
	case summary.Failed:
		status = StatusFail
	case summary.Active:
		status = StatusWarn
	}
	return Row{
		Namespace: summary.Namespace,
		Name:      summary.Name,
		Cells:     cells,
		Status:    status,
	}
}

// cronJobHealth is CronJob's Health implementation (§4.3, Phase 1 task 6):
// the usual OK/Fail/Neutral tally off Status (never Warn — CronJob's own
// Status is keyed to the last terminal outcome only, see ProjectCronJob),
// plus Active/Suspended counted a second time as cross-cutting extras —
// same shape as helmReleaseHealth's Outdated tally in registry.go.
func cronJobHealth(rows []Row) HealthCounts {
	h := StatusHealth(rows)
	for _, r := range rows {
		if r.Active {
			h.Active++
		}
		if r.Suspended {
			h.Suspended++
		}
	}
	return h
}

// cronJobHealthLabel is §4.3/36a's health-strip wording for CronJob's own
// three Status buckets: "4 last run ok", "1 last run failed". The
// Active/Suspended cross-cutting segments carry their own literal labels
// ("active now", "suspended") where browse renders them (view.go, the same
// place Outdated/ExpiringSoon's own labels live) rather than through this
// function, which only ever sees OK/Warn/Fail/Neutral.
func cronJobHealthLabel(class StatusClass) string {
	switch class {
	case StatusOK:
		return "last run ok"
	case StatusFail:
		return "last run failed"
	default:
		return "no history"
	}
}
