package app

import (
	"context"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestQuitOnHangupQuitsTheProgram: closing the terminal window (SIGHUP)
// quits through Bubble Tea, so run still saves state on the way out
// (14 L3), instead of the signal's default action killing the process.
func TestQuitOnHangupQuitsTheProgram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no SIGHUP on Windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	quit := make(chan struct{})
	quitOnHangup(ctx, func() { close(quit) })

	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := self.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-quit:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not quit the program")
	}
}
