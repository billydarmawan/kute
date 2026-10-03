package setup

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// RetryFailedMsg reports a failed Reconnect attempt: setup stays on screen
// (the root shell only swaps away on ReplaceRootMsg, which Reconnect's
// success path sends instead) and shows the new error. It is
// tui.ClusterRebuildFailedMsg, so the root releases the rebuild gate the
// attempt held on its way here.
type RetryFailedMsg = tui.ClusterRebuildFailedMsg

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cmd, ok := tui.RoutePaste(msg, m.pasteTarget()); ok {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
		m.now = time.Now()
	case RetryFailedMsg:
		m.retrying = false
		m.retryErr = msg.Err
	case switchProbeMsg:
		if msg.gen != m.probeGen {
			return m, nil // stale run, already superseded — drain silently
		}
		if m.probes == nil {
			m.probes = make(map[string]kube.ProbeResult)
		}
		m.probes[msg.res.Name] = msg.res
		return m, waitForSwitchProbe(msg.gen, msg.ch)
	case switchProbesDoneMsg:
		return m, nil
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	}
	return m, nil
}

// pasteTarget is the kubeconfig-path buffer while 'k'/'e' has it open — a
// pasted path (usually with the newline the copy came with, which enter's
// TrimSpace drops) is the normal way to fill this field.
func (m *Model) pasteTarget() tui.PasteTarget {
	if !m.editing {
		return nil
	}
	return tui.PasteInto(&m.pathInput)
}
