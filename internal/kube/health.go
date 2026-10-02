package kube

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
)

// ConnPhase is the cluster connection's state machine (mvp-plan.md §0.7).
type ConnPhase string

const (
	ConnConnected    ConnPhase = "connected"
	ConnReconnecting ConnPhase = "reconnecting"
	ConnFailed       ConnPhase = "failed"
	ConnNoCluster    ConnPhase = "no-cluster"

	// ConnUnauthenticated is "there is no usable credential for this
	// cluster" — a 401 for a plugin-minted credential (a static cert/token
	// context retries a 401 as Reconnecting instead, see needsReauth), or an
	// exec credential plugin that
	// couldn't mint a token at all (an expired `aws sso login` session, a
	// `gcloud` re-auth the plugin can't prompt for because kute holds the
	// terminal). It is deliberately not Reconnecting: on a cert-based
	// cluster a 401 really is something broken and worth retrying, but on
	// GKE/EKS it means a plugin failed, and re-running a plugin that just
	// failed is not a retry strategy — it burns the link every two seconds
	// while telling the user "reconnecting", which is the wrong sentence.
	// The recovery is a human one (re-authenticate in another shell), so
	// this phase stops the ping loop and waits for 4a/4c's explicit r.
	ConnUnauthenticated ConnPhase = "unauthenticated"
)

// ConnState is a snapshot of connection health, fanned out on ConnStateMsg
// for the 4a offline banner/stale strip.
type ConnState struct {
	Phase       ConnPhase
	Latency     time.Duration
	Err         string // verbatim error for the 4a banner
	Attempt     int
	NextRetryAt time.Time
	FetchedAt   time.Time // last successful sync, for the ⧗ stale stamp
}

// ConnStateMsg is emitted (as a tea.Msg, like ResourceChangedMsg) whenever
// ConnState changes.
type ConnStateMsg ConnState

// Offline reports whether Phase is a mid-outage state (watch/ping failing,
// backoff retries under way) rather than a one-shot failure — the single
// predicate every "disconnected"/OFFLINE treatment (4a banner, header badge,
// mutating-verb gate) shares.
func (s ConnState) Offline() bool {
	return s.Phase == ConnReconnecting || s.Phase == ConnFailed || s.Phase == ConnUnauthenticated
}

// NeedsCredentials reports whether the outage is an authentication one, for
// the surfaces that say *why* rather than just that the cluster is offline:
// the header badge, 4a's banner (which has no backoff countdown to show
// here), and 4c's title. Everything else keeps treating it as Offline().
func (s ConnState) NeedsCredentials() bool { return s.Phase == ConnUnauthenticated }

const (
	pingInterval   = 2 * time.Second // matches the "sync 2s" header chip
	pingTimeout    = 10 * time.Second
	maxBackoff     = 30 * time.Second
	initialBackoff = time.Second

	// connectGrace is how long a *timed-out* /livez ping is forgiven while
	// informer caches are still doing an initial LIST. An informer's first
	// LIST pulls every object of its kind, and client-go multiplexes it over
	// a single HTTP/2 connection shared with the ping, so on a
	// bandwidth-constrained link (an SSH port-forward to a private cluster
	// is the motivating case) the ping queues behind megabytes of body and
	// blows its deadline while the link is perfectly healthy — the same
	// cluster reports a ~200ms ping the moment the LIST finishes.
	//
	// The window is measured from the most recent informer start, not from
	// connect, because informers now start on demand: opening the Secrets
	// list an hour into a session produces exactly the contention a cold
	// connect used to, and anchoring at connect would leave it unforgiven.
	//
	// Only timeouts are forgiven, and only while something is actually
	// listing: a refused connection, DNS failure, TLS error, or 401 still
	// flips to Reconnecting on the first ping so the 4c unreachable-context
	// screen appears as promptly as ever.
	//
	// And only once the link has been shown to work (see inConnectGrace's
	// reached argument): the premise is "a link we know works is busy". A
	// blackholed endpoint — a cluster that's down, a VPN that dropped —
	// fails with exactly the same timeouts, and forgiving those held a
	// launch on browse's spinner for the whole window (longer, since every
	// lazy informer start re-arms it) instead of handing over to 4c.
	connectGrace = 90 * time.Second
)

