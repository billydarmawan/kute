package events

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// pasteTarget is the '/' filter buffer while it's open. The filter is
// re-applied inside the closure so a pasted query narrows the feed exactly as
// a typed one does (updateFilterKey's own recompute).
func (m *Model) pasteTarget() tui.PasteTarget {
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
		if msg.Kind == kube.KindEvent && m.events != nil {
			m.loadEpoch++
			return m, m.load()
		}
	case tui.CacheSyncRetryMsg:
		if msg.Gen == m.syncRetryGen && m.events != nil {
			m.loadEpoch++
			return m, m.load()
		}
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
	case tui.SwitchNamespaceMsg:
		return m, m.switchNamespace(msg.Namespace)
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

func (m *Model) applyLoaded(msg loadedMsg) (tea.Model, tea.Cmd) {
	if msg.epoch != m.loadEpoch {
		// Superseded by a namespace switch or another reload dispatched
		// since — kind/columns has no equivalent here, so this is the only
		// guard against a slow reply from a namespace the screen has since
		// left landing under the new one's breadcrumb.
		return m, nil
	}
	// alreadyRendered mirrors timeline/whocan's own stall-vs-rerender rule:
	// once something is already on screen, a background reload finding the
	// Event cache merely *stalled* must not discard it into a loading
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

	if !alreadyRendered && !tui.KindsSynced(m.lister, m.namespace, kube.KindEvent) {
		// The Event informer starts on first read, so an empty first answer
		// usually means the cache hasn't filled — not that the namespace is
		// quiet, which is a reassuring thing to say wrongly during an
		// incident.
		m.state = tui.TaskStateLoading
		m.feedback = ""
		m.syncRetryGen++
		return m, tui.ScheduleCacheSyncRetry(m.syncRetryGen)
	}
	if err := tui.KindsError(m.lister, m.namespace, kube.KindEvent); err != nil {
		// Settled without ever having loaded (see tui.KindsError). A quiet
		// namespace and an Event cache that can't fill look identical from
		// here, and only one of them is safe to report during an incident.
		// A denial always wins, even over groups already on screen.
		m.state = tui.TaskStateError
		if kube.IsPermissionError(err) {
			m.state = tui.TaskStatePermissionDenied
		}
		m.feedback = fmt.Sprintf("couldn't load events: %v", err)
		return m, nil
	}

	m.groups = msg.groups
	m.failing = msg.failing
	m.fetchedAt = time.Now()
	m.recomputeVisible()
	m.state = tui.TaskStateReady
	if len(m.groups) == 0 {
		// The checks above have already confirmed the Event cache is both
		// synced and error-free, so an empty result here is trustworthy.
		m.state = tui.TaskStateEmpty
	}
	m.feedback = ""
	return m, nil
}

// recomputeVisible rebuilds m.rows from m.groups: window + filter-query
// applied first, then partitioned warnings-first with normal groups folded
// into one summary row unless normalExpanded (docs/design README.md §9b).
// Both Update's keyboard handlers (window/fold/filter toggles) and Render's
// summary-strip counts read m.rows, so there's exactly one place events are
// windowed/filtered/folded — never two paths that could disagree.
func (m *Model) recomputeVisible() {
	cutoff := time.Time{}
	if m.window > 0 {
		cutoff = m.fetchedAt.Add(-m.window)
	}

	var warnings, normal []kube.EventGroup
	baseline, matched := 0, 0
	for _, g := range m.groups {
		if !cutoff.IsZero() && g.LastSeen.Before(cutoff) {
			continue
		}
		if g.Type != "Warning" && m.warningsOnly {
			continue
		}
		baseline++
		if m.filterInput.Value() != "" && !matchesQuery(g, m.filterInput.Value()) {
			continue
		}
		matched++
		if g.Type == "Warning" {
			warnings = append(warnings, g)
		} else {
			normal = append(normal, g)
		}
	}
	m.filterBaselineGroups, m.filterMatchedGroups = baseline, matched

	rows := make([]displayRow, 0, len(warnings)+1)
	for _, g := range warnings {
		rows = append(rows, displayRow{kind: rowGroup, group: g})
	}
	if len(normal) > 0 {
		if m.normalExpanded {
			for _, g := range normal {
				rows = append(rows, displayRow{kind: rowGroup, group: g})
			}
		} else {
			rows = append(rows, displayRow{kind: rowFolded, folded: normal})
		}
	}
	m.rows = rows
	if m.selected >= len(m.rows) {
		m.selected = max(len(m.rows)-1, 0)
	}
}

func matchesQuery(g kube.EventGroup, query string) bool {
	q := strings.ToLower(query)
	return strings.Contains(strings.ToLower(g.Reason), q) ||
		strings.Contains(strings.ToLower(g.Object), q) ||
		strings.Contains(strings.ToLower(g.Message), q)
}

// switchNamespace re-scopes 9b to a different namespace — namespace == ""
// switches to all-namespaces (browse's own "a" key, docs/design README.md
// §6a/§6b's "a all namespaces" idiom), reached here either via 9b's own "a"
// key or the namespace palette's tui.SwitchNamespaceMsg (the root shell's
// "n" key opens the palette for whatever Screen is active, not just
// browse — 9b already satisfies Screen/CapturingInput, so the palette
// already opens over it; this is what makes selecting a namespace there
// actually do something). A no-op in object-scoped mode (poddetail's/
// nodedetail's "e"), where there's no namespace to switch — the screen is
// pinned to one object, mirroring poddetail/nodedetail themselves never
// reacting to tui.SwitchNamespaceMsg either — and a no-op if already
// showing the requested namespace.
func (m *Model) switchNamespace(namespace string) tea.Cmd {
	if m.objectKind != "" || namespace == m.namespace {
		return nil
	}
	m.namespace = namespace
	m.selected = 0
	m.state = tui.TaskStateLoading
	m.feedback = "Loading events..."
	m.loadEpoch++
	return tea.Batch(m.load(), m.spinner.Tick)
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
	case "enter":
		if cmd, ok := m.openSelectedObject(); ok {
			return m, cmd
		}
	case verbs.YAML.Key:
		if task, cmd, ok := m.openSelectedYAML(); ok {
			return task, cmd
		}
	case verbs.ToggleGroup.Key:
		m.normalExpanded = !m.normalExpanded
		m.recomputeVisible()
	case verbs.WarningsOnly.Key:
		m.warningsOnly = !m.warningsOnly
		m.recomputeVisible()
	case verbs.TimeWindow.Key:
		m.cycleWindow()
	case verbs.AllNamespaces.Key:
		return m, m.switchNamespace("")
	case verbs.Filter.Key:
		if m.state == tui.TaskStateReady || m.state == tui.TaskStateEmpty {
			m.filterActive = true
			m.filterInput = textfield.New()
			m.filterInput.SetStyles(tui.TextInputStyles(m.Theme()))
			m.filterInput.Prompt = ""
			m.filterInput.Focus()
		}
	}
	return m, nil
}

func (m *Model) updateFilterKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filterActive = false
		m.filterInput.SetValue("")
		m.filterInput.Blur()
		m.recomputeVisible()
	// ctrl+j/ctrl+k are safe alongside plain j/k typing into the query — a
	// control-modified key never carries Text (charm.land/bubbletea/v2's
	// Key.Text doc), so it can't reach the default typing branch below.
	case "up", "ctrl+k":
		m.moveSelection(-1)
	case "down", "ctrl+j":
		m.moveSelection(1)
	case "ctrl+d":
		m.moveSelection(1)
	case "ctrl+u":
		m.moveSelection(-1)
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
	for i, w := range eventWindows {
		if w == m.window {
			idx = i
			break
		}
	}
	m.window = eventWindows[(idx+1)%len(eventWindows)]
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

// CapturingInput reports whether the filter box is open, so the root shell
// lets every keystroke reach events' own key handling (mirrors
// browse.CapturingInput).
func (m Model) CapturingInput() bool {
	return m.filterActive
}
