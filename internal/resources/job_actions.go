// Package resources: §37c's shared rerun/replace preflight data helpers —
// mirrors cronjob_actions.go's own reasoning for living here rather than in
// either task package: browse's Jobs-list 'R' (§37a) and the pushed
// jobattempts screen's own 'R' (§37b/§37c) both need identical answers,
// and task packages can't import one another. Every function is pure:
// caller-supplied `now` where relevant, no cluster I/O, no clock reads.
//
// kube.NextRerunName (internal/kube/cronjob_naming.go) is §37c's name
// generator — it lives in kube, not here, because it shares dnsSafeTruncate
// with ManualJobName and needs no JobAttemptsSummary/Row awareness at all;
// this file only holds the two functions that actually read attempt data.
package resources

import "time"

// JobRerunChoice is §37c's create-vs-replace pick.
type JobRerunChoice int

const (
	// JobRerunCreate is the default, non-destructive choice: a new
	// "<name>-rerun-N" Job cloned from the failed one's template. Staged,
	// then executes at TierNone — staging the choice is itself the
	// confirmation, the same "stage first, TierNone commit" shape 36b's
	// run-now uses.
	JobRerunCreate JobRerunChoice = iota
	// JobRerunReplace deletes the original and recreates it under the same
	// name — genuinely destructive (the original's Status/events/attempt
	// history are gone), so choosing it in the staged list does not itself
	// confirm anything: committing still routes through actions.Controller's
	// ordinary TierInline/TierModal confirm (verbs.Resolve's JobReplace row,
	// the type-the-name modal in PROD) rather than executing directly.
	JobRerunReplace
)

// ClassifyJobFailure inspects summary's terminal state and returns §37c's
// amber diagnostic line — "" when nothing here is classifiable, the same
// "absence means no chrome" contract cronJobOverlapNote's own unknownOverlap
// case follows. Only ever renders when the prior failure has an
// identifiable, actionable cause; "the log said something" alone isn't
// enough to earn the amber strip.
func ClassifyJobFailure(summary JobAttemptsSummary) string {
	if !summary.Failed {
		return ""
	}
	if summary.ActiveDeadlineSeconds != nil && !summary.StartedAt.IsZero() && !summary.CompletedAt.IsZero() {
		ran := summary.CompletedAt.Sub(summary.StartedAt)
		deadline := time.Duration(*summary.ActiveDeadlineSeconds) * time.Second
		if ran >= deadline {
			return "it failed on the deadline"
		}
	}
	if summary.FailedAttempts > 0 && summary.FailedAttempts >= summary.BackoffLimit {
		if same, ok := SameFailureAcrossAttempts(summary.Attempts); ok && same {
			return "same failure every attempt — rerunning unchanged will very likely do the same"
		}
	}
	return ""
}

// SameFailureAcrossAttempts reports whether every terminal attempt in
// attempts shares the same (ExitCode) outcome — §37b's "same failure all
// three times" comparison line. ok is false (not enough data to have an
// opinion) when fewer than 2 terminal attempts are present — a Job whose
// earlier pods were already garbage-collected (this build has no retained
// ledger — see jobattempts.go's package doc comment) simply can't be
// compared, and that must read as "unknown", never as a false "yes, same".
func SameFailureAcrossAttempts(attempts []JobAttempt) (same, ok bool) {
	var codes []int32
	for _, a := range attempts {
		if a.Result != JobAttemptFailed {
			continue
		}
		if a.ExitCode == nil {
			// A failed attempt with no readable exit code can't be compared
			// either way — treat the whole comparison as unknown rather than
			// silently excluding it and reporting a false "same" off a
			// partial view.
			return false, false
		}
		codes = append(codes, *a.ExitCode)
	}
	if len(codes) < 2 {
		return false, false
	}
	first := codes[0]
	for _, c := range codes[1:] {
		if c != first {
			return false, true
		}
	}
	return true, true
}
