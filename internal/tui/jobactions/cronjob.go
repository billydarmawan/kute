package jobactions

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

// RunNow is §36b's staged manual run: everything its preview needs,
// gathered before actions.Begin is ever called on the TierNone verb — the
// staging step is itself the confirmation. Re-staging always re-reads the
// live summary, so nothing here goes stale while the preflight is open.
type RunNow struct {
	Namespace, Name string
	// NewName is the generated -manual-HHMM name — the exact name the Job
	// gets.
	NewName     string
	Concurrency string
	// ActiveRuns is the CronJob's own ActiveRuns snapshot at staging time.
	ActiveRuns []resources.JobSummary
	// UnknownOverlap is true when the Job cache couldn't be read: "never
	// treat a failed Job read as evidence that no run is active" — the
	// overlap card says "overlap unknown" rather than silently taking the
	// no-conflict path.
	UnknownOverlap bool
}

// StageRunNow stages §36b's preflight for namespace/name from its
// cache-local summary. jobsErr is the Job cache read's error, if any. ok is
// false when the summary carries no CronJob object.
func StageRunNow(namespace, name string, summary resources.CronJobSummary, jobsErr error, now time.Time) (*RunNow, bool) {
	if summary.Object == nil {
		return nil, false
	}
	taken := make(map[string]bool, len(summary.Runs))
	for _, r := range summary.Runs {
		taken[r.Name] = true
	}
	return &RunNow{
		Namespace:      namespace,
		Name:           name,
		NewName:        kube.ManualJobName(name, now, func(n string) bool { return taken[n] }),
		Concurrency:    resources.ConcurrencyPolicyLabel(summary.Object.Spec),
		ActiveRuns:     summary.ActiveRuns,
		UnknownOverlap: jobsErr != nil,
	}, true
}

// Key routes a press while the preflight is showing: esc cancels, ↵
// commits, y copies the will-run command (the returned command).
func (r *RunNow) Key(press string) (Outcome, tea.Cmd) {
	switch {
	case verbs.Back.Matches(press):
		return Cancel, nil
	case verbs.Open.Matches(press):
		return Commit, nil
	case verbs.CronJobCopyCommand.Matches(press):
		return Stay, tea.SetClipboard(r.Command())
	}
	return Ignored, nil
}

// Command is the exact `kubectl create job ... --from=cronjob/...`
// invocation TriggerCronJob is equivalent to — the same command the
// keybar's will-run line and 'y' copy show, so they can never disagree.
func (r RunNow) Command() string {
	return kube.CronJobTriggerCommandString(r.Namespace, r.Name, r.NewName)
}

// Action is the request a commit hands to Controller.Begin: a TierNone
// clone of a new standalone Job from the CronJob's jobTemplate, never
// touching the CronJob itself.
func (r RunNow) Action(prod bool, creator string, now time.Time) (actions.Request, tui.TaskAction) {
	return actions.Request{Verb: verbs.CronJobRunNow, Prod: prod}, tui.TaskAction{
		ID:    "cronjob-run-now-" + r.Namespace + "/" + r.Name,
		Label: fmt.Sprintf("Run %s now", r.Name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: r.Name,
			Namespace: r.Namespace, Verb: "cronjob-run-now", IsMutating: true,
			NewName:        r.NewName,
			TriggerCreator: creator,
			StagedAt:       now,
		},
	}
}

// ActiveNames names the active Jobs the overlap card lists.
func (r RunNow) ActiveNames() []string {
	names := make([]string, len(r.ActiveRuns))
	for i, a := range r.ActiveRuns {
		names[i] = a.Name
	}
	return names
}

// Conflict reports a genuine Forbid/Replace overlap — the one case that
// earns a filled warning banner (browse's behavior strip). Allow, no active
// run and an unknown overlap all stay on the keybar line.
func (r RunNow) Conflict() bool {
	return !r.UnknownOverlap && len(r.ActiveRuns) > 0 &&
		(r.Concurrency == "Forbid" || r.Concurrency == "Replace")
}

// OverlapNote is §36b's overlap card text: "" (no chrome at all) when
// there's no active run, an "overlap unknown" warning when the Job cache
// couldn't be read, a warning naming the active Job(s) under Forbid/
// Replace, or a quieter informational note under Allow (docs/design
// README.md §36b). warn selects which of the two treatments applies.
func (r RunNow) OverlapNote() (note string, warn bool) {
	if r.UnknownOverlap {
		return "overlap unknown — job history could not be listed", true
	}
	if len(r.ActiveRuns) == 0 {
		return "", false
	}
	n := len(r.ActiveRuns)
	joined := strings.Join(r.ActiveNames(), ", ")
	switch r.Concurrency {
	case "Forbid", "Replace":
		return fmt.Sprintf("%d job%s already active (%s) · manual run bypasses concurrencyPolicy: %s · next scheduled run unaffected",
			n, PluralSuffix(n), joined, r.Concurrency), true
	default: // Allow
		return fmt.Sprintf("%d job%s already active (%s) · concurrencyPolicy: Allow, no conflict",
			n, PluralSuffix(n), joined), false
	}
}

