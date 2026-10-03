package jobattempts

import (
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// pasteTarget mirrors cronjobdetail's own — the type-the-name confirm
// buffer while job-replace's PROD modal is up is the only text buffer this
// screen ever shows.
func (m *Model) pasteTarget() tui.PasteTarget {
	if m.actions.Tier() != actions.TierModal {
		return nil
	}
	return m.actions.PasteTarget()
}

// Reload implements tui.Reloader — see its doc comment: this screen misses
// every kube.ResourceChangedMsg while parked in the stack, so BackMsg
// restoring it asks it to catch up immediately rather than showing stale
// data until an unrelated change happens to land while it's active again.
func (m *Model) Reload() tea.Cmd {
	if m.lister == nil {
		return nil
	}
	m.reloadEpoch++
	return m.scheduleReload(m.reloadEpoch)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cmd, ok := tui.RoutePaste(msg, m.pasteTarget()); ok {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ResourceChangedMsg:
		// Job is this screen itself; Pod feeds the attempt table without
		// being displayed directly — CLAUDE.md: "a screen reading a kind it
		// doesn't display must also reload on it."
		switch msg.Kind {
		case kube.KindJob, kube.KindPod:
			if m.lister != nil {
				m.reloadEpoch++
				return m, m.scheduleReload(m.reloadEpoch)
			}
		}
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
		m.actions.SetOffline(m.conn.Offline())
		m.now = time.Now()
	case loadedMsg:
		return m.applyLoaded(msg)
	case reloadDueMsg:
		if msg.epoch == m.reloadEpoch {
			return m, m.load()
		}
	case tui.CacheSyncRetryMsg:
		if msg.Gen == m.reloadEpoch {
			return m, m.load()
		}
	case tickMsg:
		m.now = time.Time(msg)
		return m, tickCmd()
	case spinner.TickMsg:
		if m.state != tui.TaskStateLoading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case actions.ResultMsg:
		m.actions.HandleResult(msg)
		if msg.Err == nil {
			m.reloadEpoch++
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

// tickMsg drives the UI-only clock — relative age text and DURATION-style
// live rebuild, never a lister/mutator call. Mirrors cronjobdetail's own.
type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) applyLoaded(msg loadedMsg) (tea.Model, tea.Cmd) {
	if msg.epoch != m.reloadEpoch {
		return m, nil
	}
	if msg.err != nil {
		m.state = tui.TaskStateError
		if kube.IsPermissionError(msg.err) {
			m.state = tui.TaskStatePermissionDenied
		}
		m.feedback = msg.err.Error()
		return m, nil
	}
	if !tui.KindsSynced(m.lister, m.namespace, kube.KindJob) {
		m.reloadEpoch++
		return m, tui.ScheduleCacheSyncRetry(m.reloadEpoch)
	}
	if err := tui.KindsError(m.lister, m.namespace, kube.KindJob); err != nil {
		if kube.IsPermissionError(err) {
			m.state = tui.TaskStatePermissionDenied
		} else {
			m.state = tui.TaskStateError
		}
		m.feedback = err.Error()
		return m, nil
	}
	if !msg.found {
		m.found = false
		m.state = tui.TaskStateReady
		m.feedback = ""
		return m, nil
	}

	m.found = true
	m.summary = msg.summary
	m.pods = msg.pods
	m.jobNames = msg.jobNames
	m.restoreAttemptSelection()

	m.state = tui.TaskStateReady
	m.feedback = ""
	return m, nil
}

// restoreAttemptSelection keeps the same pod selected across a reload (by
// name), falling back to a clamped index when it's gone — mirrors
// cronjobdetail's own restoreJobSelection.
func (m *Model) restoreAttemptSelection() {
	name := ""
	if a, ok := m.selectedAttemptData(); ok {
		name = a.PodName
	}
	list := m.attemptList()
	if name != "" {
		for i, a := range list {
			if a.PodName == name {
				m.selectedAttempt = i
				m.clampOffset()
				return
			}
		}
	}
	m.selectedAttempt = clamp(m.selectedAttempt, 0, len(list)-1)
	m.clampOffset()
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

// moveAttemptSelection shifts the attempt table/index-grid's own selected
// row by delta, clamped (j/k ≡ ↑↓ everywhere, CLAUDE.md convention).
func (m *Model) moveAttemptSelection(delta int) {
	n := len(m.attemptList())
	if n == 0 {
		return
	}
	m.selectedAttempt = clamp(m.selectedAttempt+delta, 0, n-1)
	m.clampOffset()
}

// clampOffset keeps the selected row within the table's rendered viewport —
// mirrors cronjobdetail's own.
func (m *Model) clampOffset() {
	rows := m.tableDataRows()
	if m.selectedAttempt < m.offset {
		m.offset = m.selectedAttempt
	}
	if m.selectedAttempt >= m.offset+rows {
		m.offset = m.selectedAttempt - rows + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

// moveSibling shifts to the next/prev sibling Job without leaving the
// screen — mirrors cronjobdetail's own moveSibling.
func (m *Model) moveSibling(delta int) tea.Cmd {
	if len(m.siblings) == 0 {
		return nil
	}
	next := m.siblingIndex + delta
	if next < 0 || next >= len(m.siblings) {
		return nil
	}
	m.siblingIndex = next
	ref := m.siblings[next]
	m.namespace, m.name = ref.Namespace, ref.Name
	m.found = false
	m.summary = resources.JobAttemptsSummary{}
	m.pods = nil
	m.selectedAttempt, m.offset = 0, 0
	m.failedOnly, m.diffMode = false, false
	m.pendingRerun = nil
	m.execFeedback = ""
	m.state = tui.TaskStateLoading
	m.feedback = "Loading " + m.name + "..."
	if m.lister == nil {
		m.state = tui.TaskStateError
		m.feedback = "no cluster connection"
		return nil
	}
	m.reloadEpoch++
	return tea.Batch(m.load(), m.spinner.Tick)
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.actions.Active() {
		return m.updateConfirmKey(msg)
	}
	if m.pendingRerun != nil {
		return m.updateRerunKey(msg)
	}
	if m.state == tui.TaskStateLoading {
		// found is false until the first load (and every sibling move's
		// load) lands, so the "deleted · press any key to go back" branch
		// below must not see these keys: a key pressed on open, or a held
		// ']' walking siblings, would leave the screen. Only leaving and
		// sibling movement mean anything before there's an object.
		switch msg.String() {
		case "ctrl+q", "ctrl+c":
			return m, tea.Quit
		case "esc", "backspace":
			return m, func() tea.Msg { return tui.BackMsg{} }
		case "]":
			return m, m.moveSibling(1)
		case "[":
			return m, m.moveSibling(-1)
		}
		return m, nil
	}
	if !m.found {
		switch msg.String() {
		case "ctrl+q", "ctrl+c":
			return m, tea.Quit
		default:
			return m, func() tea.Msg { return tui.BackMsg{} }
		}
	}
	if m.diffMode {
		switch msg.String() {
		case verbs.JobAttemptDiff.Key, "esc":
			m.diffMode = false
		}
		return m, nil
	}
	switch msg.String() {
	case "ctrl+q", "ctrl+c":
		return m, tea.Quit
	case "esc", "backspace":
		return m, func() tea.Msg { return tui.BackMsg{} }
	case "]":
		return m, m.moveSibling(1)
	case "[":
		return m, m.moveSibling(-1)
	case "up", "k":
		m.moveAttemptSelection(-1)
	case "down", "j":
		m.moveAttemptSelection(1)
	case "enter":
		if task, cmd, ok := m.openSelectedPodDetail(); ok {
			if task != nil {
				return task, cmd
			}
			return m, cmd
		}
	case verbs.Logs.Key:
		if task, cmd, ok := m.openSelectedLogs(); ok {
			if task != nil {
				return task, cmd
			}
			return m, cmd
		}
	case verbs.JobAttemptDiff.Key:
		if len(m.summary.Attempts) >= 2 {
			m.diffMode = true
		}
	case verbs.Events.Key:
		if task, cmd, ok := m.openSelectedEvents(); ok {
			return task, cmd
		}
	case verbs.YAML.Key:
		if task, cmd, ok := m.openSelectedYAML(); ok {
			return task, cmd
		}
	case verbs.JobIndexFailedOnly.Key:
		if m.summary.Indexed {
			m.failedOnly = !m.failedOnly
			m.selectedAttempt, m.offset = 0, 0
		}
	case verbs.JobRetry.Key:
		if verbs.JobRetry.HiddenWhileOffline(m.conn.Offline()) {
			return m, nil
		}
		m.beginRerun()
	}
	return m, nil
}

// updateConfirmKey routes keys while a confirmation is showing — mirrors
// cronjobdetail's own (this screen's only TierModal verb is job-replace's
// PROD escalation).
func (m *Model) updateConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.actions.Tier() == actions.TierModal {
		return m.updateModalConfirmKey(msg)
	}
	switch msg.String() {
	case "y":
		return m, m.actions.Confirm()
	case "n", "esc":
		m.actions.Cancel()
	}
	return m, nil
}

// updateModalConfirmKey drives job-replace's PROD type-the-name modal —
// mirrors cronjobdetail's own.
func (m *Model) updateModalConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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

// openSelectedLogs opens logs for the selected attempt's pod — task is nil
// with a non-nil cmd only when nothing can be opened, mirrors
// cronjobdetail's own openSelectedLogs.
func (m *Model) openSelectedLogs() (tea.Model, tea.Cmd, bool) {
	if m.openLogs == nil {
		return nil, nil, false
	}
	a, ok := m.selectedAttemptData()
	if !ok {
		return nil, nil, false
	}
	pod, ok := m.pods[a.PodName]
	if !ok {
		m.execFeedback = a.PodName + ": pod no longer in cache — no retained log tail (v0.9.0 scope)"
		return nil, nil, false
	}
	m.execFeedback = ""
	task, cmd := m.openLogs(pod, "", m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedPodDetail pushes tasks/poddetail (5a) for the selected
// attempt's pod — single-pod handoff, mirrors nodedetail's openSelectedPod.
func (m *Model) openSelectedPodDetail() (tea.Model, tea.Cmd, bool) {
	if m.openPodDetail == nil {
		return nil, nil, false
	}
	a, ok := m.selectedAttemptData()
	if !ok {
		return nil, nil, false
	}
	pod, ok := m.pods[a.PodName]
	if !ok {
		m.execFeedback = a.PodName + ": pod no longer in cache — no retained log tail (v0.9.0 scope)"
		return nil, nil, false
	}
	m.execFeedback = ""
	task, cmd := m.openPodDetail(pod, []string{pod.Name}, 0, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedYAML pushes 8a for the loaded Job itself.
func (m Model) openSelectedYAML() (tea.Model, tea.Cmd, bool) {
	if m.openYAML == nil || !m.found {
		return nil, nil, false
	}
	task, cmd := m.openYAML(kube.KindJob, m.namespace, m.name, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedEvents pushes 9b object-scoped for the loaded Job itself.
func (m Model) openSelectedEvents() (tea.Model, tea.Cmd, bool) {
	if m.openEvents == nil || !m.found {
		return nil, nil, false
	}
	task, cmd := m.openEvents(kube.KindJob, m.namespace, m.name, m.width, m.height)
	return task, cmd, task != nil
}
