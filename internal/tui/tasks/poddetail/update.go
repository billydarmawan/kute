package poddetail

import (
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/metapanel"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// pasteTarget mirrors updateKey's own routing precedence: the type-the-name
// confirm buffer while a PROD-tier delete modal is up, else the open meta
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
	return tea.Batch(m.load(), m.restartMetrics())
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cmd, ok := tui.RoutePaste(msg, m.pasteTarget()); ok {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ResourceChangedMsg:
		// 26a's open panel re-reads its object (and Service joins) from the
		// cache this event just updated — see metapanel.Refresh.
		if m.meta != nil {
			m.meta.Refresh(msg.Kind, &m.actions)
		}
		// The pod itself, plus the caches its panels are built from without
		// displaying directly: Events feeds the EVENTS grid; ReplicaSets
		// resolve the owner hop up to a Deployment for the meta grid and
		// RELATED's numbered owner entry; Services/Ingresses resolve
		// RELATED's numbered Service/Ingress entries;
		// PersistentVolumeClaims resolve RELATED's numbered PVC entries
		// (load.go's resolveRelatedItems) — a kind read but not listed here
		// goes stale (CLAUDE.md: a screen reading a kind it doesn't display
		// must still reload on it).
		switch msg.Kind {
		// The controller's own kind is read too, for the Flux labels that
		// resolve RELATED's reconciler entry (resolveFluxItem).
		case kube.KindPod, kube.KindEvent, kube.KindReplicaSet, kube.KindService, kube.KindIngress, kube.KindPersistentVolumeClaim,
			kube.KindDeployment, kube.KindStatefulSet, kube.KindDaemonSet, kube.KindJob:
			if m.lister != nil {
				return m, m.load()
			}
		}
	case kube.ConnStateMsg:
		wasOffline := m.conn.Offline()
		m.conn = kube.ConnState(msg)
		m.actions.SetOffline(m.conn.Offline())
		if wasOffline && !m.conn.Offline() && m.lister != nil {
			return m, m.restartMetrics()
		}
	case loadedMsg:
		return m.applyLoaded(msg)
	case tui.CacheSyncRetryMsg:
		if msg.Gen == m.syncRetryGen && m.lister != nil {
			return m, m.load()
		}
	case metricsTickMsg:
		if msg.epoch == m.metricsEpoch && m.pollsMetrics() {
			return m, m.loadMetrics()
		}
	case metricsLoadedMsg:
		if msg.epoch != m.metricsEpoch {
			return m, nil
		}
		m.usage, m.usageOK = msg.usage, msg.ok
		if m.found {
			m.applyUsage()
		}
		return m, m.scheduleMetricsTick(m.metricsEpoch)
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
			// own body reloads too.
			if m.meta.HandleResult(msg) {
				m.meta = nil
			}
			// The panel's will-run strip is this result's surface.
			m.actions.DismissResult()
			if msg.Err == nil {
				return m, m.load()
			}
			return m, nil
		}
		if msg.Err == nil {
			return m, m.load()
		}
	case execResultMsg:
		if msg.err != nil {
			m.execFeedback = "exec exited: " + msg.err.Error()
		} else {
			m.execFeedback = ""
		}
	case editResultMsg:
		if msg.err != nil {
			m.execFeedback = "edit exited: " + msg.err.Error()
		} else {
			m.execFeedback = ""
		}
	case podShellsProbedMsg:
		return m.routePodShellsProbed(msg)
	case tea.KeyPressMsg:
		// Any key acknowledges the last action's outcome line; the key
		// still does what it would have.
		m.actions.DismissResult()
		return m.updateKey(msg)
	}
	return m, nil
}

// execResultMsg carries a directly-run (single-container, no picker pushed)
// kubectl exec's exit outcome — same contract as tasks/execpicker's own
// execResultMsg and browse's, duplicated per the repo's
// package-local-seam convention.
type execResultMsg struct{ err error }

// editResultMsg carries a kubectl edit exit outcome (edit.go) — same
// feedback channel as execResultMsg, kept as its own type for the same
// reason.
type editResultMsg struct{ err error }

