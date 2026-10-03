package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/state"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/components/palette"
)

// contextColumnHeaders builds 7a's NAMESPACE/STATUS column headers (docs/
// design README.md §7a: "Columns: glyph · CONTEXT (current tagged) ·
// NAMESPACE … · STATUS right-aligned") — CONTEXT itself is the palette's
// flex NameColumnLabel column, mirroring 6a's NAMESPACE/HEALTH/CPU split.
func contextColumnHeaders() []palette.ColumnHeader {
	return []palette.ColumnHeader{
		{Label: "NAMESPACE", Width: 14, Align: components.AlignLeft},
		{Label: "STATUS", Width: 15, Align: components.AlignRight},
	}
}

// This file wires real data into the 'c' context palette (mvp-plan.md Phase
// 3, docs/design README.md §7a): kubeconfig contexts, each tagged with its
// remembered namespace, PROD status, and a lazily-probed reachability
// status; Enter runs the blocking kube.Cluster.SwitchContext rebuild in a
// tea.Cmd and restores the target context's namespace/kind.

// switchContextTimeout bounds the blocking SwitchContext rebuild (client
// build + informer cache sync) so an unreachable context doesn't hang the
// palette forever.
const switchContextTimeout = 15 * time.Second

// contextTarget is the opaque payload carried in palette.Item.Data for a
// context-scope item.
type contextTarget struct {
	name string
}

// contextProbeMsg carries one kube.ProbeContexts result as it streams in,
// plus the channel it came from so the drain loop can keep reading that
// same channel without Model having to store it (see waitForProbe);
// contextProbesDoneMsg marks it closed (every context reported). Both carry
// gen (startContextProbe's monotonic counter, mirroring browse's
// reloadEpoch/metricsEpoch guard pattern): reopening/re-probing while a
// previous run is still draining starts a new gen, so a stale run's
// results are drained (its channel still gets emptied) but not applied to
// m.probes.
type contextProbeMsg struct {
	gen int
	ch  <-chan kube.ProbeResult
	res kube.ProbeResult
}
type contextProbesDoneMsg struct{ gen int }

func contextHint() string {
	return fmt.Sprintf("%s · %d contexts", kubeconfigPath(), len(contextNames()))
}

// contextNames lists the kubeconfig's context names (sorted), or nil if the
// kubeconfig can't be read.
func contextNames() []string {
	names, _, err := kube.AvailableContexts()
	if err != nil {
		return nil
	}
	return names
}

// kubeconfigPath is the 7a right-hint's path text — abbreviated to `~/...`
// under $HOME (docs/design README.md §7a: right hint `~/.kube/config · 5
// contexts`), unlike kube.KubeconfigPath's real, unabbreviated path (used
// where an actual filesystem path is needed, e.g. app.go). A real
// $KUBECONFIG override is very often outside ~/.kube (this repo's own
// mise.toml points it at a repo-relative path) and can be long enough that
// padBetweenStyled's "drop the hint if it doesn't fit" fallback silently
// blanks the whole right-hint — shortening it here is what keeps it on
// screen in practice, not just cosmetic.
//
// The abbreviated form is slash-separated on every platform: `~` is a unix
// convention we're already borrowing on Windows, and `~\.kube\config` reads
// as neither one thing nor the other. The unabbreviated fallback keeps the
// platform's own separators, since that one is a real path a user may copy.
func kubeconfigPath() string {
	p, ok := kube.KubeconfigPath()
	if !ok {
		return "kubeconfig"
	}
	if home, err := os.UserHomeDir(); err == nil {
		if rest, ok := strings.CutPrefix(p, home); ok {
			return filepath.ToSlash("~" + rest)
		}
	}
	return p
}

