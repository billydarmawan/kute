package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/state"
	"github.com/kute-dev/kute/internal/testutil/testenv"
)

// stubProbeContexts swaps the probeContexts seam for the test's lifetime.
// Not parallel-safe: it's a package global.
func stubProbeContexts(t *testing.T, fn func(context.Context, []string) <-chan kube.ProbeResult) {
	t.Helper()
	prev := probeContexts
	probeContexts = fn
	t.Cleanup(func() { probeContexts = prev })
}

// newProbeTestModel builds a root model over reprobeTestKubeconfig without
// opening the palette, so the caller can time the open itself.
func newProbeTestModel(t *testing.T) Model {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(reprobeTestKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	testenv.SetHome(t, t.TempDir())
	m := NewWithSession(reprobeTestScreen{}, &Session{
		Theme:    Dark(),
		Location: Location{Context: "dev"},
		State:    state.State{PerContext: map[string]state.PerContext{}},
	})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	return updated.(Model)
}

// updateWithin runs m.Update(msg) and fails if it doesn't return promptly —
// the update loop must never wait on a probe.
func updateWithin(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	type res struct {
		m   Model
		cmd tea.Cmd
	}
	done := make(chan res, 1)
	go func() {
		updated, cmd := m.Update(msg)
		done <- res{updated.(Model), cmd}
	}()
	select {
	case r := <-done:
		return r.m, r.cmd
	case <-time.After(2 * time.Second):
		t.Fatalf("Update(%v) blocked on the context probe", msg)
		return m, nil
	}
}

// TestContextPaletteOpensWithoutWaitingOnProbes is the regression for 02
// H1: opening 7a with more than probeConcurrency contexts froze the
// terminal, because kube.ProbeContexts ran on the update loop and blocked
// there behind a wave of probes (unboundedly, behind a hung exec credential
// plugin). The stub prober blocks its caller the way that call did; the
// palette must still open at once with every row "probing…", and a row must
// update when its result message lands.
func TestContextPaletteOpensWithoutWaitingOnProbes(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	stubProbeContexts(t, func(_ context.Context, _ []string) <-chan kube.ProbeResult {
		<-release // a hung credential plugin, on whichever goroutine calls this
		ch := make(chan kube.ProbeResult, 1)
		ch <- kube.ProbeResult{Name: "prod-eks", Latency: 42 * time.Millisecond}
		close(ch)
		return ch
	})

	m := newProbeTestModel(t)
	m, cmd := updateWithin(t, m, tea.KeyPressMsg{Text: "c", Code: 'c'})
	if m.palette == nil || cmd == nil {
		t.Fatalf("expected c to open the context palette with a probe cmd (palette nil=%v, cmd nil=%v)", m.palette == nil, cmd == nil)
	}
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "prod-eks") || !strings.Contains(view, "probing…") {
		t.Fatalf("expected rows listed as probing… on open, got:\n%s", view)
	}

	msgs := make(chan tea.Msg, 1)
	go func() { msgs <- cmd() }()
	close(release)
	var msg tea.Msg
	select {
	case msg = <-msgs:
	case <-time.After(2 * time.Second):
		t.Fatal("probe cmd never delivered a result")
	}
	if _, ok := msg.(contextProbeMsg); !ok {
		t.Fatalf("probe cmd returned %T, want contextProbeMsg", msg)
	}
	m, _ = updateWithin(t, m, msg)
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "42ms") {
		t.Fatalf("expected prod-eks's row updated to 42ms, got:\n%s", view)
	}
}

// TestContextPaletteCloseCancelsProbes: a probe run is cancelled by a
// re-probe (whose stale results then never land) and by closing the
// palette, so nothing keeps running exec credential plugins for a palette
// nobody is looking at.
func TestContextPaletteCloseCancelsProbes(t *testing.T) {
	ctxs := make(chan context.Context, 4)
	stubProbeContexts(t, func(ctx context.Context, _ []string) <-chan kube.ProbeResult {
		ctxs <- ctx
		ch := make(chan kube.ProbeResult, 1)
		ch <- kube.ProbeResult{Name: "prod-eks", Latency: 7 * time.Millisecond}
		close(ch)
		return ch
	})
	m := newProbeTestModel(t)

	m, first := updateWithin(t, m, tea.KeyPressMsg{Text: "c", Code: 'c'})
	staleMsg := first()
	firstCtx := <-ctxs

	m, second := updateWithin(t, m, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	if firstCtx.Err() == nil {
		t.Fatal("ctrl+r re-probe left the previous probe run running")
	}
	m, _ = updateWithin(t, m, staleMsg)
	if _, ok := m.probes["prod-eks"]; ok {
		t.Fatal("a superseded probe run's result overwrote the current run's row")
	}

	_ = second()
	secondCtx := <-ctxs
	m, _ = updateWithin(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.palette != nil {
		t.Fatal("esc did not close the palette")
	}
	if secondCtx.Err() == nil {
		t.Fatal("closing the palette left its probe run running")
	}
}
