package poddetail

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

func multiContainerPod(name, ns, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: metav1.Now()},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{
				{Name: "app", Image: "example.com/app:v1"},
				{Name: "sidecar", Image: "example.com/sidecar:v1"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				{Name: "sidecar", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}
}

// TestExecSingleContainerRunsDirectly confirms 'x' execs immediately for a
// single-container pod (docs/design README.md §10a) without pushing
// execpicker.
func TestExecSingleContainerRunsDirectly(t *testing.T) {
	pod := runningPod("api-0", "default", "node-a")
	pod.Spec.InitContainers = []corev1.Container{{Name: "prepare", Image: "example.com/prepare:v1"}}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "prepare", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}},
	}}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod},
	}}
	openExecCalled := false
	m := New(Config{
		Session: newSession(), Lister: lister,
		OpenExec: func(string, string, []kube.ContainerInfo, int, int) (tea.Model, tea.Cmd) {
			openExecCalled = true
			return sentinelTask{}, nil
		},
		Namespace: "default", Name: "api-0",
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(*Model); !ok {
		t.Fatalf("expected poddetail to stay the active task for a single-container pod, got %T", updated)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil exec Cmd")
	}
	if openExecCalled {
		t.Fatal("OpenExec must not be called when the only additional container is a conventional init container")
	}
}

// TestExecMultiContainerPushesPicker confirms 'x' pushes tasks/execpicker
// via OpenExec for a multi-container pod.
func TestExecMultiContainerPushesPicker(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {multiContainerPod("api-0", "default", "node-a")},
	}}
	var gotContainers []kube.ContainerInfo
	m := New(Config{
		Session: newSession(), Lister: lister,
		OpenExec: func(ns, name string, containers []kube.ContainerInfo, w, h int) (tea.Model, tea.Cmd) {
			gotContainers = containers
			return sentinelTask{}, nil
		},
		Namespace: "default", Name: "api-0",
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected execpicker's sentinel task to be pushed, got %T", updated)
	}
	if len(gotContainers) != 2 {
		t.Fatalf("expected both containers handed to OpenExec, got %v", gotContainers)
	}
}

// TestDemoExecSkipsRealKubectl confirms --demo never hands the tty to a real
// kubectl from 5a — neither 'x' on a single-container pod nor ↵ on an
// ephemeral row — since there's no cluster behind kube/fake to attach to,
// only whatever context the kubeconfig happens to point at.
func TestDemoExecSkipsRealKubectl(t *testing.T) {
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		key  string
		down int
	}{
		{"x on a single container", runningPod("api-0", "default", "node-a"), "x", 0},
		{"enter on an ephemeral row", podWithEphemeralContainer("api-0", "default", "node-a"), "enter", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: {tc.pod}}}
			m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "api-0", Demo: true})
			m.SetSize(120, 40)
			m = step(t, m, m.Init()())
			m.moveContainerSelection(tc.down)

			_, cmd := m.Update(tea.KeyPressMsg{Text: tc.key})
			if cmd == nil {
				t.Fatal("expected a Cmd")
			}
			msg, ok := cmd().(execResultMsg)
			if !ok || !errors.Is(msg.err, kube.ErrDemoUnavailable) {
				t.Fatalf("expected execResultMsg{kube.ErrDemoUnavailable}, got %#v", cmd())
			}
		})
	}
}
