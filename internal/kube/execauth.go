package kube

import (
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/pkg/apis/clientauthentication"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/dump"
)

// Exec credential plugins (gke-gcloud-auth-plugin, `aws eks get-token`,
// kubelogin) are the credential shape kute meets on GKE and EKS, and the
// first one that is re-minted *during* a session rather than read once from
// the kubeconfig: a GKE token lasts about an hour, an EKS one about fifteen
// minutes. client-go re-runs the plugin binary when the current credential
// expires, from whichever goroutine happens to make the next request — an
// informer's watch, the /livez ping — at an arbitrary point hours after
// launch.
//
// That subprocess and kute are then fighting over one terminal, on both ends:
//
//   - stdin. A v1beta1 exec block with no interactiveMode defaults to
//     IfAvailable "for backwards compatibility"
//     (tools/clientcmd/api/v1/defaults.go), which is exactly what `aws eks
//     update-kubeconfig` writes; GKE writes v1 with interactiveMode:
//     IfAvailable spelled out. Either way IfAvailable resolves to
//     "interactive if os.Stdin is a terminal" — which it is, because kute
//     runs on a tty. The
//     plugin is then handed the real os.Stdin while Bubble Tea holds it in
//     raw mode: two readers on one descriptor, so keystrokes go missing, and
//     a plugin that prompts (expired SSO, gcloud re-auth, MFA) waits forever
//     for input the user cannot give it.
//   - stderr. client-go hands the plugin os.Stderr verbatim
//     (plugin/pkg/client/auth/exec/exec.go's `cmd.Stderr = a.stderr`) and
//     never reads it back, so anything the plugin prints lands on top of the
//     rendered frame — the same splatter silenceKlog exists to prevent, from
//     a source klog can't cover.
//
// Both are fixed here, in one place, because both are the same fact: kute
// owns the terminal, and the credential plugin has to be told so.

// credentialStdinMessage is what client-go appends to its own "standard input
// is unavailable" error for an interactiveMode: Always plugin. It names the
// recovery path, since kute cannot offer the plugin's own prompt: another
// shell, then 4a/4c's r.
const credentialStdinMessage = "kute holds the terminal; re-authenticate in another shell and press r to retry"

// prepareExecProvider tells client-go that this process's stdin is spoken for
// before any credential plugin runs. StdinUnavailable is deliberately not
// settable from a kubeconfig (tools/clientcmd/api/v1/zz_generated.conversion.go
// marks it "opted out of conversion generation") precisely because it is the
// client's statement about itself, not the cluster's: setting it turns an
// IfAvailable plugin non-interactive, and turns an Always plugin's silent
// hang into an immediate, legible error.
func prepareExecProvider(cfg *rest.Config) {
	if cfg == nil || cfg.ExecProvider == nil {
		return
	}
	cfg.ExecProvider.StdinUnavailable = true
	cfg.ExecProvider.StdinUnavailableMessage = credentialStdinMessage
}

// usesCredentialPlugin reports whether cfg mints its credential through an
// exec or auth-provider plugin — the only case where a 401 means "a human has
// to re-authenticate" rather than something worth retrying on backoff.
func usesCredentialPlugin(cfg *rest.Config) bool {
	return cfg != nil && (cfg.ExecProvider != nil || cfg.AuthProvider != nil)
}

// maxCapturedStderr bounds the credential-plugin stderr kept in memory. A
// plugin's failure message is a line or two; this is generous enough for a
// stack-trace-happy one and small enough to keep unconditionally.
const maxCapturedStderr = 4 << 10

// stderrCapture holds the pipe kute substitutes for os.Stderr while client-go
// builds its exec Authenticator, plus a ring of the bytes the plugin has
// written to it since the last read.
//
// The substitution has to happen around client construction rather than
// around the plugin run, because client-go reads os.Stderr once — when the
// Authenticator is created — and keeps that writer for the process's life
// (exec.newAuthenticator's `stderr: os.Stderr`). Authenticators are cached
// globally by exec config, so the first client built for a given kubeconfig
// user is the one that decides where every later token mint's stderr goes.
type stderrCapture struct {
	// openPipe is set by newStderrCapture, not declared here, because
	// sync.OnceValue closes over the receiver.
	openPipe func() *os.File

	mu  sync.Mutex
	buf []byte
	// grew carries one token per append, so take can wait for output instead
	// of polling for it. Buffered and sent to non-blockingly: drain must
	// never block on a reader, or a full pipe buffer stalls the credential
	// plugin's own write — the hang this file exists to prevent.
	grew chan struct{}
}

// pluginStderrs holds one capture per client-go exec Authenticator, keyed the
// way client-go's own Authenticator cache is (exec.cacheKey: the exec config
// plus, when it asks for it, the cluster). One process-global ring let every
// plugin's output — a probe of another context, the context left behind by a
// switch, a deprecation warning printed on a successful mint — become the
// active cluster's banner at its next credential failure. Keying by the
// Authenticator rather than by client matters: the first client built for an
// exec config fixes that Authenticator's stderr for the process's life, so a
// probe of a context and a later switch to it must share one ring.
var pluginStderrs = struct {
	sync.Mutex
	m map[string]*stderrCapture
}{m: map[string]*stderrCapture{}}

