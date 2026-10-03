package nodedetail

import (
	"errors"
	"fmt"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/metapanel"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// pasteTarget mirrors updateKey's own routing precedence: the open meta
// panel's focused buffer first (its own resolver picks the right one of its
// three), else the '/' pods-filter buffer while it's open. Node detail's
// other confirms (cordon/drain) are y/N cards with no text field, so they
// have nothing to paste into. The filter re-applies inside the closure so
// a pasted query narrows the pods table as a typed one does.
func (m *Model) pasteTarget() tui.PasteTarget {
	if m.meta != nil && !m.actions.Active() {
		return m.meta.PasteTarget()
	}
	if !m.filterActive {
		return nil
	}
	insert := tui.PasteInto(&m.filterInput)
	return func(s string) {
		insert(s)
		m.recomputeFiltered()
	}
}

// Reload implements tui.Reloader — see its doc comment: this screen misses
// every kube.ResourceChangedMsg while parked in the stack, so BackMsg
// restoring it asks it to catch up immediately rather than showing stale
// data until an unrelated change happens to land while it's active again
// (a pod deleted from a pushed poddetail, for instance).
func (m *Model) Reload() tea.Cmd {
	if m.lister == nil {
		return nil
	}
	m.reloadEpoch++
	// Re-arm the usage poll too: while this screen sat in the stack its
	// ticks were delivered to whatever was active instead, so the chain is
	// dead by the time we get here.
	return tea.Batch(m.scheduleReload(m.reloadEpoch), m.armMetricsTick())
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
		// Node backs the facts panel, Pod the bottom-pane table — a change to
		// either changes what this screen shows even though neither is
		// "the kind on screen" the way browse's own auxKindOf reload guards
		// against going stale (CLAUDE.md: "a screen reading a kind it
		// doesn't display must also reload on it").
		if (msg.Kind == kube.KindNode || msg.Kind == kube.KindPod) && m.lister != nil {
			m.reloadEpoch++
			return m, m.scheduleReload(m.reloadEpoch)
		}
	case kube.ConnStateMsg:
		wasOffline := m.conn.Offline()
		m.conn = kube.ConnState(msg)
		m.actions.SetOffline(m.conn.Offline())
		m.now = time.Now()
		if wasOffline && !m.conn.Offline() {
			// pollsMetrics refuses to arm while offline, so the chain died
			// when the connection dropped; coming back up has to restart it.
			return m, m.armMetricsTick()
		}
	case loadedMsg:
		return m.applyLoaded(msg)
	case metricsTickMsg:
		// A tick from a retired chain is dropped rather than re-armed —
		// that is what keeps exactly one poll loop alive.
		if msg.epoch != m.metricsEpoch {
			return m, nil
		}
		m.diskTicks++
		if m.diskDue() {
			m.diskTicks = 0
			return m, tea.Batch(m.loadMetrics(), m.loadDisk())
		}
		return m, m.loadMetrics()
	case metricsLoadedMsg:
		return m.applyMetrics(msg)
	case diskLoadedMsg:
		m.applyDisk(msg)
	case reloadDueMsg:
		if msg.epoch == m.reloadEpoch {
			return m, m.load()
		}
	case spinner.TickMsg:
		if m.state != tui.TaskStateLoading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		m.now = time.Now()
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
	case tea.KeyPressMsg:
		// Any key acknowledges the last action's outcome line; the key
		// still does what it would have.
		m.actions.DismissResult()
		return m.updateKey(msg)
	}
	return m, nil
}

// execResultMsg carries a directly-run (single-container, no picker pushed)
// kubectl exec's exit outcome — same contract as browse's and poddetail's own
// execResultMsg, duplicated per the repo's package-local-seam convention.
type execResultMsg struct{ err error }

// editResultMsg carries a kubectl edit exit outcome (edit.go) — same
// feedback channel as execResultMsg, kept as its own type for the same
// reason.
type editResultMsg struct{ err error }

