// Argo CD-specific browse machinery for §33a: the refresh, sync and
// dashboard-url verbs. Kept in its own file per browse's per-concern split
// (flux.go, deployments.go, nodes.go, helm.go, routes.go).
//
// There is deliberately no ↵ carve-out here, same reasoning as flux.go's
// own doc comment: an Argo descriptor keeps Custom: true, so
// crddetail.go's openSelectedObjectDetail already routes it to 14d's
// generic detail — §33a has no bespoke detail screen the way Flux's 31a
// does.
package browse

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// argoVerbsApply reports whether §33a's verbs are live on the current view:
// an Argo Application kind, a wired mutator, and a settled list. Keyed on
// the descriptor's own Argo flag, never a kind-name check — same shape as
// fluxVerbsApply.
func (m Model) argoVerbsApply() bool {
	return m.desc.Argo && m.mutator != nil && m.state == tui.TaskStateReady
}

// beginArgoRefresh stamps the refresh annotation on row. TierNone, so it
// executes immediately; the will-run line is set first so the command is
// on screen the moment the key lands, same as beginFluxReconcile.
func (m *Model) beginArgoRefresh(row resources.Row) tea.Cmd {
	m.execFeedback = kube.ArgoRefreshCommandString(m.kind, row.Namespace, row.Name)
	return m.actions.Begin(verbs.ArgoRefresh.Tier, tui.TaskAction{
		ID:    "argo-refresh-" + row.Name,
		Label: fmt.Sprintf("Refresh %s?", row.Name),
		Scope: tui.TaskScope{
			ResourceKind: string(m.kind), ResourceName: row.Name,
			Namespace: row.Namespace, Verb: "argo-refresh", IsMutating: true,
		},
	})
}

// beginArgoSync patches operation.sync.revision on row to its own
// already-configured target revision (argoSyncRevisionFromRow re-expands
// the row's own rendered REVISION cell, the same "read the cell instead of
// re-fetching the object" idiom fluxSourceRef uses) — a re-apply of what
// git already says to run, never a move to a different ref. TierInline:
// PROD escalates to the modal automatically via verbs.TierFor, same as
// beginRolloutRestart.
func (m *Model) beginArgoSync(row resources.Row) tea.Cmd {
	revision := argoSyncRevisionFromRow(row)
	tier := verbs.TierFor(verbs.ArgoSync, m.isProd())
	return m.actions.Begin(tier, tui.TaskAction{
		ID:    "argo-sync-" + row.Name,
		Label: fmt.Sprintf("Sync %s?", row.Name),
		Scope: tui.TaskScope{
			ResourceKind: string(m.kind), ResourceName: row.Name,
			Namespace: row.Namespace, Verb: "argo-sync", IsMutating: true,
			ArgoSyncRevision: revision,
		},
	})
}

// argoSyncRevisionFromRow re-expands a projected REVISION cell ("main@
// e41b90c", or bare "HEAD" before the first sync) back into the target ref
// argoRevisionCell built it from — the part before "@", or the whole cell
// when there's no SHA yet.
func argoSyncRevisionFromRow(row resources.Row) string {
	if len(row.Cells) < 4 {
		return "HEAD"
	}
	ref, _, _ := strings.Cut(row.Cells[3], "@")
	return ref
}

// argoSyncWillRunLine is beginArgoSync's confirm "will run: ..." line —
// same idiom as rolloutRestartWillRunLine (deployments.go).
func argoSyncWillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.ArgoSyncCommandString(
		kube.ResourceKind(scope.ResourceKind), scope.Namespace, scope.ResourceName, scope.ArgoSyncRevision)
}

// argoDashboardTimeout bounds the u key's argocd-cm read, like every other
// browse read.
const argoDashboardTimeout = 5 * time.Second

// argoControlPlaneNamespace is where a default Argo CD install keeps
// argocd-cm. Under apps-in-any-namespace an Application lives elsewhere,
// so 'u' looks there after the Application's own namespace.
const argoControlPlaneNamespace = "argocd"

