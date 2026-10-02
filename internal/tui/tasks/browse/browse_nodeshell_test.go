package browse

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// TestNodeDebugKeyPushesPanel confirms 'x' on a Nodes row pushes
// tasks/debugpanel (§41d) via OpenNodeDebug — replaces the retired
// standalone NodeShell verb's direct tea.ExecProcess launch (see verbs.go's
// NodeDebug doc comment).
func TestNodeDebugKeyPushesPanel(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {nodeObj("node-a", true, false)},
	}}
	session := newSession()
	session.Location.Kind = kube.KindNode
	var gotName string
	m := New(Config{
		Session: session, Lister: lister,
		OpenNodeDebug: func(name string, _, _, _ int) (tea.Model, tea.Cmd) {
			gotName = name
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected debugpanel's sentinel task to be pushed, got %T", updated)
	}
	if gotName != "node-a" {
		t.Fatalf("OpenNodeDebug called with name %q, want node-a", gotName)
	}
}

// TestNodeDebugRefusedWhileOffline pins the 4a gate for 'x' on a Node row:
// NodeDebug is a Mutating verb (kubectl debug creates a privileged
// node-debugger pod), and a broken connection must refuse it outright —
// docs/design README.md §52's "mutating actions disabled" while OFFLINE.
func TestNodeDebugRefusedWhileOffline(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {nodeObj("node-a", true, false)},
	}}
	session := newSession()
	session.Location.Kind = kube.KindNode
	m := New(Config{
		Session: session, Lister: lister,
		OpenNodeDebug: func(string, int, int, int) (tea.Model, tea.Cmd) {
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "i/o timeout"})

	if updated, _ := m.Update(tea.KeyPressMsg{Text: "x"}); reflect.TypeOf(updated) != reflect.TypeFor[*Model]() {
		t.Errorf("'x' should stay a no-op while offline, got %T", updated)
	}
	if hints := strings.Join(keybarKeys(m.Keybar()), " "); strings.Contains(hints, "x") && strings.Contains(hints, "debug") {
		t.Errorf("node-debug hint still advertised while offline: %q", hints)
	}

	// Back online the same key works again — the gate is the connection
	// state, not a latch.
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnConnected})
	updated, _ := m.Update(tea.KeyPressMsg{Text: "x"})
	if _, ok := updated.(stubTask); !ok {
		t.Error("'x' stayed refused after reconnecting")
	}
}

// keybarKeys flattens a Keybar's hint groups to "key label" pairs.
func keybarKeys(kb tui.Keybar) []string {
	var out []string
	for _, g := range kb.Groups {
		for _, h := range g {
			out = append(out, h.Key+" "+h.Label)
		}
	}
	return out
}

// kube.NodeShellUnavailable: node debug can't work on GKE Autopilot or EKS
// Fargate, and the honest handling is an error naming the reason rather than
// a key that quietly does nothing (or worse, hands kubectl a command the
// platform will refuse after the panel has already opened).
func TestNodeDebugExplainsItselfOnFargate(t *testing.T) {
	node := nodeObj("fargate-ip-10-0-1-23.eu-west-1.compute.internal", true, false)
	node.Labels = map[string]string{"eks.amazonaws.com/compute-type": "fargate"}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {node},
	}}
	session := newSession()
	session.Location.Kind = kube.KindNode
	m := New(Config{
		Session: session, Lister: lister,
		OpenNodeDebug: func(string, int, int, int) (tea.Model, tea.Cmd) {
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	if cmd != nil {
		t.Error("'x' ran kubectl debug against a Fargate node")
	}
	if _, ok := updated.(stubTask); ok {
		t.Fatal("expected browse to stay the active task rather than push the panel")
	}
	m = *updated.(*Model)
	note := m.Keybar().RightNote
	if !strings.Contains(note, "EKS Fargate") {
		t.Errorf("Keybar RightNote = %q, want it to name EKS Fargate", note)
	}
	// The key itself stays advertised — it works on every other cluster.
	if hints := strings.Join(keybarKeys(m.Keybar()), " "); !strings.Contains(hints, "debug") {
		t.Errorf("node-debug hint disappeared: %q", hints)
	}
}
