package update

import (
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// openedBrowserMsg carries m.openBrowser's result back through Update
// (wrapped in a tea.Cmd rather than called synchronously from a key press,
// matching how every other I/O in this codebase — however fast — runs
// off-thread).
type openedBrowserMsg struct{ err error }

func openBrowserCmd(open func(string) error, url string) tea.Cmd {
	return func() tea.Msg { return openedBrowserMsg{err: open(url)} }
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
	case tui.UpdateCheckedMsg:
		m.checking = false
		if msg.Err != nil {
			m.feedback = "check failed — offline?"
		} else {
			m.feedback = ""
		}
	case openedBrowserMsg:
		if msg.err != nil {
			m.feedback = "couldn't open a browser"
		} else {
			m.feedback = "opened in browser"
		}
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	}
	return m, nil
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+q", "ctrl+c":
		return m, tea.Quit
	case "esc", "backspace":
		return m, func() tea.Msg { return tui.BackMsg{} }
	case verbs.UpdateCopyCommand.Key:
		if info, ok := m.info(); ok {
			if _, avail := m.available(); avail {
				m.feedback = "command copied"
				return m, tea.SetClipboard(info.Install.Command)
			}
		}
	case verbs.UpdateNotes.Key:
		if release, ok := m.available(); ok && release.HTMLURL != "" && m.openBrowser != nil {
			return m, openBrowserCmd(m.openBrowser, release.HTMLURL)
		}
	case verbs.UpdateSkip.Key:
		if release, ok := m.available(); ok && m.session != nil {
			m.session.State.MarkUpdateSeen(release.Version)
			m.feedback = release.Version + " skipped"
		}
	case verbs.UpdateRecheck.Key:
		if m.state() == tui.TaskStateEmpty && !m.checkDisabled() && m.recheck != nil {
			m.checking = true
			m.feedback = "checking…"
			return m, m.recheck()
		}
	}
	return m, nil
}
