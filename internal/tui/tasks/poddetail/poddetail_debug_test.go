package poddetail

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// fakeShellDetector answers DetectShells from a fixed per-container map —
// missing entries report a real "no shell" (nil, nil), matching
// kube.DetectShells' own contract. Mirrors browse's own fakeShellDetector.
type fakeShellDetector map[string][]string

func (f fakeShellDetector) DetectShells(_ context.Context, _, _, container string) ([]string, error) {
	return f[container], nil
}

type countingShellDetector struct{ calls int }

func (f *countingShellDetector) DetectShells(context.Context, string, string, string) ([]string, error) {
	f.calls++
	return []string{"sh"}, nil
}

// TestFailedJobPodSkipsShellProbeAndOpensCopyDebug pins the path reached by
// Enter on a failed Job attempt: x must not run the kubectl exec shell probe
// against the terminal pod.
func TestFailedJobPodSkipsShellProbeAndOpensCopyDebug(t *testing.T) {
	p := runningPod("batch-0", "default", "node-a")
	p.Status.Phase = corev1.PodFailed
	p.Status.ContainerStatuses[0].Ready = false
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: {p}}}
	detector := &countingShellDetector{}
	pickerCalled := false
	debugCalls := 0
	var gotPhase string
	m := New(Config{
		Session: newSession(), Lister: lister, Shells: detector,
		OpenExec: func(string, string, []kube.ContainerInfo, map[string][]string, int, int) (tea.Model, tea.Cmd) {
			pickerCalled = true
			return sentinelTask{}, nil
		},
		OpenDebug: func(_, _ string, _ []kube.ContainerInfo, podPhase string, waiting bool, _, _ int) (tea.Model, tea.Cmd) {
			debugCalls++
			gotPhase = podPhase
			if waiting {
				t.Error("terminal container unexpectedly reported Waiting")
			}
			return sentinelTask{}, nil
		},
		Namespace: "default", Name: "batch-0",
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected copy-debug panel, got %T", updated)
	}
	if cmd != nil {
		t.Fatal("terminal pod routing returned a shell-probe/exec command")
	}
	if detector.calls != 0 {
		t.Fatalf("DetectShells calls = %d, want 0", detector.calls)
	}
	if pickerCalled {
		t.Fatal("execpicker opened for a terminal pod")
	}
	if gotPhase != "Failed" {
		t.Fatalf("pod phase = %q, want Failed", gotPhase)
	}
	updated, cmd = m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(sentinelTask); !ok || cmd != nil {
		t.Fatalf("expected the restored pod detail to reopen copy-debug, got %T with cmd %v", updated, cmd)
	}
	if debugCalls != 2 {
		t.Fatalf("OpenDebug calls = %d, want 2", debugCalls)
	}
}

// TestDistrolessSingleContainerRoutesToDebugPanel confirms §41a's fork on
// poddetail: a single-container pod with no shell in it, once Shells is
// wired, routes to the debug panel instead of execing blind.
func TestDistrolessSingleContainerRoutesToDebugPanel(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {runningPod("api-0", "default", "node-a")},
	}}
	var gotName string
	m := New(Config{
		Session: newSession(), Lister: lister,
		Shells: fakeShellDetector{},
		OpenDebug: func(_ string, name string, _ []kube.ContainerInfo, _ string, _ bool, _, _ int) (tea.Model, tea.Cmd) {
			gotName = name
			return sentinelTask{}, nil
		},
		Namespace: "default", Name: "api-0",
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(*Model); !ok {
		t.Fatalf("expected poddetail to stay active during the async probe, got %T", updated)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil probe Cmd")
	}
	updated, _ = m.Update(cmd())
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected the debug panel's sentinel task to be pushed, got %T", updated)
	}
	if gotName != "api-0" {
		t.Fatalf("OpenDebug called with name %q, want api-0", gotName)
	}
}

// TestAllShelllessMultiContainerSkipsPicker mirrors browse's own test of
// the same name: the picker is never pushed when every container is
// shell-less.
func TestAllShelllessMultiContainerSkipsPicker(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {multiContainerPod("api-0", "default", "node-a")},
	}}
	pickerCalled := false
	m := New(Config{
		Session: newSession(), Lister: lister,
		Shells: fakeShellDetector{},
		OpenExec: func(string, string, []kube.ContainerInfo, map[string][]string, int, int) (tea.Model, tea.Cmd) {
			pickerCalled = true
			return sentinelTask{}, nil
		},
		OpenDebug: func(string, string, []kube.ContainerInfo, string, bool, int, int) (tea.Model, tea.Cmd) {
			return sentinelTask{}, nil
		},
		Namespace: "default", Name: "api-0",
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	_, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	updated, _ := m.Update(cmd())
	if pickerCalled {
		t.Error("execpicker must not be pushed when every container is shell-less")
	}
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected the debug panel's sentinel task to be pushed, got %T", updated)
	}
}

// TestRepeatXWhileProbingIsIgnored mirrors browse's own test of the same
// name: a repeat 'x' during the shell probe starts no second probe.
func TestRepeatXWhileProbingIsIgnored(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {runningPod("api-0", "default", "node-a")},
	}}
	detector := &countingShellDetector{}
	m := New(Config{
		Session: newSession(), Lister: lister, Shells: detector,
		Namespace: "default", Name: "api-0",
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	_, probe := m.Update(tea.KeyPressMsg{Text: "x"})
	if probe == nil {
		t.Fatal("expected a probe Cmd")
	}
	if m.execFeedback == "" {
		t.Fatal("expected a probing note on the keybar")
	}
	if _, again := m.Update(tea.KeyPressMsg{Text: "x"}); again != nil {
		t.Fatal("a repeat x during the probe started a second probe")
	}
	m.Update(probe())
	if detector.calls != 1 {
		t.Fatalf("DetectShells calls = %d, want 1", detector.calls)
	}
	if m.execFeedback != "" {
		t.Fatalf("probing note left on the keybar: %q", m.execFeedback)
	}
}
