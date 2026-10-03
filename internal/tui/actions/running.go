package actions

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Writes run with no deadline (cancelling a mutation mid-flight doesn't
// un-apply it — review 10 L6), so a hung API call or a long drain must at
// least not be silent: once a write has been in flight for defaultSlowAfter, the
// keybar says so, and the elapsed time advances every defaultSlowEvery until the
// result lands and replaces it.
const (
	defaultSlowAfter = time.Second
	defaultSlowEvery = time.Second
)

// controllerIDs numbers Controllers, so a RunningMsg finds the controller
// that dispatched it even if a screen ever holds two (Seq alone is only
// unique per controller).
var controllerIDs atomic.Uint64

// RunningMsg reports that a dispatched write is still in flight. It is
// produced only for a slow write — a write that lands within the threshold
// returns its ResultMsg directly, with no tick at all — and it carries the
// continuation that waits for the write: every screen must hand it to
// Controller.HandleRunning and return the command it gives back, or the
// write's ResultMsg is never delivered (actions/guard_test.go enforces the
// routing). The chain is strictly serial — one RunningMsg, then either the
// next one or the ResultMsg — so it ends the moment the write lands and a
// RunningMsg can never arrive after its own result.
type RunningMsg struct {
	controller uint64
	Seq        uint64
	// Elapsed is how long the write has been running, measured by the
	// command goroutine when this message was produced — the clock value
	// the keybar line renders, so render never reads a clock.
	Elapsed time.Duration
	text    string
	next    tea.Cmd
}

// running is the in-flight line's state: the write's Seq (zero: nothing
// slow in flight), what it is, and how long it had been running at the
// last RunningMsg.
type running struct {
	seq     uint64
	text    string
	elapsed time.Duration
}

// HandleRunning records a slow write's elapsed time for the keybar's
// still-running line and returns the command that keeps waiting for it.
// The returned command must always be returned to the runtime: it is the
// only path the write's ResultMsg arrives by.
func (c *Controller) HandleRunning(msg RunningMsg) tea.Cmd {
	if msg.controller != c.id {
		return msg.next
	}
	// A newer write's line wins over an older one still running alongside
	// it (a slow cordon under a slow drain), rather than alternating.
	if msg.Seq >= c.running.seq {
		c.running = running{seq: msg.Seq, text: msg.text, elapsed: msg.Elapsed}
	}
	return msg.next
}

// settle clears the still-running line when the write it describes has
// landed. Another write's line stays: its own result is still to come.
func (c *Controller) settle(seq uint64) {
	if seq == 0 {
		// A hand-built message: it belongs to the latest action.
		seq = c.seq
	}
	if c.running.seq == seq {
		c.running = running{}
	}
}

// runningLine is the keybar's in-flight line, or the zero ResultLine.
func (c Controller) runningLine() tui.ResultLine {
	if c.running.seq == 0 {
		return tui.ResultLine{}
	}
	return tui.ResultLine{
		Text:    c.running.text + " · still running · " + formatElapsed(c.running.elapsed),
		Running: true,
	}
}

// formatElapsed renders "4s", "1m05s".
func formatElapsed(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}

// runningText names an in-flight action the registry's way: the verb's
// label and the kubectl-style target, "drain node/node-a".
func runningText(scope tui.TaskScope) string {
	label := strings.ReplaceAll(scope.Verb, "-", " ")
	if v, ok := verbs.ByID(scope.Verb); ok && v.Label != "" {
		label = v.Label
	}
	if n := len(scope.BulkTargets); n > 0 {
		return fmt.Sprintf("%s %d targets", label, n)
	}
	target := strings.ToLower(kube.ResourceKind(scope.ResourceKind).APIKind()) + "/" + scope.ResourceName
	return label + " " + target
}

// writePanic carries a panic out of the write goroutine so it is re-raised
// on the command's own goroutine, where Bubble Tea's recover sees it.
type writePanic struct{ value any }

// watchSlow wraps a write command: it runs write on its own goroutine and
// returns write's message directly when it lands within after; otherwise it
// returns a RunningMsg whose continuation waits another every, and so on.
func watchSlow(write tea.Cmd, controller, seq uint64, text string, after, every time.Duration) tea.Cmd {
	return func() tea.Msg {
		start := time.Now()
		done := make(chan tea.Msg, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					done <- writePanic{r}
				}
			}()
			done <- write()
		}()
		var await func(wait time.Duration) tea.Msg
		await = func(wait time.Duration) tea.Msg {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case msg := <-done:
				if p, ok := msg.(writePanic); ok {
					panic(p.value)
				}
				return msg
			case now := <-timer.C:
				return RunningMsg{
					controller: controller,
					Seq:        seq,
					Elapsed:    now.Sub(start),
					text:       text,
					next:       func() tea.Msg { return await(every) },
				}
			}
		}
		return await(after)
	}
}