func (m *Model) applyLoaded(msg loadedMsg) (tea.Model, tea.Cmd) {
	if msg.epoch != m.loadEpoch {
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
	if !msg.found {
		// "Pod deleted" is a claim about the cluster. A Pod cache on its
		// first read of this namespace (--namespace-scoped opening a pod in
		// another one) answers empty until its LIST lands, and one that
		// stopped filling says why instead.
		if err := tui.KindsError(m.lister, m.namespace, kube.KindPod); err != nil {
			m.state = tui.TaskStateError
			if kube.IsPermissionError(err) {
				m.state = tui.TaskStatePermissionDenied
			}
			m.feedback = err.Error()
			return m, nil
		}
		if !tui.KindsSynced(m.lister, m.namespace, kube.KindPod) {
			m.syncRetryGen++
			return m, tui.ScheduleCacheSyncRetry(m.syncRetryGen)
		}
		m.gone = true
		m.state = tui.TaskStateReady
		m.feedback = ""
		return m, nil
	}
	m.gone = false
	m.pod = msg.pod
	m.status = msg.status
	m.applyUsage()
	m.found = true
	m.eventRows = msg.events
	m.eventsErr = msg.eventsErr
	// "no events" is a claim about the cluster: a cache that stopped filling
	// (Forbidden, a failing LIST) says why instead, and one still on its
	// first LIST says it's loading and retries — a pod with genuinely no
	// events syncs without ever emitting a change event.
	var retry tea.Cmd
	m.eventsPending = false
	if len(msg.events) == 0 && msg.eventsErr == nil && m.events != nil {
		if err := tui.KindsError(m.lister, m.namespace, kube.KindEvent); err != nil {
			m.eventsErr = err
		} else if !tui.KindsSynced(m.lister, m.namespace, kube.KindEvent) {
			m.eventsPending = true
			m.syncRetryGen++
			retry = tui.ScheduleCacheSyncRetry(m.syncRetryGen)
		}
	}
	m.controller = msg.controller
	m.related = msg.related
	m.sources = msg.sources
	m.configErrors = msg.configErrors
	if total := m.totalContainerRows(); m.selectedContainer >= total {
		m.selectedContainer = max(total-1, 0)
	}
	m.state = tui.TaskStateReady
	m.feedback = ""
	return m, retry
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.actions.Active() {
		return m.updateConfirmKey(msg)
	}
	if m.pendingEdit != nil {
		return m.updateEditConfirmKey(msg)
	}
	if m.meta != nil {
		// The open 26a panel captures every key (metapanel's package doc
		// comment) — the closed-panel bindings below, 'y' YAML included,
		// only apply once esc closes it.
		closed, cmd := m.meta.Update(msg, &m.actions)
		if closed {
			m.meta = nil
		}
		return m, cmd
	}
	if m.gone {
		// "pod gone ⇒ banner + auto-back after keypress" (docs/design
		// README.md §5a) — every key returns to browse, except q/ctrl+c:
		// quit is a global expectation everywhere else in the app, and this
		// banner shouldn't be the one state where it silently does something
		// else instead.
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
		m.moveContainerSelection(-1)
	case "down", "j":
		m.moveContainerSelection(1)
	case "ctrl+d":
		m.moveContainerHalfPage(1)
	case "ctrl+u":
		m.moveContainerHalfPage(-1)
	case verbs.PageDown.Key:
		m.moveBodyPage(1)
	case verbs.PageUp.Key:
		m.moveBodyPage(-1)
	case verbs.Logs.Key:
		if task, cmd, ok := m.openSelectedLogs(); ok {
			return task, cmd
		}
	case "enter":
		// §41e: re-attach to an already-created ephemeral container — a
		// real container in the pod once attached, exec'd into the same
		// way any other container is (kube.ExecSpec), no new kube
		// function needed. No-op anywhere else in this grid; enter has no
		// other meaning on an ordinary CONTAINERS row.
		if eph, ok := m.selectedEphemeralContainer(); ok {
			if verbs.Exec.HiddenWhileOffline(m.conn.Offline()) {
				return m, nil
			}
			return m, execCmd(m.session.CommandTarget(), m.namespace, m.name, eph.Name, m.demo)
		}
	case verbs.YAML.Key:
		if task, cmd, ok := m.openSelectedYAML(); ok {
			return task, cmd
		}
	case verbs.Events.Key:
		if task, cmd, ok := m.openSelectedEvents(); ok {
			return task, cmd
		}
	case verbs.Timeline.Key:
		if task, cmd, ok := m.openSelectedTimeline(); ok {
			return task, cmd
		}
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		if task, cmd, ok := m.openRelated(int(msg.String()[0] - '1')); ok {
			if task != nil {
				return task, cmd
			}
			return m, cmd
		}
	case verbs.EnvMounts.Key:
		if m.found {
			m.showSources = !m.showSources
			m.bodyOffset = min(m.bodyOffset, m.bodyMaxOffset())
		}
	case verbs.Exec.Key:
		// Exec is Mutating: refused while offline (docs/design README.md
		// §52). keys.go has already dropped the hint and switched the keybar
		// to OFFLINE · "mutating actions disabled".
		if verbs.Exec.HiddenWhileOffline(m.conn.Offline()) {
			return m, nil
		}
		if task, cmd, ok := m.beginExecOrDebug(); ok {
			if task != nil {
				return task, cmd
			}
			return m, cmd
		}
	case verbs.Forward.Key:
		if task, cmd, ok := m.openSelectedForward(); ok {
			return task, cmd
		}
	case verbs.Edit.Key:
		// kubectl edit applies whatever the user saves, so it's gated with
		// the other mutating verbs while offline (docs/design README.md
		// §4a: "delete/exec/edit verbs are disabled while offline").
		if verbs.Edit.HiddenWhileOffline(m.conn.Offline()) {
			return m, nil
		}
		if cmd, ok := m.beginEdit(); ok {
			return m, cmd
		}
	case verbs.Delete.Key:
		return m, m.beginDelete()
	case verbs.Meta.Key:
		// 26a on the loaded pod — the same guard browse's own 'm' uses
		// (mutator wired, object loaded); Editable(Pod) is always true.
		if m.found && m.mutator != nil && !verbs.Meta.HiddenWhileOffline(m.conn.Offline()) {
			if p, ok := metapanel.Open(metapanel.Config{Session: m.session, Lister: m.lister}, kube.KindPod, m.namespace, m.name); ok {
				m.meta = p
			}
		}
	}
	return m, nil
}

// updateConfirmKey routes keys while a confirmation is showing: TierModal
// (the type-the-name PROD modal) gets its own key handling — typing,
// backspace, force-delete escalation, enter-when-matched — while
// TierInline/TierNone stay the simple y/n/esc prompt (mvp-plan.md §8b), plus
// verbs.ForceDelete.Key on the inline delete confirm, which escalates it
// into the type-the-name modal (Controller.EscalateForceDelete) — force delete
// is always modal, PROD or not.
func (m *Model) updateConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.actions.Tier() == actions.TierModal {
		return m.updateModalConfirmKey(msg)
	}
	switch msg.String() {
	case "y":
		return m, m.actions.Confirm()
	case verbs.ForceDelete.Key:
		m.actions.EscalateForceDelete(m.isProd())
	case "n", "esc":
		m.cancelInlineConfirm()
	}
	return m, nil
}

