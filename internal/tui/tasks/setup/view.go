package setup

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

func (m Model) View() tea.View { return tea.NewView(m.Render()) }

func (m Model) Render() string { return tui.Frame(m.width, m.height, m) }

func (m Model) Theme() tui.Theme {
	if m.session != nil {
		return m.session.Theme
	}
	return tui.Dark()
}

// Header names the failing cluster (4c) or omits the segment entirely
// (10b, where there's no context to name yet) — docs/design README.md
// §4c/§10b: no namespace/kind breadcrumb, since neither screen has one.
func (m Model) Header() tui.HeaderState {
	theme := m.Theme()
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	ghost2 := lipgloss.NewStyle().Foreground(theme.TextGhost2)

	crumbs := tui.BrandCrumbs(theme)
	if m.clusterName != "" {
		crumbs = append(crumbs, tui.Crumb{Text: " │ ", Style: ghost2}, tui.Crumb{Text: m.clusterName, Style: dim})
	}

	var conn tui.ConnBadge
	if m.state == Unreachable {
		text := tui.GlyphProbing + " connecting failed"
		if m.conn.NeedsCredentials() {
			text = tui.GlyphProbing + " credentials expired"
		}
		conn = tui.ConnBadge{Text: text, Style: lipgloss.NewStyle().Foreground(theme.Bad)}
	} else {
		conn = tui.ConnBadge{Text: tui.GlyphCompleted + " no cluster", Style: dim}
	}
	return tui.HeaderState{Crumbs: crumbs, ConfigChip: tui.BuildConfigChip(theme, m.session), Conn: conn}
}

func (m Model) Strips(int) []string { return nil }

func (m Model) Keybar() tui.Keybar {
	if m.editing {
		return tui.Keybar{
			Pill:     tui.ModeSetup,
			PillText: "EDIT PATH",
			Groups:   [][]tui.KeyHint{{{Key: "enter", Label: "connect"}, {Key: "esc", Label: "cancel"}}},
		}
	}
	switch m.state {
	case Unreachable:
		hints := []tui.KeyHint{}
		if len(m.otherContexts) > 0 {
			// docs/design README.md §4c: "↵ connect to selected" — only
			// meaningful once there's a SWITCH CONTEXT list to select from.
			hints = append(hints, tui.KeyHint{Key: "↵", Label: "connect to selected"})
		}
		hints = append(hints, verbs.Context.Hint(), verbs.Retry.Hint(), tui.KeyHint{Key: "e", Label: "edit kubeconfig path"})
		note := "probing other kubeconfig contexts in the background"
		if m.conn.NeedsCredentials() {
			// kute can't run the login for you — it deliberately holds the
			// terminal away from the credential plugin (kube/execauth.go) —
			// so the note has to say where the fix happens.
			note = "re-authenticate in another shell, then press r"
		}
		return tui.Keybar{
			Pill:       tui.ModeNoCluster,
			PillText:   "NO CLUSTER",
			Groups:     [][]tui.KeyHint{hints},
			RightNote:  note,
			RightHints: []tui.KeyHint{{Key: "ctrl+q", Label: "quit"}},
		}
	default:
		return tui.Keybar{
			Pill:       tui.ModeSetup,
			PillText:   "SETUP",
			Groups:     [][]tui.KeyHint{{verbs.Retry.Hint(), {Key: "k", Label: "enter kubeconfig path"}}},
			RightHints: []tui.KeyHint{{Key: "ctrl+q", Label: "quit"}},
		}
	}
}

func (m Model) Body(width, height int) string {
	if m.state == Unreachable {
		return m.unreachableBody(width, height)
	}
	return m.noConfigBody(width, height)
}

// blockWidth is the centered content column's width, per the mockups' ~560px
// panels — floored so narrow terminals still get usable text.
func blockWidth(width int) int {
	return min(64, max(width-8, 24))
}

// hintBlockWidth is the provider-hint line's own, wider column — per the
// mockup, that line sits in its own centered block sized to fit the three
// provider commands on one row (~87 cols), rather than being squeezed into
// blockWidth's ~560px wordmark column.
func hintBlockWidth(width int) int {
	return min(92, max(width-8, 24))
}