// backoffDelay is the reconnect wait for the given attempt (1-based):
// 1s→2s→4s→…→30s, capped. attempt <= 1 returns the initial delay.
func backoffDelay(attempt int) time.Duration {
	if attempt <= 1 {
		return initialBackoff
	}
	d := initialBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= maxBackoff {
			return maxBackoff
		}
	}
	return d
}

// health owns ConnState and its change channel. It's a separate type (not
// fields directly on Cluster) so the transition logic is easy to unit test
// without a live clientset.
type health struct {
	mu    sync.Mutex
	state ConnState
	ch    chan ConnStateMsg
	retry chan struct{}
	wake  chan struct{}
	// startedAt anchors connectGrace: the moment the most recent burst of
	// initial LIST traffic began. Stamped at construction, on reset (so a
	// SwitchContext into a slow cluster gets the same forgiveness a cold
	// launch does), and by noteListBurst whenever an informer starts.
	startedAt time.Time
	// staticCredentials mirrors authenticationGate.staticCredentials for the
	// active context: a 401 is then reported as Reconnecting with backoff,
	// not latched as Unauthenticated. See needsReauth.
	staticCredentials bool
	// stderr is the active context's credential-plugin capture (nil for a
	// context without an exec plugin). Set alongside staticCredentials.
	stderr *stderrCapture
}

func newHealth() *health {
	now := time.Now()
	return &health{
		state:     ConnState{Phase: ConnConnected, FetchedAt: now},
		ch:        make(chan ConnStateMsg, 8),
		retry:     make(chan struct{}, 1),
		wake:      make(chan struct{}, 1),
		startedAt: now,
	}
}

// reset restores health to a fresh Connected state without replacing ch/retry
// — used by Cluster.SwitchContext, which rebuilds the clientset/factory in
// place but must keep the same *Cluster identity: a caller already ranging
// over ConnEvents() (app.RunWithConfig's forwardEvents) holds a reference to
// this channel from program start, and swapping in a brand-new health (and
// therefore a brand-new channel) would silently orphan that reader for the
// rest of the process's life.
func (h *health) reset() {
	now := time.Now()
	h.mu.Lock()
	h.state = ConnState{Phase: ConnConnected, FetchedAt: now}
	h.startedAt = now
	h.mu.Unlock()
	// The probe timer may be parked on the unauthenticated phase's long wait.
	// A context switch must make it recompute against this fresh Connected
	// state instead of inheriting that pause from the old credentials.
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// setStaticCredentials records whether the active context authenticates with
// a static client certificate/token. Set alongside every authGate swap.
func (h *health) setStaticCredentials(v bool) {
	h.mu.Lock()
	h.staticCredentials = v
	h.mu.Unlock()
}

// setCredentialStderr points credential-failure messages at the active
// context's own plugin capture, discarding anything it already holds — a
// probe of this context, or its last session before a switch away, explains
// nothing about a failure from here on.
func (h *health) setCredentialStderr(s *stderrCapture) {
	s.clear()
	h.mu.Lock()
	h.stderr = s
	h.mu.Unlock()
}

// needsReauth reports whether err should latch ConnUnauthenticated. Only a
// plugin-minted credential qualifies: re-running a plugin that just failed
// can't help. A static cert/token context gets a 401 retried like any other
// failure — on a flaky link (microk8s behind a lossy connection, a proxy that
// answers 401 while its upstream is down) latching turned a transient blip
// into a screen that stayed "credentials expired" until the user pressed r.
// A credential that really is revoked still shows its 401 in the banner on
// every backoff attempt.
func (h *health) needsReauth(err error) bool {
	if !IsAuthenticationError(err) {
		return false
	}
	h.mu.Lock()
	static := h.staticCredentials
	h.mu.Unlock()
	return !static
}

// noteListBurst re-arms the connect-grace window because an informer has
// just been started and is about to pull its whole kind down the same
// connection the /livez ping uses.
func (h *health) noteListBurst() {
	h.mu.Lock()
	h.startedAt = time.Now()
	h.mu.Unlock()
}

func (h *health) get() ConnState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state
}

