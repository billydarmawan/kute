package cronjobdetail

import (
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/metapanel"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// pasteTarget mirrors updateKey's own routing precedence: the type-the-name
// confirm buffer while a PROD-tier suspend modal is up, else the open meta
// panel's focused buffer (its own resolver picks the right one of its three).
func (m *Model) pasteTarget() tui.PasteTarget {
	switch {
	case m.actions.Tier() == actions.TierModal:
		return m.actions.PasteTarget()
	case m.meta != nil && !m.actions.Active():
		return m.meta.PasteTarget()
	}
	return nil
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
		// CronJob is this screen itself; Job/Pod feed the Jobs table and
		// failure band without being displayed directly — a kind read but
		// not listed here goes stale (CLAUDE.md: "a screen reading a kind it
		// doesn't display must also reload on it").
		switch msg.Kind {
		case kube.KindCronJob, kube.KindJob, kube.KindPod:
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
		if metapanel.IsActionID(msg.ActionID) && m.meta != nil {
			// 26a never closes the panel on a commit's own account, success
			// or failure — HandleResult refreshes the grid or restores the
			// pre-commit state with the server's error; closed here only
			// means the object vanished mid-edit. On success the screen's
			// own body (the facts grid) reloads too.
			if m.meta.HandleResult(msg) {
				m.meta = nil
			}
			// The panel's will-run strip is this result's surface.
			m.actions.DismissResult()
			if msg.Err == nil {
				m.reloadEpoch++
				return m, m.load()
			}
			return m, nil
		}
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

// tickMsg drives the UI-only clock (§4.5's shared contract): relative ETA/
// age text and the Jobs table's live rebuild, never a lister/mutator call.
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
	// §4.4: check CronJob+Job settlement even when a CronJob was already
	// found — a Job cache that hasn't finished filling yet must never render
	// as a false "no retained runs" (see below, jobsErr/found handling).
	if !tui.KindsSynced(m.lister, m.namespace, kube.KindCronJob, kube.KindJob) {
		m.reloadEpoch++
		return m, tui.ScheduleCacheSyncRetry(m.reloadEpoch)
	}
	if err := tui.KindsError(m.lister, m.namespace, kube.KindCronJob); err != nil {
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
	m.jobsErr = msg.jobsErr
	m.pods = msg.pods
	m.controller = msg.controller
	m.restoreJobSelection()

	m.state = tui.TaskStateReady
	m.feedback = ""
	return m, nil
}

// restoreJobSelection keeps the same Job selected across a reload (by name),
// falling back to a clamped index when it's gone — mirrors nodedetail's own
// restoreSelection.
func (m *Model) restoreJobSelection() {
	name := ""
	if job, ok := m.selectedJobSummary(); ok {
		name = job.Name
	}
	if name != "" {
		for i, r := range m.summary.Runs {
			if r.Name == name {
				m.selectedJob = i
				m.clampOffset()
				return
			}
		}
	}
	m.selectedJob = clamp(m.selectedJob, 0, len(m.summary.Runs)-1)
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

// moveJobSelection shifts the Jobs table's selected row by delta, clamped
// (j/k ≡ ↑↓ everywhere, CLAUDE.md convention) — mirrors poddetail's own
// moveContainerSelection/nodedetail's moveSelection.
func (m *Model) moveJobSelection(delta int) {
	n := len(m.summary.Runs)
	if n == 0 {
		return
	}
	m.selectedJob = clamp(m.selectedJob+delta, 0, n-1)
	m.clampOffset()
}

// clampOffset keeps the selected Job row within the table's rendered
// viewport — mirrors browse/nodedetail's own clampOffset.
func (m *Model) clampOffset() {
	rows := m.tableDataRows()
	if m.selectedJob < m.offset {
		m.offset = m.selectedJob
	}
	if m.selectedJob >= m.offset+rows {
		m.offset = m.selectedJob - rows + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

// moveSibling shifts to the next/prev sibling CronJob without leaving detail
// (§3.6/§36e: "[/] move between sibling CronJobs, matching poddetail's own
// movement contract") — a no-op at either end or with no sibling list at
// all, mirrors poddetail's own moveSibling.
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
	m.summary = resources.CronJobSummary{}
	m.jobsErr = nil
	m.pods = nil
	m.controller = ""
	m.selectedJob, m.offset = 0, 0
	m.pendingRun, m.pendingResume = nil, nil
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
	if m.meta != nil {
		// The open 26a panel captures every key (metapanel's package doc
		// comment) — the closed-panel bindings below, 'y' YAML and the Jobs
		// table's own j/k and [/] included, only apply once esc closes it.
		closed, cmd := m.meta.Update(msg, &m.actions)
		if closed {
			m.meta = nil
		}
		return m, cmd
	}
	if m.pendingRun != nil {
		return m.updateRunKey(msg)
	}
	if m.pendingResume != nil {
		return m.updateResumeKey(msg)
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
		m.moveJobSelection(-1)
	case "down", "j":
		m.moveJobSelection(1)
	case "enter":
		if cmd, ok := m.openSelectedJob(); ok {
			return m, cmd
		}
	case verbs.Logs.Key:
		if task, cmd, ok := m.openSelectedLogs(); ok {
			if task != nil {
				return task, cmd
			}
			return m, cmd
		}
	case verbs.YAML.Key:
		if task, cmd, ok := m.openSelectedYAML(); ok {
			return task, cmd
		}
	case verbs.Events.Key:
		if task, cmd, ok := m.openSelectedEvents(); ok {
			return task, cmd
		}
	case verbs.CronJobRunNow.Key:
		if verbs.CronJobRunNow.HiddenWhileOffline(m.conn.Offline()) {
			return m, nil
		}
		m.beginRunNow()
	case verbs.CronJobSuspend.Key:
		if verbs.CronJobSuspend.HiddenWhileOffline(m.conn.Offline()) {
			return m, nil
		}
		return m, m.beginSuspendOrResume()
	case verbs.CronJobSetSchedule.Key:
		if task, cmd, ok := m.openSelectedSchedule(); ok {
			return task, cmd
		}
	case verbs.Meta.Key:
		// 26a on the loaded CronJob — the same guard poddetail's own 'm'
		// uses (mutator wired, object loaded); Editable(CronJob) is always
		// true.
		if m.found && m.mutator != nil && !verbs.Meta.HiddenWhileOffline(m.conn.Offline()) {
			if p, ok := metapanel.Open(metapanel.Config{Session: m.session, Lister: m.lister}, kube.KindCronJob, m.namespace, m.name); ok {
				m.meta = p
			}
		}
	}
	return m, nil
}

// updateConfirmKey routes keys while a confirmation is showing — mirrors
// poddetail's own updateConfirmKey (this screen's only TierModal verb is
// cronjob-suspend's PROD escalation, so there's no delete-family ctrl-k
// force-delete branch to carry).
func (m *Model) updateConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.actions.Tier() == actions.TierModal {
		return m.updateModalConfirmKey(msg)
	}
	switch msg.String() {
	case "y":
		return m, m.actions.Confirm()
	case "n", "esc":
		m.cancelInlineConfirm()
	}
	return m, nil
}

// cancelInlineConfirm is updateConfirmKey's shared "actually cancel" path —
// when the cancelled confirm was a 26a commit (the panel stays open under
// its TierInline y/N), the row's buffer must revert too, per the panel's
// hosting contract (metapanel.CancelConfirm's doc comment). A no-op for the
// screen's other inline confirms (suspend, resume): m.meta is nil then.
func (m *Model) cancelInlineConfirm() {
	if m.meta != nil {
		m.meta.CancelConfirm()
	}
	m.actions.Cancel()
}

// updateModalConfirmKey drives cronjob-suspend's PROD type-the-name modal —
// mirrors poddetail's own updateModalConfirmKey, minus the ctrl-k
// force-delete escalation (not applicable to a CronJob suspend).
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

// openSelectedJob emits the shared goto navigation onto the Jobs table's
// selected Job (§3.7/§36e: "enter emits the shared goto navigation onto the
// exact selected Job" — a bespoke Job attempt-ledger is v0.9.0 scope).
func (m Model) openSelectedJob() (tea.Cmd, bool) {
	job, ok := m.selectedJobSummary()
	if !ok {
		return nil, false
	}
	return tui.GotoResource(m.session, kube.KindJob, job.Namespace, job.Name), true
}

// openSelectedLogs opens logs for the selected Job's best available Pod
// (§36e task 12) — task is nil with a non-nil cmd only when nothing can be
// opened (mirrors the "explain when collected/unavailable" contract via
// execFeedback, poddetail's own openSelectedExec's task/cmd/ok shape).
func (m *Model) openSelectedLogs() (tea.Model, tea.Cmd, bool) {
	if m.openLogs == nil {
		return nil, nil, false
	}
	job, ok := m.selectedJobSummary()
	if !ok {
		return nil, nil, false
	}
	if job.PodName == "" {
		m.execFeedback = job.Name + ": no pod collected for this run yet"
		return nil, nil, false
	}
	pod, ok := m.pods[job.PodName]
	if !ok {
		pod = kube.Pod{Namespace: job.Namespace, Name: job.PodName}
	}
	m.execFeedback = ""
	task, cmd := m.openLogs(pod, "", m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedYAML pushes 8a for the loaded CronJob itself.
func (m Model) openSelectedYAML() (tea.Model, tea.Cmd, bool) {
	if m.openYAML == nil || !m.found {
		return nil, nil, false
	}
	task, cmd := m.openYAML(kube.KindCronJob, m.namespace, m.name, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedEvents pushes 9b object-scoped for the loaded CronJob itself.
func (m Model) openSelectedEvents() (tea.Model, tea.Cmd, bool) {
	if m.openEvents == nil || !m.found {
		return nil, nil, false
	}
	task, cmd := m.openEvents(kube.KindCronJob, m.namespace, m.name, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedSchedule pushes 36d for the loaded CronJob itself.
func (m Model) openSelectedSchedule() (tea.Model, tea.Cmd, bool) {
	if m.openSchedule == nil || !m.found {
		return nil, nil, false
	}
	task, cmd := m.openSchedule(m.namespace, m.name, m.width, m.height)
	return task, cmd, task != nil
}