// Keybar is the keybar while the preflight is showing (§36b: "↵ run · y
// copy · esc cancel"; a complete Job view with no overlap renders zero
// warning chrome — just the will-run line).
func (r RunNow) Keybar() tui.Keybar {
	kb := tui.Keybar{
		Pill: tui.ModeBrowse, PillText: "RUN NOW",
		Groups: [][]tui.KeyHint{{
			verbs.Open.HintAs("run"),
			verbs.CronJobCopyCommand.Hint(),
			verbs.Back.HintAs("cancel"),
		}},
		RightNote: "will run: " + r.Command(),
	}
	if note, warn := r.OverlapNote(); note != "" {
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

// ResumeEntry is one CronJob's resume preview — computed once at staging
// time, so the preflight never re-reads the clock or the lister while it's
// open (only the commit re-executes).
type ResumeEntry struct {
	Namespace, Name, ResourceVersion string
	Generation                       int64
	// SuspendedLabel/SuspendedKnown are resources.SuspendedDuration's own
	// pair — "" / false renders "start unknown" rather than a fabricated
	// duration (externally suspended, or a stale generation marker).
	SuspendedLabel string
	SuspendedKnown bool
	MissedCount    int
	MissedTrunc    bool
	MissedEligible bool
	MissedReason   string
	NextRun        string
}

// ResumeEntryFor computes namespace/name's resume preview from its
// cache-local summary. ok is false when the summary carries no object.
func ResumeEntryFor(namespace, name string, summary resources.CronJobSummary, now time.Time) (ResumeEntry, bool) {
	if summary.Object == nil {
		return ResumeEntry{}, false
	}
	spec := summary.Object.Spec
	e := ResumeEntry{
		Namespace: namespace, Name: name,
		ResourceVersion: summary.Object.ResourceVersion, Generation: summary.Object.Generation,
		NextRun: resources.NextRunLabel(spec, now),
	}
	e.SuspendedLabel, e.SuspendedKnown = resources.SuspendedDuration(summary.SuspendedAt, now)
	if summary.SuspendedAt != nil {
		e.MissedCount, e.MissedTrunc, e.MissedEligible, e.MissedReason = resources.MissedRuns(spec, *summary.SuspendedAt, now)
	} else {
		e.MissedReason = "suspension start unknown"
	}
	return e, true
}

// SummaryLine renders the entry's full preview text.
func (e ResumeEntry) SummaryLine() string {
	suspended := "suspended · start unknown"
	if e.SuspendedKnown {
		suspended = "suspended " + e.SuspendedLabel
	}
	return fmt.Sprintf("%s · %s · next %s", suspended,
		MissedRunsCopy(e.MissedCount, e.MissedTrunc, e.MissedEligible, e.MissedReason), e.NextRun)
}

// Resume is §36c's staged resume: one entry for a single CronJob, N for a
// marked-set bulk resume. Resume is reversible and executes on ↵, but it
// still stages first so the preview can show suspended duration, a bounded
// missed-schedule count and the next run.
type Resume struct {
	Entries []ResumeEntry
}

// Key routes a press while the preflight is showing: esc cancels, ↵
// commits (§36c: "no destructive escalation").
func (r *Resume) Key(press string) Outcome {
	switch {
	case verbs.Back.Matches(press):
		return Cancel
	case verbs.Open.Matches(press):
		return Commit
	}
	return Ignored
}

// Action is the request a commit hands to Controller.Begin at TierNone — a
// single-target action for one entry, a BulkTargets action for a marked
// set ("bulk resume uses one Enter preflight because it is the reversible
// direction").
func (r Resume) Action(prod bool, now time.Time) (actions.Request, tui.TaskAction) {
	req := actions.Request{Verb: verbs.CronJobSuspend, Prod: prod}
	if len(r.Entries) == 1 {
		e := r.Entries[0]
		return req, tui.TaskAction{
			ID:    "cronjob-resume-" + e.Namespace + "/" + e.Name,
			Label: "Resume " + e.Name,
			Scope: tui.TaskScope{
				ResourceKind: string(kube.KindCronJob), ResourceName: e.Name,
				Namespace: e.Namespace, Verb: "cronjob-resume", IsMutating: true,
				CronJobResourceVersion: e.ResourceVersion,
				CronJobGeneration:      e.Generation,
				StagedAt:               now,
			},
		}
	}
	targets := make([]tui.BulkTarget, len(r.Entries))
	for i, e := range r.Entries {
		targets[i] = tui.BulkTarget{Namespace: e.Namespace, ResourceName: e.Name, ResourceVersion: e.ResourceVersion, Generation: e.Generation}
	}
	return req, tui.TaskAction{
		ID:    "cronjob-bulk-resume",
		Label: fmt.Sprintf("Resume %d cronjobs", len(targets)),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), Verb: "cronjob-resume", IsMutating: true,
			StagedAt:    now,
			BulkTargets: targets,
		},
	}
}

