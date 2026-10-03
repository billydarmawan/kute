package components

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/tui/components/textfield"
)

// modalStyles is a colorless stand-in for the TypeModalStyles every screen
// builds from its Theme — these tests care about where the caret lands, not
// which token paints it.
func modalStyles() TypeModalStyles {
	return TypeModalStyles{Input: lipgloss.NewStyle(), Selection: lipgloss.NewStyle().Reverse(true)}
}

func modalInput(value string, cursor int) textfield.Model {
	in := textfield.New()
	in.SetValue(value)
	in.SetCursor(cursor)
	in.Focus()
	return in
}

// inputLine returns the modal's type-ahead row — the only line carrying both
// the caret and the N/M progress count.
func inputLine(t *testing.T, rendered string) string {
	t.Helper()
	for _, line := range strings.Split(rendered, "\n") {
		if strings.Contains(line, "/") && strings.Contains(line, "api") {
			return line
		}
	}
	t.Fatalf("no input line in:\n%s", rendered)
	return ""
}

func TestModalInputAtEndKeepsTrailingBlock(t *testing.T) {
	got := modalInputView(modalInput("api-0", 5), 20, modalStyles())
	if !strings.Contains(got, "api-0█") {
		t.Errorf("expected a trailing block cursor in %q", got)
	}
}

// TestTypedWithCursorInsideReversesCaretRune is the regression this change
// exists for: with the caret moved off the end, the character under it is
// drawn reverse-video and no "█" is appended. Before, "█" was hardcoded to
// the end of the string, so left/right/Home/End moved the real caret while
// the rendered one never budged.
func TestModalInputInsideReversesCaretRune(t *testing.T) {
	got := modalInputView(modalInput("api-0", 2), 20, modalStyles())

	if strings.Contains(got, "█") {
		t.Errorf("caret inside the buffer should not also append a block: %q", got)
	}
	// SGR 7 (reverse) is what makes the caret visible; it survives color
	// downsampling all the way to the Ascii profile, so a NO_COLOR terminal
	// still shows it.
	if !strings.Contains(got, "\x1b[7m") {
		t.Errorf("expected a reverse-video caret in %q", got)
	}
	// The reversed run must be exactly the rune under the caret ("i"),
	// with the rest of the buffer intact and unshifted.
	if before, after, ok := strings.Cut(got, "\x1b[7m"); !ok {
		t.Fatalf("no caret in %q", got)
	} else if !strings.HasSuffix(before, "ap") || !strings.HasPrefix(after, "i") {
		t.Errorf("caret landed off the 3rd rune: before=%q after=%q", before, after)
	}
	if stripped := strings.TrimRight(stripANSI(got), " "); stripped != "api-0" {
		t.Errorf("caret changed the text: %q, want %q", stripped, "api-0")
	}
}

// TestTypedWithCursorIsRuneIndexed guards against a byte-offset caret: β is
// two bytes, so a byte-indexed split would slice it in half and emit a
// replacement character.
func TestModalInputIsRuneIndexed(t *testing.T) {
	got := strings.TrimRight(stripANSI(modalInputView(modalInput("aβc", 1), 20, modalStyles())), " ")
	if got != "aβc" {
		t.Errorf("typedWithCursor over a multi-byte rune = %q, want %q", got, "aβc")
	}
}

// TestTypeNameModalProgressCountsRunes pins the N/M counter against the same
// rune indexing the caret uses, so the two can't disagree.
func TestTypeNameModalProgressCountsRunes(t *testing.T) {
	rendered := TypeNameModal("✕ Delete", "", "", "api-βeta", modalInput("api-β", 5), "delete", false, modalStyles(), 60, 20)
	if line := inputLine(t, stripANSI(rendered)); !strings.Contains(line, "5/8") {
		t.Errorf("progress = %q, want a rune-counted 5/8", strings.TrimSpace(line))
	}
}

// TestTypeCountModalCarriesCaret confirms 20a's bulk sibling threads the
// caret too — a marked-set confirm types the count into the controller's buffer.
func TestTypeCountModalCarriesCaret(t *testing.T) {
	rendered := TypeCountModal("✕ Delete", "api-0, api-1", "", 12, modalInput("12", 0), false, modalStyles(), 60, 20)
	if !strings.Contains(rendered, "\x1b[7m") {
		t.Errorf("expected a reverse-video caret at position 0:\n%s", rendered)
	}
}

// stripANSI removes every escape sequence, leaving the printable cells.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
