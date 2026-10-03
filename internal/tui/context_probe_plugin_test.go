package tui

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/state"
	"github.com/kute-dev/kute/internal/testutil/testenv"
)

// pluginProbeKubeconfig has two static-credential contexts (dev, kind-local),
// an exec-plugin one (eks-prod) and an auth-provider one (gke-legacy).
const pluginProbeKubeconfig = `
apiVersion: v1
kind: Config
current-context: dev
contexts:
- name: dev
  context: {cluster: dev, user: static}
- name: eks-prod
  context: {cluster: prod, user: aws}
- name: gke-legacy
  context: {cluster: gke, user: gcp}
- name: kind-local
  context: {cluster: kind, user: static}
clusters:
- name: dev
  cluster: {server: https://dev.example.invalid}
- name: prod
  cluster: {server: https://prod.example.invalid}
- name: gke
  cluster: {server: https://gke.example.invalid}
- name: kind
  cluster: {server: https://kind.example.invalid}
users:
- name: static
  user: {token: abc}
- name: aws
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
      args: [eks, get-token, --cluster-name, prod]
- name: gcp
  user:
    auth-provider: {name: gcp}
`

// recordProbes stubs the prober, recording each run's names and answering
// every one reachable.
func recordProbes(t *testing.T) *[][]string {
	t.Helper()
	var runs [][]string
	stubProbeContexts(t, func(_ context.Context, names []string) <-chan kube.ProbeResult {
		runs = append(runs, slices.Clone(names))
		ch := make(chan kube.ProbeResult, len(names))
		for _, n := range names {
			ch <- kube.ProbeResult{Name: n, Latency: 5e6}
		}
		close(ch)
		return ch
	})
	return &runs
}

func newPluginProbeModel(t *testing.T, active string) Model {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(pluginProbeKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	testenv.SetHome(t, t.TempDir())
	m := NewWithSession(reprobeTestScreen{}, &Session{
		Theme:    Dark(),
		Location: Location{Context: active},
		State:    state.State{PerContext: map[string]state.PerContext{}},
	})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	return updated.(Model)
}

// drainProbes runs a probe cmd's whole drain chain through the model.
func drainProbes(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for cmd != nil {
		msg := cmd()
		updated, next := m.Update(msg)
		m = updated.(Model)
		if _, done := msg.(contextProbesDoneMsg); done {
			break
		}
		cmd = next
	}
	return m
}

func contextRow(t *testing.T, m Model, name string) string {
	t.Helper()
	for line := range strings.SplitSeq(ansi.Strip(m.View().Content), "\n") {
		if strings.Contains(line, " "+name+" ") {
			return line
		}
	}
	t.Fatalf("no palette row for %s:\n%s", name, ansi.Strip(m.View().Content))
	return ""
}

// 02 M2: opening c ran every context's credential plugin — a browser login
// per kubelogin context, an SSO prompt per aws profile — for contexts the
// user only scrolled past. Only static-credential contexts are probed on
// open; plugin ones read "not probed" until ctrl+r asks for them.
func TestContextPaletteDoesNotProbeCredentialPluginContextsOnOpen(t *testing.T) {
	runs := recordProbes(t)
	m := newPluginProbeModel(t, "dev")

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "c"})
	m = drainProbes(t, updated.(Model), cmd)
	if len(*runs) != 1 {
		t.Fatalf("opening the palette started %d probe runs, want 1", len(*runs))
	}
	if got, want := (*runs)[0], []string{"dev", "kind-local"}; !slices.Equal(got, want) {
		t.Fatalf("probed on open = %v, want only the static-credential contexts %v", got, want)
	}
	for _, name := range []string{"eks-prod", "gke-legacy"} {
		if row := contextRow(t, m, name); !strings.Contains(row, "○ not probed") {
			t.Errorf("%s row = %q, want ○ not probed", name, row)
		}
	}
	if row := contextRow(t, m, "kind-local"); !strings.Contains(row, "● 5ms") {
		t.Errorf("kind-local row = %q, want its probe result", row)
	}

	updated, cmd = m.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	m = drainProbes(t, updated.(Model), cmd)
	if len(*runs) != 2 {
		t.Fatalf("ctrl+r started %d probe runs in total, want 2", len(*runs))
	}
	if got, want := (*runs)[1], []string{"dev", "eks-prod", "gke-legacy", "kind-local"}; !slices.Equal(got, want) {
		t.Fatalf("probed on ctrl+r = %v, want every context %v", got, want)
	}
	if row := contextRow(t, m, "eks-prod"); !strings.Contains(row, "● 5ms") {
		t.Errorf("eks-prod row after ctrl+r = %q, want its probe result", row)
	}
}

// The active context is already connected — its plugin has already run —
// so it is probed on open even when it uses one.
func TestContextPaletteProbesActiveCredentialPluginContext(t *testing.T) {
	runs := recordProbes(t)
	m := newPluginProbeModel(t, "eks-prod")
	updated, cmd := m.Update(tea.KeyPressMsg{Text: "c"})
	_ = drainProbes(t, updated.(Model), cmd)
	if len(*runs) != 1 || !slices.Contains((*runs)[0], "eks-prod") || slices.Contains((*runs)[0], "gke-legacy") {
		t.Fatalf("probed on open = %v, want the active eks-prod but not gke-legacy", *runs)
	}
}
