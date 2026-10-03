package browse

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

// followObject lands on msg's kind in msg's namespace and arms
// resolveFollow for msg.Name — a namespace switch fired from an object
// screen (tui.ObjectScreen), routed here by the root onto a fresh browse.
func (m *Model) followObject(msg tui.FollowObjectMsg) tea.Cmd {
	cmd := m.goToResource(tui.GotoResourceMsg(msg))
	// After goToResource: its resetAndLoad clears any follow state.
	m.pendingFollow = msg.Name
	return cmd
}

// resolveFollow runs once the followed namespace's rows have landed (Ready —
// the Empty state already says there are none). A same-named object reopens
// through ↵'s own routing, so it gets the same screen the user left; this
// instance then reports Transient so esc from it walks straight back to the
// object in the old namespace. A missing one leaves the list up with a note.
func (m *Model) resolveFollow() (tea.Model, tea.Cmd, bool) {
	name := m.pendingFollow
	if name == "" {
		return nil, nil, false
	}
	m.pendingFollow = ""
	if m.selectedName() == name {
		task, cmd, ok := m.openSelectedEnter()
		if next, self := task.(*Model); ok && (!self || next != m) {
			m.followedAway = true
		}
		return task, cmd, ok
	}
	for _, row := range m.rows {
		if row.Name == name {
			// There, just not selectable (folded or grouped away) — the
			// list is still the honest place to land, without a note.
			return nil, nil, false
		}
	}
	m.followNote = fmt.Sprintf("%s %s isn't in %s", resources.SingularDisplay(m.desc), name, m.namespace)
	return nil, nil, false
}

// Transient implements tui.Transient: true once resolveFollow has swapped
// this instance for the followed object's screen.
func (m *Model) Transient() bool { return m.followedAway }
