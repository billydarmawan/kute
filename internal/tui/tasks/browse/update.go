package browse

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/metapanel"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// pasteTarget resolves which of browse's many buffers a paste belongs to. The
// chain mirrors updateKey's own precedence branch for branch, including the
// branches that own the keyboard but have no text field (an inline y/N
// confirm, the edit/stop-all prompts): those return nil, so a paste is
// swallowed rather than falling through to a panel underneath — the same
// answer updateKey gives a stray keypress.
func (m *Model) pasteTarget() tui.PasteTarget {
	switch {
	case m.actions.Active():
		if !m.typingConfirmName() {
			return nil // inline y/N confirm: nothing to paste into
		}
		if m.pendingBulkCount() {
			return m.bulkCountPasteTarget()
		}
		return m.actions.PasteTarget()
	case m.pendingEdit != nil, m.pendingStopAllForwards:
		return nil
	case m.pendingCronJobRun != nil, m.pendingCronJobResume != nil:
		return nil // enter/y/esc only — no text buffer to paste into
	case m.pendingJobRerun != nil:
		return nil // enter/↑↓/esc only — no text buffer to paste into
	case m.pendingScale != nil:
		return m.scalePasteTarget()
	case m.pendingSetImage != nil:
		return m.setImagePasteTarget()
	case m.pendingSetResources != nil:
		return m.setResourcesPasteTarget()
	case m.pendingMeta != nil:
		return m.pendingMeta.PasteTarget()
	case m.filterActive && !m.filterListFocused:
		insert := tui.PasteInto(&m.filterInput)
		return func(s string) {
			before := m.filterInput.Value()
			insert(s)
			m.applyFilterFromInput(before)
		}
	}
	return nil
}

// Reload implements tui.Reloader: the root shell calls this on BackMsg when
// this screen resumes from the stack, since it missed every
// kube.ResourceChangedMsg for as long as it sat parked underneath whatever
// it pushed (only the active task's Update sees those — e.g. a delete
// issued from a pushed poddetail/nodedetail would otherwise leave the
// deleted row showing here until some unrelated kind change happened to
// land while this screen was active again). Mirrors this Update's own
// ResourceChangedMsg case for m.kind/auxKindOf, minus the CRD-columns
// special case — a genuine column-set change still needs its own
// ResourceChangedMsg, since Reload has no changed kind to check against.
//
// It does not re-arm the CronJob/Job clock: that chain runs on clockEpoch,
// which a reload leaves alone, and the root held its in-flight tick while
// this screen was parked — re-arming here would start a second chain.
func (m *Model) Reload() tea.Cmd {
	m.reloadEpoch++
	return m.scheduleReload(m.reloadEpoch)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	task, cmd := m.update(msg)
	// Whatever the handler swapped (rows, metrics, kind), the whole-list
	// figures View reads are brought back in step here, once, instead of
	// being recomputed on every frame (aggregates.go).
	m.syncAggregates()
	if next, ok := task.(*Model); ok && next != m {
		next.syncAggregates()
	}
	return task, cmd
}

