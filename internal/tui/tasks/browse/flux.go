// Flux-specific browse machinery for §30a: the suspend/resume, reconcile
// and jump-to-source verbs. Kept in its own file per browse's per-concern
// split (deployments.go, nodes.go, helm.go, routes.go).
//
// There is deliberately no ↵ carve-out here. A Flux descriptor keeps
// Custom: true, so crddetail.go's openSelectedObjectDetail already routes
// it — routes.go's shape is the precedent for the file layout, not for a
// ↵ branch.
package browse

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// fluxVerbsApply reports whether §30a's verbs are live on the current view:
// a Flux kind, a wired mutator, and a settled list. Keyed on the
// descriptor's own Flux flag, never a kind-name check — the Flux kind set
// is discovered at connect, not compiled in.
func (m Model) fluxVerbsApply() bool {
	return m.desc.Flux && m.mutator != nil && m.state == tui.TaskStateReady
}

// openSelectedFluxDetail pushes §31a for the cursor row. Gated on the
// descriptor's own Flux flag, so it precedes crddetail.go's generic 14d
// branch (a Flux descriptor is Custom too) without either needing a
// kind-name check — and on its API group, because §31a is the *reconciler*
// screen: a source, an alert, a provider or an image policy has no chain
// and no inventory for it to show, and §30a sends those to 14d, the same
// split 30b's tree makes.
func (m Model) openSelectedFluxDetail() (tea.Model, tea.Cmd, bool) {
	if !m.desc.Flux || m.openFluxDetail == nil || !isFluxReconcilerGroup(m.desc.APIGroup) {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	task, cmd := m.openFluxDetail(m.kind, row.Namespace, row.Name, m.width, m.height)
	return task, cmd, task != nil
}

// isFluxReconcilerGroup reports whether group serves Flux's reconciler
// kinds (Kustomization, HelmRelease) — the ones §31a is built for.
func isFluxReconcilerGroup(group string) bool {
	return group == kube.FluxGroupKustomize || group == kube.FluxGroupHelm
}

// beginFluxSuspend toggles spec.suspend on row — one verb, two directions,
// exactly beginCordon's shape. TierNone, so it executes immediately; the
// will-run line is set first so the command is on screen the moment the key
// lands rather than only after the result.
func (m *Model) beginFluxSuspend(row resources.Row) tea.Cmd {
	verb, label := "flux-suspend", fmt.Sprintf("Suspend %s?", row.Name)
	if row.Suspended {
		verb, label = "flux-resume", fmt.Sprintf("Resume %s?", row.Name)
	}
	m.execFeedback = kube.FluxSuspendCommandString(m.kind, row.Namespace, row.Name, !row.Suspended)
	return m.actions.Begin(verbs.FluxSuspend.Tier, tui.TaskAction{
		ID:    verb + "-" + row.Name,
		Label: label,
		Scope: tui.TaskScope{
			ResourceKind: string(m.kind), ResourceName: row.Name,
			Namespace: row.Namespace, Verb: verb, IsMutating: true,
		},
	})
}

// beginFluxReconcile stamps the reconcile annotation on row. The timestamp
// is taken once here so the will-run line names the same instant the patch
// carries.
func (m *Model) beginFluxReconcile(row resources.Row) tea.Cmd {
	m.execFeedback = kube.FluxReconcileCommandString(m.kind, row.Namespace, row.Name, time.Now())
	return m.actions.Begin(verbs.FluxReconcile.Tier, tui.TaskAction{
		ID:    "flux-reconcile-" + row.Name,
		Label: fmt.Sprintf("Reconcile %s?", row.Name),
		Scope: tui.TaskScope{
			ResourceKind: string(m.kind), ResourceName: row.Name,
			Namespace: row.Namespace, Verb: "flux-reconcile", IsMutating: true,
		},
	})
}

// openSelectedFluxSource jumps to the object row reconciles from — §30a's
// 'o'. The row carries the resolved ref, including the ref's own
// namespace; the SOURCE cell is only its display abbreviation, and
// re-parsing that landed a HelmRelease pointing at flux-system's
// HelmRepository on an empty list in the HelmRelease's own namespace.
func (m Model) openSelectedFluxSource() (tea.Cmd, bool) {
	row, ok := m.selectedRow()
	if !ok {
		return nil, false
	}
	src, ok := fluxSourceRef(row)
	if !ok {
		return nil, false
	}
	return func() tea.Msg {
		return tui.GotoResourceMsg{Kind: src.Kind, Namespace: src.Namespace, Name: src.Name}
	}, true
}

// fluxSourceRef is row's resolved source. Returns false for a source kind's
// own row, which has none because it *is* the source, and for a ref whose
// kind isn't declared.
func fluxSourceRef(row resources.Row) (resources.FluxSourceRef, bool) {
	src := row.FluxSource
	return src, src.Kind != "" && src.Name != ""
}

// fluxKeybarGroup is §30a's keybar block. The suspend hint's *label* flips
// with the cursor row while its key stays whatever the registry says — the
// same contextual-label pattern openPodsHint uses.
func (m Model) fluxKeybarGroup() []tui.KeyHint {
	suspend := verbs.FluxSuspend
	if row, ok := m.selectedRow(); ok && row.Suspended {
		suspend.Label = "resume"
	}
	hints := []tui.KeyHint{
		{Key: verbs.FluxReconcile.Key, Label: verbs.FluxReconcile.Label},
		{Key: suspend.Key, Label: suspend.Label},
	}
	if _, ok := fluxSourceRefForSelection(m); ok {
		hints = append(hints, tui.KeyHint{Key: verbs.FluxSource.Key, Label: verbs.FluxSource.Label})
	}
	return hints
}

// fluxSourceRefForSelection is fluxSourceRef against the cursor row, so the
// keybar can omit 'o' on a source kind's own list rather than advertising a
// key that does nothing.
func fluxSourceRefForSelection(m Model) (resources.FluxSourceRef, bool) {
	row, ok := m.selectedRow()
	if !ok {
		return resources.FluxSourceRef{}, false
	}
	return fluxSourceRef(row)
}
