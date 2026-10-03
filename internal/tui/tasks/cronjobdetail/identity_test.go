package cronjobdetail

import (
	"testing"

	"github.com/kute-dev/kute/internal/kube"
)

type fakeIdentity struct {
	id kube.ReviewedIdentity
	ok bool
}

func (f *fakeIdentity) ReviewedIdentity() (kube.ReviewedIdentity, bool) { return f.id, f.ok }

// TestRunNowCreditsTheReviewedIdentity mirrors browse's own: the reviewed
// username read at the moment of the run-now, else the kubeconfig name,
// else "kute".
func TestRunNowCreditsTheReviewedIdentity(t *testing.T) {
	id := &fakeIdentity{}
	m := New(Config{CurrentUser: "eks-admin-entry", Identity: id})
	if got := m.cronJobTriggerCreator(); got != "eks-admin-entry" {
		t.Fatalf("before a review answers, creator = %q, want the kubeconfig name", got)
	}
	id.id, id.ok = kube.ReviewedIdentity{Username: "arn:aws:iam::123:role/dev"}, true
	if got := m.cronJobTriggerCreator(); got != "arn:aws:iam::123:role/dev" {
		t.Fatalf("creator = %q, want the reviewed username", got)
	}
	if got := New(Config{}).cronJobTriggerCreator(); got != "kute" {
		t.Fatalf("with no identity at all, creator = %q, want kute", got)
	}
}