func (m *Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cmd, ok := tui.RoutePaste(msg, m.pasteTarget()); ok {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ResourceChangedMsg:
		// 26a's open panel re-reads its object (and Service joins) from the
		// cache this event just updated — see metapanel.Refresh.
		if m.pendingMeta != nil {
			m.pendingMeta.Refresh(msg.Kind, &m.actions)
		}
		if m.pendingSetImage != nil && m.pendingSetImage.awaitingRefresh != nil &&
			msg.Kind == m.pendingSetImage.kind && !m.actions.Active() {
			// The API result can arrive before the informer observes the new
			// image. Confirm it from the matching watch update without ever
			// restoring the stale tag in the open editor.
			m.refreshSetImageTarget()
		}
		if m.pendingSetResources != nil && m.pendingSetResources.message != "" &&
			msg.Kind == m.pendingSetResources.kind && !m.actions.Active() {
			// Keep 25a's still-open panel honest after its own write. The
			// ordinary m.load below refreshes the
			// table hidden behind the panel; this refreshes the panel's CURRENT
			// column from the same informer event.
			m.refreshSetResourcesTarget()
		}
		// A CRD change may mean this kind's columns just arrived — they're
		// fetched per-kind on first read, so the first render of a custom
		// kind uses neutral ones. The root shell has already rebuilt the
		// registry by the time this lands; re-read the descriptor so the
		// new columns take effect.
		if msg.Kind == kube.KindCustomResourceDefinition && m.session != nil {
			if desc, ok := m.session.Registry.Descriptor(m.kind); ok && len(desc.Columns) != len(m.desc.Columns) {
				m.desc = desc
				// The rows on screen were projected against the old column
				// set, so they can't be rendered under the new one — they'd
				// land under the wrong headers, and a set that's now wider
				// than the table would index past its last column. The reload
				// below refills them in shape.
				m.dropStaleShapedRows()
				m.reloadEpoch++
				return m, m.scheduleReload(m.reloadEpoch)
			}
		}
		if (msg.Kind == m.kind || slices.Contains(m.currentAuxKinds(), msg.Kind)) && msg.Affects(m.countNamespace()) {
			m.reloadEpoch++
			return m, m.scheduleReload(m.reloadEpoch)
		}
		// 14b's COUNT column reads every discovered kind's own instance
		// cache and shows "–" until that cache has filled, so an instance
		// event is what replaces it with the real count — and keeps the
		// count live after that.
		if m.kind == kube.KindCustomResourceDefinition && m.session != nil {
			if desc, ok := m.session.Registry.Descriptor(msg.Kind); ok && desc.Custom {
				m.reloadEpoch++
				return m, m.scheduleReload(m.reloadEpoch)
			}
		}
	case kube.ConnStateMsg:
		wasOffline := m.offline()
		wasPollingMetrics := m.pollsMetrics()
		m.conn = kube.ConnState(msg)
		m.actions.SetOffline(m.conn.Offline())
		m.now = time.Now()
		if !wasOffline && m.offline() && wasPollingMetrics {
			// A metrics tick is another authenticated cluster request. Letting
			// its 2s chain continue while an exec credential is expired would
			// keep re-running the failed plugin even though health polling has
			// deliberately stopped. Advancing the epoch invalidates the pending
			// tick and any in-flight result.
			m.metricsEpoch++
		}
		if wasOffline && !m.offline() && m.pollsMetrics() {
			// The offline transition killed the old tick chain. Start one fresh
			// poll only after the explicit retry (or ordinary network recovery)
			// has returned the connection to Connected.
			m.metricsEpoch++
			return m, tea.Batch(m.loadMetricsCmd(m.metricsEpoch), m.scheduleMetricsTick(m.metricsEpoch))
		}
	case tui.GotoKindMsg:
		if msg.Kind == kube.KindWhoCan {
			// KindWhoCan has no resources.Descriptor to list — like Ingress/
			// Gateway routing to tasks/routetable on row-Enter, this is a
			// kind-name carve-out, just triggered by a kind switch instead
			// of a row Enter (there's no row: 22a is "a query, not a
			// browser"). Defaults to "list" against the currently showing
			// kind's own resource name; 'v'/'K'/'n' change any slot once
			// whocan is open.
			if task, cmd, ok := m.openWhoCanFromCurrentKind(); ok {
				return task, cmd
			}
			return m, nil
		}
		if msg.Kind == kube.KindOverview {
			// KindOverview has no resources.Descriptor to list — 19a is "a
			// routing layer, not a dashboard" — the same kind-name carve-out
			// KindWhoCan takes just above, triggered by a kind switch since
			// there's no row to act on.
			if m.openOverview == nil {
				return m, nil
			}
			task, cmd := m.openOverview(m.width, m.height)
			return task, cmd
		}
		if msg.Kind == kube.KindFluxTree {
			// §30b is a computed join over the Flux kinds, with no list of
			// its own — the same kind-name carve-out KindOverview takes
			// above. The goto corpus only offers it on a Flux cluster, so
			// reaching here at all already means the kinds exist.
			if m.openFluxTree == nil {
				return m, nil
			}
			task, cmd := m.openFluxTree(m.width, m.height)
			return task, cmd
		}
		if msg.Kind == kube.KindEvent {
			// Unlike KindWhoCan/KindOverview, Events does have a
			// resources.Descriptor and could list as a stock browse table —
			// but 9b (tasks/events) is the curated experience for this kind
			// (dedup, severity coloring, folded normals), so a goto jump to
			// Events (the "e" alias or the ranked-list row) is redirected
			// here the same way the 'e' key already redirects (update.go's
			// openSelectedEvents, same gating).
			if task, cmd, ok := m.openSelectedEvents(); ok {
				return task, cmd
			}
			return m, nil
		}
		cmd := m.switchKind(msg.Kind)
		if msg.Filter != "" {
			// 23b: "↵ on a listener filters to attached routes" — switchKind
			// (via resetAndLoad) already cleared any prior filter, which is
			// right for a bare kind switch but not for this one, which
			// arrives with its own destination filter already chosen.
			m.filterActive = true
			m.setFilter(msg.Filter)
			m.filterInput.Focus()
		}
		return m, cmd
	case tui.GotoResourceMsg:
		return m, m.goToResource(msg)
	case tui.SwitchNamespaceMsg:
		return m, m.switchNamespace(msg.Namespace)
	case tui.FollowObjectMsg:
		return m, m.followObject(msg)
	case tui.SwitchContextMsg:
		if msg.Err == nil {
			return m, m.switchContext(msg)
		}
		if msg.RolledBack {
			// Same kind and namespace, but the rollback rebuilt every cache
			// from scratch: re-read (keeping the filter and selection), or
			// the rows sit frozen with nothing left to update them.
			return m, m.load()
		}
	case reloadDueMsg:
		if msg.epoch == m.reloadEpoch {
			return m, m.load()
		}
	case cronJobTickMsg:
		if msg.epoch != m.clockEpoch || !m.ticksCronJobClock() {
			// Superseded by a kind/namespace/context switch (or the kind is
			// no longer CronJob/Job) — let the chain die here rather than
			// rescheduling a tick nothing needs any more.
			return m, nil
		}
		m.now = time.Now()
		switch m.kind {
		case kube.KindForward:
			return m, tea.Batch(m.load(), m.scheduleCronJobTick(m.clockEpoch))
		case kube.KindJob:
			m.applyJobTick(m.now, m.actingUser())
		default:
			m.applyCronJobTick(m.now)
		}
		return m, m.scheduleCronJobTick(m.clockEpoch)
	case rowsLoadedMsg:
		return m.applyRowsLoaded(msg)
	case spinner.TickMsg:
		if m.state != tui.TaskStateLoading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		m.now = time.Now()
		return m, cmd
	case emptyHintsMsg:
		if msg.epoch == m.reloadEpoch && msg.namespace == m.namespace && msg.kind == m.kind && m.state == tui.TaskStateEmpty {
			m.hints = msg.hints
		}
	case metricsTickMsg:
		if msg.epoch == m.metricsEpoch && m.pollsMetrics() {
			return m, tea.Batch(m.loadMetricsCmd(m.metricsEpoch), m.scheduleMetricsTick(m.metricsEpoch))
		}
	case setResourcesMetricsMsg:
		m.applySetResourcesMetrics(msg)
		return m, nil
	case podMetricsLoadedMsg:
		if msg.epoch == m.metricsEpoch && msg.namespace == m.countNamespace() && msg.err == nil {
			m.podMetrics = msg.metrics
			// A live CPU/MEM sort (sort.go's cellLess) reads straight off
			// this map, so a fresh poll can change the rank ordering even
			// though m.rows itself didn't change — re-sort to keep it live.
			if m.sortColumn > 0 {
				m.applySort()
				m.recomputeVisible()
			}
		}
	case nodeMetricsLoadedMsg:
		if msg.epoch == m.metricsEpoch && msg.err == nil {
			m.nodeMetrics = msg.metrics
			if m.sortColumn > 0 {
				m.applySort()
				m.recomputeVisible()
			}
		}
	case actions.RunningMsg:
		// A slow write's keybar line; the returned command is the only
		// path its ResultMsg arrives by.
		return m, m.actions.HandleRunning(msg)
	case actions.BulkResultMsg:
		// A marked-set verb's per-target outcomes — 20a's delete
		// (bulk.go's beginBulkDelete) or §36c's suspend/resume
		// (cronjob_actions.go's beginCronJobSuspendBulk/commitCronJobResume).
		// The outcome itself — "Delete pods (N)" or the red "N of M targets
		// failed: <first error>" — is the controller's keybar result line
		// (ActionResult), the same surface a single delete's failure uses.
		if strings.HasPrefix(msg.ActionID, bulkDeleteActionPrefix) {
			msg.Results = nameBulkDeleteErrors(msg.Results)
		}
		m.actions.HandleBulkResult(msg)
		// 0.8.0 plan §3 Phase 2 task 11: "let the caller retain only failed
		// marks" — a row that was deleted must not stay in the set, or the
		// "N marked" strip, the keybar's "delete N" and a PROD retry's
		// type-the-count target all overcount, and a StatefulSet pod
		// recreated under the same name would come back already marked.
		m.marks = retainFailedMarks(msg.Results)
		return m, m.load()
	case actions.ResultMsg:
		m.actions.HandleResult(msg)
		if metapanel.IsActionID(msg.ActionID) && m.pendingMeta != nil {
			// 26a never closes the panel on a commit's own account, success
			// or failure (docs/design README.md §26a: "confirm → execute →
			// refresh → show result → remain on screen") — HandleResult
			// either refreshes the grid from the real object + shows an
			// inline "updated ..."/"removed ..." message, or restores the
			// pre-commit interaction state with the server's error, and only
			// esc/back (the panel's own "esc" case) ever closes it — closed
			// here only means the object vanished mid-edit.
			if m.pendingMeta.HandleResult(msg) {
				m.pendingMeta = nil
			}
			// The panel's will-run strip is this result's surface.
			m.actions.DismissResult()
			if msg.Err == nil {
				return m, m.load()
			}
			return m, nil
		}
		if isSetImageActionID(msg.ActionID) && m.pendingSetImage != nil {
			// 24a's retrofit onto 26a's own keep-open contract — see
			// handleSetImageResult's doc comment. m.load() refreshes the
			// table's ROLLOUT/IMAGE columns behind the still-open panel on
			// success, same as the meta/cron-schedule branches above.
			cmd := m.handleSetImageResult(msg)
			m.actions.DismissResult() // shown in the panel
			if msg.Err == nil {
				return m, tea.Batch(cmd, m.load())
			}
			return m, cmd
		}
		if isSetResourcesActionID(msg.ActionID) && m.pendingSetResources != nil {
			cmd := m.handleSetResourcesResult(msg)
			m.actions.DismissResult() // shown in the panel
			if msg.Err == nil {
				return m, tea.Batch(cmd, m.load())
			}
			return m, cmd
		}
		if strings.HasPrefix(msg.ActionID, "cronjob-run-now-") {
			// §36b: "On success, remain on the list, show the created name" —
			// the Job watch event (auxKinds' KindJob reload) adds it to
			// history/ACT on its own; this only supplies the transient
			// confirmation text. errors.Is(ErrManualJobNameConflict) names
			// the one race worth a distinct message: the generated name lost
			// to a Job created between staging and commit — pressing ctrl-r
			// again restages against the now-current Job list rather than
			// silently picking a different name (0.8.0 plan §3 Phase 2 task
			// 14).
			switch {
			case errors.Is(msg.Err, kube.ErrManualJobNameConflict):
				m.execFeedback = "job name taken since staging — press ctrl+r to restage"
				m.actions.DismissResult()
			case msg.Err == nil:
				m.execFeedback = "✓ job created · " + m.lastCronJobRunName
				m.actions.DismissResult()
			}
			return m, m.load()
		}
		// Every other verb — delete, force-delete, cordon, drain, rollout
		// restart, rollback, suspend/resume, argo/flux, cert renew, job
		// replace — reports through the controller's keybar result line
		// (ActionResult), success or failure.
		if msg.Err == nil || strings.HasPrefix(msg.ActionID, "cronjob-") {
			return m, m.load()
		}
	case argoDashboardURLMsg:
		return m, m.handleArgoDashboardURL(msg)
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
	case dryRunSetResourcesMsg:
		return m.handleDryRunSetResources(msg)
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
// execResultMsg, duplicated per the repo's package-local-seam convention.
type execResultMsg struct{ err error }

// editResultMsg carries a kubectl edit exit outcome (edit.go) — same
// feedback channel as execResultMsg, kept as its own type for the same
// reason.
type editResultMsg struct{ err error }

// applyRowsLoaded handles a fresh List reply: sorts workload kinds
// unhealthy-first (or the user's own 1-9 column choice, if any —
// applySort/sort.go), recomputes the filtered/visible set (preserving
// selection by name where possible), and picks the resulting task state.
func (m *Model) applyRowsLoaded(msg rowsLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.epoch != m.reloadEpoch {
		// Superseded by a namespace/kind/context switch (or a CRD's columns
		// landing) that's since bumped reloadEpoch — kind and columns can
		// both still match (a namespace switch keeps the same kind and
		// descriptor), so epoch is the guard that actually catches this.
		return m, nil
	}
	if msg.kind != m.kind {
		return m, nil // stale reply for a kind we've since switched away from
	}
	if msg.columns != len(m.desc.Columns) {
		// Same staleness, one dimension over: these rows were projected
		// against a descriptor this kind no longer has (its printer columns
		// landed, or a context switch reset them), so their cells no longer
		// line up with the columns on screen. Whatever changed the descriptor
		// scheduled its own reload; this reply is that reload's predecessor.
		return m, nil
	}
	if msg.err != nil {
		m.pendingFollow = ""
		m.now = time.Now() // the error card's "last successful list … ago"
		m.state = tui.TaskStateError
		if kube.IsPermissionError(msg.err) {
			m.state = tui.TaskStatePermissionDenied
		}
		m.feedback = msg.err.Error()
		return m, nil
	}

	if m.kind == kube.KindCronJob && !tui.KindsSynced(m.lister, m.namespace, kube.KindCronJob, kube.KindJob) {
		// §4.4 point 1 / Phase 4 task 3: gate Ready/Empty on *both* required
		// caches settling, even when msg.rows is already non-empty — a
		// CronJob row projected before the Job cache has filled would show
		// "no retained runs"/ACT 0 for a CronJob that may well have history,
		// which is exactly the false-empty claim KindsSynced exists to
		// prevent. Stay loading and retry shortly, the same debounced-reload
		// idiom the generic empty-rows branch below already uses.
		m.reloadEpoch++
		return m, m.scheduleReload(m.reloadEpoch)
	}
	if m.kind == kube.KindPod && m.releasePodView {
		kinds := m.releasePodSupportKinds()
		if !m.auxKindsSynced(kinds) {
			m.reloadEpoch++
			return m, m.scheduleReload(m.reloadEpoch)
		}
		if err := m.auxKindsError(kinds); err != nil {
			m.state = tui.TaskStateError
			if kube.IsPermissionError(err) {
				m.state = tui.TaskStatePermissionDenied
			}
			m.feedback = fmt.Sprintf("couldn't resolve release Pods: %v", err)
			return m, nil
		}
	}

	m.rows = msg.rows
	m.rowColumns = msg.columns
	m.decorateDebugCopies()
	m.applySort()
	m.pods = msg.pods
	m.helmReleases = msg.helmReleases
	m.chartCacheNote = msg.chartCacheNote
	m.nodeCount = msg.nodeCount
	m.nodeCapacity = msg.nodeCapacity
	m.podCountByNode = msg.podCountByNode
	m.clusterPodTotal = msg.clusterPodTotal
	m.nodePodHealth = msg.nodePodHealth
	m.drainImpactByNode = msg.drainImpactByNode
	m.cronJobSummaries = msg.cronJobSummaries
	m.cronJobJobsErr = msg.cronJobJobsErr
	m.jobListSummaries = msg.jobListSummaries
	m.fetchedAt = time.Now()
	// Real data has landed — 15a's cached/dimmed loading view (if any) is
	// superseded either way, whether this resolves to Ready or Empty below.
	m.cachedView = false

	if len(m.rows) == 0 {
		// An aux cache that's still filling can make an otherwise-correct
		// empty result untrustworthy just as easily as m.kind's own cache
		// can, so both checks below ask about m.kind plus every aux kind its
		// own columns/prompts read (auxkinds.go) — each aux kind at its own
		// correct scope (auxScope), since not every aux kind shares the
		// primary kind's own namespace (Node's Pod aux-kind reads
		// cluster-wide regardless of m.namespace).
		if msg.cacheUnreadyAtRead || !tui.KindsSynced(m.lister, m.namespace, m.kind) || !m.auxKindsSynced(m.currentAuxKinds()) {
			// The informer cache is still filling (just after launch or mid
			// SwitchContext) — this empty result isn't trustworthy yet.
			// Stay in the loading state and retry shortly rather than
			// flashing "no <kind> in <namespace>".
			m.reloadEpoch++
			return m, m.scheduleReload(m.reloadEpoch)
		}
		err := tui.KindsError(m.lister, m.namespace, m.kind)
		if err == nil {
			err = m.auxKindsError(m.currentAuxKinds())
		}
		if err != nil {
			// Settled, but with nothing to show and a reason why. Saying so
			// beats the two alternatives — a spinner that outlives the
			// user's patience, or "no <kind>", which is a claim about the
			// cluster this screen is in no position to make.
			m.pendingFollow = ""
			if kube.IsPermissionError(err) {
				// A denial is permanent for the session, so this is 4b's
				// card rather than the retrying line below: there is no
				// retry underneath to promise, and RBAC will not change
				// while the process runs.
				m.state = tui.TaskStatePermissionDenied
				m.feedback = err.Error()
				return m, nil
			}
			// A stalled initial LIST, on the other hand, really is still
			// being retried underneath, and a success emits a change event
			// that reloads this list — so the error is a status, not a dead
			// end.
			m.state = tui.TaskStateError
			m.feedback = fmt.Sprintf("couldn't load %s: %v — retrying", lowerDisplay(m.desc.Display), err)
			return m, nil
		}
		// The empty state already says the namespace has none of this
		// kind, the followed object included.
		m.pendingFollow = ""
		m.filterActive = false
		m.setFilter("")
		m.visible = nil
		m.expandedGroups = nil
		m.display = nil
		m.selected, m.offset = 0, 0
		m.state = tui.TaskStateEmpty
		m.hints = emptyHints{}
		return m, m.loadEmptyHints()
	}

	m.recomputeVisible()
	m.state = tui.TaskStateReady
	m.feedback = ""
	m.cacheCurrentRows()
	if task, cmd, ok := m.resolveFollow(); ok {
		return task, cmd
	}

	// m.kind's own cache is already known-good (rows are non-empty), so only
	// the aux kinds its columns/prompts read need asking. A problem here
	// must not take the whole screen away from what's already correctly on
	// the table — it surfaces as one extra strip line instead of a blank/
	// wrong cell (docs/lazy-informers.md §5.6).
	m.auxKindsDeniedNote = ""
	var auxRetry tea.Cmd
	if kinds := m.currentAuxKinds(); len(kinds) > 0 {
		switch {
		case !m.auxKindsSynced(kinds):
			// Not a permission problem — the aux cache just hasn't finished
			// its initial fill yet, and a kind with genuinely zero objects
			// never emits the change event that would otherwise prompt a
			// reload (kindsync.go). Note it and check again shortly.
			m.auxKindsDeniedNote = "some columns may be incomplete — still loading"
			auxRetry = m.scheduleReload(m.reloadEpoch)
		default:
			if err := m.auxKindsError(kinds); err != nil {
				if kube.IsPermissionError(err) {
					m.auxKindsDeniedNote = "some columns may be incomplete — permission denied: " + err.Error()
				} else {
					m.auxKindsDeniedNote = "some columns may be incomplete — " + err.Error()
				}
			}
		}
	}
	return m, auxRetry
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.followNote = ""
	if m.actions.Active() {
		return m.updateConfirmKey(msg)
	}
	if m.pendingEdit != nil {
		return m.updateEditConfirmKey(msg)
	}
	if m.pendingStopAllForwards {
		return m.updateStopAllForwardsKey(msg)
	}
	if m.pendingScale != nil {
		return m.updateScaleKey(msg)
	}
	if m.pendingCronJobRun != nil {
		return m.updateCronJobRunKey(msg)
	}
	if m.pendingCronJobResume != nil {
		return m.updateCronJobResumeKey(msg)
	}
	if m.pendingJobRerun != nil {
		return m.updateJobRerunKey(msg)
	}
	if m.pendingSetImage != nil {
		return m.updateSetImageKey(msg)
	}
	if m.pendingSetResources != nil {
		return m.updateSetResourcesKey(msg)
	}
	if m.pendingMeta != nil {
		closed, cmd := m.pendingMeta.Update(msg, &m.actions)
		if closed {
			m.pendingMeta = nil
		}
		return m, cmd
	}
	if m.filterActive && !m.filterListFocused {
		return m.updateFilterKey(msg)
	}
	switch msg.String() {
	case "ctrl+q", "ctrl+c":
		return m, tea.Quit
	case "esc", "backspace":
		// 20a: "esc clears marks before it walks back a level."
		if m.clearMarks() {
			return m, nil
		}
		if m.filterActive {
			// A committed-but-list-focused filter (filterListFocused) still
			// owns esc, same as while typing — "palette/filter → close"
			// applies to both, so esc clears it rather than walking back.
			m.filterActive = false
			m.filterListFocused = false
			m.setFilter("")
			m.filterInput.Blur()
			m.clearOrigin()
			m.recomputeVisible()
			return m, nil
		}
		if m.originName != "" {
			return m, m.backToOrigin()
		}
		return m, func() tea.Msg { return tui.BackMsg{} }
	case verbs.Mark.Key:
		if m.state == tui.TaskStateReady {
			m.markCursorAndAdvance()
		}
	case verbs.MarkAll.Key:
		if m.state == tui.TaskStateReady {
			m.markAllFiltered()
		}
	case "up", "k":
		m.moveSelection(-1)
	case "down", "j":
		m.moveSelection(1)
	case "ctrl+u", verbs.PageUp.Key:
		m.moveHalfPage(-1)
	// ctrl+d is Drain on a node here, so pgdown is the half-page-down
	// that works in both modes — paging a node's pods must never open a
	// drain confirm.
	case verbs.PageDown.Key:
		m.moveHalfPage(1)
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		if m.state == tui.TaskStateReady {
			m.handleSortKey(int(msg.String()[0] - '0'))
		}
	case verbs.Filter.Key:
		if m.state == tui.TaskStateReady {
			m.filterActive = true
			m.filterListFocused = false
			m.filterInput.Focus()
		}
	case verbs.Logs.Key:
		if task, cmd, ok := m.openSelectedLogs(); ok {
			return task, cmd
		}
		if m.kind == kube.KindJob && m.openLogs != nil {
			// §37a: tails the newest attempt's pod directly from the list.
			if row, ok := m.selectedRow(); ok {
				if pod, reason, ok := m.jobLogsTarget(row); ok {
					task, cmd := m.openLogs(pod, "", m.width, m.height)
					if task != nil {
						return task, cmd
					}
				} else if reason != "" {
					m.execFeedback = reason
				}
			}
		}
		if m.kind == kube.KindCronJob && m.openLogs != nil {
			// §36a: the newest useful Pod of the selected row's most recent
			// associated Job, active or terminal, succeeded or failed
			// (verbs.Logs' own doc comment) — one level of indirection past
			// openSelectedLogs' Pod-only case above, so a CronJob row itself
			// is never the log source.
			if pod, reason, ok := m.cronJobLogsTarget(); ok {
				task, cmd := m.openLogs(pod, "", m.width, m.height)
				if task != nil {
					return task, cmd
				}
			} else if reason != "" {
				m.execFeedback = reason
			}
		}
	case "enter":
		if task, cmd, ok := m.openSelectedEnter(); ok {
			return task, cmd
		}
	case verbs.Cordon.Key:
		if m.kind == kube.KindNode {
			if row, ok := m.selectedRow(); ok {
				return m, m.beginCordon(row)
			}
		}
	case verbs.CronJobSetSchedule.Key:
		if m.argoVerbsApply() {
			// §33a's sync. Disjoint from the CronJob schedule push below
			// (Kinds never overlap — a row is never both an Application and
			// a CronJob).
			if row, ok := m.selectedRow(); ok {
				return m, m.beginArgoSync(row)
			}
		}
		if task, cmd, ok := m.openSelectedCronJobSchedule(); ok {
			return task, cmd
		}
	case verbs.ScaleUpKey:
		m.beginScale(1)
	case verbs.ScaleDownKey:
		m.beginScale(-1)
	case verbs.SetImage.Key:
		m.beginSetImage()
	case verbs.Meta.Key:
		if metapanel.Editable(m.kind) && m.mutator != nil {
			m.beginMeta()
		}
	case verbs.AllNamespaces.Key:
		if !m.desc.ClusterScoped {
			return m, m.switchNamespace("")
		}
	case verbs.JumpNamespace.Key:
		if m.grouped() {
			if ns, ok := m.selectedNamespace(); ok {
				return m, m.switchNamespace(ns)
			}
		}
	case verbs.ToggleGroup.Key:
		// Grouped mode expands a namespace group; a §30a Flux or §33a Argo
		// list expands its single healthy-tail fold, which exists ungrouped
		// too.
		if m.grouped() || m.desc.Flux || m.desc.Argo {
			m.toggleGroup()
		}
	case verbs.FluxSource.Key:
		if m.fluxNavApplies() {
			if cmd, ok := m.openSelectedFluxSource(); ok {
				return m, cmd
			}
		}
	case verbs.RolloutRestart.Key:
		if m.kind == kube.KindDeployment && m.state == tui.TaskStateReady && m.mutator != nil {
			if row, ok := m.selectedRow(); ok {
				return m, m.beginRolloutRestart(row)
			}
		}
		if m.kind == kube.KindJob && m.state == tui.TaskStateReady && m.mutator != nil {
			if row, ok := m.selectedRow(); ok {
				m.beginJobRerun(row)
				return m, nil
			}
		}
		if m.kind == kube.KindCronJob && m.state == tui.TaskStateReady && m.mutator != nil {
			if row, ok := m.selectedRow(); ok {
				m.beginCronJobRunNow(row)
				return m, nil
			}
		}
		if m.kind == kube.KindHelmRelease && m.state == tui.TaskStateReady && m.mutator != nil {
			if row, ok := m.selectedRow(); ok {
				return m, m.beginRollback(row)
			}
		}
	case verbs.SetResources.Key:
		switch {
		case m.offline() && m.retrier != nil:
			// 4a's "r retry now", promised by the offline banner and
			// keybar. First, because an offline list keeps its rows in
			// TaskStateReady, so every row verb below would otherwise
			// claim 'r' (and then be refused by actions as offline).
			m.retrier.RetryNow()
		case resourceEditable(m.kind) && m.state == tui.TaskStateReady && m.mutator != nil:
			if m.beginSetResources() {
				return m, m.loadSetResourcesMetrics()
			}
		case m.fluxVerbsApply():
			// §30a's reconcile. Ahead of the error retry case below because
			// a Flux list in TaskStateReady is by definition not in the
			// error states it handles.
			if row, ok := m.selectedRow(); ok {
				return m, m.beginFluxReconcile(row)
			}
		case m.argoVerbsApply():
			// §33a's refresh. Same reasoning as the Flux case above — an
			// Argo list in TaskStateReady is by definition not offline/
			// error, and the two never contend on the same row.
			if row, ok := m.selectedRow(); ok {
				return m, m.beginArgoRefresh(row)
			}
		case m.certVerbsApply():
			// §35c's force-renew. Same reasoning as the Flux/Argo cases
			// above — Kinds is disjoint, so a Certificate row never
			// contends with a Flux/Argo/Forward one for 'r'.
			if row, ok := m.selectedRow(); ok {
				return m, m.beginCertRenew(row)
			}
		case m.kind == kube.KindForward && m.state == tui.TaskStateReady:
			return m, m.restartSelectedForward()
		case m.state == tui.TaskStatePermissionDenied || m.state == tui.TaskStateError:
			// Permission errors need a manual retry because RBAC will not change
			// during this session; a load error is already being retried by the
			// informer, but this gives the user an immediate retry path.
			return m, m.resetAndLoad()
		}
	case verbs.WhoCan.Key:
		if m.state == tui.TaskStatePermissionDenied {
			if task, cmd, ok := m.openWhoCanFromCurrentKind(); ok {
				return task, cmd
			}
		}
	case verbs.YAML.Key:
		if m.state == tui.TaskStatePermissionDenied || m.state == tui.TaskStateError {
			return m, tea.SetClipboard(m.feedback)
		}
		if m.kind == kube.KindForward {
			return m, m.copySelectedForwardURL()
		}
		if m.kind != kube.KindHelmRelease {
			if task, cmd, ok := m.openSelectedYAML(); ok {
				return task, cmd
			}
		}
	case verbs.ArgoURL.Key:
		if m.argoVerbsApply() {
			return m, m.copySelectedArgoDashboardURL()
		}
	case verbs.Events.Key:
		if task, cmd, ok := m.openSelectedEvents(); ok {
			return task, cmd
		}
	case verbs.CopyServiceClusterIP.Key:
		if m.kind == kube.KindService {
			return m, m.copySelectedServiceAddress(2)
		}
	case verbs.CopyServiceExternalIP.Key:
		if m.kind == kube.KindService {
			return m, m.copySelectedServiceAddress(3)
		}
	case verbs.Timeline.Key:
		if task, cmd, ok := m.openSelectedTimeline(); ok {
			return task, cmd
		}
	case verbs.HelmValues.Key:
		if task, cmd, ok := m.openSelectedHelmValues(); ok {
			return task, cmd
		}
	case verbs.HelmHistory.Key:
		if task, cmd, ok := m.openSelectedHelmHistory(); ok {
			return task, cmd
		}
	case verbs.HelmPods.Key:
		if m.kind == kube.KindHelmRelease {
			if row, ok := m.selectedRow(); ok {
				return m, m.openReleaseObjects(row)
			}
		}
	case verbs.Forward.Key:
		if task, cmd, ok := m.openSelectedForward(); ok {
			return task, cmd
		}
	case verbs.Exec.Key:
		if m.kind == kube.KindForward {
			return m, m.stopSelectedForward()
		}
		if m.kind == kube.KindNode {
			// NodeDebug: replaces the retired standalone NodeShell verb
			// ('s') — see verbs.go's NodeDebug doc comment. Same gate
			// shape as Exec below, and a stronger reason for it: kubectl
			// debug creates a privileged node-debugger pod, so this writes
			// to the cluster before the user types anything.
			if verbs.NodeDebug.HiddenWhileOffline(m.offline()) {
				return m, nil
			}
			if task, cmd, ok := m.openSelectedNodeDebug(); ok {
				return task, cmd
			}
			return m, nil
		}
		// Exec is Mutating: refused while offline, like every other mutating
		// verb (docs/design README.md §52). The keybar has already dropped
		// the hint and says "mutating actions disabled", so this is a
		// deliberate no-op rather than a silent one.
		if verbs.Exec.HiddenWhileOffline(m.offline()) {
			return m, nil
		}
		if task, cmd, ok := m.beginExecOrDebug(); ok {
			if task != nil {
				return task, cmd
			}
			return m, cmd
		}
	case verbs.Drain.Key:
		if m.kind == kube.KindNode {
			if row, ok := m.selectedRow(); ok {
				return m, m.beginDrain(row)
			}
		}
	case verbs.StopAllForwards.Key:
		if m.kind == kube.KindForward && m.state == tui.TaskStateReady {
			m.beginStopAllForwards()
		}
	case verbs.FluxSuspend.Key:
		if m.fluxVerbsApply() {
			// §30a's suspend/resume. NodeShell's own 's' below is Node-only,
			// so the two never contend on the same row.
			if row, ok := m.selectedRow(); ok {
				return m, m.beginFluxSuspend(row)
			}
		}
		if m.kind == kube.KindJob && m.state == tui.TaskStateReady && m.mutator != nil {
			// A Job's own suspend/resume. Never contends with the Flux block
			// above (a row can't be both) or NodeShell's own 's' below
			// (Node-only).
			if row, ok := m.selectedRow(); ok {
				return m, m.beginJobSuspend(row)
			}
		}
		if m.kind == kube.KindCronJob && m.state == tui.TaskStateReady && m.mutator != nil {
			// A CronJob's own suspend/resume — direction chosen per-row, or
			// (with a marked set) from the cursor row, per
			// beginCronJobSuspendOrResume's own doc comment. Never contends
			// with Job's own 's' above (disjoint Kinds) or NodeShell's below
			// (Node-only).
			if row, ok := m.selectedRow(); ok {
				return m, m.beginCronJobSuspendOrResume(row)
			}
		}
	case verbs.Edit.Key:
		// kubectl edit applies whatever the user saves, so it's gated with
		// the other mutating verbs while offline (docs/design README.md
		// §4a: "delete/exec/edit verbs are disabled while offline").
		if verbs.Edit.HiddenWhileOffline(m.offline()) {
			return m, nil
		}
		if cmd, ok := m.beginEdit(); ok {
			return m, cmd
		}
	case verbs.Delete.Key:
		if m.state == tui.TaskStateReady && m.mutator != nil && m.kind != kube.KindForward && m.kind != kube.KindHelmRelease {
			if verbs.Delete.Bulk && len(m.marks) > 0 {
				return m, m.beginBulkDelete()
			}
			if row, ok := m.selectedRow(); ok {
				return m, m.beginDelete(row)
			}
		}
	}
	return m, nil
}

// openSelectedEnter is enter's routing chain for updateKey's plain enter —
// not while filtering, where enter commits the filter instead of opening
// anything (updateFilterKey's "enter" case): node/pod detail, the
// Ingress/Gateway-API routing table (23a/23b — checked ahead of the generic
// Custom-kind branch below since HTTPRoute/GRPCRoute/TCPRoute/Gateway are
// themselves Custom), a Deployment's own pods, a StatefulSet's own pods, a
// DaemonSet's own pods, a Job's own pods, a CronJob's own pods (skipping the
// intermediate Job it spawns), a CRD's instance list, or a generic Custom
// kind's object detail, in that priority order. ok is false
// when the current kind/selection has no enter destination (e.g. Services,
// Forwards), so the caller leaves the key unhandled.
func (m *Model) openSelectedEnter() (tea.Model, tea.Cmd, bool) {
	if task, cmd, ok := m.openSelectedNodeDetail(); ok {
		return task, cmd, true
	}
	if task, cmd, ok := m.openSelectedPodDetail(); ok {
		return task, cmd, true
	}
	if task, cmd, ok := m.openSelectedRouteTable(); ok {
		return task, cmd, true
	}
	if task, cmd, ok := m.openSelectedSecretData(); ok {
		return task, cmd, true
	}
	if task, cmd, ok := m.openSelectedConfigMapData(); ok {
		return task, cmd, true
	}
	if cmd, ok := m.openSelectedNamespacePods(); ok {
		return m, cmd, true
	}
	if m.kind == kube.KindDeployment {
		if row, ok := m.selectedRow(); ok {
			return m, m.openDeploymentPods(row), true
		}
	}
	if m.kind == kube.KindStatefulSet {
		if row, ok := m.selectedRow(); ok {
			return m, m.openStatefulSetPods(row), true
		}
	}
	if m.kind == kube.KindDaemonSet {
		if row, ok := m.selectedRow(); ok {
			return m, m.openDaemonSetPods(row), true
		}
	}
	if task, cmd, ok := m.openSelectedJobAttempts(); ok {
		return task, cmd, true
	}
	if task, cmd, ok := m.openSelectedCronJobDetail(); ok {
		return task, cmd, true
	}
	if m.kind == kube.KindHelmRelease {
		if task, cmd, ok := m.openSelectedHelmDetail(); ok {
			return task, cmd, true
		}
	}
	if m.kind == kube.KindCustomResourceDefinition {
		if cmd, ok := m.openCRDInstances(); ok {
			return m, cmd, true
		}
	}
	// §31a before 14d: a Flux descriptor is Custom too, so the generic
	// branch would otherwise claim it first.
	if task, cmd, ok := m.openSelectedFluxDetail(); ok {
		return task, cmd, true
	}
	// §35a before 14d, same reasoning: Certificate is Custom too.
	if task, cmd, ok := m.openSelectedCertChain(); ok {
		return task, cmd, true
	}
	if task, cmd, ok := m.openSelectedObjectDetail(); ok {
		return task, cmd, true
	}
	return nil, nil, false
}

// updateConfirmKey routes keys while a mutating action's confirmation is
// showing: a verb requiresTypeNameConfirm at TierModal — delete/force-delete
// or 9a's rollout-restart (8b/9a's type-the-name PROD modal) — gets its own
// key handling; every other confirming case — TierNone/TierInline, and
// Drain's TierModal (nodes.go's beginDrain, still Phase 9's plain
// ConfirmCard, deliberately not upgraded — see mvp-tasks.md's Phase 5/8b
// exit notes) — stays the simple y/n/esc prompt, plus verbs.ForceDelete.Key
// on a pending inline Pod delete, which escalates it to force-delete at
// ForceDelete's own TierModal (Controller.EscalateForceDelete): the confirm
// becomes the type-the-name modal, never a second inline "y" (CLAUDE.md:
// "drain/force-delete always modal"). Everything else is swallowed so
// movement/filter can't act underneath.
func (m *Model) updateConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.typingConfirmName() {
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

// cancelInlineConfirm is updateConfirmKey's shared "actually cancel" path
// for n and esc.
func (m *Model) cancelInlineConfirm() {
	if m.pendingMeta != nil {
		// The panel stayed open under this confirm (metapanel's doc comment)
		// with the row's edit already applied to its buffer — cancelling
		// must revert that buffer, the same "esc backs out without keeping
		// the typed change" contract editing mode's own esc already has. A
		// no-op for a pending removal, whose buffer never diverged from
		// current in the first place.
		m.pendingMeta.CancelConfirm()
	}
	if m.pendingSetImage != nil {
		// 24a's own version of the same revert: the buffer already holds the
		// attempted tag/ref (commitSetImage no longer clears it), so cancelling
		// the PROD confirm must discard it back to the active container's real
		// current image — the same "esc backs out without keeping the typed
		// change" contract.
		resetSetImageBuffer(m.pendingSetImage)
		m.pendingSetImage.historyIdx = matchHistoryIndex(m.pendingSetImage)
	}
	if m.pendingSetResources != nil {
		m.pendingSetResources.pendingCommit = nil
		m.selectSetResourcesContainer(m.pendingSetResources.containerIdx)
	}
	m.actions.Cancel()
}

// isSetImageActionID reports whether id names a 24a set-image action
// (setimage.go's commitSetImage ID scheme) — used by the actions.ResultMsg
// handler above to route the result through handleSetImageResult instead of
// the generic m.load()-only path.
func isSetImageActionID(id string) bool {
	return strings.HasPrefix(id, "set-image-")
}

func isSetResourcesActionID(id string) bool {
	return strings.HasPrefix(id, "set-resources-")
}

// updateModalConfirmKey drives the 8b type-the-name modal: enter executes
// only once Controller.NameMatches ("↵ stays dead until the typed name
// matches"), backspace/typing edit the buffer, verbs.ForceDelete.Key
// escalates a pending Pod delete to force-delete (the same chord the modal's
// detail line advertises, verbs.ForceDeleteModalHint), esc cancels.
//
// A marked-set confirm (pendingBulkCount: 20a's delete or §36c's
// cronjob-suspend) reuses this same routing but against the count grammar
// instead: enter's gate is still Controller.Confirm's own, which for a marked
// set compares the typed digits with the marked count, and typing is
// digit-filtered (bulkCountKeyIsDigit). ForceDelete.Key never escalates a
// marked set (Controller.EscalateForceDelete refuses bulk targets), so it
// reaches the count buffer like any other key.
func (m *Model) updateModalConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	bulk := m.pendingBulkCount()
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
		if bulk && msg.Text != "" && !bulkCountKeyIsDigit(msg) {
			return m, nil
		}
		return m, m.actions.HandleTypeKey(msg)
	}
	return m, nil
}

func (m *Model) updateFilterKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filterActive = false
		m.filterListFocused = false
		m.setFilter("")
		m.filterInput.Blur()
		m.clearOrigin()
		m.recomputeVisible()
	case "enter":
		// Enter never opens a destination while filtering, even for kinds
		// that have one (Pods, Nodes, Deployments, …) — it always commits
		// the filter instead: query/rows/chrome stay exactly as they are,
		// but keys stop being captured as typing, so j/k, ctrl-d, etc. act
		// on the narrowed rows directly. '/' resumes editing the same
		// query; a second enter (now routed through updateKey, unfiltered)
		// opens the selected row's destination same as it always has.
		m.filterListFocused = true
		m.filterInput.Blur()
	// Only the arrow keys (plus ctrl+j/ctrl+k, which never carry Text) move
	// selection while filtering — plain "j"/"k" must stay typeable into the
	// query (mvp-plan.md's "j/k ≡ ↑↓ everywhere" is for browse mode; a live
	// filter input takes every character).
	case "up", "ctrl+k":
		m.moveSelection(-1)
	case "down", "ctrl+j":
		m.moveSelection(1)
	case "ctrl+d", verbs.PageDown.Key:
		m.moveHalfPage(1)
	case "ctrl+u", verbs.PageUp.Key:
		m.moveHalfPage(-1)
	case verbs.MarkAll.Key:
		// 20a: "filter-then-mark is the bulk grammar" — '*' marks every row
		// the live query currently matches without leaving filter mode.
		// Intercepted here rather than falling to the default typing branch
		// because '*' can never appear in a Kubernetes object name, so this
		// never shadows a character a real filter query would need (unlike
		// 6a's "a", which stays typeable for exactly that reason).
		m.markAllFiltered()
	default:
		var cmd tea.Cmd
		before := m.filterInput.Value()
		m.filterInput, cmd = m.filterInput.Update(msg)
		m.applyFilterFromInput(before)
		return m, cmd
	}
	return m, nil
}

