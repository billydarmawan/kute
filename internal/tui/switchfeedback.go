package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/tui/components"
)

// 7a's feedback while a context switch runs and after one fails (docs/
// design README.md §7a). Neither belongs to a task: the switch is the
// root's own tea.Cmd, and the screen underneath keeps rendering the context
// being left until it lands. So the root overrides two bands of whatever
// Screen is active — the header's connection badge while the switch is in
// flight, and the keybar's result line once one fails — the same splice
// over the task's own frame that 2b's GOTO keybar already uses.

// switchingText is the header badge's label while a 7a switch is in flight.
const switchingText = "switching…"

// beginSwitchFeedback marks a 7a switch as in flight and starts the badge's
// spinner. The spinner is built once and reused: a second switch started
// before the first chain's last tick lands re-arms the same tag, which the
// spinner itself de-duplicates.
func (m *Model) beginSwitchFeedback() tea.Cmd {
	m.switching = true
	m.rootResult = ResultLine{}
	if m.switchSpinner.ID() == 0 {
		m.switchSpinner = components.NewSpinner()
	}
	return m.switchSpinner.Tick
}

// tickSwitchSpinner advances the badge's spinner. ok is false for a tick
// that isn't the root's own (it carries on to the active task); the chain
// stops by itself once the switch has landed.
func (m *Model) tickSwitchSpinner(msg spinner.TickMsg) (tea.Cmd, bool) {
	if m.switchSpinner.ID() == 0 || msg.ID != m.switchSpinner.ID() {
		return nil, false
	}
	if !m.switching {
		return nil, true
	}
	var cmd tea.Cmd
	m.switchSpinner, cmd = m.switchSpinner.Update(msg)
	return cmd, true
}

// switchingBadge is the connection badge while a 7a switch is in flight:
// the shared loading spinner and the "◌ probing…" status's Warn tone — the
// palette's own word for a context whose reachability isn't known yet.
func switchingBadge(theme Theme, sp spinner.Model) ConnBadge {
	return ConnBadge{
		Text:  sp.View() + " " + switchingText,
		Style: lipgloss.NewStyle().Foreground(theme.Warn),
	}
}

// switchFailureResult is a finished 7a switch's keybar line: empty on
// success, otherwise the reason it didn't happen.
func switchFailureResult(msg SwitchContextMsg) ResultLine {
	if msg.Err == nil {
		return ResultLine{}
	}
	return ResultLine{Text: switchFailureText(msg.Context, msg.Err), Failed: true}
}

// switchFailureText words a failed or refused switch to target.
func switchFailureText(target string, err error) string {
	if target == "" {
		return fmt.Sprintf("couldn't switch context: %v", err)
	}
	return fmt.Sprintf("couldn't switch to %s: %v", target, err)
}

// spliceSwitchFeedback overrides the active Screen's header badge and
// keybar result line in content (the task's own rendered frame) with the
// root's switch feedback. A task that isn't a Screen draws no chrome, and
// is left alone.
func (m Model) spliceSwitchFeedback(content string, width int) string {
	screen, ok := m.task.(Screen)
	if !ok || (!m.switching && m.rootResult.Empty()) {
		return content
	}
	theme := screen.Theme()
	if m.switching {
		header := screen.Header()
		header.Conn = switchingBadge(theme, m.switchSpinner)
		content = replaceFirstLines(content, renderHeaderV2(header, theme, width))
	}
	if !m.rootResult.Empty() {
		kb := screen.Keybar()
		kb.Result = m.rootResult
		content = replaceLastLine(content, renderKeybarV2(kb, theme, width))
	}
	return content
}

// replaceFirstLines swaps content's leading len(lines) lines for lines —
// the header band is always exactly Frame's first frameHeaderLines lines.
func replaceFirstLines(content string, lines []string) string {
	rest := content
	for range lines {
		idx := strings.IndexByte(rest, '\n')
		if idx < 0 {
			return strings.Join(lines, "\n")
		}
		rest = rest[idx+1:]
	}
	return strings.Join(lines, "\n") + "\n" + rest
}
