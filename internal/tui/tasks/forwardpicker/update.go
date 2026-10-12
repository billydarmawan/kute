package forwardpicker

import (
	"errors"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// pasteTarget is the open buffer on the row being edited, under that buffer's
// own value rule: digit-gated for a local port, kubectl's `local:remote`
// spelling for the free-port row's remote half. CharLimit applies on top in
// both cases, so a paste too long for the field is truncated rather than
// making a nonsense port.
func (m *Model) pasteTarget() tui.PasteTarget {
	if !m.editing() {
		// A paste onto the free-port row opens its buffer, the same as a typed
		// digit does — otherwise a pasted `8080:3000` would need a throwaway
		// digit first, and land as `18080:3000`.
		if m.state == tui.TaskStateReady && m.row().manual {
			return tui.PastePortPair(func(s string) {
				m.beginEdit("", true)
				row := &m.rows[m.selected]
				row.editInput.SetValue(s)
				row.editInput.CursorEnd()
				m.liveRemote(row, false)
			})
		}
		return nil
	}
	into := tui.PasteInto(&m.rows[m.selected].editInput)
	if m.rows[m.selected].editRemote {
		return tui.PastePortPair(into)
	}
	return tui.PasteDigits(into)
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
	held := m.heldLocalPorts()
	m.rows = make([]portRow, 0, len(msg.ports)+1)
	for i, p := range msg.ports {
		chosen, busyFrom := pickLocalPort(preferredLocalPort(p.Port), held)
		row := portRow{PortOption: p, localPort: chosen, busyFrom: busyFrom}
		if i < len(msg.podPorts) {
			row.podPort = msg.podPorts[i]
		}
		m.rows = append(m.rows, row)
	}
	// The free-entry row is unconditional, and always last: a declared port
	// is a hint, never a limit, so there is no state in which this panel
	// can't start a forward — including a pod that declares no ports at all.
	m.rows = append(m.rows, portRow{manual: true})
	m.state = tui.TaskStateReady
	m.feedback = ""
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// A refusal from the last keypress ("type a port to forward") is stale the
	// moment another key lands — the same rule the shared result line follows.
	if m.state == tui.TaskStateReady {
		m.feedback = ""
	}
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
		// Typing a digit on a selected row starts an in-place port edit
		// (docs/design README.md §13a: "local port is edited in place on the
		// selected row"). Which half it types is the row's to say: the local
		// port on a declared row, whose remote half is already known, and the
		// remote port on the free-entry row, which is the half that isn't.
		if m.state == tui.TaskStateReady && verbs.ForwardLocalPort.Matches(msg.Text) {
			m.beginEdit(msg.Text, m.row().manual)
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

func (m *Model) beginEdit(firstDigit string, remote bool) {
	row := &m.rows[m.selected]
	seed := ""
	if remote {
		saved := *row
		row.beforeEdit = &saved
		// Typing onto a free-port row that already holds a port edits it,
		// the way any text field does — a fresh buffer would throw away a
		// committed `9000:3000` (and its local override) on the first digit.
		seed = manualSpelling(*row)
	}
	row.editing = true
	row.editRemote = remote
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
	if remote {
		row.editInput.CharLimit = portPairLimit
	}
	row.editInput.SetValue(seed + firstDigit)
	row.editInput.CursorEnd()
	row.editInput.Focus()
	m.liveRemote(row, false)
}

// manualSpelling is the free-port row's committed port as its buffer spells
// it: `local:remote` when the local half was chosen, else the remote alone.
func manualSpelling(row portRow) string {
	switch {
	case row.Port == 0:
		return ""
	case row.localEdited:
		return strconv.Itoa(row.localPort) + ":" + strconv.Itoa(int(row.Port))
	default:
		return strconv.Itoa(int(row.Port))
	}
}

func (m *Model) updateEditKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	row := &m.rows[m.selected]
	switch msg.String() {
	case "esc":
		if row.beforeEdit != nil {
			// The free-port buffer drove the row live; cancelling it puts back
			// exactly what was there before it opened.
			*row = *row.beforeEdit
			return m, nil
		}
		row.editing = false
		row.editRemote = false
		row.editInput.Blur()
	case "enter":
		m.commitEdit()
		return m, m.startSelected()
	default:
		// Digits only, matching this field's port-number semantics, plus the
		// one colon the free-port row's `local:remote` buffer allows — a
		// keypress carrying any other rune is dropped rather than forwarded,
		// and everything else (backspace, left/right, Home/End, Ctrl-arrow
		// word-jump) reaches the text field. A pasted value gets the same rule
		// from pasteTarget's tui.PasteDigits/PastePortPair.
		for _, r := range msg.Text {
			if r == ':' {
				// One separator, after the local half — never a second one,
				// and never leading, since neither spells a port pair.
				if row.editRemote && row.editInput.Value() != "" && !strings.Contains(row.editInput.Value(), ":") {
					continue
				}
				return m, nil
			}
			if r < '0' || r > '9' {
				return m, nil
			}
		}
		var cmd tea.Cmd
		row.editInput, cmd = row.editInput.Update(msg)
		m.liveRemote(row, false)
		return m, cmd
	}
	return m, nil
}

// liveRemote keeps the free-port row in step with its buffer as it is typed:
// the remote port it names is what the forward will dial, so the will-run line
// and the local port follow the keystrokes rather than waiting for the enter
// that commits them. A left half (`8080:3000`) is the user's own local port and
// is taken as given — including that it may be busy, exactly like a local port
// typed onto a declared row — while a bare number lets deriveLocal pre-fill.
func (m *Model) liveRemote(row *portRow, probe bool) {
	if !row.editRemote {
		return
	}
	local, remote, err := parsePortPair(row.editInput.Value())
	row.pairErr = ""
	if err != nil {
		row.pairErr = err.Error()
	}
	m.setManualRemote(row, remote)
	row.localEdited = local > 0
	if local > 0 {
		row.localPort, row.busyFrom = local, 0
	}
	m.deriveLocal(row, probe)
}

// setManualRemote points the free-port row at remote. On a Service that's a
// Service port, the way kubectl reads `svc/<name> local:remote`: a port the
// Service declares takes that port's targetPort mapping, exactly as its own
// row would. A port it doesn't declare has no mapping to borrow, so it is
// dialled on the backing pod directly — the will-run line then names the pod,
// not the Service, so it never claims kubectl's svc/ meaning.
func (m *Model) setManualRemote(row *portRow, remote int) {
	row.PortOption, row.podPort = kube.PortOption{Port: int32(remote)}, int32(remote)
	if m.target.Kind != kube.KindService || remote == 0 {
		return
	}
	for _, declared := range m.rows {
		if !declared.manual && declared.Port == int32(remote) {
			row.PortOption, row.podPort = declared.PortOption, declared.podPort
			return
		}
	}
}

// commitEdit parses the in-progress edit buffer into the row's port state and
// clears editing state. A local port that doesn't parse leaves the pre-fill in
// place; a free-port buffer that parses to nothing leaves the row with no port
// at all, which startSelected then refuses with an inline note.
func (m *Model) commitEdit() {
	row := &m.rows[m.selected]
	if row.editRemote {
		// The free-port row's buffer is its whole port state, so committing it
		// is the same read the keystrokes were already doing live — plus the
		// bind probe they skipped.
		m.liveRemote(row, true)
	} else if n := parsePort(row.editInput.Value()); n > 0 {
		row.localPort = n
		row.localEdited = true
		row.busyFrom = 0
	}
	row.editing = false
	row.editRemote = false
	row.beforeEdit = nil
	row.editInput.Blur()
}

// parsePort reads a port number, 0 for anything that isn't one: empty, a
// non-digit, or past 65535.
func parsePort(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 65535 || strings.ContainsAny(s, "+-") {
		return 0
	}
	return n
}

// parsePortPair reads the free-port row's buffer, which may be kubectl's own
// `local:remote` spelling: one half is the remote port with the local derived
// from it, two halves set both. Either half that isn't a usable port number
// reads as 0 — a half-typed "8080:" never becomes a forward's target — and a
// half typed out of range says so, rather than reading as untyped or quietly
// falling back to the pre-fill.
func parsePortPair(s string) (local, remote int, err error) {
	l, r, paired := strings.Cut(s, ":")
	if !paired {
		l, r = "", s
	}
	if outOfRange(l) || outOfRange(r) {
		return 0, 0, errPortRange
	}
	return parsePort(l), parsePort(r), nil
}

var errPortRange = errors.New("port must be 1-65535")

func outOfRange(s string) bool {
	return s != "" && parsePort(s) == 0
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
	// A free-port row with nothing typed in it has no port to dial. Saying so
	// beats starting a forward on port 0 that could never carry a byte. A
	// Service/Deployment with no backing pod yet is *not* refused: the
	// manager's reconnect loop re-resolves the pod each retry and connects
	// once one is Ready.
	if row.manual && row.pairErr != "" {
		m.feedback = row.pairErr
		return nil
	}
	if row.manual && row.Port == 0 {
		m.feedback = "type a port to forward"
		return nil
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
