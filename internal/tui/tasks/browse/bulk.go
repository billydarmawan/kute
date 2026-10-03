// 20a bulk operations (docs/design README.md §20a): space marks the cursor
// row and advances, * marks every row the current filter matches
// ("filter-then-mark is the bulk grammar — no range-mark chord"), esc clears
// marks before it walks back a level. Delete is the one bulk-capable verb
// today (verbs.Delete.Bulk) — inline y/N in non-prod contexts, the
// type-the-count modal in PROD, same 8b tiering the single-row delete
// already uses, run through the same actions.Controller.Begin. Kept in its own file, browse's per-concern split convention
// (like delete.go/scale.go/nodes.go).
package browse

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

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

// bulkDeleteActionPrefix prefixes a marked-set delete's TaskAction.ID, so
// the actions.BulkResultMsg handler can tell its result from a marked-set
// suspend's.
const bulkDeleteActionPrefix = "bulk-delete-"

// beginBulkDelete opens 20a's confirm for the marked set through
// actions.Controller.Begin, like every single-row verb — inline y/N in
// non-prod, the type-the-count modal in PROD, resolved by the same
// verbs.Resolve single-row delete uses. A marked set of Namespaces,
// CustomResourceDefinitions or Nodes therefore always gets the type-the-count modal,
// even outside PROD: a set has no one name to type, and the count is the
// fact a `*`-marked wipe is most likely to get wrong (§20a's PROD grammar,
// reused rather than inventing a third confirm surface). Begin owns the
// OFFLINE refusal (docs/design README.md §4a), and executeBulk runs
// kube.Mutator.DeleteResource once per marked row, recording each row's
// outcome rather than stopping at the first failure.
func (m *Model) beginBulkDelete() tea.Cmd {
	rows := m.markedRows()
	if len(rows) == 0 || m.mutator == nil {
		return nil
	}
	targets := make([]tui.BulkTarget, len(rows))
	for i, r := range rows {
		targets[i] = tui.BulkTarget{Namespace: r.Namespace, ResourceName: r.Name}
	}
	return m.actions.Begin(actions.Request{Verb: verbs.Delete, Prod: m.isProd()}, tui.TaskAction{
		ID:    bulkDeleteActionPrefix + string(m.kind),
		Label: "Delete " + lowerDisplay(m.desc.Display),
		Scope: tui.TaskScope{
			ResourceKind: string(m.kind),
			Verb:         verbs.Delete.ID,
			IsMutating:   true,
			BulkTargets:  targets,
		},
	})
}

// pendingBulkDelete reports whether the active confirm is 20a's marked-set
// delete — Body()'s modal dispatch and the inline keybar's will-run line
// read it.
func (m Model) pendingBulkDelete() bool {
	pending := m.actions.Pending()
	return pending != nil && pending.Scope.Verb == verbs.Delete.ID && len(pending.Scope.BulkTargets) > 0
}

// pendingBulkCount reports whether the active confirm is a marked-set
// TierModal confirm (delete or cronjob-suspend), which types the marked
// count rather than a name: pasteTarget's digit-only paste keys off it (the
// enter gate and the typed digit filter are actions.HandleConfirmKey's own).
func (m Model) pendingBulkCount() bool {
	return m.pendingBulkDelete() || m.pendingBulkCronJobSuspend()
}

// nameBulkDeleteErrors prefixes each failed target's error with its name, so
// the result line's "N of M targets failed: <first error>" says which object
// it was about.
func nameBulkDeleteErrors(results []actions.TargetResult) []actions.TargetResult {
	out := make([]actions.TargetResult, len(results))
	for i, r := range results {
		if r.Err != nil {
			r.Err = fmt.Errorf("%s: %w", r.ResourceName, r.Err)
		}
		out[i] = r
	}
	return out
}

// targetNames extracts each target's ResourceName, in order.
func targetNames(targets []tui.BulkTarget) []string {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.ResourceName
	}
	return names
}

// distinctTargetNamespaces counts the unique namespaces among targets.
func distinctTargetNamespaces(targets []tui.BulkTarget) int {
	seen := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		seen[t.Namespace] = struct{}{}
	}
	return len(seen)
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

// bulkDeleteWillRunLine is a marked-set delete's TierInline keybar
// RightNote: the exact kubectl invocation, naming every marked object
// (docs/design README.md §20a: "kubectl delete pod a b c -n <ns>") — or, for
// a marked set spanning multiple namespaces (a marked set can span them in
// 6b's all-namespaces triage), a plain summary rather than a command that
// would misrepresent a single kubectl call's scope.
func (m Model) bulkDeleteWillRunLine(scope tui.TaskScope) string {
	targets := scope.BulkTargets
	if ns, uniform := bulkTargetNamespace(targets); uniform {
		return "will run: " + kube.DeleteCommandString(m.kind, ns, targetNames(targets))
	}
	return fmt.Sprintf("will delete %d %s across %d namespaces", len(targets), lowerDisplay(m.desc.Display), distinctTargetNamespaces(targets))
}

// bulkDeleteConfirmModal renders 20a's type-the-count modal: title
// ("Delete N pods?"), every marked object, the grace-period detail line, and
// the type-ahead prompt against the count (components.TypeCountModal),
// sourced from the controller's pending BulkTargets and typed buffer.
func (m Model) bulkDeleteConfirmModal(width, height int) string {
	theme := m.Theme()
	pending := m.actions.Pending()
	if pending == nil {
		return components.CenterLines([]string{"Confirm"}, width, height)
	}
	targets := pending.Scope.BulkTargets
	count := len(targets)
	title := fmt.Sprintf("✕ Delete %d %s?", count, lowerDisplay(m.desc.Display))

	_, uniform := bulkTargetNamespace(targets)
	objectsLine := wrapLabels(bulkTargetLabels(targets, uniform), 56)

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
	return components.TypeCountModal(title, objectsLine, "default grace period applies", count, m.actions.TypedInput(), m.isProd(), styles, width, height)
}
