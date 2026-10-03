package timeline

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// pasteTarget is the '/' filter buffer while it's open, or 16b's
// type-the-name confirm buffer while a PROD-tier confirmation is up (the same
// gate updateConfirmKey routes typed keys through). The filter re-applies
// inside the closure so a pasted query narrows the timeline as a typed one
// does.
func (m *Model) pasteTarget() tui.PasteTarget {
	if m.actionsCtl.Tier() == actions.TierModal {
		return m.actionsCtl.PasteTarget()
	}
	if !m.filterActive {
		return nil
	}
	insert := tui.PasteInto(&m.filterInput)
	return func(s string) {
		insert(s)
		m.recomputeVisible()
	}
}

// Reload implements tui.Reloader — see its doc comment: this screen misses
// every kube.ResourceChangedMsg while parked in the stack, so BackMsg
// restoring it asks it to catch up immediately rather than showing stale
// data until an unrelated change happens to land while it's active again.
func (m *Model) Reload() tea.Cmd {
	if m.events == nil {
		return nil
	}
	return m.reload()
}

// reload supersedes every load still in flight, then dispatches a new one.
func (m *Model) reload() tea.Cmd {
	m.loadEpoch++
	return m.load()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cmd, ok := tui.RoutePaste(msg, m.pasteTarget()); ok {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ResourceChangedMsg:
		if m.isTimelineSource(msg.Kind) && m.events != nil {
			return m, m.reload()
		}
	case tui.CacheSyncRetryMsg:
		if msg.Gen == m.syncRetryGen && m.lister != nil {
			return m, m.reload()
		}
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
		m.actionsCtl.SetOffline(m.conn.Offline())
	case loadedMsg:
		return m.applyLoaded(msg)
	case spinner.TickMsg:
		if m.state != tui.TaskStateLoading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case actions.RunningMsg:
		// A slow write's keybar line; the returned command is the only
		// path its ResultMsg arrives by.
		return m, m.actionsCtl.HandleRunning(msg)
	case actions.ResultMsg:
		m.actionsCtl.HandleResult(msg)
		if msg.Err == nil {
			return m, m.reload()
		}
		// A failed rollback is the controller's keybar result line
		// (ActionResult).
	case tea.KeyPressMsg:
		// Any key acknowledges the last action's outcome line; the key
		// still does what it would have.
		m.actionsCtl.DismissResult()
		return m.updateKey(msg)
	}
	return m, nil
}

// isTimelineSource reports whether a watch event could change the merged
// feed — Events directly, Pods (restarts) and ReplicaSets (rollout
// revisions) indirectly, Deployments (rail[0]'s live status, change-cause)
// and the Argo Applications and Flux sources load() reads for sync
// attribution and revision pairing. Those last two only ever fire once
// load() itself has started their informers.
func (m *Model) isTimelineSource(kind kube.ResourceKind) bool {
	switch kind {
	case kube.KindEvent, kube.KindPod, kube.KindReplicaSet, kube.KindDeployment:
		return true
	}
	if m.session == nil {
		return false
	}
	d, ok := m.session.Registry.Descriptor(kind)
	return ok && (d.Argo || d.Flux)
}

