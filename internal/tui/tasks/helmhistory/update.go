package helmhistory

import (
	"fmt"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Reload implements tui.Reloader — see its doc comment: this screen misses
// every kube.ResourceChangedMsg while parked in the stack, so BackMsg
// restoring it asks it to catch up immediately rather than showing stale
// data until an unrelated change happens to land while it's active again.
func (m *Model) Reload() tea.Cmd {
	m.reloadEpoch++
	return m.load()
}

// pasteTarget is the PROD type-the-name confirm buffer while one is up (the
// same gate updateConfirmKey routes typed keys through) — this screen has no
// other text buffer.
func (m *Model) pasteTarget() tui.PasteTarget {
	if m.actions.Active() && m.actions.Tier() == actions.TierModal {
		return m.actions.PasteTarget()
	}
	return nil
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cmd, ok := tui.RoutePaste(msg, m.pasteTarget()); ok {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ResourceChangedMsg:
		if msg.Kind == kube.KindSecret || msg.Kind == kube.KindHelmRelease {
			m.reloadEpoch++
			return m, m.load()
		}
	case tui.CacheSyncRetryMsg:
		if msg.Gen == m.reloadEpoch {
			return m, m.load()
		}
	case kube.ConnStateMsg:
		m.now = time.Now()
		m.conn = kube.ConnState(msg)
		m.actions.SetOffline(m.conn.Offline())
	case loadedMsg:
		return m.applyLoaded(msg)
	case spinner.TickMsg:
		if m.state != tui.TaskStateLoading {
			return m, nil
		}
		// The loading header's counting timer reads m.now, so it advances
		// off the same tick that animates the spinner — never off a clock
		// read in a render function.
		m.now = time.Now()
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case actions.ResultMsg:
		m.actions.HandleResult(msg)
		if msg.Err == nil {
			m.reloadEpoch++
			return m, m.load()
		}
		// A failed rollback (helm missing from PATH included) is the
		// controller's keybar result line (ActionResult).
	case tea.KeyPressMsg:
		// Any key acknowledges the last action's outcome line; the key
		// still does what it would have.
		m.actions.DismissResult()
		return m.updateKey(msg)
	}
	return m, nil
}

func (m *Model) applyLoaded(msg loadedMsg) (tea.Model, tea.Cmd) {
	if msg.epoch != m.reloadEpoch {
		return m, nil
	}
	wasLoading := m.state == tui.TaskStateLoading
	if msg.err != nil {
		m.state = tui.TaskStateError
		if kube.IsPermissionError(msg.err) {
			m.state = tui.TaskStatePermissionDenied
		}
		m.feedback = msg.err.Error()
		return m, nil
	}
	m.revisions = msg.revisions
	m.state = tui.TaskStateReady
	if len(m.revisions) == 0 {
		// The release Secrets live in a cache that starts on first read, so
		// an empty first answer means "not loaded yet" far more often than
		// "this release has no revisions" — and the latter is an alarming
		// thing to assert about a release the user just opened.
		if !tui.KindsSynced(m.lister, m.namespace, kube.KindHelmRelease) {
			m.state = tui.TaskStateLoading
			m.feedback = ""
			cmds := []tea.Cmd{tui.ScheduleCacheSyncRetry(m.reloadEpoch)}
			if !wasLoading {
				// Re-entering loading from ready (a reload after a rollback,
				// say). Update's spinner case bails out when the screen isn't
				// loading, so the tick chain has already stopped — restart it,
				// and the timer with it. Doing this unconditionally would run
				// two chains and double the spin rate.
				m.now, m.loadStartedAt = time.Now(), time.Now()
				cmds = append(cmds, m.spinner.Tick)
			}
			return m, tea.Batch(cmds...)
		}
		if err := tui.KindsError(m.lister, m.namespace, kube.KindHelmRelease); err != nil {
			// The release cache is settled only in the sense that it has
			// stopped making progress — see tui.KindsError. "No revisions"
			// about a release the user just opened would be a worse answer
			// than the read failure that actually happened.
			m.state = tui.TaskStateError
			m.feedback = fmt.Sprintf("couldn't load release history: %v — retrying", err)
			return m, nil
		}
		m.state = tui.TaskStateEmpty
	}
	m.feedback = ""
	if m.selected >= len(m.revisions) {
		m.selected = max(len(m.revisions)-1, 0)
	}
	m.clampOffset()
	return m, nil
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.actions.Active() {
		return m.updateConfirmKey(msg)
	}
	switch msg.String() {
	case "ctrl+q", "ctrl+c":
		return m, tea.Quit
	case "esc", "backspace":
		return m, func() tea.Msg { return tui.BackMsg{} }
	case "up", "k":
		m.moveSelection(-1)
	case "down", "j":
		m.moveSelection(1)
	case "ctrl+d":
		m.moveSelection(max(1, m.tableDataRows()/2))
	case "ctrl+u":
		m.moveSelection(-max(1, m.tableDataRows()/2))
	case verbs.Rollback.Key:
		if m.mutator != nil && m.state == tui.TaskStateReady {
			if rev, ok := m.selectedRevision(); ok {
				return m, m.beginRollback(rev)
			}
		}
	}
	return m, nil
}

// updateConfirmKey routes keys while 'R' rollback's confirmation is showing:
// TierModal (the PROD type-the-name modal — "Rollback inherits 8b friction")
// gets typing/backspace/enter-when-matched, TierInline stays the simple
// y/n/esc prompt — mirrors tasks/timeline's own updateConfirmKey.
func (m *Model) updateConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.actions.Tier() == actions.TierModal {
		switch msg.String() {
		case "esc":
			m.actions.Cancel()
		case "enter":
			return m, m.actions.Confirm()
		default:
			return m, m.actions.HandleTypeKey(msg)
		}
		return m, nil
	}
	switch msg.String() {
	case "y":
		return m, m.actions.Confirm()
	case "n", "esc":
		m.actions.Cancel()
	}
	return m, nil
}

func (m *Model) moveSelection(delta int) {
	if len(m.revisions) == 0 {
		m.selected, m.offset = 0, 0
		return
	}
	m.selected = clamp(m.selected+delta, 0, len(m.revisions)-1)
	m.clampOffset()
}

// clampOffset keeps the selected revision within the table's rendered
// viewport — mirrors nodedetail/routetable's own clampOffset/tableDataRows
// pattern.
func (m *Model) clampOffset() {
	rows := m.tableDataRows()
	if m.selected < m.offset {
		m.offset = m.selected
	}
	if m.selected >= m.offset+rows {
		m.offset = m.selected - rows + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// beginRollback confirms rolling back to rev — the same tier/friction
// tasks/browse's own beginRollback uses ("Rollback inherits 8b friction"),
// duplicated here per the repo's package-local-seam convention since this
// screen has its own actions.Controller.
func (m *Model) beginRollback(rev kube.HelmRelease) tea.Cmd {
	return m.actions.Begin(actions.Request{Verb: verbs.Rollback, Prod: m.isProd()}, tui.TaskAction{
		ID:    fmt.Sprintf("rollback-%s/%s-r%d", m.namespace, m.name, rev.Revision),
		Label: fmt.Sprintf("Rollback %s to revision %d?", m.name, rev.Revision),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindHelmRelease),
			ResourceName: m.name,
			Namespace:    m.namespace,
			Verb:         "rollback",
			IsMutating:   true,
			Revision:     rev.Revision,
		},
	})
}
