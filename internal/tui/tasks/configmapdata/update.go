package configmapdata

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

// focusedInput returns whichever of the add row's two buffers currently has
// focus, per a.onValue — shared by pasteTarget and updateAddKey's own default
// case.
func (a *addKeyState) focusedInput() *textfield.Model {
	if a.onValue {
		return &a.valueInput
	}
	return &a.keyInput
}

// pasteTarget is whichever add/edit buffer is open, for tui.RoutePaste. The
// 'e' multi-line editor takes the textarea variant, so a pasted multi-line
// value keeps its line breaks instead of being flattened the way a
// single-line field has to flatten them.
func (m *Model) pasteTarget() tui.PasteTarget {
	switch {
	case m.adding != nil:
		return tui.PasteInto(m.adding.focusedInput())
	case m.editing != nil:
		return tui.PasteInto(&m.editing.valueInput)
	case m.multiline != nil:
		return tui.PasteIntoArea(&m.multiline.textarea)
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
		// A consumer being added/removed elsewhere (or the ConfigMap itself
		// changing out from under us) should refresh both the grid and the
		// consumer strip.
		switch msg.Kind {
		case kube.KindConfigMap, kube.KindDeployment, kube.KindStatefulSet, kube.KindDaemonSet:
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

// cacheKinds is every kind load() reads: the ConfigMap itself plus the
// workload kinds its consumer strip is built from, which would otherwise
// render "no consumers" off a cache that hasn't filled yet.
var cacheKinds = append([]kube.ResourceKind{kube.KindConfigMap}, configMapConsumerKinds...)

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
	if !tui.KindsSynced(m.lister, m.namespace, cacheKinds...) {
		m.reloadEpoch++
		return m, tui.ScheduleCacheSyncRetry(m.reloadEpoch)
	}
	if err := tui.KindsError(m.lister, m.namespace, cacheKinds...); err != nil {
		m.state = tui.TaskStateError
		if kube.IsPermissionError(err) {
			m.state = tui.TaskStatePermissionDenied
		}
		m.feedback = err.Error()
		return m, nil
	}
	if !msg.found {
		m.state = tui.TaskStateError
		m.feedback = fmt.Sprintf("configmap %s/%s not found", m.namespace, m.name)
		return m, nil
	}
	m.keys = msg.keys
	m.consumers = msg.consumers
	m.state = tui.TaskStateReady
	m.feedback = ""
	switch {
	case m.focusKey != "":
		if idx, ok := indexOfConfigMapKey(m.keys, m.focusKey); ok {
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
	if m.multiline != nil {
		return m.updateMultilineKey(msg)
	}
	if msg.String() != "esc" {
		// A leftover "updated KEY"/error line from the last commit only
		// answers "what just happened" — stale the moment the user does
		// anything else, same rule secretdata's own updateKey uses.
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
	case verbs.AddConfigMapKey.Key, "insert":
		if m.mutator != nil && m.state == tui.TaskStateReady {
			theme := m.Theme()
			keyInput := textfield.New()
			keyInput.Prompt = ""
			keyInput.SetStyles(configMapInputStyles(theme))
			keyInput.Focus()
			valueInput := textfield.New()
			valueInput.Prompt = ""
			valueInput.SetStyles(configMapInputStyles(theme))
			m.adding = &addKeyState{keyInput: keyInput, valueInput: valueInput}
		}
	case "enter":
		if m.mutator != nil && m.state == tui.TaskStateReady {
			if row, ok := m.selectedKeyRow(); ok {
				m.beginEditRow(row)
			}
		}
	case "e":
		// 'e' explicitly opens the buffer editor — only meaningful (and only
		// offered in the keybar) on a multi-line row; on a single-line row
		// it's already reachable via '↵'.
		if m.mutator != nil && m.state == tui.TaskStateReady {
			if row, ok := m.selectedKeyRow(); ok && row.multiline() {
				m.beginEditRow(row)
			}
		}
	case verbs.RemoveConfigMapKey.Key:
		if m.mutator != nil && m.state == tui.TaskStateReady {
			if row, ok := m.selectedKeyRow(); ok {
				return m, m.beginRemove(row)
			}
		}
	}
	return m, nil
}

// beginEditRow opens the right editor for row's shape: the single-line
// in-place edit for a plain value, or the multi-line buffer editor
// (docs/design README.md §27a: "Multi-line keys ... e opens the buffer
// editor") when the value contains a newline — '↵' redirects there too
// rather than leaving multi-line rows with a dead Enter key. A value
// neither buffer can hold unchanged (tabs, CRLF, binary) is refused with a
// notice instead: it would open already normalized, so applying any edit —
// or none — would rewrite the whole value.
func (m *Model) beginEditRow(row configMapKeyRow) {
	if !textfield.RoundTrips(row.value) && !textareaRoundTrips(row.value) {
		m.notice = row.key + " has tabs, CRLF line endings or binary content · can't edit it here — use E on the ConfigMaps list"
		return
	}
	if !textfield.RoundTrips(row.value) {
		m.multiline = newMultilineEditState(row.key, row.value, m.Theme())
		return
	}
	valueInput := textfield.New()
	valueInput.Prompt = ""
	valueInput.SetStyles(configMapInputStyles(m.Theme()))
	valueInput.SetValue(row.value)
	valueInput.CursorEnd()
	valueInput.Focus()
	m.editing = &editKeyState{key: row.key, original: row.value, valueInput: valueInput}
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
// showing — every printable character inserts literally into whichever
// buffer has focus (tab/shift+tab switches). ctrl+r commits and restarts
// every consumer, the same alternate depth an edit's ctrl+r gives — a chord
// (verbs.RestartConfigMapConsumers), so no printable key ever commits.
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
		return m, m.commitAdd(false)
	case verbs.RestartConfigMapConsumers.Key:
		return m, m.commitAdd(true)
	default:
		var cmd tea.Cmd
		input := a.focusedInput()
		*input, cmd = input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// updateEditKey routes keys while '↵'s single-line in-place edit is showing
// — the key itself isn't editable here, only its value. esc reverts to the
// original value without applying anything. ctrl+r commits and restarts
// every consumer (docs/design README.md §27a: "ctrl-r chains the apply with
// kubectl rollout restart for every consuming workload").
func (m *Model) updateEditKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	e := m.editing
	switch msg.String() {
	case "esc":
		m.editing = nil
	case "enter":
		return m, m.commitEdit(false)
	case verbs.RestartConfigMapConsumers.Key:
		return m, m.commitEdit(true)
	default:
		var cmd tea.Cmd
		e.valueInput, cmd = e.valueInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

// updateMultilineKey routes keys while the buffer editor (multiline) is
// showing — the "simpler solution" this package substitutes for 17a's own
// shared buffer editor. Arrow keys move the cursor across lines/columns,
// enter inserts a newline (this screen's own commit key is alt+a/ctrl+r,
// not enter, since enter has to stay available for the buffer's own
// content — alt+a avoids terminal flow-control keys,
// backspace at column 0 joins with the previous line.
func (m *Model) updateMultilineKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	e := m.multiline
	switch msg.String() {
	case "esc":
		m.multiline = nil
	case "alt+a":
		return m, m.commitMultiline(false)
	case verbs.RestartConfigMapConsumers.Key:
		return m, m.commitMultiline(true)
	default:
		var cmd tea.Cmd
		e.textarea, cmd = e.textarea.Update(msg)
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

// beginCommit runs the shared "figure out the tier, hand it to
// actions.Controller.Begin" plumbing for an add/edit — restart carries
// through to Scope.ConfigMapRestartConsumers/ConfigMapConsumers so the
// controller's "configmap-data" case (internal/tui/actions/controller.go)
// chains a RolloutRestart per consumer after a successful patch.
func (m *Model) beginCommit(key, value string, isEdit bool, original string, restart bool) tea.Cmd {
	m.pendingCommit = &configMapPendingCommit{
		key: key, value: value, isEdit: isEdit, original: original,
		restartConsumers: restart, consumers: m.consumers,
	}
	m.message, m.lastError = "", ""
	tier := verbs.TierForConfigMapData(m.isProd())
	label := fmt.Sprintf("Add key %s to %s?", key, m.name)
	id := "add-configmap-key-" + m.namespace + "/" + m.name + "/" + key
	if isEdit {
		label = fmt.Sprintf("Update key %s on %s?", key, m.name)
		id = "edit-configmap-key-" + m.namespace + "/" + m.name + "/" + key
	}
	return m.actions.Begin(tier, tui.TaskAction{
		ID:    id,
		Label: label,
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindConfigMap), ResourceName: m.name, Namespace: m.namespace,
			Verb: "configmap-data", IsMutating: true,
			ConfigMapKey: key, ConfigMapValue: value,
			ConfigMapRestartConsumers: restart, ConfigMapConsumers: consumerRefs(m.consumers),
		},
	})
}

// commitAdd executes the add row's key=value — a no-op while the key buffer
// is blank.
func (m *Model) commitAdd(restart bool) tea.Cmd {
	a := m.adding
	key := strings.TrimSpace(a.keyInput.Value())
	if key == "" {
		return nil
	}
	value := a.valueInput.Value()
	m.adding = nil
	return m.beginCommit(key, value, false, "", restart)
}

// commitEdit executes the single-line edit row's rewritten value — a no-op
// when the value is unchanged from its original.
func (m *Model) commitEdit(restart bool) tea.Cmd {
	e := m.editing
	if !e.changed() {
		m.editing = nil
		return nil
	}
	key, value, original := e.key, e.valueInput.Value(), e.original
	m.editing = nil
	return m.beginCommit(key, value, true, original, restart)
}

// commitMultiline executes the buffer editor's rewritten value — a no-op
// when unchanged from its original.
func (m *Model) commitMultiline(restart bool) tea.Cmd {
	e := m.multiline
	if !e.changed() {
		m.multiline = nil
		return nil
	}
	key, value, original := e.key, e.value(), e.original
	m.multiline = nil
	return m.beginCommit(key, value, true, original, restart)
}

// beginRemove executes a key removal — always TierInline regardless of PROD
// (docs/design README.md §27a inherits 27b's own "removing a key keeps the
// y/N too" policy), never chained with a restart (ctrl-r's own restart
// chaining is described only for the value apply, not a removal).
func (m *Model) beginRemove(row configMapKeyRow) tea.Cmd {
	m.pendingCommit = &configMapPendingCommit{key: row.key, remove: true}
	m.message, m.lastError = "", ""
	return m.actions.Begin(actions.TierInline, tui.TaskAction{
		ID:    "remove-configmap-key-" + m.namespace + "/" + m.name + "/" + row.key,
		Label: fmt.Sprintf("Remove key %s from %s?", row.key, m.name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindConfigMap), ResourceName: m.name, Namespace: m.namespace,
			Verb: "configmap-data", IsMutating: true,
			ConfigMapKey: row.key, ConfigMapRemove: true,
		},
	})
}

// handleResult applies an add/edit/remove action's outcome — never leaves
// the screen, per docs/design README.md §27a's own "confirm → execute →
// refresh → show result → remain on screen" contract (26a/27b's own
// established shape). On success the ConfigMap (and its consumer list) is
// re-fetched and focus follows the touched key. On failure nothing is
// refetched: a failed add/edit re-opens its own editor with the attempted
// value intact, and the server's error is surfaced via lastError.
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
			if strings.Contains(pc.value, "\n") || strings.Contains(pc.original, "\n") {
				m.multiline = newMultilineEditState(pc.key, pc.value, m.Theme())
			} else {
				valueInput := textfield.New()
				valueInput.Prompt = ""
				valueInput.SetStyles(configMapInputStyles(m.Theme()))
				valueInput.SetValue(pc.value)
				valueInput.CursorEnd()
				valueInput.Focus()
				m.editing = &editKeyState{key: pc.key, original: pc.original, valueInput: valueInput}
			}
		default:
			theme := m.Theme()
			keyInput := textfield.New()
			keyInput.Prompt = ""
			keyInput.SetStyles(configMapInputStyles(theme))
			keyInput.SetValue(pc.key)
			keyInput.CursorEnd()
			valueInput := textfield.New()
			valueInput.Prompt = ""
			valueInput.SetStyles(configMapInputStyles(theme))
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
	case pc.isEdit:
		m.message = "updated " + pc.key
	default:
		m.message = "added " + pc.key
	}
	if pc.restartConsumers && len(pc.consumers) > 0 {
		m.message += fmt.Sprintf(" · restarted %d consumer", len(pc.consumers))
		if len(pc.consumers) != 1 {
			m.message += "s"
		}
	}
	m.focusKey = pc.key
	m.reloadEpoch++
	return m.load()
}

// consumerRefs strips configMapConsumer's display-only refKind down to the
// bare kube.ConfigMapConsumerRef list TaskScope.ConfigMapConsumers carries.
func consumerRefs(consumers []configMapConsumer) []kube.ConfigMapConsumerRef {
	if len(consumers) == 0 {
		return nil
	}
	out := make([]kube.ConfigMapConsumerRef, len(consumers))
	for i, c := range consumers {
		out[i] = c.ConfigMapConsumerRef
	}
	return out
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
