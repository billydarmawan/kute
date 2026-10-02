package secretdata

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// focusedAddInput returns whichever of the add row's two buffers currently
// has focus, per a.onValue — shared by pasteTarget and updateAddKey's own
// default case.
func (a *addKeyState) focusedInput() *textfield.Model {
	if a.onValue {
		return &a.valueInput
	}
	return &a.keyInput
}

// pasteTarget is whichever add/edit buffer is open, for tui.RoutePaste.
// docs/design README.md §27b keys this screen with "ctrl-v paste (never
// echoed to scrollback)": the app renders full-alt-screen (tui.Model.View
// sets AltScreen), so nothing that lands here reaches scrollback regardless
// of which paste path delivered it.
func (m *Model) pasteTarget() tui.PasteTarget {
	switch {
	case m.adding != nil:
		return tui.PasteInto(m.adding.focusedInput())
	case m.editing != nil:
		return tui.PasteInto(&m.editing.valueInput)
	}
	return nil
}

// Reload implements tui.Reloader — see its doc comment: this screen misses
// every kube.ResourceChangedMsg while parked in the stack, so BackMsg
// restoring it asks it to catch up immediately rather than showing stale
// data until an unrelated change happens to land while it's active again.
func (m *Model) Reload() tea.Cmd {
	m.reloadEpoch++
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
		if msg.Kind == kube.KindSecret {
			m.reloadEpoch++
			return m, m.load()
		}
	case tui.CacheSyncRetryMsg:
		if msg.Gen == m.reloadEpoch {
			return m, m.load()
		}
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
		m.actions.SetOffline(m.conn.Offline())
	case loadedMsg:
		return m.applyLoaded(msg)
	case spinner.TickMsg:
		if m.state != tui.TaskStateLoading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case actions.ResultMsg:
		m.actions.HandleResult(msg)
		// This screen's own will-run strip is the result's surface (the
		// spec'd inline result line), so the keybar doesn't repeat it.
		m.actions.DismissResult()
		return m, m.handleResult(msg)
	case tea.KeyPressMsg:
		// Any key acknowledges the last action's outcome line; the key
		// still does what it would have.
		m.actions.DismissResult()
		return m.updateKey(msg)
	}
	return m, nil
}

func (m *Model) applyLoaded(msg loadedMsg) (tea.Model, tea.Cmd) {
	if msg.epoch != m.reloadEpoch {
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
	// The first read of a kind starts its informer and returns whatever the
	// cache holds so far — often nothing yet. Hold the spinner until the
	// cache has filled (CLAUDE.md: an empty or not-found state is a claim
	// about the cluster), and surface why a cache stopped filling rather than
	// calling the object missing.
	if !tui.KindsSynced(m.lister, m.namespace, kube.KindSecret) {
		m.reloadEpoch++
		return m, tui.ScheduleCacheSyncRetry(m.reloadEpoch)
	}
	if err := tui.KindsError(m.lister, m.namespace, kube.KindSecret); err != nil {
		m.state = tui.TaskStateError
		if kube.IsPermissionError(err) {
			m.state = tui.TaskStatePermissionDenied
		}
		m.feedback = err.Error()
		return m, nil
	}
	if !msg.found {
		m.state = tui.TaskStateError
		m.feedback = fmt.Sprintf("secret %s/%s not found", m.namespace, m.name)
		return m, nil
	}
	m.secretType = msg.secretType
	m.keys = msg.keys
	m.state = tui.TaskStateReady
	m.feedback = ""
	switch {
	case m.focusKey != "":
		if idx, ok := indexOfSecretKey(m.keys, m.focusKey); ok {
			m.selected = idx
		} else {
			m.selected = clamp(m.selected, 0, max(len(m.keys)-1, 0))
		}
		m.focusKey = ""
	case m.selected >= len(m.keys):
		m.selected = max(len(m.keys)-1, 0)
	}
	return m, nil
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.actions.Active() {
		return m.updateConfirmKey(msg)
	}
	if m.adding != nil {
		return m.updateAddKey(msg)
	}
	if m.editing != nil {
		return m.updateEditKey(msg)
	}
	if msg.String() != "esc" {
		// A leftover "added SMTP_PASSWORD"/error line from the last commit
		// only answers "what just happened" — stale the moment the user
		// does anything else, same rule 26a's meta.go uses.
		m.message, m.lastError, m.notice = "", "", ""
	}
	switch msg.String() {
	case "ctrl+q", "ctrl+c":
		return m, tea.Quit
	case "esc", "backspace":
		return m, func() tea.Msg { return tui.BackMsg{} }
	case "up", "k":
		m.moveSelection(-1)
	case "down", "j":
		m.moveSelection(1)
	case "ctrl+d":
		m.moveHalfPage(1)
	case "ctrl+u":
		m.moveHalfPage(-1)
	case verbs.AddSecretKey.Key, "insert":
		if m.mutator != nil && m.state == tui.TaskStateReady {
			theme := m.Theme()
			keyInput := newSecretInput(theme)
			keyInput.Focus()
			m.adding = &addKeyState{keyInput: keyInput, valueInput: newSecretInput(theme)}
		}
	case "enter":
		if m.mutator != nil && m.state == tui.TaskStateReady {
			if row, ok := m.selectedKeyRow(); ok {
				if !textfield.RoundTrips(row.value) {
					// A PEM, SSH key or binary keystore can't survive the
					// single-line buffer: it would open already flattened,
					// and ↵ on the untouched buffer would write that back.
					// Refuse up front, naming the key only — never the value.
					m.notice = row.key + " is multi-line or binary · can't edit it here — use E on the Secrets list"
					return m, nil
				}
				valueInput := newSecretInput(m.Theme())
				valueInput.SetValue(row.value)
				valueInput.CursorEnd()
				valueInput.Focus()
				m.editing = &editKeyState{key: row.key, original: row.value, valueInput: valueInput}
			}
		}
	case verbs.RemoveSecretKey.Key:
		if m.mutator != nil && m.state == tui.TaskStateReady {
			if row, ok := m.selectedKeyRow(); ok {
				return m, m.beginRemove(row)
			}
		}
	}
	return m, nil
}

func (m *Model) updateConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y":
		return m, m.actions.Confirm()
	case "n", "esc":
		m.actions.Cancel()
	}
	return m, nil
}

