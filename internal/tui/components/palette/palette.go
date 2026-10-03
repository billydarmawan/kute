// Package palette is the one modal shell shared by every scoped picker —
// jump (2b/3a), namespace (6a), and context (7a) — replacing picker/
// (mvp-plan.md §0.5). It is pure UI: the root shell feeds it Items and
// reads Selected(); it never touches kube. Data wiring (real kinds/
// resources/namespaces/contexts, recents persistence, navigation) lands in
// Phases 2–3; this package only owns filtering and rendering.
package palette

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"
	"github.com/sahilm/fuzzy"

	"github.com/kute-dev/kute/internal/tui/components"
)

// Scope names which picker the palette is standing in for. The shell swaps
// Scope/Prompt/Hint when opening a different picker; the rendering and
// filtering logic below don't otherwise branch on it.
type Scope string

const (
	ScopeGoto      Scope = "goto"
	ScopeNamespace Scope = "namespace"
	ScopeContext   Scope = "context"
	// ScopeVerb/ScopeResource back tasks/whocan's (22a) 'v'/'K' slot edits —
	// "opening the palette shell to change each slot" (docs/design
	// README.md §22a). Same shell, same Filter/Move/Render machinery as
	// every other scope; only whocan's own root-shell wiring (tui/whocan.go)
	// differs.
	ScopeVerb     Scope = "verb"
	ScopeResource Scope = "resource"
)

// KeyContextReprobe and KeyContextMarkProd are 7a's context-palette keys,
// declared here once so the root shell's key routing (tui.handlePaletteKey)
// and this package's own key row can't drift apart — the verbs registry
// imports tui, so neither side can reference a verbs.Verb. Both are control
// chords because the context palette is a fuzzy text input: as bare 'r'
// and 'P' they swallowed letters out of "prod", "staging-eu-north" and
// every "arn:…" context name. Neither chord is bound by textfield, the
// palette's own navigation, or tui.RoutePaste (ctrl+v).
const (
	KeyContextReprobe  = "ctrl+r"
	KeyContextMarkProd = "ctrl+p"
)

// cursor is the input row's block cursor glyph and the selected row's
// 1-cell accent bar — mirrors tui.GlyphSelBar, duplicated locally rather
// than imported: palette (like every components/* package) stays
// Theme-agnostic and import-cycle-free from tui, which holds a *Model on
// the root shell (mvp-plan.md §0.9).
const cursor = "▎"

// pendingGlyph mirrors tui.GlyphPending ("▲") — duplicated locally rather
// than imported, same as cursor above, so palette stays Theme/glyph-agnostic
// and import-cycle-free from tui.
const pendingGlyph = "▲"

// Tone selects the style a piece of result-row text renders through —
// ToneDefault is the faint Detail shade (counts, ↗ markers); OK/Warn/Bad
// are the status colors (12b's fuzzy resource rows carry their live status
// glyph on the right; 6a's HEALTH column segments use them per glyph).
// Secondary/Faint/Ghost/Info back 6a's structured Cols cells, which need
// shades Right's single Detail default doesn't cover: PODS (TextSecondary),
// CPU (TextDim), the zero-pod dash (TextGhost), and HEALTH's ○ completed
// count (Info blue, same token AllNS already uses).
type Tone int

const (
	ToneDefault Tone = iota
	ToneOK
	ToneWarn
	ToneBad
	ToneSecondary
	ToneFaint
	ToneGhost
	ToneInfo
)

// FooterTone selects the style one Footer span renders through.
type FooterTone int

const (
	FooterDim FooterTone = iota // TextDim — the default explanatory tone
	FooterKey                   // Accent — key glyphs, the "alias" word, the namespace (12a/12b)
	FooterEm                    // Text — the emphasized jump destination (12b)
)

// FooterSpan is one styled run of the footer line. The mockups' footers are
// multi-colored ("alias" purple then dim; 12b's "↵ jumps to Deployments in
// nva-stage" mixes Accent/Text/dim), so the footer is spans, not one string.
type FooterSpan struct {
	Text string
	Tone FooterTone
}

// Item is one palette result row.
type Item struct {
	Label  string
	Detail string // e.g. "kind · Workloads", "pod · nva-stage"
	Right  string // count / latency / status
	// RightTone colors Right: ToneDefault renders through Detail's faint
	// shade; the status tones back 12b's live status glyph on resource rows.
	RightTone Tone
	Matches   []int // fuzzy-matched rune indexes into Label, for highlighting
	Dim       bool  // zero-count / unreachable rows
	// Muted renders Label one step darker than Normal (TextDim) without the
	// full zero-count Dim — 12a's unaliased kinds below the ranked daily
	// kinds (the mockup draws StatefulSets/Jobs dimmer than the aliased rows).
	Muted bool
	Tag   string // "current", "PROD"
	// Data is an opaque payload the caller attaches when building Items and
	// reads back off Selected() on Enter. Palette never interprets it — this
	// keeps the package free of any kube/tui/domain dependency.
	Data any
	// ProdTag renders a small "PROD" tag after the label (7a's PROD-context
	// escalation cue), styled via Styles.ProdTag. Independent of Tag, which
	// carries plainer text like "current".
	ProdTag bool
	// TopRule draws a divider line above this item, separating it from the
	// row before it (6a's pinned "all namespaces" last row).
	TopRule bool
	// AllNS renders the label in the blue ALL-NS token instead of the usual
	// Normal/Dim text color (6a's "all namespaces" row; the glyph itself is
	// just part of Label, supplied by the caller — palette doesn't know
	// tui/glyphs.go's constants).
	AllNS bool
	// AliasMatch marks this row as 12b's alias-typed pin-to-rank-1 match: an
	// "alias match" label appears after Label. The highlighted first letter
	// itself is carried by Matches (12a/12b render it exactly like a fuzzy
	// match — AccentHi bold, no chip glyph, no gutter column).
	AliasMatch bool
	// Cols holds structured right-hand cells for scopes with an explicit
	// column-header row (Model.ColumnHeaders, e.g. 6a's PODS/HEALTH/CPU) in
	// place of Right's single free-form string. Every result Item must
	// carry the same length as ColumnHeaders when the scope sets it; nil
	// for scopes without headers (goto/context keep using Right/Detail).
	Cols []Cell
	// RecentNum, when > 0 (1-9), renders as a key-glyph digit in the row's
	// leading gutter cell instead of leaving it blank (6a/7a's numbered
	// recent-pick: typing that digit jumps straight to this row — see
	// digitRecentTarget in package tui). Ignored on the selected row, which
	// keeps showing the cursor accent bar there instead — you're already on
	// it. Every scope but namespace/context leaves this at its zero value,
	// so their gutter cell renders blank exactly as before.
	RecentNum int
}