// argoDashboardURLMsg carries the u key's lookup back to the update loop: the
// deep link to copy, or the feedback line saying why there is none.
type argoDashboardURLMsg struct {
	url      string
	feedback string
}

// copySelectedArgoDashboardURL builds §33a's 'u' deep link from argocd-cm's
// own configured base URL (the same ConfigMap argocd-server itself reads
// its "url" key from) plus the row's namespace/name, and copies it to the
// clipboard — a local write, not a cluster call the mutator seam needs to
// know about, same shape as copySelectedForwardURL. The read runs as a
// tea.Cmd under the cluster context with a timeout, never on the update
// loop, and a miss says why — still loading, unreadable, or genuinely not
// configured — so the key always answers rather than silently doing
// nothing.
func (m *Model) copySelectedArgoDashboardURL() tea.Cmd {
	row, ok := m.selectedRow()
	if !ok || m.lister == nil {
		return nil
	}
	lister := m.lister
	parent := m.session.ClusterContext()
	namespace, name := row.Namespace, row.Name
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, argoDashboardTimeout)
		defer cancel()
		base, feedback := argoDashboardBaseURL(ctx, lister, namespace)
		if base == "" {
			return argoDashboardURLMsg{feedback: feedback}
		}
		return argoDashboardURLMsg{url: strings.TrimRight(base, "/") + "/applications/" + namespace + "/" + name}
	}
}

// handleArgoDashboardURL copies a found link, or shows why there isn't one.
func (m *Model) handleArgoDashboardURL(msg argoDashboardURLMsg) tea.Cmd {
	if msg.url == "" {
		m.execFeedback = msg.feedback
		return nil
	}
	return tea.SetClipboard(msg.url)
}

// argoDashboardBaseURL reads argocd-cm's own "url" data key, looking in the
// Application's namespace and then Argo's default control-plane namespace.
// It returns the URL, or "" plus the feedback line for the miss. A cache
// that hasn't filled yet (the first 'u' starts the ConfigMap informer) or
// can't be read is reported as such, never as "not configured".
func argoDashboardBaseURL(ctx context.Context, lister resources.RawLister, namespace string) (string, string) {
	namespaces := []string{namespace}
	if namespace != argoControlPlaneNamespace {
		namespaces = append(namespaces, argoControlPlaneNamespace)
	}
	var loading bool
	var readErr error
	for _, ns := range namespaces {
		objs, err := lister.ListRaw(ctx, kube.KindConfigMap, ns)
		if err == nil {
			err = tui.KindsError(lister, ns, kube.KindConfigMap)
		}
		if err != nil {
			readErr = err
			continue
		}
		for _, obj := range objs {
			cm, ok := obj.(*corev1.ConfigMap)
			if !ok || cm.Name != "argocd-cm" {
				continue
			}
			if url := cm.Data["url"]; url != "" {
				return url, ""
			}
			return "", "dashboard url not configured — argocd-cm has no url key in " + ns
		}
		if !tui.KindsSynced(lister, ns, kube.KindConfigMap) {
			loading = true
		}
	}
	switch {
	case loading:
		return "", "argocd-cm still loading — press u again in a moment"
	case readErr != nil:
		return "", "couldn't read argocd-cm: " + readErr.Error()
	}
	return "", "dashboard url not configured — no argocd-cm in " + strings.Join(namespaces, " or ")
}

// argoKeybarGroup is §33a's keybar block.
func (m Model) argoKeybarGroup() []tui.KeyHint {
	hints := []tui.KeyHint{
		{Key: verbs.ArgoRefresh.Key, Label: verbs.ArgoRefresh.Label},
		{Key: verbs.ArgoSync.Key, Label: verbs.ArgoSync.Label},
	}
	if m.lister != nil {
		hints = append(hints, tui.KeyHint{Key: verbs.ArgoURL.Key, Label: verbs.ArgoURL.Label})
	}
	return hints
}