func (m *Model) applyLoaded(msg loadedMsg) (tea.Model, tea.Cmd) {
	if msg.epoch != m.loadEpoch {
		return m, nil // superseded by a newer load
	}
	// firstLoad is read before m.state is overwritten below — only the very
	// first loadedMsg (Init's own load(), while m.state is still Loading)
	// should default focus onto the rail; a later watch-triggered refresh
	// must never yank focus away from wherever the user already moved it.
	firstLoad := m.state == tui.TaskStateLoading
	// alreadyRendered mirrors whocan's own stall-vs-rerender rule: once
	// something is already on screen, a background reload finding a
	// required cache merely *stalled* must not discard it into a loading
	// spinner — only a confirmed denial/error (checked below, unconditionally)
	// may change what's on screen once something has already rendered.
	alreadyRendered := m.state == tui.TaskStateReady
	if msg.err != nil {
		m.state = tui.TaskStateError
		if kube.IsPermissionError(msg.err) {
			m.state = tui.TaskStatePermissionDenied
		}
		m.feedback = msg.err.Error()
		return m, nil
	}

	// Event, ReplicaSet, Deployment, and Pod are all structurally required
	// to the merged feed's honesty — a missing rollout or missing restart is
	// exactly the dangerous silent-gap case
	// docs/lazy-informers.md §5.6 calls out. Unlike
	// browse/overview/cronjobdetail (each has a clear primary identity plus
	// separate best-effort reads), timeline has no such split: every kind it
	// merges is part of the one answer it presents, so a denial on any of
	// them always promotes to the full error/permission-denied state — even
	// when the others already produced a non-empty feed, which is why these
	// checks run unconditionally rather than only when the feed was empty.
	if !alreadyRendered && !tui.KindsSynced(m.lister, m.namespace, kube.KindEvent, kube.KindReplicaSet, kube.KindDeployment, kube.KindPod) {
		// Events, ReplicaSets, Deployments, and Pods all start on first
		// read, so a cold open would otherwise report a quiet timeline for
		// an object mid-incident.
		m.state = tui.TaskStateLoading
		m.feedback = ""
		m.syncRetryGen++
		return m, tui.ScheduleCacheSyncRetry(m.syncRetryGen)
	}
	if err := tui.KindsError(m.lister, m.namespace, kube.KindEvent, kube.KindReplicaSet, kube.KindDeployment, kube.KindPod); err != nil {
		// One of those caches is settled only by having given up (see
		// tui.KindsError) — "a quiet timeline" is the same wrong answer here
		// as an empty event feed, for the same reason. A denial always wins,
		// even over entries already on screen.
		m.state = tui.TaskStateError
		if kube.IsPermissionError(err) {
			m.state = tui.TaskStatePermissionDenied
		}
		m.feedback = fmt.Sprintf("couldn't load the timeline: %v", err)
		return m, nil
	}

	m.entries = msg.entries
	m.rail = msg.rail
	m.railDeployment = msg.railDeployment
	m.fetchedAt = time.Now()
	m.recomputeVisible()
	m.state = tui.TaskStateReady
	if len(m.rows) == 0 && len(m.rail) == 0 {
		// A 16b revision rail is still worth showing even when nothing
		// happened in the feed's own window — it's not "empty" until there's
		// truly nothing on screen. The checks above have already confirmed
		// every merged cache is synced and error-free, so this really is a
		// quiet window/object, not an unfilled cache.
		m.state = tui.TaskStateEmpty
	}
	if firstLoad && len(m.rail) > 0 {
		// 16b opens with focus already on the rail — it's the reason you
		// pressed 't' on a workload, and the rail cursor's own default
		// (index 0, the current revision) needs a live sync too so the feed
		// isn't left pointing at whatever recomputeVisible defaulted it to.
		m.railFocused = true
		m.syncFeedToRailSelection()
	}
	m.feedback = ""
	return m, nil
}

// recomputeVisible rebuilds m.rows from m.entries: window, then 16a's
// warningsOnly hard-filter, then filter-query applied — the one place the
// feed is windowed/filtered, so the summary strip's counts (view.go) and
// Body's row walk can never disagree about what's currently shown (mirrors
// tasks/events' own recomputeVisible). The survivors are then partitioned
// into "significant" (rollouts/restarts/warning-severity events — always
// shown) and "normal" (Normal-severity events) exactly like tasks/events'
// own warnings/normal split: 16a folds normal entries into one footer line
// unless normalExpanded; 16b (objectScoped) never folds — its feed is
// small and the mockup never shows a folded row there.
func (m *Model) recomputeVisible() {
	cutoff := time.Time{}
	if m.window > 0 {
		cutoff = m.fetchedAt.Add(-m.window)
	}

	var significant, normal []kube.TimelineEntry
	baseline := 0
	for _, e := range m.entries {
		if !cutoff.IsZero() && e.Time.Before(cutoff) {
			continue
		}
		if isNormalEvent(e) && m.warningsOnly {
			continue
		}
		baseline++
		if m.filterInput.Value() != "" && !matchesQuery(e, m.filterInput.Value()) {
			continue
		}
		if isNormalEvent(e) {
			normal = append(normal, e)
		} else {
			significant = append(significant, e)
		}
	}
	m.filterBaselineRows = baseline
	m.normalPresent = len(normal) > 0

	if m.objectScoped() {
		// 16b never folds — merge back and keep newest-first.
		m.rows = mergeTimelineSorted(significant, normal)
		m.foldedNormal = nil
	} else if m.normalExpanded {
		m.rows = mergeTimelineSorted(significant, normal)
		m.foldedNormal = nil
	} else {
		m.rows = significant
		m.foldedNormal = normal
	}
	if m.selected >= len(m.rows) {
		m.selected = max(len(m.rows)-1, 0)
	}
}

// isNormalEvent reports whether e is a Normal-severity Event — the only
// entry kind 16a's warningsOnly/fold-into-one-line treatment ever hides;
// restarts and rollouts always count as "significant" regardless of
// severity (they're never mere chatter).
func isNormalEvent(e kube.TimelineEntry) bool {
	return e.Kind == kube.TimelineEvent && e.Severity != "Warning"
}

