package browse

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
// kube.DetectShells' own contract.
type fakeShellDetector map[string][]string

func (f fakeShellDetector) DetectShells(_ context.Context, _, _, container string) ([]string, error) {
	return f[container], nil
}

type countingShellDetector struct{ calls int }

func (f *countingShellDetector) DetectShells(context.Context, string, string, string) ([]string, error) {
	f.calls++
	return []string{"sh"}, nil
}

// TestFailedPodSkipsShellProbeAndOpensCopyDebug is the failed-Job
// regression: terminal state must win before DetectShells, whose kubectl
// exec probe cannot run against a completed pod.
func TestFailedPodSkipsShellProbeAndOpensCopyDebug(t *testing.T) {
	p := podWithContainers("default", "batch-0", "worker")
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
			return stubTask{}, nil
		},
		OpenDebug: func(_, _ string, _ []kube.ContainerInfo, podPhase string, waiting bool, _, _ int) (tea.Model, tea.Cmd) {
			debugCalls++
			gotPhase = podPhase
			if waiting {
				t.Error("terminal container unexpectedly reported Waiting")
			}
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(stubTask); !ok {
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
	if _, ok := updated.(stubTask); !ok || cmd != nil {
		t.Fatalf("expected the restored Pods view to reopen copy-debug, got %T with cmd %v", updated, cmd)
	}
	if debugCalls != 2 {
		t.Fatalf("OpenDebug calls = %d, want 2", debugCalls)
	}
}

// TestDistrolessSingleContainerRoutesToDebugPanel confirms §41a's fork: a
// single-container pod with no shell in it, once Shells is wired, routes to
// the debug panel instead of execing blind (the old behavior, still exact
// for a container that *has* a shell — TestExecSingleContainerRunsDirectly
// covers that unchanged fallback for when Shells isn't wired at all).
func TestDistrolessSingleContainerRoutesToDebugPanel(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {podWithContainers("default", "api-0", "app")},
	}}
	var gotName string
	m := New(Config{
		Session: newSession(), Lister: lister,
		Shells: fakeShellDetector{}, // every container answers "no shell"
		OpenDebug: func(_ string, name string, _ []kube.ContainerInfo, _ string, _ bool, _, _ int) (tea.Model, tea.Cmd) {
			gotName = name
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(*Model); !ok {
		t.Fatalf("expected browse to stay active during the async probe, got %T", updated)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil probe Cmd")
	}
	updated, _ = m.Update(cmd())
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected the debug panel's stub task to be pushed, got %T", updated)
	}
	if gotName != "api-0" {
		t.Fatalf("OpenDebug called with name %q, want api-0", gotName)
	}
}

// TestShellfulSingleContainerStillExecsDirectly confirms a container that
// does have a shell still execs immediately once the probe comes back, the
// same fast path as the Shells-unwired fallback.
func TestShellfulSingleContainerStillExecsDirectly(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {podWithContainers("default", "api-0", "app")},
	}}
	debugCalled := false
	m := New(Config{
		Session: newSession(), Lister: lister,
		Shells: fakeShellDetector{"app": {"bash"}},
		OpenDebug: func(string, string, []kube.ContainerInfo, string, bool, int, int) (tea.Model, tea.Cmd) {
			debugCalled = true
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	_, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	if cmd == nil {
		t.Fatal("expected a non-nil probe Cmd")
	}
	updated, execCmd := m.Update(cmd())
	if _, ok := updated.(*Model); !ok {
		t.Fatalf("expected browse to stay the active task for a shell-ful container, got %T", updated)
	}
	if execCmd == nil {
		t.Fatal("expected a non-nil exec Cmd")
	}
	if debugCalled {
		t.Fatal("OpenDebug must not be called for a container with a shell")
	}
}

// TestAllShelllessMultiContainerSkipsPicker confirms the picker is never
// pushed when every container is shell-less — the debug panel opens
// directly (docs/design v.0.11.0.dc.html §41a: "a picker with nothing to
// pick is chrome").
func TestAllShelllessMultiContainerSkipsPicker(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {podWithContainers("default", "api-0", "app", "sidecar")},
	}}
	pickerCalled := false
	m := New(Config{
		Session: newSession(), Lister: lister,
		Shells: fakeShellDetector{},
		OpenExec: func(string, string, []kube.ContainerInfo, map[string][]string, int, int) (tea.Model, tea.Cmd) {
			pickerCalled = true
			return stubTask{}, nil
		},
		OpenDebug: func(string, string, []kube.ContainerInfo, string, bool, int, int) (tea.Model, tea.Cmd) {
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	_, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	updated, _ := m.Update(cmd())
	if pickerCalled {
		t.Error("execpicker must not be pushed when every container is shell-less")
	}
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected the debug panel's stub task to be pushed, got %T", updated)
	}
}

// TestMixedShellsStillPushesPicker confirms a pod with at least one shell
// somewhere still gets the ordinary picker treatment.
func TestMixedShellsStillPushesPicker(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {podWithContainers("default", "api-0", "app", "sidecar")},
	}}
	var gotContainers []kube.ContainerInfo
	var gotProbed map[string][]string
	m := New(Config{
		Session: newSession(), Lister: lister,
		Shells: fakeShellDetector{"sidecar": {"sh"}},
		OpenExec: func(_, _ string, containers []kube.ContainerInfo, probed map[string][]string, _, _ int) (tea.Model, tea.Cmd) {
			gotContainers = containers
			gotProbed = probed
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	_, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	updated, _ := m.Update(cmd())
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected execpicker's stub task to be pushed, got %T", updated)
	}
	if len(gotContainers) != 2 {
		t.Fatalf("expected both containers handed to OpenExec, got %v", gotContainers)
	}
	// The picker reuses these instead of probing every container again.
	if shells, ok := gotProbed["app"]; !ok || len(shells) != 0 {
		t.Fatalf("probed[app] = %v (present %v), want a known empty result", shells, ok)
	}
	if got := gotProbed["sidecar"]; len(got) != 1 || got[0] != "sh" {
		t.Fatalf("probed[sidecar] = %v, want [sh]", got)
	}
}

// TestDebugPanelDefaultModeFromCrashLoop confirms routePodShellsProbed
// passes the actual Running phase plus the Waiting state that makes the
// debug panel default to copy mode.
func TestDebugPanelDefaultModeFromCrashLoop(t *testing.T) {
	crashPod := podWithContainers("default", "worker-0", "app")
	crashPod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {crashPod},
	}}
	var gotPhase string
	var gotWaiting bool
	m := New(Config{
		Session: newSession(), Lister: lister,
		Shells: fakeShellDetector{},
		OpenDebug: func(_, _ string, _ []kube.ContainerInfo, podPhase string, waiting bool, _, _ int) (tea.Model, tea.Cmd) {
			gotPhase, gotWaiting = podPhase, waiting
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected crash-looping pod to open debug directly, got %T", updated)
	}
	if cmd != nil {
		t.Fatal("crash-looping pod returned a shell-probe command")
	}
	if gotPhase != "Running" || !gotWaiting {
		t.Fatalf("pod state = (%q, %v), want (Running, true)", gotPhase, gotWaiting)
	}
}

// TestDebugCopyTagRendersInPodsTable confirms §41c's "the copy also carries
// a ⚑ debug copy tag in the pods table for the rest of the session": a pod
// that Session.DebugCopies recognizes as kute's own copy-mode pod gets the
// NameSuffix tag on load, and an ordinary pod in the same list doesn't.
func TestDebugCopyTagRendersInPodsTable(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			pod("default", "worker-0"),
			pod("default", "worker-0-debug"),
		},
	}}
	session := newSession()
	session.DebugCopies = kube.NewDebugCopyRegistry()
	session.DebugCopies.Add(session.Location.Context, "default", "worker-0-debug")

	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	var gotPlain, gotDebug bool
	for _, row := range m.rows {
		switch row.Name {
		case "worker-0":
			gotPlain = true
			if row.NameSuffix != "" {
				t.Fatalf("worker-0 NameSuffix = %q, want empty — not a debug copy", row.NameSuffix)
			}
		case "worker-0-debug":
			gotDebug = true
			if row.NameSuffix != " ⚑ debug copy" {
				t.Fatalf("worker-0-debug NameSuffix = %q, want %q", row.NameSuffix, " ⚑ debug copy")
			}
		}
	}
	if !gotPlain || !gotDebug {
		t.Fatalf("expected both rows loaded, got rows=%+v", m.rows)
	}
}

