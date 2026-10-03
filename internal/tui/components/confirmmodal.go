package components

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/tui/components/textfield"
)

// ConfirmStyles are the pre-styled spans ConfirmCard composes from — built
// by the caller from Theme.Confirm* tokens, keeping this component
// Theme-agnostic like Card. Border should already carry
// BorderForeground(Theme.ConfirmBorder) + Background(Theme.ConfirmHeaderBg)
// (mvp-plan.md §8b, invariant: "red borders reserved exclusively for
// destructive confirms"). ConfirmHeaderBg is the empty lipgloss.Color in
// both themes (theme.go), so the modal renders on the terminal's own
// background — only the red border itself carries a real color. Rule should
// carry Theme.TextGhost — the same token palette.Styles.Rule and chrome.go's
// band rules use — for the divider ConfirmCard draws above the key row.
type ConfirmStyles struct {
	Border lipgloss.Style
	Title  lipgloss.Style
	Detail lipgloss.Style
	Rule   lipgloss.Style
	Key    lipgloss.Style
	Label  lipgloss.Style
}

// ConfirmCard renders a y/N confirmation prompt centered within width×height:
// title line, an optional detail line (e.g. drain's "3 evictable · …"), a
// rule, and a key-hint row (mirroring palette.Model.Render's own divider
// above its key row). This is the minimal inline shape Phase 9's drain
// confirm needs — the full type-the-name PROD modal (mvp-plan.md §8b) is a
// later addition to this same file.
//
// title and detail are each one line: both are ellipsized to the card's
// inner width (width less the border and padding) so a long node name or a
// narrow terminal shortens the line instead of overflowing the screen.
// detail may already carry its own styling (tui.DrainImpactLine) —
// truncation is ANSI-aware.
func ConfirmCard(title, detail string, styles ConfirmStyles, width, height int) string {
	if inner := width - confirmChrome; inner > 0 {
		title = Truncate(title, inner)
		detail = Truncate(detail, inner)
	}
	box := ConfirmBox(title, detail, styles)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}

// confirmChrome is the cells ConfirmBox's frame takes from each line: a
// one-cell rounded border and three cells of padding on each side.
const confirmChrome = 2 + 2*3

// ConfirmBox renders ConfirmCard's bordered content only, without the
// width×height Place wrapper — for callers that composite it themselves
// (the root shell's quit confirm, layered over the active screen via
// components.Compose rather than centered in its own width×height).
func ConfirmBox(title, detail string, styles ConfirmStyles) string {
	lines := []string{styles.Title.Render(title)}
	if detail != "" {
		lines = append(lines, styles.Detail.Render(detail))
	}
	keyLine := styles.Key.Render("y") + styles.Label.Render(" confirm") + "   " + styles.Key.Render("esc") + styles.Label.Render(" cancel")
	lines = append(lines, styles.Rule.Render(strings.Repeat("─", maxLineWidth(lines, keyLine))), keyLine)
	content := strings.Join(lines, "\n")
	return styles.Border.Border(lipgloss.RoundedBorder()).Padding(1, 3).Render(content)
}

// TypeModalStyles are the pre-styled spans TypeNameModal composes from —
// built by the caller from Theme.ConfirmBorder/ConfirmHeaderBg (border/
// header — "the app's only red-bordered surface") and Theme.ProdBorder/
// ProdText (the PROD CONTEXT tag, the same tokens 7a's context palette
// already uses for the same meaning). Kept Theme-agnostic like ConfirmCard.
// Rule carries Theme.TextGhost, same as ConfirmStyles.Rule.
type TypeModalStyles struct {
	Border    lipgloss.Style
	Title     lipgloss.Style
	ProdTag   lipgloss.Style
	Owner     lipgloss.Style
	Detail    lipgloss.Style
	Rule      lipgloss.Style
	Input     lipgloss.Style
	Selection lipgloss.Style
	Progress  lipgloss.Style
	Key       lipgloss.Style
	Label     lipgloss.Style
}