func (h *health) set(s ConnState) {
	h.mu.Lock()
	h.state = s
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
	select {
	case h.ch <- ConnStateMsg(s):
	default:
		// Consumer is behind: drop the oldest queued state, the new one
		// supersedes it.
		select {
		case <-h.ch:
		default:
		}
		select {
		case h.ch <- ConnStateMsg(s):
		default:
		}
	}
}

// onWatchError is the informer WatchErrorHandler: the first dropped
// connection flips to Reconnecting with the verbatim error and schedules a
// backoff retry. Once Reconnecting, the ping loop owns further attempts/
// backoff so repeated watch errors during the same outage don't reset it.
// synced/reached/now carry the same connectGrace gate recordPing uses — a
// reflector whose LIST times out while it's competing with twenty of its
// siblings for a constrained link is reporting congestion, not an outage.
func (h *health) onWatchError(err error, synced, reached bool, now time.Time) {
	prev := h.get()
	if prev.Phase == ConnReconnecting || prev.Phase == ConnUnauthenticated {
		// Already unauthenticated: reflectors go on retrying their watches
		// on their own schedule, and each failure would otherwise re-emit
		// the same message. The ping loop, driven by the user's r, owns
		// getting out of this phase.
		return
	}
	if h.needsReauth(err) {
		h.setUnauthenticated(err, 0, prev)
		return
	}
	if IsPermissionError(err) {
		// A 403 is not a connection fact, and this handler only reports
		// connection facts. The cluster is reachable and it knows who you
		// are; it has answered one kind's LIST with "no", which is exactly
		// the per-kind distinction IsAuthenticationError's doc comment draws
		// against a 401.
		//
		// Reporting it here made one forbidden kind — often a kind the user
		// never asked for, since the eager set and CRD discovery both run
		// unprompted — replace every working screen with "cluster is
		// unreachable" and a backoff countdown that could not help: there is
		// nothing to reconnect to, and the reflector will be refused again
		// on every retry for as long as the process lives.
		//
		// The denial is not dropped. Cluster.noteWatchError, called
		// immediately before this at every one of the three call sites,
		// latches it per-kind for KindForbidden, so the screen that reads
		// that kind shows 4b's card while the rest of the app carries on.
		return
	}
	if h.inConnectGrace(synced, reached, err, now) {
		return
	}
	attempt := prev.Attempt + 1
	h.set(ConnState{
		Phase:       ConnReconnecting,
		Err:         err.Error(),
		Attempt:     attempt,
		NextRetryAt: now.Add(backoffDelay(attempt)),
		FetchedAt:   prev.FetchedAt,
	})
}

// retryNow requests an immediate probe, bypassing the backoff wait.
func (h *health) retryNow() {
	select {
	case h.retry <- struct{}{}:
	default:
	}
}

// ConnEvents streams connection-state changes for app.Run to fan into the
// program alongside Events().
func (c *Cluster) ConnEvents() <-chan ConnStateMsg { return c.health.ch }

// ConnState returns the last known connection state.
func (c *Cluster) ConnState() ConnState { return c.health.get() }

// RetryNow requests an immediate reconnect probe (the 4a "r" key).
func (c *Cluster) RetryNow() {
	c.mu.Lock()
	if c.authGate != nil {
		c.authGate.permitProbe()
	}
	c.mu.Unlock()
	c.health.retryNow()
}