// TestDebugCopyTagCoexistsWithEphemeralTag confirms a pod carrying both
// §41e's real ephemeral-container tag and §41c's client-side debug-copy fact
// gets both, without repeating the ⚑ glyph.
func TestDebugCopyTagCoexistsWithEphemeralTag(t *testing.T) {
	p := pod("default", "worker-0-debug")
	p.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: "debugger"}}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {p},
	}}
	session := newSession()
	session.DebugCopies = kube.NewDebugCopyRegistry()
	session.DebugCopies.Add(session.Location.Context, "default", "worker-0-debug")

	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(m.rows))
	}
	if got, want := m.rows[0].NameSuffix, " ⚑ · debug copy"; got != want {
		t.Fatalf("NameSuffix = %q, want %q", got, want)
	}
}

// TestRepeatXWhileProbingIsIgnored pins L1: a second 'x' while the first
// press's shell probe is still in flight must not start a second probe (and
// so a second kubectl exec once the first shell exits), and the keybar says
// the probe is running.
func TestRepeatXWhileProbingIsIgnored(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {podWithContainers("default", "api-0", "app")},
	}}
	detector := &countingShellDetector{}
	m := New(Config{Session: newSession(), Lister: lister, Shells: detector})
	m.SetSize(120, 36)
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
	_, execCmd := m.Update(probe())
	if execCmd == nil {
		t.Fatal("expected the probe result to exec")
	}
	if detector.calls != 1 {
		t.Fatalf("DetectShells calls = %d, want 1", detector.calls)
	}
	if m.execFeedback != "" {
		t.Fatalf("probing note left on the keybar: %q", m.execFeedback)
	}
	if _, next := m.Update(tea.KeyPressMsg{Text: "x"}); next == nil {
		t.Fatal("x after the probe finished should probe again")
	}
}

// TestSingleContainerExecRunsDetectedShell pins L3: the single-container
// fast path runs the shell the probe found, so an image with bash but no sh
// still execs, and falls back to ExecSpec's bash-then-sh wrapper only when
// the probe couldn't run.
func TestSingleContainerExecRunsDetectedShell(t *testing.T) {
	found := podShellsProbedMsg{results: []podShellDetection{{container: "app", shells: []string{"bash"}}}}
	if got := found.firstShell(); got != "bash" {
		t.Fatalf("firstShell = %q, want bash", got)
	}
	failed := podShellsProbedMsg{results: []podShellDetection{{container: "app", err: context.DeadlineExceeded}}}
	if got := failed.firstShell(); got != "" {
		t.Fatalf("firstShell after a failed probe = %q, want the fallback", got)
	}
}
