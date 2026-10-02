package kube

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/kute-dev/kute/internal/testutil/testenv"
)

// twoContextKubeconfig is the shared two-context fixture: ctx-a (the file's
// own current-context) and ctx-b.
const twoContextKubeconfig = `apiVersion: v1
kind: Config
current-context: ctx-a
clusters:
- name: cluster-a
  cluster:
    server: https://127.0.0.1:16440
- name: cluster-b
  cluster:
    server: https://127.0.0.1:16441
contexts:
- name: ctx-a
  context:
    cluster: cluster-a
    namespace: ns-a
    user: user-a
- name: ctx-b
  context:
    cluster: cluster-b
    namespace: ns-b
    user: user-b
users:
- name: user-a
  user: {}
- name: user-b
  user: {}
`

// TestBuildConfigLookupErrorPaths pins 10b's "LOOKED IN" box data (mvp-plan.md
// Phase 4): each path kute checked, and why it didn't work.
func TestBuildConfigLookupErrorPaths(t *testing.T) {
	t.Parallel()
	cause := errors.New("stat /nonexistent: no such file or directory")

	t.Run("KUBECONFIG unset", func(t *testing.T) {
		t.Parallel()
		err := buildConfigLookupError("", "", "/home/x/.kube/config", cause)
		if len(err.Paths) != 2 {
			t.Fatalf("Paths = %d entries, want 2", len(err.Paths))
		}
		if err.Paths[0].Label != "$KUBECONFIG" || err.Paths[0].Reason != "not set" {
			t.Errorf("Paths[0] = %+v, want $KUBECONFIG/not set", err.Paths[0])
		}
		if err.Paths[1].Label != "~/.kube/config" || err.Paths[1].Path != "/home/x/.kube/config" {
			t.Errorf("Paths[1] = %+v, want ~/.kube/config with the resolved path", err.Paths[1])
		}
		if err.Paths[1].Reason != "no such file" {
			t.Errorf("Paths[1].Reason = %q, want %q", err.Paths[1].Reason, "no such file")
		}
	})

	t.Run("KUBECONFIG set to a missing file", func(t *testing.T) {
		t.Parallel()
		err := buildConfigLookupError("/tmp/does-not-exist-kute-test", "$KUBECONFIG", "/home/x/.kube/config", cause)
		if err.Paths[0].Path != "/tmp/does-not-exist-kute-test" {
			t.Errorf("Paths[0].Path = %q, want the env value", err.Paths[0].Path)
		}
		if err.Paths[0].Reason != "no such file" {
			t.Errorf("Paths[0].Reason = %q, want %q", err.Paths[0].Reason, "no such file")
		}
	})

	t.Run("Error/Unwrap", func(t *testing.T) {
		t.Parallel()
		err := buildConfigLookupError("", "", "", cause)
		if !errors.Is(err, cause) {
			t.Errorf("errors.Is(err, cause) = false, want true (Unwrap must expose cause)")
		}
		if err.Error() == "" {
			t.Errorf("Error() should not be empty")
		}
	})
}

// TestNewClientForContextHonorsExplicitOverride pins a real bug found while
// building 4c's SwitchToContext (docs/design README.md §4c: "↵ connect to
// selected"): rawConfig.RawConfig() reflects the kubeconfig file's own
// current-context verbatim — configOverrides only ever reaches
// clientConfig.ClientConfig()'s constructed REST config, never this raw
// view — so requesting a context other than the file's own default must
// still resolve ContextName/ClusterName/Namespace to the one actually
// requested, not silently fall back to describing the file's default one
// (previously: switching from "ctx-a" to "ctx-b" correctly dialed ctx-b's
// server, but the header kept naming "ctx-a").
func TestNewClientForContextHonorsExplicitOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, []byte(twoContextKubeconfig), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("KUBECONFIG", path)

	client, err := newClientForContext("ctx-b")
	if err != nil {
		t.Fatalf("newClientForContext(ctx-b) = %v", err)
	}
	if client.Context.ContextName != "ctx-b" {
		t.Errorf("ContextName = %q, want %q", client.Context.ContextName, "ctx-b")
	}
	if client.Context.ClusterName != "cluster-b" {
		t.Errorf("ClusterName = %q, want %q", client.Context.ClusterName, "cluster-b")
	}
	if client.Context.Namespace != "ns-b" {
		t.Errorf("Namespace = %q, want %q", client.Context.Namespace, "ns-b")
	}
	// user-b has no client cert, so its name is only the kubeconfig entry.
	if client.Context.UserName != "user-b" || !client.Context.UserFromAuthInfo {
		t.Errorf("UserName = %q, UserFromAuthInfo = %v; want user-b from the AuthInfo entry", client.Context.UserName, client.Context.UserFromAuthInfo)
	}

	// The no-override case ("" — NewCluster's own call) must still fall
	// back to the file's own current-context, unchanged.
	client, err = newClientForContext("")
	if err != nil {
		t.Fatalf("newClientForContext(\"\") = %v", err)
	}
	if client.Context.ContextName != "ctx-a" {
		t.Errorf("ContextName = %q, want the file's default %q", client.Context.ContextName, "ctx-a")
	}
}

