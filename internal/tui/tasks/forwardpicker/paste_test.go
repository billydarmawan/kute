package forwardpicker

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/testutil/goldentest"
)

// TestPasteIntoLocalPortIsDigitGated pins the numeric field's paste rule:
// surrounding whitespace is trimmed (a copied port almost always carries a
// newline), but anything else non-digit rejects the paste whole — the same
// all-or-nothing rule the typed path applies per keypress.
func TestPasteIntoLocalPortIsDigitGated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		paste string
		want  string
	}{
		{paste: "8080", want: "98080"}, // inserted at the cursor, after the typed 9
		{paste: " 8080\n", want: "98080"},
		{paste: "80a80", want: "9"}, // rejected whole
		{paste: "80 80", want: "9"},
	}
	for _, tc := range tests {
		m := newModel(podWithPort("default", "web-0", 80))
		m = loadPorts(t, m)

		updated, _ := m.Update(tea.KeyPressMsg{Text: "9"})
		next := updated.(*Model)
		if !next.rows[0].editing {
			t.Fatal("expected a digit to begin the local-port edit")
		}
		updated, _ = next.Update(tea.PasteMsg{Content: tc.paste})
		if got := updated.(*Model).rows[0].editInput.Value(); got != tc.want {
			t.Errorf("paste %q -> %q, want %q", tc.paste, got, tc.want)
		}
	}
}

// TestPasteChordRequestsClipboardWhileEditing: ctrl+v while the port buffer is
// open answers with the clipboard read.
func TestPasteChordRequestsClipboardWhileEditing(t *testing.T) {
	t.Parallel()
	m := newModel(podWithPort("default", "web-0", 80))
	m = loadPorts(t, m)

	updated, _ := m.Update(tea.KeyPressMsg{Text: "9"})
	if _, cmd := updated.(*Model).Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'v'}); cmd == nil {
		t.Fatal("ctrl+v while editing returned no cmd, want the clipboard read")
	}
}

// TestPasteWithNoEditOpenIsIgnored: the row list's own keys are navigation, so
// a paste with no buffer open must not start an edit.
func TestPasteWithNoEditOpenIsIgnored(t *testing.T) {
	t.Parallel()
	m := newModel(podWithPort("default", "web-0", 80))
	m = loadPorts(t, m)

	updated, _ := m.Update(tea.PasteMsg{Content: "8080"})
	if updated.(*Model).rows[0].editing {
		t.Fatal("paste must not begin a local-port edit")
	}
}

// TestPasteIntoFreePortRowAcceptsAPortPair pins the free-port row's wider
// value rule: kubectl's `local:remote` spelling lands whole, while a second
// colon or a leading/trailing one is rejected the same way any non-port
// character is — a half-typed pair must never become a forward's target.
func TestPasteIntoFreePortRowAcceptsAPortPair(t *testing.T) {
	t.Parallel()
	tests := []struct {
		paste string
		want  string
	}{
		{paste: "8080:3000", want: "8080:3000"},
		{paste: " 8080:3000\n", want: "8080:3000"},
		{paste: "80:80:80", want: ""},
		{paste: ":8080", want: ""},
		{paste: "8080:", want: ""},
		{paste: "8080 3000", want: ""},
	}
	for _, tc := range tests {
		m := newModel(barePod("default", "web-0"))
		m = loadPorts(t, m)

		updated, _ := m.Update(tea.KeyPressMsg{Text: "3"})
		next := updated.(*Model)
		if !next.rows[0].editing {
			t.Fatal("expected a digit to begin the free-port row's edit")
		}
		next.rows[0].editInput.SetValue("")
		updated, _ = next.Update(tea.PasteMsg{Content: tc.paste})
		if got := updated.(*Model).rows[0].editInput.Value(); got != tc.want {
			t.Errorf("paste %q -> %q, want %q", tc.paste, got, tc.want)
		}
	}
}

// TestPastedPairForwardsBothHalves: the colon-tolerant buffer is not cosmetic
// — a pasted pair is what starts the forward on the local port the user asked
// for, not the pre-filled one.
func TestPastedPairForwardsBothHalves(t *testing.T) {
	t.Parallel()
	m, mgr, _ := newForwardingModel(t, barePod("default", "web-0"))
	updated, _ := m.Update(tea.KeyPressMsg{Text: "3"})
	next := updated.(*Model)
	next.rows[0].editInput.SetValue("")
	updated, _ = next.Update(tea.PasteMsg{Content: "18080:5432"})
	m = *updated.(*Model)

	m.commitEdit()
	if m.rows[0].localPort != 18080 || m.rows[0].Port != 5432 {
		t.Fatalf("row = local %d remote %d, want 18080 → 5432", m.rows[0].localPort, m.rows[0].Port)
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"}); cmd == nil {
		t.Fatal("expected a Cmd from enter")
	}
	if len(mgr.List()) != 1 {
		t.Fatalf("List() = %+v, want one started session", mgr.List())
	}
}

// TestFullLengthLocalPortRendersWhole pins the edit field's render width: the
// buffer holds the typed value either way, so only a rendered frame catches a
// field one cell too narrow for its own trailing cursor.
func TestFullLengthLocalPortRendersWhole(t *testing.T) {
	t.Parallel()
	m := newModel(podWithPort("default", "web-0", 80))
	m = loadPorts(t, m)

	updated, _ := m.Update(tea.KeyPressMsg{Text: "4"})
	updated, _ = updated.(*Model).Update(tea.PasteMsg{Content: "5678"})
	next := updated.(*Model)
	if got := next.rows[0].editInput.Value(); got != "45678" {
		t.Fatalf("buffer = %q, want %q", got, "45678")
	}
	if frame := goldentest.Plain(next.Render()); !strings.Contains(frame, "localhost:45678") {
		t.Errorf("a full-length local port did not render whole:\n%s", frame)
	}
}

// TestPasteOntoFreeRowOpensItsBuffer: a paste is how a `local:remote` pair
// usually arrives, so it opens the free-port row's buffer itself — it no
// longer needs a throwaway digit first that turned 8080:3000 into 18080:3000.
func TestPasteOntoFreeRowOpensItsBuffer(t *testing.T) {
	t.Parallel()
	m := newModel(barePod("default", "web-0"))
	m = loadPorts(t, m)

	updated, _ := m.Update(tea.PasteMsg{Content: "8080:3000"})
	row := updated.(*Model).rows[0]
	if !row.editing || row.editInput.Value() != "8080:3000" {
		t.Fatalf("row editing=%v buffer=%q, want the pasted pair in an open buffer", row.editing, row.editInput.Value())
	}
	if row.localPort != 8080 || row.Port != 3000 {
		t.Fatalf("row = local %d remote %d, want 8080 → 3000", row.localPort, row.Port)
	}
}
