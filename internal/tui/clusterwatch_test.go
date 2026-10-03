package tui_test

import (
	"testing"
	"time"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// A cluster never closes its event channels, so a drain left on one a
// reconnect replaced parked a goroutine forever, one per rebuild. Its Stop
// closing done has to end the drain.
func TestWatchClusterEndsWhenClusterStops(t *testing.T) {
	events := make(chan kube.ResourceChangedMsg)
	conn := make(chan kube.ConnStateMsg)
	done := make(chan struct{})
	cmd := tui.WatchCluster(events, conn, done)

	got := make(chan any, 1)
	go func() { got <- cmd() }()
	close(done)
	select {
	case msg := <-got:
		if msg != nil {
			t.Fatalf("WatchCluster after Stop = %#v, want nil (drain ended)", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WatchCluster still blocked on a stopped cluster's channels")
	}
}
