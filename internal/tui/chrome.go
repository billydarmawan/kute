package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/kute-dev/kute/internal/kube"
)

func fitBody(content string, width, height int) string {
	lines := strings.Split(content, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = PadLine(lines[i], width)
	}
	return strings.Join(lines, "\n")
}

// --- Chrome v2 (mvp-plan.md §0.3) ---
//
// The inverted-layout skeleton every new screen renders through: header ·
// optional strips · body · keybar. Individual text spans arrive pre-styled
// (Crumb, KeyHint groups render via the screen's own Theme/Styles) so Frame
// itself stays render-agnostic aside from the full-width background fills,
// which it reads off Screen.Theme().

// Crumb is one pre-styled header breadcrumb segment, e.g. "kute", "›",
// "prod-eks", "default", "Pods".
type Crumb struct {
	Text  string
	Style lipgloss.Style
	// Fixed keeps the segment's full text when the header is too narrow
	// for every crumb — a list's kind, which is the screen's identity.
	// Other segments give up width widest-first (fitCrumbs).
	Fixed bool
}

// BrandCrumbs is the header's own wordmark: "kute" in the theme's plain
// text color (docs/design README.md §2a). Every Chrome v2 screen's
// Header() starts with this.
func BrandCrumbs(theme Theme) []Crumb {
	label := lipgloss.NewStyle().Foreground(theme.Text).Bold(true)
	return []Crumb{
		{Text: "kute", Style: label},
	}
}

// ConnBadge is the pre-styled connection indicator shown at the right of the
// header: "● connected · 12ms" / "◌ disconnected" / "○ no cluster".
type ConnBadge struct {
	Text  string
	Style lipgloss.Style
}

// LiveConnBadge builds the header's connection badge from the last
// kube.ConnStateMsg the screen saw (the root shell forwards it to the active
// task): connectedText — plus a "· 12ms" latency suffix when known — in
// green while connected, a red "◌ disconnected" once the watch/ping loop
// reports Reconnecting/Failed. Screens with genuinely stateful badges
// (podlogs' following/paused, setup's connecting-failed) build their own.
func LiveConnBadge(theme Theme, conn kube.ConnState, connectedText string) ConnBadge {
	if conn.Offline() {
		// "disconnected" would be a lie for an expired credential: the
		// cluster is answering, it just doesn't know who we are, and the fix
		// is re-authenticating rather than waiting.
		text := GlyphProbing + " disconnected"
		if conn.NeedsCredentials() {
			text = GlyphProbing + " credentials expired"
		}
		return ConnBadge{
			Text:  text,
			Style: lipgloss.NewStyle().Foreground(theme.Bad),
		}
	}
	text := connectedText
	if ms := conn.Latency.Milliseconds(); ms > 0 {
		text += fmt.Sprintf(" · %dms", ms)
	}
	return ConnBadge{Text: text, Style: lipgloss.NewStyle().Foreground(theme.Good)}
}

// HeaderState is the data a Chrome v2 screen exposes for the shared header
// band: "kute │ ctx › ns › Kind + navigation hints" on the left,
// SyncNote + Conn on the right.
type HeaderState struct {
	Crumbs   []Crumb
	SyncNote string // e.g. "sync 2s"
	// ConfigChip is the persistent "config.yaml has errors" warning —
	// empty (zero value) renders nothing, like UpdateChip/ForwardChip.
	// Built via BuildConfigChip. Renders leftmost of the chips: it outlives
	// every other one and changes what every destructive confirm asks.
	ConfigChip ConnBadge
	// UpdateChip is 28a's ambient "update available" indicator — empty
	// (zero value) renders nothing at all, same "zero chrome when inert"
	// contract as ForwardChip. Built via BuildUpdateChip. Renders left of
	// ForwardChip/Conn, matching the mockup's "↑ 0.2.1" then "● connected".
	UpdateChip ConnBadge
	// ForwardChip is 13d's ambient port-forward indicator — empty (zero
	// value) renders nothing at all, so a screen with no active forwards
	// leaves the header untouched (docs/design README.md §13d: "zero
	// chrome when no forwards exist"). Built via BuildForwardChip.
	ForwardChip ConnBadge
	Conn        ConnBadge
}

