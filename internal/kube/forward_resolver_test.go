package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestResolveForwardPodSkipsTerminatingAndUnrunnablePods pins 13a's
// Service/Deployment backing-pod choice to the pods ForwardPodAvailable
// accepts: a terminating pod (still phase Running in its grace period) is
// never picked, a Ready pod beats a merely Running one, and with nothing
// Running the resolver errors instead of handing back a Pending pod.
func TestResolveForwardPodSkipsTerminatingAndUnrunnablePods(t *testing.T) {
	t.Parallel()
	sel := map[string]string{"app": "web"}
	pod := func(name string, phase corev1.PodPhase, ready, terminating bool) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: sel},
			Status:     corev1.PodStatus{Phase: phase},
		}
		if ready {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		if terminating {
			now := metav1.Now()
			p.DeletionTimestamp = &now
		}
		return p
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Selector: sel},
	}
	target := ForwardTarget{Kind: KindService, Namespace: "default", Name: "web"}

	r := NewClientsetPodResolver(fake.NewSimpleClientset(svc,
		pod("a-dying", corev1.PodRunning, true, true),
		pod("b-running", corev1.PodRunning, false, false),
		pod("c-ready", corev1.PodRunning, true, false),
	))
	if got, err := r.ResolveForwardPod(t.Context(), target); err != nil || got != "c-ready" {
		t.Fatalf("ResolveForwardPod = %q, %v; want c-ready", got, err)
	}

	r = NewClientsetPodResolver(fake.NewSimpleClientset(svc,
		pod("a-dying", corev1.PodRunning, true, true),
		pod("b-pending", corev1.PodPending, false, false),
	))
	if got, err := r.ResolveForwardPod(t.Context(), target); err == nil {
		t.Fatalf("ResolveForwardPod = %q, want an error with no usable pod", got)
	}
}
