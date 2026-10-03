package kube

import (
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
)

// TestIsPermissionErrorClassification pins 4b's classification (mvp-plan.md
// Phase 4): a typed *apierrors.StatusError from a real API server response
// is preferred over the substring fallback, which still catches sources
// that don't return one (kube/fake's seeded errors, a wrapped message).
func TestIsPermissionErrorClassification(t *testing.T) {
	t.Parallel()
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: "", Resource: "secrets"},
		"",
		errors.New(`User "dev-readonly" cannot list resource "secrets" in namespace "nva-stage"`),
	)

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"typed forbidden", forbidden, true},
		{"substring forbidden", errors.New("secrets is forbidden: User cannot list"), true},
		{"substring permission", errors.New("permission denied"), true},
		{"unrelated error", errors.New("dial tcp: i/o timeout"), false},
		{"typed not found", apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "x"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsPermissionError(tt.err); got != tt.want {
				t.Errorf("IsPermissionError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestStreamPodLogsRequiresNamespace pins L13: an empty namespace is an
// error, never a silent "default" that streams the wrong pod.
func TestStreamPodLogsRequiresNamespace(t *testing.T) {
	t.Parallel()
	_, err := ClientPodLogStreamer{Client: fake.NewClientset()}.StreamPodLogs(t.Context(), LogStreamRequest{PodName: "api"})
	if err == nil {
		t.Fatal("expected an error for an empty namespace")
	}
}
