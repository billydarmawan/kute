// Certificate-specific browse machinery for §35c (docs/design/
// v.0.7.0.dc.html): the 'r' force-renew verb on a Certificate row in the
// list. Kept in its own file per browse's per-concern split convention
// (argo.go, flux.go, jobs.go), separate from certchain.go's ↵ carve-out
// into tasks/certchain — that screen has the same verb, wired independently
// through its own actions.Controller (tasks/certchain/update.go's own
// beginCertRenew), per the repo's package-local-seam convention rather than
// exported from here.
package browse

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// certVerbsApply reports whether §35c's verb is live on the current view: a
// Certificate kind, a wired mutator, and a settled list — same shape as
// argoVerbsApply/fluxVerbsApply.
func (m Model) certVerbsApply() bool {
	return m.kind == kube.KindCertificate && m.mutator != nil && m.state == tui.TaskStateReady
}

// beginCertRenew starts verbs.CertRenew ('r'): its policy row never
// escalates past the plain inline y/N in PROD — see CertRenew's own doc
// comment for why, the same reasoning beginJobRetry's own doc comment
// explains for Retry.
func (m *Model) beginCertRenew(row resources.Row) tea.Cmd {
	return m.actions.Begin(actions.Request{Verb: verbs.CertRenew, Prod: m.isProd()}, tui.TaskAction{
		ID:    "cert-renew-" + row.Namespace + "/" + row.Name,
		Label: fmt.Sprintf("Renew %s?", row.Name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCertificate), ResourceName: row.Name,
			Namespace: row.Namespace, Verb: "cert-renew", IsMutating: true,
		},
	})
}

// certRenewWillRunLine is beginCertRenew's confirm "will run: ..." line —
// same idiom as jobactions.RetryWillRunLine/argoSyncWillRunLine: the literal kubectl
// patch RenewCertificate issues, never `cmctl renew`
// (RenewCertificateCommandString's own doc comment has the reasoning).
func certRenewWillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.RenewCertificateCommandString(scope.Namespace, scope.ResourceName)
}

// certKeybarGroup is §35c's keybar block.
func (m Model) certKeybarGroup() []tui.KeyHint {
	return []tui.KeyHint{verbs.CertRenew.Hint()}
}
