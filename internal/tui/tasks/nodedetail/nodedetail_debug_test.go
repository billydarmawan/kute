package nodedetail

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

func TestWaitingPodRowOpensCopyDebugInsteadOfExec(t *testing.T) {
	p := schedPodWithContainers("default", "pending-0", "node-a", "worker")
	p.Status.Phase = corev1.PodPending
	p.Status.ContainerStatuses[0].Ready = false
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-a")},
		kube.KindPod:  {p},
	}}
	pickerCalled := false
	var gotPhase string
	m := New(Config{
		Session: newSession(), Lister: lister, NodeName: "node-a",
		OpenExec: func(string, string, []kube.ContainerInfo, map[string][]string, int, int) (tea.Model, tea.Cmd) {
			pickerCalled = true
			return sentinelTask{}, nil
		},
		OpenDebug: func(_, _ string, _ []kube.ContainerInfo, podPhase string, waiting bool, _, _ int) (tea.Model, tea.Cmd) {
			gotPhase = podPhase
			if !waiting {
				t.Error("waiting container state was not forwarded")
			}
			return sentinelTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected copy-debug panel, got %T", updated)
	}
	if cmd != nil {
		t.Fatal("terminal pod routing returned an exec command")
	}
	if pickerCalled {
		t.Fatal("execpicker opened for a terminal pod")
	}
	if gotPhase != "Pending" {
		t.Fatalf("pod phase = %q, want Pending", gotPhase)
	}
}