// Segment is one independently toned run within a Cell — 6a's HEALTH
// column needs this, since one cell shows multiple glyph counts each in
// their own status color ("●32 ▲2 ✕1").
type Segment struct {
	Text string
	Tone Tone
}

// Cell is one structured column value for Item.Cols. Segments, when
// non-empty, renders as space-joined independently toned runs (HEALTH);
// otherwise Text renders through Tone as a single run (PODS, CPU).
type Cell struct {
	Text     string
	Tone     Tone
	Segments []Segment
}

// ColumnHeader is one entry in Model.ColumnHeaders — a fixed-width column
// with its own header label and the alignment its Item.Cols cell values
// render through (header labels themselves always render left-aligned,
// matching 6a's mockup where e.g. the CPU header sits left while its
// values sit right in the same column).
type ColumnHeader struct {
	Label string
	Width int
	Align components.Align
}

// Model is the palette's state. Items holds the complete caller-provided
// list — already Filter-ed fuzzy results, or (in Browse mode) the goto
// scope's 12a ranked list. Render applies the height-bounded viewport.
type Model struct {
	Scope  Scope
	Prompt string // e.g. "ns ›"
	// Input drives the query box's own editing (cursor, paste, word-jump);
	// its native Prompt stays empty since renderInputRow hand-renders the
	// "›" prompt before it. Construct via NewInput(Scope), which also wires
	// the scope-specific empty-query placeholder. Query() reads its value.
	Input  textfield.Model
	Items  []Item
	Sel    int
	Recent []string
	Hint   string // right-hand hint on the input row, e.g. "jump anywhere"
	Browse bool   // goto scope, no query typed yet: 12a's ranked-chips state
	// Footer is an optional single line shown under the results (and RECENT,
	// if present), above the key row — 12a's static alias hint in Browse,
	// 12b's "↵ jumps to X" destination confirmation once an alias resolves.
	// Empty renders nothing.
	Footer []FooterSpan
	// ColumnHeaders, non-empty only for 6a today, draws an explicit header
	// row (NameColumnLabel + these labels) directly under the input row's
	// rule, and switches every result row to Item.Cols' fixed-width layout
	// instead of the free-form Label+Detail+Right line every other scope
	// uses. Must be the same length as every result Item's Cols.
	ColumnHeaders []ColumnHeader
	// NameColumnLabel is the header row's flex-column label (6a's
	// "NAMESPACE") — only meaningful alongside ColumnHeaders.
	NameColumnLabel string
	// GutterGlyph, non-empty only for 6a's "▸", reserves one cell right
	// after the selection accent bar and draws this glyph there on the
	// selected row only. 12a/7a explicitly have no gutter column ("no chip
	// glyph, no gutter column"), so this stays empty for them.
	GutterGlyph string
	// RecentHint, non-empty only for 6a's "↵ toggles last" alt-tab hint,
	// appends a "│"-separated hint to the RECENT line itself (unlike Footer,
	// which is always its own line below RECENT).
	RecentHint []FooterSpan
	// Loading, when true, replaces the results area with a pending-glyph
	// loading line instead of Items/"no matches" — 6a's namespace list opens
	// this way the first time it's asked for before the informer cache has
	// completed its initial sync (just after launch or mid SwitchContext),
	// so a genuinely empty result isn't mistaken for "no matches" while data
	// is still on its way in (mirrors browse's 15a loading state).
	Loading bool
	// Denied, when true (only meaningful for ScopeNamespace today), replaces
	// the results area with DeniedNote instead of Items/"no matches" and
	// switches the key row to a bare "↵ switch" hint — this identity may not
	// list Namespace, so there is nothing to browse, and the palette becomes
	// a free-typed "switch to <namespace>" input instead
	// (docs/lazy-informers.md §5.6). The caller is
	// responsible for leaving Items empty and, typically, relabeling Input's
	// placeholder ("switch to").
	Denied bool
	// DeniedNote is the notice line shown when Denied is true, e.g. "cannot
	// list namespaces".
	DeniedNote string
	// Offset is the first item in the scrollable result viewport. Items
	// always keeps the complete ranked/filtered result set; Render clips only
	// what does not fit between the palette's fixed chrome, and movement keeps
	// Sel visible within this window.
	Offset int
}

// Query returns the query box's current text.
func (m Model) Query() string {
	return m.Input.Value()
}

