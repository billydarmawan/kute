package app

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

// quitOnHangup arranges for quit to be called when the process receives
// SIGHUP — closing the terminal window — until ctx is done. Bubble Tea only
// listens for SIGINT and SIGTERM, and SIGHUP's default action kills the
// process outright, so without this a closed window never reached run's
// state save. The handler is registered before quitOnHangup returns.
func quitOnHangup(ctx context.Context, quit func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP)
	go func() {
		defer utilruntime.HandleCrash()
		defer signal.Stop(sig)
		select {
		case <-ctx.Done():
		case <-sig:
			quit()
		}
	}()
}