// TypeNameModal renders 8b's type-the-name destructive confirm (docs/design
// README.md §8b): title (+ a right-aligned PROD CONTEXT tag when prod), an
// optional owner line ("Deployment/x — will be recreated"), a detail line
// (grace period / force-delete hint), the type-ahead prompt with a block
// cursor at the caret and "N/M" progress count, and
// the key row. ↵ only executes once
// typed == target — screens gate that via actions.Controller.Confirm, this
// component is purely presentational. actionVerb names the key row's verb
// ("delete", "rollback", …) — 16b's rollout-undo reuses this same modal for
// its own PROD confirm (docs/design README.md §16b) rather than "delete"
// being hardcoded there.
func TypeNameModal(title, ownerLine, detailLine, target string, input textfield.Model, actionVerb string, prod bool, styles TypeModalStyles, width, height int) string {
	titleLine := styles.Title.Render(title)

	body := []string{}
	if ownerLine != "" {
		body = append(body, styles.Owner.Render(ownerLine))
	}
	if detailLine != "" {
		body = append(body, styles.Detail.Render(detailLine))
	}
	body = append(body, "", styles.Detail.Render("type \""+target+"\" to confirm"))
	typed := input.Value()
	progress := styles.Progress.Render(fmt.Sprintf("%d/%d", utf8.RuneCountInString(typed), utf8.RuneCountInString(target)))
	body = append(body, modalInputView(input, max(width-12-lipgloss.Width(progress), 1), styles)+"  "+progress)
	keyLine := styles.Key.Render("↵") + styles.Label.Render(" "+actionVerb+" (when name matches)") + "   " +
		styles.Key.Render("esc") + styles.Label.Render(" cancel")

	if prod {
		prodTag := styles.ProdTag.Render("PROD CONTEXT")
		titleLine = padBetween2(titleLine, prodTag, max(maxLineWidth(body, keyLine), lipgloss.Width(titleLine)+lipgloss.Width(prodTag)+1))
	}

	lines := append([]string{titleLine}, body...)
	lines = append(lines, styles.Rule.Render(strings.Repeat("─", maxLineWidth(lines, keyLine))), keyLine)

	content := strings.Join(lines, "\n")
	box := styles.Border.Border(lipgloss.RoundedBorder()).Padding(1, 3).Render(content)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}

// TypeCountModal renders 20a's bulk PROD confirm — the count-based sibling
// of TypeNameModal: 8b's PROD delete escalation "becomes type-the-count"
// when the pending action targets a marked set rather than one resource
// (docs/design README.md §20a: "type 3 to confirm"). title (+ PROD CONTEXT
// tag), objectsLine listing every marked object, an optional detail line
// (grace period), the type-ahead prompt against the count's digit string,
// and the key row. ↵ only executes once typed == strconv of count — screens
// gate that themselves (browse's updateBulkDeleteKey), this component is
// purely presentational, same contract as TypeNameModal.
func TypeCountModal(title, objectsLine, detailLine string, count int, input textfield.Model, prod bool, styles TypeModalStyles, width, height int) string {
	target := strconv.Itoa(count)
	titleLine := styles.Title.Render(title)

	body := []string{}
	if objectsLine != "" {
		body = append(body, styles.Owner.Render(objectsLine))
	}
	if detailLine != "" {
		body = append(body, styles.Detail.Render(detailLine))
	}
	body = append(body, "", styles.Detail.Render("type \""+target+"\" to confirm"))
	typed := input.Value()
	progress := styles.Progress.Render(fmt.Sprintf("%d/%d", utf8.RuneCountInString(typed), utf8.RuneCountInString(target)))
	body = append(body, modalInputView(input, max(width-12-lipgloss.Width(progress), 1), styles)+"  "+progress)
	keyLine := styles.Key.Render("↵") + styles.Label.Render(" delete (when count matches)") + "   " +
		styles.Key.Render("esc") + styles.Label.Render(" cancel")

	if prod {
		prodTag := styles.ProdTag.Render("PROD CONTEXT")
		titleLine = padBetween2(titleLine, prodTag, max(maxLineWidth(body, keyLine), lipgloss.Width(titleLine)+lipgloss.Width(prodTag)+1))
	}

	lines := append([]string{titleLine}, body...)
	lines = append(lines, styles.Rule.Render(strings.Repeat("─", maxLineWidth(lines, keyLine))), keyLine)

	content := strings.Join(lines, "\n")
	box := styles.Border.Border(lipgloss.RoundedBorder()).Padding(1, 3).Render(content)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}

// modalInputView applies the confirmation modal's established solid-block
// end cursor while retaining the shared field's navigation, viewport, and
// selection behavior.
func modalInputView(input textfield.Model, width int, styles TypeModalStyles) string {
	// Preserve the modal's original single styled span in the common case;
	// the shared renderer takes over as soon as selection or scrolling is
	// needed.
	if !input.HasSelection() && input.Position() == utf8.RuneCountInString(input.Value()) && lipgloss.Width(input.Value())+1 <= width {
		return styles.Input.Render(input.Value() + "█")
	}
	input.EndOfBufferCharacter = '█'
	input.SetStyles(textfield.Styles{
		Focused:   textfield.StyleState{Text: styles.Input},
		Blurred:   textfield.StyleState{Text: styles.Input},
		Cursor:    styles.Input,
		Selection: styles.Selection,
	})
	return input.ViewWidth(width)
}

// padBetween2 right-pads left so right lands at width — the title row's
// PROD CONTEXT tag alignment. Named distinctly from any per-task padBetween
// helper since this file is shared across every screen that renders a modal.
func padBetween2(left, right string, width int) string {
	gap := max(width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + strings.Repeat(" ", gap) + right
}

// maxLineWidth measures a rule's target width: the widest of lines and any
// extra strings (e.g. the key row rendered separately from lines), all
// already-styled — lipgloss.Width ignores the ANSI.
func maxLineWidth(lines []string, extra ...string) int {
	w := 0
	for _, l := range append(append([]string{}, lines...), extra...) {
		if lw := lipgloss.Width(l); lw > w {
			w = lw
		}
	}
	return w
}
