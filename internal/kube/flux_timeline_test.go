package kube

import (
	"testing"
	"time"
)

// TestFluxCommitSubjectParsesSourceControllerMessage. The subject lives
// only in this message — status.artifact carries no commit metadata and the
// fetched tarball excludes .git — so this parse is the single on-cluster
// source for it. Anchored on the message shape, not the event Reason, which
// has moved between NewArtifact and GitOperationSucceeded across versions.
func TestFluxCommitSubjectParsesSourceControllerMessage(t *testing.T) {
	t.Parallel()
	tests := []struct{ msg, want string }{
		{"stored artifact for commit 'fix: raise worker memory limit (#412)'", "fix: raise worker memory limit (#412)"},
		{"stored artifact for commit 'chore: bump'", "chore: bump"},
		{"stored artifact for commit 'openwebui bumped to 16.0.0'", "openwebui bumped to 16.0.0"},
		// Observed on a real cluster: source-controller substitutes the
		// revision when it has no subject. Echoing it back would print the
		// SHA twice on one row, the second time dressed as a commit message.
		{"stored artifact for commit 'master@sha1:efd398bed98a38348c7702355ecd98fc11ac2bef'", ""},
		{"stored artifact for commit 'sha256:d83a8a3354d98907a55beba524407a0b'", ""},
		// The steady-state message carries no subject and must yield none
		// rather than a plausible-looking fragment.
		{"no changes since last reconciliation: observed revision 'master@sha1:efd398be'", ""},
		{"Reconciliation finished in 1.4s, next run in 10m0s", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := FluxCommitSubject(tc.msg); got != tc.want {
			t.Errorf("FluxCommitSubject(%q) = %q, want %q", tc.msg, got, tc.want)
		}
	}
}

// TestTimelineFromFluxEventsReadsTheRevisionAnnotation pins §32a's core
// mechanism: the revision comes from the event's own annotation, which is
// data, rather than from its message, which is prose.
func TestTimelineFromFluxEventsReadsTheRevisionAnnotation(t *testing.T) {
	t.Parallel()
	now := time.Now()
	events := []Event{
		{
			Object: "Kustomization/nebula-workers", Namespace: "flux-system",
			Reason: "ReconciliationSucceeded", LastSeen: now,
			Message: "Reconciliation finished in 1.4s, next run in 10m0s",
			Annotations: map[string]string{
				"kustomize.toolkit.fluxcd.io/revision": "master@sha1:efd398bed98a38348c7702355ecd98fc11ac2bef",
			},
		},
		// No revision annotation: an ordinary event, not a revision row.
		{Object: "Pod/api-1", Namespace: "flux-system", Reason: "BackOff", LastSeen: now},
	}
	subjects := map[string]string{
		"master@sha1:efd398bed98a38348c7702355ecd98fc11ac2bef": "fix: raise worker memory limit (#412)",
	}

	got := TimelineFromFluxEvents(events, subjects)
	if len(got) != 1 {
		t.Fatalf("expected exactly one revision entry, got %d", len(got))
	}
	e := got[0]
	if e.Kind != TimelineRevision {
		t.Errorf("kind = %v, want TimelineRevision", e.Kind)
	}
	if e.GitRevision != "master@efd398b" {
		t.Errorf("GitRevision = %q, want %q", e.GitRevision, "master@efd398b")
	}
	if e.CommitSubject != "fix: raise worker memory limit (#412)" {
		t.Errorf("CommitSubject = %q", e.CommitSubject)
	}
}

// TestTimelineFromFluxEventsDegradesToTheBareSHA is the honest-degradation
// path: source-controller Events expire (~1h), so a revision whose subject
// was never seen or has aged out renders as the SHA alone. Never fabricated,
// and never fetched from a git remote.
func TestTimelineFromFluxEventsDegradesToTheBareSHA(t *testing.T) {
	t.Parallel()
	events := []Event{{
		Object: "Kustomization/nebula-workers", Namespace: "flux-system", LastSeen: time.Now(),
		Annotations: map[string]string{
			"kustomize.toolkit.fluxcd.io/revision": "master@sha1:efd398bed98a38348c7702355ecd98fc11ac2bef",
		},
	}}
	got := TimelineFromFluxEvents(events, nil)
	if len(got) != 1 {
		t.Fatalf("expected one entry, got %d", len(got))
	}
	if got[0].GitRevision != "master@efd398b" {
		t.Errorf("GitRevision = %q", got[0].GitRevision)
	}
	if got[0].CommitSubject != "" {
		t.Errorf("CommitSubject should be empty when no event carried one, got %q", got[0].CommitSubject)
	}
}

// TestFluxCommitSubjectsPairsSubjectToRevision covers the join: the
// "stored artifact" message names the subject but not the SHA, so the SHA
// comes from the source object's own artifact revision.
func TestFluxCommitSubjectsPairsSubjectToRevision(t *testing.T) {
	t.Parallel()
	events := []Event{
		{Object: "GitRepository/flux-system", Message: "stored artifact for commit 'fix: raise worker memory limit (#412)'"},
		{Object: "GitRepository/flux-system", Message: "no changes since last reconciliation: observed revision 'x'"},
	}
	revisionOf := func(object string) string {
		if object == "GitRepository/flux-system" {
			return "master@sha1:efd398be"
		}
		return ""
	}
	got := FluxCommitSubjects(events, revisionOf)
	if got["master@sha1:efd398be"] != "fix: raise worker memory limit (#412)" {
		t.Errorf("expected the subject paired to the source's revision, got %v", got)
	}
	if len(got) != 1 {
		t.Errorf("only the stored-artifact message should contribute, got %v", got)
	}
}

// TestFluxCommitSubjectsNeverPairsAnOlderCommitWithTheHead is M8: two
// commits fetched within the Event TTL both keyed on the source's current
// revision, so the head's row could carry the older commit's subject.
func TestFluxCommitSubjectsNeverPairsAnOlderCommitWithTheHead(t *testing.T) {
	t.Parallel()
	now := time.Now()
	head := "main@sha1:bbbbbbbb"
	revisionOf := func(string) string { return head }

	// Unannotated: only the newest stored-artifact event stands for the
	// current revision, in whatever order the events arrive.
	unannotated := []Event{
		{Object: "GitRepository/fleet", Message: "stored artifact for commit 'B: newer'", LastSeen: now},
		{Object: "GitRepository/fleet", Message: "stored artifact for commit 'A: older'", LastSeen: now.Add(-20 * time.Minute)},
	}
	if got := FluxCommitSubjects(unannotated, revisionOf); got[head] != "B: newer" || len(got) != 1 {
		t.Errorf("unannotated: got %v, want only the newest subject on the head revision", got)
	}

	// Annotated: each subject keys on its own revision.
	annotated := []Event{
		{Object: "GitRepository/fleet", Message: "stored artifact for commit 'B: newer'", LastSeen: now,
			Annotations: map[string]string{"source.toolkit.fluxcd.io/revision": head}},
		{Object: "GitRepository/fleet", Message: "stored artifact for commit 'A: older'", LastSeen: now.Add(-20 * time.Minute),
			Annotations: map[string]string{"source.toolkit.fluxcd.io/revision": "main@sha1:aaaaaaaa"}},
	}
	got := FluxCommitSubjects(annotated, revisionOf)
	if got[head] != "B: newer" || got["main@sha1:aaaaaaaa"] != "A: older" {
		t.Errorf("annotated: got %v, want each subject on its own revision", got)
	}
}
