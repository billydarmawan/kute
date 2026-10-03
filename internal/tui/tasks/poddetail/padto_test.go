package poddetail

import (
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// padTo fits an EVENTS cell by display width, not bytes: a reason carrying
// multi-byte runes must come out valid UTF-8 at exactly width cells.
func TestPadToIsRuneSafe(t *testing.T) {
	for _, s := range []string{"Échec", "ÉchecDeTéléchargement", "拉取镜像失败", ""} {
		for _, width := range []int{4, 6, 8, 16} {
			got := padTo(s, width)
			if !utf8.ValidString(got) || ansi.StringWidth(got) != width {
				t.Fatalf("padTo(%q, %d) = %q (%d cells), want valid UTF-8 at %d cells", s, width, got, ansi.StringWidth(got), width)
			}
		}
	}
}
