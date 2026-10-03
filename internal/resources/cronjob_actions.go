// Package resources: 0.8.0 plan Phase 5's neutral CronJob action/preflight
// data helpers — the pure computations behind §36b's run-now overlap card
// and §36c's suspend/resume previews. Kept separate from cronjobs.go
// (Phase 1's aggregation/projection) because these are read by a staged
// *action*, not a table row: browse's jobs.go/cronjob_actions.go calls them
// today, and Phase 7's cronjobdetail (which needs the identical run-now/
// suspend/resume previews on its own facts grid) will call the same
// functions once it exists — task packages can't import one another, so
// this shared logic lives here instead of in either task package. Every
// function is pure: caller-supplied `now`, no cluster I/O, no clock reads.
package resources

import (
	"time"

	batchv1 "k8s.io/api/batch/v1"
)

// ConcurrencyPolicyLabel returns spec.ConcurrencyPolicy's display value,
// defaulting to "Allow" per batchv1's own documented zero-value behavior —
// the same default view.go's cronJobBehaviorStripLine already applies,
// factored out here so the run-now overlap card and the behavior strip can
// never disagree about what an unset policy means.
func ConcurrencyPolicyLabel(spec batchv1.CronJobSpec) string {
	if spec.ConcurrencyPolicy == "" {
		return "Allow"
	}
	return string(spec.ConcurrencyPolicy)
}

// missedRunCountCap bounds MissedRuns' search — §3.3: "report 100+ when it
// exceeds the CronJob controller's practical missed-schedule threshold."
const missedRunCountCap = 100

// MissedRunEligible reports whether Kubernetes could still start an
// immediate catch-up Job on resume, given spec.StartingDeadlineSeconds and
// the newest missed occurrence's age relative to now (§3.3). A nil deadline
// means every missed run stays eligible — Kubernetes' own default when
// startingDeadlineSeconds is unset. When the newest missed occurrence is
// already older than the deadline, none of them are eligible: an older
// deadline can't make an even-older miss eligible either.
func MissedRunEligible(deadlineSeconds *int64, newestMissed, now time.Time) bool {
	if deadlineSeconds == nil {
		return true
	}
	if newestMissed.IsZero() {
		return false
	}
	return now.Sub(newestMissed) <= time.Duration(*deadlineSeconds)*time.Second
}

// MissedRuns computes §3.3's truthful resume-preview count: how many of
// spec.Schedule's firings landed strictly between suspendedAt and now, in
// spec.TimeZone, capped at missedRunCountCap so a long-suspended
// high-frequency CronJob renders "100+" (truncated=true) rather than a slow,
// false-precision exact count. eligible reports whether Kubernetes could
// still start one of them immediately on resume (MissedRunEligible).
// reason explains why count is meaningless — an unset timezone (§3.9: Kute
// cannot evaluate a controller-local schedule) or an invalid schedule/
// timezone string — rather than letting a caller mistake 0 for "definitely
// nothing missed". suspendedAt.IsZero() (§3.3: externally suspended, no
// trustworthy Kute timestamp) is reason enough on its own.
func MissedRuns(spec batchv1.CronJobSpec, suspendedAt, now time.Time) (count int, truncated, eligible bool, reason string) {
	if suspendedAt.IsZero() {
		return 0, false, false, "suspension start unknown"
	}
	timezone := ""
	if spec.TimeZone != nil {
		timezone = *spec.TimeZone
	}
	if timezone == "" {
		return 0, false, false, "controller-local timezone — missed count unknown"
	}
	n, trunc, err := CountCronRuns(spec.Schedule, timezone, suspendedAt, now, missedRunCountCap)
	if err != nil {
		return 0, false, false, err.Error()
	}
	if n == 0 {
		return 0, false, false, ""
	}
	// The newest missed occurrence's own timestamp is what
	// MissedRunEligible needs. A truncated count stopped walking long before
	// now, so it's found by searching back from now instead — never assumed
	// to be now itself, which would call a miss eligible under a deadline
	// shorter than the gap since the last real firing.
	newest, ok := lastCronRun(spec.Schedule, timezone, suspendedAt, now)
	if !ok {
		return n, trunc, false, ""
	}
	return n, trunc, MissedRunEligible(spec.StartingDeadlineSeconds, newest, now), ""
}

// SuspendedDuration renders §3.3's "suspended Nd" — shortAge(now -
// SuspendedAt) — or ok=false when CronJobSummary.SuspendedAt is nil
// (externally suspended, or the generation marker went stale): the caller
// must render "start unknown" rather than a fabricated duration.
func SuspendedDuration(suspendedAt *time.Time, now time.Time) (label string, ok bool) {
	if suspendedAt == nil {
		return "", false
	}
	return shortAge(now.Sub(*suspendedAt)), true
}

// lastCronRun returns schedule's newest firing in (after, to], searching
// back from to over a doubling window so the walk covers only the most
// recent stretch, however long the CronJob has been suspended. ok is false
// when there is none (or the schedule/timezone doesn't parse).
func lastCronRun(schedule, timezone string, after, to time.Time) (time.Time, bool) {
	sched, err := parseCronSchedule(schedule)
	if err != nil || timezone == "" {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, false
	}
	toIn := to.In(loc)
	for window := time.Minute; ; window *= 2 {
		start := toIn.Add(-window)
		clamped := !start.After(after)
		if clamped {
			start = after.In(loc)
		}
		var last time.Time
		for at := sched.Next(start); !at.IsZero() && !at.After(toIn); at = sched.Next(at) {
			last = at
		}
		if !last.IsZero() {
			return last, true
		}
		if clamped {
			return time.Time{}, false
		}
	}
}