// contextItems lists every kubeconfig context: its remembered namespace
// (Session.State.PerContext), a PROD tag from config.IsProd, the current
// context tagged, and reachability from probes (nil/missing entries render
// "probing…" — docs/design README.md §7a: "probed lazily on open").
func contextItems(sess *Session, probes map[string]kube.ProbeResult) []palette.Item {
	if sess == nil {
		return nil
	}
	names := contextNames()
	recentNums := recentNumbers(sess.State.RecentContexts, sess.Location.Context)
	prevTarget, hasPrev := mostRecentOther(sess.State.RecentContexts, sess.Location.Context)
	items := make([]palette.Item, 0, len(names))
	for _, name := range names {
		namespace := ""
		if pc, ok := sess.State.PerContext[name]; ok {
			namespace = pc.Namespace
		}
		item := palette.Item{
			Label: name,
			Cols: []palette.Cell{
				{Text: namespace},
				{Text: probeStatus(probes[name]), Tone: probeTone(probes[name])},
			},
			ProdTag:   sess.Config.IsProd(name),
			Data:      contextTarget{name: name},
			RecentNum: recentNums[name],
		}
		switch {
		case name == sess.Location.Context:
			item.Tag = "current"
		case hasPrev && name == prevTarget:
			item.Tag = "previous"
		}
		if res, ok := probes[name]; ok && res.Err != nil {
			item.Dim = true
		}
		items = append(items, item)
	}
	promoteRecentItems(items)
	return items
}

// probeStatus renders one context row's reachability glyph+text: "◌
// probing…" before a result has arrived, "● 12ms" on success, "✕
// unreachable" on error, and "○ not probed" for a credential-plugin context
// left alone until ctrl+r (docs/design README.md §7a).
func probeStatus(res kube.ProbeResult) string {
	switch {
	case res.Skipped:
		return GlyphCompleted + " not probed"
	case res.Name == "" && res.Err == nil && res.Latency == 0:
		return GlyphProbing + " probing…"
	case res.Err != nil:
		return GlyphFailed + " unreachable"
	default:
		return fmt.Sprintf("%s %dms", GlyphRunning, res.Latency.Milliseconds())
	}
}

// probeTone colors probeStatus's text to match (docs/design README.md §7a:
// "● 12ms green, ◌ probing… yellow, ✕ unreachable red") — probeStatus alone
// only ever rendered through the row's default faint tone, silently losing
// the reachability-at-a-glance cue the palette mockup relies on.
func probeTone(res kube.ProbeResult) palette.Tone {
	switch {
	case res.Skipped:
		return palette.ToneFaint
	case res.Name == "" && res.Err == nil && res.Latency == 0:
		return palette.ToneWarn
	case res.Err != nil:
		return palette.ToneBad
	default:
		return palette.ToneOK
	}
}

// probeContexts is kube.ProbeContexts, as a seam so tests can stand in a
// prober that blocks the way a hung exec credential plugin does.
var probeContexts = kube.ProbeContexts

// probeContextsCmd kicks off kube.ProbeContexts for names, tagged with gen,
// and returns the first cmd in the drain chain (see waitForProbe) — mirrors
// podlogs/stream.go's waitForStream pattern for turning a result channel
// into a tea.Cmd stream. nil for an empty names list (no contexts to
// probe). The probe run itself starts inside the returned Cmd, never on the
// caller's goroutine: the caller is the root Update, and every probe builds
// a client and may run a context's exec credential plugin, which client-go
// invokes with no deadline of its own (02 H1). ctx is derived from the
// session's *process* context, not its cluster one: a probe run reaches
// every kubeconfig context, so switching between them is no reason to
// abandon it — quitting is, and so is closing the palette (the caller
// cancels ctx then; see Model.stopContextProbe).
func probeContextsCmd(ctx context.Context, gen int, names []string) tea.Cmd {
	if len(names) == 0 {
		return nil
	}
	return func() tea.Msg {
		return waitForProbe(gen, probeContexts(ctx, names))()
	}
}

// waitForProbe reads one result off ch, tagged with gen so a stale drain
// loop (from a since-superseded probe run) can be told apart from the
// current one — see contextProbeMsg's doc comment. The closure keeps
// draining the same ch it was given; it never reaches back into Model, so
// there's nothing for a second, newer probe run to redirect it onto.
func waitForProbe(gen int, ch <-chan kube.ProbeResult) tea.Cmd {
	return func() tea.Msg {
		res, ok := <-ch
		if !ok {
			return contextProbesDoneMsg{gen: gen}
		}
		return contextProbeMsg{gen: gen, ch: ch, res: res}
	}
}

// contextRecentLabels is the 7a RECENT row's entries — context names are
// already their own display label, unlike goto's kind recents. Current and
// the immediately-previous context are excluded (numberedRecents), same as
// namespaceRecentLabels — they already have their own on-row tag.
func contextRecentLabels(sess *Session) []string {
	if sess == nil {
		return nil
	}
	return numberedRecents(sess.State.RecentContexts, sess.Location.Context)
}

