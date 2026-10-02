package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/state"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/tasks/setup"
)

// writeTestKubeconfig writes a minimal multi-context kubeconfig (no live
// cluster needed — kube.AvailableContexts only reads the file, it never
// dials out) and points $KUBECONFIG at it for the test's duration.
func writeTestKubeconfig(t *testing.T, currentContext string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	yaml := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: %s
clusters:
- name: cluster-a
  cluster:
    server: https://a.example.com
contexts:
- name: ctx-a
  context:
    cluster: cluster-a
    namespace: default
- name: ctx-b
  context:
    cluster: cluster-a
    namespace: default
users: []
`, currentContext)
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing test kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", path)
}

func TestSelectThemeFlagWins(t *testing.T) {
	t.Parallel()
	if got := selectTheme("dark", "light"); got != tui.Dark() {
		t.Fatalf("flag theme should win over config theme")
	}
}

func TestSelectThemeConfigFallsBackWhenNoFlag(t *testing.T) {
	t.Parallel()
	if got := selectTheme("", "light"); got != tui.Light() {
		t.Fatalf("config theme should apply when no flag is set")
	}
}

func TestSelectThemeUnrecognizedValuesDeferToDetection(t *testing.T) {
	t.Parallel()
	got := selectTheme("auto", "also-not-a-theme")
	if got != tui.Dark() && got != tui.Light() {
		t.Fatalf("expected a valid detected theme, got %+v", got)
	}
}

// TestSelectThemeDetectionSkipsLiveQueryWhenNotATerminal pins a regression:
// selectTheme's detection fallback must never attempt a live terminal
// background-color query when stdin/stdout aren't real TTYs (always true
// under go test). A prior version issued the query unconditionally, which
// on Windows can block in ReadConsole indefinitely — cancellation goes
// through a fallback reader that can't interrupt an already-blocked read —
// turning a single go test invocation into a 10-minute CI hang. This test
// fails by timing out if that guard is ever removed.
func TestSelectThemeDetectionSkipsLiveQueryWhenNotATerminal(t *testing.T) {
	t.Parallel()
	done := make(chan tui.Theme, 1)
	go func() { done <- selectTheme("", "") }()
	select {
	case got := <-done:
		if got != tui.Dark() && got != tui.Light() {
			t.Fatalf("expected a valid detected theme, got %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("selectTheme blocked for over a second — did it attempt a live terminal query?")
	}
}

func TestBuildSessionDemoModeHasNoCluster(t *testing.T) {
	t.Parallel()
	sess, cluster, err := BuildSession(Config{AppName: "kute", Demo: true, Theme: "dark"})
	if cluster != nil {
		t.Fatalf("expected a nil *kube.Cluster in demo mode")
	}
	if err != nil {
		t.Fatalf("expected no error in demo mode, got %v", err)
	}
	if sess.Cluster != nil {
		t.Fatalf("expected Session.Cluster nil in demo mode")
	}
	if sess.Theme != tui.Dark() {
		t.Fatalf("expected the --theme override honored in demo mode")
	}
	if _, ok := sess.Registry.Descriptor("Pod"); !ok {
		t.Fatalf("expected the default registry populated on Session")
	}
	if len(sess.Groups) == 0 {
		t.Fatalf("expected default groups populated on Session")
	}
}

// TestBuildSessionNoUpdateCheckFlagDisablesCheck pins --no-update-check as a
// per-invocation override that beats the loaded config file, mirroring
// selectTheme's flag-wins precedent.
func TestBuildSessionNoUpdateCheckFlagDisablesCheck(t *testing.T) {
	t.Parallel()
	sess, _, err := BuildSession(Config{AppName: "kute", Demo: true, NoUpdateCheck: true})
	if err != nil {
		t.Fatalf("expected no error in demo mode, got %v", err)
	}
	if sess.Config.UpdateCheckEnabled() {
		t.Fatalf("expected --no-update-check to disable the update check")
	}
}

// TestBuildSessionUpdateCheckEnabledByDefault pins the unchanged default:
// omitting the flag leaves the update check enabled (absent any config file
// disabling it).
func TestBuildSessionUpdateCheckEnabledByDefault(t *testing.T) {
	t.Parallel()
	sess, _, err := BuildSession(Config{AppName: "kute", Demo: true})
	if err != nil {
		t.Fatalf("expected no error in demo mode, got %v", err)
	}
	if !sess.Config.UpdateCheckEnabled() {
		t.Fatalf("expected the update check enabled by default")
	}
}

func TestStartupContextPrefersMostRecentAvailableContext(t *testing.T) {
	writeTestKubeconfig(t, "ctx-a")
	got := startupContext(Config{}, state.State{RecentContexts: []string{"ctx-b", "ctx-a"}})
	if got != "ctx-b" {
		t.Fatalf("startupContext() = %q, want ctx-b", got)
	}
}

func TestStartupContextFallsBackWhenRecentContextIsGone(t *testing.T) {
	writeTestKubeconfig(t, "ctx-a")
	got := startupContext(Config{}, state.State{RecentContexts: []string{"ctx-deleted"}})
	if got != "" {
		t.Fatalf("startupContext() = %q, want empty (defer to kubeconfig current-context)", got)
	}
}

func TestStartupContextEmptyWithNoRecents(t *testing.T) {
	writeTestKubeconfig(t, "ctx-a")
	got := startupContext(Config{}, state.State{})
	if got != "" {
		t.Fatalf("startupContext() = %q, want empty", got)
	}
}

// TestStartupContextFlagWins pins --context ahead of the last-used context:
// the flag is passed through verbatim, so even a name the kubeconfig doesn't
// have reaches the cluster builder (and surfaces as 4c naming it) rather than
// silently falling back to somewhere else.
func TestStartupContextFlagWins(t *testing.T) {
	writeTestKubeconfig(t, "ctx-a")

	if got := startupContext(Config{Context: "ctx-b"}, state.State{RecentContexts: []string{"ctx-a"}}); got != "ctx-b" {
		t.Errorf("startupContext(--context ctx-b) = %q, want ctx-b over the recent ctx-a", got)
	}
	if got := startupContext(Config{Context: "ctx-typo"}, state.State{}); got != "ctx-typo" {
		t.Errorf("startupContext(--context ctx-typo) = %q, want it passed through verbatim", got)
	}
}

// TestBuildSessionNamespaceFlagWins pins -n/--namespace as the
// highest-precedence namespace source: ahead of the context's own namespace
// and ahead of the persisted per-context restore. Building a session needs no
// live cluster — client construction doesn't dial.
func TestBuildSessionNamespaceFlagWins(t *testing.T) {
	writeTestKubeconfig(t, "ctx-a")

	sess, cluster, err := BuildSession(Config{Namespace: "ingress-nginx"})
	if err != nil || cluster == nil {
		t.Fatalf("BuildSession() = %v, cluster %v; want a client built from the test kubeconfig", err, cluster)
	}
	if got := sess.Location.Namespace; got != "ingress-nginx" {
		t.Errorf("Location.Namespace = %q, want the --namespace value to win", got)
	}

	// Without the flag it falls back to the context's own namespace.
	sess, _, err = BuildSession(Config{})
	if err != nil {
		t.Fatalf("BuildSession() = %v", err)
	}
	if got := sess.Location.Namespace; got != "default" {
		t.Errorf("Location.Namespace = %q with no flag, want the context's own namespace", got)
	}
}

// TestBuildSessionNamespaceFlagWinsInDemo pins the same precedence for
// --demo, whose namespace otherwise comes from the fake cluster.
func TestBuildSessionNamespaceFlagWinsInDemo(t *testing.T) {
	sess, _, _ := BuildSession(Config{Demo: true, Namespace: "ingress-nginx"})
	if got := sess.Location.Namespace; got != "ingress-nginx" {
		t.Errorf("Location.Namespace = %q, want the --namespace value", got)
	}
}

// TestBuildSessionAppliesKubeconfigFlag pins the ordering that makes
// --kubeconfig work at all: BuildSession must hand it to kube before building
// any client, so every kubeconfig reader resolves the same file.
func TestBuildSessionAppliesKubeconfigFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "explicit-config")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	t.Cleanup(func() { kube.SetKubeconfigPath("") })

	if _, _, err := BuildSession(Config{Demo: true, Kubeconfig: path}); err != nil {
		t.Fatalf("BuildSession() = %v", err)
	}
	if got, ok := kube.KubeconfigPath(); !ok || got != path {
		t.Errorf("kube.KubeconfigPath() = %q/%v after BuildSession, want the --kubeconfig path", got, ok)
	}
}

// TestNewModelDemoStartsInFlaggedNamespace drives the flag the way the binary
// does — through NewModel, which builds the demo cluster and would otherwise
// overwrite Location.Namespace with the fake cluster's own default — and
// checks the rendered header actually names it.
func TestNewModelDemoStartsInFlaggedNamespace(t *testing.T) {
	model, _, demo := NewModel(Config{AppName: DefaultAppName, Demo: true, Namespace: "ingress-nginx"})
	if demo == nil {
		t.Fatal("expected a demo cluster")
	}
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	view := ansi.Strip(updated.(tui.Model).View().Content)
	if !strings.Contains(view, "ingress-nginx") {
		t.Errorf("rendered view never names the --namespace value:\n%s", view)
	}
}

// A 4c rebuild whose new cluster can't even be built must leave the old one
// running: r on the 4c that stays up is bound to the old cluster's RetryNow,
// and its in-flight reads have nothing newer to give way to.
func TestFailedRebuildKeepsTheOldCluster(t *testing.T) {
	writeTestKubeconfig(t, "ctx-a")
	sess, old, err := BuildSession(Config{})
	if err != nil || old == nil {
		t.Fatalf("BuildSession() = %v, cluster %v", err, old)
	}
	sess.SetContext(t.Context())
	readCtx := sess.ClusterContext()

	for name, cmd := range map[string]tea.Cmd{
		"switch":    attemptSwitchContext(Config{}, sess, "no-such-context"),
		"reconnect": attemptReconnect(Config{}, sess, filepath.Join(t.TempDir(), "missing")),
	} {
		if _, ok := cmd().(setup.RetryFailedMsg); !ok {
			t.Fatalf("%s: want RetryFailedMsg for a cluster that can't be built", name)
		}
		if sess.Cluster != old {
			t.Errorf("%s: session cluster replaced after a failed build", name)
		}
		if readCtx.Err() != nil {
			t.Errorf("%s: the old cluster's reads were cancelled by a rebuild that never happened", name)
		}
	}
}