// cancelInlineConfirm is updateConfirmKey's shared "actually cancel" path —
// when the cancelled confirm was a 26a commit (the panel stays open under
// its TierInline y/N), the row's buffer must revert too, per the panel's
// hosting contract (metapanel.CancelConfirm's doc comment).
func (m *Model) cancelInlineConfirm() {
	if m.meta != nil {
		m.meta.CancelConfirm()
	}
	m.actions.Cancel()
}

// updateModalConfirmKey drives the 8b type-the-name modal: enter executes
// only once Controller.NameMatches (a no-op otherwise, "↵ stays dead until
// the typed name matches"), backspace/typing edit the buffer,
// verbs.ForceDelete.Key escalates a pending Pod delete to force-delete, esc cancels.
func (m *Model) updateModalConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.actions.Cancel()
	case "enter":
		return m, m.actions.Confirm()
	case verbs.ForceDelete.Key:
		if m.actions.EscalateForceDelete(m.isProd()) {
			return m, nil
		}
		return m, m.actions.HandleTypeKey(msg)
	default:
		return m, m.actions.HandleTypeKey(msg)
	}
	return m, nil
}

// isProd reports whether the active session's current context is tagged
// prod in ~/.config/kute/config.yaml — the same source 7a's context
// palette PROD tag reads (internal/tui/context.go).
func (m Model) isProd() bool {
	if m.session == nil {
		return false
	}
	return m.session.Config.IsProd(m.session.Location.Context)
}

