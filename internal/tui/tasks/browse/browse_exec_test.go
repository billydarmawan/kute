package browse

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

func podWithContainers(ns, name string, containers ...string) *corev1.Pod {
	cs := make([]corev1.Container, len(containers))
	statuses := make([]corev1.ContainerStatus, len(containers))
	for i, c := range containers {
		cs[i] = corev1.Container{Name: c, Image: c + ":latest"}
		statuses[i] = corev1.ContainerStatus{Name: c, Ready: true}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{Containers: cs},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: statuses},
	}
}

// TestExecSingleContainerRunsDirectly confirms 'x' on a single-container pod
// execs immediately (docs/design README.md §10a: "skipped entirely for
// single-container pods") — no execpicker task is pushed, and the Cmd
// returned is the tea.ExecProcess wrapping kube.ExecSpec.
func TestExecSingleContainerRunsDirectly(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	openExecCalled := false
	m := New(Config{
		Session: newSession(), Lister: lister,
		OpenExec: func(string, string, []kube.ContainerInfo, map[string][]string, int, int) (tea.Model, tea.Cmd) {
			openExecCalled = true
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(*Model); !ok {
		t.Fatalf("expected browse to stay the active task for a single-container pod, got %T", updated)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil exec Cmd")
	}
	if openExecCalled {
		t.Fatal("OpenExec must not be called for a single-container pod")
	}
}

// TestExecMultiContainerPushesPicker confirms 'x' on a multi-container pod
// pushes tasks/execpicker via OpenExec instead of execing directly.
func TestExecMultiContainerPushesPicker(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {podWithContainers("default", "api-0", "app", "sidecar")},
	}}
	var gotContainers []kube.ContainerInfo
	m := New(Config{
		Session: newSession(), Lister: lister,
		OpenExec: func(ns, name string, containers []kube.ContainerInfo, _ map[string][]string, w, h int) (tea.Model, tea.Cmd) {
			gotContainers = containers
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected execpicker's stub task to be pushed, got %T", updated)
	}
	if len(gotContainers) != 2 {
		t.Fatalf("expected both containers handed to OpenExec, got %v", gotContainers)
	}
}

// TestExecSingleContainerDemoModeSkipsRealKubectl confirms 'x' on a
// single-container pod never builds a real kubectl exec subprocess when
// Config.Demo is set — it resolves synchronously to kube.ErrDemoUnavailable
// instead, since there's no real cluster behind kube/fake for a tty to
// attach to (kute-dev/kute issue: --demo pods other than the one
// multi-container fixture failed with a raw "connection refused" from a
// real kubectl exec attempt).
func TestExecSingleContainerDemoModeSkipsRealKubectl(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Demo: true})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	_, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	if cmd == nil {
		t.Fatal("expected a non-nil Cmd")
	}
	msg, ok := cmd().(execResultMsg)
	if !ok {
		t.Fatalf("expected execResultMsg, got %T", msg)
	}
	if !errors.Is(msg.err, kube.ErrDemoUnavailable) {
		t.Fatalf("expected kube.ErrDemoUnavailable, got %v", msg.err)
	}
}

// TestExecResultFeedbackSurfacesInKeybar confirms a non-zero direct-exec
// exit sets browse's own execFeedback, surfaced via Keybar's RightNote
// (docs/design README.md §10a: "feedback line on non-zero exit").
func TestExecResultFeedbackSurfacesInKeybar(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, execResultMsg{err: exitStatusError{}})
	kb := m.Keybar()
	if kb.RightNote == "" {
		t.Fatal("expected the exec-exit feedback in Keybar RightNote")
	}
}

type exitStatusError struct{}

func (exitStatusError) Error() string { return "exit status 127" }

// TestExecRefusedWhileOffline pins the 4a invariant for 'x': Exec is a
// Mutating verb, so a broken connection must refuse it outright rather than
// handing the tty to a kubectl that can only fail (docs/design README.md
// §52: "mutating actions disabled" while OFFLINE).
func TestExecRefusedWhileOffline(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	openExecCalled := false
	m := New(Config{
		Session: newSession(), Lister: lister,
		OpenExec: func(string, string, []kube.ContainerInfo, map[string][]string, int, int) (tea.Model, tea.Cmd) {
			openExecCalled = true
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "i/o timeout"})

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(*Model); !ok {
		t.Fatalf("expected browse to stay the active task, got %T", updated)
	}
	if cmd != nil {
		t.Error("'x' handed the tty to kubectl while offline")
	}
	if openExecCalled {
		t.Error("'x' pushed the exec picker while offline")
	}

	// Back online, the same key works again — the gate is the connection
	// state, not a latch.
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnConnected})
	if _, cmd = m.Update(tea.KeyPressMsg{Text: "x"}); cmd == nil {
		t.Error("'x' stayed refused after reconnecting")
	}
}
