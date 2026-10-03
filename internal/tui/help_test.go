package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// fakeHelpScreen is a minimal Screen stub — renderHelp only ever calls
// Keybar() on the active screen, so every other method is a stub.
type fakeHelpScreen struct {
	kb Keybar
}

func (f fakeHelpScreen) Theme() Theme         { return Dark() }
func (f fakeHelpScreen) Header() HeaderState  { return HeaderState{} }
func (f fakeHelpScreen) Strips(int) []string  { return nil }
func (f fakeHelpScreen) Keybar() Keybar       { return f.kb }
func (f fakeHelpScreen) Body(_, _ int) string { return "" }

// TestRenderHelpDoesNotTruncateColumns pins the 7b fix (docs/design
// README.md §113-114): columns are sized against the row's real budget
// (helpInset's frameWidth-2), not the full frame width — the previous
// off-by-two truncated every row with a stray "…" at most terminal widths.
func TestRenderHelpDoesNotTruncateColumns(t *testing.T) {
	view := fakeHelpScreen{kb: Keybar{
		PillText: "POD",
		Groups: [][]KeyHint{{
			{Key: "l", Label: "logs"},
			{Key: "y", Label: "yaml"},
		}},
	}}
	scope := []KeyHint{{Key: "g", Label: "goto"}, {Key: "n", Label: "namespace"}}
	list := []KeyHint{{Key: "/", Label: "filter"}, {Key: "↑↓ jk", Label: "move"}}
	resource := []KeyHint{{Key: "y", Label: "yaml"}, {Key: "m", Label: "labels/annotations"}}
	misc := []KeyHint{{Key: "?", Label: "help"}, {Key: "ctrl+q", Label: "quit"}}

	for _, width := range []int{80, 100, 120, 160, 220} {
		panel := renderHelp(Dark(), view, scope, list, resource, misc, width, 100)
		plain := ansi.Strip(panel)
		if strings.Contains(plain, "…") {
			t.Errorf("width %d: rendered help panel truncated a column with '…':\n%s", width, plain)
		}
	}
}

// TestHelpColumnWidthsShrinksEvenly pins M4: at 80 columns the old
// smallest-need-first fill topped up the three narrower columns and left the
// widest (LIST) at the floor, ellipsizing every one of its labels to "…".
func TestHelpColumnWidthsShrinksEvenly(t *testing.T) {
	natural := [4]int{9, 16, 22, 20} // PODS VIEW, SCOPE, LIST, RESOURCE
	const available, floor = 53, 8
	widths := helpColumnWidths(natural, available, floor)
	sum := 0
	for i, w := range widths {
		sum += w
		if w > natural[i] {
			t.Errorf("column %d widened past its natural width: %d > %d", i, w, natural[i])
		}
	}
	if sum > available {
		t.Fatalf("widths %v overflow the %d-cell budget", widths, available)
	}
	if widths[0] != natural[0] {
		t.Errorf("a column narrower than the common cap should keep its natural width, got %v", widths)
	}
	if widths[2] <= floor+4 {
		t.Errorf("the widest column was starved to %d cells (widths %v)", widths[2], widths)
	}
}

// On a short terminal the help panel must fit the palette's height cap and
// keep its footer (MISC, esc close): the columns give up their bottom rows to
// a "+N more" marker instead of Compose clipping the panel's bottom edge.
func TestRenderHelpFitsShortTerminal(t *testing.T) {
	var hints []KeyHint
	for i := range 20 {
		hints = append(hints, KeyHint{Key: string(rune('a' + i)), Label: "verb"})
	}
	view := fakeHelpScreen{kb: Keybar{PillText: "POD", Groups: [][]KeyHint{hints}}}
	scope := []KeyHint{{Key: "g", Label: "goto"}}
	misc := []KeyHint{{Key: "?", Label: "help"}, {Key: "ctrl+q", Label: "quit"}}

	const maxHeight = 14
	plain := ansi.Strip(renderHelp(Dark(), view, scope, nil, nil, misc, 120, maxHeight))
	lines := strings.Split(plain, "\n")
	if len(lines) > maxHeight {
		t.Fatalf("help panel is %d rows, want <= %d:\n%s", len(lines), maxHeight, plain)
	}
	for _, want := range []string{"MISC", "esc close", "more", "goto"} {
		if !strings.Contains(plain, want) {
			t.Errorf("expected %q in the capped panel:\n%s", want, plain)
		}
	}
	// A roomy terminal shows every hint, no marker.
	if roomy := ansi.Strip(renderHelp(Dark(), view, scope, nil, nil, misc, 120, 100)); strings.Contains(roomy, "more") {
		t.Errorf("expected no marker when everything fits:\n%s", roomy)
	}
}