// moveSibling shifts to the next/prev pod in browse's ordered list without
// leaving detail (docs/design README.md §5a: "[/] moves through the table's
// pod list without leaving detail view") — a no-op at either end, and a
// no-op when there's no sibling list at all (e.g. reached from nodedetail's
// single-pod handoff).
func (m *Model) moveSibling(delta int) tea.Cmd {
	if len(m.siblings) == 0 {
		return nil
	}
	next := m.siblingIndex + delta
	if next < 0 || next >= len(m.siblings) {
		return nil
	}
	m.siblingIndex = next
	m.loadEpoch++
	m.namespace = m.siblings[next].Namespace
	m.name = m.siblings[next].Name
	m.gone = false
	m.found = false
	m.pod = kube.Pod{}
	m.status = podStatus{}
	m.usage, m.usageOK = kube.PodMetrics{}, false
	m.eventRows = nil
	m.eventsErr = nil
	m.eventsPending = false
	m.related = nil
	m.sources = nil
	m.configErrors = nil
	m.selectedContainer = 0
	m.bodyOffset = 0
	m.state = tui.TaskStateLoading
	m.feedback = "Loading " + m.name + "..."
	if m.lister == nil {
		m.state = tui.TaskStateError
		m.feedback = "no cluster connection"
		return nil
	}
	return tea.Batch(m.load(), m.spinner.Tick, m.restartMetrics())
}

// totalContainerRows is the combined CONTAINERS + INIT CONTAINERS +
// EPHEMERAL selection space. Init rows are loggable but openSelectedExec
// deliberately continues to use only ContainerInfos.
func (m Model) totalContainerRows() int {
	return len(m.pod.ContainerInfos) + len(m.pod.InitContainerInfos) + len(m.pod.EphemeralContainerInfos)
}

func (m Model) selectedInitContainer() (kube.ContainerInfo, bool) {
	i := m.selectedContainer - len(m.pod.ContainerInfos)
	if i < 0 || i >= len(m.pod.InitContainerInfos) {
		return kube.ContainerInfo{}, false
	}
	return m.pod.InitContainerInfos[i], true
}

// selectedEphemeralContainer resolves m.selectedContainer to an
// EphemeralContainerInfo when the cursor is on the EPHEMERAL group, ok
// false otherwise.
func (m Model) selectedEphemeralContainer() (kube.EphemeralContainerInfo, bool) {
	i := m.selectedContainer - len(m.pod.ContainerInfos) - len(m.pod.InitContainerInfos)
	if i < 0 || i >= len(m.pod.EphemeralContainerInfos) {
		return kube.EphemeralContainerInfo{}, false
	}
	return m.pod.EphemeralContainerInfos[i], true
}

// moveContainerSelection shifts the combined CONTAINERS/INIT/EPHEMERAL
// selection by delta, clamped rather than wrapping — same as browse's own
// moveSelection (j/k ≡ ↑↓ everywhere, CLAUDE.md convention).
func (m *Model) moveContainerSelection(delta int) {
	n := m.totalContainerRows()
	if n == 0 {
		return
	}
	next := m.selectedContainer + delta
	if next < 0 {
		next = 0
	}
	if next >= n {
		next = n - 1
	}
	m.selectedContainer = next
}

func (m *Model) moveContainerHalfPage(direction int) {
	n := m.totalContainerRows()
	position := components.MoveHalfPage(m.selectedContainer, 0, max(n, 1), n, direction)
	m.selectedContainer = position.Selected
}