// contextItemIndex finds target's row in items by its contextTarget
// payload — shared by contextBrowseSelection (alt-tab) and
// refreshContextPalette's digit-recent lookup.
func contextItemIndex(items []palette.Item, target string) (int, bool) {
	for i, it := range items {
		if t, ok := it.Data.(contextTarget); ok && t.name == target {
			return i, true
		}
	}
	return 0, false
}

// contextRecentFooter is 7a's digit-select confirmation, shown once a typed
// digit resolves to a RECENT-row context — mirrors namespaceRecentFooter/
// 12b's gotoAliasMatchFooter.
func contextRecentFooter(target string) []palette.FooterSpan {
	return []palette.FooterSpan{
		{Text: "↵", Tone: palette.FooterKey},
		{Text: " switches to ", Tone: palette.FooterDim},
		{Text: target, Tone: palette.FooterEm},
	}
}

// toggleSelectedContextProd flips the selected row's PROD status (7a's
// ctrl+p key) via config.Config.SetProd, best-effort (a write failure — e.g.
// an unwritable ~/.config — leaves the in-memory tag exactly where SetProd
// left it, which is untouched on error). A config.yaml that failed to load
// is refused outright, with the reason in the palette footer. Unlike startContextProbe/
// refreshContextPalette (which reset Sel to the alt-tab target on every
// call), this keeps the toggled row selected — the whole point of the key
// is to see its own tag change, so jumping the selection away would defeat
// it. A no-op when nothing is selected or the row isn't a context item.
func (m *Model) toggleSelectedContextProd() {
	item, ok := m.palette.Selected()
	target, isCtx := item.Data.(contextTarget)
	if !ok || !isCtx || m.session == nil {
		return
	}
	if err := m.session.Config.SetProd(target.name, !m.session.Config.IsProd(target.name)); errors.Is(err, config.ErrUnparsable) {
		// The in-memory Config is only what survived a broken parse, so
		// SetProd refused to write it over the user's file — say so where
		// they pressed the key rather than toggling nothing in silence.
		m.palette.Footer = contextMarkProdRefusedFooter()
		return
	}

	items := contextItems(m.session, m.probes)
	if m.palette.Query() != "" {
		items = palette.Filter(items, m.palette.Query())
	}
	m.palette.Items = items
	if i, ok := contextItemIndex(items, target.name); ok {
		m.palette.Sel = i
	}
}

// contextMarkProdRefusedFooter is ctrl+p's answer when config.yaml failed to
// load: the file is left untouched until it's fixed by hand.
func contextMarkProdRefusedFooter() []palette.FooterSpan {
	return []palette.FooterSpan{
		{Text: "config.yaml has errors", Tone: palette.FooterEm},
		{Text: " — fix it by hand before changing PROD; kute won't overwrite it", Tone: palette.FooterDim},
	}
}

// contextDispatch turns a selected context item into the SwitchContext cmd.
// A no-op (nil) when there's no live cluster to rebuild (--demo, or no
// cluster reachable at all) or the target is already current.
func contextDispatch(sess *Session, item palette.Item) tea.Cmd {
	target, ok := item.Data.(contextTarget)
	if !ok || sess == nil {
		return nil
	}
	return switchContextCmd(sess, target.name)
}

