package objectdetail

import (
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
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
		// 26a's open panel re-reads its object (and Service joins) from the
		// cache this event just updated — see metapanel.Refresh.
		if m.meta != nil {
			m.meta.Refresh(msg.Kind, &m.actions)
		}
		// The object itself, plus Event: the EVENTS grid is read from a
		// cache this screen doesn't list, so it must reload on that kind
		// too or its rows go stale (CLAUDE.md: a screen reading a kind it
		// doesn't display must still reload on it).
		if (msg.Kind == m.kind || msg.Kind == kube.KindEvent) && m.lister != nil {
			return m, m.load()
		}
	case tui.CacheSyncRetryMsg:
		if msg.Gen == m.syncRetryGen && m.lister != nil {
			return m, m.load()
		}
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
		m.actions.SetOffline(m.conn.Offline())
	case loadedMsg:
		return m.applyLoaded(msg)
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
	case tea.KeyPressMsg:
		// Any key acknowledges the last action's outcome line; the key
		// still does what it would have.
		m.actions.DismissResult()
		return m.updateKey(msg)
	}
	return m, nil
}

// Transient implements tui.Transient: an instance that redirected to
// tasks/yamlview (applyLoaded, below) must not sit in the navigation stack
// under it — see the interface's doc comment.
func (m *Model) Transient() bool { return m.redirected }

// applyLoaded is load()'s result handler. When the object has neither
// conditions nor events, it redirects straight to tasks/yamlview instead of
// transitioning to ready (docs/design README.md §14d: "an empty detail
// screen is worse than the manifest") — root's generic task-swap plumbing
// (tui/model.go's sameTask check) makes this read as "↵ opened YAML"
// directly, with no empty-screen flash, since this Update call replaces the
// active task before any render of this frame happens. An events-fetch
// error is a distinct, non-empty state (unknown, not "no events") and never
// triggers the redirect.
func (m *Model) applyLoaded(msg loadedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.state = tui.TaskStateError
		if kube.IsPermissionError(msg.err) {
			m.state = tui.TaskStatePermissionDenied
		}
		m.feedback = msg.err.Error()
		return m, nil
	}
	if !msg.found {
		// "Not in the cache" only means deleted once that cache has filled
		// for this namespace — a sibling in a namespace this screen hasn't
		// read yet (scoped mode) or a cache still on its initial LIST would
		// otherwise read as a false "deleted" banner.
		if !tui.KindsSynced(m.lister, m.namespace, m.kind) {
			m.syncRetryGen++
			return m, tui.ScheduleCacheSyncRetry(m.syncRetryGen)
		}
		m.gone = true
		m.state = tui.TaskStateReady
		m.feedback = ""
		return m, nil
	}
	// The Event informer starts lazily on this screen's first read and
	// answers empty until its initial LIST lands — "no events" (and the
	// redirect it can trigger) is a claim about the cluster, so it waits
	// for that cache to fill. The retry covers an object with genuinely no
	// events, which syncs without ever emitting a change event.
	if len(msg.events) == 0 && msg.eventsErr == nil && m.events != nil && !tui.KindsSynced(m.lister, m.namespace, kube.KindEvent) {
		m.syncRetryGen++
		return m, tui.ScheduleCacheSyncRetry(m.syncRetryGen)
	}
	if len(msg.conditions) == 0 && len(msg.events) == 0 && msg.eventsErr == nil && m.openYAML != nil {
		task, cmd := m.openYAML(m.kind, m.namespace, m.name, m.width, m.height)
		if task != nil {
			m.redirected = true
			return task, cmd
		}
	}
	m.obj = msg.obj
	m.row = msg.row
	m.conditions = msg.conditions
	m.eventRows = msg.events
	m.eventsErr = msg.eventsErr
	m.found = true
	m.state = tui.TaskStateReady
	m.feedback = ""
	return m, nil
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.actions.Active() {
		return m.updateConfirmKey(msg)
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
		// "object gone ⇒ banner + auto-back after keypress" (mirrors
		// poddetail's own §5a behavior), except q/ctrl+c: quit is a global
		// expectation everywhere else in the app, and this banner shouldn't
		// be the one state where it silently does something else instead.
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
	case "j", "down":
		return m, m.moveSibling(1)
	case "k", "up":
		return m, m.moveSibling(-1)
	case verbs.YAML.Key:
		if task, cmd, ok := m.openSelectedYAML(); ok {
			return task, cmd
		}
	case verbs.Events.Key:
		if task, cmd, ok := m.openSelectedEvents(); ok {
			return task, cmd
		}
	case verbs.Delete.Key:
		return m, m.beginDelete()
	case verbs.Meta.Key:
		// 26a on the loaded object — the same guard browse's own 'm' uses
		// (mutator wired, object loaded), plus Editable since a synthetic
		// kind has no metadata to edit.
		if m.found && metapanel.Editable(m.kind) && m.mutator != nil && !verbs.Meta.HiddenWhileOffline(m.conn.Offline()) {
			if p, ok := metapanel.Open(metapanel.Config{Session: m.session, Lister: m.lister}, m.kind, m.namespace, m.name); ok {
				m.meta = p
			}
		}
	}
	return m, nil
}

