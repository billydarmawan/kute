// 17b's +/− inline scale prompt (docs/design README.md §17b): reversible,
// so it never goes through actions.Controller's confirm machinery — instead
// a bespoke gate (pendingScale, like edit.go's pendingEdit) gathers a target
// replica count via a numeric type-ahead buffer, then commits through
// actions.Controller once ↵ applies, reusing its execute()/ResultMsg/
// HandleResult plumbing the same way beginRolloutRestart/beginCordon do for
// their own TierNone verbs. Kept in its own file, browse's per-concern split
// convention (like deployments.go/nodes.go/forwards.go).
package browse

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"
	autoscalingv2 "k8s.io/api/autoscaling/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// scaleTarget is the state pendingScale gates on while 17b's prompt is
// showing.
type scaleTarget struct {
	kind      kube.ResourceKind
	namespace string
	name      string
	// input is the typed-ahead replica count, pre-filled to current±1 by
	// beginScale.
	input textfield.Model
	// typed is true once a digit or backspace has touched input, so the
	// next digit appends instead of replacing the prefill (docs/design
	// README.md §17b: "typing a number replaces it").
	typed bool
	// hpaName is the name of the HorizontalPodAutoscaler targeting this
	// row, or "" when none does (docs/design README.md §17b: "HPA-managed
	// workloads show managed by hpa/<name> ... as a yellow note instead of
	// blocking" — scaling still applies, it's just informational).
	hpaName string
}

// scalable reports whether kind takes 17b's scale prompt — Deployments and
// StatefulSets, the only two kinds browse projects a spec.replicas-derived
// RDY column for.
func scalable(kind kube.ResourceKind) bool {
	return kind == kube.KindDeployment || kind == kube.KindStatefulSet
}

// currentReplicas reads a Deployment/StatefulSet row's desired replica
// count back out of its own RDY cell ("3/3" — resources/projections.go's
// readyRatio puts the ready count and spec.replicas at Cells[1] for both
// kinds) rather than a second raw fetch.
func currentReplicas(row resources.Row) int32 {
	if len(row.Cells) < 2 {
		return 0
	}
	_, want, ok := strings.Cut(row.Cells[1], "/")
	if !ok {
		return 0
	}
	return scaleValue(want)
}

// scaleValue parses s as a non-negative replica count, defaulting to 0 for
// an empty or invalid buffer (docs/design README.md §17b: "0 = deliberate
// scale-to-zero").
func scaleValue(s string) int32 {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return int32(n)
}

// hpaManaging finds an autoscaling/v2 HorizontalPodAutoscaler in namespace
// whose scaleTargetRef points at kind/name (docs/design README.md §17b:
// "HPA-managed workloads show managed by hpa/<name>"). Returns "" when
// there's no lister, the list fails, or no HPA targets this row — never
// blocks the scale prompt itself, purely informational.
func hpaManaging(ctx context.Context, lister resources.RawLister, kind kube.ResourceKind, namespace, name string) string {
	if lister == nil {
		return ""
	}
	objs, err := lister.ListRaw(ctx, kube.KindHorizontalPodAutoscaler, namespace)
	if err != nil {
		return ""
	}
	for _, obj := range objs {
		hpa, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler)
		if !ok {
			continue
		}
		if hpa.Spec.ScaleTargetRef.Kind == string(kind) && hpa.Spec.ScaleTargetRef.Name == name {
			return hpa.Name
		}
	}
	return ""
}

