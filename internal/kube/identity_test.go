package kube

import (
	"errors"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authenticationv1beta1 "k8s.io/api/authentication/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// selfSubjectReviewCreates counts SelfSubjectReview creates at version
// ("v1"/"v1beta1").
func selfSubjectReviewCreates(cs *fake.Clientset, version string) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "selfsubjectreviews" && a.GetResource().Version == version {
			n++
		}
	}
	return n
}

func answerSelfSubjectReview(cs *fake.Clientset, username string, groups ...string) {
	cs.PrependReactor("create", "selfsubjectreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Version != "v1" {
			return false, nil, nil
		}
		review := &authenticationv1.SelfSubjectReview{}
		review.Status.UserInfo.Username = username
		review.Status.UserInfo.Groups = groups
		return true, review, nil
	})
}

// TestStartIssuesExactlyOneSelfSubjectReview: the review is a connect-time
// one-shot, never re-issued when 22a resolves a query.
func TestStartIssuesExactlyOneSelfSubjectReview(t *testing.T) {
	c, cs := newLazyTestCluster()
	defer c.Stop()
	c.kctx = Context{UserName: "gke_proj_zone_c", UserFromAuthInfo: true}
	answerSelfSubjectReview(cs, "alice@example.com", "devs", "system:authenticated")

	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := selfSubjectReviewCreates(cs, "v1"); got != 1 {
		t.Fatalf("connect issued %d SelfSubjectReviews, want exactly 1", got)
	}

	for range 3 {
		result, err := c.WhoCan(t.Context(), WhoCanQuery{Verb: "list", Resource: "secrets", Namespace: "default"})
		if err != nil {
			t.Fatalf("WhoCan: %v", err)
		}
		if result.CurrentUser != "alice@example.com" || result.CurrentUserUnknown {
			t.Fatalf("pinned %q (unknown=%v), want the reviewed alice@example.com", result.CurrentUser, result.CurrentUserUnknown)
		}
	}
	if got := selfSubjectReviewCreates(cs, "v1"); got != 1 {
		t.Fatalf("who-can queries issued more reviews: %d total, want 1", got)
	}
	user, groups, fromAuthInfo := c.whoCanIdentity()
	if user != "alice@example.com" || fromAuthInfo || len(groups) != 2 || groups[0] != "devs" {
		t.Fatalf("whoCanIdentity = %q %v %v, want the reviewed user and groups", user, groups, fromAuthInfo)
	}
}

// TestSelfSubjectReviewFallsBackToV1beta1WhenV1IsNotServed.
func TestSelfSubjectReviewFallsBackToV1beta1WhenV1IsNotServed(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "selfsubjectreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Version == "v1" {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "authentication.k8s.io", Resource: "selfsubjectreviews"}, "")
		}
		review := &authenticationv1beta1.SelfSubjectReview{}
		review.Status.UserInfo.Username = "bob"
		return true, review, nil
	})
	id, ok := selfSubjectReview(t.Context(), cs)
	if !ok || id.Username != "bob" {
		t.Fatalf("got %+v ok=%v, want bob from v1beta1", id, ok)
	}
	if selfSubjectReviewCreates(cs, "v1beta1") != 1 {
		t.Fatal("expected one v1beta1 review after v1 answered NotFound")
	}
}

// TestFailedSelfSubjectReviewKeepsTheKubeconfigFallback: Forbidden (or any
// error) leaves who-can on the kubeconfig name, still marked unknown, and
// never retries v1beta1 for a non-NotFound answer.
func TestFailedSelfSubjectReviewKeepsTheKubeconfigFallback(t *testing.T) {
	for name, err := range map[string]error{
		"forbidden": apierrors.NewForbidden(schema.GroupResource{Group: "authentication.k8s.io", Resource: "selfsubjectreviews"}, "", errors.New("no")),
		"transport": errors.New("connection reset"),
	} {
		t.Run(name, func(t *testing.T) {
			c, cs := newLazyTestCluster()
			defer c.Stop()
			c.kctx = Context{UserName: "gke_proj_zone_c", UserFromAuthInfo: true}
			cs.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, err
			})
			if err := c.Start(t.Context()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if _, ok := c.ReviewedIdentity(); ok {
				t.Fatal("a failed review must leave the identity unset")
			}
			if n := selfSubjectReviewCreates(cs, "v1beta1"); n != 0 {
				t.Fatalf("%s from v1 retried v1beta1 %d times", name, n)
			}
			result, werr := c.WhoCan(t.Context(), WhoCanQuery{Verb: "list", Resource: "secrets", Namespace: "default"})
			if werr != nil {
				t.Fatalf("WhoCan: %v", werr)
			}
			if result.CurrentUser != "gke_proj_zone_c" || !result.CurrentUserUnknown {
				t.Fatalf("got %q unknown=%v, want the kubeconfig fallback marked unknown", result.CurrentUser, result.CurrentUserUnknown)
			}
		})
	}
}

// TestSupersededSelfSubjectReviewLeavesTheNewContextAlone.
func TestSupersededSelfSubjectReviewLeavesTheNewContextAlone(t *testing.T) {
	c, cs := newLazyTestCluster()
	defer c.Stop()
	cs.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		c.mu.Lock()
		c.generation++
		c.mu.Unlock()
		review := &authenticationv1.SelfSubjectReview{}
		review.Status.UserInfo.Username = "old-cluster-user"
		return true, review, nil
	})
	c.probeSelfSubjectReview(t.Context(), c.generation, cs)
	if id, ok := c.ReviewedIdentity(); ok {
		t.Fatalf("superseded review wrote %+v into the new context", id)
	}
}