// updateConfirmKey/updateModalConfirmKey mirror poddetail's own — TierModal
// (the type-the-name PROD modal) gets its own key handling; TierInline/
// TierNone stay the simple y/n/esc prompt. verbs.ForceDelete.Key escalates
// a pending delete of a kind ForceDelete applies to into the type-the-name
// modal (Controller.EscalateForceDelete — a no-op for any other kind).
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

// isProd mirrors poddetail's own — same source (7a's context palette PROD
// tag, internal/tui/context.go).
func (m Model) isProd() bool {
	if m.session == nil {
		return false
	}
	return m.session.Config.IsProd(m.session.Location.Context)
}

// moveSibling shifts to the next/prev object in browse's ordered list
// without leaving detail — mirrors poddetail's own moveSibling, generalized
// to any kind.
func (m *Model) moveSibling(delta int) tea.Cmd {
	if len(m.siblings) == 0 {
		return nil
	}
	next := m.siblingIndex + delta
	if next < 0 || next >= len(m.siblings) {
		return nil
	}
	m.siblingIndex = next
	m.namespace = m.siblings[next].Namespace
	m.name = m.siblings[next].Name
	m.gone = false
	m.found = false
	m.obj = nil
	m.row = resources.Row{}
	m.conditions = nil
	m.eventRows = nil
	m.eventsErr = nil
	m.state = tui.TaskStateLoading
	m.feedback = "Loading " + m.name + "..."
	if m.lister == nil {
		m.state = tui.TaskStateError
		m.feedback = "no cluster connection"
		return nil
	}
	return tea.Batch(m.load(), m.spinner.Tick)
}

// openSelectedYAML pushes 8a for the loaded object.
func (m Model) openSelectedYAML() (tea.Model, tea.Cmd, bool) {
	if m.openYAML == nil || !m.found {
		return nil, nil, false
	}
	task, cmd := m.openYAML(m.kind, m.namespace, m.name, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedEvents pushes 9b object-scoped for the loaded object.
func (m Model) openSelectedEvents() (tea.Model, tea.Cmd, bool) {
	if m.openEvents == nil || !m.found {
		return nil, nil, false
	}
	task, cmd := m.openEvents(m.kind, m.namespace, m.name, m.width, m.height)
	return task, cmd, task != nil
}

// beginDelete confirms deleting the object — inline y/N in non-prod
// contexts, the full type-the-name modal in PROD, always the modal for a
// cascade-scope kind (verbs.Resolve, the same resolver browse uses).
// Owner rides along for the modal's "will be recreated" line when the
// object has an ownerReference.
func (m *Model) beginDelete() tea.Cmd {
	if !m.found {
		return nil
	}
	return m.actions.Begin(actions.Request{Verb: verbs.Delete, Prod: m.isProd()}, tui.TaskAction{
		ID:    "delete-" + string(m.kind) + "-" + m.namespace + "/" + m.name,
		Label: "Delete " + singularDisplay(m.desc.Display) + " " + m.name + "?",
		Owner: firstOwnerRef(m.obj),
		Scope: tui.TaskScope{
			ResourceKind: string(m.kind),
			ResourceName: m.name,
			Namespace:    m.namespace,
			Verb:         "delete",
			IsMutating:   true,
		},
	})
}

// singularDisplay strips a trailing "s" off a Descriptor.Display plural
// (e.g. "certificates" -> "certificate") for the delete confirm's label —
// good enough for the regular CRD plurals kute never guesses columns for
// either; an irregular plural just reads slightly off, not wrong.
func singularDisplay(plural string) string {
	return strings.TrimSuffix(plural, "s")
}

// firstOwnerRef renders obj's first ownerReference as "Kind/Name" for the
// delete modal's "will be recreated" line — "" when there is none, same as
// poddetail's Owner field for a pod with no controller.
func firstOwnerRef(obj *unstructured.Unstructured) string {
	if obj == nil {
		return ""
	}
	refs := obj.GetOwnerReferences()
	if len(refs) == 0 {
		return ""
	}
	return refs[0].Kind + "/" + refs[0].Name
}
