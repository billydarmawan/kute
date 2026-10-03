package kube

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
)

// TestStartStopConcurrentlyIsRaceFree pins the c.stopCh handoff between Start
// and Stop.
//
// Start reads stopCh to hand it to the health loop and to the cache-sync wait,
// and both of those reads used to happen after Start had released c.mu — while
// every writer of the field (Stop, SwitchContext) holds it. That is a plain
// data race, and the failure past the race detector is worse than a torn read:
// Stop's "this cluster is dead" sentinel is a *nil* stopCh, so losing the race
// handed startHealthLoop a nil channel and panicked the process on the first
// ping, and handed cache.WaitForCacheSync a nil stop channel it never returns
// from.
//
// Fresh cluster per iteration because Start latches c.started and Stop does not
// clear it — a loop over one cluster would only ever exercise the first Start.
func TestStartStopConcurrentlyIsRaceFree(t *testing.T) {
	for range 20 {
		c, _ := newLazyTestCluster()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			// Three legal outcomes: Start wins outright; Stop wins and Start
			// refuses the dead cluster; or Stop lands mid-connect and cuts the
			// cache wait short. Anything else — a panic, a nil stop channel
			// reaching an informer, a hang — is the bug.
			err := c.Start(t.Context())
			if err != nil && !errors.Is(err, ErrClusterStopped) && !errors.Is(err, ErrCacheSyncFailed) {
				t.Errorf("Start: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			c.Stop()
		}()
		wg.Wait()

		c.Stop()
	}
}

// TestWaitForCacheSyncReleasesOnContextCancel pins that a cancelled wait does
// not strand the goroutine blocked inside cache.WaitForCacheSync.
//
// The caches here never settle, so nothing but the cancellation can end the
// wait. That is the shape of the bug: WaitForCacheSync knows nothing about
// context, so handing it the cluster's own stopCh left this goroutine blocked
// until the whole cluster was torn down — the normal path whenever
// SwitchContext's or attemptReconnect's timeout expires against a slow cluster.
//
// The synctest bubble is what makes the assertion exact rather than a goroutine
// census: it does not finish while a goroutine started inside it is still
// running, so a stranded waiter fails the test by construction rather than by
// timing. Restore the pre-fix `WaitForCacheSync(stopCh, ...)` and this test
// hangs the bubble instead of passing.
func TestWaitForCacheSyncReleasesOnContextCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		neverSyncs := func() bool { return false }
		// Deliberately never closed: the cancellation must be what ends the
		// wait. Closing it at test end would let the pre-fix code unwind too
		// and hide the leak.
		stopCh := make(chan struct{})

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		synced, err := waitForCacheSync(ctx, stopCh, neverSyncs)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waitForCacheSync err = %v, want context.Canceled", err)
		}
		if synced {
			t.Fatal("waitForCacheSync reported synced against a cache that never settles")
		}
	})
}

// TestWaitForCacheSyncReleasesOnStopChannel is the other half: a cluster torn
// down mid-connect ends the wait too, reported as un-synced rather than as a
// ctx error, which is what turns into ErrCacheSyncFailed at the call site.
func TestWaitForCacheSyncReleasesOnStopChannel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		neverSyncs := func() bool { return false }
		stopCh := make(chan struct{})
		close(stopCh)

		synced, err := waitForCacheSync(t.Context(), stopCh, neverSyncs)
		if err != nil {
			t.Fatalf("waitForCacheSync err = %v, want nil", err)
		}
		if synced {
			t.Fatal("waitForCacheSync reported synced after the stop channel closed")
		}
	})
}

// TestStopClosesDoneOnce: Done is how a reconnect's event drain learns the
// cluster it was reading is gone, and Stop runs more than once (a rebuild
// stops the old cluster, quit stops whatever is live).
func TestStopClosesDoneOnce(t *testing.T) {
	c := &Cluster{done: make(chan struct{}), stopCh: make(chan struct{})}
	c.Stop()
	c.Stop()
	select {
	case <-c.Done():
	default:
		t.Fatal("Done still open after Stop")
	}
}