// applyFilterFromInput re-applies the filter after an in-place edit of the
// query buffer — typing, or a paste (pasteTarget) — when the value actually
// changed from before. Shared so the two paths can't drift.
func (m *Model) applyFilterFromInput(before string) {
	after := m.filterInput.Value()
	if after == before {
		return
	}
	// Sync Session.Location.Filter directly rather than going through
	// setFilter, which forces the cursor to the end — right for a wholesale
	// replace (setFilter's other callers) but wrong here, where the edit that
	// just landed may have been a mid-string insert/delete.
	if m.session != nil {
		m.session.Location.Filter = after
	}
	m.clearOrigin()
	m.recomputeVisible()
}

// openSelectedLogs pushes the log-stream screen for the selected row (Pods
// only). ok is false when logs aren't wired or nothing's selected, so the
// caller leaves 'l' a no-op rather than pushing a broken screen.
func (m Model) openSelectedLogs() (tea.Model, tea.Cmd, bool) {
	if m.openLogs == nil || m.kind != kube.KindPod {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	pod, ok := m.pods[kube.PodKey(row.Namespace, row.Name)]
	if !ok {
		pod = kube.Pod{Namespace: row.Namespace, Name: row.Name}
	}
	task, cmd := m.openLogs(pod, "", m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedPodDetail pushes 5a for the selected Pod row, handing over
// the current visible list's ordered namespace-qualified refs + the
// selected row's position so
// poddetail's j/k can move to the next/prev pod without leaving detail
// (works the same in 6b's grouped view — m.visible stays name-ordered
// regardless of the interspersed GroupHeader rendering, per grouping.go).
func (m Model) openSelectedPodDetail() (tea.Model, tea.Cmd, bool) {
	if m.openPodDetail == nil || m.kind != kube.KindPod {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	pod, ok := m.pods[kube.PodKey(row.Namespace, row.Name)]
	if !ok {
		pod = kube.Pod{Namespace: row.Namespace, Name: row.Name}
	}
	siblings := make([]PodSiblingRef, len(m.visible))
	index := 0
	for i, fm := range m.visible {
		siblings[i] = PodSiblingRef{Namespace: fm.row.Namespace, Name: fm.row.Name}
		if fm.row.Namespace == row.Namespace && fm.row.Name == row.Name {
			index = i
		}
	}
	task, cmd := m.openPodDetail(pod, siblings, index, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedSecretData pushes 27b for the selected Secret row's Data view.
// ok is false when the hook isn't wired, the current kind isn't Secret, or
// nothing's selected — so ↵ falls through to whatever the rest of
// openSelectedEnter's chain (or ultimately nothing) resolves to.
func (m Model) openSelectedSecretData() (tea.Model, tea.Cmd, bool) {
	if m.openSecretData == nil || m.kind != kube.KindSecret {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	task, cmd := m.openSecretData(row.Namespace, row.Name, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedConfigMapData pushes 27a for the selected ConfigMap row's Data
// view. ok is false when the hook isn't wired, the current kind isn't
// ConfigMap, or nothing's selected — so ↵ falls through to whatever the rest
// of openSelectedEnter's chain (or ultimately nothing) resolves to.
func (m Model) openSelectedConfigMapData() (tea.Model, tea.Cmd, bool) {
	if m.openConfigMapData == nil || m.kind != kube.KindConfigMap {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	task, cmd := m.openConfigMapData(row.Namespace, row.Name, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedYAML pushes 8a for the selected row, any kind (docs/design
// README.md's system-wide interactions: "y opens the YAML view on any
// selected object, any kind" — not gated to Pods, unlike logs/detail).
func (m Model) openSelectedYAML() (tea.Model, tea.Cmd, bool) {
	if m.openYAML == nil || m.state != tui.TaskStateReady {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	task, cmd := m.openYAML(m.kind, row.Namespace, row.Name, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedEvents pushes 9b namespace-scoped (docs/design README.md
// §9b: "reached by e from browse, namespace-scoped") — m.namespace is
// already "" in 6b's all-namespaces triage, so no separate branch is needed
// for that case. Doesn't need a selected row, unlike YAML/logs/detail —
// which is exactly why TaskStateEmpty (10c: connected, zero rows of *this
// kind* in this namespace) must work too, not just TaskStateReady: the
// namespace can easily have events tied to some other kind (10c's own hint
// line already says as much — "g other kinds — this namespace has N
// pods...") even when the browsed kind has none. Excluding TaskStateEmpty
// silently ate every 'e' press (and 'g'-jump to Events, which redirects
// through this same gate) from an empty-state screen.
func (m Model) openSelectedEvents() (tea.Model, tea.Cmd, bool) {
	if m.openEvents == nil || (m.state != tui.TaskStateReady && m.state != tui.TaskStateEmpty) {
		return nil, nil, false
	}
	task, cmd := m.openEvents(m.namespace, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedTimeline pushes 16b object-scoped when the cursor sits on a
// Deployment/Pod/Node row (the mockup's own "t on a selected deployment"
// caption, docs/design README.md §16b — previously only reachable via
// poddetail/nodedetail's own 't'), falling back to 16a namespace-scoped
// (§16a) otherwise — same "no selected row needed, namespace already
// carries the all-namespaces case" shape openSelectedEvents uses, including
// the same TaskStateEmpty carve-out.
func (m Model) openSelectedTimeline() (tea.Model, tea.Cmd, bool) {
	if m.state != tui.TaskStateReady && m.state != tui.TaskStateEmpty {
		return nil, nil, false
	}
	if m.openObjectTimeline != nil && isObjectTimelineKind(m.kind) {
		if row, ok := m.selectedRow(); ok {
			task, cmd := m.openObjectTimeline(m.kind, row.Namespace, row.Name, m.width, m.height)
			return task, cmd, task != nil
		}
	}
	if m.openTimeline == nil {
		return nil, nil, false
	}
	task, cmd := m.openTimeline(m.namespace, m.width, m.height)
	return task, cmd, task != nil
}

// isObjectTimelineKind reports whether kind resolves to a real 16b scope —
// the same three kinds tasks/timeline's own load.go (restartsForScope/
// resolveOwningDeployment) knows how to scope a merged feed + revision rail
// to.
func isObjectTimelineKind(kind kube.ResourceKind) bool {
	return kind == kube.KindDeployment || kind == kube.KindPod || kind == kube.KindNode
}

// openSelectedExec resolves 'x' for the selected Pod row (docs/design
// README.md §10a): a single container execs immediately via kube.ExecSpec —
// task is nil and cmd is the tea.ExecProcess Cmd, so browse stays the
// active task and handles its own execResultMsg — while more than one
// container pushes tasks/execpicker instead. ok is false when nothing
// applies (not a Pod, no row selected, or no containers known), so 'x'
// stays a no-op rather than the caller misreading a nil task as failure.
func (m Model) openSelectedExec() (tea.Model, tea.Cmd, bool) {
	if m.kind != kube.KindPod {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	pod, ok := m.pods[kube.PodKey(row.Namespace, row.Name)]
	if !ok || len(pod.ContainerInfos) == 0 {
		return nil, nil, false
	}
	if len(pod.ContainerInfos) == 1 {
		return nil, execCmd(m.session.CommandTarget(), pod.Namespace, pod.Name, pod.ContainerInfos[0].Name, "", m.demo), true
	}
	if m.openExec == nil {
		return nil, nil, false
	}
	task, cmd := m.openExec(pod.Namespace, pod.Name, pod.ContainerInfos, nil, m.width, m.height)
	return task, cmd, task != nil
}

// execCmd suspends the program and hands the tty to kubectl for container
// (tea.ExecProcess over kube.ExecSpec) — shared shape with
// tasks/execpicker's own execSelected, duplicated per the repo's
// package-local-seam convention. demo short-circuits before building a real
// kubectl command (kube.ErrDemoUnavailable's own doc comment): there's no
// cluster behind kube/fake for a real tty to attach to.
func execCmd(target kube.CommandTarget, namespace, pod, container, shell string, demo bool) tea.Cmd {
	if demo {
		return func() tea.Msg { return execResultMsg{err: kube.ErrDemoUnavailable} }
	}
	spec := kube.ExecSpec(target, namespace, pod, container, shell)
	return tea.ExecProcess(spec, func(err error) tea.Msg {
		return execResultMsg{err: err}
	})
}

// openSelectedNodeDebug resolves 'x' for the selected Nodes row (§41d):
// pushes tasks/debugpanel via openNodeDebug. ok is false when nothing
// applies (not the Nodes kind, no row selected, not wired) or the row can't
// host a node shell — in the latter case browse sets execFeedback to the
// reason instead of pushing (kube.NodeShellUnavailable: the key stays on
// the keybar and explains itself, rather than a hidden key).
func (m *Model) openSelectedNodeDebug() (tea.Model, tea.Cmd, bool) {
	if m.openNodeDebug == nil || m.kind != kube.KindNode || m.state != tui.TaskStateReady {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	if row.NodeShellUnavailable != "" {
		m.execFeedback = row.NodeShellUnavailable
		return nil, nil, false
	}
	task, cmd := m.openNodeDebug(row.Name, m.podCountByNode[row.Name], m.width, m.height)
	return task, cmd, task != nil
}
