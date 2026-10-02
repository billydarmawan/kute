package kube

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// errAuthenticationGated is returned before client-go's credential transport
// is reached while a context has no usable credentials.
var errAuthenticationGated = errors.New("kubernetes traffic paused until credentials are retried")

// authenticationGate sits outside client-go's credential round tripper. It
// prevents reflector retries and one-shot reads from repeatedly invoking a
// failed exec plugin. An explicit retry grants exactly one /livez request.
type authenticationGate struct {
	mu           sync.Mutex
	blocked      bool
	probeAllowed bool
	// staticCredentials is set for a context whose credential is a client
	// certificate or bearer token rather than a plugin. There is nothing to
	// protect from re-running, and a 401 against such a context is as often
	// the link as the credential (a flaky connection through a proxy, an
	// apiserver mid-restart), so the gate never latches: traffic keeps
	// flowing and health's ordinary backoff decides when to retry.
	staticCredentials bool
	onBlocked         func(error)
	// responded latches the first HTTP response this client got back from
	// the server (any status below 500 — a 5xx may be a proxy answering for
	// an upstream it can't reach). It is proof the link works, which is the
	// premise connectGrace forgives timeouts on; see Cluster.linkProvenLocked.
	// The gate is per client, so a context switch starts without it.
	responded atomic.Bool
}

func (g *authenticationGate) hasResponded() bool { return g.responded.Load() }

func (g *authenticationGate) block(err error) {
	if g.staticCredentials {
		return
	}
	g.mu.Lock()
	first := !g.blocked
	g.blocked = true
	g.probeAllowed = false
	callback := g.onBlocked
	g.mu.Unlock()
	if first && callback != nil {
		callback(err)
	}
}

func (g *authenticationGate) permitProbe() {
	g.mu.Lock()
	if g.blocked {
		g.probeAllowed = true
	}
	g.mu.Unlock()
}

func (g *authenticationGate) open() {
	g.mu.Lock()
	g.blocked = false
	g.probeAllowed = false
	g.mu.Unlock()
}

func (g *authenticationGate) isBlocked() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.blocked
}

func (g *authenticationGate) allow(req *http.Request) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.blocked {
		return true
	}
	if req.URL.Path == "/livez" && g.probeAllowed {
		g.probeAllowed = false
		return true
	}
	return false
}

type observingRoundTripper struct {
	next     http.RoundTripper
	gate     *authenticationGate
	observer *watchObserver
}

func (t *observingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.gate.allow(req) {
		return nil, errAuthenticationGated
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		if IsAuthenticationError(err) {
			t.gate.block(err)
		}
		return nil, err
	}
	if resp.StatusCode < http.StatusInternalServerError {
		t.gate.responded.Store(true)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		t.gate.block(fmt.Errorf("kubernetes API returned %s", resp.Status))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && req.URL.Query().Get("watch") == "true" {
		if ended := t.observer.established(req); ended != nil {
			resp.Body = &observedWatchBody{ReadCloser: resp.Body, ended: ended}
		}
	}
	return resp, nil
}

// observedWatchBody makes the transport observer symmetrical: response
// headers prove a replacement WATCH was established, while the response
// body's end proves that established stream is no longer healthy. Relying
// only on SharedIndexInformer's WatchErrorHandler misses client-go recovery
// paths (notably an HTTP 410 followed by a streaming relist), allowing a
// successful /livez probe to turn the header green before the replacement
// WATCH exists.
//
// The body's end is not by itself a failure, though: the apiserver closes
// every WATCH cleanly when the reflector's own timeoutSeconds (5–10 min)
// expires, so io.EOF and errWatchStreamClosed are routine rotations — see
// Cluster.recordWatchEnded for how the two are told apart.
type observedWatchBody struct {
	io.ReadCloser
	ended func(error)
	once  sync.Once
}

func (b *observedWatchBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.finish(err)
	}
	return n, err
}

func (b *observedWatchBody) Close() error {
	err := b.ReadCloser.Close()
	if err != nil {
		b.finish(err)
	} else {
		b.finish(errWatchStreamClosed)
	}
	return err
}

// errWatchStreamClosed is the end a WATCH body reports when its reader closed
// it without a read error — client-go tearing the stream down on its own.
var errWatchStreamClosed = errors.New("kubernetes watch stream closed")

func (b *observedWatchBody) finish(err error) {
	b.once.Do(func() { b.ended(err) })
}

type watchRegistration struct {
	group, version, resource, namespace, fieldSelector string
}

type watchCallbacks struct {
	established func()
	ended       func(error)
}

type watchObserver struct {
	mu        sync.Mutex
	callbacks map[watchRegistration][]watchCallbacks
}

func newWatchObserver() *watchObserver {
	return &watchObserver{callbacks: map[watchRegistration][]watchCallbacks{}}
}

func (o *watchObserver) register(
	gvr schema.GroupVersionResource,
	namespace, fieldSelector string,
	established func(),
	ended func(error),
) {
	o.mu.Lock()
	o.callbacks[watchRegistration{gvr.Group, gvr.Version, gvr.Resource, namespace, fieldSelector}] = append(
		o.callbacks[watchRegistration{gvr.Group, gvr.Version, gvr.Resource, namespace, fieldSelector}],
		watchCallbacks{established: established, ended: ended},
	)
	o.mu.Unlock()
}

func (o *watchObserver) established(req *http.Request) func(error) {
	got, ok := watchRegistrationForRequest(req)
	if !ok {
		return nil
	}
	o.mu.Lock()
	callbacks := append([]watchCallbacks{}, o.callbacks[got]...)
	o.mu.Unlock()
	for _, callback := range callbacks {
		callback.established()
	}
	if len(callbacks) == 0 {
		return nil
	}
	return func(err error) {
		for _, callback := range callbacks {
			callback.ended(err)
		}
	}
}

func watchRegistrationForRequest(req *http.Request) (watchRegistration, bool) {
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	var got watchRegistration
	switch {
	case len(parts) >= 3 && parts[0] == "api":
		got.version = parts[1]
		parts = parts[2:]
	case len(parts) >= 4 && parts[0] == "apis":
		got.group, got.version = parts[1], parts[2]
		parts = parts[3:]
	default:
		return watchRegistration{}, false
	}
	if len(parts) >= 3 && parts[0] == "namespaces" {
		got.namespace = parts[1]
		parts = parts[2:]
	}
	if len(parts) != 1 {
		return watchRegistration{}, false
	}
	got.resource = parts[0]
	got.fieldSelector = req.URL.Query().Get("fieldSelector")
	return got, true
}
