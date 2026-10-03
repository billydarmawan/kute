package overview

import (
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ResourceChangedMsg:
		if isOverviewKind(msg.Kind) && m.lister != nil {
			return m, m.scheduleReload()
		}
	case reloadDueMsg:
		m.reloadPending = false
		if m.lister != nil {
			return m, m.startLoad()
		}
	case kube.ConnStateMsg:
		wasOffline := m.conn.Offline()
		m.conn = kube.ConnState(msg)
		if wasOffline && !m.conn.Offline() {
			// pollsMetrics refuses to arm while offline, so the chain died
			// when the connection dropped; coming back up has to restart it.
			return m, m.restartMetrics()
		}
	case tui.CacheSyncRetryMsg:
		if msg.Gen == m.reloadEpoch && m.lister != nil {
			return m, m.startLoad()
		}
	case loadedMsg:
		return m.applyLoaded(msg)
	case metricsTickMsg:
		// A tick from a retired chain is dropped rather than re-armed —
		// that is what keeps exactly one poll loop alive.
		if msg.epoch == m.metricsEpoch {
			return m, m.loadMetrics()
		}
	case metricsLoadedMsg:
		if msg.epoch != m.metricsEpoch {
			return m, nil
		}
		m.metricsPolled = true
		if msg.err != nil && m.metricsAvailable && !kube.IsMetricsServerMissing(msg.err) {
			// A failed poll (timeout, Forbidden, a transient 503) says
			// nothing about usage: keep the last good reading rather than
			// flipping CAPACITY to a note until the next poll lands.
			m.metricsErr = msg.err
			return m, m.scheduleMetricsTick(m.metricsEpoch)
		}
		m.metricsAvailable, m.metricsErr = msg.available, msg.err
		m.capCPUUsed, m.capMemUsed = msg.cpuUsed, msg.memUsed
		return m, m.scheduleMetricsTick(m.metricsEpoch)
	case spinner.TickMsg:
		if m.state != tui.TaskStateLoading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	}
	return m, nil
}

// Reload implements tui.Reloader: the root shell calls this when BackMsg
// restores this screen from the stack, since it missed every
// kube.ResourceChangedMsg while parked underneath whatever it pushed (only
// the active task's Update sees those) — without it, a pod deleted from a
// screen opened via TROUBLE's ↵ would keep showing up here until some
// unrelated change happened to land while this screen was active again.
func (m *Model) Reload() tea.Cmd {
	if m.lister == nil {
		return nil
	}
	// A debounced reload armed before this screen was covered fired at
	// whatever was active instead; this load supersedes it. The usage
	// poll's chain died the same way.
	m.reloadPending = false
	return tea.Batch(m.startLoad(), m.restartMetrics())
}

// isOverviewKind reports whether kind's change should trigger a reload —
// every kind 19a's panels read from (Node/Pod/Namespace/ReplicaSet, plus
// HelmRelease for the TROUBLE panel's outdated tail), not every possible
// kind change. A kind the screen displays but doesn't reload on goes stale
// on screen, which is what browse's own auxKinds exists to prevent.
func isOverviewKind(kind kube.ResourceKind) bool {
	switch kind {
	case kube.KindNode, kube.KindPod, kube.KindNamespace, kube.KindReplicaSet, kube.KindHelmRelease:
		return true
	default:
		return false
	}
}