// updateAddKey routes keys while 'a'/insert's line-insert add row is
// showing — a text-entry context, so every printable character (including
// 'x') inserts literally into whichever buffer has focus; ctrl-x toggles
// the value's mask (docs/design README.md §27b: "ctrl-x re-mask input" —
// a chorded key so plain 'x' stays available to type, the same reasoning
// that makes configmapdata's apply+restart ctrl-r rather than a bare R).
// tab/shift+tab move focus between the key and value buffers, the same
// two-buffer shape meta.go's own add sub-flow uses.
func (m *Model) updateAddKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	a := m.adding
	switch msg.String() {
	case "esc":
		m.adding = nil
	case "tab":
		a.onValue = true
		a.keyInput.Blur()
		a.valueInput.Focus()
	case "shift+tab":
		a.onValue = false
		a.valueInput.Blur()
		a.keyInput.Focus()
	case "enter":
		return m, m.commitAdd()
	case "ctrl+x":
		if a.onValue {
			a.masked = !a.masked
		} else {
			var cmd tea.Cmd
			a.keyInput, cmd = a.keyInput.Update(msg)
			return m, cmd
		}
	default:
		var cmd tea.Cmd
		input := a.focusedInput()
		*input, cmd = input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// updateEditKey routes keys while '↵'s decode-then-edit row is showing —
// a single-buffer text-entry context (the key itself isn't editable here),
// so every printable character inserts literally; ctrl-x toggles the mask,
// same as the add row. esc reverts to the original decoded value without
// applying anything.
func (m *Model) updateEditKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	e := m.editing
	switch msg.String() {
	case "esc":
		m.editing = nil
	case "enter":
		return m, m.commitEdit()
	case "ctrl+x":
		e.masked = !e.masked
	default:
		var cmd tea.Cmd
		e.valueInput, cmd = e.valueInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *Model) moveSelection(delta int) {
	if len(m.keys) == 0 {
		m.selected = 0
		return
	}
	m.selected = clamp(m.selected+delta, 0, len(m.keys)-1)
}

func (m *Model) moveHalfPage(direction int) {
	position := components.MoveHalfPage(m.selected, 0, 8, len(m.keys), direction)
	m.selected = position.Selected
}

// commitAdd executes the add row's key=value through actions.Controller —
// TierNone (applies immediately) outside PROD, TierInline (inline y/N)
// in PROD (docs/design README.md §27b, verbs.TierForAddSecretKey). A no-op
// while the key buffer is blank.
func (m *Model) commitAdd() tea.Cmd {
	a := m.adding
	key := strings.TrimSpace(a.keyInput.Value())
	if key == "" {
		return nil
	}
	value := a.valueInput.Value()
	m.adding = nil
	_, overwrite := indexOfSecretKey(m.keys, key)
	m.pendingCommit = &secretPendingCommit{key: key, value: value, overwrite: overwrite}
	m.message, m.lastError = "", ""
	tier := verbs.TierForAddSecretKey(m.isProd())
	label := fmt.Sprintf("Add key %s to %s?", key, m.name)
	if overwrite {
		// 'a' with a key that's already there replaces its value — for a
		// Secret often the only copy of a credential — and the merge patch
		// can't tell an add from an overwrite, so ask first.
		tier = max(tier, actions.TierInline)
		label = fmt.Sprintf("Overwrite existing key %s on %s?", key, m.name)
	}
	return m.actions.Begin(tier, tui.TaskAction{
		ID:    "add-secret-key-" + m.namespace + "/" + m.name + "/" + key,
		Label: label,
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindSecret), ResourceName: m.name, Namespace: m.namespace,
			Verb: "secret-data", IsMutating: true,
			SecretKey: key, SecretValue: value,
		},
	})
}

