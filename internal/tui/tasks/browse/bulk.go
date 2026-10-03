// 20a bulk operations (docs/design README.md §20a): space marks the cursor
// row and advances, * marks every row the current filter matches
// ("filter-then-mark is the bulk grammar — no range-mark chord"), esc clears
// marks before it walks back a level. Delete is the one bulk-capable verb
// today (verbs.Delete.Bulk) — inline y/N in non-prod contexts, the
// type-the-count modal in PROD, same 8b tiering the single-row delete
// already uses. Kept in its own file, browse's per-concern split convention
// (like delete.go/scale.go/nodes.go).
package browse

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// bulkDeleteTarget is the state pendingBulkDelete gates on while 20a's
// confirm is showing.
type bulkDeleteTarget struct {
	rows []resources.Row
	tier actions.Tier
	// typedInput is the type-ahead buffer for a TierModal (PROD) confirm —
	// unused for TierInline. Never rendered via its own View() (components.
	// TypeCountModal keeps its own bespoke progress-counter look, reading
	// Value() for the text and Position() for the caret), so it's never
	// styled — this exists purely for HandleTypeKey-equivalent input
	// handling (Home/End; paste arrives via pasteTarget), matching
	// actions.Controller's own typedInput.
	typedInput textfield.Model
}

// bulkDeleteResultMsg carries a bulk delete's per-target outcomes, in
// marked-row order, under label ("Delete pods") — reported on the keybar's
// result line via actions.Controller.ReportBulkResult, the same red/green
// line every single-object mutation uses.
type bulkDeleteResultMsg struct {
	label   string
	results []actions.TargetResult
}

// retainFailedMarks is the marked set left after a bulk verb ran: only the
// targets whose call failed (0.8.0 plan §3 Phase 2 task 11: "let the caller
// retain only failed marks"), nil when every target succeeded.
func retainFailedMarks(results []actions.TargetResult) map[string]bool {
	var retained map[string]bool
	for _, r := range results {
		if r.Err == nil {
			continue
		}
		if retained == nil {
			retained = make(map[string]bool)
		}
		retained[markKey(r.Namespace, r.ResourceName)] = true
	}
	return retained
}

// markKey identifies a row for the marked set — namespace+name rather than
// just name, so 6b's cross-namespace grouped view can't collide two
// same-named rows in different namespaces.
func markKey(namespace, name string) string {
	return namespace + "/" + name
}

// isMarked reports whether row is in the marked set.
func (m Model) isMarked(row resources.Row) bool {
	if len(m.marks) == 0 {
		return false
	}
	return m.marks[markKey(row.Namespace, row.Name)]
}

// toggleMark flips row's marked state.
func (m *Model) toggleMark(row resources.Row) {
	key := markKey(row.Namespace, row.Name)
	if m.marks == nil {
		m.marks = make(map[string]bool)
	}
	if m.marks[key] {
		delete(m.marks, key)
	} else {
		m.marks[key] = true
	}
}

// markCursorAndAdvance is 20a's "space": mark the cursor row and move down
// one, so repeated presses hand-pick a run of rows without a separate
// range-mark chord.
func (m *Model) markCursorAndAdvance() {
	if row, ok := m.selectedRow(); ok {
		m.toggleMark(row)
	}
	m.moveSelection(1)
}

// markAllFiltered is 20a's "*": mark every row the current filter matches —
// "filter-then-mark is the bulk grammar" — so this reads m.visible (already
// post-filter), not m.rows. Safe to call while filterActive's typing
// captures every other key: '*' can never appear in a Kubernetes object
// name, so intercepting it here (browse's update.go / updateFilterKey) never
// shadows a character a real filter query would need, unlike 6a's "a".
func (m *Model) markAllFiltered() {
	if len(m.visible) == 0 {
		return
	}
	if m.marks == nil {
		m.marks = make(map[string]bool)
	}
	for _, fm := range m.visible {
		m.marks[markKey(fm.row.Namespace, fm.row.Name)] = true
	}
}

// clearMarks empties the marked set, reporting whether there was anything to
// clear — esc's "clears marks before it walks back a level" needs to know
// whether it should consume the keypress or fall through to normal back-nav.
func (m *Model) clearMarks() bool {
	if len(m.marks) == 0 {
		return false
	}
	m.marks = nil
	return true
}

