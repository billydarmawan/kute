package fluxtree

import (
	"fmt"
	"slices"
	"strings"
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
	if m.lister == nil {
		return nil
	}
	return m.load()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case spinner.TickMsg:
		if m.state == tui.TaskStateLoading {
			sp, cmd := m.spinner.Update(msg)
			m.spinner = sp
			m.now = time.Now()
			return m, cmd
		}
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
		m.actionsCtl.SetOffline(m.conn.Offline())
		m.now = time.Now()
	case kube.ResourceChangedMsg:
		// Only the kinds this screen draws — a Pod event must not re-run the
		// whole join.
		if m.lister != nil && m.isFluxKind(msg.Kind) {
			return m, m.load()
		}
	case tui.CacheSyncRetryMsg:
		if msg.Gen == m.reloadEpoch && m.lister != nil {
			return m, m.load()
		}
	case loadedMsg:
		return m, m.applyLoaded(msg)
	case actions.ResultMsg:
		return m, m.handleResult(msg)
	case tea.KeyPressMsg:
		// Any key acknowledges the last action's outcome line; the key
		// still does what it would have.
		m.actionsCtl.DismissResult()
		return m.handleKey(msg)
	}
	return m, nil
}

// isFluxKind reports whether kind is one this screen reads, so an unrelated
// informer event doesn't re-run the join.
func (m Model) isFluxKind(kind kube.ResourceKind) bool {
	desc, ok := m.registry().Descriptor(kind)
	return ok && desc.Flux
}

func (m *Model) applyLoaded(msg loadedMsg) tea.Cmd {
	m.reloadEpoch++
	if !tui.KindsSynced(m.lister, "", msg.kinds...) {
		// A non-empty join can still be partial while one of the Flux caches is
		// filling. Do not publish it as ready: the tree would silently miss
		// sources or reconcilers until the next watch event.
		m.state = tui.TaskStateLoading
		return tui.ScheduleCacheSyncRetry(m.reloadEpoch)
	}
	m.kindErrs = msg.kindErrs
	if msg.err != nil {
		// Nothing could be read at all. A failing kind is degraded per kind
		// in load, so this is every Flux kind failing at once.
		m.state = tui.TaskStateError
		m.feedback = fmt.Sprintf("couldn't read Flux objects: %v", msg.err)
		if m.stalled() {
			m.feedback += " — retrying"
		}
		return m.scheduleStallRetry()
	}
	m.groups = msg.groups
	m.recomputeVisible()
	m.state = tui.TaskStateReady
	m.feedback = ""

	if len(m.groups) == 0 {
		// "This cluster runs no Flux objects" is a claim about the cluster,
		// and the caches these kinds live in start filling only when this
		// screen first reads them. Never make it against a cache that hasn't
		// settled (CLAUDE.md's empty-state rule).
		if !tui.KindsSynced(m.lister, "", msg.kinds...) {
			m.state = tui.TaskStateLoading
			return tui.ScheduleCacheSyncRetry(m.reloadEpoch)
		}
		m.state = tui.TaskStateEmpty
	}
	return m.scheduleStallRetry()
}

// stalled reports whether any unreadable kind is failing for a reason that
// may yet clear (an initial LIST being retried) rather than a denial, which
// won't change while the process runs.
func (m Model) stalled() bool {
	for _, e := range m.kindErrs {
		if !kube.IsPermissionError(e.err) {
			return true
		}
	}
	return false
}

// stallRetryInterval is how often a tree with a stalled kind re-reads its
// caches. A stalled kind that recovers with no objects emits no change
// event, so without this the strip would name it unreadable for good —
// yet the read is only ever a cache read, so there is no reason to do it at
// CacheSyncRetryInterval's 250 ms for as long as a stall lasts.
const stallRetryInterval = 5 * time.Second

// scheduleStallRetry backs the "retrying" a stalled kind's note promises.
// Nothing is scheduled for a denial alone.
func (m Model) scheduleStallRetry() tea.Cmd {
	if !m.stalled() {
		return nil
	}
	gen := m.reloadEpoch
	return tea.Tick(stallRetryInterval, func(time.Time) tea.Msg { return tui.CacheSyncRetryMsg{Gen: gen} })
}

// handleResult lands a verb's outcome on the keybar result line and reloads, so
// the row reflects what just happened rather than what it said before.
func (m *Model) handleResult(msg actions.ResultMsg) tea.Cmd {
	m.actionsCtl.HandleResult(msg)
	// The outcome is the controller's keybar result line (ActionResult),
	// red on failure; the will-run command it answered is cleared.
	m.execFeedback = ""
	if msg.Err != nil {
		return nil
	}
	return m.load()
}

