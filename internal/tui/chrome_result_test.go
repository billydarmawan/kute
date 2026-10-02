package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// sgrPrefix is the escape sequence a style emits before its text.
func sgrPrefix(style lipgloss.Style) string {
	out := style.Render("x")
	return out[:strings.Index(out, "x")]
}

// TestKeybarFailedResult pins review 10 H1's shared render path: a failed
// ResultLine replaces the hint groups, renders in the theme's Bad token
// behind GlyphFailed in both themes, folds a multi-line error onto one
// line, and truncates rather than dropping when it is too long to fit.
func TestKeybarFailedResult(t *testing.T) {
	for name, theme := range map[string]Theme{"dark": Dark(), "light": Light()} {
		t.Run(name, func(t *testing.T) {
			kb := Keybar{
				Pill: ModeBrowse, PillText: "PODS",
				Groups:     [][]KeyHint{{{Key: "l", Label: "logs"}}},
				RightHints: []KeyHint{{Key: "?", Label: "help"}},
				Result:     ResultLine{Text: "Delete Pod api-0 failed: forbidden\n" + strings.Repeat("x", 200), Failed: true},
			}
			out := renderKeybarV2(kb, theme, 100)
			text := ansi.Strip(out)
			if !strings.Contains(text, GlyphFailed+" Delete Pod api-0 failed: forbidden x") {
				t.Fatalf("keybar = %q, want the folded error", text)
			}
			if strings.Contains(text, "logs") {
				t.Fatalf("keybar = %q, the error replaces the hint groups", text)
			}
			if ansi.StringWidth(text) != 100 || !strings.Contains(text, "...") {
				t.Fatalf("keybar = %q, want a truncated full-width line", text)
			}
			if !strings.Contains(text, "? help") {
				t.Fatalf("keybar = %q, want ? help kept", text)
			}
			if !strings.Contains(out, sgrPrefix(lipgloss.NewStyle().Foreground(theme.Bad))) {
				t.Fatalf("failed result not rendered in theme.Bad: %q", out)
			}
		})
	}
}

// TestKeybarSuccessResult: a success keeps the hints and answers green on
// the right, ahead of any will-run note.
func TestKeybarSuccessResult(t *testing.T) {
	for name, theme := range map[string]Theme{"dark": Dark(), "light": Light()} {
		t.Run(name, func(t *testing.T) {
			kb := Keybar{
				Pill: ModeBrowse, PillText: "PODS",
				Groups: [][]KeyHint{{{Key: "l", Label: "logs"}}},
				Result: ResultLine{Text: "Delete Pod api-0"},
			}
			out := renderKeybarV2(kb, theme, 100)
			text := ansi.Strip(out)
			if !strings.Contains(text, "logs") || !strings.Contains(text, GlyphSucceeded+" Delete Pod api-0") {
				t.Fatalf("keybar = %q, want hints plus the success line", text)
			}
			if !strings.Contains(out, sgrPrefix(lipgloss.NewStyle().Foreground(theme.Good))) {
				t.Fatalf("success not rendered in theme.Good: %q", out)
			}
		})
	}
}

// TestKeybarRightNoteShortFallback pins the will-run fallback: a RightNote
// that overflows the line is swapped for RightNoteShort rather than dropped
// outright (insetChromeLine's behaviour for any right side that won't fit),
// while a RightNote that fits is always shown in full.
func TestKeybarRightNoteShortFallback(t *testing.T) {
	const full = "helm rollback shop 1 -n kute-e2e --kubeconfig /tmp/a/very/long/path/to/some/proxy.kubeconfig --kube-context kind-kute"
	const short = "helm rollback shop 1 -n kute-e2e --kube-context kind-kute …"
	kb := Keybar{
		Pill: ModeConfirm, PillText: "CONFIRM",
		Groups:         [][]KeyHint{{{Key: "y", Label: "confirm"}, {Key: "esc", Label: "cancel"}}},
		RightNote:      full,
		RightNoteShort: short,
	}
	if text := ansi.Strip(renderKeybarV2(kb, Dark(), 200)); !strings.Contains(text, full) {
		t.Fatalf("keybar at 200 cols = %q, want the full note", text)
	}
	text := ansi.Strip(renderKeybarV2(kb, Dark(), 120))
	if !strings.Contains(text, short) || strings.Contains(text, "--kubeconfig") {
		t.Fatalf("keybar at 120 cols = %q, want the short note in place of the full one", text)
	}
	kb.RightNoteShort = ""
	if text := ansi.Strip(renderKeybarV2(kb, Dark(), 120)); strings.Contains(text, "helm rollback") {
		t.Fatalf("keybar at 120 cols with no fallback = %q, want the overflowing note dropped", text)
	}
}
