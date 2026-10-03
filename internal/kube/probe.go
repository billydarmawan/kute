package kube

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

// ProbeResult is one kubeconfig context's reachability check.
type ProbeResult struct {
	Name    string
	Latency time.Duration
	Err     error
	// Skipped marks a context deliberately left unprobed (7a's
	// credential-plugin contexts until ctrl+r) — neither reachable nor not.
	Skipped bool
}

// probeTimeout bounds one context's reachability check. Generous rather than
// snappy: results stream in as they land, so a slow-but-reachable context
// (a bastion/port-forwarded cluster, or any context probed while the active
// cluster's informers are saturating the same link) costs a later verdict,
// whereas a tight deadline costs a *wrong* one — an "unreachable" label on a
// context that works fine.
const probeTimeout = 6 * time.Second

// probeConcurrency bounds how many contexts are probed at once. Each probe is
// a fresh client plus a TLS handshake, and a large corporate kubeconfig is the
// case this exists for.
const probeConcurrency = 8

// ProbeContexts probes every named kubeconfig context concurrently — for
// each, build a rest.Config (no caching; this is a one-shot check, not a
// long-lived client) and hit /livez with probeTimeout — and streams results
// as they complete. Used by the context palette (7a) and the
// unreachable-at-launch screen (4c) to show reachability + latency in the
// background while the user browses. The channel closes once every context
// has reported.
func ProbeContexts(ctx context.Context, names []string) <-chan ProbeResult {
	return probeContextsWith(ctx, names, defaultProbe)
}

// probeFunc pings one named context. Factored out so probe fan-out
// (concurrency, result delivery, channel close) can be unit-tested with a
// fake instead of a live cluster (mvp-plan.md Phase 0 verification).
type probeFunc func(ctx context.Context, name string) (time.Duration, error)

func probeContextsWith(ctx context.Context, names []string, probe probeFunc) <-chan ProbeResult {
	out := make(chan ProbeResult, len(names))
	// Bounded: every probe past the first builds its own rest.Config and does
	// its own TLS handshake, so an unbounded fan-out meant a 50-context
	// corporate kubeconfig opened 50 concurrent handshakes the instant the
	// context palette was opened. SetLimit blocks at g.Go, so the bound is on
	// goroutines, not just on requests in flight — which is exactly why the
	// launch loop runs on its own goroutine rather than the caller's: the
	// callers sit on the Bubble Tea update loop, and with more than
	// probeConcurrency names a caller-side loop froze the UI until a whole
	// wave of probes had finished (unboundedly, behind a hung exec plugin).
	go func() {
		defer utilruntime.HandleCrash()
		defer close(out) // every probe reports through out, never through error
		g := new(errgroup.Group)
		g.SetLimit(probeConcurrency)
		for _, name := range names {
			if ctx.Err() != nil {
				// Cancelled (palette closed, re-probed, or quit): nobody is
				// waiting for the rest, so don't start them.
				break
			}
			g.Go(func() error {
				latency, err := probeBounded(ctx, name, probe)
				out <- ProbeResult{Name: name, Latency: latency, Err: err}
				return nil
			})
		}
		_ = g.Wait()
	}()
	return out
}

// probeWallTimeout bounds one probe's wall time independently of the probe
// itself. probeTimeout only reaches the /livez request; an exec credential
// plugin runs inside client-go's RoundTrip via exec.Command with no context
// at all, so a plugin waiting on a browser login or an SSO endpoint would
// otherwise hold its concurrency slot (and its row's verdict) forever. The
// margin over probeTimeout lets an ordinary slow request report its own
// error first.
const probeWallTimeout = probeTimeout + 2*time.Second

// errProbeTimeout is a probe abandoned at probeWallTimeout.
var errProbeTimeout = fmt.Errorf("probe timed out after %s", probeWallTimeout)

// probeBounded runs probe but stops waiting for it at probeWallTimeout or
// when ctx is cancelled. An abandoned probe's goroutine lives on until the
// plugin itself exits — client-go gives no way to kill it — but its result
// is dropped into a buffered channel nobody reads, so it never blocks.
func probeBounded(ctx context.Context, name string, probe probeFunc) (time.Duration, error) {
	type result struct {
		latency time.Duration
		err     error
	}
	done := make(chan result, 1)
	go func() {
		defer utilruntime.HandleCrash()
		latency, err := probe(ctx, name)
		done <- result{latency, err}
	}()
	timer := time.NewTimer(probeWallTimeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.latency, r.err
	case <-timer.C:
		return probeWallTimeout, errProbeTimeout
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func defaultProbe(ctx context.Context, name string) (time.Duration, error) {
	client, err := NewClientForContext(name)
	if err != nil {
		return 0, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	start := time.Now()
	err = client.Interface.Discovery().RESTClient().Get().AbsPath("/livez").Do(probeCtx).Error()
	return time.Since(start), err
}