// markedRows collects the full Row for every currently marked key, in
// m.rows order — the target list a bulk verb acts on.
func (m Model) markedRows() []resources.Row {
	if len(m.marks) == 0 {
		return nil
	}
	rows := make([]resources.Row, 0, len(m.marks))
	for _, r := range m.rows {
		if m.marks[markKey(r.Namespace, r.Name)] {
			rows = append(rows, r)
		}
	}
	return rows
}

// beginBulkDelete opens 20a's confirm for the marked set — inline y/N in
// non-prod, the type-the-count modal in PROD, resolved by the same
// verbs.Resolve single-row delete uses. A marked set of Namespaces,
// CustomResourceDefinitions or Nodes therefore always gets the type-the-count modal,
// even outside PROD: a set has no one name to type, and the count is the
// fact a `*`-marked wipe is most likely to get wrong (§20a's PROD grammar,
// reused rather than inventing a third confirm surface).
func (m *Model) beginBulkDelete() tea.Cmd {
	rows := m.markedRows()
	if len(rows) == 0 || m.mutator == nil {
		return nil
	}
	// Bulk delete runs its own confirm outside actions.Controller.Begin, so
	// it needs Begin's OFFLINE gate explicitly (docs/design README.md §4a:
	// "delete/exec/edit verbs are disabled while offline").
	if m.actions.RefuseIfOffline() {
		return nil
	}
	tier := verbs.Resolve(verbs.Delete, verbs.Target{Kind: m.kind}, m.isProd()).Tier
	input := textfield.New()
	input.Prompt = ""
	if tier == actions.TierModal {
		input.Focus()
	}
	m.pendingBulkDelete = &bulkDeleteTarget{rows: rows, tier: tier, typedInput: input}
	return nil
}

// updateBulkDeleteKey routes keys while pendingBulkDelete's confirm is
// showing: TierModal drives the type-the-count buffer (enter only executes
// once the typed digits equal the marked count); TierInline stays a plain
// y/n.
func (m *Model) updateBulkDeleteKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	t := m.pendingBulkDelete
	if t.tier == actions.TierModal {
		switch msg.String() {
		case "esc":
			m.pendingBulkDelete = nil
		case "enter":
			if t.typedInput.Value() == strconv.Itoa(len(t.rows)) {
				return m, m.executeBulkDelete()
			}
		default:
			// Digits only, matching this field's count semantics — any
			// keypress whose Text carries a non-digit rune is dropped,
			// everything else (backspace, left/right, Home/End) reaches
			// the text field.
			for _, r := range msg.Text {
				if r < '0' || r > '9' {
					return m, nil
				}
			}
			var cmd tea.Cmd
			t.typedInput, cmd = t.typedInput.Update(msg)
			return m, cmd
		}
		return m, nil
	}
	switch msg.String() {
	case "y":
		return m, m.executeBulkDelete()
	case "n", "esc":
		m.pendingBulkDelete = nil
	}
	return m, nil
}

// executeBulkDelete runs kube.Mutator.DeleteResource for every marked row,
// recording each row's outcome rather than stopping at the first failure —
// a partial failure still deletes everything it can.
func (m *Model) executeBulkDelete() tea.Cmd {
	t := m.pendingBulkDelete
	mutator := m.mutator
	kind := m.kind
	rows := t.rows
	label := "Delete " + lowerDisplay(m.desc.Display)
	return func() tea.Msg {
		results := make([]actions.TargetResult, 0, len(rows))
		for _, r := range rows {
			err := mutator.DeleteResource(context.Background(), kind, r.Namespace, r.Name)
			if err != nil {
				err = fmt.Errorf("%s: %w", r.Name, err)
			}
			results = append(results, actions.TargetResult{Namespace: r.Namespace, ResourceName: r.Name, Err: err})
		}
		return bulkDeleteResultMsg{label: label, results: results}
	}
}

// bulkNamespace returns the marked rows' shared namespace and whether they
// share one — a marked set can span multiple namespaces in 6b's
// all-namespaces triage, and a single `kubectl delete ... -n ns` can't
// represent that truthfully.
func bulkNamespace(rows []resources.Row) (string, bool) {
	if len(rows) == 0 {
		return "", true
	}
	ns := rows[0].Namespace
	for _, r := range rows[1:] {
		if r.Namespace != ns {
			return "", false
		}
	}
	return ns, true
}

