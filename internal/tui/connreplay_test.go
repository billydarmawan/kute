package tui_test

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// connTask records the last kube.ConnStateMsg it received, the way every
// task package keeps its own conn field. Its zero value reads as online,
// which is exactly the false claim the root's replay exists to prevent.
type connTask struct {
	name string
	next tui.Task
	conn kube.ConnState
	seen int
}

func (t *connTask) Init() tea.Cmd    { return nil }
func (t *connTask) SetSize(int, int) {}
func (t *connTask) View() tea.View   { return tea.NewView(t.name) }

func (t *connTask) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case kube.ConnStateMsg:
		t.conn = kube.ConnState(msg)
		t.seen++
	case tea.KeyPressMsg:
		if msg.String() == "open" && t.next != nil {
			return t.next, nil
		}
	}
	return t, nil
}

func step(t *testing.T, m tui.Model, msg tea.Msg) tui.Model {
	t.Helper()
	updated, _ := m.Update(msg)
	return updated.(tui.Model)
}

// TestPushedTaskReceivesCurrentConnState: a screen opened during an outage
// must learn about it before its first frame — the health loop never
// re-emits during ConnUnauthenticated, so nothing else would tell it.
func TestPushedTaskReceivesCurrentConnState(t *testing.T) {
	t.Parallel()

	child := &connTask{name: "child"}
	parent := &connTask{name: "parent", next: child}
	m := tui.New(parent)
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnUnauthenticated, Err: "exec plugin failed"})
	_ = step(t, m, tea.KeyPressMsg{Text: "open"})

	if !child.conn.Offline() || child.conn.Phase != kube.ConnUnauthenticated {
		t.Fatalf("pushed task conn = %+v, want the root's Unauthenticated state", child.conn)
	}
}

// TestPoppedTaskReceivesTransitionMissedWhileParked: the parent was parked
// while connected, the outage happened under the child, and esc pops back —
// the parent must not keep claiming "connected".
func TestPoppedTaskReceivesTransitionMissedWhileParked(t *testing.T) {
	t.Parallel()

	child := &connTask{name: "child"}
	parent := &connTask{name: "parent", next: child}
	m := tui.New(parent)
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnConnected})
	m = step(t, m, tea.KeyPressMsg{Text: "open"})
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnUnauthenticated, Err: "exec plugin failed"})
	if parent.conn.Phase != kube.ConnConnected {
		t.Fatalf("precondition: parked parent should not have seen the outage yet, got %+v", parent.conn)
	}
	m = step(t, m, tui.BackMsg{})

	if m.Conn().Phase != kube.ConnUnauthenticated {
		t.Fatalf("root conn = %+v, want Unauthenticated", m.Conn())
	}
	if parent.conn.Phase != kube.ConnUnauthenticated {
		t.Fatalf("popped parent conn = %+v, want the root's Unauthenticated state", parent.conn)
	}
}

// TestRoutedGotoTaskReceivesCurrentConnState covers a root-built swap: a
// goto from a pushed screen builds a fresh browse via the factory.
func TestRoutedGotoTaskReceivesCurrentConnState(t *testing.T) {
	t.Parallel()

	fresh := &connTask{name: "fresh-browse"}
	child := &connTask{name: "child"}
	parent := &connTask{name: "parent", next: child}
	m := tui.NewWithSession(parent, testSession()).
		WithRootFactories(nil, func() tui.Task { return fresh })
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "dial timeout"})
	m = step(t, m, tea.KeyPressMsg{Text: "open"})
	_ = step(t, m, tui.GotoKindMsg{Kind: kube.KindPod})

	if fresh.conn.Phase != kube.ConnReconnecting {
		t.Fatalf("goto-built task conn = %+v, want Reconnecting", fresh.conn)
	}
}

// TestReplaceRootDoesNotReplayOldClusterState: ReplaceRootMsg brings in a
// different cluster, so the replaced cluster's outage must not be replayed
// into the new browse (it reports its own state over msg.Conn).
func TestReplaceRootDoesNotReplayOldClusterState(t *testing.T) {
	t.Parallel()

	root := &connTask{name: "setup"}
	replacement := &connTask{name: "browse"}
	m := tui.New(root)
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "unreachable"})
	m = step(t, m, tui.ReplaceRootMsg{Task: replacement})

	if replacement.seen != 0 {
		t.Fatalf("replacement got %d ConnStateMsg replays (%+v), want none", replacement.seen, replacement.conn)
	}
	if m.Conn().Phase != "" {
		t.Fatalf("root conn after ReplaceRootMsg = %+v, want reset", m.Conn())
	}
}