func (m *Model) applyLoaded(msg loadedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil && !errors.Is(msg.err, errNodeNotFound) {
		m.state = tui.TaskStateError
		if kube.IsPermissionError(msg.err) {
			m.state = tui.TaskStatePermissionDenied
		}
		m.feedback = msg.err.Error()
		return m, nil
	}

	// Node and Pod are nodedetail's two required caches, checked
	// unconditionally — whether findNode's scan came back errNodeNotFound or
	// the pods list is already non-empty — so a cache that's stalled or
	// denied is never masked by data already on screen. A context switch or
	// an RBAC change mid-session can leave stale-but-non-empty data behind
	// after the cache it came from has since gone bad (CLAUDE.md: "never
	// gate a loading state on Synced()").
	if !tui.KindsSynced(m.lister, "", kube.KindNode, kube.KindPod) {
		// The informer cache is still filling (just after launch, mid a
		// context switch, or on a very fast node-open) — msg's node/pods
		// aren't trustworthy yet. Stay loading and retry shortly rather than
		// flashing "node X not found" or "no pods on this node" before
		// they've landed.
		m.reloadEpoch++
		return m, m.scheduleReload(m.reloadEpoch)
	}
	if err := tui.KindsError(m.lister, "", kube.KindNode, kube.KindPod); err != nil {
		// KindSynced reports settled for a Forbidden cache too — that's the
		// anti-hang rule, not a claim the node is missing or has no pods.
		// Without this check a denied cache would fall straight through to
		// either a bare "not found" or Ready with an empty pods table,
		// exactly the false claims KindsError exists to prevent (CLAUDE.md's
		// informer invariants).
		if kube.IsPermissionError(err) {
			m.state = tui.TaskStatePermissionDenied
		} else {
			m.state = tui.TaskStateError
		}
		m.feedback = err.Error()
		return m, nil
	}
	if msg.err != nil {
		// errNodeNotFound, and both caches are synced and clean — this
		// really is a node that no longer exists, not a still-filling or
		// denied cache masquerading as one.
		m.state = tui.TaskStateError
		m.feedback = msg.err.Error()
		return m, nil
	}

	m.node = msg.node
	m.allocated = msg.allocated
	m.allocatable = msg.allocatable
	m.capacity = msg.capacity
	m.allPods = msg.pods
	m.drainImpact = msg.drainImpact
	m.applyPodUsage()
	m.recomputeFiltered()

	firstReady := m.state != tui.TaskStateReady
	m.state = tui.TaskStateReady
	m.feedback = ""
	if firstReady {
		// The first landed load is what tells the poll which namespaces to
		// read, so the chain starts here — reading now, not a tick from now,
		// so USED and the pods' CPU/MEM fill in with the rest of the screen.
		m.metricsEpoch++
		if m.pollsMetrics() {
			return m, m.loadMetrics()
		}
	}
	return m, nil
}

// applyPodUsage overlays the last usage poll on allPods' rows. A pod the
// poll didn't cover (it landed on the node since) keeps whatever it had.
func (m *Model) applyPodUsage() {
	for i := range m.allPods {
		pod := &m.allPods[i].pod
		if pm, found := m.podMetrics[kube.PodKey(pod.Namespace, pod.Name)]; found {
			pod.CPU, pod.MEM = pm.CPU, pm.MEM
			pod.CPUMilli, pod.MEMBytes = pm.CPUMilli, pm.MemBytes
		}
	}
}

// applyMetrics folds one usage poll into the model and arms the next tick.
// It touches only usage — never the pod set, the node, or the selection —
// so a poll landing mid-scroll or mid-filter can't move anything under the
// user.
func (m *Model) applyMetrics(msg metricsLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.epoch != m.metricsEpoch {
		return m, nil
	}
	m.used, m.usedOK = msg.used, msg.usedOK
	m.usagePolled = true
	if msg.podMetrics != nil {
		m.podMetrics = msg.podMetrics
		m.applyPodUsage()
		// pods holds copies of allPods' rows, so the filtered view has to
		// be rebuilt for the refreshed numbers to reach the table.
		m.recomputeFiltered()
	}
	return m, m.scheduleMetricsTick(m.metricsEpoch)
}