// NewInput builds the textfield.Model for scope's query box: no native
// prompt (Render supplies its own "›" prompt ahead of it) and, when empty,
// the scope-specific placeholder text that used to be a per-scope switch
// inside renderInputRow — the goto scope keeps no placeholder ("a bare
// cursor... the ranked list below is the affordance"). Callers must still
// call SetStyles (built from Theme via tui.TextInputStyles, kept out of this
// Theme-agnostic package) before Render.
func NewInput(scope Scope) textfield.Model {
	ti := textfield.New()
	ti.Prompt = ""
	switch scope {
	case ScopeNamespace:
		ti.Placeholder = "type to filter namespaces"
	case ScopeContext:
		ti.Placeholder = "type to filter contexts"
	case ScopeVerb:
		ti.Placeholder = "type to filter verbs"
	case ScopeResource:
		ti.Placeholder = "type to filter resources"
	}
	ti.Focus()
	return ti
}

// Styles are the pre-built style values Render composes from — the same
// Theme-agnostic convention as Table/MiniBar/Card. The caller (the root
// shell, which owns Theme) builds these once from Theme.Accent,
// Theme.BorderPalette, etc.; see docs/design/README.md §Design Tokens for
// which token backs which field.
//
// Every text style carries its region's background (BgInput on the input
// row, BgPalette on body rows and the key row) and every pad/gap is
// rendered through a region fill style — an outer background wrap would be
// cancelled by each inner span's ANSI reset, leaving the fill applied
// unevenly across the panel between spans.
type Styles struct {
	Frame  lipgloss.Style // border (BorderPalette fg, BgPalette bg), whole panel
	Body   lipgloss.Style // bg-only BgPalette: fill for body-row pads/gaps
	Input  lipgloss.Style // bg-only BgInput: fill for the input band's pads/gaps
	Prompt lipgloss.Style // "›" prompt, Accent bold
	// Cursor/query text/placeholder for the input row itself render through
	// Model.Input's own textfield.Styles (set via SetStyles), not through
	// this Styles struct — Placeholder below is only the "no matches"/
	// loading-line style now.
	Placeholder lipgloss.Style // TextFaint
	Hint        lipgloss.Style // right-hand input-row hint, TextFaint
	Match       lipgloss.Style // matched-char highlight, AccentHi bold
	Normal      lipgloss.Style // label text, TextPrimary
	Dim         lipgloss.Style // dim label (zero-count/unreachable), TextGhost
	Muted       lipgloss.Style // 12a's unaliased kind labels, TextDim
	Detail      lipgloss.Style // detail text and 2b/3a's right-aligned count, TextFaint
	// RightOK/RightWarn/RightBad back Item.RightTone's status hues (12b's
	// resource-row status glyphs): Good/Warn/Bad on BgPalette.
	RightOK, RightWarn, RightBad lipgloss.Style
	SelBar                       lipgloss.Style // selected row's 1-cell accent bar, Accent fg + SelBg bg
	SelRow                       lipgloss.Style // rest of the selected row, TextPrimary fg + SelBg bg
	SelBg                        lipgloss.Style // bg-only SelBg, for browse-grid selected cells
	Rule                         lipgloss.Style // edge-to-edge divider under the input row and above the key row, TextGhost
	RecentRule                   lipgloss.Style // divider above RECENT and the 3a footer, TextGhost2
	RecentLabel                  lipgloss.Style // "RECENT" label and its · separators, TextFaint
	RecentItem                   lipgloss.Style // recent entry text, TextSecondary
	KeyRow                       lipgloss.Style // bottom key row's labels + band fill, TextDim on BgPalette
	KeyRowKey                    lipgloss.Style // bottom key row's key tokens, Accent on BgPalette
	FooterDetail                 lipgloss.Style // FooterDim spans (the footer's explanatory text), TextDim
	FooterKey                    lipgloss.Style // FooterKey spans (key glyphs / "alias" / namespace), Accent
	FooterEm                     lipgloss.Style // FooterEm spans (12b's jump-destination kind), Text
	// ProdTag styles the 7a "PROD" tag's text, ProdText bold.
	ProdTag lipgloss.Style
	// ProdBorder styles the bracket glyphs wrapped around ProdTag's text
	// (docs/design README.md §7a: "PROD tag (border #4a2a2a, …)") — a
	// terminal-idiom stand-in for a literal bordered box, which would need
	// multiple lines a single palette row doesn't have.
	ProdBorder lipgloss.Style
	// AllNS styles the 6a "all namespaces" row's label, Info blue (the
	// mockup's #6aa8ef — distinct from the ALL-NS keybar pill's own token).
	AllNS lipgloss.Style
	// AliasLabel styles 12b's "alias match" text after a pinned row's label,
	// AccentHi.
	AliasLabel lipgloss.Style
}

// Filter fuzzy-matches query against each item's Label (sahilm/fuzzy),
// returning matches ranked by score with Matches populated for highlight
// rendering. An empty query returns items unchanged — callers switch to
// Browse for that case rather than relying on Filter's passthrough.
func Filter(items []Item, query string) []Item {
	if query == "" {
		return items
	}
	labels := make([]string, len(items))
	for i, it := range items {
		labels[i] = it.Label
	}
	matches := fuzzy.Find(query, labels)
	out := make([]Item, 0, len(matches))
	for _, m := range matches {
		item := items[m.Index]
		item.Matches = append([]int(nil), m.MatchedIndexes...)
		out = append(out, item)
	}
	return out
}

// Move shifts the selection by delta and scrolls the result viewport just
// enough to keep it visible. maxHeight is the panel's full height budget,
// including borders and fixed chrome.
func (m *Model) Move(delta, maxHeight int) {
	if len(m.Items) == 0 {
		m.Sel, m.Offset = 0, 0
		return
	}
	m.ClampViewport(maxHeight)
	m.Sel = clampInt(m.Sel+delta, 0, len(m.Items)-1)
	m.ClampViewport(maxHeight)
}

