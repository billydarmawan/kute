package kube

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestProbeContextsFansOutAllNames(t *testing.T) {
	t.Parallel()
	names := []string{"dev", "staging", "prod"}
	probe := func(_ context.Context, name string) (time.Duration, error) {
		return 5 * time.Millisecond, nil
	}

	seen := map[string]ProbeResult{}
	for r := range probeContextsWith(t.Context(), names, probe) {
		seen[r.Name] = r
	}
	if len(seen) != len(names) {
		t.Fatalf("got %d results, want %d", len(seen), len(names))
	}
	for _, n := range names {
		if _, ok := seen[n]; !ok {
			t.Fatalf("missing result for %q", n)
		}
	}
}

// TestProbeContextsRunsConcurrently asserts the fan-out is a fan-out.
//
// In a synctest bubble the clock is fake and advances only when every
// goroutine is blocked, so a concurrent fan-out takes *exactly* one probe's
// worth of time. Against the real clock this had to be written as "faster than
// serial", which is a threshold that CI load can cross for reasons that have
// nothing to do with the code.
func TestProbeContextsRunsConcurrently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		names := []string{"a", "b", "c", "d"}
		const perProbe = 40 * time.Millisecond
		probe := func(_ context.Context, name string) (time.Duration, error) {
			time.Sleep(perProbe)
			return perProbe, nil
		}

		start := time.Now()
		for range probeContextsWith(t.Context(), names, probe) {
		}
		if elapsed := time.Since(start); elapsed != perProbe {
			t.Fatalf("elapsed = %v, want exactly %v for a fan-out of %d", elapsed, perProbe, len(names))
		}
	})
}

// TestProbeContextsBoundsConcurrency pins the limit on the fan-out itself.
//
// Every probe builds its own rest.Config and does its own TLS handshake, so an
// unbounded fan-out meant a large corporate kubeconfig opened one connection
// per context the instant the palette was opened. Fake time makes the peak
// exact: each batch starts together, sleeps together, and finishes together.
func TestProbeContextsBoundsConcurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		names := make([]string, 5*probeConcurrency)
		for i := range names {
			names[i] = fmt.Sprintf("ctx-%d", i)
		}

		var mu sync.Mutex
		var inFlight, peak int
		probe := func(_ context.Context, _ string) (time.Duration, error) {
			mu.Lock()
			inFlight++
			peak = max(peak, inFlight)
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			return time.Millisecond, nil
		}

		var got int
		for range probeContextsWith(t.Context(), names, probe) {
			got++
		}
		if got != len(names) {
			t.Fatalf("got %d results, want %d", got, len(names))
		}
		if peak > probeConcurrency {
			t.Errorf("peak concurrency = %d, want at most %d", peak, probeConcurrency)
		}
	})
}

func TestProbeContextsPropagatesPerContextError(t *testing.T) {
	t.Parallel()
	names := []string{"reachable", "unreachable"}
	wantErr := errors.New("dial tcp: timeout")
	probe := func(_ context.Context, name string) (time.Duration, error) {
		if name == "unreachable" {
			return 0, wantErr
		}
		return time.Millisecond, nil
	}

	results := map[string]ProbeResult{}
	for r := range probeContextsWith(t.Context(), names, probe) {
		results[r.Name] = r
	}
	if results["reachable"].Err != nil {
		t.Fatalf("reachable context should have no error, got %v", results["reachable"].Err)
	}
	if !errors.Is(results["unreachable"].Err, wantErr) {
		t.Fatalf("unreachable context error = %v, want %v", results["unreachable"].Err, wantErr)
	}
}

func TestProbeContextsEmptyNamesClosesImmediately(t *testing.T) {
	t.Parallel()
	ch := probeContextsWith(t.Context(), nil, defaultProbe)
	count := 0
	for range ch {
		count++
	}
	if count != 0 {
		t.Fatalf("expected no results for empty names, got %d", count)
	}
}

// TestProbeContextsReturnsBeforeProbesFinish pins review finding 02 H1: the
// callers sit on the Bubble Tea update loop, so ProbeContexts must hand back
// its channel immediately however many contexts there are. The launch loop
// used to run on the caller's goroutine, where SetLimit's blocking g.Go
// stalled it until a whole wave of probes had finished — opening the context
// palette with more than probeConcurrency contexts froze the terminal.
func TestProbeContextsReturnsBeforeProbesFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		names := make([]string, 2*probeConcurrency+1)
		for i := range names {
			names[i] = fmt.Sprintf("ctx-%d", i)
		}
		release := make(chan struct{})
		probe := func(_ context.Context, _ string) (time.Duration, error) {
			<-release
			return time.Millisecond, nil
		}

		returned := make(chan (<-chan ProbeResult), 1)
		go func() { returned <- probeContextsWith(t.Context(), names, probe) }()
		synctest.Wait()
		var ch <-chan ProbeResult
		select {
		case ch = <-returned:
		default:
			close(release)
			<-returned
			t.Fatalf("probeContextsWith blocked its caller while %d probes were in flight", probeConcurrency)
		}
		close(release)
		n := 0
		for range ch {
			n++
		}
		if n != len(names) {
			t.Fatalf("got %d results, want %d", n, len(names))
		}
	})
}

// TestProbeContextsAbandonsHungProbe: an exec credential plugin runs with no
// context, so a probe can hang past its own request deadline. The fan-out
// must give up on it at probeWallTimeout, report a timeout, and close.
func TestProbeContextsAbandonsHungProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hang := make(chan struct{})
		defer close(hang)
		probe := func(_ context.Context, name string) (time.Duration, error) {
			if name == "hung" {
				<-hang // ignores ctx, like client-go's exec.Command
			}
			return time.Millisecond, nil
		}

		start := time.Now()
		results := map[string]ProbeResult{}
		for r := range probeContextsWith(t.Context(), []string{"ok", "hung"}, probe) {
			results[r.Name] = r
		}
		if elapsed := time.Since(start); elapsed != probeWallTimeout {
			t.Fatalf("elapsed = %v, want %v", elapsed, probeWallTimeout)
		}
		if results["ok"].Err != nil {
			t.Fatalf("ok: unexpected error %v", results["ok"].Err)
		}
		if !errors.Is(results["hung"].Err, errProbeTimeout) {
			t.Fatalf("hung: err = %v, want errProbeTimeout", results["hung"].Err)
		}
	})
}

// TestProbeContextsCancelStopsRun: closing the palette cancels the run's
// context, which must close the channel without waiting on hung probes and
// without starting the ones still queued behind the concurrency limit.
func TestProbeContextsCancelStopsRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hang := make(chan struct{})
		defer close(hang)
		var mu sync.Mutex
		started := 0
		probe := func(_ context.Context, _ string) (time.Duration, error) {
			mu.Lock()
			started++
			mu.Unlock()
			<-hang
			return 0, nil
		}
		names := make([]string, 3*probeConcurrency)
		for i := range names {
			names[i] = fmt.Sprintf("ctx-%d", i)
		}

		ctx, cancel := context.WithCancel(t.Context())
		ch := probeContextsWith(ctx, names, probe)
		synctest.Wait()
		cancel()
		start := time.Now()
		for range ch {
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("channel closed after %v, want immediately on cancel", elapsed)
		}
		mu.Lock()
		defer mu.Unlock()
		if started > 2*probeConcurrency {
			t.Fatalf("%d probes started after cancel; queued ones should be skipped", started)
		}
	})
}
