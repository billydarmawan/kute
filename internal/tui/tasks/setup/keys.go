package setup

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.editing {
		return m.updateEditKey(msg)
	}
	switch msg.String() {
	case verbs.Quit.Key, "ctrl+c":
		return m, tea.Quit
	case verbs.Retry.Key:
		return m.doRetry("")
	case "up":
		if m.state == Unreachable {
			m.moveSwitchSelection(-1)
		}
	case "down":
		if m.state == Unreachable {
			m.moveSwitchSelection(1)
		}
	case verbs.SetupEnterPath.Key:
		switch m.state {
		case NoConfig:
			m.startEdit()
		case Unreachable:
			// 4c's SWITCH CONTEXT list: j/k ≡ ↑↓ (CLAUDE.md convention),
			// distinct from NoConfig's own 'k' = "enter kubeconfig path".
			m.moveSwitchSelection(-1)
		}
	case "j":
		if m.state == Unreachable {
			m.moveSwitchSelection(1)
		}
	case "enter":
		if m.state == Unreachable {
			if cmd, ok := m.connectToSelected(); ok {
				return m, cmd
			}
		}
	case verbs.SetupEditPath.Key:
		if m.state == Unreachable {
			m.startEdit()
		}
	}
	return m, nil
}

func (m *Model) startEdit() {
	m.editing = true
	m.pathInput = textfield.New()
	m.pathInput.SetStyles(tui.TextInputStyles(m.Theme()))
	m.pathInput.Prompt = ""
	m.pathInput.SetValue(m.kubeconfigPath)
	m.syncPathInputWidth()
	m.pathInput.CursorEnd()
	m.pathInput.Focus()
	m.retryErr = nil
}

// syncPathInputWidth gives the buffer the same text column editLines renders
// it into (blockWidth, less the two-space indent), so a value longer than the
// block — which is what a pasted path usually is — scrolls horizontally with
// the cursor in view instead of being truncated at the frame's edge.
func (m *Model) syncPathInputWidth() {
	m.pathInput.SetWidth(max(blockWidth(m.width)-2, 8))
}

// doRetry is 'r”s plain retry (path=="") and the edit input's submit
// (path==typed value): Unreachable's plain retry re-probes the existing
// cluster in place (RetryNow, no rebuild); every other case — NoConfig's
// 'r'/'k', or Unreachable with an edited path — rebuilds via Reconnect.
func (m *Model) doRetry(path string) (tea.Model, tea.Cmd) {
	if m.retrying {
		// One rebuild at a time: a second would race the first to replace
		// the root, and the loser's cluster would run for the process's life.
		return m, nil
	}
	switch {
	case path == "" && m.state == Unreachable && m.retryNow != nil:
		m.retryNow()
		// The siblings' reachability is as stale as the current context's:
		// re-probe them under a fresh gen, so the previous run's drain can't
		// land on top of this one's results.
		m.probeGen++
		return m, probeSwitchContextsCmd(m.session.Context(), m.probeGen, m.otherContexts)
	case m.reconnect != nil:
		m.retrying = true
		m.retryErr = nil
		return m, m.reconnect(path)
	}
	return m, nil
}

func (m *Model) updateEditKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.editing = false
		m.pathInput.Blur()
	case "enter":
		m.editing = false
		m.pathInput.Blur()
		// Trimmed because a pasted path arrives with whatever whitespace the
		// source had around it (textinput collapses its newlines to spaces),
		// and " /path/to/config " is not a path.
		return m.doRetry(strings.TrimSpace(m.pathInput.Value()))
	default:
		var cmd tea.Cmd
		m.pathInput, cmd = m.pathInput.Update(msg)
		return m, cmd
	}
	return m, nil
}