// ForwardSummary is the counts BuildForwardChip needs — kept minimal so a
// screen's Header() doesn't need a *kube.ForwardManager import just to
// render the chip (Session.ForwardSummary computes it).
type ForwardSummary struct {
	Active, Reconnecting int
}

// BuildForwardChip renders 13d's header chip from summary: nothing when no
// forwards exist, a quiet purple "⇄ N" while every session is healthy, or a
// yellow "⇄ N · ◌ M" the moment any session is reconnecting (docs/design
// README.md §13d: "one reconnecting: the chip goes yellow").
func BuildForwardChip(theme Theme, summary ForwardSummary) ConnBadge {
	switch {
	case summary.Active == 0 && summary.Reconnecting == 0:
		return ConnBadge{}
	case summary.Reconnecting > 0:
		text := fmt.Sprintf("%s %d", GlyphProbing, summary.Reconnecting)
		if summary.Active > 0 {
			text = fmt.Sprintf("%s %d · %s", GlyphForward, summary.Active, text)
		}
		return ConnBadge{Text: text, Style: lipgloss.NewStyle().Foreground(theme.Warn)}
	default:
		return ConnBadge{
			Text:  fmt.Sprintf("%s %d", GlyphForward, summary.Active),
			Style: lipgloss.NewStyle().Foreground(theme.Accent),
		}
	}
}

// Mode names the current interaction mode, driving the keybar's colored mode
// pill (purple = normal modes, blue = ALL NS, red = OFFLINE/CONFIRM, gray =
// SETUP — docs/design/README.md §Design Tokens).
type Mode string

const (
	ModeBrowse    Mode = "browse"
	ModeFilter    Mode = "filter"
	ModeGoto      Mode = "goto"
	ModeAllNS     Mode = "all-ns"
	ModeOffline   Mode = "offline"
	ModeNoCluster Mode = "no-cluster"
	ModeConfirm   Mode = "confirm"
	ModeHelp      Mode = "help"
	ModeYAML      Mode = "yaml"
	ModeExec      Mode = "exec"
	ModeSetup     Mode = "setup"
)

// KeyHint is one key/label pair rendered in a keybar group, sourced from a
// verbs.Verb reference (mvp-plan.md §0.4) — screens never inline a key or
// label string literal here.
type KeyHint struct {
	Key      string
	Label    string
	Disabled bool
}

// Keybar is the shared bottom-of-screen band: a colored mode pill, curated
// key-hint groups (joined with "│"), and an optional right-aligned side
// (plain note and/or key hints — hints render accent-key + dim-label like
// the left groups, mockup 2a's "? help").
type Keybar struct {
	Pill      Mode
	PillText  string // e.g. "PODS", "GOTO", "OFFLINE"
	Groups    [][]KeyHint
	RightNote string // e.g. "mutating actions disabled", "type to narrow"
	// RightNoteShort stands in for RightNote when the full keybar line would
	// overflow — insetChromeLine drops the whole right side rather than
	// truncating it, so a note that only sometimes fits (a will-run command
	// whose length depends on the kubeconfig path and context name) needs a
	// shorter form or it vanishes. Empty means no fallback.
	RightNoteShort string
	// RightWarnNote is a yellow-toned right-side note rendered after
	// RightNote — 17b's "HPA-managed workloads show ... as a yellow note
	// instead of blocking" (docs/design README.md §17b) is the one caller;
	// a plain RightNote always renders dim, with no room for its own color.
	RightWarnNote string
	RightHints    []KeyHint // e.g. the ? help verb; rendered after RightNote/RightWarnNote
	// Result is a mutation's outcome line (Frame fills it from
	// ActionResulter). The keybar is where a non-prod inline y/N asked the
	// question, so it is where the answer lands: a failure replaces the
	// hint groups and is truncated, never dropped; a success replaces
	// RightNote in green and keeps the hints.
	Result ResultLine
}

