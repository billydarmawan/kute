package setup

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
)

// This file backs 4c's SWITCH CONTEXT list (docs/design README.md §4c):
// "bordered list of all kubeconfig contexts, pre-probed in the background
// with reachability + latency". Probing reuses kube.ProbeContexts, the same
// mechanism tui/context.go's 7a palette already establishes — a fresh
// probeGen per probe run guards against a stale drain (e.g. after 'r'
// retries the current context) clobbering a newer run's results.

// switchProbeMsg carries one kube.ProbeContexts result as it streams in,
// plus the channel it came from so the drain loop can keep reading without
// Model storing it; switchProbesDoneMsg marks the channel closed.
type switchProbeMsg struct {
	gen int
	ch  <-chan kube.ProbeResult
	res kube.ProbeResult
}
type switchProbesDoneMsg struct{ gen int }

// probeSwitchContextsCmd kicks off kube.ProbeContexts for names tagged with
// gen, returning the first Cmd in the drain chain. nil for an empty list.
// ctx is the session's process context, not its cluster one — the same
// split tui/context.go's probeContextsCmd documents: this run reaches every
// kubeconfig context, so only quitting should abandon it. The run starts
// inside the returned Cmd rather than in Init itself, which is called on the
// program goroutine — the same reason tui/context.go's probeContextsCmd
// does (02 H1).
func probeSwitchContextsCmd(ctx context.Context, gen int, names []string) tea.Cmd {
	if len(names) == 0 {
		return nil
	}
	return func() tea.Msg {
		return waitForSwitchProbe(gen, kube.ProbeContexts(ctx, names))()
	}
}

// waitForSwitchProbe reads one result off ch, tagged with gen — mirrors
// tui/context.go's waitForProbe exactly (same reasoning: the closure only
// ever reads from the ch it was given, so a newer probe run can't redirect
// it, it can only be out-gen'd once drained).
func waitForSwitchProbe(gen int, ch <-chan kube.ProbeResult) tea.Cmd {
	return func() tea.Msg {
		res, ok := <-ch
		if !ok {
			return switchProbesDoneMsg{gen: gen}
		}
		return switchProbeMsg{gen: gen, ch: ch, res: res}
	}
}

// switchContextRow is one row in 4c's own SWITCH CONTEXT list — current
// context first (its reachability is already known: it's why 4c exists),
// then every sibling context in OtherContexts' order.
type switchContextRow struct {
	name    string
	current bool
}

// switchContextRows builds the combined current+siblings list switchSel
// indexes into.
func (m Model) switchContextRows() []switchContextRow {
	rows := make([]switchContextRow, 0, 1+len(m.otherContexts))
	rows = append(rows, switchContextRow{name: m.clusterName, current: true})
	for _, n := range m.otherContexts {
		rows = append(rows, switchContextRow{name: n})
	}
	return rows
}

// moveSwitchSelection moves switchSel by delta, clamped to the row list.
func (m *Model) moveSwitchSelection(delta int) {
	rows := m.switchContextRows()
	if len(rows) == 0 {
		m.switchSel = 0
		return
	}
	m.switchSel = clampInt(m.switchSel+delta, 0, len(rows)-1)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// connectToSelected is 4c's '↵' (docs/design README.md §4c: "↵ connect to
// selected"): a no-op for the current row (already selected, and pressing
// r/e already cover retrying/redirecting it), with no SwitchToContext
// hook wired (NoConfig has none), or while a rebuild is already running.
func (m *Model) connectToSelected() (tea.Cmd, bool) {
	rows := m.switchContextRows()
	if m.retrying || m.switchToContext == nil || m.switchSel < 0 || m.switchSel >= len(rows) {
		return nil, false
	}
	row := rows[m.switchSel]
	if row.current {
		return nil, false
	}
	m.retrying = true
	m.retryErr = nil
	return m.switchToContext(row.name), true
}