// commitEdit executes the edit row's rewritten value through
// actions.Controller — same TierNone-outside-PROD/TierInline-in-PROD policy
// as commitAdd (verbs.TierForAddSecretKey; both go through the same
// PatchSecretData call, so the same tiering applies). A no-op when the
// value is unchanged from its original decoded value — nothing to apply.
func (m *Model) commitEdit() tea.Cmd {
	e := m.editing
	if !e.changed() {
		m.editing = nil
		return nil
	}
	key, value, original := e.key, e.valueInput.Value(), e.original
	m.editing = nil
	m.pendingCommit = &secretPendingCommit{key: key, value: value, isEdit: true, original: original}
	m.message, m.lastError = "", ""
	tier := verbs.TierForAddSecretKey(m.isProd())
	return m.actions.Begin(tier, tui.TaskAction{
		ID:    "edit-secret-key-" + m.namespace + "/" + m.name + "/" + key,
		Label: fmt.Sprintf("Update key %s on %s?", key, m.name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindSecret), ResourceName: m.name, Namespace: m.namespace,
			Verb: "secret-data", IsMutating: true,
			SecretKey: key, SecretValue: value,
		},
	})
}

// beginRemove executes a key removal through actions.Controller — always
// TierInline regardless of PROD (docs/design README.md §27b: "removing a
// key keeps the y/N too"), never escalated further to a type-the-name
// modal, the same policy 26a's meta.go removal uses.
func (m *Model) beginRemove(row secretKeyRow) tea.Cmd {
	m.pendingCommit = &secretPendingCommit{key: row.key, remove: true}
	m.message, m.lastError = "", ""
	return m.actions.Begin(actions.TierInline, tui.TaskAction{
		ID:    "remove-secret-key-" + m.namespace + "/" + m.name + "/" + row.key,
		Label: fmt.Sprintf("Remove key %s from %s?", row.key, m.name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindSecret), ResourceName: m.name, Namespace: m.namespace,
			Verb: "secret-data", IsMutating: true,
			SecretKey: row.key, SecretRemove: true,
		},
	})
}

// handleResult applies an add/edit/remove action's outcome — update.go's
// actions.ResultMsg case calls this instead of ever leaving the screen,
// per docs/design README.md §27b's own contract: "confirm → execute →
// refresh → show result → remain on screen." On success the Secret is
// re-fetched (never an optimistic local patch) and focus follows the
// touched key — the same row again after an add or edit, or the nearest
// remaining row after a removal (applyLoaded's own fallback once
// indexOfSecretKey can't find it, since it's gone). On failure nothing is
// refetched: a failed add/edit re-opens its own row with the attempted
// value intact, and the server's error is surfaced via lastError (view.go's
// will-run strip) — never the value, per this screen's own no-leak rule.
func (m *Model) handleResult(msg actions.ResultMsg) tea.Cmd {
	pc := m.pendingCommit
	m.pendingCommit = nil
	if msg.Err != nil {
		m.lastError = msg.Err.Error()
		m.message = ""
		switch {
		case pc == nil || pc.remove:
			// A failed removal has no buffer to restore — the row is still
			// right there, unmoved.
		case pc.isEdit:
			valueInput := newSecretInput(m.Theme())
			valueInput.SetValue(pc.value)
			valueInput.CursorEnd()
			valueInput.Focus()
			m.editing = &editKeyState{key: pc.key, original: pc.original, valueInput: valueInput}
		default:
			theme := m.Theme()
			keyInput := newSecretInput(theme)
			keyInput.SetValue(pc.key)
			keyInput.CursorEnd()
			valueInput := newSecretInput(theme)
			valueInput.SetValue(pc.value)
			valueInput.CursorEnd()
			valueInput.Focus()
			m.adding = &addKeyState{keyInput: keyInput, valueInput: valueInput, onValue: true}
		}
		return nil
	}
	m.lastError = ""
	if pc == nil {
		return nil
	}
	switch {
	case pc.remove:
		m.message = "removed " + pc.key
	case pc.isEdit, pc.overwrite:
		m.message = "updated " + pc.key
	default:
		m.message = "added " + pc.key
	}
	m.focusKey = pc.key
	m.reloadEpoch++
	return m.load()
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
