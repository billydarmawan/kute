package nodedetail

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// schedPodWithContainers is schedPod without the resource requests but with
// named containers, for the single- vs multi-container exec branching.
func schedPodWithContainers(ns, name, node string, containers ...string) *corev1.Pod {
	cs := make([]corev1.Container, len(containers))
	statuses := make([]corev1.ContainerStatus, len(containers))
	for i, c := range containers {
		cs[i] = corev1.Container{Name: c, Image: c + ":latest"}
		statuses[i] = corev1.ContainerStatus{Name: c, Ready: true}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{NodeName: node, Containers: cs},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: statuses},
	}
}

// TestExecSingleContainerRunsDirectly confirms 'x' on a single-container pod
// row execs immediately (docs/design README.md §10a: "skipped entirely for
// single-container pods") — no execpicker task is pushed, and the Cmd
// returned is the tea.ExecProcess wrapping kube.ExecSpec. Mirrors browse's
// test of the same name.
func TestExecSingleContainerRunsDirectly(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-a")},
		kube.KindPod:  {schedPodWithContainers("default", "api-0", "node-a", "app")},
	}}
	openExecCalled := false
	m := New(Config{
		Session: newSession(), Lister: lister, NodeName: "node-a",
		OpenExec: func(string, string, []kube.ContainerInfo, map[string][]string, int, int) (tea.Model, tea.Cmd) {
			openExecCalled = true
			return sentinelTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(*Model); !ok {
		t.Fatalf("expected nodedetail to stay the active task for a single-container pod, got %T", updated)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil exec Cmd")
	}
	if openExecCalled {
		t.Fatal("OpenExec must not be called for a single-container pod")
	}
}

// TestExecMultiContainerPushesPicker confirms 'x' on a multi-container pod
// row pushes tasks/execpicker via OpenExec instead of execing directly.
func TestExecMultiContainerPushesPicker(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-a")},
		kube.KindPod:  {schedPodWithContainers("default", "api-0", "node-a", "app", "sidecar")},
	}}
	var gotContainers []kube.ContainerInfo
	m := New(Config{
		Session: newSession(), Lister: lister, NodeName: "node-a",
		OpenExec: func(ns, name string, containers []kube.ContainerInfo, _ map[string][]string, w, h int) (tea.Model, tea.Cmd) {
			gotContainers = containers
			return sentinelTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected execpicker's sentinel task to be pushed, got %T", updated)
	}
	if len(gotContainers) != 2 {
		t.Fatalf("expected both containers handed to OpenExec, got %v", gotContainers)
	}
}

// TestOpenLogsHandoff confirms 'l' on a pod row hands off to the log-stream
// task with the selected row's pod — no poddetail detour.
func TestOpenLogsHandoff(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-a")},
		kube.KindPod:  {schedPod("default", "big", "node-a", "1Gi")},
	}}
	var openedPod kube.Pod
	openLogs := func(pod kube.Pod, _ string, _, _ int) (tea.Model, tea.Cmd) {
		openedPod = pod
		return sentinelTask{}, nil
	}
	m := New(Config{Session: newSession(), Lister: lister, NodeName: "node-a", OpenLogs: openLogs})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "l"})
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected 'l' to hand off to the logs task, got %T", updated)
	}
	if openedPod.Name != "big" || openedPod.Namespace != "default" {
		t.Fatalf("openLogs called with %s/%s, want default/big", openedPod.Namespace, openedPod.Name)
	}
}

// TestExecResultFeedbackSurfacesInKeybar confirms a non-zero direct-exec
// exit sets nodedetail's own execFeedback, surfaced via Keybar's RightNote
// (docs/design README.md §10a: "feedback line on non-zero exit").
func TestExecResultFeedbackSurfacesInKeybar(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-a")},
		kube.KindPod:  {schedPodWithContainers("default", "api-0", "node-a", "app")},
	}}
	m := New(Config{Session: newSession(), Lister: lister, NodeName: "node-a"})
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

// TestNodeDebugKeyPushesPanel confirms 's' pushes tasks/debugpanel (§41d)
// for the node itself via OpenNodeDebug — replaces the retired NodeShell
// verb's direct tea.ExecProcess launch (see verbs.go's NodeDebug/
// NodeDebugDetail doc comments); the panel itself owns the launch and its
// exit feedback now, so nodedetail's own execFeedback channel no longer
// carries a node-debug result.
func TestNodeDebugKeyPushesPanel(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-a")},
	}}
	var gotName string
	m := New(Config{
		Session: newSession(), Lister: lister, NodeName: "node-a",
		OpenNodeDebug: func(name string, _, _, _ int) (tea.Model, tea.Cmd) {
			gotName = name
			return sentinelTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "s"})
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected debugpanel's sentinel task to be pushed, got %T", updated)
	}
	if gotName != "node-a" {
		t.Fatalf("OpenNodeDebug called with name %q, want node-a", gotName)
	}
}

// The 11b twin of browse's TestNodeDebugExplainsItselfOnFargate
// (kube.NodeShellUnavailable): on GKE Autopilot the privileged host-namespace
// container kubectl debug needs is rejected by admission, so 's' says why
// instead of pushing a panel for a command that can't work.
func TestNodeDebugExplainsItselfOnAutopilot(t *testing.T) {
	node := testNode("gk3-autopilot-cluster-default-pool-bcd71fbe-6qh9")
	node.Labels = map[string]string{"cloud.google.com/gke-nodepool": "default-pool"}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {node},
	}}
	m := New(Config{
		Session: newSession(), Lister: lister,
		NodeName: "gk3-autopilot-cluster-default-pool-bcd71fbe-6qh9",
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "s"})
	if cmd != nil {
		t.Error("'s' ran kubectl debug against an Autopilot node")
	}
	m = *updated.(*Model)
	if note := m.Keybar().RightNote; !strings.Contains(note, "GKE Autopilot") {
		t.Errorf("Keybar RightNote = %q, want it to name GKE Autopilot", note)
	}
}