// TestKubeconfigSourcePrecedence pins --kubeconfig ahead of $KUBECONFIG
// (kubectl's own precedence) and the label each one reports into 10b's
// LOOKED IN box, so a failure names the file the user actually pointed at.
func TestKubeconfigSourcePrecedence(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/from-env")
	t.Cleanup(func() { SetKubeconfigPath("") })

	if path, label := kubeconfigSource(); path != "/tmp/from-env" || label != "$KUBECONFIG" {
		t.Errorf("kubeconfigSource() = %q/%q with no flag, want the env path", path, label)
	}

	SetKubeconfigPath("/tmp/from-flag")
	if path, label := kubeconfigSource(); path != "/tmp/from-flag" || label != "--kubeconfig" {
		t.Errorf("kubeconfigSource() = %q/%q, want the flag to win over $KUBECONFIG", path, label)
	}
	if got, ok := KubeconfigPath(); !ok || got != "/tmp/from-flag" {
		t.Errorf("KubeconfigPath() = %q/%v, want the flag path — the 7a palette hint must name the file in use", got, ok)
	}

	// Clearing the override falls back to the env var rather than sticking.
	SetKubeconfigPath("")
	if path, _ := kubeconfigSource(); path != "/tmp/from-env" {
		t.Errorf("kubeconfigSource() = %q after clearing the flag, want the env path", path)
	}
}

// TestKubeconfigLoadingRulesDropTheLegacyMigration pins the one thing
// kubeconfigLoadingRules does beyond resolving a path: it clears clientcmd's
// legacy migration rule, which Load() would otherwise run — a *file copy* —
// before every read, including reads that explicitly named another file.
//
// The failure it prevents is Windows-only (see kubeconfigLoadingRules's own
// comment: there, source and destination are two different homes' config
// files, and a missing destination directory turns the read into an error —
// the 7a palette rendered "0 contexts"), so this asserts the rule's absence
// rather than the behaviour, which is the only form that has teeth on every
// platform. The skip guards against the assertion going vacuous if upstream
// ever retires the migration on its own.
func TestKubeconfigLoadingRulesDropTheLegacyMigration(t *testing.T) {
	if len(clientcmd.NewDefaultClientConfigLoadingRules().MigrationRules) == 0 {
		t.Skip("clientcmd no longer ships migration rules; nothing left to clear")
	}
	if rules := kubeconfigLoadingRules(); len(rules.MigrationRules) != 0 {
		t.Errorf("MigrationRules = %v, want none — reading a kubeconfig must never copy one", rules.MigrationRules)
	}
}

// TestAvailableContextsWithHomeKubeconfig covers the exact shape the 7a
// palette's golden fixtures set up — $HOME/.kube/config populated, and
// $KUBECONFIG pointing at a file — which on Windows made the migration rule
// above fire with a live source and a missing destination, and left
// AvailableContexts returning an error instead of the file's contexts.
func TestAvailableContextsWithHomeKubeconfig(t *testing.T) {
	home := t.TempDir()
	testenv.SetHome(t, home)
	dir := filepath.Join(home, ".kube")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte(twoContextKubeconfig), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Setenv("KUBECONFIG", path)

	names, current, err := AvailableContexts()
	if err != nil {
		t.Fatalf("AvailableContexts() = %v", err)
	}
	if len(names) != 2 || names[0] != "ctx-a" || names[1] != "ctx-b" {
		t.Errorf("names = %v, want [ctx-a ctx-b]", names)
	}
	if current != "ctx-a" {
		t.Errorf("current = %q, want ctx-a", current)
	}
}

// TestConfigLookupErrorNamesTheFlag pins the diagnostic label: when
// --kubeconfig pointed at a missing file, 10b must say --kubeconfig, not
// $KUBECONFIG.
func TestConfigLookupErrorNamesTheFlag(t *testing.T) {
	t.Parallel()
	err := buildConfigLookupError("/tmp/kute-flag-missing", "--kubeconfig", "/home/x/.kube/config", errors.New("boom"))
	if err.Paths[0].Label != "--kubeconfig" {
		t.Errorf("Paths[0].Label = %q, want --kubeconfig", err.Paths[0].Label)
	}
}

// Only a kubeconfig that isn't there is "no kubeconfig found". A typo'd
// --context against a perfectly good file used to come back as 10b's lookup
// error with that file marked "invalid kubeconfig" — and, inside a pod, to
// fall back silently to the in-cluster API server under the typo's name.
func TestClientConfigErrorsOnlyLookupWhenKubeconfigMissing(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "config")
	if err := os.WriteFile(good, []byte(`
apiVersion: v1
kind: Config
current-context: dev
contexts:
- name: dev
  context: {cluster: dev}
clusters:
- name: dev
  cluster: {server: https://dev.example.invalid}
users: []
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", good)
	_, err := NewClientForContext("typo")
	if err == nil {
		t.Fatal("NewClientForContext(typo) succeeded")
	}
	if _, ok := errors.AsType[*ConfigLookupError](err); ok {
		t.Errorf("NewClientForContext(typo) = %v, want the context error itself, not \"no kubeconfig found\"", err)
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("NewClientForContext(typo) = %v, want it to name the missing context", err)
	}

	t.Setenv("KUBECONFIG", filepath.Join(dir, "missing"))
	if _, err := NewClientForContext(""); err == nil {
		t.Fatal("NewClientForContext with no kubeconfig succeeded")
	} else if _, ok := errors.AsType[*ConfigLookupError](err); !ok {
		t.Errorf("NewClientForContext with no kubeconfig = %v, want a *ConfigLookupError", err)
	}
}