// healthProbeSnapshot binds a health loop to exactly one cluster generation.
// SwitchContext replaces all four fields under Cluster.mu and cancels ctx, so
// an old loop can neither issue a request through the replacement client nor
// apply a late result to the replacement gate or health state.
type healthProbeSnapshot struct {
	clientset  kubernetes.Interface
	gate       *authenticationGate
	generation int
	ctx        context.Context
}

// startHealthLoop pings /livez every pingInterval while healthy and follows
// ConnState.NextRetryAt while reconnecting. RetryNow bypasses either wait.
// It runs until stopCh closes.
func (c *Cluster) startHealthLoop(stopCh <-chan struct{}, probe healthProbeSnapshot) {
	defer utilruntime.HandleCrash()
	timer := time.NewTimer(nextProbeDelay(c.health.get(), time.Now()))
	defer timer.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-timer.C:
			c.ping(probe)
		case <-c.health.retry:
			c.ping(probe)
		case <-c.health.wake:
			// A watch failure may schedule a retry sooner than the healthy
			// interval already on the timer. Recompute without probing now.
		}
		resetProbeTimer(timer, c.health.get(), time.Now())
	}
}

func nextProbeDelay(state ConnState, now time.Time) time.Duration {
	if state.Phase == ConnUnauthenticated {
		// Only RetryNow wakes this phase. A long timer keeps the select shape
		// simple without repeatedly invoking an expired credential plugin.
		return 24 * time.Hour
	}
	if state.Phase == ConnReconnecting || state.Phase == ConnFailed {
		if delay := state.NextRetryAt.Sub(now); !state.NextRetryAt.IsZero() && delay > 0 {
			return delay
		}
		return 0
	}
	return pingInterval
}

func resetProbeTimer(timer *time.Timer, state ConnState, now time.Time) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(nextProbeDelay(state, now))
}

func (c *Cluster) ping(probe healthProbeSnapshot) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(probe.ctx, pingTimeout)
	defer cancel()
	err := probe.clientset.Discovery().RESTClient().Get().AbsPath("/livez").Do(ctx).Error()
	// Resolved before the lock: rendering a credential failure waits on the
	// plugin's stderr, which must not hold c.mu (see resolvedCredentialError).
	err = c.health.resolveCredentialError(err)

	// Keep the generation check and every result-side effect in the same
	// critical section as SwitchContext's generation bump, field replacement,
	// and health reset. Checking before this lock and writing afterward would
	// leave the same stale-result window in a smaller form.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != probe.generation {
		return
	}
	if err == nil && probe.gate != nil {
		probe.gate.open()
	}
	if err != nil && probe.gate != nil && probe.gate.isBlocked() && !IsAuthenticationError(err) {
		return
	}
	if err == nil {
		// /livez answered, so this cluster has demonstrably been reached —
		// independent of whether any cache has filled yet.
		c.reached = true
	}
	// Latch from cache state in the same critical section that produces the
	// ConnState the 4c swap reads, so an outage reported here can never be
	// mistaken for a cluster that was never reachable (see Reached).
	c.noteReachedIfAnyCacheSyncedLocked()
	c.health.recordPing(time.Since(start), err, c.allStartedKindsReadyLocked(), c.linkProvenLocked(), time.Now())
}

