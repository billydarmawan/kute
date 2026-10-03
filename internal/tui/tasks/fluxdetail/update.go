package fluxdetail

import (
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Reload implements tui.Reloader — see its doc comment: this screen misses
// every kube.ResourceChangedMsg while parked in the stack, so BackMsg
// restoring it asks it to catch up immediately rather than showing stale
// data until an unrelated change happens to land while it's active again.
//
// The tick and spinner chains died while another task was on top (their
// messages went to it), so both are restarted here.
func (m *Model) Reload() tea.Cmd {
	m.now = time.Now()
	m.tickActive = false
	cmds := []tea.Cmd{m.load(), m.ensureTick()}
	if m.state == tui.TaskStateLoading {
		cmds = append(cmds, m.spinner.Tick)
	}
	return tea.Batch(cmds...)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ResourceChangedMsg:
		// Reload on this object's own kind, and on the kinds its inventory
		// pulls in — otherwise the READY column goes stale while the screen
		// keeps claiming it is live.
		if m.reloadsOn(msg.Kind) {
			return m, m.load()
		}
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
		m.actions.SetOffline(m.conn.Offline())
	case tickMsg:
		// The retry countdown's clock lives here, not in View — render
		// functions stay pure. A stale chain, or one outliving the failure
		// card, stops.
		if msg.gen != m.tickGen || !m.tickActive {
			return m, nil
		}
		if m.fail == nil {
			m.tickActive = false
			return m, nil
		}
		m.now = msg.at
		return m, tickCmd(m.tickGen)
	case spinner.TickMsg:
		// The spinner only renders while loading; let its chain end after.
		if m.state != tui.TaskStateLoading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case loadedMsg:
		return m, m.applyLoaded(msg)
	case tui.CacheSyncRetryMsg:
		if msg.Gen == m.loadEpoch {
			return m, m.load()
		}
	case actions.ResultMsg:
		// The outcome is the controller's keybar result line
		// (ActionResult); the will-run command it replaces is cleared.
		m.actions.HandleResult(msg)
		if m.state == tui.TaskStateReady {
			m.feedback = ""
		}
		if msg.Err == nil {
			return m, m.load()
		}
	case tea.KeyPressMsg:
		// Any key acknowledges the last action's outcome line; the key
		// still does what it would have.
		m.actions.DismissResult()
		return m.updateKey(msg)
	}
	return m, nil
}

// reloadsOn reports whether a change to kind could change this screen.
func (m Model) reloadsOn(kind kube.ResourceKind) bool {
	if kind == m.kind || kind == m.chn.SourceKind {
		return true
	}
	// The failure card's drill-through reads the culprit workload's pods
	// (drillToPod), so its reason would go stale on a pod restart.
	if kind == kube.KindPod && m.fail != nil && m.fail.CulpritName != "" {
		return true
	}
	for _, it := range m.inventory {
		if it.Kind == kind {
			return true
		}
	}
	return false
}

func (m *Model) applyLoaded(msg loadedMsg) tea.Cmd {
	m.loadEpoch++
	if msg.pending {
		// Not found in a cache still filling: keep whatever is on screen
		// (the spinner on a first load) and look again once it has.
		return tui.ScheduleCacheSyncRetry(m.loadEpoch)
	}
	if msg.err != nil {
		m.state, m.feedback = tui.TaskStateError, msg.err.Error()
		return nil
	}
	if msg.gone {
		m.state = tui.TaskStateError
		m.feedback = m.name + " no longer exists · esc to go back"
		return nil
	}
	m.fail, m.chn, m.inventory = msg.fail, msg.chn, msg.inventory
	m.suspended, m.statusText = msg.suspended, msg.statusText
	m.state = tui.TaskStateReady
	// Drop the "loading…" note, or it sits in the keybar's right slot for
	// the rest of the session pretending a load is still in flight.
	if strings.HasPrefix(m.feedback, "loading ") {
		m.feedback = ""
	}
	m.clampOffset(m.inventoryViewportRows())
	tick := m.ensureTick()
	if msg.invPending {
		// The inventory's unresolved rows are unread, not unknowable: say
		// so, and reload once their caches settle — a kind with nothing in
		// it fills without a change event to trigger one.
		m.feedback = "loading inventory…"
		return tea.Batch(tick, tui.ScheduleCacheSyncRetry(m.loadEpoch))
	}
	return tick
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace":
		return m, func() tea.Msg { return tui.BackMsg{} }
	case "up", "k":
		m.moveSelection(-1)
	case "down", "j":
		m.moveSelection(1)
	case "ctrl+d":
		m.moveHalfPage(1)
	case "ctrl+u":
		m.moveHalfPage(-1)
	case "tab":
		m.invExpanded = !m.invExpanded
		m.clampOffset(m.inventoryViewportRows())
	case "enter":
		// ↵ lands on the object the cursor is on, which after the sort is
		// the failing one by default — §31a's "zero digs".
		if it, ok := m.selectedItem(); ok {
			return m, func() tea.Msg {
				return tui.GotoResourceMsg{Kind: it.Kind, Namespace: it.Namespace, Name: it.Name}
			}
		}
	case verbs.YAML.Key:
		if m.openYAML != nil {
			task, cmd := m.openYAML(m.kind, m.namespace, m.name, m.width, m.height)
			if task != nil {
				return task, cmd
			}
		}
	case verbs.FluxReconcile.Key:
		if m.mutator != nil && !m.offline() {
			return m, m.beginReconcile()
		}
	case verbs.FluxSuspend.Key:
		if m.mutator != nil && !m.offline() {
			return m, m.beginSuspend()
		}
	case verbs.FluxSource.Key:
		if m.chn.SourceKind != "" && m.chn.SourceName != "" {
			src := m.chn
			return m, func() tea.Msg {
				return tui.GotoResourceMsg{Kind: src.SourceKind, Namespace: src.SourceNamespace, Name: src.SourceName}
			}
		}
	}
	return m, nil
}

func (m *Model) moveSelection(delta int) {
	n := len(m.visibleInventory())
	if n == 0 {
		m.selected = 0
		return
	}
	m.selected = min(max(0, m.selected+delta), n-1)
	m.clampOffset(m.inventoryViewportRows())
}

func (m *Model) moveHalfPage(direction int) {
	position := components.MoveHalfPage(
		m.selected, m.offset, m.inventoryViewportRows(),
		len(m.visibleInventory()), direction,
	)
	m.selected, m.offset = position.Selected, position.Offset
}
