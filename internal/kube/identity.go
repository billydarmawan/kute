// This file is the connect-time SelfSubjectReview: one Create of
// authentication.k8s.io/v1 SelfSubjectReview (v1beta1 when v1 isn't
// served), taken once alongside the server-version read in Start (which
// SwitchContext also calls) and cached on the Cluster — never repeated on a
// timer and never re-issued per screen open, the same one-shot class as
// capability.go's probe. It answers "who does the API server think I am",
// which kubeconfig can't for token, OIDC or exec-plugin auth (every managed
// cloud): there the local UserName is only the AuthInfo entry's name.
package kube

import (
	"context"
	"slices"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authenticationv1beta1 "k8s.io/api/authentication/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ReviewedIdentity is the API server's own answer to "who am I": the
// username and groups a SelfSubjectReview returned. Groups already include
// the server-added ones (system:authenticated, system:serviceaccounts…).
type ReviewedIdentity struct {
	Username string
	Groups   []string
}

// selfSubjectReviewTimeout bounds the connect-time review so an apiserver
// that accepts the request and never answers can't stall Start.
const selfSubjectReviewTimeout = 5 * time.Second

// ReviewedIdentity returns the identity the last Start/SwitchContext's
// SelfSubjectReview reported, and false when no review answered (not yet
// connected, a pre-1.27 server without the API, Forbidden, any error). A
// consumer then falls back to the kubeconfig-derived Context.UserName.
func (c *Cluster) ReviewedIdentity() (ReviewedIdentity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.identity == nil {
		return ReviewedIdentity{}, false
	}
	return ReviewedIdentity{Username: c.identity.Username, Groups: slices.Clone(c.identity.Groups)}, true
}

// probeSelfSubjectReview issues the one review and stores its answer if gen
// is still current (a superseded Start must not stamp the old cluster's
// identity onto the new context). Best-effort: any failure leaves the
// identity unset, which every consumer already handles.
func (c *Cluster) probeSelfSubjectReview(ctx context.Context, gen int, cs kubernetes.Interface) {
	ctx, cancel := context.WithTimeout(ctx, selfSubjectReviewTimeout)
	defer cancel()
	id, ok := selfSubjectReview(ctx, cs)
	if !ok {
		return
	}
	c.mu.Lock()
	if c.generation == gen {
		c.identity = &id
	}
	c.mu.Unlock()
}

// selfSubjectReview asks v1 first (GA in 1.28) and v1beta1 only when v1
// isn't served (NotFound) — a Forbidden or transport error from v1 says
// nothing v1beta1 would answer differently.
func selfSubjectReview(ctx context.Context, cs kubernetes.Interface) (ReviewedIdentity, bool) {
	v1, err := cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err == nil {
		return reviewedIdentity(v1.Status.UserInfo.Username, v1.Status.UserInfo.Groups)
	}
	if !apierrors.IsNotFound(err) {
		return ReviewedIdentity{}, false
	}
	beta, err := cs.AuthenticationV1beta1().SelfSubjectReviews().Create(ctx, &authenticationv1beta1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return ReviewedIdentity{}, false
	}
	return reviewedIdentity(beta.Status.UserInfo.Username, beta.Status.UserInfo.Groups)
}

// reviewedIdentity rejects an empty username: a review that names nobody
// is no better than the kubeconfig fallback.
func reviewedIdentity(username string, groups []string) (ReviewedIdentity, bool) {
	if username == "" {
		return ReviewedIdentity{}, false
	}
	return ReviewedIdentity{Username: username, Groups: slices.Clone(groups)}, true
}

// whoCanIdentity is the identity 22a pins as "(you)": the reviewed one when
// the server answered, else the kubeconfig-derived name, flagged when that
// name is only an AuthInfo entry (see markUnknownIdentity).
func (c *Cluster) whoCanIdentity() (user string, groups []string, fromAuthInfo bool) {
	if id, ok := c.ReviewedIdentity(); ok {
		return id.Username, id.Groups, false
	}
	kctx := c.ContextSnapshot()
	return kctx.UserName, kctx.UserGroups, kctx.UserFromAuthInfo
}
