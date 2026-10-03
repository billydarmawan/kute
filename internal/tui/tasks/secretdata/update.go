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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	case actions.RunningMsg:
		// A slow write's keybar line; the returned command is the only
		// path its ResultMsg arrives by.
		return m, m.actions.HandleRunning(msg)
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
	m.resourceVersion = m.ownWrites.Resolve(msg.resourceVersion)
	if m.bufferRVFollows {
		m.bufferRV = m.resourceVersion
	}
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
			m.openBuffer()
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
				m.openBuffer()
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
		// Cancelling walks back exactly one level — to the buffer the
		// commit came from, with the typed value intact (a removal has
		// none: its row never moved).
		m.actions.Cancel()
		pc := m.pendingCommit
		m.pendingCommit = nil
		m.restoreBuffer(pc)
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
	case verbs.SecretMaskToggle.Key:
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
	case verbs.SecretMaskToggle.Key:
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
// in PROD (docs/design README.md §27b, verbs.Resolve's AddSecretKey row). A no-op
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
	m.pendingCommit = &secretPendingCommit{key: key, value: value, overwrite: overwrite, masked: a.masked, resourceVersion: m.bufferRV}
	m.message, m.lastError = "", ""
	label := fmt.Sprintf("Add key %s to %s?", key, m.name)
	if overwrite {
		// 'a' with a key that's already there replaces its value — for a
		// Secret often the only copy of a credential — and the merge patch
		// can't tell an add from an overwrite, so ask first.
		label = fmt.Sprintf("Overwrite existing key %s on %s?", key, m.name)
	}
	return m.actions.Begin(actions.Request{Verb: verbs.AddSecretKey, Prod: m.isProd(), Overwrite: overwrite}, tui.TaskAction{
		ID:    "add-secret-key-" + m.namespace + "/" + m.name + "/" + key,
		Label: label,
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindSecret), ResourceName: m.name, Namespace: m.namespace,
			Verb: "secret-data", IsMutating: true,
			SecretKey: key, SecretValue: value, DataResourceVersion: m.bufferRV,
		},
	})
}

// commitEdit executes the edit row's rewritten value through
// actions.Controller — same TierNone-outside-PROD/TierInline-in-PROD policy
// as commitAdd (verbs.Resolve's AddSecretKey row; both go through the same
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
	m.pendingCommit = &secretPendingCommit{key: key, value: value, isEdit: true, original: original, masked: e.masked, resourceVersion: m.bufferRV}
	m.message, m.lastError = "", ""
	return m.actions.Begin(actions.Request{Verb: verbs.AddSecretKey, Prod: m.isProd()}, tui.TaskAction{
		ID:    "edit-secret-key-" + m.namespace + "/" + m.name + "/" + key,
		Label: fmt.Sprintf("Update key %s on %s?", key, m.name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindSecret), ResourceName: m.name, Namespace: m.namespace,
			Verb: "secret-data", IsMutating: true,
			SecretKey: key, SecretValue: value, DataResourceVersion: m.bufferRV,
		},
	})
}

// beginRemove executes a key removal through actions.Controller — always
// TierInline regardless of PROD (docs/design README.md §27b: "removing a
// key keeps the y/N too"), never escalated further to a type-the-name
// modal, the same policy 26a's meta.go removal uses.
func (m *Model) beginRemove(row secretKeyRow) tea.Cmd {
	m.pendingCommit = &secretPendingCommit{key: row.key, remove: true, resourceVersion: m.resourceVersion}
	m.message, m.lastError = "", ""
	return m.actions.Begin(actions.Request{Verb: verbs.RemoveSecretKey, Prod: m.isProd()}, tui.TaskAction{
		ID:    "remove-secret-key-" + m.namespace + "/" + m.name + "/" + row.key,
		Label: fmt.Sprintf("Remove key %s from %s?", row.key, m.name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindSecret), ResourceName: m.name, Namespace: m.namespace,
			Verb: "secret-data", IsMutating: true,
			SecretKey: row.key, SecretRemove: true, DataResourceVersion: m.resourceVersion,
		},
	})
}

// openBuffer pins a just-opened add/edit buffer's patch precondition to
// the Secret as it is on screen right now.
func (m *Model) openBuffer() {
	m.bufferRV = m.resourceVersion
	m.bufferRVFollows = false
}

// conflictMessage is the result line for a commit the API server refused
// with 409 Conflict: the Secret changed after the buffer opened (or, for a
// removal, after the row was read). It names the object and key only,
// never a value.
func conflictMessage(name string, pc *secretPendingCommit) string {
	retry := "↵ again applies your edit to the latest version"
	if pc != nil && pc.remove {
		retry = verbs.RemoveSecretKey.Key + " again removes " + pc.key + " from the latest version"
	}
	return name + " changed on the server since you opened it — reloaded · " + retry
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
// A 409 Conflict (the Secret changed after the buffer opened — every patch
// carries the observed resourceVersion as a precondition) is the one
// failure that does refetch: the buffer is restored as for any failure, the
// result line says the Secret changed, and the restored buffer follows the
// reload's resourceVersion so ↵ again applies over the latest version.
func (m *Model) handleResult(msg actions.ResultMsg) tea.Cmd {
	pc := m.pendingCommit
	m.pendingCommit = nil
	if msg.Err != nil {
		m.lastError = msg.Err.Error()
		m.message = ""
		conflict := apierrors.IsConflict(msg.Err)
		if conflict {
			m.lastError = conflictMessage(m.name, pc)
			// The server has moved past every version this screen wrote:
			// trust the reload.
			m.ownWrites.Forget()
		}
		if pc != nil {
			m.bufferRV = pc.resourceVersion
		}
		m.bufferRVFollows = conflict
		m.restoreBuffer(pc)
		if conflict {
			m.reloadEpoch++
			return m.load()
		}
		return nil
	}
	m.lastError = ""
	if pc == nil {
		return nil
	}
	m.recordWrite(pc, msg.DataResourceVersion)
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

// restoreBuffer reopens the add/edit row a commit came from, holding the
// value it tried to write and masked or revealed as it was — after a failed
// apply or a cancelled confirm. A removal (or no commit at all) has no
// buffer: its row is still right there, unmoved.
func (m *Model) restoreBuffer(pc *secretPendingCommit) {
	switch {
	case pc == nil || pc.remove:
	case pc.isEdit:
		valueInput := newSecretInput(m.Theme())
		valueInput.SetValue(pc.value)
		valueInput.CursorEnd()
		valueInput.Focus()
		m.editing = &editKeyState{key: pc.key, original: pc.original, valueInput: valueInput, masked: pc.masked}
	default:
		theme := m.Theme()
		keyInput := newSecretInput(theme)
		keyInput.SetValue(pc.key)
		keyInput.CursorEnd()
		valueInput := newSecretInput(theme)
		valueInput.SetValue(pc.value)
		valueInput.CursorEnd()
		valueInput.Focus()
		m.adding = &addKeyState{keyInput: keyInput, valueInput: valueInput, onValue: true, masked: pc.masked}
	}
}

// recordWrite notes a landed patch's returned resourceVersion and makes it
// current at once, so a buffer opened before the reload (or the reload of a
// cache that hasn't caught up yet) preconditions on the server's version
// after this write, not the one before it.
func (m *Model) recordWrite(pc *secretPendingCommit, after string) {
	if pc == nil || after == "" {
		return
	}
	m.ownWrites.Wrote(pc.resourceVersion, after)
	m.resourceVersion = after
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
