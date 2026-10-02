package app

import (
	"context"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
)

type bridgeEventSource struct {
	events chan kube.ResourceChangedMsg
	conn   chan kube.ConnStateMsg
}

func (s bridgeEventSource) Events() <-chan kube.ResourceChangedMsg { return s.events }
func (s bridgeEventSource) ConnEvents() <-chan kube.ConnStateMsg   { return s.conn }

type bridgeReadyMsg struct{}

type bridgeCaptureModel struct {
	ready   chan struct{}
	updates chan kube.ResourceKind
	once    sync.Once
}

func (m *bridgeCaptureModel) Init() tea.Cmd { return nil }
func (m *bridgeCaptureModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case bridgeReadyMsg:
		m.once.Do(func() { close(m.ready) })
	case kube.ResourceChangedMsg:
		m.updates <- msg.Kind
	}
	return m, nil
}
func (*bridgeCaptureModel) View() tea.View { return tea.NewView("") }

func TestForwardEventsCoalescesStormByKindAndDeliversTheFinalTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	model := &bridgeCaptureModel{ready: make(chan struct{}), updates: make(chan kube.ResourceKind, 8)}
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithoutRenderer(), tea.WithInput(nil), tea.WithoutSignalHandler())
	done := make(chan error, 1)
	go func() {
		_, err := program.Run()
		done <- err
	}()
	program.Send(bridgeReadyMsg{})
	select {
	case <-model.ready:
	case <-time.After(time.Second):
		t.Fatal("Bubble Tea program did not start")
	}

	source := bridgeEventSource{events: make(chan kube.ResourceChangedMsg, 512), conn: make(chan kube.ConnStateMsg, 1)}
	go forwardEvents(ctx, source, program, nil)
	for range 200 {
		source.events <- kube.ResourceChangedMsg{Kind: kube.KindPod}
		source.events <- kube.ResourceChangedMsg{Kind: kube.KindEvent}
	}

	seen := map[kube.ResourceKind]int{}
	deadline := time.NewTimer(5 * resourceEventDebounce)
	defer deadline.Stop()
	for len(seen) < 2 {
		select {
		case kind := <-model.updates:
			seen[kind]++
		case <-deadline.C:
			t.Fatalf("coalesced kinds never arrived: %v", seen)
		}
	}
	if seen[kube.KindPod] != 1 || seen[kube.KindEvent] != 1 {
		t.Fatalf("storm delivered %+v, want one turn per kind", seen)
	}
	select {
	case kind := <-model.updates:
		t.Fatalf("storm accumulated another %s turn after settling", kind)
	case <-time.After(2 * resourceEventDebounce):
	}

	// A later change still gets its own turn; coalescing is not a latch.
	source.events <- kube.ResourceChangedMsg{Kind: kube.KindPod}
	select {
	case kind := <-model.updates:
		if kind != kube.KindPod {
			t.Fatalf("later turn kind = %s", kind)
		}
	case <-time.After(5 * resourceEventDebounce):
		t.Fatal("later Pod change was lost")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Bubble Tea program did not stop")
	}
}

// TestEventCoalescerFlushesAContinuouslyChangingKindWithinMaxWait drives the
// bridge's schedule with synthetic times: one Pod event every 200 ms — faster
// than the 250 ms debounce — must still flush within resourceEventMaxWait,
// and keep flushing for as long as the stream lasts. A pure trailing-edge
// debounce pushes the deadline forward on every event and never delivers.
func TestEventCoalescerFlushesAContinuouslyChangingKindWithinMaxWait(t *testing.T) {
	c := newEventCoalescer()
	start := time.Unix(0, 0)
	step := 200 * time.Millisecond
	var flushes []time.Time
	for i := range 30 { // 6 s of steady churn
		now := start.Add(time.Duration(i) * step)
		// The bridge's timer fires at next() if that comes before this event.
		if next := c.next(); !next.IsZero() && !next.After(now) {
			if kinds := c.flush(next); len(kinds) == 1 && kinds[0] == kube.KindPod {
				flushes = append(flushes, next)
			}
		}
		c.observe(kube.KindPod, now)
	}
	if len(flushes) == 0 {
		t.Fatal("a kind changing every 200ms was never flushed")
	}
	if first := flushes[0].Sub(start); first > resourceEventMaxWait {
		t.Fatalf("first flush after %v, want within %v", first, resourceEventMaxWait)
	}
	for i := 1; i < len(flushes); i++ {
		if gap := flushes[i].Sub(flushes[i-1]); gap > resourceEventMaxWait+step {
			t.Fatalf("flush gap %v exceeds max-wait", gap)
		}
		if gap := flushes[i].Sub(flushes[i-1]); gap < resourceEventDebounce {
			t.Fatalf("flush gap %v: a steady stream must still coalesce", gap)
		}
	}
}

// TestEventCoalescerKeepsTrailingEdgeForAShortBurst pins that the max-wait
// only bounds a long stream: a burst that goes quiet still flushes once, on
// its trailing edge, and kinds are scheduled independently.
func TestEventCoalescerKeepsTrailingEdgeForAShortBurst(t *testing.T) {
	c := newEventCoalescer()
	start := time.Unix(0, 0)
	for i := range 5 {
		c.observe(kube.KindPod, start.Add(time.Duration(i)*10*time.Millisecond))
	}
	c.observe(kube.KindEvent, start.Add(100*time.Millisecond))

	podDue := start.Add(40*time.Millisecond + resourceEventDebounce)
	if got := c.next(); !got.Equal(podDue) {
		t.Fatalf("next = %v, want the Pod burst's trailing edge %v", got.Sub(start), podDue.Sub(start))
	}
	if kinds := c.flush(podDue.Add(-time.Millisecond)); len(kinds) != 0 {
		t.Fatalf("flushed %v before the trailing edge", kinds)
	}
	if kinds := c.flush(podDue); len(kinds) != 1 || kinds[0] != kube.KindPod {
		t.Fatalf("flush at Pod due = %v, want [Pod] only", kinds)
	}
	eventDue := start.Add(100*time.Millisecond + resourceEventDebounce)
	if got := c.next(); !got.Equal(eventDue) {
		t.Fatalf("next = %v, want Event's own trailing edge %v", got.Sub(start), eventDue.Sub(start))
	}
	if kinds := c.flush(eventDue); len(kinds) != 1 || kinds[0] != kube.KindEvent {
		t.Fatalf("flush at Event due = %v, want [Event]", kinds)
	}
	if got := c.next(); !got.IsZero() {
		t.Fatalf("next = %v after draining, want zero", got)
	}
}
