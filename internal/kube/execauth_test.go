package kube

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// An exec credential plugin is just a binary that prints an ExecCredential
// JSON document to stdout, so a handful of shell scripts reproduces the GKE
// and EKS credential shape faithfully enough to test every path that matters
// — including the ones AKS and MicroK8s never reach, because their
// credentials are long-lived and never re-minted mid-session.

// execPlugin writes a shell-script credential plugin and returns its path.
func execPlugin(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script credential plugin fixtures are POSIX-only")
	}
	path := filepath.Join(t.TempDir(), "credential-plugin.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write plugin: %v", err)
	}
	return path
}

// execKubeconfig writes a kubeconfig whose single context authenticates via
// plugin, pins it as the kubeconfig kute reads, and returns nothing — the
// caller builds a client with NewClient().
func execKubeconfig(t *testing.T, server, plugin string) {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	// A plain-HTTP server would be pointless here: clientcmd only reads a
	// user's auth information at all when the connection is TLS
	// (DirectClientConfig.ClientConfig's isConfigTransportTLS gate), so an
	// http:// test server silently produces a config with no ExecProvider.
	cfg.Clusters["c"] = &clientcmdapi.Cluster{Server: server, InsecureSkipTLSVerify: true}
	cfg.AuthInfos["u"] = &clientcmdapi.AuthInfo{
		Exec: &clientcmdapi.ExecConfig{
			APIVersion: "client.authentication.k8s.io/v1beta1",
			Command:    plugin,
			// Left unset deliberately: this is what `aws eks
			// update-kubeconfig` writes, and clientcmd defaults it to
			// IfAvailable "for backwards compatibility" — the default that
			// hands a plugin the real stdin on a tty.
			InteractiveMode: "",
		},
	}
	cfg.Contexts["ctx"] = &clientcmdapi.Context{Cluster: "c", AuthInfo: "u", Namespace: "default"}
	cfg.CurrentContext = "ctx"

	path := filepath.Join(t.TempDir(), "config")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	SetKubeconfigPath(path)
	t.Cleanup(func() { SetKubeconfigPath("") })
}

// tokenScript is a plugin body printing tok as a never-expiring credential.
func tokenScript(token string) string {
	return `printf '{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential","status":{"token":"` + token + `"}}\n'`
}

// authRecorder is an apiserver stub recording the bearer tokens it is
// presented with, so a test can tell one token mint from the next.
type authRecorder struct {
	mu     sync.Mutex
	tokens []string
}

func (a *authRecorder) server(t *testing.T) string {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.tokens = append(a.tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		a.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (a *authRecorder) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.tokens...)
}

func livez(t *testing.T, c Client) error {
	t.Helper()
	return c.Interface.Discovery().RESTClient().Get().AbsPath("/livez").Do(t.Context()).Error()
}

// The fix itself: every exec provider kute builds a client for is told that
// stdin belongs to the program, and carries the message the user needs when a
// plugin insists on being interactive anyway.
func TestExecProviderIsToldStdinIsUnavailable(t *testing.T) {
	rec := &authRecorder{}
	execKubeconfig(t, rec.server(t), execPlugin(t, tokenScript("tok")))

	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	exec := client.credentialConfig.ExecProvider
	if exec == nil {
		t.Fatal("REST config has no ExecProvider")
	}
	if !exec.StdinUnavailable {
		t.Error("StdinUnavailable = false, want true — client-go will hand the plugin the raw terminal")
	}
	if exec.StdinUnavailableMessage != credentialStdinMessage {
		t.Errorf("StdinUnavailableMessage = %q, want %q", exec.StdinUnavailableMessage, credentialStdinMessage)
	}
}

// A config with no exec block must come through untouched — the MicroK8s/AKS
// client-certificate shape has no plugin to configure.
func TestPrepareExecProviderIgnoresNonExecConfigs(t *testing.T) {
	prepareExecProvider(nil) // must not panic
}

// The happy path: the plugin mints a token and every request carries it.
func TestCredentialPluginTokenReachesTheAPIServer(t *testing.T) {
	rec := &authRecorder{}
	execKubeconfig(t, rec.server(t), execPlugin(t, tokenScript("tok-happy")))

	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := livez(t, client); err != nil {
		t.Fatalf("livez: %v", err)
	}
	if got := rec.seen(); len(got) != 1 || got[0] != "tok-happy" {
		t.Errorf("tokens presented = %v, want [tok-happy]", got)
	}
}

