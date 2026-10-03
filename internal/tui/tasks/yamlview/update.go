package yamlview

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// pasteTarget is the '/' search buffer while it's open, jumping to the
// first match inside the closure exactly as updateSearchKey does after a typed
// character.
func (m *Model) pasteTarget() tui.PasteTarget {
	if !m.searchActive {
		return nil
	}
	insert := tui.PasteInto(&m.searchInput)
	return func(s string) {
		insert(s)
		m.jumpToMatch()
	}
}

// Reload implements tui.Reloader — see its doc comment: this screen misses
// every kube.ResourceChangedMsg while parked in the stack, so BackMsg
// restoring it asks it to catch up immediately rather than showing stale
// data until an unrelated change happens to land while it's active again.
func (m *Model) Reload() tea.Cmd {
	if m.lister == nil {
		return nil
	}
	m.loadEpoch++
	return m.load()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cmd, ok := tui.RoutePaste(msg, m.pasteTarget()); ok {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ResourceChangedMsg:
		if msg.Kind == m.kind && m.lister != nil {
			m.loadEpoch++
			return m, m.load()
		}
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
	case loadedMsg:
		return m.applyLoaded(msg)
	case spinner.TickMsg:
		if m.state != tui.TaskStateLoading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	}
	return m, nil
}

func (m *Model) applyLoaded(msg loadedMsg) (tea.Model, tea.Cmd) {
	if msg.epoch != m.loadEpoch {
		return m, nil
	}
	if msg.err != nil {
		m.state = tui.TaskStateError
		if kube.IsPermissionError(msg.err) {
			m.state = tui.TaskStatePermissionDenied
		}
		m.feedback = msg.err.Error()
		return m, nil
	}
	text, copyText := msg.text, msg.text
	if m.kind == kube.KindSecret {
		// Mask before anything is stored: m.lines feeds the view, the fold
		// index and '/' search, so it must never hold a Secret value
		// (secret.go). An unparseable Secret fails closed.
		m.isSecret = true
		masked, err := maskSecretYAML(msg.text)
		if err != nil {
			masked = maskedSecret{text: secretUnparseableNotice, secretType: "Opaque"}
		}
		text, copyText = masked.text, masked.copyText
		m.secretType = masked.secretType
		m.secretData = masked.entries
		if m.revealed == nil {
			m.revealed = map[string]bool{}
		}
	}
	m.lines = strings.Split(text, "\n")
	m.copyText = copyText
	m.resourceVersion = msg.resourceVersion
	if !msg.keepManagedFields {
		m.managedFieldsLines = splitManagedFieldsLines(msg.managedFieldsYAML)
	}
	if m.folded == nil {
		m.folded = defaultFolds(m.lines)
		if len(m.managedFieldsLines) > 0 {
			m.folded["managedFields"] = true
		}
	}
	n := len(m.rendered())
	if m.cursor >= n {
		m.cursor = max(n-1, 0)
	}
	m.clampOffset()
	m.state = tui.TaskStateReady
	m.feedback = ""
	return m, nil
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.searchActive {
		return m.updateSearchKey(msg)
	}
	if m.revealAllConfirm {
		return m.updateRevealAllConfirmKey(msg)
	}
	switch msg.String() {
	case "ctrl+q", "ctrl+c":
		return m, tea.Quit
	case "esc", "backspace":
		return m, func() tea.Msg { return tui.BackMsg{} }
	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case verbs.YAMLFold.Key:
		if m.folded != nil {
			rendered := m.rendered()
			// FoldableKey (managedfields.go) marks an expanded header not
			// backed by a real Model.lines line — toggleFoldAtCursor's
			// topLevelBlocks lookup can't find it, so fold it directly.
			if m.cursor >= 0 && m.cursor < len(rendered) && rendered[m.cursor].FoldableKey != "" {
				m.folded[rendered[m.cursor].FoldableKey] = true
			} else {
				toggleFoldAtCursor(m.lines, m.folded, rendered, m.cursor)
			}
			m.clampCursor()
		}
	case verbs.YAMLUnfoldAll.Key:
		if m.folded != nil {
			unfoldAll(m.folded)
			m.clampCursor()
		}
	case verbs.Search.Key:
		m.searchActive = true
		m.searchInput = textfield.New()
		m.searchInput.SetStyles(tui.TextInputStyles(m.Theme()))
		m.searchInput.Prompt = ""
		m.searchInput.Focus()
	case verbs.YAMLCopy.Key:
		// copyText, not m.lines: for a Secret, m.lines is the masked
		// rendering, while §21a's full-YAML copy keeps values base64
		// (with stringData folded into data and last-applied dropped, so
		// it never carries plaintext — secret.go's maskedSecret.copyText).
		if m.copyText == "" {
			return m, nil
		}
		return m, tea.SetClipboard(m.copyText)
	case verbs.SecretRevealToggle.Key:
		if m.isSecret {
			m.toggleRevealAtCursor()
		}
	case verbs.SecretRevealAll.Key:
		if m.isSecret && m.hasUnrevealedSecretData() {
			m.revealAllConfirm = true
		}
	case verbs.SecretCopyValue.Key:
		if m.isSecret {
			return m, m.copyDecodedSecretValue()
		}
	}
	return m, nil
}

