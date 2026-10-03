package certchain

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// wrap measures display cells, not bytes: a failure message carrying
// multi-byte runes must never be cut mid-rune or overrun the width.
func TestWrapIsRuneSafe(t *testing.T) {
	msg := "the order’s challenge — “dns-01” — failed: 証明書の発行に失敗しました again and again"
	for _, width := range []int{7, 10, 13, 20} {
		lines := wrap(msg, width)
		for _, l := range lines {
			if !utf8.ValidString(l) {
				t.Fatalf("width %d: line %q is not valid UTF-8", width, l)
			}
			if w := ansi.StringWidth(l); w > width {
				t.Fatalf("width %d: line %q is %d cells", width, l, w)
			}
		}
		if got := strings.ReplaceAll(strings.Join(lines, ""), " ", ""); got != strings.ReplaceAll(msg, " ", "") {
			t.Fatalf("width %d: wrapping lost text: %q", width, got)
		}
	}
}