// MoveHalfPage moves selection and viewport together by half of the number
// of item rows currently visible. The shared helper provides the same
// boundary behavior as list screens; ClampViewport then accounts for the
// namespace palette's extra divider above its pinned all-namespaces row.
func (m *Model) MoveHalfPage(direction, maxHeight int) {
	if len(m.Items) == 0 {
		m.Sel, m.Offset = 0, 0
		return
	}
	m.ClampViewport(maxHeight)
	start, end := m.visibleItemRange(maxHeight)
	position := components.MoveHalfPage(m.Sel, start, max(end-start, 1), len(m.Items), direction)
	m.Sel, m.Offset = position.Selected, position.Offset
	m.ClampViewport(maxHeight)
}

// ClampViewport repairs selection and offset after a resize or result-set
// replacement, preserving the complete Items slice while ensuring Sel is in
// the window Render will draw.
func (m *Model) ClampViewport(maxHeight int) {
	if len(m.Items) == 0 {
		m.Sel, m.Offset = 0, 0
		return
	}
	m.Sel = clampInt(m.Sel, 0, len(m.Items)-1)
	m.Offset, _ = m.visibleItemRange(maxHeight)
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

// Selected returns the highlighted item, or (Item{}, false) if there is none.
func (m Model) Selected() (Item, bool) {
	if m.Sel < 0 || m.Sel >= len(m.Items) {
		return Item{}, false
	}
	return m.Items[m.Sel], true
}

// Width is the palette panel's outer width for the given screen width: a
// fixed ~69%, floored at 40 columns and capped at screenWidth. Fixed across
// the browse and fuzzy states — the mockups show 2b narrower (~54%), but
// resizing the panel on the first typed character makes the dialog visibly
// jump, so one width wins over per-state fidelity.
func Width(screenWidth int) int {
	return min(max(int(float64(screenWidth)*0.69), 40), screenWidth)
}

// Render draws the palette panel per the 2b/12a/12b mockups: rounded
// border, an edge-to-edge input band (prompt + block cursor), a Border
// rule, the ranked/fuzzy results list, an optional RECENT line, an optional
// Footer line, and an edge-to-edge key-row band under its own Border rule.
// Text rows inset one cell per side (the mockup's panel padding); the bands
// and rules run border to border.
func (m Model) Render(styles Styles, screenWidth, maxHeight int) string {
	width := Width(screenWidth)
	// frameWidth is the space between the borders — every row below is
	// wrapped/ruled to this column width, then the frame style's own
	// Width() (below) adds the border back to land on width overall.
	frameWidth := max(width-2, 12)

	rule := styles.Rule.Render(strings.Repeat("─", frameWidth))
	lines := []string{m.renderInputRow(styles, frameWidth), rule}
	lines = append(lines, m.renderColumnHeaderRow(styles, frameWidth)...)
	lines = append(lines, m.renderResults(styles, frameWidth, maxHeight)...)
	if len(m.Recent) > 0 {
		lines = append(lines, m.renderRecent(styles, frameWidth)...)
	}
	if len(m.Footer) > 0 {
		lines = append(lines, m.renderFooterLine(styles, frameWidth)...)
	}
	lines = append(lines, rule, m.renderKeyRow(styles, frameWidth))

	// +2: lipgloss v2's Width now counts the border itself (v1 added it on
	// top, hence frameWidth's own -2 above), so the style needs
	// frameWidth+2 to render at exactly width again.
	frame := styles.Frame.Border(lipgloss.RoundedBorder()).Width(frameWidth + 2)
	return frame.Render(strings.Join(lines, "\n"))
}

// fillSpaces renders n spaces through the region's fill style, so pads and
// gaps carry the panel background instead of the terminal's.
func fillSpaces(fill lipgloss.Style, n int) string {
	if n <= 0 {
		return ""
	}
	return fill.Render(strings.Repeat(" ", n))
}

// padTo truncates/pads content to exactly width cells, padding through fill.
func padTo(content string, width int, fill lipgloss.Style) string {
	content = components.Truncate(content, width)
	return content + fillSpaces(fill, width-lipgloss.Width(content))
}

// inset pads content to the text width and adds the 1-cell side margins,
// producing a line exactly width wide, all fills carrying fill's background.
func inset(content string, width int, fill lipgloss.Style) string {
	return fillSpaces(fill, 1) + padTo(content, width-2, fill) + fillSpaces(fill, 1)
}

func (m Model) renderInputRow(styles Styles, width int) string {
	prompt := m.Prompt
	if prompt == "" {
		prompt = "›"
	}
	input := m.Input
	hint := styles.Hint.Render(m.Hint)
	inputWidth := max(width-2-lipgloss.Width(prompt+" ")-lipgloss.Width(hint)-1, 1)
	if input.Value() == "" {
		inputWidth = max(inputWidth, lipgloss.Width(input.Placeholder))
	}
	left := styles.Prompt.Render(prompt+" ") + input.ViewWidth(inputWidth)
	if input.Value() == "" {
		left += styles.Input.Render(" ")
	}
	row := padBetweenStyled(left, hint, width-2, styles.Input)
	return inset(row, width, styles.Input)
}

func (m Model) renderResults(styles Styles, width, maxHeight int) []string {
	if m.Denied {
		note := styles.RightBad.Background(styles.Body.GetBackground()).Render(" ✕ " + m.DeniedNote)
		return []string{inset(note, width, styles.Body)}
	}
	if m.Loading {
		text := "loading…"
		if m.Scope == ScopeNamespace {
			text = "loading namespaces…"
		}
		loading := styles.RightWarn.Background(styles.Body.GetBackground()).Render(" " + pendingGlyph + " " + text)
		return []string{inset(loading, width, styles.Body)}
	}
	if len(m.Items) == 0 {
		empty := styles.Placeholder.Background(styles.Body.GetBackground()).Render(" no matches")
		return []string{inset(empty, width, styles.Body)}
	}
	start, end := m.visibleItemRange(maxHeight)
	lines := make([]string, 0, end-start+1)
	for i := start; i < end; i++ {
		item := m.Items[i]
		if item.TopRule && i > start {
			lines = append(lines, styles.RecentRule.Render(strings.Repeat("─", width)))
		}
		if len(m.ColumnHeaders) > 0 {
			lines = append(lines, m.renderColumnsResultLine(item, i == m.Sel, styles, width))
			continue
		}
		lines = append(lines, m.renderResultLine(item, i == m.Sel, styles, width))
	}
	return lines
}

// resultLineBudget returns how many terminal rows remain for results after
// reserving the panel border and every currently-present chrome row. A
// result state always gets at least one best-effort line; terminals smaller
// than the fixed chrome are handled by Compose's existing screen clamp.
func (m Model) resultLineBudget(maxHeight int) int {
	// Border (2), input+rule (2), bottom rule+key row (2).
	chrome := 6
	if len(m.ColumnHeaders) > 0 {
		chrome += 2
	}
	if len(m.Recent) > 0 {
		chrome += 2
	}
	if len(m.Footer) > 0 {
		chrome += 2
	}
	return max(maxHeight-chrome, 1)
}

// visibleItemRange computes the [start,end) window that fits the current
// line budget and contains Sel. Result rows consume one line, except a
// TopRule item consumes a second line when another visible item precedes it.
// At the bottom, the window backfills so the dialog does not leave avoidable
// blank result space after scrolling to the last item.
func (m Model) visibleItemRange(maxHeight int) (start, end int) {
	if len(m.Items) == 0 {
		return 0, 0
	}
	selected := clampInt(m.Sel, 0, len(m.Items)-1)
	start = clampInt(m.Offset, 0, len(m.Items)-1)
	if selected < start {
		start = selected
	}
	budget := m.resultLineBudget(maxHeight)

	for {
		end = m.windowEnd(start, budget)
		if selected < end || start >= selected {
			break
		}
		start++
	}

	if end == len(m.Items) {
		for start > 0 && m.windowLines(start-1, end) <= budget {
			start--
		}
	}
	return start, m.windowEnd(start, budget)
}

func (m Model) windowEnd(start, budget int) int {
	used := 0
	for i := start; i < len(m.Items); i++ {
		rowHeight := 1
		if i > start && m.Items[i].TopRule {
			rowHeight++
		}
		if used+rowHeight > budget && i > start {
			return i
		}
		used += rowHeight
		if used >= budget {
			return i + 1
		}
	}
	return len(m.Items)
}

func (m Model) windowLines(start, end int) int {
	lines := end - start
	for i := start + 1; i < end; i++ {
		if m.Items[i].TopRule {
			lines++
		}
	}
	return lines
}

// columnsLayout splits innerWidth (the text budget between the panel's
// 1-cell side margins) between the optional 1-cell GutterGlyph column and
// the flexible name column, for both renderColumnHeaderRow and
// renderColumnsResultLine to share — Model.ColumnHeaders' own widths are
// fixed and used as-is. Each of the len(ColumnHeaders) trailing columns
// gets a 2-cell gap before it; the gutter (when present) gets a 1-cell gap
// after it.
func (m Model) columnsLayout(innerWidth int) (gutterWidth, nameWidth int) {
	if m.GutterGlyph != "" {
		gutterWidth = 1
	}
	trailing := 0
	for _, h := range m.ColumnHeaders {
		trailing += h.Width + 2
	}
	gutterGap := 0
	if gutterWidth > 0 {
		gutterGap = 1
	}
	nameWidth = max(innerWidth-gutterWidth-gutterGap-trailing, 1)
	return gutterWidth, nameWidth
}

// renderColumnHeaderRow draws 6a's NAMESPACE/PODS/HEALTH/CPU header line
// under its own BorderSubtle rule, matching the mockup's header-row
// treatment (distinct from the input row's Border rule above it). Returns
// nil when the scope has no ColumnHeaders (every scope but 6a).
func (m Model) renderColumnHeaderRow(styles Styles, width int) []string {
	if len(m.ColumnHeaders) == 0 {
		return nil
	}
	innerWidth := width - 2
	gutterWidth, nameWidth := m.columnsLayout(innerWidth)

	var b strings.Builder
	b.WriteString(fillSpaces(styles.Body, gutterWidth))
	if gutterWidth > 0 {
		b.WriteString(fillSpaces(styles.Body, 1))
	}
	b.WriteString(padTo(styles.Detail.Render(m.NameColumnLabel), nameWidth, styles.Body))
	for _, h := range m.ColumnHeaders {
		b.WriteString(fillSpaces(styles.Body, 2))
		b.WriteString(padTo(styles.Detail.Render(h.Label), h.Width, styles.Body))
	}
	row := inset(b.String(), width, styles.Body)
	rule := styles.RecentRule.Render(strings.Repeat("─", width))
	return []string{row, rule}
}

// renderColumnsResultLine draws one 6a result row: the selection accent
// bar, an optional GutterGlyph column (selected row only), the namespace
// label (highlighted/tagged exactly like renderResultLine's Label
// handling), then Item.Cols laid out under ColumnHeaders' fixed widths.
func (m Model) renderColumnsResultLine(item Item, selected bool, styles Styles, width int) string {
	innerWidth := width - 2
	fill := styles.Body
	if selected {
		fill = styles.SelBg
	}
	onRow := func(s lipgloss.Style) lipgloss.Style {
		if selected {
			return s.Background(styles.SelBg.GetBackground())
		}
		return s
	}
	gutterWidth, nameWidth := m.columnsLayout(innerWidth)

	base := styles.Normal
	switch {
	case item.AllNS:
		base = styles.AllNS
	case item.Dim:
		base = styles.Dim
	case item.Muted:
		base = styles.Muted
	}
	if selected {
		base = styles.SelRow
	}

	var b strings.Builder
	if gutterWidth > 0 {
		if selected && m.GutterGlyph != "" {
			b.WriteString(onRow(styles.SelBar).Render(m.GutterGlyph))
		} else {
			b.WriteString(fillSpaces(fill, 1))
		}
		b.WriteString(fillSpaces(fill, 1))
	}
	label := renderHighlighted(item.Label, item.Matches, onRow(styles.Match), base)
	label += resultTag(item, onRow(styles.Detail), onRow(styles.ProdTag), onRow(styles.ProdBorder), fill)
	b.WriteString(padTo(label, nameWidth, fill))
	for i, h := range m.ColumnHeaders {
		b.WriteString(fillSpaces(fill, 2))
		var cell Cell
		if i < len(item.Cols) {
			cell = item.Cols[i]
		}
		b.WriteString(renderCell(cell, onRow, styles, h.Width, h.Align, fill))
	}

	content := padTo(b.String(), innerWidth, fill)
	if !selected {
		return recentGutterCell(item, styles, fill) + content + fillSpaces(fill, 1)
	}
	return styles.SelBar.Render(cursor) + content + fillSpaces(fill, 1)
}

// renderCell draws one Item.Cols entry: Segments (if present) as
// space-joined independently toned runs (6a's HEALTH), otherwise Text
// through Tone as a single run (PODS, CPU), aligned and padded to width.
func renderCell(cell Cell, onRow func(lipgloss.Style) lipgloss.Style, styles Styles, width int, align components.Align, fill lipgloss.Style) string {
	var content string
	if len(cell.Segments) > 0 {
		parts := make([]string, len(cell.Segments))
		for i, seg := range cell.Segments {
			parts[i] = onRow(resultRightStyle(seg.Tone, styles)).Render(seg.Text)
		}
		content = strings.Join(parts, fillSpaces(fill, 1))
	} else if cell.Text != "" {
		content = onRow(resultRightStyle(cell.Tone, styles)).Render(cell.Text)
	}
	content = components.Truncate(content, width)
	slack := max(width-lipgloss.Width(content), 0)
	if align == components.AlignRight {
		return fillSpaces(fill, slack) + content
	}
	return content + fillSpaces(fill, slack)
}

// renderResultLine draws one 2b/12a result at full band width: col 0 is the
// selection-bar gutter, then the label starts (12a's alias letter highlights
// inline via Matches — no chip glyph, no gutter column), and the right-hand
// count/status ends one cell in from the edge.
//
// A selected row keeps its spans' hues on the SelBg fill — per the mockups
// the label brightens to Text (like the table's selected NAME), the detail
// stays faint, the count goes Accent (12a "its count in purple"), and 12b's
// matched-rune highlight and "alias match" label survive selection. Only
// the backgrounds swap; nothing flattens to one color.
func (m Model) renderResultLine(item Item, selected bool, styles Styles, width int) string {
	innerWidth := width - 2
	fill := styles.Body
	if selected {
		fill = styles.SelBg
	}
	onRow := func(s lipgloss.Style) lipgloss.Style {
		if selected {
			return s.Background(styles.SelBg.GetBackground())
		}
		return s
	}

	base := styles.Normal
	switch {
	case item.AllNS:
		base = styles.AllNS
	case item.Dim:
		base = styles.Dim
	case item.Muted:
		base = styles.Muted
	}
	if selected {
		base = styles.SelRow
	}
	label := renderHighlighted(item.Label, item.Matches, onRow(styles.Match), base)
	label += resultTag(item, onRow(styles.Detail), onRow(styles.ProdTag), onRow(styles.ProdBorder), fill)
	if item.AliasMatch {
		label += fillSpaces(fill, 2) + onRow(styles.AliasLabel).Render("alias match")
	}
	left := label + resultDetail(item.Detail, onRow(styles.Detail), fill)

	rightStyle := onRow(resultRightStyle(item.RightTone, styles))
	if selected && item.RightTone == ToneDefault {
		// 12a: the selected row's count renders Accent — SelBar already
		// carries Accent-on-SelBg.
		rightStyle = styles.SelBar
	}
	right := rightStyle.Render(item.Right)

	text := padTo(padBetweenStyled(left, right, innerWidth, fill), innerWidth, fill)
	if !selected {
		return recentGutterCell(item, styles, fill) + text + fillSpaces(fill, 1)
	}
	return styles.SelBar.Render(cursor) + text + fillSpaces(fill, 1)
}

// recentGutterCell renders the outer 1-cell selection-bar slot for an
// unselected row: item.RecentNum's digit (styled as a key glyph via
// styles.FooterKey — Accent-on-BgPalette, the same "this is a key you can
// press" treatment footers already use for "↵"/alias letters) when set,
// else a blank cell exactly as before. Shared by renderResultLine (7a/2b's
// free-form rows) and renderColumnsResultLine (6a's fixed-column rows),
// whose outer wraps are otherwise identical.
func recentGutterCell(item Item, styles Styles, fill lipgloss.Style) string {
	if item.RecentNum > 0 {
		return styles.FooterKey.Render(strconv.Itoa(item.RecentNum))
	}
	return fillSpaces(fill, 1)
}

// resultRightStyle maps a Tone to its style — Detail's faint shade for
// counts, the status hues for 12b's resource-row glyphs and 6a's HEALTH
// segments, and the Secondary/Faint/Ghost/Info shades 6a's PODS/CPU/
// zero-pod/completed cells need (see Tone's doc comment) — each reused
// from an existing Styles field rather than minting a new one.
func resultRightStyle(tone Tone, styles Styles) lipgloss.Style {
	switch tone {
	case ToneOK:
		return styles.RightOK
	case ToneWarn:
		return styles.RightWarn
	case ToneBad:
		return styles.RightBad
	case ToneSecondary:
		return styles.RecentItem
	case ToneFaint:
		return styles.FooterDetail
	case ToneGhost:
		return styles.Dim
	case ToneInfo:
		return styles.AllNS
	}
	return styles.Detail
}

func resultDetail(detail string, style, fill lipgloss.Style) string {
	if detail == "" {
		return ""
	}
	return fillSpaces(fill, 2) + style.Render(detail)
}

// resultTag renders a row's Tag/ProdTag suffix — Tag (e.g. "current")
// through Detail's shade, ProdTag as a bracketed "[PROD]" chip (prodBorder
// coloring the brackets, prodStyle the text) so it reads as the bordered
// escalation cue the spec calls for. Gaps render through fill so the row
// background stays unbroken.
func resultTag(item Item, tagStyle, prodStyle, prodBorder, fill lipgloss.Style) string {
	var b strings.Builder
	if item.Tag != "" {
		b.WriteString(fillSpaces(fill, 1))
		b.WriteString(tagStyle.Render(item.Tag))
	}
	if item.ProdTag {
		b.WriteString(fillSpaces(fill, 1))
		b.WriteString(prodBorder.Render("[") + prodStyle.Render("PROD") + prodBorder.Render("]"))
	}
	return b.String()
}

func renderHighlighted(label string, matches []int, matchStyle, base lipgloss.Style) string {
	if len(matches) == 0 {
		return base.Render(label)
	}
	matchSet := make(map[int]bool, len(matches))
	for _, i := range matches {
		matchSet[i] = true
	}
	var b strings.Builder
	for i, r := range []rune(label) {
		if matchSet[i] {
			b.WriteString(matchStyle.Render(string(r)))
		} else {
			b.WriteString(base.Render(string(r)))
		}
	}
	return b.String()
}

// renderRecent draws the RECENT section as the mockup's single line —
// "RECENT  Pods · Services · …" — under a BorderSubtle divider.
// renderRecent draws the RECENT line, plus, when RecentHint is set (6a/7a's
// "↵ toggles last"/"1-9 pick" alt-tab hint), a "│"-separated hint appended on
// the same line (mockup: "RECENT  nva-prod · dev-payments  │  ↵ toggles
// last"). The hint's width is reserved *before* laying out entries — a
// steady-state recents list (state.MaxRecent = 11 real namespace/context
// names) routinely overflows the panel width, and inset's tail-truncation
// would otherwise silently swallow the hint (and chop an entry's name
// mid-word) with no "…" to show it happened. Entries that don't fit are
// dropped instead, always leaving at least one so the row is never empty.
func (m Model) renderRecent(styles Styles, width int) []string {
	divider := styles.RecentRule.Render(strings.Repeat("─", width))
	innerWidth := width - 2 // matches inset's text budget between the 1-cell side margins

	label := styles.RecentLabel.Render("RECENT") + fillSpaces(styles.Body, 2)
	sep := styles.RecentLabel.Render(" · ")
	var hint string
	if len(m.RecentHint) > 0 {
		hint = fillSpaces(styles.Body, 2) + styles.Dim.Render("│") + fillSpaces(styles.Body, 2) + renderFooterSpans(m.RecentHint, styles)
	}
	budget := innerWidth - lipgloss.Width(label) - lipgloss.Width(hint)

	var entries strings.Builder
	used, shown := 0, 0
	for _, r := range m.Recent {
		item := styles.RecentItem.Render(r)
		add := lipgloss.Width(item)
		if shown > 0 {
			add += lipgloss.Width(sep)
		}
		if used+add > budget && shown > 0 {
			break
		}
		if shown > 0 {
			entries.WriteString(sep)
		}
		entries.WriteString(item)
		used += add
		shown++
	}

	line := label + entries.String() + hint
	return []string{divider, inset(line, width, styles.Body)}
}

// renderFooterLine draws Model.Footer as a single line under a BorderSubtle
// divider — 12a's static alias hint in Browse mode, 12b's "↵ jumps to X"
// destination confirmation once a typed alias resolves.
func (m Model) renderFooterLine(styles Styles, width int) []string {
	divider := styles.RecentRule.Render(strings.Repeat("─", width))
	return []string{divider, inset(renderFooterSpans(m.Footer, styles), width, styles.Body)}
}

// renderFooterSpans renders a []FooterSpan through each span's tone style
// (dim explanation, Accent keys, bright emphasis) — shared by
// renderFooterLine and renderRecent's trailing hint.
func renderFooterSpans(spans []FooterSpan, styles Styles) string {
	var b strings.Builder
	for _, span := range spans {
		st := styles.FooterDetail
		switch span.Tone {
		case FooterKey:
			st = styles.FooterKey
		case FooterEm:
			st = styles.FooterEm
		}
		b.WriteString(st.Render(span.Text))
	}
	return b.String()
}

// keyHint is one key+label pair on the palette's bottom key row.
type keyHint struct{ key, label string }

// renderKeyRow draws the palette's own bottom key band per the mockup:
// keys Accent, labels TextDim, pairs gap-separated (no · dots, unlike the
// app keybar), "esc close" right-aligned, all on the BgPalette band.
func (m Model) renderKeyRow(styles Styles, width int) string {
	// ctrl+jk is advertised only for goto/namespace — they have room for it,
	// and the context picker's row stays uncluttered (it's already carrying
	// reachability-probing/PROD-tag concerns the other two scopes don't).
	moveKey := "↑↓"
	if m.Scope == ScopeGoto || m.Scope == ScopeNamespace {
		moveKey = "↑↓ ctrl+jk"
	}
	halfPage := keyHint{"ctrl+d/u", "half-page"}
	narrow := "to narrow"
	aliasNarrow := "to keep narrowing"
	if width < 70 && (m.Scope == ScopeGoto || m.Scope == ScopeNamespace) {
		// Keep every actionable hint, including esc, on compact palettes.
		// The control-key line synonym remains active even when only arrows
		// fit in the row; ctrl-d/u's compact ½ label is self-explanatory.
		moveKey = "↑↓"
		halfPage.label = "½"
		narrow = "narrow"
		aliasNarrow = "narrow"
	}
	hints := []keyHint{{"↵", "jump"}, {"tab", "complete"}, {moveKey, "move"}}
	if m.Scope == ScopeGoto {
		hints = append(hints, halfPage)
	}
	switch {
	case m.Scope == ScopeNamespace && m.Denied:
		// 6a's denied state: nothing to browse or move through, just a
		// free-typed namespace name and Enter — see Model.Denied's doc
		// comment.
		hints = []keyHint{{"↵", "switch"}}
	case m.Browse:
		// 12a: "type to narrow · ↑↓ move · ↵ jump · esc close" — no
		// tab-complete hint in the empty-query ranked-chips state.
		hints = []keyHint{{"type", narrow}, {moveKey, "move"}, halfPage, {"↵", "jump"}}
	case len(m.Items) > 0 && m.Items[0].AliasMatch:
		// 12b: an alias match is pinned — "↵ jump · ↑↓ move · type to keep
		// narrowing · esc close".
		hints = []keyHint{{"↵", "jump"}, {moveKey, "move"}, halfPage, {"type", aliasNarrow}}
	case m.Scope == ScopeNamespace:
		// 6a: "↵ switch · ↑↓ move · 1-9 recent · esc close" — no tab-complete
		// hint (there's no alias/kind text to complete here). No "a"
		// shortcut: "a" types into the query like any other letter, so
		// filtering to a namespace starting with "a" (e.g. "nva-stage") isn't
		// shadowed by an all-namespaces jump — reach the pinned row via
		// ↑↓/↵ instead. "1-9 recent" names the digit gutter on recent rows
		// (palette.Item.RecentNum) — the row IS the legend.
		hints = []keyHint{{"↵", "switch"}, {moveKey, "move"}, halfPage, {"1-9", "recent"}}
	case m.Scope == ScopeContext:
		// 7a: "↵ switch · ↑↓ move · 1-9 recent · ctrl+r re-probe · ctrl+p
		// mark prod · esc close" — same no-tab-complete/1-9-recent
		// reasoning as ScopeNamespace, plus the re-probe and PROD-tag
		// chords (KeyContextReprobe/KeyContextMarkProd — never bare
		// letters, which must keep reaching the fuzzy query) in place of
		// the generic default's phantom "tab complete" (nothing in this
		// package implements tab-completion for any scope today).
		hints = []keyHint{{"↵", "switch"}, {moveKey, "move"}, {"1-9", "recent"}, {KeyContextReprobe, "re-probe"}, {KeyContextMarkProd, "mark prod"}}
	case m.Scope == ScopeVerb || m.Scope == ScopeResource:
		// 22a's v/k slot edits: "↵ set · ↑↓ move · esc close" — no
		// tab-complete hint, same reasoning as ScopeNamespace above.
		hints = []keyHint{{"↵", "set"}, {moveKey, "move"}}
	}
	var b strings.Builder
	b.WriteString(styles.KeyRow.Render(" "))
	for i, h := range hints {
		if i > 0 {
			b.WriteString(styles.KeyRow.Render("  "))
		}
		b.WriteString(styles.KeyRowKey.Render(h.key))
		b.WriteString(styles.KeyRow.Render(" " + h.label))
	}
	left := b.String()
	right := styles.KeyRowKey.Render("esc") + styles.KeyRow.Render(" close ")
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return padTo(left, width, styles.KeyRow)
	}
	return left + fillSpaces(styles.KeyRow, gap) + right
}

// padBetweenStyled places left-aligned left and right-aligned right within
// width, measuring already-styled (ANSI-containing) strings via
// lipgloss.Width and rendering the gap through fill so it carries the
// region background. When there isn't room for both, right is ellipsized to
// what fits (components.Truncate is ANSI-aware, so an already-styled string
// truncates safely) rather than dropped outright — 7a's right hint
// (`~/.kube/config · N contexts`) previously vanished with zero warning the
// moment a real $KUBECONFIG path pushed it a couple of columns over budget,
// which reads as a rendering bug, not a graceful degrade. Only truly no
// room at all (left alone already fills width) drops right entirely.
func padBetweenStyled(left, right string, width int, fill lipgloss.Style) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		budget := width - lipgloss.Width(left) - 1
		if budget <= 0 {
			return left
		}
		return left + fillSpaces(fill, 1) + components.Truncate(right, budget)
	}
	return left + fillSpaces(fill, gap) + right
}