func (m *Model) moveBodyPage(direction int) {
	height := tui.FrameBodyHeight(m.height, 0)
	contentHeight := len(strings.Split(m.readyContent(m.width), "\n"))
	maxOffset := max(contentHeight-height, 0)
	m.bodyOffset = min(max(m.bodyOffset+direction*max(height-2, 1), 0), maxOffset)
}

func (m Model) bodyOverflows() bool {
	return m.bodyMaxOffset() > 0
}

func (m Model) bodyMaxOffset() int {
	height := tui.FrameBodyHeight(m.height, 0)
	return max(len(strings.Split(m.readyContent(m.width), "\n"))-height, 0)
}

// openSelectedLogs pushes the log-stream screen for the loaded pod — same
// contract as browse.openSelectedLogs (ok is false when logs aren't wired
// or nothing's loaded yet, so 'l' stays a no-op rather than pushing a
// broken screen). Opens on whichever container the CONTAINERS grid has
// selected, not always index 0 — the only screen among openLogs' three
// callers with a per-container selection to hand off.
func (m Model) openSelectedLogs() (tea.Model, tea.Cmd, bool) {
	if m.openLogs == nil || !m.found {
		return nil, nil, false
	}
	task, cmd := m.openLogs(m.pod, m.selectedContainerName(), m.width, m.height)
	return task, cmd, task != nil
}

// selectedContainerName is the name behind the combined CONTAINERS/INIT/
// EPHEMERAL selection, "" when nothing is loaded.
func (m Model) selectedContainerName() string {
	if m.selectedContainer < len(m.pod.ContainerInfos) {
		return m.pod.ContainerInfos[m.selectedContainer].Name
	}
	if init, ok := m.selectedInitContainer(); ok {
		return init.Name
	}
	if eph, ok := m.selectedEphemeralContainer(); ok {
		return eph.Name
	}
	return ""
}