// beginScale opens 17b's prompt for the selected row, pre-filled to
// current+delta clamped at 0 (delta is +1 for '+' and -1 for '−'). Returns
// false (no-op) when nothing applies — wrong kind, no mutator, not ready, or
// no row selected — mirroring openSelectedForward's ok-bool contract.
func (m *Model) beginScale(delta int32) bool {
	if !scalable(m.kind) || m.mutator == nil || m.state != tui.TaskStateReady {
		return false
	}
	row, ok := m.selectedRow()
	if !ok {
		return false
	}
	value := max(currentReplicas(row)+delta, 0)
	input := textfield.New()
	input.SetStyles(tui.TextInputStyles(m.Theme()))
	input.Prompt = ""
	input.SetValue(strconv.Itoa(int(value)))
	input.CursorEnd()
	input.Focus()
	m.pendingScale = &scaleTarget{
		kind: m.kind, namespace: row.Namespace, name: row.Name,
		input:   input,
		hpaName: hpaManaging(m.session.ClusterContext(), m.lister, m.kind, row.Namespace, row.Name),
	}
	return true
}

// scalePasteTarget is the replica-count buffer. Digit-gated like the typed
// path, and it honours the same §17b rule the first typed digit does: before
// anything has been typed the paste *replaces* the pre-filled count rather
// than inserting into it, so pasting "12" over a "3" gives 12, not 312.
func (m *Model) scalePasteTarget() tui.PasteTarget {
	t := m.pendingScale
	return tui.PasteDigits(func(s string) {
		if !t.typed {
			t.input.SetValue(s)
			t.input.CursorEnd()
			t.typed = true
			return
		}
		tui.PasteInto(&t.input)(s)
	})
}

// updateScaleKey routes keys while pendingScale's prompt is showing.
func (m *Model) updateScaleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	t := m.pendingScale
	switch msg.String() {
	case "esc":
		m.pendingScale = nil
	case "enter":
		m.pendingScale = nil
		return m, m.commitScale(*t)
	case "backspace":
		t.typed = true
		var cmd tea.Cmd
		t.input, cmd = t.input.Update(msg)
		return m, cmd
	case "+":
		t.input.SetValue(strconv.Itoa(int(scaleValue(t.input.Value()) + 1)))
		t.input.CursorEnd()
		t.typed = true
	case "-":
		t.input.SetValue(strconv.Itoa(int(max(scaleValue(t.input.Value())-1, 0))))
		t.input.CursorEnd()
		t.typed = true
	default:
		if msg.Text != "" {
			// Digits only, matching this field's replica-count semantics —
			// any keypress whose Text carries a non-digit rune is dropped.
			if len(msg.Text) != 1 || msg.Text[0] < '0' || msg.Text[0] > '9' {
				return m, nil
			}
			if !t.typed {
				// docs/design README.md §17b: "typing a number replaces it"
				// — the first digit after opening replaces the whole
				// pre-filled value rather than inserting into it.
				t.input.SetValue(msg.Text)
				t.input.CursorEnd()
				t.typed = true
				return m, nil
			}
		}
		var cmd tea.Cmd
		t.input, cmd = t.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// commitScale executes t through actions.Controller — verbs.Scale is
// TierNone, so Begin runs it immediately with no separate confirm, the same
// "reversible, no confirm" path RolloutRestart/Cordon already take.
func (m *Model) commitScale(t scaleTarget) tea.Cmd {
	replicas := scaleValue(t.input.Value())
	return m.actions.Begin(actions.Request{Verb: verbs.Scale, Prod: m.isProd()}, tui.TaskAction{
		ID:    "scale-" + t.namespace + "/" + t.name,
		Label: fmt.Sprintf("Scale %s to %d", t.name, replicas),
		Scope: tui.TaskScope{
			ResourceKind: string(t.kind),
			ResourceName: t.name,
			Namespace:    t.namespace,
			Verb:         "scale",
			IsMutating:   true,
			Replicas:     replicas,
		},
	})
}

// scaleWillRunLine is pendingScale's keybar RightNote: the exact kubectl
// invocation Scale is equivalent to (docs/design README.md §17b: "same
// copyable-documentation idiom as 10a/13a"), updating live as the typed
// value changes.
func (m Model) scaleWillRunLine() string {
	t := m.pendingScale
	return "will run: " + kube.ScaleCommandString(t.kind, t.namespace, t.name, scaleValue(t.input.Value()))
}
