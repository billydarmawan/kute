package browse

import (
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/resources"
)

// pushWhoCan pushes tasks/whocan (22a) pre-filled with verb/resource/
// namespace — shared by the goto KindWhoCan carve-out (update.go's
// GotoKindMsg case) and the 4b 403 card's 'w' recovery key.
func (m Model) pushWhoCan(verb, resource, namespace string) (tea.Model, tea.Cmd, bool) {
	if m.openWhoCan == nil {
		return nil, nil, false
	}
	task, cmd := m.openWhoCan(verb, resource, namespace, m.width, m.height)
	return task, cmd, task != nil
}

// openWhoCanFromCurrentKind is 22a's default entry from browse: "list"
// (the only verb browse's own load ever issues) against whatever kind/
// namespace is currently showing — used both by a bare `g "who"` jump
// (update.go's GotoKindMsg carve-out, no denial in play) and the 4b 403
// card's 'w' (docs/design README.md §22a: "arriving with the failed
// verb+resource pre-filled" — a 403 card only ever exists because this
// same kind/namespace's list load was just denied).
func (m Model) openWhoCanFromCurrentKind() (tea.Model, tea.Cmd, bool) {
	return m.pushWhoCan("list", resources.RBACResource(m.desc), m.namespace)
}
