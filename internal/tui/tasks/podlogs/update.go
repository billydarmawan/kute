package podlogs

import (
	"fmt"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// pasteTarget is the '/' filter buffer while it's open, clamping the
// viewport inside the closure exactly as updateFilterKey does after a typed
// character.
func (m *Model) pasteTarget() tui.PasteTarget {
	if !m.filterActive {
		return nil
	}
	insert := tui.PasteInto(&m.filterInput)
	return func(s string) {
		insert(s)
		m.clampOffsets()
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cmd, ok := tui.RoutePaste(msg, m.pasteTarget()); ok {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case streamStartedMsg:
		m.stream = msg.state
		m.feedback = ""
	case containerReadyMsg:
		if msg.streamID != 0 && msg.streamID != m.streamID {
			return m, nil
		}
		m.crashLoopReason = msg.crashLoop
		return m, m.connect(m.streamID)
	case containerWaitingMsg:
		if msg.streamID != 0 && msg.streamID != m.streamID {
			return m, nil
		}
		m.stream = StreamWaitingForContainer
		m.waitingReason = msg.reason
		m.feedback = "waiting for container to start: " + msg.reason
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
	case kube.ResourceChangedMsg:
		if msg.Kind == kube.KindPod && m.stream == StreamWaitingForContainer {
			return m, m.checkContainerCmd(m.streamID)
		}
	case spinner.TickMsg:
		if m.taskState() != tui.TaskStateLoading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case logLineMsg:
		if msg.streamID != 0 && msg.streamID != m.streamID {
			return m, m.nextStreamCmd()
		}
		m.appendEntry(msg.entry)
		return m, m.nextStreamCmd()
	case logBatchMsg:
		// One render for the whole burst — see waitForStream. Each entry still
		// goes through appendEntry, so the incremental scroll/eviction
		// bookkeeping is identical to arriving one line at a time.
		if msg.streamID == 0 || msg.streamID == m.streamID {
			for _, entry := range msg.entries {
				m.appendEntry(entry)
			}
		}
		if msg.tail != nil {
			return m.Update(msg.tail)
		}
		return m, m.nextStreamCmd()
	case streamEmptyMsg:
		if msg.streamID != 0 && msg.streamID != m.streamID {
			return m, m.nextStreamCmd()
		}
		m.stream = StreamEmpty
		m.feedback = fmt.Sprintf("No logs found for %s.", m.scope())
	case streamErrorMsg:
		if msg.streamID != 0 && msg.streamID != m.streamID {
			return m, m.nextStreamCmd()
		}
		m.stream = StreamError
		m.lastError = msg.err.Error()
		m.permDenied = kube.IsPermissionError(msg.err)
		m.feedback = m.lastError
		if m.permDenied {
			m.feedback = "Permission denied reading logs for " + m.scope() + ": " + msg.err.Error()
		}
	case streamClosedMsg:
		if msg.streamID != 0 && msg.streamID != m.streamID {
			return m, nil
		}
		m.stream = StreamClosed
		m.feedback = "Log stream closed."
	case streamWaitMsg:
		return m, nil
	case rateTickMsg:
		if msg.gen != m.rateGen {
			return m, nil // stale generation from a since-superseded beginStream — drop, don't reschedule
		}
		m.lastRate = m.linesSinceTick
		m.linesSinceTick = 0
		return m, rateTickCmd(m.rateGen)
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.filterActive {
		return m.updateFilterKey(msg)
	}

	switch msg.String() {
	case "ctrl+q", "ctrl+c":
		m.cancelStream()
		m.streamID++ // invalidate any in-flight container check/ResourceChangedMsg re-check
		m.stream = StreamClosed
		m.feedback = "Log stream closed."
		return m, tea.Quit
	case "esc":
		m.cancelStream()
		m.streamID++ // invalidate any in-flight container check/ResourceChangedMsg re-check
		m.stream = StreamClosed
		m.feedback = "Log stream closed."
		return m, func() tea.Msg { return tui.BackMsg{} }
	case "j", "down":
		m.moveVertical(1)
	case "k", "up":
		m.moveVertical(-1)
	case "h", "left":
		m.moveHorizontal(-1)
	case "l", "right":
		m.moveHorizontal(1)
	case "home":
		m.view.VerticalOffset = 0
		m.view.AutoScroll = false
	case "G", "end":
		m.view.AutoScroll = false
		m.view.VerticalOffset = m.maxVerticalOffset()
	case "pgdown", "ctrl+f":
		m.moveVertical(m.entryViewportHeight())
	case "pgup", "ctrl+b":
		m.moveVertical(-m.entryViewportHeight())
	case "ctrl+d":
		m.moveVertical(max(1, m.entryViewportHeight()/2))
	case "ctrl+u":
		m.moveVertical(-max(1, m.entryViewportHeight()/2))
	case verbs.LogPause.Key:
		m.view.AutoScroll = !m.view.AutoScroll
	case verbs.LogToggleWrap.Key:
		m.view.Wrap = !m.view.Wrap
		if m.view.Wrap {
			m.view.HorizontalOffset = 0
		}
	case verbs.LogToggleTime.Key:
		m.view.Timestamps = !m.view.Timestamps
	case verbs.LogCycleContainer.Key:
		m.cycleContainer()
		return m, m.beginStream(StreamReconnecting)
	case verbs.LogCycleSince.Key:
		m.cycleSince()
		return m, m.beginStream(StreamReconnecting)
	case verbs.LogNextWarning.Key:
		m.jumpSeverity(SeverityWarn)
	case verbs.LogNextError.Key:
		m.jumpSeverity(SeverityErr)
	case verbs.Filter.Key:
		if m.stream != StreamLoading {
			m.filterActive = true
			m.filterInput = textfield.New()
			m.filterInput.SetStyles(tui.TextInputStyles(m.Theme()))
			m.filterInput.Prompt = ""
			m.filterInput.Focus()
		}
	case verbs.LogCopyView.Key:
		return m, tea.SetClipboard(m.visibleViewText())
	}
	m.clampOffsets()
	return m, nil
}

func (m *Model) updateFilterKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filterActive = false
		m.filterInput.SetValue("")
		m.filterInput.Blur()
		m.clampOffsets()
	// ctrl+j/k are safe alongside plain j/k typing into the query —
	// a control-modified key never carries Text (charm.land/bubbletea/v2's
	// Key.Text doc), so it can't reach the default typing branch below.
	case "up", "ctrl+k":
		m.moveVertical(-1)
	case "down", "ctrl+j":
		m.moveVertical(1)
	case "alt+h":
		m.moveHorizontal(-1)
	case "alt+l":
		m.moveHorizontal(1)
	default:
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		m.clampOffsets()
		return m, cmd
	}
	return m, nil
}

func (m *Model) moveVertical(delta int) {
	m.view.AutoScroll = false
	m.view.VerticalOffset += delta
	m.clampOffsets()
}

func (m *Model) moveHorizontal(delta int) {
	if m.view.Wrap {
		return
	}
	m.view.HorizontalOffset += delta
	if m.view.HorizontalOffset < 0 {
		m.view.HorizontalOffset = 0
	}
}

// jumpSeverity moves the viewport to the first physical row of the next
// matching entry after the current top-of-view position, wrapping to the
// start. Continuation rows are skipped so one long wrapped warning cannot
// consume several presses before navigation advances to the next warning.
func (m *Model) jumpSeverity(severity string) {
	m.syncLayout()
	// Walking the row index rather than laying the buffer out: the target is
	// always an entry's *first* row, which the index already locates.
	next, first, offset := -1, -1, 0
	for i, rows := range m.rowCounts {
		if rows == 0 {
			continue
		}
		if m.buffer.Entries[i].Severity == severity {
			if first < 0 {
				first = offset
			}
			if next < 0 && offset > m.view.VerticalOffset {
				next = offset
			}
		}
		offset += rows
	}
	if next < 0 {
		next = first // wrap to the earliest match, as scanning past the end did
	}
	if next < 0 {
		return
	}
	m.view.AutoScroll = false
	m.view.VerticalOffset = next
	m.clampOffsets()
}

// CapturingInput reports whether the '/' filter input is open, so the root
// shell lets every keystroke reach podlogs' own key handling instead of
// treating letters as global g/n/c/? shortcuts (mirrors browse.
// CapturingInput).
func (m Model) CapturingInput() bool {
	return m.filterActive
}