// mergeTimelineSorted concatenates a and b (each already newest-first) and
// re-sorts newest-first — used when 16a's fold is expanded, or in 16b where
// significant/normal are never split apart for display.
func mergeTimelineSorted(a, b []kube.TimelineEntry) []kube.TimelineEntry {
	if len(b) == 0 {
		return a
	}
	return kube.MergeTimeline(a, b)
}

func matchesQuery(e kube.TimelineEntry, query string) bool {
	q := strings.ToLower(query)
	return strings.Contains(strings.ToLower(e.Reason), q) ||
		strings.Contains(strings.ToLower(e.Object), q) ||
		strings.Contains(strings.ToLower(e.Message), q)
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.actionsCtl.Active() {
		return m.updateConfirmKey(msg)
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
		if m.railFocused {
			m.moveRailSelection(-1)
		} else {
			m.moveSelection(-1)
		}
	case "down", "j":
		if m.railFocused {
			m.moveRailSelection(1)
		} else {
			m.moveSelection(1)
		}
	case "ctrl+d":
		if m.railFocused {
			m.moveRailSelection(1)
		} else {
			m.moveSelection(1)
		}
	case "ctrl+u":
		if m.railFocused {
			m.moveRailSelection(-1)
		} else {
			m.moveSelection(-1)
		}
	case "tab":
		if len(m.rail) > 0 {
			m.railFocused = !m.railFocused
		} else if !m.objectScoped() {
			m.normalExpanded = !m.normalExpanded
			m.recomputeVisible()
		}
	case "enter":
		if cmd, ok := m.openSelectedObject(); ok {
			return m, cmd
		}
	case verbs.Events.Key:
		if task, cmd, ok := m.openSelectedEvents(); ok {
			return task, cmd
		}
	case "t":
		m.cycleWindow()
	case "w":
		if !m.objectScoped() {
			m.warningsOnly = !m.warningsOnly
			m.recomputeVisible()
		}
	case verbs.Filter.Key:
		if !m.objectScoped() && (m.state == tui.TaskStateReady || m.state == tui.TaskStateEmpty) {
			m.filterActive = true
			m.filterInput = textfield.New()
			m.filterInput.SetStyles(tui.TextInputStyles(m.Theme()))
			m.filterInput.Prompt = ""
			m.filterInput.Focus()
		}
	case "v":
		// §32a/§34a: copy the revision — the thing you paste into `git show`
		// or an incident channel. Only meaningful on a revision or sync row.
		if e, ok := m.selectedRow(); ok && (e.Kind == kube.TimelineRevision || e.Kind == kube.TimelineSync) && e.GitRevision != "" {
			return m, tea.SetClipboard(e.GitRevision)
		}
	case verbs.RolloutUndo.Key:
		if m.railFocused && m.mutator != nil {
			if rev, ok := m.selectedRevision(); ok {
				if m.railSelected == 0 {
					// rail[0] is the live revision: rolling back to it
					// patches the template to what it already is.
					m.actionsCtl.Refuse(fmt.Sprintf("revision %d is the current revision · pick an earlier one to roll back to", rev.Revision))
					return m, nil
				}
				return m, m.beginRollback(rev)
			}
		}
	}
	return m, nil
}

// updateConfirmKey routes keys while a confirmation is showing, through
// the controller's one router (actions.HandleConfirmKey) — 'R'
// rollback's y/N, or its PROD type-the-name modal.
func (m *Model) updateConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	cmd, _ := m.actionsCtl.HandleConfirmKey(msg)
	return m, cmd
}

// beginRollback confirms rolling deploy/m.railDeployment back to rev — the
// same tier/friction helmhistory's own beginRollback uses for Helm releases
// (docs/design README.md §16b: "inherits the same two-tier friction"),
// duplicated here per the repo's package-local-seam convention.
func (m *Model) beginRollback(rev kube.TimelineEntry) tea.Cmd {
	return m.actionsCtl.Begin(actions.Request{Verb: verbs.RolloutUndo, Prod: m.isProd()}, tui.TaskAction{
		ID:    fmt.Sprintf("rollout-undo-%s/%s-r%d", m.namespace, m.railDeployment, rev.Revision),
		Label: fmt.Sprintf("Roll %s back to revision %d?", m.railDeployment, rev.Revision),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindDeployment),
			ResourceName: m.railDeployment,
			Namespace:    m.namespace,
			Verb:         "rollout-undo",
			IsMutating:   true,
			Revision:     rev.Revision,
		},
	})
}