// stderrCaptureFor returns the capture for cfg's exec Authenticator, or nil
// when cfg mints no credential through an exec plugin.
func stderrCaptureFor(cfg *rest.Config) *stderrCapture {
	if cfg == nil || cfg.ExecProvider == nil {
		return nil
	}
	var cluster *clientauthentication.Cluster
	if cfg.ExecProvider.ProvideClusterInfo {
		// An error here fails rest.TransportFor too, so the key it would
		// have produced never names a live Authenticator.
		cluster, _ = rest.ConfigToExecCluster(cfg)
	}
	key := dump.Pretty(struct {
		conf    *clientcmdapi.ExecConfig
		cluster *clientauthentication.Cluster
	}{cfg.ExecProvider, cluster})
	pluginStderrs.Lock()
	defer pluginStderrs.Unlock()
	s, ok := pluginStderrs.m[key]
	if !ok {
		s = newStderrCapture()
		pluginStderrs.m[key] = s
	}
	return s
}

func newStderrCapture() *stderrCapture {
	s := &stderrCapture{grew: make(chan struct{}, 1)}
	// The pipe and its draining goroutine are created at most once, and the
	// result is the value of the call — which is all the old once+field pair
	// was ever doing by hand.
	s.openPipe = sync.OnceValue(func() *os.File {
		r, w, err := os.Pipe()
		if err != nil {
			// Capture is skipped; stderr keeps its normal (frame-corrupting,
			// but not worse-than-today) behaviour.
			return nil
		}
		go s.drain(r)
		return w
	})
	return s
}

// writer lazily creates the pipe and the goroutine draining it. Returns nil
// if the pipe can't be created.
func (s *stderrCapture) writer() *os.File { return s.openPipe() }

// drain reads the pipe forever, appending to the ring. It must keep reading
// whether or not anyone ever asks for the contents: a full pipe buffer would
// block the credential plugin's own write, which is the hang this file exists
// to prevent.
func (s *stderrCapture) drain(r *os.File) {
	chunk := make([]byte, 1024)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			s.append(chunk[:n])
		}
		if err != nil {
			return
		}
	}
}

func (s *stderrCapture) append(p []byte) {
	s.mu.Lock()
	s.buf = append(s.buf, p...)
	if len(s.buf) > maxCapturedStderr {
		s.buf = s.buf[len(s.buf)-maxCapturedStderr:]
	}
	s.mu.Unlock()

	// Non-blocking: one pending token is as good as ten, and drain must never
	// park here.
	select {
	case s.grew <- struct{}{}:
	default:
	}
}

// capture runs fn with os.Stderr pointed at the pipe, so any exec
// Authenticator fn constructs adopts it. The swap is process-global for the
// duration, hence the lock: ProbeContexts builds clients for several contexts
// concurrently, and client construction does no I/O, so serialising it costs
// nothing.
var captureMu sync.Mutex

func (s *stderrCapture) capture(fn func()) {
	if s == nil {
		fn()
		return
	}
	w := s.writer()
	if w == nil {
		fn()
		return
	}
	captureMu.Lock()
	real := os.Stderr
	os.Stderr = w
	defer func() {
		os.Stderr = real
		captureMu.Unlock()
	}()
	fn()
}

// take returns everything the credential plugin has written since the last
// call and clears the ring, so a later failure never reports a stale message.
//
// It waits briefly for content when the ring is empty because the caller is
// on the other side of a race it can't otherwise close: the plugin writes,
// exits, and client-go turns the non-zero exit into an error, all before the
// draining goroutine is necessarily scheduled. The wait only ever happens on
// a path that has already decided a credential error occurred, and never on
// the render goroutine.
//
// Once the ring is non-empty it keeps polling until a short quiet window
// passes with no further growth, rather than returning on the first peek.
// getCreds never caches a failed mint (exec.go's Authenticator only caches
// success), so a caller that makes two requests against a still-broken
// credential before ever reading the output — as the health loop's retry
// does — can have a second plugin run still landing on the pipe when the
// buffer is first seen non-empty. Returning immediately there would clear
// the first write and strand the second to surface stale on the *next*
// take(), the exact bug this function's clearing exists to prevent.
func (s *stderrCapture) take() string {
	if s == nil {
		return ""
	}
	const (
		quiet   = 20 * time.Millisecond
		initial = 100 * time.Millisecond
	)

	// Waiting on append's own signal rather than re-reading len(s.buf) every
	// 5ms: same two windows as before — `initial` for anything at all to
	// arrive, then `quiet` restarted by each write — but the wait ends the
	// moment the window actually elapses instead of up to a poll late, and
	// the loop no longer takes the mutex on every tick. That matters because
	// take is reachable from any health caller: nothing structural stops a future
	// caller reaching this from the render goroutine.
	timer := time.NewTimer(initial)
	defer timer.Stop()
	for {
		select {
		case <-s.grew:
			// A write landed. Restart the quiet window so a second plugin run
			// already in flight — which the health loop's retry really does
			// produce — has room to land in the same take.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(quiet)
		case <-timer.C:
			s.mu.Lock()
			out := string(s.buf)
			s.buf = nil
			s.mu.Unlock()
			// Drop a token that raced in alongside the final read, so the
			// next take starts from a clean signal rather than shortening its
			// own initial window.
			select {
			case <-s.grew:
			default:
			}
			return out
		}
	}
}