// ResultLine is a mutating action's outcome as the keybar shows it: a
// failure renders red behind GlyphFailed, a success green behind
// GlyphSucceeded. Text is folded onto one line, never wrapped.
type ResultLine struct {
	Text   string
	Failed bool
}

// Empty reports whether there is no outcome to show.
func (r ResultLine) Empty() bool { return r.Text == "" }

// ActionResulter is implemented by every screen that executes mutations
// through actions.Controller: ActionResult returns the controller's
// Result(), and Frame renders it on the keybar. One
// rendering path, so a failed delete/drain/cordon/restart can never just
// make the confirm vanish as success does (actions/guard_test.go enforces
// that every controller-embedding screen implements this).
type ActionResulter interface {
	ActionResult() ResultLine
}

// Screen is the Chrome v2 contract every redesigned task implements. Frame
// wraps Body between the header, strips, and keybar bands, so no screen
// hand-rolls its own chrome. Theme is exposed so Frame can style the parts
// of the chrome it owns (keybar hints, mode pill, header sync note) without
// every render call threading a Theme argument through
// Header/Strips/Keybar/Body — those already return pre-styled content built
// from the same Theme value. Chrome bands render transparent: the
// terminal's own background shows through everywhere.
type Screen interface {
	Theme() Theme
	Header() HeaderState
	// Strips returns 0..n full-width lines rendered under the header
	// (health/stale/error banner), pre-fit to width.
	Strips(width int) []string
	Keybar() Keybar
	// Body returns the inner content for the given width and the exact body
	// height Frame has budgeted. Frame truncates/pads the result.
	Body(width, height int) string
}

// InputCapturer is implemented by a Task with an active free-text input
// (e.g. browse's "/" filter) that needs every keystroke verbatim —
// including letters the root shell would otherwise treat as global
// shortcuts (g/n/c/?). The root shell checks this before routing a key to
// its own handling; a Task that doesn't implement it behaves as before.
type InputCapturer interface {
	CapturingInput() bool
}

// FrameInset is the Chrome v2 horizontal inset: every chrome band's text
// starts FrameInset cells in and ends FrameInset cells short of the right
// edge (the mockups' 14px padding). Exported so screens building their own
// full-width strip lines (browse's health/filter strips) align to it. The
// table body doesn't use it — its 2-cell marker slot ("▎ ") provides the
// same inset with the selection bar/background still reaching the screen
// edge.
const FrameInset = 2

// frameHeaderLines is the header band's height: just the breadcrumb line —
// matching the design README's "header = 1 line" terminal mapping. Flush
// against the top edge, mirroring the keybar sitting flush against the
// bottom edge; the rule below it (and the rule above the keybar) is each
// band's only separation, so the two stay genuinely symmetric.
const frameHeaderLines = 1

// borderHorizontal is the rule rune drawn below the header, below the strip
// band (when a screen renders one), and below the body — the design
// README's four bands (breadcrumb/header, cluster summary, table, context/
// help footer) visually separated without a full boxed border around them.
const borderHorizontal = "─"

// frameChromeLines is every fixed-height row Frame wraps the header/strip/
// body/keybar bands in: the header band's frameHeaderLines, the rule below
// the header, the rule below the body, and the keybar's own line. The rule
// below the strips (present only when a screen renders any) is added on top
// of this in FrameBodyHeight.
const frameChromeLines = frameHeaderLines + 1 + 1 + LegendHeight

// FrameBodyHeight is the body height Frame budgets for a screen rendering
// stripLines strip lines — exported so screens (browse's table viewport
// math) compute the same number instead of hardcoding band heights.
func FrameBodyHeight(height, stripLines int) int {
	h := NormalizeSize(DefaultWidth, height).Height - frameChromeLines - stripLines
	if stripLines > 0 {
		h-- // divider rule between the strip band and the table body
	}
	return max(h, 1)
}

