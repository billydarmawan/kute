package kube

import (
	"context"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AccessReviewResult is the API server's authoritative answer for the
// current authenticated identity. Unlike WhoCanResult, it is evaluated by
// the server's complete authorizer chain rather than inferred from cached
// RBAC bindings alone.
type AccessReviewResult struct {
	Allowed         bool
	Denied          bool
	Reason          string
	EvaluationError string
}

// CanI evaluates query with a live SelfSubjectAccessReview. It is a
// deliberate one-shot read issued only from an explicitly opened debug
// panel; it starts no informer and is never polled.
func (c *Cluster) CanI(ctx context.Context, query WhoCanQuery) (AccessReviewResult, error) {
	resource, subresource, _ := strings.Cut(query.Resource, "/")
	review, err := c.clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace:   query.Namespace,
				Verb:        query.Verb,
				Group:       query.Group,
				Resource:    resource,
				Subresource: subresource,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return AccessReviewResult{}, err
	}
	return AccessReviewResult{
		Allowed:         review.Status.Allowed,
		Denied:          review.Status.Denied,
		Reason:          review.Status.Reason,
		EvaluationError: review.Status.EvaluationError,
	}, nil
}
