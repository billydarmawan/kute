package whocan

import (
	"fmt"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// Reload implements tui.Reloader — see its doc comment: this screen misses
// every kube.ResourceChangedMsg while parked in the stack, so BackMsg
// restoring it asks it to catch up immediately rather than showing stale
// data until an unrelated RBAC-kind change happens to land while it's
// active again. Calls m.load() directly rather than the heavier m.reload()
// (which resets to the loading state and clears the rows) — a resumed
// screen should refresh quietly, the same way an active one does on its own
// kube.ResourceChangedMsg case just below.
func (m *Model) Reload() tea.Cmd {
	if m.rbac == nil {
		return nil
	}
	return m.load()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ResourceChangedMsg:
		if isRBACKind(msg.Kind) && m.rbac != nil {
			return m, m.load()
		}
	case tui.CacheSyncRetryMsg:
		if msg.Gen == m.reloadEpoch && m.rbac != nil {
			return m, m.load()
		}
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
	case tui.SwitchNamespaceMsg:
		if msg.Namespace != m.namespace {
			m.namespace = msg.Namespace
			return m, m.reload()
		}
	case tui.SetWhoCanVerbMsg:
		if msg.Verb != m.verb {
			m.verb = msg.Verb
			return m, m.reload()
		}
	case tui.SetWhoCanResourceMsg:
		if msg.Resource != m.resource {
			m.resource = msg.Resource
			return m, m.reload()
		}
	case loadedMsg:
		return m.applyLoaded(msg)
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

// isRBACKind reports whether kind is one of the four RBAC kinds whose
// change should trigger a re-resolve — every other kind change is
// irrelevant to who-can's own binding graph.
func isRBACKind(kind kube.ResourceKind) bool {
	switch kind {
	case kube.KindRole, kube.KindRoleBinding, kube.KindClusterRole, kube.KindClusterRoleBinding:
		return true
	default:
		return false
	}
}

// reload puts the model back into the loading state and re-issues load() —
// called whenever the query's verb/resource/namespace slot changes.
func (m *Model) reload() tea.Cmd {
	m.reloadEpoch++
	m.state = tui.TaskStateLoading
	m.feedback = "Resolving who can " + m.verb + " " + m.resource + "..."
	m.rows = nil
	m.selected = 0
	m.offset = 0
	return tea.Batch(m.load(), m.spinner.Tick)
}

func (m Model) load() tea.Cmd {
	epoch := m.reloadEpoch
	rbac := m.rbac
	resource, group := kube.SplitRBACResource(m.resource)
	query := kube.WhoCanQuery{Verb: m.verb, Resource: resource, Group: group, Namespace: m.namespace}
	ctx := m.session.ClusterContext()
	return func() tea.Msg {
		result, err := rbac.WhoCan(ctx, query)
		return loadedMsg{epoch: epoch, result: result, err: err}
	}
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
	// alreadyRendered is true once a prior load has already put rows on
	// screen. A background reload finding a required cache merely *stalled*
	// (not yet synced, no error yet) must not discard that content back into
	// a loading spinner — only a confirmed denial/error (checked below,
	// unconditionally) may change what's on screen once something has
	// already rendered.
	alreadyRendered := m.state == tui.TaskStateReady

	// Role/RoleBinding (namespaced) and ClusterRole/ClusterRoleBinding
	// (cluster-scoped) are who-can's one required set — there is no
	// secondary cache here; the four kinds together ARE the answer. These
	// checks run unconditionally rather than only when the result was
	// empty, so a denial on one pair is never invisible just because the
	// other pair already resolved a non-empty result — a partial answer is
	// as dangerous a security claim as an empty one
	// (docs/lazy-informers.md §5.6).
	if !tui.KindsSynced(m.rbac, m.namespace, kube.KindRole, kube.KindRoleBinding) ||
		!tui.KindsSynced(m.rbac, "", kube.KindClusterRole, kube.KindClusterRoleBinding) {
		// 22a resolves bindings entirely from informer caches, and all four
		// RBAC informers start on first read — so the first answer is empty
		// on a cold session. "No one can do this" is a security claim; it
		// must not be made about a cache that hasn't loaded. The retry is
		// scheduled unconditionally — a background reload can find a cache
		// merely stalled just as easily as a cold-start one can — but once
		// something has already rendered, that result stays on screen
		// as-is: only alreadyRendered's own state/feedback are skipped,
		// never the retry, so a stalled cache doesn't collapse existing
		// rows back into a loading spinner while still catching up once
		// the cache actually fills.
		if !alreadyRendered {
			m.state = tui.TaskStateLoading
			m.feedback = ""
		}
		return m, tui.ScheduleCacheSyncRetry(m.reloadEpoch)
	}
	if err := tui.KindsError(m.rbac, m.namespace, kube.KindRole, kube.KindRoleBinding); err != nil {
		// The same security claim, against a cache that stopped filling
		// rather than one that never started (see tui.KindsError) — a
		// denial always wins, even over rows already on screen.
		m.state = tui.TaskStateError
		if kube.IsPermissionError(err) {
			m.state = tui.TaskStatePermissionDenied
		}
		m.feedback = fmt.Sprintf("couldn't load RBAC bindings: %v", err)
		return m, nil
	}
	if err := tui.KindsError(m.rbac, "", kube.KindClusterRole, kube.KindClusterRoleBinding); err != nil {
		// Same as above, for the cluster-scoped half of the RBAC graph.
		m.state = tui.TaskStateError
		if kube.IsPermissionError(err) {
			m.state = tui.TaskStatePermissionDenied
		}
		m.feedback = fmt.Sprintf("couldn't load RBAC bindings: %v", err)
		return m, nil
	}

	m.result = msg.result
	m.rows = buildRows(msg.result)
	m.state = tui.TaskStateReady
	if len(m.rows) == 0 {
		// The checks above have already confirmed all four caches are both
		// synced and error-free, so an empty result here is a trustworthy
		// one.
		m.state = tui.TaskStateEmpty
	}
	m.feedback = ""
	if m.selected >= len(m.rows) {
		m.selected = max(len(m.rows)-1, 0)
	}
	m.clampOffset()
	return m, nil
}

// buildRows promotes the current user's own verdict to a pinned first row
// (docs/design README.md §22a). The pinned row always names CurrentUser
// itself (not whichever subject actually matched) since the grant is very
// often a Group the user belongs to rather than a "User" subject bearing
// their own name (e.g. a client cert's Organization — "system:masters" —
// rather than its CommonName), so kube.ResolveWhoCan's own
// CurrentUserVia/CurrentUserClusterScope (which already account for both
// shapes) are the source of truth here, not a re-derived Subjects scan.
// Only an exact "User" kind match is dropped from the plain list below it
// (the same person, never shown twice) — a Group row that happens to cover
// the current user stays listed as-is, since it also covers every other
// member of that group.
func buildRows(result kube.WhoCanResult) []whoCanRow {
	rows := make([]whoCanRow, 0, len(result.Subjects)+1)
	if result.CurrentUser != "" {
		rows = append(rows, whoCanRow{
			pinned:  true,
			granted: result.CurrentUserGranted,
			unknown: result.CurrentUserUnknown,
			subject: kube.WhoCanSubject{
				Name:         result.CurrentUser,
				Kind:         "User",
				Via:          result.CurrentUserVia,
				ClusterScope: result.CurrentUserClusterScope,
				OnlyNames:    result.CurrentUserOnlyNames,
			},
		})
	}
	for _, s := range result.Subjects {
		if result.CurrentUser != "" && s.Kind == "User" && s.Name == result.CurrentUser {
			continue
		}
		rows = append(rows, whoCanRow{subject: s})
	}
	return rows
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
		m.moveSelection(max(1, m.height/2))
	case "ctrl+u":
		m.moveSelection(-max(1, m.height/2))
	case "enter":
		if task, cmd, ok := m.openSelectedBinding(); ok {
			return task, cmd
		}
	}
	return m, nil
}

func (m *Model) moveSelection(delta int) {
	if len(m.rows) == 0 {
		m.selected, m.offset = 0, 0
		return
	}
	m.selected = clamp(m.selected+delta, 0, len(m.rows)-1)
	m.clampOffset()
}

// clampOffset keeps the selected row within the table's rendered viewport —
// mirrors nodedetail/routetable's own clampOffset/tableDataRows pattern.
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

// openSelectedBinding is 22a's "↵ opens the binding's YAML" — pushes 8a for
// the resolved subject row's backing RoleBinding/ClusterRoleBinding. A
// no-op for the pinned miss row (BindingName is empty: there's no real
// binding to open when the current user has no matching grant at all).
func (m Model) openSelectedBinding() (tea.Model, tea.Cmd, bool) {
	row, ok := m.selectedRow()
	if !ok || m.openYAML == nil || row.subject.BindingName == "" {
		return nil, nil, false
	}
	task, cmd := m.openYAML(row.subject.BindingKind, row.subject.BindingNamespace, row.subject.BindingName, m.width, m.height)
	return task, cmd, task != nil
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