// recordPing folds one /livez result into ConnState. synced is the informer
// caches' sync state and reached whether this generation has ever had an
// answer from the apiserver; both gate connectGrace (see its doc comment).
// now is injected so the grace window is testable without sleeping.
func (h *health) recordPing(latency time.Duration, err error, synced, reached bool, now time.Time) {
	prev := h.get()

	if err == nil {
		// /livez succeeding says the endpoint is reachable, not that the
		// resource watches which made the cache useful are healthy again.
		if !synced && prev.Offline() {
			// Keep the outage's advertised backoff moving forward. Leaving the
			// expired NextRetryAt in place would reset the timer to zero and turn
			// a healthy /livez plus broken WATCH into a tight probe loop.
			attempt := prev.Attempt + 1
			h.set(ConnState{
				Phase:       ConnReconnecting,
				Latency:     latency,
				Err:         prev.Err,
				Attempt:     attempt,
				NextRetryAt: now.Add(backoffDelay(attempt)),
				FetchedAt:   prev.FetchedAt,
			})
			return
		}
		// Whatever the plugin printed on the way to this success (a
		// deprecation warning on every mint) must not sit in the ring until
		// the next real failure, hours later, and be reported as its cause.
		h.mu.Lock()
		stderr := h.stderr
		h.mu.Unlock()
		stderr.clear()
		h.set(ConnState{Phase: ConnConnected, Latency: latency, FetchedAt: now})
		return
	}

	if h.needsReauth(err) {
		// Unconditionally re-emitted, unlike onWatchError's silent case:
		// while unauthenticated the ticker stops pinging, so the only way to
		// reach this line twice is the user pressing r — which has to
		// produce a visible answer even when the answer is the same.
		h.setUnauthenticated(err, latency, prev)
		return
	}

	if h.inConnectGrace(synced, reached, err, now) {
		return
	}

	attempt := prev.Attempt
	if prev.Phase != ConnReconnecting {
		attempt = 0
	}
	attempt++
	h.set(ConnState{
		Phase:       ConnReconnecting,
		Latency:     latency,
		Err:         err.Error(),
		Attempt:     attempt,
		NextRetryAt: now.Add(backoffDelay(attempt)),
		FetchedAt:   prev.FetchedAt,
	})
}

// setUnauthenticated records a credential failure. Attempt and NextRetryAt
// are deliberately left zero: there is no scheduled retry to count down to,
// and 4a/4c read exactly that to swap the backoff countdown for the
// re-authenticate hint.
//
// One failure is routinely reported twice: the auth gate's first-block
// callback takes the plugin's stderr, then the request that tripped it (most
// often the ping) reports the same failure with the ring already drained. So
// a report that only restates a failure already on screen keeps the message
// that named it, rather than replacing "run aws sso login" with client-go's
// "exec: executable aws failed with exit code 255".
func (h *health) setUnauthenticated(err error, latency time.Duration, prev ConnState) {
	msg, fromPlugin := h.credentialMessage(err)
	if !fromPlugin && prev.Phase == ConnUnauthenticated && prev.Err != "" && restatesCredentialFailure(err) {
		msg = prev.Err
	}
	h.set(ConnState{
		Phase:     ConnUnauthenticated,
		Latency:   latency,
		Err:       msg,
		FetchedAt: prev.FetchedAt,
	})
}

// pausesPolling reports whether the periodic /livez ping should be skipped —
// true only while unauthenticated, where each ping re-runs a credential
// plugin that has already failed and cannot succeed until the user does
// something outside kute. An explicit RetryNow always pings regardless.
func (h *health) pausesPolling() bool { return h.get().Phase == ConnUnauthenticated }

// inConnectGrace reports whether a failed probe should be swallowed as
// startup congestion rather than reported as an outage. Deliberately blind to
// the current phase: keying off "we're still Connected" would mean a single
// unlucky watch error early in the sync flipped us to Reconnecting and made
// every subsequent congested ping reportable again, which is the exact
// banner storm this exists to prevent.
//
// reached is the evidence the link works at all (Cluster.linkProvenLocked).
// Without it a timeout is indistinguishable from a blackholed endpoint, and
// is reported at once: congestion needs a connection to be congested.
func (h *health) inConnectGrace(synced, reached bool, err error, now time.Time) bool {
	if synced || !reached || !isTimeout(err) {
		return false
	}
	h.mu.Lock()
	startedAt := h.startedAt
	h.mu.Unlock()
	return now.Sub(startedAt) < connectGrace
}

// isTimeout distinguishes "the link is busy or slow" from "the link is broken"
// — the ping's own deadline expiring, or any net.Error that self-reports as a
// timeout. Everything else (refused, DNS, TLS, HTTP status) is a real failure.
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