// Keybar is the keybar while the preflight is showing — a single target
// gets the full suspended-duration/missed-run/next preview (§36c); a
// marked-set resume gets a quieter aggregate line naming the targets, since
// a per-target breakdown doesn't fit one keybar row.
func (r Resume) Keybar() tui.Keybar {
	kb := tui.Keybar{
		Pill: tui.ModeBrowse, PillText: "RESUME",
		Groups: [][]tui.KeyHint{{
			verbs.Open.HintAs("resume"),
			verbs.Back.HintAs("cancel"),
		}},
	}
	if len(r.Entries) == 1 {
		e := r.Entries[0]
		kb.RightNote = fmt.Sprintf("resume %s? %s", e.Name, e.SummaryLine())
		return kb
	}
	names := make([]string, len(r.Entries))
	for i, e := range r.Entries {
		names[i] = e.Name
	}
	kb.RightNote = fmt.Sprintf("resume %d cronjobs? %s", len(r.Entries), strings.Join(names, ", "))
	return kb
}

// MissedRunsCopy renders the truthful missed-schedule sentence:
//
//	"N schedules were missed · Kubernetes may start one immediately on resume"
//
// — explicit about starting-deadline ineligibility, an unset/unknown
// timezone, or an unknown suspension start (reason non-empty), and a plain
// "no schedules missed" statement when count is genuinely 0.
func MissedRunsCopy(count int, truncated, eligible bool, reason string) string {
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
	plural := PluralSuffix(count)
	if eligible {
		return fmt.Sprintf("%s schedule%s missed while suspended · Kubernetes may start one immediately on resume", label, plural)
	}
	return fmt.Sprintf("%s schedule%s missed while suspended · none eligible to backfill (starting deadline elapsed)", label, plural)
}

// ---------------------------------------------------------------------
// §36c suspend (the dangerous, Controller-confirmed direction)
// ---------------------------------------------------------------------

// SuspendAction is verbs.CronJobSuspend's suspend direction for one
// CronJob, through the controller's ordinary confirm (inline y/N, the
// type-the-name modal in PROD). The resourceVersion/generation are the
// precondition and expected-generation SetCronJobSuspend needs; a summary
// without an object sends them empty.
func SuspendAction(namespace, name string, summary resources.CronJobSummary, prod bool, now time.Time) (actions.Request, tui.TaskAction) {
	var resourceVersion string
	var generation int64
	if summary.Object != nil {
		resourceVersion = summary.Object.ResourceVersion
		generation = summary.Object.Generation
	}
	return actions.Request{Verb: verbs.CronJobSuspend, Prod: prod}, tui.TaskAction{
		ID:    "cronjob-suspend-" + namespace + "/" + name,
		Label: fmt.Sprintf("Suspend %s?", name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: name,
			Namespace: namespace, Verb: "cronjob-suspend", IsMutating: true,
			CronJobResourceVersion: resourceVersion,
			CronJobGeneration:      generation,
			StagedAt:               now,
		},
	}
}

// SuspendWillRunLine is a single-target cronjob-suspend confirm's "will
// run: ..." line.
func SuspendWillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.CronJobSuspendCommandString(scope.Namespace, scope.ResourceName, scope.Verb == "cronjob-suspend")
}

// SuspendDangerNote supplements the suspend confirm (§36c: "the card names
// any currently-active Jobs and states plainly that they are unaffected:
// suspend only stops *future* scheduling"), naming the actual next
// occurrence rather than a vague "future runs". "" when the summary has no
// object.
func SuspendDangerNote(summary resources.CronJobSummary, now time.Time) string {
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

// SuspendHint is 's”s keybar hint, labelled for the direction pressing it
// would take: "resume" on a suspended CronJob, else the registry's own.
func SuspendHint(suspended bool) tui.KeyHint {
	if suspended {
		return verbs.CronJobSuspend.HintAs("resume")
	}
	return verbs.CronJobSuspend.Hint()
}

// PluralSuffix is "s" unless n is 1.
func PluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
