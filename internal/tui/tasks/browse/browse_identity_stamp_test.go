package browse

import (
	"testing"

	"github.com/kute-dev/kute/internal/kube"
)

// TestJobStampsCreditTheReviewedIdentity: §36b's run-now and §37c's rerun
// stamp the API server's reviewed username — read when the action is
// taken, since the review lands after browse is built — and fall back to
// the kubeconfig user name, then "kute". The SOURCE column's "manual · you"
// compares against the same answer, or a Job you just created would read as
// someone else's.
func TestJobStampsCreditTheReviewedIdentity(t *testing.T) {
	id := &fakeIdentity{}
	m := New(Config{Session: newSession(), CurrentUser: "eks-admin-entry", Identity: id})
	if got := m.cronJobTriggerCreator(); got != "eks-admin-entry" {
		t.Fatalf("before a review answers, run-now creator = %q, want the kubeconfig name", got)
	}

	id.id, id.ok = kube.ReviewedIdentity{Username: "arn:aws:iam::123:role/dev"}, true
	for name, got := range map[string]string{
		"run-now": m.cronJobTriggerCreator(),
		"rerun":   m.jobRerunCreator(),
		"source":  m.actingUser(),
	} {
		if got != "arn:aws:iam::123:role/dev" {
			t.Errorf("%s identity = %q, want the reviewed username", name, got)
		}
	}

	bare := New(Config{Session: newSession()})
	if got := bare.cronJobTriggerCreator(); got != "kute" {
		t.Fatalf("with no identity at all, creator = %q, want kute", got)
	}
}