// recomputeFiltered reapplies filterQuery to allPods (called after a reload
// or a filter-query edit), trying to keep the same pod selected by name —
// mirrors browse's recomputeVisible/restoreSelection, flattened since this
// list has no grouping.
func (m *Model) recomputeFiltered() {
	name := m.selectedPodName()
	m.pods = applyPodFilter(m.allPods, m.filterInput.Value())
	m.restoreSelection(name)
}

func (m Model) selectedPodName() string {
	row, ok := m.selectedPod()
	if !ok {
		return ""
	}
	return row.pod.Name
}

// restoreSelection re-finds name among m.pods (just refiltered), falling
// back to a clamped index when it's gone (filtered out or deleted).
func (m *Model) restoreSelection(name string) {
	if name != "" {
		for i, row := range m.pods {
			if row.pod.Name == name {
				m.selected = i
				m.clampOffset()
				return
			}
		}
	}
	m.selected = clamp(m.selected, 0, len(m.pods)-1)
	m.clampOffset()
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
	if m.filterActive {
		return m.updateFilterKey(msg)
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
	case "ctrl+u":
		m.moveHalfPage(-1)
	case verbs.Filter.Key:
		if m.state == tui.TaskStateReady {
			m.filterActive = true
			m.filterInput = textfield.New()
			m.filterInput.SetStyles(tui.TextInputStyles(m.Theme()))
			m.filterInput.Prompt = ""
			m.filterInput.Focus()
		}
	case "enter":
		if task, cmd, ok := m.openSelectedPod(); ok {
			return task, cmd
		}
	case verbs.Logs.Key:
		if task, cmd, ok := m.openSelectedLogs(); ok {
			return task, cmd
		}
	case verbs.Exec.Key:
		// Exec is Mutating: refused while offline (docs/design README.md
		// §52), same as this screen's cordon/drain.
		if verbs.Exec.HiddenWhileOffline(m.conn.Offline()) {
			return m, nil
		}
		if task, cmd, ok := m.openSelectedExecOrDebug(); ok {
			if task != nil {
				return task, cmd
			}
			return m, cmd
		}
	case verbs.Forward.Key:
		if task, cmd, ok := m.openSelectedForward(); ok {
			return task, cmd
		}
	case verbs.NodeDebugDetail.Key:
		// The debug panel launches a privileged node-debugger pod, so it's
		// gated like cordon/drain (verbs.NodeDebugDetail.HiddenWhileOffline).
		if verbs.NodeDebugDetail.HiddenWhileOffline(m.conn.Offline()) {
			return m, nil
		}
		if m.node != nil {
			// Same contract as browse's Nodes list: the key is never hidden,
			// so a platform that can't host a node shell says so instead of
			// handing kubectl a command that will be refused
			// (kube.NodeShellUnavailable).
			if reason := kube.NodeShellUnavailable(m.nodeName, m.node.Labels); reason != "" {
				m.execFeedback = reason
				return m, nil
			}
		}
		if task, cmd, ok := m.openSelectedNodeDebug(); ok {
			return task, cmd
		}
	case verbs.Cordon.Key:
		return m, m.beginCordon()
	case verbs.Drain.Key:
		return m, m.beginDrain()
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
	case verbs.Meta.Key:
		// 26a on the loaded node — the same guard this screen's own
		// cordon/drain use (mutator wired, node loaded, not offline);
		// Editable(Node) is always true.
		if m.node != nil && m.mutator != nil && !verbs.Meta.HiddenWhileOffline(m.conn.Offline()) {
			if p, ok := metapanel.Open(metapanel.Config{Session: m.session, Lister: m.lister}, kube.KindNode, "", m.nodeName); ok {
				m.meta = p
			}
		}
	}
	return m, nil
}

func (m *Model) updateConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
// hosting contract (metapanel.CancelConfirm's doc comment).
func (m *Model) cancelInlineConfirm() {
	if m.meta != nil {
		m.meta.CancelConfirm()
	}
	m.actions.Cancel()
}