func (m *Model) moveRailSelection(delta int) {
	if len(m.rail) == 0 {
		m.railSelected = 0
		return
	}
	m.railSelected = clamp(m.railSelected+delta, 0, len(m.rail)-1)
	m.syncFeedToRailSelection()
}

// syncFeedToRailSelection moves the feed cursor live, as the rail cursor
// moves — no '↵' needed — onto the most recent entry from the selected
// revision's own lifetime (its own rollout up to whenever it was superseded,
// or "now" for the current revision): usually a restart or event that
// happened under that revision, falling back to the revision's own ROLLOUT
// row when nothing else did. Widens the window to "all time" first when the
// target isn't in the currently windowed feed (an older revision's history
// almost always falls outside the default 30m/1h/6h/24h windows) rather than
// silently no-opping. Reports whether the sync landed, purely for tests.
func (m *Model) syncFeedToRailSelection() bool {
	target, ok := m.railSelectionTarget()
	if !ok {
		return false
	}
	if idx := m.indexOfEntry(target); idx >= 0 {
		m.selected = idx
		return true
	}
	m.window = 0
	m.recomputeVisible()
	idx := m.indexOfEntry(target)
	if idx < 0 {
		return false
	}
	m.selected = idx
	return true
}

// railSelectionTarget resolves the entry syncFeedToRailSelection should
// point the feed cursor at for the currently rail-selected revision — the
// newest entry (by time, scanning the unwindowed m.entries) inside that
// revision's own lifetime window, or the revision's own ROLLOUT entry itself
// when nothing else happened during it.
func (m Model) railSelectionTarget() (kube.TimelineEntry, bool) {
	rev, ok := m.selectedRevision()
	if !ok {
		return kube.TimelineEntry{}, false
	}
	// The window ends at the next revision *by time*, not the next by
	// number: a rollback-reused ReplicaSet keeps its original creation time,
	// so revision order and time order disagree after any rollback, and a
	// window taken from the neighbouring rail entry would run backwards.
	start := m.rail[m.railSelected].Time
	end := m.fetchedAt
	for i, r := range m.rail {
		if i != m.railSelected && r.Time.After(start) && r.Time.Before(end) {
			end = r.Time
		}
	}
	var latest *kube.TimelineEntry
	for i := range m.entries {
		e := &m.entries[i]
		if e.Time.Before(start) || !e.Time.Before(end) {
			continue
		}
		if latest == nil || e.Time.After(latest.Time) {
			latest = e
		}
	}
	if latest != nil {
		return *latest, true
	}
	return rev, true
}

// indexOfEntry finds target in m.rows by (Kind, Object, Time, Reason) —
// enough to disambiguate the merged feed's entries without a dedicated ID
// field, since target always comes from m.entries (the same slice m.rows is
// filtered from) rather than being freshly constructed.
func (m Model) indexOfEntry(target kube.TimelineEntry) int {
	return slices.IndexFunc(m.rows, func(e kube.TimelineEntry) bool {
		return e.Kind == target.Kind && e.Object == target.Object &&
			e.Reason == target.Reason && e.Time.Equal(target.Time)
	})
}

func (m *Model) updateFilterKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filterActive = false
		m.filterInput.SetValue("")
		m.filterInput.Blur()
		m.recomputeVisible()
	// ctrl+j/ctrl+k are safe alongside plain j/k typing into the query, same
	// reasoning as tasks/events' own filter handler.
	case "up", "ctrl+k":
		m.moveSelection(-1)
	case "down", "ctrl+j":
		m.moveSelection(1)
	default:
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		m.recomputeVisible()
		return m, cmd
	}
	return m, nil
}

func (m *Model) moveSelection(delta int) {
	if len(m.rows) == 0 {
		m.selected = 0
		return
	}
	m.selected = clamp(m.selected+delta, 0, len(m.rows)-1)
}

func (m *Model) cycleWindow() {
	idx := 0
	for i, w := range windowSteps {
		if w == m.window {
			idx = i
			break
		}
	}
	m.window = windowSteps[(idx+1)%len(windowSteps)]
	m.recomputeVisible()
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

// CapturingInput reports whether the filter box or 16b's rollback confirm
// (y/N, or PROD's type-the-name modal) is open, so the root shell lets every
// keystroke reach timeline's own key handling instead of letting a bare 'n'
// get hijacked into the global namespace palette mid-confirm (mirrors
// poddetail.CapturingInput).
func (m Model) CapturingInput() bool {
	return m.filterActive || m.actionsCtl.Active()
}