// switchContextCmd builds the tea.Cmd that runs the blocking
// cluster.SwitchContext rebuild. Every Session read (PerContext lookup,
// recording the outgoing context's location, recents) happens synchronously
// here, before the Cmd's goroutine starts — the returned closure only
// touches the stable *kube.Cluster pointer (internally mutex-protected), so
// there's no data race with the main Update loop's later Session access.
func switchContextCmd(sess *Session, name string) tea.Cmd {
	cluster := sess.Cluster
	if cluster == nil || name == "" || name == sess.Location.Context {
		return nil
	}
	// Refused before any Session write below: the rebuild already running
	// owns the cluster until it lands (rebuildgate.go).
	token, ok := sess.BeginClusterRebuild()
	if !ok {
		return func() tea.Msg { return SwitchContextMsg{Context: name, Err: ErrClusterRebuildInFlight} }
	}

	sess.SyncLocationToPerContext()
	sess.ClearPendingKind()
	// No remembered namespace leaves restoreNS empty: SwitchContext then
	// keeps the target context's own kubeconfig namespace (scoping the eager
	// Pod cache there too), and the Cmd reads it back once the switch lands —
	// the same namespace a launch into that context would open on.
	restoreNS, restoreKind, restoreFilter := "", kube.KindPod, ""
	if pc, ok := sess.State.PerContext[name]; ok {
		if pc.Namespace != "" {
			restoreNS = pc.Namespace
		}
		if pc.Kind != "" {
			restoreKind = kube.ResourceKind(pc.Kind)
		}
		restoreFilter = pc.Filter
	}
	// Seed the outgoing context too (see pushRecentNamespace's identical
	// note in namespace.go) — otherwise a context only ever entered
	// RecentContexts by being switched *to*, so the first alt-tab away
	// from whatever context the app launched into had no "previous" to
	// resolve to.
	sess.State.RecentContexts = state.PushRecent(sess.State.RecentContexts, sess.Location.Context)
	sess.State.RecentContexts = state.PushRecent(sess.State.RecentContexts, name)

	// Every read still in flight belongs to the cluster being left; cancel
	// them here, on the Update loop, rather than after the switch lands —
	// SwitchContext below blocks for the whole cache re-sync, which is
	// exactly how long a stale list would otherwise have to complete and
	// deliver rows from the old cluster into a screen showing the new one.
	// The rebuild itself is parented on the process context (Context, not
	// ClusterContext): it must survive the reset it just performed, and only
	// quitting should abandon it.
	sess.ResetClusterContext()
	parent := sess.Context()
	oldContext := sess.Location.Context
	oldNamespace := sess.Location.Namespace

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, switchContextTimeout)
		defer cancel()
		// restoreNS is threaded through so a scoped Cluster's eager Pod
		// cache scopes to the namespace the UI is about to show (the
		// target context's own restored namespace) rather than that
		// context's kubeconfig-default one — see SwitchContext's doc
		// comment. Ignored entirely in cluster-wide mode.
		if err := cluster.SwitchContext(ctx, name, restoreNS); err != nil {
			var notStarted *kube.SwitchNotStartedError
			if errors.As(err, &notStarted) {
				// Failed before anything was torn down: the original context
				// is still live, and rebuilding it would only throw away its
				// lazily started informers for nothing.
				return SwitchContextMsg{Context: name, Err: err, token: token}
			}
			// SwitchContext replaces informer factories in place. Rebuild the
			// original context before reporting failure so the task and Session
			// location we intentionally leave untouched still point at a usable
			// cluster rather than a half-switched target.
			rollbackCtx, rollbackCancel := context.WithTimeout(parent, switchContextTimeout)
			rollbackErr := cluster.SwitchContext(rollbackCtx, oldContext, oldNamespace)
			rollbackCancel()
			if rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("restore context %s: %w", oldContext, rollbackErr))
			}
			return SwitchContextMsg{Context: name, Err: err, RolledBack: true, token: token}
		}
		if restoreNS == "" {
			restoreNS = cluster.CurrentNamespace()
		}
		return SwitchContextMsg{Context: name, Namespace: restoreNS, Kind: restoreKind, Filter: restoreFilter, token: token}
	}
}

// contextBrowseSelection picks the initial Sel for a freshly opened 7a
// list: the most recently visited *other* context — mostRecentOther, since
// recentContexts[0] is always the current one — so a bare "c ↵" alt-tabs
// back to whatever context you were on before (docs/design README.md §7a).
// Falls back to the first (always selectable) item when there's no other
// context to toggle to.
func contextBrowseSelection(items []palette.Item, recentContexts []string, current string) int {
	if target, ok := mostRecentOther(recentContexts, current); ok {
		if i, ok := contextItemIndex(items, target); ok {
			return i
		}
	}
	return 0
}

// contextPaletteSelection resolves the 7a palette's Sel on open, re-probe,
// and every re-filter: the alt-tab target (contextBrowseSelection) while the
// query is empty, otherwise the top fuzzy match.
func contextPaletteSelection(sess *Session, items []palette.Item, query string) int {
	if query != "" {
		return 0
	}
	return contextBrowseSelection(items, sess.State.RecentContexts, sess.Location.Context)
}