// updateFilterKey drives the pods list's live "/" filter — same shape as
// browse's updateFilterKey, flattened for this package's ungrouped list:
// esc clears and exits, backspace edits the query, up/down still move the
// selection (plain j/k stay typeable into the query, same as browse).
func (m *Model) updateFilterKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filterActive = false
		m.filterInput.SetValue("")
		m.filterInput.Blur()
		m.recomputeFiltered()
	case "up":
		m.moveSelection(-1)
	case "down":
		m.moveSelection(1)
	case "ctrl+d":
		m.moveHalfPage(1)
	case "ctrl+u":
		m.moveHalfPage(-1)
	default:
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		m.recomputeFiltered()
		return m, cmd
	}
	return m, nil
}

func (m *Model) moveSelection(delta int) {
	if len(m.pods) == 0 {
		m.selected, m.offset = 0, 0
		return
	}
	m.selected = clamp(m.selected+delta, 0, len(m.pods)-1)
	m.clampOffset()
}

func (m *Model) moveHalfPage(direction int) {
	position := components.MoveHalfPage(m.selected, m.offset, m.tableDataRows(), len(m.pods), direction)
	m.selected, m.offset = position.Selected, position.Offset
}

// clampOffset keeps the selected pod row within the table's rendered
// viewport — mirrors browse's own clampOffset/tableDataRows pattern
// (selection.go), against the bottom pane's actual visible row count.
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