// rowNames extracts each row's Name, in order.
func rowNames(rows []resources.Row) []string {
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.Name
	}
	return names
}

// bulkObjectLabels formats every marked row for the confirm modal's object
// list — just the name when every row shares a namespace, "namespace/name"
// when they don't (so a cross-namespace set stays unambiguous).
func bulkObjectLabels(rows []resources.Row, uniformNamespace bool) []string {
	labels := make([]string, len(rows))
	for i, r := range rows {
		if uniformNamespace {
			labels[i] = r.Name
		} else {
			labels[i] = r.Namespace + "/" + r.Name
		}
	}
	return labels
}

// wrapLabels joins labels with ", ", wrapping to width-wide lines so the
// confirm modal's object list ("lists every object") doesn't force the
// modal arbitrarily wide for a large marked set.
func wrapLabels(labels []string, width int) string {
	var lines []string
	var cur strings.Builder
	for i, lbl := range labels {
		piece := lbl
		if i < len(labels)-1 {
			piece += ", "
		}
		if cur.Len() > 0 && cur.Len()+len(piece) > width {
			lines = append(lines, strings.TrimRight(cur.String(), " "))
			cur.Reset()
		}
		cur.WriteString(piece)
	}
	if cur.Len() > 0 {
		lines = append(lines, strings.TrimRight(cur.String(), " "))
	}
	return strings.Join(lines, "\n")
}

// bulkDeleteWillRunLine is the pendingBulkDelete keybar's RightNote while
// TierInline: the exact kubectl invocation, naming every marked object
// (docs/design README.md §20a: "kubectl delete pod a b c -n <ns>") — or, for
// a marked set spanning multiple namespaces, a plain summary rather than a
// command that would misrepresent a single kubectl call's scope.
func (m Model) bulkDeleteWillRunLine() string {
	t := m.pendingBulkDelete
	if ns, uniform := bulkNamespace(t.rows); uniform {
		return "will run: " + kube.DeleteCommandString(m.kind, ns, rowNames(t.rows))
	}
	return fmt.Sprintf("will delete %d %s across %d namespaces", len(t.rows), lowerDisplay(m.desc.Display), distinctNamespaces(t.rows))
}

// bulkDeleteConfirmModal renders 20a's PROD type-the-count modal: title
// ("Delete N pods?"), every marked object, the grace-period detail line, and
// the type-ahead prompt against the count (components.TypeCountModal).
func (m Model) bulkDeleteConfirmModal(width, height int) string {
	theme := m.Theme()
	t := m.pendingBulkDelete
	count := len(t.rows)
	title := fmt.Sprintf("✕ Delete %d %s?", count, lowerDisplay(m.desc.Display))

	_, uniform := bulkNamespace(t.rows)
	objectsLine := wrapLabels(bulkObjectLabels(t.rows, uniform), 56)

	styles := components.TypeModalStyles{
		Border:    lipgloss.NewStyle().BorderForeground(theme.ConfirmBorder).Background(theme.ConfirmHeaderBg),
		Title:     lipgloss.NewStyle().Foreground(theme.Bad).Bold(true).Background(theme.ConfirmHeaderBg),
		ProdTag:   lipgloss.NewStyle().Foreground(theme.ProdText).Bold(true).Background(theme.ConfirmHeaderBg),
		Owner:     lipgloss.NewStyle().Foreground(theme.TextSecondary).Background(theme.ConfirmHeaderBg),
		Detail:    lipgloss.NewStyle().Foreground(theme.TextSecondary).Background(theme.ConfirmHeaderBg),
		Rule:      lipgloss.NewStyle().Foreground(theme.TextGhost).Background(theme.ConfirmHeaderBg),
		Input:     lipgloss.NewStyle().Foreground(theme.Text).Background(theme.ConfirmHeaderBg),
		Selection: lipgloss.NewStyle().Foreground(theme.Bg).Background(theme.Accent),
		Progress:  lipgloss.NewStyle().Foreground(theme.TextFaint).Background(theme.ConfirmHeaderBg),
		Key:       lipgloss.NewStyle().Foreground(theme.Bad).Background(theme.ConfirmHeaderBg),
		Label:     lipgloss.NewStyle().Foreground(theme.TextDim).Background(theme.ConfirmHeaderBg),
	}
	return components.TypeCountModal(title, objectsLine, "default grace period applies", count, t.typedInput, m.isProd(), styles, width, height)
}
