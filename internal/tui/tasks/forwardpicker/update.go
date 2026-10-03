package forwardpicker

import (
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// pasteTarget is the local-port buffer of the row being edited. Digit-gated
// like the typed path: CharLimit 5 still applies on top, so a paste that's
// too long is truncated rather than making a nonsense port.
func (m *Model) pasteTarget() tui.PasteTarget {
	if !m.editing() {
		return nil
	}
	return tui.PasteDigits(tui.PasteInto(&m.rows[m.selected].editInput))
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cmd, ok := tui.RoutePaste(msg, m.pasteTarget()); ok {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
	case portsLoadedMsg:
		m.applyPortsLoaded(msg)
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	}
	return m, nil
}

func (m *Model) applyPortsLoaded(msg portsLoadedMsg) {
	if msg.err != nil {
		m.state = tui.TaskStateError
		m.feedback = msg.err.Error()
		return
	}
	m.resolvedPod = msg.resolvedPod
	m.rows = make([]portRow, len(msg.ports))
	held := m.heldLocalPorts()
	for i, p := range msg.ports {
		chosen, busyFrom := pickLocalPort(preferredLocalPort(p.Port), held)
		m.rows[i] = portRow{PortOption: p, localPort: chosen, busyFrom: busyFrom}
		if i < len(msg.podPorts) {
			m.rows[i].podPort = msg.podPorts[i]
		}
	}
	if len(m.rows) == 0 {
		m.state = tui.TaskStateEmpty
		m.feedback = "no forwardable ports found"
		return
	}
	m.state = tui.TaskStateReady
	m.feedback = ""
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.editing() {
		return m.updateEditKey(msg)
	}
	switch msg.String() {
	case "ctrl+q", "ctrl+c":
		return m, tea.Quit
	case "esc":
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
		return m, m.startSelected()
	default:
		// Typing a digit on a selected row starts an in-place local-port
		// edit (docs/design README.md §13a: "local port is edited in place
		// on the selected row").
		if m.state == tui.TaskStateReady && len(msg.Text) == 1 && msg.Text[0] >= '0' && msg.Text[0] <= '9' {
			m.beginEdit(msg.Text)
		}
	}
	return m, nil
}

func (m Model) editing() bool {
	if m.selected < 0 || m.selected >= len(m.rows) {
		return false
	}
	return m.rows[m.selected].editing
}

func (m *Model) beginEdit(firstDigit string) {
	row := &m.rows[m.selected]
	row.editing = true
	row.editInput = textfield.New()
	// Editing only ever happens on the selected row, so the typed digits
	// render on the same SelBg highlight as the rest of the row — the same
	// text-field-on-SelBg treatment tasks/secretdata applies (secretdata
	// model.go).
	styles := tui.TextInputStyles(m.Theme())
	styles.Focused.Text = styles.Focused.Text.Background(m.Theme().SelBg)
	row.editInput.SetStyles(styles)
	row.editInput.Prompt = ""
	row.editInput.CharLimit = 5
	row.editInput.SetValue(firstDigit)
	row.editInput.CursorEnd()
	row.editInput.Focus()
}

func (m *Model) updateEditKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	row := &m.rows[m.selected]
	switch msg.String() {
	case "esc":
		row.editing = false
		row.editInput.Blur()
	case "enter":
		m.commitEdit()
		return m, m.startSelected()
	default:
		// Digits only, matching this field's port-number semantics — any
		// keypress whose Text carries a non-digit rune is dropped rather than
		// forwarded, everything else (backspace, left/right, Home/End,
		// Ctrl-arrow word-jump) reaches the text field. A pasted value gets the
		// same rule from pasteTarget's tui.PasteDigits.
		for _, r := range msg.Text {
			if r < '0' || r > '9' {
				return m, nil
			}
		}
		var cmd tea.Cmd
		row.editInput, cmd = row.editInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

// commitEdit parses the in-progress edit buffer into the row's local port
// (ignoring an empty/zero result, which leaves the previous port in place)
// and clears editing state.
func (m *Model) commitEdit() {
	row := &m.rows[m.selected]
	if n := parsePort(row.editInput.Value()); n > 0 {
		row.localPort = n
		row.busyFrom = 0
	}
	row.editing = false
	row.editInput.Blur()
}

func parsePort(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	if n > 65535 {
		return 0
	}
	return n
}

func (m *Model) moveSelection(delta int) {
	if len(m.rows) == 0 {
		return
	}
	m.selected = clamp(m.selected+delta, 0, len(m.rows)-1)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// startSelected starts the forward for the highlighted port and pops back
// to whichever screen pushed the picker — starting runs in the background
// (kube.ForwardManager.Start), unlike exec/node-shell this never suspends
// the program.
func (m *Model) startSelected() tea.Cmd {
	if m.selected < 0 || m.selected >= len(m.rows) || m.manager == nil {
		return nil
	}
	row := m.rows[m.selected]
	if row.editing {
		m.commitEdit()
		row = m.rows[m.selected]
	}
	// A local port another forward owns would leave this one looping in
	// Reconnecting forever, never able to bind: bump it the same way the
	// pre-fill does and stay, so the row says so before anything starts.
	if held := m.heldLocalPorts(); held[row.localPort] {
		chosen, busyFrom := pickLocalPort(row.localPort, held)
		if busyFrom != 0 {
			m.rows[m.selected].localPort, m.rows[m.selected].busyFrom = chosen, busyFrom
			return nil
		}
	}
	// The whole PortOption goes through, not just Port: for a Service the
	// manager dials the targetPort resolved against the backing pod, never
	// the Service port itself.
	m.manager.StartPort(m.dialer, m.resolver, m.target, m.resolvedPod, row.localPort, row.PortOption)
	return func() tea.Msg { return tui.BackMsg{} }
}