// openSelectedPod pushes tasks/poddetail (5a) for the selected pod row.
func (m Model) openSelectedPod() (tea.Model, tea.Cmd, bool) {
	if m.openPod == nil {
		return nil, nil, false
	}
	row, ok := m.selectedPod()
	if !ok {
		return nil, nil, false
	}
	task, cmd := m.openPod(row.pod, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedLogs pushes the log-stream screen for the selected pod row —
// same direct path browse's 'l' gives its Pods list, so tailing a node's
// pod doesn't require opening poddetail first. ok is false when logs aren't
// wired or nothing's selected, so 'l' stays a no-op.
func (m Model) openSelectedLogs() (tea.Model, tea.Cmd, bool) {
	if m.openLogs == nil {
		return nil, nil, false
	}
	row, ok := m.selectedPod()
	if !ok {
		return nil, nil, false
	}
	task, cmd := m.openLogs(row.pod, "", m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedExec resolves 'x' for the selected pod row (docs/design
// README.md §10a), mirroring browse's openSelectedExec: a single container
// execs immediately via kube.ExecSpec — task is nil and cmd is the
// tea.ExecProcess Cmd, so nodedetail stays the active task and handles its
// own execResultMsg — while more than one container pushes tasks/execpicker.
// ok is false when nothing applies (no row selected or no containers known),
// so 'x' stays a no-op rather than the caller misreading a nil task as
// failure.
func (m Model) openSelectedExec() (tea.Model, tea.Cmd, bool) {
	row, ok := m.selectedPod()
	if !ok || len(row.pod.ContainerInfos) == 0 {
		return nil, nil, false
	}
	if len(row.pod.ContainerInfos) == 1 {
		return nil, execCmd(m.session.CommandTarget(), row.pod.Namespace, row.pod.Name, row.pod.ContainerInfos[0].Name), true
	}
	if m.openExec == nil {
		return nil, nil, false
	}
	task, cmd := m.openExec(row.pod.Namespace, row.pod.Name, row.pod.ContainerInfos, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedForward resolves 'f' for the selected pod row (docs/design
// README.md §304, §308: "on any object row") by pushing tasks/forwardpicker
// (13a) — mirrors browse.openSelectedForward's contract.
func (m Model) openSelectedForward() (tea.Model, tea.Cmd, bool) {
	row, ok := m.selectedPod()
	if !ok || m.openForward == nil {
		return nil, nil, false
	}
	target := kube.ForwardTarget{Kind: kube.KindPod, Namespace: row.pod.Namespace, Name: row.pod.Name}
	task, cmd := m.openForward(target, m.width, m.height)
	return task, cmd, task != nil
}

// execCmd suspends the program and hands the tty to kubectl for container
// (tea.ExecProcess over kube.ExecSpec) — shared shape with browse's and
// tasks/execpicker's own execCmd/execSelected, duplicated per the repo's
// package-local-seam convention.
func execCmd(target kube.CommandTarget, namespace, pod, container string) tea.Cmd {
	spec := kube.ExecSpec(target, namespace, pod, container, "")
	return tea.ExecProcess(spec, func(err error) tea.Msg {
		return execResultMsg{err: err}
	})
}

// openSelectedNodeDebug pushes tasks/debugpanel (§41d) for the loaded node —
// 's' here (verbs.NodeDebugDetail), not NodeDebug's 'x' (see that verb's own
// doc comment for why). ok is false until the node has loaded or the
// callback isn't wired.
func (m Model) openSelectedNodeDebug() (tea.Model, tea.Cmd, bool) {
	if m.openNodeDebug == nil || m.node == nil {
		return nil, nil, false
	}
	task, cmd := m.openNodeDebug(m.nodeName, len(m.allPods), m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedYAML pushes 8a for the node itself.
func (m Model) openSelectedYAML() (tea.Model, tea.Cmd, bool) {
	if m.openYAML == nil || m.node == nil {
		return nil, nil, false
	}
	task, cmd := m.openYAML(kube.KindNode, "", m.nodeName, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedEvents pushes 9b object-scoped for the node itself
// (docs/design README.md §11b: "e node events").
func (m Model) openSelectedEvents() (tea.Model, tea.Cmd, bool) {
	if m.openEvents == nil || m.node == nil {
		return nil, nil, false
	}
	task, cmd := m.openEvents(kube.KindNode, "", m.nodeName, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedTimeline pushes 16b object-scoped for the node itself
// (docs/design README.md §16b) — same shape as openSelectedEvents.
func (m Model) openSelectedTimeline() (tea.Model, tea.Cmd, bool) {
	if m.openTimeline == nil || m.node == nil {
		return nil, nil, false
	}
	task, cmd := m.openTimeline(kube.KindNode, "", m.nodeName, m.width, m.height)
	return task, cmd, task != nil
}

// beginCordon toggles the node's schedulable state — TierNone (verbs.Cordon),
// so it executes immediately with no confirmation, mirroring browse's 11a.
// Begin resolves the tier from verbs.Cordon's policy row, so a future change
// to it can't be skipped here.
func (m *Model) beginCordon() tea.Cmd {
	if m.node == nil {
		return nil
	}
	verb, label := "cordon", fmt.Sprintf("Cordon %s?", m.nodeName)
	if m.node.Spec.Unschedulable {
		verb, label = "uncordon", fmt.Sprintf("Uncordon %s?", m.nodeName)
	}
	return m.actions.Begin(actions.Request{Verb: verbs.Cordon, Prod: m.isProd()}, tui.TaskAction{
		ID:    "node-" + verb + "-" + m.nodeName,
		Label: label,
		Scope: tui.TaskScope{ResourceKind: string(kube.KindNode), ResourceName: m.nodeName, Verb: verb, IsMutating: true},
	})
}

// beginDrain confirms draining the node — TierModal (verbs.Drain). The title
// is just the question; confirmBody states the consequences — how many of its
// pods (the cache backing the bottom pane) will be evicted, and which won't
// come back — on its own line. Counts allPods, not the (possibly filtered) pods list, since drain evicts
// regardless of what the filter currently hides — less the DaemonSet and
// static pods it leaves in place.
func (m *Model) beginDrain() tea.Cmd {
	if m.node == nil {
		return nil
	}
	return m.actions.Begin(actions.Request{Verb: verbs.Drain, Prod: m.isProd()}, tui.TaskAction{
		ID:    "node-drain-" + m.nodeName,
		Label: fmt.Sprintf("Drain %s?", m.nodeName),
		Scope: tui.TaskScope{ResourceKind: string(kube.KindNode), ResourceName: m.nodeName, Verb: "drain", IsMutating: true},
	})
}
