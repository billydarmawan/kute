package resources

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
)

func int64ptr(v int64) *int64 { return &v }

func TestConcurrencyPolicyLabelDefaultsToAllow(t *testing.T) {
	if got := ConcurrencyPolicyLabel(batchv1.CronJobSpec{}); got != "Allow" {
		t.Fatalf("got %q, want Allow", got)
	}
	spec := batchv1.CronJobSpec{ConcurrencyPolicy: batchv1.ForbidConcurrent}
	if got := ConcurrencyPolicyLabel(spec); got != "Forbid" {
		t.Fatalf("got %q, want Forbid", got)
	}
}

func TestMissedRunEligible(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if !MissedRunEligible(nil, now.Add(-time.Hour), now) {
		t.Fatal("nil deadline should always be eligible")
	}
	if MissedRunEligible(int64ptr(60), time.Time{}, now) {
		t.Fatal("a zero newestMissed should never be eligible")
	}
	if !MissedRunEligible(int64ptr(600), now.Add(-5*time.Minute), now) {
		t.Fatal("a miss inside the deadline window should be eligible")
	}
	if MissedRunEligible(int64ptr(60), now.Add(-5*time.Minute), now) {
		t.Fatal("a miss older than the deadline should not be eligible")
	}
}

func TestMissedRunsUnknownTimezoneReturnsReason(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	spec := batchv1.CronJobSpec{Schedule: "*/5 * * * *"}
	count, truncated, eligible, reason := MissedRuns(spec, now.Add(-time.Hour), now)
	if count != 0 || truncated || eligible || reason == "" {
		t.Fatalf("count=%d truncated=%v eligible=%v reason=%q, want a zero result with a reason", count, truncated, eligible, reason)
	}
}

func TestMissedRunsUnknownSuspendStartReturnsReason(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tz := "UTC"
	spec := batchv1.CronJobSpec{Schedule: "*/5 * * * *", TimeZone: &tz}
	count, truncated, eligible, reason := MissedRuns(spec, time.Time{}, now)
	if count != 0 || truncated || eligible || reason != "suspension start unknown" {
		t.Fatalf("count=%d truncated=%v eligible=%v reason=%q, want suspension start unknown", count, truncated, eligible, reason)
	}
}

func TestMissedRunsCountsAndEligibility(t *testing.T) {
	tz := "UTC"
	spec := batchv1.CronJobSpec{Schedule: "*/5 * * * *", TimeZone: &tz}
	suspendedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := suspendedAt.Add(20 * time.Minute) // 4 missed firings: :05 :10 :15 :20

	count, truncated, eligible, reason := MissedRuns(spec, suspendedAt, now)
	if reason != "" {
		t.Fatalf("unexpected reason %q", reason)
	}
	if truncated {
		t.Fatal("expected no truncation for a small window")
	}
	if count != 4 {
		t.Fatalf("count = %d, want 4", count)
	}
	if !eligible {
		t.Fatal("expected eligible with no starting deadline configured")
	}

	// Now cap the deadline tight enough that the newest miss (12:35, the
	// last */5 firing before 12:37) is already 2 minutes stale relative to
	// "now" — well past a 1s deadline.
	spec.StartingDeadlineSeconds = int64ptr(1)
	later := suspendedAt.Add(37 * time.Minute)
	_, _, eligible2, _ := MissedRuns(spec, suspendedAt, later)
	if eligible2 {
		t.Fatal("expected ineligible once the newest miss is older than the deadline")
	}
}

func TestMissedRunsTruncatesAtCap(t *testing.T) {
	tz := "UTC"
	spec := batchv1.CronJobSpec{Schedule: "* * * * *", TimeZone: &tz} // every minute
	suspendedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := suspendedAt.Add(200 * time.Minute) // 200 missed firings, cap is 100

	count, truncated, _, reason := MissedRuns(spec, suspendedAt, now)
	if reason != "" {
		t.Fatalf("unexpected reason %q", reason)
	}
	if !truncated {
		t.Fatal("expected truncated=true past the missed-run cap")
	}
	if count != missedRunCountCap {
		t.Fatalf("count = %d, want the cap %d", count, missedRunCountCap)
	}
}

// A truncated count must still judge eligibility against the newest real
// firing: hourly at :00 with a 60s deadline has nothing eligible at :30.
func TestMissedRunsTruncatedEligibilityUsesLastRealFiring(t *testing.T) {
	tz := "UTC"
	spec := batchv1.CronJobSpec{Schedule: "0 * * * *", TimeZone: &tz, StartingDeadlineSeconds: int64ptr(60)}
	suspendedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	now := suspendedAt.Add(200*time.Hour + 30*time.Minute)
	_, truncated, eligible, _ := MissedRuns(spec, suspendedAt, now)
	if !truncated {
		t.Fatal("expected truncated=true past the missed-run cap")
	}
	if eligible {
		t.Fatal("eligible = true at :30 with a 60s deadline; the last firing was 30m ago")
	}

	now = suspendedAt.Add(200*time.Hour + 30*time.Second)
	if _, _, eligible, _ = MissedRuns(spec, suspendedAt, now); !eligible {
		t.Fatal("eligible = false 30s after a firing with a 60s deadline")
	}
}

func TestSuspendedDurationUnknownWithoutTimestamp(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if _, ok := SuspendedDuration(nil, now); ok {
		t.Fatal("expected ok=false for a nil suspendedAt")
	}
	at := now.Add(-3 * 24 * time.Hour)
	label, ok := SuspendedDuration(&at, now)
	if !ok || label != "3d" {
		t.Fatalf("label=%q ok=%v, want 3d/true", label, ok)
	}
}