func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.moveSelection(-1)
		return m, nil
	case "down", "j":
		m.moveSelection(1)
		return m, nil
	case "home":
		m.selectFirst()
		return m, nil
	case "end":
		m.selectLast()
		return m, nil
	case verbs.ToggleGroup.Key:
		if m.foldedSources > 0 || m.expanded {
			m.expanded = !m.expanded
			m.recomputeVisible()
		}
		return m, nil
	case "enter":
		if task, cmd, ok := m.openSelectedDetail(); ok {
			if task != nil {
				return task, cmd
			}
			return m, cmd
		}
		return m, nil
	case verbs.FluxSource.Key:
		if row, ok := m.selectedRow(); ok {
			m.selectSourceOf(row)
		}
		return m, nil
	case verbs.FluxReconcile.Key:
		if !m.verbsApply() {
			return m, nil
		}
		if row, ok := m.selectedRow(); ok && !row.missing {
			return m, m.beginReconcile(row)
		}
		return m, nil
	case verbs.FluxSuspend.Key:
		if !m.verbsApply() {
			return m, nil
		}
		if row, ok := m.selectedRow(); ok && !row.missing {
			return m, m.beginSuspend(row)
		}
		return m, nil
	case "esc":
		return m, func() tea.Msg { return tui.BackMsg{} }
	}
	return m, nil
}

// verbsApply gates the mutating verbs on a wired mutator and a settled
// screen — §30a's own fluxVerbsApply, applied to this screen's state.
func (m Model) verbsApply() bool {
	return m.mutator != nil && m.state == tui.TaskStateReady && !m.conn.Offline()
}

// beginReconcile is §30b's 'r', whose scope follows the cursor: on a source
// row it reconciles the source; on a reconciler row it reconciles
// *with-source*, the equivalent of `flux reconcile --with-source`, because a
// reconciler asked to sync against a stale artifact would re-apply exactly
// what it already has. The will-run line shows the difference — two
// annotate commands rather than one.
func (m *Model) beginReconcile(row treeRow) tea.Cmd {
	now := time.Now()
	scope := tui.TaskScope{
		ResourceKind: string(row.kind), ResourceName: row.name,
		Namespace: row.namespace, Verb: "flux-reconcile", IsMutating: true,
	}
	cmds := []string{kube.FluxReconcileCommandString(row.kind, row.namespace, row.name, now)}
	label := fmt.Sprintf("Reconcile %s?", row.name)
	if !row.isSource && row.sourceName != "" {
		scope.FluxSourceKind = string(row.sourceKind)
		scope.FluxSourceName = row.sourceName
		scope.FluxSourceNamespace = row.sourceNamespace
		// Source first: the reconciler's own sync is only worth anything
		// once the artifact behind it is current.
		cmds = []string{
			kube.FluxReconcileCommandString(row.sourceKind, row.sourceNamespace, row.sourceName, now),
			cmds[0],
		}
		label = fmt.Sprintf("Reconcile %s with source?", row.name)
	}
	m.execFeedback = strings.Join(cmds, " && ")
	return m.actionsCtl.Begin(actions.Request{Verb: verbs.FluxReconcile, Prod: m.isProd()}, tui.TaskAction{
		ID:    "flux-reconcile-" + row.name,
		Label: label,
		Scope: scope,
	})
}

// beginSuspend toggles spec.suspend — one verb, two directions, exactly
// §30a's own shape.
func (m *Model) beginSuspend(row treeRow) tea.Cmd {
	verb, label := "flux-suspend", fmt.Sprintf("Suspend %s?", row.name)
	if row.suspended {
		verb, label = "flux-resume", fmt.Sprintf("Resume %s?", row.name)
	}
	m.execFeedback = kube.FluxSuspendCommandString(row.kind, row.namespace, row.name, !row.suspended)
	return m.actionsCtl.Begin(actions.Request{Verb: verbs.FluxSuspend, Prod: m.isProd()}, tui.TaskAction{
		ID:    verb + "-" + row.name,
		Label: label,
		Scope: tui.TaskScope{
			ResourceKind: string(row.kind), ResourceName: row.name,
			Namespace: row.namespace, Verb: verb, IsMutating: true,
		},
	})
}

// openSelectedDetail is §30b's ↵: §31a's inventory for a reconciler, §14d's
// generic detail for a source (which publishes an artifact, not a set of
// applied objects, so the inventory screen has no question to answer about
// one).
//
// **Both push a task directly; neither goes through tui.GotoResource.**
// tasks/events and tasks/timeline's own ↵ fire tui.GotoResource, which lands
// on a *browse list* with the row selected (one esc back to whichever of
// them fired it, since model.go's routeGoto pushes a fresh browse view
// rather than navigating this screen away) — right for a feed you were
// reading *about* an object. Here it's wrong: the tree is a place you work
// from, and every ↵ out of it should land straight on the object's own
// detail view, not a browse-list hop first — so this pushes the detail task
// itself instead of asking browse to select a row.
func (m Model) openSelectedDetail() (tea.Model, tea.Cmd, bool) {
	row, ok := m.selectedRow()
	if !ok || row.missing {
		return nil, nil, false
	}
	if row.isSource {
		if m.openObjectDetail == nil {
			return nil, nil, false
		}
		task, cmd := m.openObjectDetail(row.kind, row.namespace, row.name, m.width, m.height)
		return task, cmd, task != nil
	}
	if m.openFluxDetail == nil {
		return nil, nil, false
	}
	task, cmd := m.openFluxDetail(row.kind, row.namespace, row.name, m.width, m.height)
	return task, cmd, task != nil
}

