package certchain

import (
	"fmt"
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
	return m.load()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ResourceChangedMsg:
		if m.reloadsOn(msg.Kind) {
			return m, m.load()
		}
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
		m.actions.SetOffline(m.conn.Offline())
	case tickMsg:
		m.now = time.Time(msg)
		return m, tickCmd()
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case loadedMsg:
		return m, m.applyLoaded(msg)
	case tui.CacheSyncRetryMsg:
		if msg.Gen == m.syncRetryGen && m.lister != nil {
			return m, m.load()
		}
	case actions.ResultMsg:
		m.actions.HandleResult(msg)
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

// reloadsOn reports whether a change to kind could change this screen —
// its own kind, any kind actually present in the resolved chain, or either
// ref's kind. Never a blanket "reload on anything," which would defeat the
// point of the narrow reads load() already does.
func (m Model) reloadsOn(kind kube.ResourceKind) bool {
	if kind == kube.KindCertificate || kind == kube.KindCertificateRequest {
		return true
	}
	for _, n := range m.chain {
		if n.Kind == kind {
			return true
		}
	}
	if m.haveSecret && (m.secretRef.Kind == kind || kind == kube.KindTLSSecret) {
		// The ref is read from the filtered TLS cache when the lister has
		// one, which reports its changes under its own key.
		return true
	}
	if m.haveIssuer && m.issuerRef.Kind == kind {
		return true
	}
	return false
}

func (m *Model) applyLoaded(msg loadedMsg) tea.Cmd {
	if msg.err != nil {
		m.state, m.feedback = tui.TaskStateError, msg.err.Error()
		if kube.IsPermissionError(msg.err) {
			m.state = tui.TaskStatePermissionDenied
		}
		return nil
	}
	var retry tea.Cmd
	if msg.pending {
		m.syncRetryGen++
		retry = tui.ScheduleCacheSyncRetry(m.syncRetryGen)
	}
	if msg.gone && msg.pending {
		// The Certificate cache is still filling: not gone, not yet here.
		return retry
	}
	if msg.gone {
		m.state = tui.TaskStateError
		m.feedback = m.name + " no longer exists · esc to go back"
		return nil
	}
	m.fail, m.chain = msg.fail, msg.chain
	m.secretRef, m.issuerRef = msg.secretRef, msg.issuerRef
	m.haveSecret, m.haveIssuer = msg.haveSecret, msg.haveIssuer
	m.attempts = msg.attempts
	m.chainNote = msg.chainNote
	m.state = tui.TaskStateReady
	if strings.HasPrefix(m.feedback, "loading ") {
		m.feedback = ""
	}
	if n := m.selectableCount(); m.selected >= n {
		m.selected = max(0, n-1)
	}
	// Default the cursor onto the deepest failure — "zero digs," the same
	// rule fluxdetail's inventory sort gives ↵ for free by putting the
	// failing entry first. Here the chain's order is already fixed (it's a
	// walk, not a sortable list), so the cursor is placed explicitly
	// instead.
	if m.fail != nil {
		for i, n := range m.chain {
			if n.Kind == m.fail.Kind && n.Name == m.fail.Name {
				m.selected = i
				break
			}
		}
	}
	return retry
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.actions.Active() {
		return m.updateConfirmKey(msg)
	}
	switch msg.String() {
	case "esc", "backspace":
		return m, func() tea.Msg { return tui.BackMsg{} }
	case "up", "k":
		m.moveSelection(-1)
	case "down", "j":
		m.moveSelection(1)
	case "enter":
		if kind, name, ok := m.selectedTarget(); ok {
			ns := m.namespace
			return m, func() tea.Msg {
				return tui.GotoResourceMsg{Kind: kind, Namespace: ns, Name: name}
			}
		}
	case verbs.YAML.Key:
		if m.openYAML != nil {
			task, cmd := m.openYAML(kube.KindCertificate, m.namespace, m.name, m.width, m.height)
			if task != nil {
				return task, cmd
			}
		}
	case verbs.Events.Key:
		if m.openEvents != nil {
			task, cmd := m.openEvents(kube.KindCertificate, m.namespace, m.name, m.width, m.height)
			if task != nil {
				return task, cmd
			}
		}
	case verbs.CertRenew.Key:
		if m.mutator != nil && m.state == tui.TaskStateReady && !m.conn.Offline() {
			return m, m.beginCertRenew()
		}
	}
	return m, nil
}

func (m *Model) updateConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y":
		return m, m.actions.Confirm()
	case "n", "esc":
		m.actions.Cancel()
	}
	return m, nil
}

// beginCertRenew starts verbs.CertRenew ('r') against the Certificate this
// screen is about — one inline y/N that PROD never escalates (verbs.CertRenew's
// policy row; tasks/browse/certmanager.go's own beginCertRenew has the full
// reasoning).
func (m *Model) beginCertRenew() tea.Cmd {
	return m.actions.Begin(actions.Request{Verb: verbs.CertRenew, Prod: m.isProd()}, tui.TaskAction{
		ID:    "cert-renew-" + m.namespace + "/" + m.name,
		Label: fmt.Sprintf("Renew %s?", m.name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCertificate), ResourceName: m.name,
			Namespace: m.namespace, Verb: "cert-renew", IsMutating: true,
		},
	})
}

// certRenewWillRunLine is beginCertRenew's confirm "will run: ..." line —
// same command tasks/browse/certmanager.go's own certRenewWillRunLine
// renders, duplicated per the repo's package-local-seam convention.
func certRenewWillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.RenewCertificateCommandString(scope.Namespace, scope.ResourceName)
}

func (m *Model) moveSelection(delta int) {
	n := m.selectableCount()
	if n == 0 {
		m.selected = 0
		return
	}
	m.selected = min(max(0, m.selected+delta), n-1)
}
