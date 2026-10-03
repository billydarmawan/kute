package execpicker

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
)

// distrolessModel is newModel with a third, shell-less "distroless"
// container appended, and every container's detection already resolved —
// mirrors TestShellsTextStates' fixture.
func distrolessModel(openDebug OpenDebugFunc) Model {
	m := New(Config{
		Session:   newModel().session,
		Namespace: "default",
		PodName:   "nva-gateway-2b81x",
		Containers: []kube.ContainerInfo{
			{Name: "gateway", Image: "nva-gateway:1.19.0", State: "Running"},
			{Name: "distroless", Image: "distroless/static", State: "Running"},
		},
		Shells:    stubShellDetector{},
		OpenDebug: openDebug,
	})
	m.detected = map[string]shellResult{
		"gateway":    {shells: []string{"bash", "sh"}},
		"distroless": {},
	}
	return m
}

// TestEnterOnShelllessRowOpensDebugPanel confirms §41a's fork: enter on a
// row already known to have no shell pushes the debug panel via OpenDebug
// instead of a dead exec attempt, naming the highlighted container as the
// attach target.
func TestEnterOnShelllessRowOpensDebugPanel(t *testing.T) {
	t.Parallel()
	var gotTarget string
	m := distrolessModel(func(_, _ string, _ []kube.ContainerInfo, target string, _, _ int) (tea.Model, tea.Cmd) {
		gotTarget = target
		return stubTask{}, nil
	})
	m.selected = 1 // "distroless"

	updated, _ := m.Update(tea.KeyPressMsg{Text: "enter"})
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected the debug panel's stub task to be pushed, got %T", updated)
	}
	if gotTarget != "distroless" {
		t.Fatalf("OpenDebug called with target %q, want distroless", gotTarget)
	}
}

// TestEnterOnShellfulRowStillExecs confirms the fork only applies to a
// genuinely shell-less row — a container with a shell still execs.
func TestEnterOnShellfulRowStillExecs(t *testing.T) {
	t.Parallel()
	debugCalled := false
	m := distrolessModel(func(string, string, []kube.ContainerInfo, string, int, int) (tea.Model, tea.Cmd) {
		debugCalled = true
		return stubTask{}, nil
	})
	m.selected = 0 // "gateway", has bash/sh

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "enter"})
	if _, ok := updated.(*Model); !ok {
		t.Fatalf("expected execpicker to stay the active task, got %T", updated)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil exec Cmd")
	}
	if debugCalled {
		t.Fatal("OpenDebug must not be called for a container with a shell")
	}
}

// TestKeybarLabelFollowsCursor pins docs/design v.0.11.0.dc.html §41a: "the
// ↵ label follows the cursor" — "debug <name>" on a shell-less row, "open
// shell" everywhere else.
func TestKeybarLabelFollowsCursor(t *testing.T) {
	t.Parallel()
	m := distrolessModel(nil)

	m.selected = 0
	if got := m.Keybar().Groups[0][0].Label; got != "open shell" {
		t.Errorf("enter label on a shell-ful row = %q, want %q", got, "open shell")
	}

	m.selected = 1
	if got := m.Keybar().Groups[0][0].Label; got != "debug distroless" {
		t.Errorf("enter label on a shell-less row = %q, want %q", got, "debug distroless")
	}
}

// TestShelllessRowWithoutOpenDebugShowsFeedback confirms enter on a
// shell-less row is a legible no-op (not a silent one) when OpenDebug isn't
// wired.
func TestShelllessRowWithoutOpenDebugShowsFeedback(t *testing.T) {
	t.Parallel()
	m := distrolessModel(nil)
	m.selected = 1

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "enter"})
	next, ok := updated.(*Model)
	if !ok {
		t.Fatalf("expected execpicker to stay the active task, got %T", updated)
	}
	if cmd != nil {
		t.Error("expected a nil Cmd when OpenDebug isn't wired")
	}
	if next.feedback == "" {
		t.Error("expected a feedback line explaining why enter did nothing")
	}
}

type stubTask struct{}

func (stubTask) Init() tea.Cmd                       { return nil }
func (stubTask) Update(tea.Msg) (tea.Model, tea.Cmd) { return stubTask{}, nil }
func (stubTask) View() tea.View                      { return tea.NewView("") }

type recordingShellDetector struct{ asked []string }

func (d *recordingShellDetector) DetectShells(_ context.Context, _, _, container string) ([]string, error) {
	d.asked = append(d.asked, container)
	return []string{"sh"}, nil
}

// TestProbedContainersAreNotProbedAgain pins L2: a container the opener's
// own 'x' pre-check already probed renders that result at once, and Init
// probes only the containers it has no answer for.
func TestProbedContainersAreNotProbedAgain(t *testing.T) {
	t.Parallel()
	detector := &recordingShellDetector{}
	m := New(Config{
		Session:   newModel().session,
		Namespace: "default",
		PodName:   "nva-gateway-2b81x",
		Containers: []kube.ContainerInfo{
			{Name: "gateway", State: "Running"},
			{Name: "distroless", State: "Running"},
			{Name: "sidecar", State: "Running"},
		},
		Shells: detector,
		Probed: map[string][]string{"gateway": {"bash", "sh"}, "distroless": nil},
	})
	if got := m.preferredShell(0); got != "bash" {
		t.Fatalf("preferredShell(gateway) = %q, want bash from the pre-check", got)
	}
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("expected a probe for the unprobed sidecar")
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			c()
		}
	}
	if len(detector.asked) != 1 || detector.asked[0] != "sidecar" {
		t.Fatalf("probed %v, want only [sidecar]", detector.asked)
	}
}
