package poddetail

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// podWithEphemeralContainer is runningPod plus one attached debug container
// — §41e's fixture.
func podWithEphemeralContainer(name, ns, node string) *corev1.Pod {
	pod := runningPod(name, ns, node)
	pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{
		{
			EphemeralContainerCommon: corev1.EphemeralContainerCommon{
				Name: "debugger-7xk2", Image: "nicolaka/netshoot",
			},
			TargetContainerName: "app",
		},
	}
	pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{
		{Name: "debugger-7xk2", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	}
	return pod
}

// TestEphemeralGroupHiddenWithoutOne pins 13d: no ephemeral containers, no
// group — the rendered view must never mention EPHEMERAL for an ordinary
// pod.
func TestEphemeralGroupHiddenWithoutOne(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {runningPod("api-0", "default", "node-a")},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if out := ansi.Strip(m.Render()); strings.Contains(out, "EPHEMERAL") {
		t.Fatalf("expected no EPHEMERAL group for a pod without one, got:\n%s", out)
	}
}

// TestEphemeralGroupRendersWhenPresent confirms the group appears with the
// attached container's name/target once at least one exists.
func TestEphemeralGroupRendersWhenPresent(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {podWithEphemeralContainer("api-0", "default", "node-a")},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	out := ansi.Strip(m.Render())
	if !strings.Contains(out, "EPHEMERAL") {
		t.Fatalf("expected an EPHEMERAL group, got:\n%s", out)
	}
	if !strings.Contains(out, "debugger-7xk2") || !strings.Contains(out, "app") {
		t.Fatalf("expected the ephemeral container's name and target in the group, got:\n%s", out)
	}
}

// TestContainerCursorExtendsIntoEphemeralGroup confirms j/k moves from the
// ordinary CONTAINERS grid into the EPHEMERAL one — one combined selection
// space (totalContainerRows), not two independent cursors.
func TestContainerCursorExtendsIntoEphemeralGroup(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {podWithEphemeralContainer("api-0", "default", "node-a")},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if m.totalContainerRows() != 2 {
		t.Fatalf("totalContainerRows = %d, want 2 (1 ordinary + 1 ephemeral)", m.totalContainerRows())
	}
	m.moveContainerSelection(1)
	eph, ok := m.selectedEphemeralContainer()
	if !ok || eph.Name != "debugger-7xk2" {
		t.Fatalf("expected the cursor to land on the ephemeral container, got %+v (ok=%v)", eph, ok)
	}
	// Clamped at the end, not wrapping.
	m.moveContainerSelection(1)
	if m.selectedContainer != 1 {
		t.Fatalf("selectedContainer = %d, want clamped to 1", m.selectedContainer)
	}
}

// TestEnterOnEphemeralRowReattaches confirms §41e's "↵ re-attach" — a
// kubectl attach (kube.AttachSpec) to the running ephemeral container.
func TestEnterOnEphemeralRowReattaches(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {podWithEphemeralContainer("api-0", "default", "node-a")},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())
	m.moveContainerSelection(1) // onto the ephemeral row

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "enter"})
	if _, ok := updated.(*Model); !ok {
		t.Fatalf("expected poddetail to stay the active task, got %T", updated)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil exec Cmd re-attaching to the ephemeral container")
	}
}

// TestEnterOnExitedEphemeralRowSaysSo pins L12: a Terminated ephemeral
// container can't be attached to, so ↵ explains that instead of handing
// kubectl a command that can only fail.
func TestEnterOnExitedEphemeralRowSaysSo(t *testing.T) {
	pod := podWithEphemeralContainer("api-0", "default", "node-a")
	pod.Status.EphemeralContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: {pod}}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())
	m.moveContainerSelection(1)

	if _, cmd := m.Update(tea.KeyPressMsg{Text: "enter"}); cmd != nil {
		t.Fatal("expected no kubectl handoff for an exited ephemeral container")
	}
	if !strings.Contains(m.execFeedback, "debugger-7xk2 has exited") {
		t.Fatalf("execFeedback = %q, want an exited note", m.execFeedback)
	}
}

// TestLogsOnEphemeralRowTargetsIt confirms 'l' on the ephemeral row opens
// logs scoped to that container, the same as any ordinary container row.
func TestLogsOnEphemeralRowTargetsIt(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {podWithEphemeralContainer("api-0", "default", "node-a")},
	}}
	var gotContainer string
	m := New(Config{
		Session: newSession(), Lister: lister,
		OpenLogs: func(_ kube.Pod, container string, _, _ int) (tea.Model, tea.Cmd) {
			gotContainer = container
			return sentinelTask{}, nil
		},
		Namespace: "default", Name: "api-0",
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())
	m.moveContainerSelection(1)

	updated, _ := m.Update(tea.KeyPressMsg{Text: "l"})
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected logs' sentinel task to be pushed, got %T", updated)
	}
	if gotContainer != "debugger-7xk2" {
		t.Fatalf("OpenLogs called with container %q, want debugger-7xk2", gotContainer)
	}
}
