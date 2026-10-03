package kube

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// prodTarget is a context deliberately unlike any kubeconfig's
// current-context: the regression these tests pin is a kubectl/helm child
// silently resolving the file's own current-context instead of the one kute
// is connected to (docs/review/04-pods.md H1, 08-helm.md H1).
var prodTarget = CommandTarget{Kubeconfig: "/etc/kute/config", Context: "prod-eu"}

// TestKubectlBuildersPinTargetContext asserts every kubectl subprocess kute
// launches carries --kubeconfig/--context ahead of its verb (so neither can
// land after a `--` and reach the container command instead), and that each
// "will run" string is exactly the argv that runs.
func TestKubectlBuildersPinTargetContext(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		args    []string
		willRun string
	}{
		{"exec", ExecSpec(prodTarget, "db", "postgres-0", "", "bash").Args, ExecCommandString(prodTarget, "db", "postgres-0", "", "bash")},
		{"edit", EditSpec(prodTarget, KindDeployment, "db", "api").Args, ""},
		{"debug attach", PodDebugAttachSpec(prodTarget, "db", "postgres-0", "", "postgres", "").Args, PodDebugAttachCommandString(prodTarget, "db", "postgres-0", "", "postgres", "")},
		{"debug copy", PodDebugCopySpec(prodTarget, "db", "postgres-0", "postgres-0-debug", "postgres", "", true, "").Args, PodDebugCopyCommandString(prodTarget, "db", "postgres-0", "postgres-0-debug", "postgres", "", true, "")},
		{"node debug", NodeDebugSpec(prodTarget, "node-a", "", "").Args, NodeDebugCommandString(prodTarget, "node-a", "", "")},
	}
	wantPrefix := []string{"kubectl", "--kubeconfig", "/etc/kute/config", "--context", "prod-eu"}
	for _, tc := range cases {
		if !slices.Equal(tc.args[:len(wantPrefix)], wantPrefix) {
			t.Errorf("%s argv = %q, want prefix %q", tc.name, tc.args, wantPrefix)
		}
		if tc.willRun == "" {
			continue
		}
		if got := commandString(tc.args); tc.willRun != got {
			t.Errorf("%s will-run = %q, want the real argv %q", tc.name, tc.willRun, got)
		}
	}
}

// TestCommandTargetZeroAddsNoFlags covers in-cluster config (no kubeconfig
// file, no context name): kubectl must be left to its own in-cluster
// fallback rather than handed an empty --context.
func TestCommandTargetZeroAddsNoFlags(t *testing.T) {
	t.Parallel()
	got := ExecCommandString(CommandTarget{}, "default", "api-1", "", "sh")
	if want := "kubectl exec -it api-1 -n default -- sh"; got != want {
		t.Fatalf("zero-target exec = %q, want %q", got, want)
	}
	if got := HelmRollbackCommandString(CommandTarget{Context: "kind-dev"}, "default", "web", 0); got != "helm rollback web -n default --kube-context kind-dev" {
		t.Fatalf("context-only helm rollback = %q", got)
	}
}

func TestHelmRollbackCommandStringPinsTarget(t *testing.T) {
	t.Parallel()
	got := HelmRollbackCommandString(prodTarget, "production", "postgresql", 2)
	want := "helm rollback postgresql 2 -n production --kubeconfig /etc/kute/config --kube-context prod-eu"
	if got != want {
		t.Fatalf("HelmRollbackCommandString = %q, want %q", got, want)
	}
}

// TestHelmRollbackCommandSummaryElidesOnlyKubeconfig pins the keybar's
// narrow-width fallback: the --kubeconfig path is the one thing dropped (and
// marked as dropped), the cluster-pinning --kube-context is kept, and with no
// --kubeconfig to elide it is the full command.
func TestHelmRollbackCommandSummaryElidesOnlyKubeconfig(t *testing.T) {
	t.Parallel()
	got := HelmRollbackCommandSummary(prodTarget, "production", "postgresql", 2)
	if want := "helm rollback postgresql 2 -n production --kube-context prod-eu …"; got != want {
		t.Fatalf("HelmRollbackCommandSummary = %q, want %q", got, want)
	}
	contextOnly := CommandTarget{Context: "prod-eu"}
	if got, want := HelmRollbackCommandSummary(contextOnly, "production", "postgresql", 2), HelmRollbackCommandString(contextOnly, "production", "postgresql", 2); got != want {
		t.Fatalf("HelmRollbackCommandSummary without --kubeconfig = %q, want the full %q", got, want)
	}
}

// writeArgvRecorder installs a fake binary on PATH that writes its argv, one
// per line, to a file — so the tests below observe the subprocess kute
// actually spawns rather than a builder's return value.
func writeArgvRecorder(t *testing.T, name string) (argvFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("argv recorder is a POSIX shell script")
	}
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + argvFile + "\necho sh\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvFile
}

func readArgv(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("subprocess never ran: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// TestClusterHelmRollbackTargetsClusterContext is the 08 H1 regression: a
// rollback confirmed against kute's context must reach helm with that
// context, not the kubeconfig's current-context.
func TestClusterHelmRollbackTargetsClusterContext(t *testing.T) {
	argvFile := writeArgvRecorder(t, "helm")
	SetKubeconfigPath("/etc/kute/config")
	t.Cleanup(func() { SetKubeconfigPath("") })

	c := &Cluster{kctx: Context{ContextName: "prod-eu"}}
	if err := c.HelmRollback(t.Context(), "production", "postgresql", 2); err != nil {
		t.Fatalf("HelmRollback: %v", err)
	}
	got := readArgv(t, argvFile)
	want := []string{"rollback", "postgresql", "2", "-n", "production", "--kubeconfig", "/etc/kute/config", "--kube-context", "prod-eu"}
	if !slices.Equal(got, want) {
		t.Fatalf("helm argv = %q, want %q", got, want)
	}
}

// TestDetectShellsTargetsContext covers the one kubectl call that isn't a
// tty handoff: the shell probe behind every 'x'.
func TestDetectShellsTargetsContext(t *testing.T) {
	argvFile := writeArgvRecorder(t, "kubectl")
	if _, err := DetectShells(t.Context(), prodTarget, "db", "postgres-0", "postgres"); err != nil {
		t.Fatalf("DetectShells: %v", err)
	}
	got := readArgv(t, argvFile)
	if want := []string{"--kubeconfig", "/etc/kute/config", "--context", "prod-eu", "exec"}; !slices.Equal(got[:len(want)], want) {
		t.Fatalf("probe argv = %q, want prefix %q", got, want)
	}
}