// openSelectedYAML pushes 8a for the loaded pod.
func (m Model) openSelectedYAML() (tea.Model, tea.Cmd, bool) {
	if m.openYAML == nil || !m.found {
		return nil, nil, false
	}
	task, cmd := m.openYAML(kube.KindPod, m.namespace, m.name, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedEvents pushes 9b object-scoped for the loaded pod.
func (m Model) openSelectedEvents() (tea.Model, tea.Cmd, bool) {
	if m.openEvents == nil || !m.found {
		return nil, nil, false
	}
	task, cmd := m.openEvents(kube.KindPod, m.namespace, m.name, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedTimeline pushes 16b object-scoped for the loaded pod
// (docs/design README.md §16b: "object-scoped from detail").
func (m Model) openSelectedTimeline() (tea.Model, tea.Cmd, bool) {
	if m.openTimeline == nil || !m.found {
		return nil, nil, false
	}
	task, cmd := m.openTimeline(kube.KindPod, m.namespace, m.name, m.width, m.height)
	return task, cmd, task != nil
}

// openRelated jumps to the RELATED section's (idx+1)-th entry — the digit
// keys' replacement for the old 'o'/'i' shortcuts. tui.GotoResource fires
// the same navigation the 'g' goto palette's own resource picks do,
// pre-filled: model.go's routeGoto pushes a fresh browse view retargeted at
// the destination and keeps poddetail one esc-back away, rather than
// discarding it. ok is false when idx is out of range, so a digit with
// nothing behind it stays a no-op. The targets themselves are resolved once
// in load() (load.go's resolveRelatedItems), not here — render/key-handling
// code stays free of the synchronous lookups that used to live in this
// file's own resolveOwnerWorkload/resolveIngress.
//
// Secret and ConfigMap entries push their Data view (27b/27a) on top of
// poddetail instead, when wired: the reference is about the object's keys,
// so that's where the jump should land. task is nil for a goto jump, which
// keeps poddetail active until the navigation message arrives.
func (m Model) openRelated(idx int) (tea.Model, tea.Cmd, bool) {
	if idx < 0 || idx >= len(m.related) {
		return nil, nil, false
	}
	item := m.related[idx]
	switch {
	case item.Kind == kube.KindSecret && m.openSecret != nil:
		if task, cmd := m.openSecret(m.namespace, item.Name, m.width, m.height); task != nil {
			return task, cmd, true
		}
	case item.Kind == kube.KindConfigMap && m.openConfig != nil:
		if task, cmd := m.openConfig(m.namespace, item.Name, m.width, m.height); task != nil {
			return task, cmd, true
		}
	case item.Namespace != "" && m.openFlux != nil:
		// A Flux reconciler lands on 31a, whose inventory is the answer
		// to "what else did this apply" — the question the link is for.
		if task, cmd := m.openFlux(item.Kind, item.Namespace, item.Name, m.width, m.height); task != nil {
			return task, cmd, true
		}
	}
	ns := m.namespace
	if item.Namespace != "" {
		ns = item.Namespace
	}
	return nil, tui.GotoResource(m.session, item.Kind, ns, item.Name), true
}

// openSelectedForward resolves 'f' for the loaded pod (docs/design
// README.md §304, §308: "on any object row") by pushing tasks/forwardpicker
// (13a) — mirrors browse.openSelectedForward's contract.
func (m Model) openSelectedForward() (tea.Model, tea.Cmd, bool) {
	if !m.found || m.openForward == nil {
		return nil, nil, false
	}
	target := kube.ForwardTarget{Kind: kube.KindPod, Namespace: m.namespace, Name: m.name}
	task, cmd := m.openForward(target, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedExec resolves 'x' for the loaded pod (docs/design README.md
// §10a): a single container execs immediately via kube.ExecSpec — task is
// nil and cmd is the tea.ExecProcess Cmd, so poddetail stays the active
// task and handles its own execResultMsg — while more than one container
// pushes tasks/execpicker instead. ok is false when nothing's loaded or no
// containers are known, so 'x' stays a no-op.
func (m Model) openSelectedExec() (tea.Model, tea.Cmd, bool) {
	if !m.found || len(m.pod.ContainerInfos) == 0 {
		return nil, nil, false
	}
	if len(m.pod.ContainerInfos) == 1 {
		return nil, execCmd(m.session.CommandTarget(), m.namespace, m.name, m.pod.ContainerInfos[0].Name, m.demo), true
	}
	if m.openExec == nil {
		return nil, nil, false
	}
	task, cmd := m.openExec(m.namespace, m.name, m.pod.ContainerInfos, m.width, m.height)
	return task, cmd, task != nil
}

// execCmd suspends the program and hands the tty to kubectl for container
// (tea.ExecProcess over kube.ExecSpec) — shared shape with browse's own
// execCmd and tasks/execpicker's execSelected, duplicated per the repo's
// package-local-seam convention. demo short-circuits before building a real
// kubectl command (kube.ErrDemoUnavailable's own doc comment).
func execCmd(target kube.CommandTarget, namespace, pod, container string, demo bool) tea.Cmd {
	if demo {
		return func() tea.Msg { return execResultMsg{err: kube.ErrDemoUnavailable} }
	}
	spec := kube.ExecSpec(target, namespace, pod, container, "")
	return tea.ExecProcess(spec, func(err error) tea.Msg {
		return execResultMsg{err: err}
	})
}

// beginDelete confirms deleting the pod — inline y/N in non-prod contexts,
// the full type-the-name modal in PROD (mvp-plan.md §8b, verbs.Resolve).
// Owner rides along for the modal's "will be recreated" line when known.
func (m *Model) beginDelete() tea.Cmd {
	if !m.found {
		return nil
	}
	return m.actions.Begin(actions.Request{Verb: verbs.Delete, Prod: m.isProd()}, tui.TaskAction{
		ID:    "pod-delete-" + m.namespace + "/" + m.name,
		Label: "Delete pod " + m.name + "?",
		Owner: m.pod.Owner,
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindPod),
			ResourceName: m.name,
			Namespace:    m.namespace,
			Verb:         "delete",
			IsMutating:   true,
		},
	})
}