// centerBlock left-aligns every line up to bw (so multi-line sub-boxes like
// the error/LOOKED IN panels keep their own internal alignment, and so short
// single lines land under the wordmark's own snug column) and centers the
// resulting block within width×height, mirroring components.Card's placement
// without forcing an outer border around everything. Lines already wider
// than bw (e.g. hintBlockWidth-centered provider hints) are passed through
// untouched — lipgloss.Place centers those independently by their own width
// rather than folding them into the bw column.
func centerBlock(lines []string, bw, width, height int) string {
	padded := make([]string, len(lines))
	for i, l := range lines {
		if lipgloss.Width(l) > bw {
			padded[i] = l
			continue
		}
		padded[i] = components.Pad(l, bw)
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, strings.Join(padded, "\n"))
}

func padBetween(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

// unreachableBody is 4c: title + retry countdown, the raw error, the
// pre-probed SWITCH CONTEXT list (switchContextLines — reachability and
// latency per context, from m.probes), and — while editing — the inline
// kubeconfig-path input.
func (m Model) unreachableBody(width, height int) string {
	theme := m.Theme()
	bw := blockWidth(width)

	bad := lipgloss.NewStyle().Foreground(theme.Bad)
	title := lipgloss.NewStyle().Foreground(theme.Text).Bold(true)
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	// docs/design README.md §4c calls for a red-tinted box (bg #16121a,
	// border #3a2a30, …), matching the 10b LOOKED IN box's own bordered
	// treatment (lookedInBox). Deliberate deviation: the fill reads as a
	// stray dark patch against the terminal's own background, so this
	// renders on the default background instead — border and text color
	// stay per spec.
	// bw-2: lipgloss v2's Width counts the border itself (v1 added it on
	// top), so this now needs 2 less subtracted to render at the same
	// total width as before.
	errStyle := lipgloss.NewStyle().Foreground(theme.BadMuted).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(theme.ErrCardBorder).
		Padding(0, 1).Width(bw - 2)

	ctx := m.clusterName
	if ctx == "" {
		ctx = "cluster"
	}
	headline := ctx + " is unreachable"
	if m.conn.NeedsCredentials() {
		// The cluster is answering; the credential isn't. Saying
		// "unreachable" here sends the user off checking VPNs and DNS.
		headline = ctx + " needs re-authentication"
	}
	left := bad.Render(tui.GlyphFailed) + " " + title.Render(headline)
	right := faint.Render(retryCountdown(m.conn, m.now))

	errText := m.conn.Err
	if errText == "" && m.err != nil {
		errText = m.err.Error()
	}
	if errText == "" {
		errText = "unreachable"
	}

	lines := []string{
		padBetween(left, right, bw),
		errStyle.Render(errText),
		"",
	}
	lines = append(lines, m.switchContextLines(theme, bw)...)
	lines = append(lines, m.statusLines(theme, bw)...)

	return centerBlock(lines, bw, width, height)
}

// statusLines renders whichever of the three mutually-exclusive retry
// states applies below the main body: the inline kubeconfig-path editor
// ('e'/'k'), a "connecting…" note while Reconnect's Cmd is in flight, or the
// last attempt's error — shared by both Unreachable and NoConfig.
func (m Model) statusLines(theme tui.Theme, bw int) []string {
	switch {
	case m.editing:
		return append([]string{""}, m.editLines(theme, bw)...)
	case m.retrying:
		return []string{"", lipgloss.NewStyle().Foreground(theme.TextDim).Render("connecting…")}
	case m.retryErr != nil:
		return []string{"", lipgloss.NewStyle().Foreground(theme.Bad).Render(tui.GlyphFailed + " " + m.retryErr.Error())}
	default:
		return nil
	}
}

// switchContextLines renders 4c's own SWITCH CONTEXT section: a bordered
// list of every kubeconfig context, pre-probed with reachability + latency
// (docs/design README.md §4c) — the current (failing) context always stays
// listed first, per the spec's own worked example.
func (m Model) switchContextLines(theme tui.Theme, bw int) []string {
	label := lipgloss.NewStyle().Foreground(theme.TextFaint).Render("SWITCH CONTEXT")
	rows := m.switchContextRows()
	if len(rows) <= 1 {
		return []string{label, lipgloss.NewStyle().Foreground(theme.TextGhost).Render("  (no other contexts configured)")}
	}

	// innerWidth is the box's own text column: Width(bw-4) minus its
	// Padding(0, 1) (1ch either side) — the width a selected row's
	// background must fully cover to read as a highlighted row rather
	// than just its text.
	innerWidth := bw - 6

	lines := make([]string, 0, len(rows))
	for i, row := range rows {
		glyph, status, tone := switchRowStatus(row, m.probes[row.name], m.conn.NeedsCredentials())
		fg := lipgloss.NewStyle()
		switch tone {
		case switchToneGood:
			fg = lipgloss.NewStyle().Foreground(theme.Good)
		case switchToneBad:
			fg = lipgloss.NewStyle().Foreground(theme.Bad)
		case switchToneWarn:
			fg = lipgloss.NewStyle().Foreground(theme.Warn)
		case switchToneFaint:
			fg = lipgloss.NewStyle().Foreground(theme.TextFaint)
		}
		name := lipgloss.NewStyle().Foreground(theme.TextSecondary)
		dim := lipgloss.NewStyle().Foreground(theme.TextGhost)
		fill := lipgloss.NewStyle()
		if i == m.switchSel {
			// Background must be set on every span (and the trailing
			// fill), not wrapped around the pre-rendered line: each
			// span's own ANSI reset would otherwise clear a background
			// applied only around the outside, leaving gaps between
			// tokens unhighlighted (docs/design README.md §4c — the
			// selected row highlights edge to edge, not just its text).
			fg = fg.Background(theme.SelBg)
			name = name.Background(theme.SelBg)
			dim = dim.Background(theme.SelBg)
			fill = fill.Background(theme.SelBg)
		}
		text := "  " + glyph + " " + row.name + " (" + status + ")"
		line := fill.Render("  ") + fg.Render(glyph) + fill.Render(" ") + name.Render(row.name) + fill.Render(" ") + dim.Render("("+status+")")
		if pad := innerWidth - lipgloss.Width(text); pad > 0 {
			line += fill.Render(strings.Repeat(" ", pad))
		}
		lines = append(lines, line)
	}

	// bw-2: lipgloss v2's Width counts the border itself (v1 added it on
	// top), so this now needs 2 less subtracted to render at the same
	// total width as before; innerWidth above (bw-6) is unaffected — it's
	// the content-only column, not this Width() argument.
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(theme.BorderSubtle).
		Padding(0, 1).
		Width(bw - 2).
		Render(strings.Join(lines, "\n"))
	return append([]string{label}, strings.Split(box, "\n")...)
}

// switchTone names switchRowStatus's color, since setup's view doesn't
// otherwise carry a StatusClass-like enum of its own.
type switchTone int

const (
	switchToneNone switchTone = iota
	switchToneGood
	switchToneBad
	switchToneWarn
	switchToneFaint
)

// switchRowStatus renders one SWITCH CONTEXT row's glyph/status text/tone:
// the current context is always "current · unreachable" — a short, fixed
// word rather than conn's verbatim dial error (already shown in full in the
// raw-error box just above this list; a real "dial tcp ... connection
// refused" string wraps this compact row ugly, unlike the mockup's
// illustrative one-word "timeout") — every other context reads its own
// probes entry: "probing…" before a result lands, "reachable · Nms" on
// success, "unreachable" on error, "not probed" for a credential-plugin
// context left alone (docs/design README.md §4c).
func switchRowStatus(row switchContextRow, probe kube.ProbeResult, needsCreds bool) (glyph, status string, tone switchTone) {
	if row.current {
		// needsCreds swaps the current row's one word, for the same reason
		// the headline changes: the context is reachable, its credential
		// isn't valid, and a sibling context may well share the same
		// expired login — so naming the real problem matters more here than
		// anywhere else on this screen.
		if needsCreds {
			return tui.GlyphFailed, "current · credentials expired", switchToneBad
		}
		return tui.GlyphFailed, "current · unreachable", switchToneBad
	}
	switch {
	case probe.Skipped:
		return tui.GlyphCompleted, "not probed", switchToneFaint
	case probe.Name == "" && probe.Err == nil && probe.Latency == 0:
		return tui.GlyphProbing, "probing…", switchToneWarn
	case probe.Err != nil:
		return tui.GlyphFailed, "unreachable", switchToneBad
	default:
		return tui.GlyphRunning, fmt.Sprintf("reachable · %dms", probe.Latency.Milliseconds()), switchToneGood
	}
}

// noConfigWordmark is 10b's ASCII wordmark verbatim from the mockup (docs/
// design/Kute Spec.dc.html §10b) — the block letters spelling "KUTE", with
// the product name, tagline, and version captions inline on rows 2/3/5
// exactly as the mockup places them. Rendered as one left-aligned unit (the
// mockup's own wordmark div is itself text-align:left; only the block as a
// whole sits in the centered column) so the letterforms stay aligned across
// rows regardless of each row's differing caption-driven length.
func noConfigWordmark() string {
	return "██  ██  ██  ██  ██████  ██████\n" +
		"██ ██   ██  ██    ██    ██        kute\n" +
		"████    ██  ██    ██    █████     a console for your clusters\n" +
		"██ ██   ██  ██    ██    ██\n" +
		"██  ██   ████     ██    ██████    v" + tui.Version
}

// noConfigBody is 10b: the wordmark, "no kubeconfig found", the LOOKED IN
// diagnostic box (from a *kube.ConfigLookupError when Err is one), a
// provider hint line, and — while editing — the inline path input.
func (m Model) noConfigBody(width, height int) string {
	theme := m.Theme()
	bw := blockWidth(width)
	hw := hintBlockWidth(width)

	wordmark := lipgloss.NewStyle().Foreground(theme.TextGhost2)
	text := lipgloss.NewStyle().Foreground(theme.TextPrimary)
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	hintLine := lipgloss.NewStyle().Width(hw).Align(lipgloss.Center)

	lines := strings.Split(wordmark.Render(noConfigWordmark()), "\n")
	lines = append(lines, "", text.Render(components.Truncate(noConfigTitle(m.err), bw)))
	lines = append(lines, m.lookedInBox(theme)...)
	lines = append(lines, hintLine.Render(faint.Render("get a kubeconfig from your cluster provider or run")))
	lines = append(lines, providerHintLines(dim, faint, hw)...)

	lines = append(lines, m.statusLines(theme, bw)...)

	return centerBlock(lines, bw, width, height)
}

// noConfigTitle is 10b's title line, naming what actually went wrong (docs/
// design README.md §10b): "no kubeconfig found" only when there is none (a
// *kube.ConfigLookupError — missing or empty), the context's name when the
// file lacks it, and a plain "couldn't be loaded" for anything else, whose
// real message the LOOKED IN box below carries.
func noConfigTitle(err error) string {
	if err == nil {
		return "no kubeconfig found"
	}
	if missing, ok := errors.AsType[*kube.ContextNotFoundError](err); ok {
		return "context " + missing.Name + " not found in kubeconfig"
	}
	if _, ok := asConfigLookupError(err); ok {
		return "no kubeconfig found"
	}
	return "kubeconfig couldn't be loaded"
}

// providerHintLines wraps 10b's three copy-pasteable provider commands
// ("·"-joined in the mockup) across as many lines as hw needs, never
// breaking a command mid-word — the mockup's fixed 960px canvas fits them on
// one centered line, but a narrow terminal may not, and silently truncating
// a command with "…" would make it uncopyable. Each resulting line is
// centered within hw (matching the mockup's own centered block) rather than
// left-aligned.
func providerHintLines(cmdStyle, dotStyle lipgloss.Style, hw int) []string {
	commands := []string{"aws eks update-kubeconfig", "gcloud container clusters get-credentials", "microk8s config"}
	const sep = " · "
	center := lipgloss.NewStyle().Width(hw).Align(lipgloss.Center)
	var out []string
	var cur string
	curWidth := 0
	for _, c := range commands {
		w := lipgloss.Width(c)
		if cur != "" && curWidth+lipgloss.Width(sep)+w > hw {
			out = append(out, center.Render(cur))
			cur, curWidth = "", 0
		}
		if cur == "" {
			cur = cmdStyle.Render(c)
			curWidth = w
		} else {
			cur += dotStyle.Render(sep) + cmdStyle.Render(c)
			curWidth += lipgloss.Width(sep) + w
		}
	}
	if cur != "" {
		out = append(out, center.Render(cur))
	}
	return out
}

// lookedInBox renders the LOOKED IN diagnostic — from a
// *kube.ConfigLookupError's Paths, falling back to Err's plain message when
// it isn't one (any other build failure — e.g. a malformed kubeconfig once
// in-cluster config also isn't available) — inside the mockup's bordered
// panel (BorderSubtle border, BgStrip fill), split back into lines for
// centerBlock.
func (m Model) lookedInBox(theme tui.Theme) []string {
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(theme.BorderSubtle).
		// Background(theme.BgStrip).
		Padding(0, 2).
		Render(strings.Join(m.lookedInLines(theme), "\n"))
	return strings.Split(box, "\n")
}

// lookedInLines renders the LOOKED IN box's inner content (the label plus
// one row per checked path) — lookedInBox wraps this in the bordered panel.
// Every span here must carry BgStrip explicitly: lipgloss's outer
// Background(BgStrip) on the box only fills the border/padding it adds
// itself, not the content — each inner span's own ANSI reset would
// otherwise fall through to the terminal's default background.
func (m Model) lookedInLines(theme tui.Theme) []string {
	fill := lipgloss.NewStyle() //.Background(theme.BgStrip)
	label := fill.Foreground(theme.TextFaint).Render("LOOKED IN")
	bad := fill.Foreground(theme.Bad)
	name := fill.Foreground(theme.TextSecondary)
	reason := fill.Foreground(theme.TextGhost)

	var lookup *kube.ConfigLookupError
	if m.err != nil {
		lookup, _ = asConfigLookupError(m.err)
	}
	if lookup == nil {
		msg := "unknown error"
		if m.err != nil {
			msg = m.err.Error()
		}
		return []string{label, reason.Render(msg)}
	}

	gap := fill.Render(" ")
	out := []string{label}
	for _, p := range lookup.Paths {
		row := bad.Render(tui.GlyphFailed) + gap + name.Render(p.Label)
		if p.Path != "" {
			row += gap + reason.Render(p.Path)
		}
		row += gap + reason.Render("— "+p.Reason)
		out = append(out, row)
	}
	return out
}

// asConfigLookupError unwraps err looking for a *kube.ConfigLookupError
// (Cluster/client construction may wrap it).
//
// errors.AsType, not a hand-rolled Unwrap() error walk: the hand-rolled
// version only followed single-error chains, so an errors.Join'd config
// error — one lookup failure gathered up beside anything else — defeated it
// and the LOOKED IN box degraded to a bare "unknown error" line.
func asConfigLookupError(err error) (*kube.ConfigLookupError, bool) {
	return errors.AsType[*kube.ConfigLookupError](err)
}

// editLines renders the inline kubeconfig-path input (shown by 'e'/'k')
// with a block cursor, or nil when not editing.
func (m Model) editLines(theme tui.Theme, bw int) []string {
	if !m.editing {
		return nil
	}
	label := lipgloss.NewStyle().Foreground(theme.TextFaint).Render("kubeconfig path")
	hint := lipgloss.NewStyle().Foreground(theme.TextFaint).Render("enter connect · ctrl-v paste · esc cancel")
	return []string{label, "  " + m.pathInput.View(), components.Pad(hint, bw)}
}

// retryCountdown formats 4c's "retrying in Ns · attempt N" from conn as of
// now — a zero conn (no ConnStateMsg observed yet) renders a neutral
// placeholder instead of "retrying in 0s".
func retryCountdown(conn kube.ConnState, now time.Time) string {
	if conn.NeedsCredentials() {
		// No attempt counter and no NextRetryAt in this phase, by design:
		// nothing is retrying, and saying so is the point. Kept short
		// because padBetween drops the whole right-hand segment rather than
		// wrapping it once the two sides don't fit the 64-column block.
		return "not retrying · r to recheck"
	}
	if conn.Attempt == 0 {
		return "connecting…"
	}
	next := max(conn.NextRetryAt.Sub(now), 0)
	return fmt.Sprintf("retrying in %ds · attempt %d", int(next.Round(time.Second).Seconds()), conn.Attempt)
}
