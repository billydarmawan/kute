package tui

import (
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"
)

// TestRoutePasteDeliveryPaths pins the three ways a paste reaches a screen,
// plus what must NOT be treated as one.
func TestRoutePasteDeliveryPaths(t *testing.T) {
	tests := []struct {
		name        string
		msg         tea.Msg
		noTarget    bool
		wantValue   string
		wantHandled bool
		wantCmd     bool
	}{
		{
			name:        "bracketed paste inserts",
			msg:         tea.PasteMsg{Content: "kube-system"},
			wantValue:   "kube-system",
			wantHandled: true,
		},
		{
			name:        "bracketed paste with no buffer open is swallowed",
			msg:         tea.PasteMsg{Content: "kube-system"},
			noTarget:    true,
			wantHandled: true,
		},
		{
			name:        "OSC52 reply inserts",
			msg:         tea.ClipboardMsg{Content: "prod-eks", Selection: 'c'},
			wantValue:   "prod-eks",
			wantHandled: true,
		},
		{
			// A reply the user has navigated away from must not land in
			// whatever buffer happens to be open next.
			name:     "OSC52 reply with no buffer open is not handled",
			msg:      tea.ClipboardMsg{Content: "prod-eks", Selection: 'c'},
			noTarget: true,
		},
		{
			name:        "ctrl+v asks for the clipboard",
			msg:         tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'v'},
			wantHandled: true,
			wantCmd:     true,
		},
		{
			name:        "super+v asks for the clipboard",
			msg:         tea.KeyPressMsg{Mod: tea.ModSuper, Code: 'v'},
			wantHandled: true,
			wantCmd:     true,
		},
		{
			// Without a buffer open the chord has to stay available as an
			// ordinary key the screen can bind.
			name:     "ctrl+v with no buffer open is not handled",
			msg:      tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'v'},
			noTarget: true,
		},
		{
			name: "a bare v is not a paste",
			msg:  tea.KeyPressMsg{Text: "v", Code: 'v'},
		},
		{
			name: "an unrelated message is not a paste",
			msg:  tea.WindowSizeMsg{Width: 80, Height: 24},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := textfield.New()
			in.Focus()
			target := PasteInto(&in)
			if tc.noTarget {
				target = nil
			}
			cmd, handled := RoutePaste(tc.msg, target)
			if handled != tc.wantHandled {
				t.Fatalf("handled = %v, want %v", handled, tc.wantHandled)
			}
			if (cmd != nil) != tc.wantCmd {
				t.Fatalf("cmd != nil = %v, want %v", cmd != nil, tc.wantCmd)
			}
			if got := in.Value(); got != tc.wantValue {
				t.Fatalf("buffer = %q, want %q", got, tc.wantValue)
			}
		})
	}
}

// TestPasteIntoInsertsAtCursor pins that a paste lands at the cursor rather
// than replacing the buffer, and that a single-line field flattens the
// newline a pasted value usually carries.
func TestPasteIntoInsertsAtCursor(t *testing.T) {
	in := textfield.New()
	in.Focus()
	in.SetValue("ab")
	in.SetCursor(1)
	PasteInto(&in)("X\nY")
	if got := in.Value(); got != "aX Yb" {
		t.Fatalf("value = %q, want %q", got, "aX Yb")
	}
}

// TestPasteIntoAreaKeepsNewlines: the multi-line buffer is the one place a
// pasted newline must survive as a newline.
func TestPasteIntoAreaKeepsNewlines(t *testing.T) {
	ta := textarea.New()
	ta.Focus()
	PasteIntoArea(&ta)("first\nsecond")
	if got := ta.Value(); got != "first\nsecond" {
		t.Fatalf("value = %q, want %q", got, "first\nsecond")
	}
}

func TestPasteDigits(t *testing.T) {
	tests := []struct {
		paste string
		want  string
	}{
		{paste: "8080", want: "8080"},
		{paste: " 8080\n", want: "8080"}, // the trim that makes a real paste work
		{paste: "8080a", want: ""},       // rejected whole, per the fields' own rule
		{paste: "80 80", want: ""},       // interior space is not whitespace to trim
		{paste: "-1", want: ""},
		{paste: "   ", want: ""},
	}
	for _, tc := range tests {
		in := textfield.New()
		in.Focus()
		PasteDigits(PasteInto(&in))(tc.paste)
		if got := in.Value(); got != tc.want {
			t.Errorf("paste %q -> %q, want %q", tc.paste, got, tc.want)
		}
	}
	if PasteDigits(nil) != nil {
		t.Error("PasteDigits(nil) must stay nil so a closed buffer reads as no target")
	}
}

// TestPastePortPair is PasteDigits plus exactly the one separator the
// `local:remote` spelling uses: two colons, or one at either end, is as wrong
// as a letter — a half-written pair must never reach the buffer whole.
func TestPastePortPair(t *testing.T) {
	tests := []struct {
		paste string
		want  string
	}{
		{paste: "8080:3000", want: "8080:3000"},
		{paste: " 8080:3000\n", want: "8080:3000"},
		{paste: "3000", want: "3000"},
		{paste: "80:80:80", want: ""},
		{paste: ":3000", want: ""},
		{paste: "8080:", want: ""},
		{paste: "8080 3000", want: ""},
		{paste: "-1", want: ""},
		{paste: "   ", want: ""},
	}
	for _, tc := range tests {
		in := textfield.New()
		in.Focus()
		PastePortPair(PasteInto(&in))(tc.paste)
		if got := in.Value(); got != tc.want {
			t.Errorf("paste %q -> %q, want %q", tc.paste, got, tc.want)
		}
	}
	if PastePortPair(nil) != nil {
		t.Error("PastePortPair(nil) must stay nil so a closed buffer reads as no target")
	}
}