// toggleRevealAtCursor flips the mask/reveal state of the data: entry the
// cursor is currently on (a no-op everywhere else — most lines carry no
// SecretKey). Docs/design README.md §21a: "x reveals/masks the cursor key
// in place".
func (m *Model) toggleRevealAtCursor() {
	rendered := m.rendered()
	if m.cursor < 0 || m.cursor >= len(rendered) {
		return
	}
	key := rendered[m.cursor].SecretKey
	if key == "" {
		return
	}
	if m.revealed == nil {
		m.revealed = map[string]bool{}
	}
	m.revealed[key] = !m.revealed[key]
	m.clampCursor()
}

func (m *Model) hasUnrevealedSecretData() bool {
	return slices.ContainsFunc(m.secretData, func(e secretDataLine) bool { return !m.revealed[e.id] })
}

// updateRevealAllConfirmKey routes keys while the "X reveal all" inline y/N
// gate is showing (§21a: "X reveals all behind an inline y/N").
func (m *Model) updateRevealAllConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == verbs.ConfirmAccept.Key {
		if m.revealed == nil {
			m.revealed = map[string]bool{}
		}
		for _, e := range m.secretData {
			m.revealed[e.id] = true
		}
	}
	m.revealAllConfirm = false
	return m, nil
}

// revealAllConfirmPrompt renders the confirm keybar's RightNote — mirrors
// browse/edit.go's editConfirmPrompt shape for this package's own bespoke
// (non-actions.Controller) inline gate.
func (m Model) revealAllConfirmPrompt() string {
	return verbs.ConfirmPrompt(fmt.Sprintf("Reveal all %d keys in this Secret?", len(m.secretData)))
}

// copyDecodedSecretValue copies the cursor's decoded plaintext — "the only
// plaintext export" (§21a) — regardless of whether it's currently masked or
// revealed on screen; decoding already happened at load, in memory only.
func (m Model) copyDecodedSecretValue() tea.Cmd {
	rendered := m.rendered()
	if m.cursor < 0 || m.cursor >= len(rendered) {
		return nil
	}
	key := rendered[m.cursor].SecretKey
	if key == "" {
		return nil
	}
	for _, e := range m.secretData {
		if e.id == key && e.decodeOK {
			return tea.SetClipboard(string(e.decoded))
		}
	}
	return nil
}

func (m *Model) updateSearchKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "enter":
		m.searchActive = false
		m.searchInput.Blur()
	case "up":
		m.moveCursor(-1)
	case "down":
		m.moveCursor(1)
	default:
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)
		m.jumpToMatch()
		return m, cmd
	}
	return m, nil
}

// jumpToMatch moves the cursor to the nearest case-insensitive match at or
// after the current position, wrapping to the top if none is found below —
// mirrors browse's "/" filter UX (docs/design README.md §8a: "/ searches
// (jump + highlight)").
func (m *Model) jumpToMatch() {
	if m.searchInput.Value() == "" {
		return
	}
	query := strings.ToLower(m.searchInput.Value())
	rendered := m.rendered()
	for i := m.cursor; i < len(rendered); i++ {
		if strings.Contains(strings.ToLower(rendered[i].Text), query) {
			m.cursor = i
			m.clampOffset()
			return
		}
	}
	for i := range m.cursor {
		if strings.Contains(strings.ToLower(rendered[i].Text), query) {
			m.cursor = i
			m.clampOffset()
			return
		}
	}
}

func (m *Model) moveCursor(delta int) {
	n := len(m.rendered())
	if n == 0 {
		m.cursor, m.offset = 0, 0
		return
	}
	m.cursor = clamp(m.cursor+delta, 0, n-1)
	m.clampOffset()
}

func (m *Model) clampCursor() {
	n := len(m.rendered())
	m.cursor = clamp(m.cursor, 0, max(n-1, 0))
	m.clampOffset()
}

// bodyRows is how many content rows the viewport shows — kept in sync with
// view.go's own body-height budget.
func (m Model) bodyRows() int {
	return max(tui.FrameBodyHeight(m.height, len(m.Strips(m.width))), 1)
}

func (m *Model) clampOffset() {
	rows := m.bodyRows()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+rows {
		m.offset = m.cursor - rows + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
