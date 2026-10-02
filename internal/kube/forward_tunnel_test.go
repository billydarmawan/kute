package kube

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	//nolint:staticcheck // the same deprecated package forward.go's spdyTunnel dials through.
	"k8s.io/apimachinery/pkg/util/httpstream"
)

// countingStreamDialer records every SPDY dial and fails it, so a Run that
// reaches the network is observable without one.
type countingStreamDialer struct{ dials atomic.Int32 }

func (d *countingStreamDialer) Dial(...string) (httpstream.Connection, string, error) {
	d.dials.Add(1)
	return nil, "", errors.New("dial refused by test")
}

// ForwardManager publishes a tunnel before calling Run, so a Stop/Restart in
// that gap Closes it first. The close must stick: Run afterwards may not dial
// and bind a listener nobody is left to close.
func TestSpdyTunnelCloseBeforeRunIsSticky(t *testing.T) {
	dialer := &countingStreamDialer{}
	tunnel := &spdyTunnel{dialer: dialer, localPort: 0, remotePort: 8080}
	tunnel.Close()

	done := make(chan error, 1)
	go func() { done <- tunnel.Run(nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after Close = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		tunnel.Close()
		t.Fatal("Run after Close kept running")
	}
	if n := dialer.dials.Load(); n != 0 {
		t.Fatalf("Run after Close dialed %d time(s), want 0", n)
	}
}
