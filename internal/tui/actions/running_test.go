package actions

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// slowMutator blocks Cordon until release is closed — a hung API call.
type slowMutator struct {
	*fakeMutator
	release chan struct{}
}

func (s slowMutator) Cordon(context.Context, string, bool) error {
	<-s.release
	return nil
}

func newSlow(t *testing.T) (Controller, chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	c := New(slowMutator{fakeMutator: &fakeMutator{}, release: release})
	c.slowAfter, c.slowEvery = 5*time.Millisecond, 5*time.Millisecond
	return c, release
}

// TestFastWriteHasNoRunningLine: a write that lands inside the threshold
// returns its ResultMsg directly — no tick, nothing to flash.
func TestFastWriteHasNoRunningLine(t *testing.T) {
	c := New(&fakeMutator{})
	msg := beginAt(&c, TierNone, cordonAction())()
	if _, ok := msg.(ResultMsg); !ok {
		t.Fatalf("fast write produced %T, want ResultMsg", msg)
	}
	if !c.Result().Empty() {
		t.Fatalf("result = %+v before the result landed, want nothing", c.Result())
	}
}

// TestSlowWriteShowsStillRunningUntilResult pins review 10 L6: a write with
// no deadline that runs past the threshold says so on the keybar, with an
// elapsed time from the tick, and its own result replaces the line.
func TestSlowWriteShowsStillRunningUntilResult(t *testing.T) {
	c, release := newSlow(t)
	msg := beginAt(&c, TierNone, cordonAction())()
	running, ok := msg.(RunningMsg)
	if !ok {
		t.Fatalf("slow write produced %T, want RunningMsg", msg)
	}
	next := c.HandleRunning(running)
	if next == nil {
		t.Fatal("HandleRunning must hand back the continuation")
	}
	r := c.Result()
	if !r.Running || r.Failed || !strings.Contains(r.Text, "cordon node/node-a · still running · ") {
		t.Fatalf("result = %+v, want a still-running line naming the verb and target", r)
	}

	close(release)
	var result ResultMsg
	for {
		m := next()
		if rm, ok := m.(RunningMsg); ok {
			next = c.HandleRunning(rm)
			continue
		}
		result = m.(ResultMsg)
		break
	}
	c.HandleResult(result)
	if r := c.Result(); r.Running || r.Failed || r.Empty() {
		t.Fatalf("result = %+v, want the success line in place of the running one", r)
	}
	c.DismissResult()
	if r := c.Result(); !r.Empty() {
		t.Fatalf("result = %+v after dismiss, want nothing — the write has landed", r)
	}
}

// TestRunningLineYieldsToConfirmAndOtherResults: an older slow write's
// line hides under a newer confirm, isn't cleared by a newer action's
// result, and comes back once that result is dismissed.
func TestRunningLineYieldsToConfirmAndOtherResults(t *testing.T) {
	c, release := newSlow(t)
	defer close(release)
	running := beginAt(&c, TierNone, cordonAction())().(RunningMsg)
	c.HandleRunning(running)

	beginAt(&c, TierInline, deleteAction())
	if !c.Result().Empty() {
		t.Fatalf("result = %+v under an open confirm, want nothing", c.Result())
	}
	c.HandleResult(ResultMsg{ActionID: "delete-pod", Label: "delete pod api", Seq: c.seq})
	if r := c.Result(); r.Running || r.Empty() {
		t.Fatalf("result = %+v, want the delete's outcome", r)
	}
	c.DismissResult()
	if r := c.Result(); !r.Running {
		t.Fatalf("result = %+v, want the cordon still running", r)
	}
	// The stale cordon's own result clears its line.
	c.HandleResult(ResultMsg{ActionID: "cordon-node-a", Label: "Cordon node-a", Seq: running.Seq})
	c.DismissResult()
	if r := c.Result(); !r.Empty() {
		t.Fatalf("result = %+v, want nothing once every write has landed", r)
	}
}

// TestForeignRunningMsgOnlyContinues: another controller's tick changes
// nothing here but still hands back the continuation.
func TestForeignRunningMsgOnlyContinues(t *testing.T) {
	c := New(&fakeMutator{})
	called := false
	next := c.HandleRunning(RunningMsg{controller: c.id + 1, Seq: 1, next: func() tea.Msg { called = true; return nil }})
	if !c.Result().Empty() {
		t.Fatalf("result = %+v, want a foreign tick ignored", c.Result())
	}
	if next == nil {
		t.Fatal("a foreign RunningMsg must still hand back its continuation")
	}
	next()
	if !called {
		t.Fatal("wrong continuation returned")
	}
}

func TestFormatElapsed(t *testing.T) {
	for d, want := range map[time.Duration]string{
		1200 * time.Millisecond: "1s",
		4 * time.Second:         "4s",
		65 * time.Second:        "1m05s",
	} {
		if got := formatElapsed(d); got != want {
			t.Errorf("formatElapsed(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestWritePanicReachesTheCommand: a panic in the write goroutine is
// re-raised on the command's goroutine, where Bubble Tea recovers it.
func TestWritePanicReachesTheCommand(t *testing.T) {
	cmd := watchSlow(func() tea.Msg { panic("boom") }, 1, 1, "x", time.Hour, time.Hour)
	defer func() {
		if r := recover(); r != "boom" {
			t.Fatalf("recovered %v, want the write's panic", r)
		}
	}()
	cmd()
}
