package tui

import (
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/kute-dev/kute/internal/tui/components/textfield"
)

// PasteTarget receives pasted text. Screens build one with PasteInto (or
// PasteIntoArea) around whichever buffer currently has focus, and return nil
// when no buffer is open — RoutePaste then swallows the paste rather than
// letting it fall through to the screen's key handling.
type PasteTarget func(string)

// PasteInto builds a PasteTarget that inserts at in's cursor. Two ways to get
// a silent no-op out of this, both easy to write by accident:
//
//   - The returned closure holds the pointer, so a resolver that builds one
//     must be on a pointer receiver — a value receiver's copy would take the
//     insert away with it.
//   - in must be the *focused* buffer: textfield.Update returns early when
//     blurred, so pointing this at an open-but-blurred field drops the paste
//     with no error anywhere.
func PasteInto(in *textfield.Model) PasteTarget {
	if in == nil {
		return nil
	}
	return func(s string) {
		// Delegated, never reimplemented: textfield's own tea.PasteMsg path
		// sanitizes the content for a single-line field (tabs and the newline
		// a pasted value usually carries collapse to spaces) and handles
		// CharLimit. The Cmd it returns is dropped because on this path it
		// only ever carries cursor-blink ticks, which TextInputStyles pins
		// off anyway (Blink: false).
		*in, _ = in.Update(tea.PasteMsg{Content: s})
	}
}

// PasteIntoArea is PasteInto for a multi-line buffer (configmapdata's 17a
// value editor), where newlines in the pasted text are kept as newlines.
func PasteIntoArea(ta *textarea.Model) PasteTarget {
	if ta == nil {
		return nil
	}
	return func(s string) {
		*ta, _ = ta.Update(tea.PasteMsg{Content: s})
	}
}

// PasteDigits wraps a numeric field's PasteTarget (a replica count, a local
// port): surrounding whitespace is trimmed, then the paste is rejected whole
// if anything left isn't a digit. That all-or-nothing rule is the one those
// fields already apply per keypress — the trim is the only addition, so a
// pasted "8080\n" lands as 8080 instead of being thrown away.
func PasteDigits(target PasteTarget) PasteTarget {
	if target == nil {
		return nil
	}
	return func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, r := range s {
			if r < '0' || r > '9' {
				return
			}
		}
		target(s)
	}
}

// PastePortPair wraps the port-pair field's PasteTarget (13a's free-port row,
// whose buffer is kubectl's own `local:remote` spelling). PasteDigits' rule,
// widened by exactly the one separator that spelling uses: digits, with at
// most one colon and never leading or trailing, so a pasted "8080:3000" lands
// whole and a pasted "kubectl port-forward …" is still thrown away.
func PastePortPair(target PasteTarget) PasteTarget {
	if target == nil {
		return nil
	}
	return func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		colons := 0
		for _, r := range s {
			switch {
			case r == ':':
				colons++
			case r < '0' || r > '9':
				return
			}
		}
		if colons > 1 || strings.HasPrefix(s, ":") || strings.HasSuffix(s, ":") {
			return
		}
		target(s)
	}
}

// RoutePaste routes msg into target when msg is one of the three ways a paste
// reaches a screen:
//
//   - tea.PasteMsg — the terminal's own bracketed paste, which arrives as one
//     message and so never passes through a key handler at all;
//   - ctrl+v (or super+v) — answered with PasteCmd, because bubbles' built-in
//     handler for that chord replies with an unexported message type that no
//     screen outside that package can route to its buffer;
//   - tea.ClipboardMsg — PasteCmd's OSC52 fallback reply.
//
// Reports false for every other message, so callers fall through to their
// normal handling unchanged. With no buffer open the three cases differ on
// purpose: a bracketed paste is claimed and dropped (it is text, and there is
// nowhere for it to go), while the chord and a late OSC52 reply report false —
// the chord so a screen can still bind ctrl+v to something else, the reply so
// it can't land in a screen the user has since moved to.
func RoutePaste(msg tea.Msg, target PasteTarget) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		if target != nil {
			target(msg.Content)
		}
		return nil, true
	case tea.ClipboardMsg:
		// Only accepted while a buffer is focused: an OSC52 reply is
		// asynchronous, and the terminal may answer after the user has left
		// the screen that asked.
		if target == nil {
			return nil, false
		}
		target(msg.Content)
		return nil, true
	case tea.KeyPressMsg:
		if target == nil || !IsPasteKey(msg) {
			return nil, false
		}
		return PasteCmd(), true
	}
	return nil, false
}

// IsPasteKey reports whether msg is the paste chord. Terminals usually
// intercept the paste shortcut themselves and send bracketed paste instead —
// this is the path for the ones that pass it through.
func IsPasteKey(msg tea.KeyPressMsg) bool {
	if !msg.Mod.Contains(tea.ModCtrl) && !msg.Mod.Contains(tea.ModSuper) {
		return false
	}
	return msg.Code == 'v' || msg.Code == 'V'
}

// PasteCmd reads the clipboard and delivers it as the same tea.PasteMsg a
// bracketed paste produces, so the whole app has one paste message to route.
//
// The OS clipboard comes first (atotto/clipboard, the same reader bubbles
// uses). It fails on a host with no clipboard tool installed, and over SSH it
// would read the *remote* clipboard, so the fallback asks the terminal itself
// for its selection over OSC52 — which is the user's real clipboard in that
// case. A terminal that doesn't support clipboard reads simply never answers,
// leaving the buffer untouched.
func PasteCmd() tea.Cmd {
	return func() tea.Msg {
		if s, err := clipboard.ReadAll(); err == nil && s != "" {
			return tea.PasteMsg{Content: s}
		}
		return tea.ReadClipboard()
	}
}