// Frame composes a Chrome v2 screen: header (frameHeaderLines lines) · rule
// · strips (n lines) · rule (only when n > 0) · body (budgeted) · rule ·
// keybar (1 line) — the four bands the design README calls out (breadcrumb/
// header, cluster summary, table, context/help footer), each set off by a
// horizontal rule so they read as distinct bands even though the chrome
// itself stays transparent, always totalling exactly the normalized height.
func Frame(width, height int, s Screen) string {
	size := NormalizeSize(width, height)
	width, height = size.Width, size.Height
	theme := s.Theme()

	header := renderHeaderV2(s.Header(), theme, width)
	strips := s.Strips(width)
	stripLines := make([]string, len(strips))
	for i, line := range strips {
		stripLines[i] = PadLine(line, width)
	}
	kb := s.Keybar()
	if r, ok := s.(ActionResulter); ok && kb.Result.Empty() {
		kb.Result = r.ActionResult()
	}
	keybar := renderKeybarV2(kb, theme, width)

	bodyHeight := FrameBodyHeight(height, len(strips))
	body := fitBody(s.Body(width, bodyHeight), width, bodyHeight)

	// Rules read as visible-but-quiet structural glyphs rather than a loud
	// boxed border, so they use the text ramp's ghost tones (already the
	// convention for other subtle structural marks, e.g. the table
	// footer's scrollbar glyphs) instead of the darker Border/BorderSubtle
	// tokens, which nearly disappear against a real terminal's own
	// background once rendered without an explicit bg fill.
	primaryRule := lipgloss.NewStyle().Foreground(theme.TextGhost)
	stripRule := lipgloss.NewStyle().Foreground(theme.TextGhost2)
	rule := func(style lipgloss.Style) string {
		return style.Render(strings.Repeat(borderHorizontal, width))
	}

	lines := make([]string, 0, frameChromeLines+len(stripLines))
	lines = append(lines, header...)
	lines = append(lines, rule(primaryRule))
	lines = append(lines, stripLines...)
	if len(stripLines) > 0 {
		lines = append(lines, rule(stripRule))
	}
	for l := range strings.SplitSeq(body, "\n") {
		lines = append(lines, l)
	}
	lines = append(lines, rule(primaryRule))
	// The keybar band is the dedicated footer surface: the rule just
	// appended sets it off from the table body, the same way the header
	// band's own rule sets it off from the strip/body content above.
	lines = append(lines, keybar)
	return strings.Join(lines, "\n")
}

// insetChromeLine places pre-styled left/right content within width with
// FrameInset margins on both sides. Chrome renders transparent — the
// terminal's own background shows through (decided when the header's
// padding rows first made the BgChrome fill visible as a black bar on
// non-matching terminals). When there isn't room for both, right is
// dropped.
func insetChromeLine(left, right string, width int) string {
	inner := max(width-2*FrameInset, 0)
	gap := inner - lipgloss.Width(left) - lipgloss.Width(right)
	line := left
	if gap >= 1 && right != "" {
		line += strings.Repeat(" ", gap) + right
	}
	margin := strings.Repeat(" ", FrameInset)
	return PadLine(margin+line, width)
}

// renderHeaderV2 returns the header band's frameHeaderLines lines: just the
// breadcrumb line.
//
// The right side is laid out by priority rather than all-or-nothing: the
// connection badge always shows (it is how 4a announces an outage on
// screens without a stale strip), then the config, forward and update
// chips, then the sync note, each kept only while the crumbs can still fit
// at their shrunk minimum. The crumbs then take whatever is left, giving up
// width from their widest segment first — in practice the context, which
// for every EKS user is a full ARN — rather than pushing the badge off.
func renderHeaderV2(h HeaderState, theme Theme, width int) []string {
	var sync ConnBadge
	if h.SyncNote != "" {
		sync = ConnBadge{Text: h.SyncNote, Style: lipgloss.NewStyle().Foreground(theme.TextDim)}
	}
	// Render order, left to right; priority (which survive) is below.
	optional := []ConnBadge{sync, h.ConfigChip, h.UpdateChip, h.ForwardChip}
	keep := make([]bool, len(optional))
	composeRight := func() string {
		var right strings.Builder
		for i, chip := range optional {
			if keep[i] && chip.Text != "" {
				right.WriteString(chip.Style.Render(chip.Text) + "  ")
			}
		}
		return right.String() + h.Conn.Style.Render(h.Conn.Text)
	}

	inner := max(width-2*FrameInset, 0)
	minLeft := crumbsMinWidth(h.Crumbs)
	for _, i := range []int{1, 3, 2, 0} { // config, forward, update, sync
		keep[i] = true
		if minLeft+lipgloss.Width(composeRight())+1 > inner {
			keep[i] = false
		}
	}
	right := composeRight()
	budget := inner
	if right != "" {
		budget = max(inner-lipgloss.Width(right)-1, 0)
	}
	return []string{insetChromeLine(fitCrumbs(h.Crumbs, budget), right, width)}
}

