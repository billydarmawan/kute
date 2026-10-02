package tui

import (
	"reflect"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// tickerTask is a minimal task with one self-rescheduling chain, the shape
// of browse's metrics poll: Init arms a tick, and every tick it receives
// re-arms the next one. pushTickerMsg makes it push a child of the same
// type, so a misrouted tick is visible as the wrong instance counting it.
type tickerTask struct {
	name  string
	ticks int
	child *tickerTask
}

type tickerTickMsg struct{ from string }

type pushTickerMsg struct{}

func (t *tickerTask) tick() tea.Cmd {
	from := t.name
	return func() tea.Msg { return tickerTickMsg{from: from} }
}

func (t *tickerTask) Init() tea.Cmd { return t.tick() }

func (t *tickerTask) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case tickerTickMsg:
		t.ticks++
		return t, t.tick()
	case pushTickerMsg:
		return t.child, t.child.Init()
	}
	return t, nil
}

func (t *tickerTask) View() tea.View   { return tea.NewView(t.name) }
func (t *tickerTask) SetSize(_, _ int) {}

// runCmd executes cmd the way the runtime does, flattening batches and
// sequences, and returns every message it produced.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return nil
	}
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeFor[tea.Cmd]() {
		var out []tea.Msg
		for i := range v.Len() {
			inner, _ := v.Index(i).Interface().(tea.Cmd)
			out = append(out, runCmd(inner)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func onlyMsg(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	msgs := runCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("want exactly one message, got %d: %#v", len(msgs), msgs)
	}
	return msgs[0]
}

// TestParkedTaskTickResumesOnPop pins 13 H2: a parked task's timer message
// must not land on the task covering it (where the chain used to die, or be
// double-counted by a same-typed instance), and the chain must be running
// again once esc brings the parked task back.
func TestParkedTaskTickResumesOnPop(t *testing.T) {
	child := &tickerTask{name: "child"}
	parent := &tickerTask{name: "parent", child: child}
	var model tea.Model = New(parent)

	parentTick := onlyMsg(t, model.(Model).Init())

	// Push the child; its own Init tick is in flight too.
	model, cmd := model.Update(pushTickerMsg{})
	childTick := onlyMsg(t, cmd)

	// The parent's tick fires while the child is on top.
	model, cmd = model.Update(parentTick)
	if child.ticks != 0 || parent.ticks != 0 {
		t.Fatalf("parked parent's tick reached a task while covered: parent=%d child=%d", parent.ticks, child.ticks)
	}
	if msgs := runCmd(cmd); len(msgs) != 0 {
		t.Fatalf("a held tick must not re-arm anything while parked, got %#v", msgs)
	}

	// The child's own tick still reaches the child and re-arms it.
	_, cmd = model.Update(childTick)
	if child.ticks != 1 || len(runCmd(cmd)) != 1 {
		t.Fatalf("active child's own tick: ticks=%d, want 1 and a re-armed chain", child.ticks)
	}

	model, cmd = model.Update(BackMsg{})
	if parent.ticks != 1 {
		t.Fatalf("parent ticks after pop = %d, want 1 (held tick delivered on resume)", parent.ticks)
	}
	resumed := runCmd(cmd)
	if len(resumed) != 1 {
		t.Fatalf("pop should re-arm the parent's chain, got %#v", resumed)
	}

	// The re-armed chain keeps running on the parent.
	_, _ = model.Update(resumed[0])
	if parent.ticks != 2 || child.ticks != 1 {
		t.Fatalf("after resume: parent=%d child=%d, want 2 and 1", parent.ticks, child.ticks)
	}
}

// TestClosedTaskTickIsDropped: a popped task's late tick has no reader; it
// must not be handed to whatever is now on top.
func TestClosedTaskTickIsDropped(t *testing.T) {
	child := &tickerTask{name: "child"}
	parent := &tickerTask{name: "parent", child: child}
	var model tea.Model = New(parent)
	_ = model.(Model).Init() // the parent's own chain isn't under test

	model, cmd := model.Update(pushTickerMsg{})
	childTick := onlyMsg(t, cmd)
	model, _ = model.Update(BackMsg{})

	_, cmd = model.Update(childTick)
	if parent.ticks != 0 || child.ticks != 0 {
		t.Fatalf("closed child's tick was delivered: parent=%d child=%d", parent.ticks, child.ticks)
	}
	if cmd != nil && len(runCmd(cmd)) != 0 {
		t.Fatal("a dropped tick must not produce follow-up commands")
	}
}

// TestRootMessagesStayUntagged: navigation and runtime messages must reach
// the root (and Bubble Tea) as-is, even inside a task's batch or sequence.
func TestRootMessagesStayUntagged(t *testing.T) {
	back := func() tea.Msg { return BackMsg{} }
	tick := func() tea.Msg { return tickerTickMsg{} }
	msgs := runCmd(ownCmd(7, tea.Sequence(back, tea.Batch(tick, tea.Quit))))
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3: %#v", len(msgs), msgs)
	}
	if _, ok := msgs[0].(BackMsg); !ok {
		t.Errorf("BackMsg was tagged: %#v", msgs[0])
	}
	var sawOwned, sawQuit bool
	for _, msg := range msgs[1:] {
		switch msg := msg.(type) {
		case ownedMsg:
			sawOwned = msg.owner == 7
		case tea.QuitMsg:
			sawQuit = true
		}
	}
	if !sawOwned || !sawQuit {
		t.Errorf("want the tick owned by 7 and QuitMsg untouched, got %#v", msgs[1:])
	}
}
