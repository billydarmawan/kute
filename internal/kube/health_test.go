package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestBackoffDelaySchedule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, time.Second},
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second}, // 32s would exceed the cap
		{7, 30 * time.Second},
		{100, 30 * time.Second},
	}
	for _, tt := range tests {
		if got := backoffDelay(tt.attempt); got != tt.want {
			t.Errorf("backoffDelay(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestHealthOnWatchErrorFlipsToReconnecting(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.onWatchError(errors.New("dial tcp: i/o timeout"), true, true, time.Now())

	got := h.get()
	if got.Phase != ConnReconnecting {
		t.Fatalf("Phase = %v, want Reconnecting", got.Phase)
	}
	if got.Err != "dial tcp: i/o timeout" {
		t.Fatalf("Err = %q, want verbatim error", got.Err)
	}
	if got.Attempt != 1 {
		t.Fatalf("Attempt = %d, want 1", got.Attempt)
	}
	if !got.NextRetryAt.After(time.Now()) {
		t.Fatalf("NextRetryAt should be in the future")
	}
}

func TestHealthOnWatchErrorDoesNotResetAttemptWhileReconnecting(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.onWatchError(errors.New("first error"), true, true, time.Now())
	first := h.get()

	h.onWatchError(errors.New("second error, same outage"), true, true, time.Now())
	second := h.get()

	if second.Attempt != first.Attempt {
		t.Fatalf("Attempt changed from %d to %d on a repeated watch error mid-outage", first.Attempt, second.Attempt)
	}
	if second.Err != first.Err {
		t.Fatalf("Err should stay from the first watch error, got %q", second.Err)
	}
}

func TestHealthSetEmitsOnChannel(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.set(ConnState{Phase: ConnFailed})

	select {
	case msg := <-h.ch:
		if msg.Phase != ConnFailed {
			t.Fatalf("got phase %v, want Failed", msg.Phase)
		}
	default:
		t.Fatalf("expected a ConnStateMsg on the channel")
	}
}

func TestHealthSetDropsOldestWhenChannelFull(t *testing.T) {
	t.Parallel()
	h := newHealth()
	// Fill the buffered channel (capacity 8) past its limit.
	for i := 0; i < 10; i++ {
		h.set(ConnState{Phase: ConnConnected, Attempt: i})
	}
	// The struct's own state must reflect the latest set regardless of
	// channel backpressure.
	if got := h.get().Attempt; got != 9 {
		t.Fatalf("get().Attempt = %d, want 9 (latest)", got)
	}
}

func TestHealthRetryNowIsNonBlocking(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.retryNow()
	h.retryNow() // second call must not block even though the buffer is 1
	select {
	case <-h.retry:
	default:
		t.Fatalf("expected a pending retry signal")
	}
}

func TestNewHealthStartsConnected(t *testing.T) {
	t.Parallel()
	h := newHealth()
	got := h.get()
	if got.Phase != ConnConnected {
		t.Fatalf("initial Phase = %v, want Connected", got.Phase)
	}
	if got.FetchedAt.IsZero() {
		t.Fatalf("expected FetchedAt to be stamped")
	}
}

// timeoutErr mimics what client-go hands back when the ping's own context
// deadline fires: the transport error wrapped in a *url.Error.
func timeoutErr() error {
	return &url.Error{
		Op:  "Get",
		URL: "https://localhost:10443/livez",
		Err: context.DeadlineExceeded,
	}
}

func TestIsTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"bare deadline exceeded", context.DeadlineExceeded, true},
		{"wrapped in url.Error", timeoutErr(), true},
		{"net.Error reporting timeout", &net.DNSError{IsTimeout: true}, true},
		{"connection refused", errors.New("dial tcp 127.0.0.1:10443: connect: connection refused"), false},
		{"unauthorized", errors.New("the server has asked for the client to provide credentials"), false},
	}
	for _, tt := range tests {
		if got := isTimeout(tt.err); got != tt.want {
			t.Errorf("isTimeout(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestRecordPingForgivesTimeoutDuringInitialSync is the SSH-tunnel fix: while
// the informer caches are still syncing, Start()'s 21 cluster-wide LISTs
// saturate the link and the /livez ping blows its deadline. That must not
// surface as an outage.
func TestRecordPingForgivesTimeoutDuringInitialSync(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.recordPing(pingTimeout, timeoutErr(), false, true, h.startedAt.Add(30*time.Second))

	if got := h.get(); got.Phase != ConnConnected {
		t.Fatalf("Phase = %v, want Connected (timeout mid-sync is startup congestion)", got.Phase)
	}
	select {
	case msg := <-h.ch:
		t.Fatalf("a forgiven ping must not emit a ConnStateMsg, got %+v", msg)
	default:
	}
}

func TestRecordPingReportsTimeoutOnceCachesSynced(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.recordPing(pingTimeout, timeoutErr(), true, true, h.startedAt.Add(30*time.Second))

	got := h.get()
	if got.Phase != ConnReconnecting {
		t.Fatalf("Phase = %v, want Reconnecting (post-sync timeout is a real outage)", got.Phase)
	}
	if got.Attempt != 1 {
		t.Fatalf("Attempt = %d, want 1", got.Attempt)
	}
}

func TestRecordPingReportsTimeoutPastConnectGrace(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.recordPing(pingTimeout, timeoutErr(), false, true, h.startedAt.Add(connectGrace+time.Second))

	if got := h.get(); got.Phase != ConnReconnecting {
		t.Fatalf("Phase = %v, want Reconnecting — the grace window is bounded, not open-ended", got.Phase)
	}
}

// TestRecordPingReportsNonTimeoutImmediately pins the 4c path: an unreachable
// context still has to reach the setup screen on the first ping, so only
// timeouts are forgiven.
func TestRecordPingReportsNonTimeoutImmediately(t *testing.T) {
	t.Parallel()
	h := newHealth()
	refused := errors.New("dial tcp 127.0.0.1:10443: connect: connection refused")
	h.recordPing(time.Millisecond, refused, false, true, h.startedAt.Add(2*time.Second))

	got := h.get()
	if got.Phase != ConnReconnecting {
		t.Fatalf("Phase = %v, want Reconnecting on a refused connection mid-sync", got.Phase)
	}
	if got.Err != refused.Error() {
		t.Fatalf("Err = %q, want the verbatim error", got.Err)
	}
}

// TestRecordPingAdvancesAttemptMidOutage: post-sync, a timeout during an
// outage already in progress keeps advancing the attempt/backoff.
func TestRecordPingAdvancesAttemptMidOutage(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.onWatchError(errors.New("watch dropped"), true, true, time.Now())
	h.recordPing(pingTimeout, timeoutErr(), true, true, time.Now())

	got := h.get()
	if got.Phase != ConnReconnecting {
		t.Fatalf("Phase = %v, want Reconnecting", got.Phase)
	}
	if got.Attempt != 2 {
		t.Fatalf("Attempt = %d, want 2 (the outage's attempts keep accruing)", got.Attempt)
	}
}

// TestConnectGraceIsPhaseBlind pins the deliberate choice in inConnectGrace:
// a lone watch error early in the sync must not re-arm the banner storm for
// every congested ping that follows it.
func TestConnectGraceIsPhaseBlind(t *testing.T) {
	t.Parallel()
	h := newHealth()
	// A reflector LIST times out mid-sync and flips us to Reconnecting...
	h.onWatchError(timeoutErr(), true, true, time.Now())
	if h.get().Phase != ConnReconnecting {
		t.Fatalf("precondition: expected the watch error to flip to Reconnecting")
	}
	before := h.get()

	// ...but the congested pings behind it are still forgiven while syncing.
	h.recordPing(pingTimeout, timeoutErr(), false, true, h.startedAt.Add(5*time.Second))

	if got := h.get(); got.Attempt != before.Attempt {
		t.Fatalf("Attempt advanced %d→%d on a forgiven mid-sync ping", before.Attempt, got.Attempt)
	}
}

// TestOnWatchErrorForgivesTimeoutDuringInitialSync: the reflector's own LIST
// competing with twenty siblings for a constrained link is congestion, not an
// outage — same rule the ping path follows.
func TestOnWatchErrorForgivesTimeoutDuringInitialSync(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.onWatchError(timeoutErr(), false, true, h.startedAt.Add(10*time.Second))

	if got := h.get(); got.Phase != ConnConnected {
		t.Fatalf("Phase = %v, want Connected (LIST timeout mid-sync is congestion)", got.Phase)
	}
}

// TestOnWatchErrorReportsNonTimeoutDuringInitialSync: a genuinely broken link
// still flips immediately, even mid-sync.
func TestOnWatchErrorReportsNonTimeoutDuringInitialSync(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.onWatchError(errors.New("connect: connection refused"), false, true, h.startedAt.Add(time.Second))

	if got := h.get(); got.Phase != ConnReconnecting {
		t.Fatalf("Phase = %v, want Reconnecting on a refused connection", got.Phase)
	}
}

func TestRecordPingSuccessClearsOutage(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.onWatchError(errors.New("watch dropped"), true, true, time.Now())
	h.recordPing(226*time.Millisecond, nil, true, true, time.Now())

	got := h.get()
	if got.Phase != ConnConnected {
		t.Fatalf("Phase = %v, want Connected", got.Phase)
	}
	if got.Latency != 226*time.Millisecond {
		t.Fatalf("Latency = %v, want the measured round trip", got.Latency)
	}
	if got.Attempt != 0 {
		t.Fatalf("Attempt = %d, want 0 after recovery", got.Attempt)
	}
}

func TestRecordPingSuccessWaitsForWatchRecovery(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.onWatchError(errors.New("watch dropped"), true, true, time.Now())
	before := h.get()
	now := time.Now()
	h.recordPing(12*time.Millisecond, nil, false, true, now)

	got := h.get()
	if got.Phase != ConnReconnecting {
		t.Fatalf("Phase = %v, want Reconnecting while a resource watch is still unhealthy", got.Phase)
	}
	if got.Attempt != before.Attempt+1 {
		t.Fatalf("Attempt = %d, want %d so a livez-only success keeps backoff moving", got.Attempt, before.Attempt+1)
	}
	if want := now.Add(backoffDelay(got.Attempt)); !got.NextRetryAt.Equal(want) {
		t.Fatalf("NextRetryAt = %s, want %s", got.NextRetryAt, want)
	}
}

func TestNextProbeDelayFollowsAdvertisedBackoff(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for _, tt := range []struct {
		name  string
		state ConnState
		want  time.Duration
	}{
		{name: "healthy", state: ConnState{Phase: ConnConnected}, want: pingInterval},
		{name: "first retry", state: ConnState{Phase: ConnReconnecting, NextRetryAt: now.Add(time.Second)}, want: time.Second},
		{name: "later retry", state: ConnState{Phase: ConnReconnecting, NextRetryAt: now.Add(8 * time.Second)}, want: 8 * time.Second},
		{name: "retry due", state: ConnState{Phase: ConnReconnecting, NextRetryAt: now.Add(-time.Second)}, want: 0},
		{name: "credentials pause", state: ConnState{Phase: ConnUnauthenticated}, want: 24 * time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextProbeDelay(tt.state, now); got != tt.want {
				t.Fatalf("nextProbeDelay() = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestHealthResetRestampsConnectGrace: a SwitchContext into a slow cluster
// gets the same startup forgiveness a cold launch does.
func TestHealthResetRestampsConnectGrace(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.startedAt = time.Now().Add(-time.Hour) // pretend the process is old

	h.reset()
	h.recordPing(pingTimeout, timeoutErr(), false, true, time.Now())

	if got := h.get(); got.Phase != ConnConnected {
		t.Fatalf("Phase = %v, want Connected — reset() must restamp the grace window", got.Phase)
	}
}

// TestHealthResetPreservesChannelIdentity pins the SwitchContext fix
// (cluster.go now calls health.reset() instead of replacing health
// wholesale via newHealth()): a caller already ranging over the original
// ch (app.RunWithConfig's forwardEvents, which reads ConnEvents() once at
// program start) must keep receiving events after a context switch, which
// only holds if reset() reuses the same channel rather than building a new
// health struct.
func TestHealthResetPreservesChannelIdentity(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.onWatchError(errors.New("boom"), true, true, time.Now())
	origCh, origRetry, origWake := h.ch, h.retry, h.wake
	// Drain the watch-error wake so reset has to publish its own signal.
	select {
	case <-h.wake:
	default:
		t.Fatal("watch error did not wake the probe scheduler")
	}

	h.reset()

	if h.ch != origCh {
		t.Fatalf("reset() replaced ch — a reader from before the reset is now orphaned")
	}
	if h.retry != origRetry {
		t.Fatalf("reset() replaced retry — RetryNow()'s existing signal path is now orphaned")
	}
	if h.wake != origWake {
		t.Fatalf("reset() replaced wake — the health loop is now orphaned")
	}
	select {
	case <-h.wake:
	default:
		t.Fatal("reset() did not wake a probe timer parked by the old context")
	}
	got := h.get()
	if got.Phase != ConnConnected {
		t.Fatalf("Phase after reset = %v, want Connected", got.Phase)
	}
	if got.Attempt != 0 {
		t.Fatalf("Attempt after reset = %d, want 0", got.Attempt)
	}
}

// TestHealthProbeDropsResultsFromReplacedGeneration pins both corruptions a
// probe already inside RoundTrip could cause after SwitchContext replaced the
// cluster fields. The transport deliberately ignores cancellation: request
// cancellation is best-effort, while the generation check is the authority
// that prevents a late result from landing.
func TestHealthProbeDropsResultsFromReplacedGeneration(t *testing.T) {
	for _, tt := range []struct {
		name      string
		resultErr error
	}{
		{name: "success cannot open replacement authentication gate"},
		{name: "failure cannot overwrite replacement health", resultErr: errors.New("old context refused connection")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				close(started)
				<-release
				if tt.resultErr != nil {
					return nil, tt.resultErr
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("ok")),
					Request:    req,
				}, nil
			})
			oldClient, err := kubernetes.NewForConfig(&rest.Config{
				Host:      "https://old.example.test",
				Transport: transport,
			})
			if err != nil {
				t.Fatalf("build old client: %v", err)
			}

			oldGate := &authenticationGate{blocked: true}
			probeCtx, cancelProbe := context.WithCancel(t.Context())
			c := &Cluster{
				clientset:         oldClient,
				authGate:          oldGate,
				generation:        7,
				health:            newHealth(),
				cancelHealthProbe: cancelProbe,
			}
			probe := healthProbeSnapshot{
				clientset:  oldClient,
				gate:       oldGate,
				generation: c.generation,
				ctx:        probeCtx,
			}

			done := make(chan struct{})
			go func() {
				defer close(done)
				c.ping(probe)
			}()
			<-started

			// SwitchContext's replacement critical section: invalidate and
			// cancel the old probe before installing the new generation's gate
			// and resetting its health.
			newGate := &authenticationGate{blocked: true}
			c.mu.Lock()
			c.generation++
			c.cancelHealthProbe()
			c.cancelHealthProbe = nil
			c.authGate = newGate
			c.health.reset()
			c.mu.Unlock()

			select {
			case <-probeCtx.Done():
			default:
				t.Fatal("replacement did not cancel the old probe context")
			}
			close(release)
			<-done

			if !oldGate.isBlocked() {
				t.Fatal("stale success opened the old authentication gate instead of being discarded")
			}
			if !newGate.isBlocked() {
				t.Fatal("stale success opened the replacement authentication gate")
			}
			if got := c.health.get(); got.Phase != ConnConnected || got.Err != "" {
				t.Fatalf("replacement health = %+v; stale probe result was not discarded", got)
			}
		})
	}
}

// unauthorized is a real apiserver 401 as client-go surfaces it.
func unauthorized() error {
	return apierrors.NewUnauthorized("Unauthorized")
}

// credentialPluginFailure is what client-go's exec round tripper returns when
// the plugin binary itself fails — no HTTP status involved, so no typed
// apierror to match on.
func credentialPluginFailure() error {
	return fmt.Errorf(`Get "https://example.eks.amazonaws.com/livez": getting credentials: exec: executable aws failed with exit code 255`)
}

func TestHealthPingClassifies401AsUnauthenticated(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.recordPing(12*time.Millisecond, unauthorized(), true, true, time.Now())

	got := h.get()
	if got.Phase != ConnUnauthenticated {
		t.Fatalf("Phase = %v, want Unauthenticated", got.Phase)
	}
	if !got.Offline() {
		t.Error("Offline() = false, want true — the 4a banner and mutating-verb gate key off it")
	}
	if !got.NeedsCredentials() {
		t.Error("NeedsCredentials() = false, want true")
	}
	if got.Attempt != 0 || !got.NextRetryAt.IsZero() {
		t.Errorf("Attempt/NextRetryAt = %d/%v, want no scheduled retry", got.Attempt, got.NextRetryAt)
	}
}

func TestHealthPingClassifiesCredentialPluginFailureAsUnauthenticated(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.recordPing(0, credentialPluginFailure(), true, true, time.Now())

	got := h.get()
	if got.Phase != ConnUnauthenticated {
		t.Fatalf("Phase = %v, want Unauthenticated — a plugin that can't mint a token is not a network outage", got.Phase)
	}
	if !strings.Contains(got.Err, "exit code 255") {
		t.Errorf("Err = %q, want the plugin failure verbatim", got.Err)
	}
}

func TestHealthStopsPollingWhileUnauthenticated(t *testing.T) {
	t.Parallel()
	h := newHealth()
	if h.pausesPolling() {
		t.Fatal("pausesPolling() = true while connected")
	}
	h.recordPing(0, unauthorized(), true, true, time.Now())
	if !h.pausesPolling() {
		t.Error("pausesPolling() = false while unauthenticated, want the ping loop to stop re-running the plugin")
	}
	// …but an explicit retry that succeeds still gets us out.
	h.recordPing(5*time.Millisecond, nil, true, true, time.Now())
	if got := h.get(); got.Phase != ConnConnected {
		t.Errorf("Phase = %v after a successful retry, want Connected", got.Phase)
	}
}

func TestHealthWatchErrorDoesNotChurnWhileUnauthenticated(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.onWatchError(unauthorized(), true, true, time.Now())
	if got := h.get(); got.Phase != ConnUnauthenticated {
		t.Fatalf("Phase = %v, want Unauthenticated", got.Phase)
	}
	// Drain the initial transition, then prove the reflectors' own retries
	// don't queue a message apiece.
	<-h.ch
	h.onWatchError(unauthorized(), true, true, time.Now())
	h.onWatchError(errors.New("dial tcp: connection refused"), true, true, time.Now())
	select {
	case msg := <-h.ch:
		t.Errorf("watch errors re-emitted %v while already unauthenticated", msg.Phase)
	default:
	}
}

func TestHealthRetryReemitsUnauthenticated(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.recordPing(0, unauthorized(), true, true, time.Now())
	<-h.ch
	// 4a/4c's 'r' pings again; a still-failing credential has to say so.
	h.recordPing(0, unauthorized(), true, true, time.Now())
	select {
	case msg := <-h.ch:
		if msg.Phase != ConnUnauthenticated {
			t.Errorf("Phase = %v, want Unauthenticated", msg.Phase)
		}
	default:
		t.Error("an explicit retry produced no state change at all")
	}
}

func TestIsAuthenticationError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"401", unauthorized(), true},
		{"credential plugin failure", credentialPluginFailure(), true},
		{"403 is a permission denial, not an auth one", apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "p", errors.New("nope")), false},
		{"timeout", context.DeadlineExceeded, false},
		{"refused", errors.New("dial tcp 10.0.0.1:443: connect: connection refused"), false},
	}
	for _, tt := range tests {
		if got := IsAuthenticationError(tt.err); got != tt.want {
			t.Errorf("%s: IsAuthenticationError = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestCredentialErrorMessagePrefersPluginStderr(t *testing.T) {
	pluginStderr.append([]byte("Error loading SSO Token:\n  Token has expired\n"))
	got := credentialErrorMessage(unauthorized())
	if got != "Error loading SSO Token: Token has expired" {
		t.Errorf("credentialErrorMessage = %q, want the plugin's stderr flattened to one line", got)
	}
	// Consumed, so the next failure can't inherit it.
	if got := credentialErrorMessage(unauthorized()); got != "Unauthorized" {
		t.Errorf("credentialErrorMessage = %q, want client-go's own message once stderr is spent", got)
	}
}

func TestCredentialErrorMessageCapsLength(t *testing.T) {
	pluginStderr.append([]byte(strings.Repeat("x", maxCredentialErrorText+50)))
	got := credentialErrorMessage(nil)
	if len(got) > maxCredentialErrorText+len("…") {
		t.Errorf("credentialErrorMessage length = %d, want capped at %d", len(got), maxCredentialErrorText)
	}
}

func forbiddenWatchErr(resource string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", errors.New("nope"))
}

// TestOnWatchErrorIgnoresADenial: a 403 is not a connection fact. The cluster
// is reachable and knows exactly who you are — it has refused one kind's
// LIST, which is the per-kind distinction IsAuthenticationError's doc comment
// draws against a 401.
//
// Reported as an outage, one forbidden kind replaced every working screen
// with "cluster is unreachable" and a backoff countdown that could not help.
// Often a kind the user never asked for: the eager set and CRD discovery both
// run unprompted, so on a partially-granted cluster this fired before the
// first keypress.
func TestOnWatchErrorIgnoresADenial(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.onWatchError(forbiddenWatchErr("deployments"), true, true, time.Now())

	if got := h.get(); got.Phase != ConnConnected {
		t.Fatalf("Phase = %v, want Connected — a refused LIST says nothing about the connection", got.Phase)
	}
	select {
	case msg := <-h.ch:
		t.Errorf("a denial emitted a connection-state change (%v); it belongs to the kind, not the cluster", msg.Phase)
	default:
	}
}

// TestOnWatchErrorDenialLeavesAnOutageAlone: ignoring a denial must not mean
// clearing state either. If the link really is down, a forbidden kind's
// reflector failing alongside it is no evidence of recovery.
func TestOnWatchErrorDenialLeavesAnOutageAlone(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.onWatchError(errors.New("connect: connection refused"), true, true, time.Now())
	before := h.get()
	if before.Phase != ConnReconnecting {
		t.Fatalf("precondition: Phase = %v, want Reconnecting", before.Phase)
	}

	h.onWatchError(forbiddenWatchErr("secrets"), true, true, time.Now())

	if got := h.get(); got.Phase != ConnReconnecting || got.Err != before.Err {
		t.Fatalf("ConnState = %+v after a denial mid-outage, want the outage untouched (%+v)", got, before)
	}
}

// TestOnWatchErrorStillReportsRealOutages is the guard on the guard: the
// denial branch must not swallow anything else. A refused connection carries
// the word "connection", not "forbidden", and has to keep flipping the phase.
func TestOnWatchErrorStillReportsRealOutages(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.onWatchError(errors.New("dial tcp 10.0.0.1:443: connect: connection refused"), true, true, time.Now())

	if got := h.get(); got.Phase != ConnReconnecting {
		t.Fatalf("Phase = %v, want Reconnecting — this one really is an outage", got.Phase)
	}
}

// TestOnWatchErrorAuthenticationStillWins: a 401 is a whole-connection fact
// and keeps its own phase. Ordering matters — a credential failure whose
// message happens to contain "forbidden" must not be reclassified as a
// per-kind denial and silently dropped.
func TestOnWatchErrorAuthenticationStillWins(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.onWatchError(errors.New("getting credentials: exec: token is forbidden"), true, true, time.Now())

	if got := h.get(); got.Phase != ConnUnauthenticated {
		t.Fatalf("Phase = %v, want Unauthenticated — no credential means no connection at all", got.Phase)
	}
}

// A 401 against a cert/token context is retried on backoff like any other
// failure, and clears on the next successful ping without the user's r.
func TestHealthRetries401ForStaticCredentials(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.setStaticCredentials(true)

	h.onWatchError(unauthorized(), true, true, time.Now())
	got := h.get()
	if got.Phase != ConnReconnecting {
		t.Fatalf("watch 401: Phase = %v, want Reconnecting", got.Phase)
	}
	if got.NeedsCredentials() || h.pausesPolling() {
		t.Error("static-credential 401 paused polling / asked for re-authentication")
	}
	if got.NextRetryAt.IsZero() {
		t.Error("NextRetryAt is zero, want a scheduled backoff retry")
	}

	h.recordPing(12*time.Millisecond, unauthorized(), true, true, time.Now())
	if got := h.get(); got.Phase != ConnReconnecting || got.Attempt != 2 {
		t.Fatalf("ping 401: Phase/Attempt = %v/%d, want Reconnecting/2", got.Phase, got.Attempt)
	}
	h.recordPing(12*time.Millisecond, nil, true, true, time.Now())
	if got := h.get(); got.Phase != ConnConnected {
		t.Errorf("Phase = %v after a successful ping, want Connected", got.Phase)
	}
}