// crumbMinWidth is the narrowest a single header crumb is shrunk to before
// the next-widest one gives up width instead — wide enough that a middle-
// ellipsized context still shows its distinctive tail (".../payments-prod").
const crumbMinWidth = 12

// crumbLastResortWidth is the second-pass floor, used only when every
// segment at crumbMinWidth still leaves no room for the connection badge.
const crumbLastResortWidth = 4

// crumbsMinWidth is the narrowest fitCrumbs can render crumbs without
// cutting the line itself.
func crumbsMinWidth(crumbs []Crumb) int {
	total := 0
	for _, c := range crumbs {
		if c.Fixed {
			total += lipgloss.Width(c.Text)
		} else {
			total += min(lipgloss.Width(c.Text), crumbMinWidth)
		}
	}
	return total
}

// fitCrumbs renders crumbs within budget cells: while they overflow, the
// leftmost non-Fixed segment wider than crumbMinWidth is middle-ellipsized
// down to it — the context first, then the namespace, and so on toward the
// screen's own segment — so the short structural ones (the wordmark,
// separators, key prefixes) and a Fixed kind keep their full text. Only
// once every segment is at that floor do they shrink further, and only
// past that is the line itself cut.
func fitCrumbs(crumbs []Crumb, budget int) string {
	texts := make([]string, len(crumbs))
	total := 0
	for i, c := range crumbs {
		texts[i] = c.Text
		total += lipgloss.Width(c.Text)
	}
	for _, floor := range []int{crumbMinWidth, crumbLastResortWidth} {
		for i, t := range texts {
			w := lipgloss.Width(t)
			if total <= budget || crumbs[i].Fixed || w <= floor {
				continue
			}
			texts[i] = middleEllipsis(t, max(w-(total-budget), floor))
			total -= w - lipgloss.Width(texts[i])
		}
	}
	var left strings.Builder
	for i, c := range crumbs {
		left.WriteString(c.Style.Render(texts[i]))
	}
	if total > budget {
		return ansi.Truncate(left.String(), budget, "…")
	}
	return left.String()
}

// middleEllipsis shortens plain text s to width cells by replacing its
// middle with "…", keeping both ends — a context's provider prefix and
// its cluster name.
func middleEllipsis(s string, width int) string {
	if ansi.StringWidth(s) <= width {
		return s
	}
	if width <= 1 {
		return ansi.Truncate(s, width, "")
	}
	head := (width - 1 + 1) / 2
	tail := width - 1 - head
	return ansi.Truncate(s, head, "") + "…" + ansi.TruncateLeft(s, ansi.StringWidth(s)-tail, "")
}