// selectSourceOf is 'o' — "go to the source this reconciles from". On this
// screen that source is on screen already, as the row's own parent, so 'o'
// moves the cursor to it rather than navigating anywhere. §30a's version of
// the same verb has to leave the list (the source is a different kind, and
// so a different list); here leaving would be the strictly worse answer,
// and would take the tree off the stack with it.
func (m *Model) selectSourceOf(row treeRow) bool {
	if row.isSource || row.sourceName == "" {
		return false
	}
	for i, l := range m.lines {
		if l.kind != lineRow {
			continue
		}
		head := m.rows[l.row]
		if head.isSource && head.name == row.sourceName && head.namespace == row.sourceNamespace {
			m.selected = i
			m.clampOffset()
			return true
		}
	}
	return false
}

// recomputeVisible flattens the groups into rendered lines, folding the
// chains where nothing is wrong.
func (m *Model) recomputeVisible() {
	prev, hadPrev := m.selectedRow()
	m.rows = m.rows[:0]
	m.lines = m.lines[:0]
	m.foldedSources, m.foldedReconcilers = 0, 0

	for _, g := range m.groups {
		if g.healthy() && !m.expanded {
			m.foldedSources++
			m.foldedReconcilers += len(g.children)
			continue
		}
		m.appendRow(g.head, false)
		for _, c := range g.children {
			m.appendRow(c, true)
		}
	}
	if m.foldedSources > 0 {
		m.lines = append(m.lines, line{kind: lineFold, row: -1, text: m.foldText()})
	}

	// Keep the cursor on the same object across a reload where possible;
	// otherwise land it on the first selectable line rather than a sub-line.
	m.selected = 0
	if hadPrev {
		for i, l := range m.lines {
			if l.kind != lineRow {
				continue
			}
			r := m.rows[l.row]
			if r.kind == prev.kind && r.namespace == prev.namespace && r.name == prev.name {
				m.selected = i
				break
			}
		}
	}
	if !m.selectable(m.selected) {
		m.selectFirst()
	}
	m.clampOffset()
}

// appendRow records one object as a selectable line, plus its condition
// message as a non-selectable continuation under it — §30a's sub-line
// idiom, unchanged.
func (m *Model) appendRow(row treeRow, child bool) {
	if !row.missing {
		m.rows = append(m.rows, row)
		m.lines = append(m.lines, line{kind: lineRow, row: len(m.rows) - 1, child: child})
	} else {
		// A missing source has no object behind it, so it renders as a
		// heading rather than a row: nothing can be done to it.
		m.lines = append(m.lines, line{kind: lineSub, row: -1, text: row.name + " · " + row.kindLabel, child: child})
	}
	if row.subLine != "" {
		m.lines = append(m.lines, line{kind: lineSub, row: -1, text: row.subLine, child: true})
	}
	if row.applies != "" {
		m.lines = append(m.lines, line{kind: lineSub, row: -1, text: row.applies, child: true})
	}
}

func (m Model) foldText() string {
	return fmt.Sprintf("+ %s · %s ready · %s expand",
		plural(m.foldedSources, "source"), plural(m.foldedReconcilers, "reconciler"), tui.GlyphTab)
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func (m Model) selectable(i int) bool {
	return i >= 0 && i < len(m.lines) && m.lines[i].kind == lineRow
}

func (m *Model) moveSelection(delta int) {
	if len(m.lines) == 0 {
		return
	}
	i := m.selected
	for {
		i += delta
		if i < 0 || i >= len(m.lines) {
			return // no further selectable line in that direction; stay put
		}
		if m.selectable(i) {
			m.selected = i
			m.clampOffset()
			return
		}
	}
}

func (m *Model) selectFirst() {
	for i := range m.lines {
		if m.selectable(i) {
			m.selected = i
			m.clampOffset()
			return
		}
	}
	m.selected = 0
}

func (m *Model) selectLast() {
	for i := range slices.Backward(m.lines) {
		if m.selectable(i) {
			m.selected = i
			m.clampOffset()
			return
		}
	}
}

// clampOffset keeps the selected line inside the rendered viewport.
func (m *Model) clampOffset() {
	visible := m.bodyRowCount()
	if visible <= 0 || len(m.lines) <= visible {
		m.offset = 0
		return
	}
	if m.selected < m.offset {
		m.offset = m.selected
	}
	if m.selected >= m.offset+visible {
		m.offset = m.selected - visible + 1
	}
	m.offset = min(max(m.offset, 0), max(len(m.lines)-visible, 0))
}