func (m *Model) applyLoaded(msg loadedMsg) (tea.Model, tea.Cmd) {
	// Loads race — the slowest reply is not necessarily the oldest — so
	// only one newer than everything already applied may land.
	if msg.seq <= m.appliedSeq {
		return m, nil
	}
	m.appliedSeq = msg.seq
	if msg.err != nil {
		m.state = tui.TaskStateError
		if kube.IsPermissionError(msg.err) {
			m.state = tui.TaskStatePermissionDenied
		}
		m.feedback = msg.err.Error()
		return m, nil
	}
	// loadOverview's own ListRaw calls never return an error for a denied
	// cache — a Forbidden reflector just leaves it empty (CLAUDE.md's own
	// invariant) — so a permanently-denied Node or Pod cache would otherwise
	// sail through the err check above and render as a cluster with zero
	// nodes/pods instead of a permission-denied card. Node and Pod are the
	// two required caches (every other read here — Namespace, HelmRelease,
	// ReplicaSet — is already best-effort per loadOverview's own comments).
	if !tui.KindsSynced(m.lister, "", kube.KindNode, kube.KindPod) {
		m.reloadEpoch++
		return m, tui.ScheduleCacheSyncRetry(m.reloadEpoch)
	}
	if err := tui.KindsError(m.lister, "", kube.KindNode, kube.KindPod); err != nil {
		if kube.IsPermissionError(err) {
			m.state = tui.TaskStatePermissionDenied
		} else {
			m.state = tui.TaskStateError
		}
		m.feedback = err.Error()
		return m, nil
	}
	d := msg.data
	m.version = d.version
	m.nodeCount = d.nodeCount
	m.podCount = d.podCount
	m.nsCount = d.nsCount
	m.capCPUTotal, m.capMemTotal = d.capCPUTotal, d.capMemTotal
	m.capPodsUsed, m.capPodsTotal = d.capPodsUsed, d.capPodsTotal
	m.nodeTrouble, m.nodeHealthy = d.nodeTrouble, d.nodeHealthy
	m.podTrouble, m.podHealthy = d.podTrouble, d.podHealthy
	m.helmOutdated = d.helmOutdated
	m.changes = d.changes
	m.nodesSel = clamp(m.nodesSel, 0, cappedMax(len(m.nodeTrouble)))
	m.troubleSel = clamp(m.troubleSel, 0, cappedMax(len(m.troubleEntries())))
	m.changesSel = clamp(m.changesSel, 0, cappedMax(len(m.changes)))

	// Namespace/HelmRelease/ReplicaSet are secondary/best-effort caches
	// (loadOverview's own reads swallow their errors) — a denial here
	// decorates just the fact/panel it backs rather than promoting to the
	// full-screen state Node/Pod's denial does above
	// (docs/lazy-informers.md §5.6). Pending is the same
	// idea for the transient case: a cache that hasn't finished its initial
	// fill yet reads as an "unknown right now" dash instead of a false
	// zero/empty, and self-heals via the retry below rather than waiting on
	// a change event a genuinely-empty cache would never emit.
	m.nsPending, m.nsDenied = bestEffort(m.lister, kube.KindNamespace)
	m.helmPending, m.helmDenied = bestEffort(m.lister, kube.KindHelmRelease)
	m.changesPending, m.changesDenied = bestEffort(m.lister, kube.KindReplicaSet)

	m.state = tui.TaskStateReady
	m.feedback = ""

	var retry tea.Cmd
	if m.nsPending || m.helmPending || m.changesPending {
		m.reloadEpoch++
		retry = tui.ScheduleCacheSyncRetry(m.reloadEpoch)
	}
	return m, retry
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.state != tui.TaskStateReady {
		switch msg.String() {
		case "ctrl+q", "ctrl+c":
			return m, tea.Quit
		case "esc", "backspace":
			return m, func() tea.Msg { return tui.BackMsg{} }
		}
		return m, nil
	}

	switch msg.String() {
	case "ctrl+q", "ctrl+c":
		return m, tea.Quit
	case "esc", "backspace":
		return m, func() tea.Msg { return tui.BackMsg{} }
	case "tab":
		m.nextPanel()
	case "up", "k":
		m.moveSelection(-1)
	case "down", "j":
		m.moveSelection(1)
	case "enter":
		if task, cmd, ok := m.openSelected(); ok {
			return task, cmd
		}
	case verbs.Timeline.Key:
		if m.openTimeline != nil {
			task, cmd := m.openTimeline("", m.width, m.height)
			if task != nil {
				return task, cmd
			}
		}
	case verbs.Events.Key:
		if m.openEvents != nil {
			task, cmd := m.openEvents("", m.width, m.height)
			if task != nil {
				return task, cmd
			}
		}
	}
	return m, nil
}

// nextPanel is 19a's `↹` — cycles focus NODES → TROUBLE → CHANGES → NODES,
// skipping an empty panel so the cursor never lands somewhere with nothing
// to select.
func (m *Model) nextPanel() {
	order := []panel{panelNodes, panelTrouble, panelChanges}
	for range order {
		m.focus = order[(int(m.focus)+1)%len(order)]
		if m.panelHasRows(m.focus) {
			return
		}
	}
}

func (m Model) panelHasRows(p panel) bool {
	switch p {
	case panelNodes:
		return len(m.nodeTrouble) > 0
	case panelTrouble:
		return len(m.troubleEntries()) > 0
	case panelChanges:
		return len(m.changes) > 0
	default:
		return false
	}
}

func (m *Model) moveSelection(delta int) {
	switch m.focus {
	case panelNodes:
		m.nodesSel = clamp(m.nodesSel+delta, 0, cappedMax(len(m.nodeTrouble)))
	case panelTrouble:
		m.troubleSel = clamp(m.troubleSel+delta, 0, cappedMax(len(m.troubleEntries())))
	case panelChanges:
		m.changesSel = clamp(m.changesSel+delta, 0, cappedMax(len(m.changes)))
	}
}

// cappedMax is the highest selectable index in a panel whose display caps
// at maxPanelRows (view.go): the cursor must never move past what's
// actually rendered, so selection clamps against the same cap the fold-line
// ("+N more") accounts for.
func cappedMax(n int) int {
	return max(min(n, maxPanelRows)-1, 0)
}

// openSelected dispatches ↵ for whichever panel is focused: NODES pushes
// 11b directly; TROUBLE/CHANGES jump to the object via tui.GotoResource
// (jumpTo), the same navigation tasks/timeline's own openSelectedObject and
// the root shell's palette Enter use.
func (m Model) openSelected() (tea.Model, tea.Cmd, bool) {
	switch m.focus {
	case panelNodes:
		row, ok := m.selectedNode()
		if !ok || m.openNodeDetail == nil {
			return nil, nil, false
		}
		task, cmd := m.openNodeDetail(row.Name, m.width, m.height)
		return task, cmd, task != nil
	case panelTrouble:
		entry, ok := m.selectedTrouble()
		if !ok {
			return nil, nil, false
		}
		return nil, m.jumpTo(entry.kind, entry.row.Namespace, entry.row.Name), true
	case panelChanges:
		entry, ok := m.selectedChange()
		if !ok {
			return nil, nil, false
		}
		kind, name := splitObject(entry.Object)
		if kind == "" || name == "" {
			return nil, nil, false
		}
		return nil, m.jumpTo(kind, entry.Namespace, name), true
	default:
		return nil, nil, false
	}
}

func (m Model) jumpTo(kind kube.ResourceKind, namespace, name string) tea.Cmd {
	return tui.GotoResource(m.session, kind, namespace, name)
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