// The stdin-reading plugin fixture. Reading stdin is what a plugin
// that prompts does, and with the real terminal attached it would block
// forever behind Bubble Tea's own reader. Non-interactive, client-go leaves
// cmd.Stdin nil, the read hits EOF, and the plugin finishes.
//
// Note what this can and can't prove: `go test`'s own stdin is not a
// terminal, so client-go's IfAvailable check comes out non-interactive here
// whether or not StdinUnavailable is set. The fix's input is asserted
// directly by TestExecProviderIsToldStdinIsUnavailable; the tty-attached case
// stays a manual check: install this stdin-reading plugin against a kind
// cluster, launch kute, navigate past the token's lifetime, and confirm the
// frame is never corrupted and no keystroke is lost (README.md's
// Authentication section states the user-facing guarantee: a credential
// plugin can never prompt).
func TestCredentialPluginReadingStdinDoesNotHang(t *testing.T) {
	rec := &authRecorder{}
	plugin := execPlugin(t, "read line\n"+tokenScript("tok-stdin"))
	execKubeconfig(t, rec.server(t), plugin)

	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- livez(t, client) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("livez: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("request blocked on a credential plugin reading stdin")
	}
	if got := rec.seen(); len(got) != 1 || got[0] != "tok-stdin" {
		t.Errorf("tokens presented = %v, want [tok-stdin]", got)
	}
}

// Mid-session re-minting, the case a long-lived client certificate never
// reaches: an already-expired credential makes client-go run the plugin again
// for the next request, and the fresh token is the one that goes on the wire.
func TestExpiredCredentialIsReminted(t *testing.T) {
	rec := &authRecorder{}
	counter := filepath.Join(t.TempDir(), "runs")
	plugin := execPlugin(t, `
n=$(cat `+counter+` 2>/dev/null || echo 0)
n=$((n+1))
echo $n > `+counter+`
printf '{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential","status":{"token":"tok-%s","expirationTimestamp":"2001-01-01T00:00:00Z"}}\n' "$n"
`)
	execKubeconfig(t, rec.server(t), plugin)

	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := livez(t, client); err != nil {
			t.Fatalf("livez %d: %v", i, err)
		}
	}
	got := rec.seen()
	if len(got) != 2 || got[0] == got[1] {
		t.Errorf("tokens presented = %v, want two distinct mints", got)
	}
}

// A failing plugin's own message is the only thing that tells the user what
// to run. client-go hands the plugin os.Stderr and never reads it back, so
// without the capture the message lands on the rendered frame and nowhere
// else.
func TestFailingCredentialPluginStderrIsCaptured(t *testing.T) {
	rec := &authRecorder{}
	plugin := execPlugin(t, `
echo "Error loading SSO Token: Token has expired and refresh failed" >&2
exit 255
`)
	execKubeconfig(t, rec.server(t), plugin)

	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := livez(t, client); err == nil {
		t.Fatal("livez succeeded with a failing credential plugin")
	}
	// The whole point of classifying it: this is not a network outage, so
	// the health loop must not sit in Reconnecting re-running the plugin.
	if err := livez(t, client); !IsAuthenticationError(err) {
		t.Errorf("IsAuthenticationError(%v) = false, want true", err)
	}

	out := client.pluginStderr.take()
	if !strings.Contains(out, "Token has expired") {
		t.Errorf("plugin stderr = %q, want the plugin's own stderr", out)
	}
	if second := client.pluginStderr.take(); second != "" {
		t.Errorf("plugin stderr = %q on the second read, want it cleared", second)
	}
	if got := rec.seen(); len(got) != 0 {
		t.Errorf("apiserver saw %v, want no request at all", got)
	}
}

// Each exec Authenticator gets its own capture: a probe of another context
// whose plugin failed loudly must leave nothing in the active context's ring,
// while two clients for the same plugin — which share client-go's cached
// Authenticator, and so its stderr — must share one.
func TestCredentialPluginStderrIsPerAuthenticator(t *testing.T) {
	rec := &authRecorder{}
	server := rec.server(t)

	execKubeconfig(t, server, execPlugin(t, `
echo "OTHER CONTEXT: run gcloud auth login" >&2
exit 1
`))
	other, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient(other): %v", err)
	}
	if err := livez(t, other); err == nil {
		t.Fatal("livez succeeded with the other context's failing plugin")
	}

	execKubeconfig(t, server, execPlugin(t, "exit 255\n"))
	active, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient(active): %v", err)
	}
	if err := livez(t, active); err == nil {
		t.Fatal("livez succeeded with the active context's failing plugin")
	}
	if got := active.pluginStderr.take(); strings.Contains(got, "OTHER CONTEXT") {
		t.Errorf("active context's plugin stderr = %q, want nothing from another context", got)
	}
	if got := other.pluginStderr.take(); !strings.Contains(got, "OTHER CONTEXT") {
		t.Errorf("other context's plugin stderr = %q, want its own failure", got)
	}

	again, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient(active again): %v", err)
	}
	if again.pluginStderr != active.pluginStderr {
		t.Error("two clients for one exec config got different captures; the second's would never see the shared Authenticator's output")
	}
}
