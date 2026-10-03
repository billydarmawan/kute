package fluxdetail

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// beginSuspend/beginReconcile mirror browse's own §30a handlers exactly —
// same verbs, same tier, same Scope shape — so the two screens can't drift
// into offering different friction for the same action.
func (m *Model) beginSuspend() tea.Cmd {
	verb, label := "flux-suspend", fmt.Sprintf("Suspend %s?", m.name)
	if m.suspended {
		verb, label = "flux-resume", fmt.Sprintf("Resume %s?", m.name)
	}
	m.feedback = kube.FluxSuspendCommandString(m.kind, m.namespace, m.name, !m.suspended)
	return m.actions.Begin(actions.Request{Verb: verbs.FluxSuspend, Prod: m.isProd()}, tui.TaskAction{
		ID:    verb + "-" + m.name,
		Label: label,
		Scope: tui.TaskScope{
			ResourceKind: string(m.kind), ResourceName: m.name,
			Namespace: m.namespace, Verb: verb, IsMutating: true,
		},
	})
}

func (m *Model) beginReconcile() tea.Cmd {
	m.feedback = kube.FluxReconcileCommandString(m.kind, m.namespace, m.name, time.Now())
	return m.actions.Begin(actions.Request{Verb: verbs.FluxReconcile, Prod: m.isProd()}, tui.TaskAction{
		ID:    "flux-reconcile-" + m.name,
		Label: fmt.Sprintf("Reconcile %s?", m.name),
		Scope: tui.TaskScope{
			ResourceKind: string(m.kind), ResourceName: m.name,
			Namespace: m.namespace, Verb: "flux-reconcile", IsMutating: true,
		},
	})
}