func renderKeybarV2(k Keybar, theme Theme, width int) string {
	accent := lipgloss.NewStyle().Foreground(theme.Accent)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	warn := lipgloss.NewStyle().Foreground(theme.Warn)
	// The inter-group │ is quieter than the labels around it (mockup 2a:
	// Border, not TextDim).
	sep := lipgloss.NewStyle().Foreground(theme.Border)

	var left strings.Builder
	if k.PillText != "" {
		left.WriteString(pillStyle(k.Pill, theme).Render(" " + k.PillText + " "))
		left.WriteString(" ")
	}
	renderHints := func(group []KeyHint) string {
		hints := make([]string, 0, len(group))
		for _, hint := range group {
			style := accent
			if hint.Disabled {
				style = faint
			}
			hints = append(hints, style.Render(hint.Key)+dim.Render(" "+hint.Label))
		}
		return strings.Join(hints, "  ")
	}

	if k.Result.Failed {
		return renderFailedKeybar(k, left.String(), renderHints, theme, width)
	}

	groupStrs := make([]string, 0, len(k.Groups))
	for _, group := range k.Groups {
		groupStrs = append(groupStrs, renderHints(group))
	}
	left.WriteString(strings.Join(groupStrs, sep.Render(" │ ")))

	renderRight := func(note string) string {
		right := ""
		if !k.Result.Empty() {
			// A success answers on the right, ahead of a TierNone verb's
			// will-run note, and keeps the hints: it's low-stakes, and a
			// narrow keybar may drop it the way it drops any right-side note.
			right = lipgloss.NewStyle().Foreground(theme.Good).Render(GlyphSucceeded + " " + singleLine(k.Result.Text))
		}
		if note != "" {
			if right != "" {
				right += "   "
			}
			right += dim.Render(note)
		}
		if k.RightWarnNote != "" {
			if right != "" {
				right += "   "
			}
			right += warn.Render(k.RightWarnNote)
		}
		if len(k.RightHints) > 0 {
			if right != "" {
				right += "   "
			}
			right += renderHints(k.RightHints)
		}
		return right
	}
	right := renderRight(k.RightNote)
	if k.RightNoteShort != "" && !chromeLineFits(left.String(), right, width) {
		right = renderRight(k.RightNoteShort)
	}
	return insetChromeLine(left.String(), right, width)
}

// chromeLineFits reports whether insetChromeLine would keep right rather
// than dropping it for want of room.
func chromeLineFits(left, right string, width int) bool {
	inner := max(width-2*FrameInset, 0)
	return inner-lipgloss.Width(left)-lipgloss.Width(right) >= 1
}

// renderFailedKeybar is the keybar while a failed ResultLine is showing:
// the mode pill, then the error in Bad behind GlyphFailed, truncated (never
// dropped, unlike a right-side note) so the right-side hints (? help) still
// fit when they can. The hint groups come back on the next key.
func renderFailedKeybar(k Keybar, pill string, renderHints func([]KeyHint) string, theme Theme, width int) string {
	right := ""
	if len(k.RightHints) > 0 {
		right = renderHints(k.RightHints)
	}
	inner := max(width-2*FrameInset, 0)
	avail := inner - lipgloss.Width(pill)
	if right != "" {
		avail -= lipgloss.Width(right) + 3
		if avail < 20 {
			right = ""
			avail = inner - lipgloss.Width(pill)
		}
	}
	text := Truncate(GlyphFailed+" "+singleLine(k.Result.Text), max(avail, 0))
	return insetChromeLine(pill+lipgloss.NewStyle().Foreground(theme.Bad).Render(text), right, width)
}

// singleLine folds a multi-line error (a joined drain error, a webhook
// denial) onto one keybar line.
func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// pillStyle picks the mode pill's hue per docs/design/README.md §Design
// Tokens: purple = normal modes, blue = ALL NS, red = OFFLINE/CONFIRM, gray =
// SETUP.
func pillStyle(mode Mode, theme Theme) lipgloss.Style {
	switch mode {
	case ModeAllNS:
		return lipgloss.NewStyle().Background(theme.AllNsPillBg).Foreground(theme.AllNsPillText)
	case ModeOffline, ModeConfirm, ModeNoCluster:
		return lipgloss.NewStyle().Background(theme.ConfirmPillBg).Foreground(theme.Bad)
	case ModeSetup:
		// docs/design README.md §10b: pill bg #1c1c2c / text #9a9ab2 — BorderSubtle/TextSecondary.
		return lipgloss.NewStyle().Background(theme.BorderSubtle).Foreground(theme.TextSecondary)
	case ModeGoto:
		// docs/design README.md §39: pill bg #1d1633 / text #c4b5fd, bold —
		// SelBg/AccentHi, brighter and bolder than every other mode's plain
		// Accent pill.
		return lipgloss.NewStyle().Background(theme.SelBg).Foreground(theme.AccentHi).Bold(true)
	default:
		return lipgloss.NewStyle().Background(theme.SelBg).Foreground(theme.Accent)
	}
}
