package tui

import (
	"reflect"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
)

// Owned-message routing: every message a task's own command produces goes
// back to that task, whether or not it is still the active one.
//
// Bubble Tea has one Update entry point, and the root forwards whatever
// reaches it to m.task. Without routing, a parked task's self-rescheduling
// chain — browse's 2s metrics poll, a spinner, a 1s clock, a cache-sync
// retry, a log stream's next-line read — fires while something else is on
// top, lands on that task, which ignores it (or worse, misreads a same-typed
// message meant for another instance), and the chain is gone for good:
// nothing re-arms it when esc brings the parked screen back, so its numbers
// freeze while the header still says it's syncing.
//
// So the root tags each task-produced command with the producing slot's
// ID (ownCmd), and routeOwned sends the result:
//
//   - to the active task when it owns it — exactly the old path;
//   - into the owner's stack entry when it is parked, where it waits. popTask
//     delivers the held messages, in arrival order, the moment the task is
//     active again, so a chain resumes where it stopped instead of either
//     dying or polling the cluster for a screen nobody can see ("the app
//     pays for the screen you're on"). A chain only re-arms when its message
//     is delivered, so each parked chain holds at most one message;
//   - nowhere when the owner has been popped or discarded: a closed screen's
//     late result has no reader, and handing it to whatever is on top is the
//     misdelivery this exists to stop.
//
// Messages the root itself acts on (rootMsg) and Bubble Tea's own are never
// tagged — navigation (BackMsg, GotoKindMsg, …) must reach the root even
// from a task that just closed itself, and the runtime has to see its
// batch/sequence/exec/quit messages untouched. Batches and sequences are
// unpacked and their inner commands tagged instead, so a tea.Batch of a load
// and a tick still routes each result.
type ownedMsg struct {
	owner uint64
	msg   tea.Msg
}

// stackEntry is one parked task: its routing ID, and the owned messages
// that arrived for it while it was covered.
type stackEntry struct {
	task Task
	id   uint64
	held []tea.Msg
}

// allocTaskID hands out a routing ID for a task slot the root has just
// built (the root task keeps the zero value).
func (m *Model) allocTaskID() uint64 {
	m.nextTaskID++
	return m.nextTaskID
}

// push parks the active task, with its routing ID, on the stack.
func (m *Model) push() {
	m.stack = append(m.stack, stackEntry{task: m.task, id: m.taskID})
}

// taskInit runs the active task's Init with its commands routed back to it.
func (m Model) taskInit() tea.Cmd {
	return ownCmd(m.taskID, m.task.Init())
}

// forward delivers msg to the active task and applies what it returns: the
// same task updated in place, or a different one — a push (or a transient
// task replacing itself), whose commands belong to the child: its parent
// built it and batched the child's Init into cmd.
func (m *Model) forward(msg tea.Msg) tea.Cmd {
	updated, cmd := m.task.Update(msg)
	task, ok := updated.(Task)
	if !ok || sameTask(task, m.task) {
		if ok {
			m.task = task
		}
		return ownCmd(m.taskID, cmd)
	}
	if !transient(m.task) {
		m.push()
	}
	m.task = task
	m.taskID = m.allocTaskID()
	return tea.Batch(ownCmd(m.taskID, cmd), m.replayConn())
}

// routeOwned sends an owned message to its task (see the file comment).
func (m Model) routeOwned(om ownedMsg) (tea.Model, tea.Cmd) {
	if om.owner == m.taskID {
		return m.Update(om.msg)
	}
	for i := range m.stack {
		if m.stack[i].id == om.owner {
			m.stack[i].held = append(m.stack[i].held, om.msg)
			return m, nil
		}
	}
	return m, nil
}

// deliverHeld forwards the messages entry collected while parked to the
// now-active task (entry's own). If one of them makes it push or replace
// itself, the rest stay held for it — on its new stack entry when it was
// pushed under the child, dropped when it replaced itself and is gone.
func (m *Model) deliverHeld(entry stackEntry) tea.Cmd {
	var cmds []tea.Cmd
	for i, msg := range entry.held {
		if m.taskID != entry.id {
			for j := range m.stack {
				if m.stack[j].id == entry.id {
					m.stack[j].held = append(m.stack[j].held, entry.held[i:]...)
				}
			}
			break
		}
		cmds = append(cmds, m.forward(msg))
	}
	return tea.Batch(cmds...)
}

// ownCmd tags cmd's message (every message, for a batch or sequence) as
// owned by the task slot id.
func ownCmd(id uint64, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg { return ownMsg(id, cmd()) }
}

var (
	cmdType    = reflect.TypeFor[tea.Cmd]()
	teaPkgPath = reflect.TypeFor[tea.BatchMsg]().PkgPath()
)

func ownMsg(id uint64, msg tea.Msg) tea.Msg {
	if msg == nil {
		return nil
	}
	// tea.BatchMsg and tea.Sequence's unexported sequenceMsg are both
	// []tea.Cmd; rebuild the same type so the runtime still runs it as a
	// batch/sequence, with every inner command tagged.
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == cmdType {
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			inner, _ := v.Index(i).Interface().(tea.Cmd)
			out.Index(i).Set(reflect.ValueOf(ownCmd(id, inner)))
		}
		return out.Interface()
	}
	if rootMsg(msg) {
		return msg
	}
	return ownedMsg{owner: id, msg: msg}
}

// rootMsg reports whether msg is one the root shell acts on (or Bubble
// Tea's runtime does), and so must never be held for a parked task.
func rootMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case BackMsg, GotoKindMsg, GotoResourceMsg, SwitchNamespaceMsg, SwitchContextMsg,
		OpenUpdatePanelMsg, ReplaceRootMsg, UpdateCheckedMsg, WindowSizeMsg, KeycastTickMsg,
		kube.ConnStateMsg, kube.ResourceChangedMsg, kube.CRDsDiscoveredMsg,
		clusterEventMsg, clusterConnMsg:
		return true
	}
	t := reflect.TypeOf(msg)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.PkgPath() == teaPkgPath
}