// clear discards whatever the plugin has written so far without waiting:
// output from before the cluster adopted this Authenticator, or from a mint
// that went on to succeed, explains no later failure.
func (s *stderrCapture) clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.buf = nil
	s.mu.Unlock()
	select {
	case <-s.grew:
	default:
	}
}

// credentialFailurePrefix is what client-go's exec round tripper prefixes
// every failure to obtain a credential with (exec.go's roundTripper.RoundTrip:
// `fmt.Errorf("getting credentials: %v", err)`). Matching the string is
// unlovely, but it is the only handle: the wrapped error is a plain
// fmt.Errorf with nothing to type-assert or errors.Is against, and a plugin
// that never produced a token never produced a 401 either — there is no
// apierrors form of "the SSO session expired".
const credentialFailurePrefix = "getting credentials:"

// IsAuthenticationError classifies err as "kute has no usable credential for
// this cluster" — either the credential plugin couldn't mint one, or the
// apiserver rejected the one it minted.
//
// It is deliberately separate from IsPermissionError: a 403 means the cluster
// knows who you are and says no, which is a per-kind fact the 4b card
// reports; a 401 means it doesn't know who you are at all, which is a
// whole-connection fact — and, unlike a dropped TCP connection, one that
// retrying cannot fix.
func IsAuthenticationError(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsUnauthorized(err) {
		return true
	}
	if errors.Is(err, errAuthenticationGated) {
		return true
	}
	return strings.Contains(err.Error(), credentialFailurePrefix)
}

// credentialMessage renders err for ConnState.Err. The credential
// plugin's own stderr wins when there is any: it is the only text that names
// what the user has to run (`aws sso login`, `gcloud auth login`), and
// guessing that from the provider is exactly the guess kute shouldn't make.
// client-go's own message — "exec: executable aws failed with exit code 255"
// — is the fallback, and the whole message for a plain 401.
//
// The result is always one line: ConnState.Err renders into 4a's
// single-line banner, where an embedded newline would break the strip.
//
// fromPlugin reports whether the text is the plugin's own stderr rather than
// the fallback. The stderr read is the active context's own capture, never
// another context's.
func (h *health) credentialMessage(err error) (msg string, fromPlugin bool) {
	var resolved *resolvedCredentialError
	if errors.As(err, &resolved) {
		return resolved.msg, resolved.fromPlugin
	}
	h.mu.Lock()
	stderr := h.stderr
	h.mu.Unlock()
	text := strings.TrimSpace(stderr.take())
	fromPlugin = text != ""
	if !fromPlugin {
		if err == nil {
			return "credentials rejected", false
		}
		text = err.Error()
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > maxCredentialErrorText {
		text = text[:maxCredentialErrorText] + "…"
	}
	return text, fromPlugin
}

// restatesCredentialFailure reports whether err is only a second report of
// a credential failure already on screen: the gate refusing a request, or the
// plugin failing again without saying why. Neither carries anything the
// plugin's own earlier message didn't, so it must not replace it.
func restatesCredentialFailure(err error) bool {
	return errors.Is(err, errAuthenticationGated) ||
		(err != nil && strings.Contains(err.Error(), credentialFailurePrefix))
}

// resolvedCredentialError carries credentialMessage's text, computed
// before Cluster.mu is taken. The message drains the plugin's stderr through
// take(), which waits at least 100 ms for the plugin's write to land; doing
// that under c.mu stalled ListRaw and KindSynced on the Update loop for every
// credential failure. Error and Unwrap pass through, so IsAuthenticationError
// and needsReauth classify it exactly as they would the original.
type resolvedCredentialError struct {
	error
	msg        string
	fromPlugin bool
}

func (e *resolvedCredentialError) Unwrap() error { return e.error }

// resolveCredentialError renders err's ConnState.Err text now, outside any
// lock, when err is a credential failure health will report; any other error
// is returned unchanged and costs nothing.
func (h *health) resolveCredentialError(err error) error {
	if !h.needsReauth(err) {
		return err
	}
	var resolved *resolvedCredentialError
	if errors.As(err, &resolved) {
		return err
	}
	msg, fromPlugin := h.credentialMessage(err)
	return &resolvedCredentialError{error: err, msg: msg, fromPlugin: fromPlugin}
}

// maxCredentialErrorText caps the banner text. A plugin that dumps a stack
// trace shouldn't push the retry hints off the end of the line; the 4c
// screen's own error box wraps, so nothing needs the full text on one row.
const maxCredentialErrorText = 300
