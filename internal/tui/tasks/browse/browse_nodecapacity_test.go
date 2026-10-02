package browse

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// aksNode models a real managed node: 16Gi of machine, ~13Gi of it
// schedulable after kube-reserved. The gap is the whole point of the
// fixture — see TestNodeMEMBarDividesByCapacity.
func aksNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.34.3"},
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1900m"),
				corev1.ResourceMemory: resource.MustParse("13Gi"),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
		},
	}
}

type fakeNodeMetrics map[string]kube.NodeMetric

func (f fakeNodeMetrics) NodeMetrics(context.Context) (map[string]kube.NodeMetric, error) {
	return f, nil
}

// TestNodeMEMBarDividesByCapacity: the MEM bar's numerator is whole-node
// usage — kubelet and container runtime included — so its denominator has
// to be the whole node too. 8Gi on a 16Gi machine is half full, and that is
// what `kubectl top node` prints; dividing by the 13Gi Allocatable instead
// would fill 4 of the 6 cells rather than 3 and report the node ten points
// hotter than kubectl does.
func TestNodeMEMBarDividesByCapacity(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {aksNode("aks-np-vmss000000")},
		kube.KindPod:  {},
	}}
	nodeMetrics := fakeNodeMetrics{"aks-np-vmss000000": {
		CPU: "177m", MEM: "8.0Gi",
		CPUMilli: 177, MemBytes: 8 * 1024 * 1024 * 1024,
	}}

	sess := newSession()
	sess.Location.Kind = kube.KindNode
	m := New(Config{Session: sess, Lister: lister, NodeMetrics: nodeMetrics})
	m.SetSize(160, 36)
	m = step(t, m, m.load()())
	m = step(t, m, m.loadNodeMetrics(m.metricsEpoch)())

	row := nodeRowLine(t, ansi.Strip(m.Render()), "aks-np-vmss000000")

	// 8Gi / 16Gi = 50% ⇒ ceil(0.5*6) = 3 filled cells.
	if want := "▮▮▮▯▯▯ 8.0Gi"; !strings.Contains(row, want) {
		t.Errorf("MEM cell missing %q (capacity denominator); row:\n%s", want, row)
	}
	// Dividing by 13Gi Allocatable would be 61.5% ⇒ 4 cells.
	if bad := "▮▮▮▮▯▯ 8.0Gi"; strings.Contains(row, bad) {
		t.Errorf("MEM bar filled %q — divided by Allocatable, not Capacity", bad)
	}
	// PODS stays on Allocatable, which is kubectl's own denominator there.
	if want := "0/110"; !strings.Contains(row, want) {
		t.Errorf("PODS cell missing %q; row:\n%s", want, row)
	}
}

func nodeRowLine(t *testing.T, view, name string) string {
	t.Helper()
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, name) && strings.Contains(line, "Ready") {
			return line
		}
	}
	t.Fatalf("no row for %q in view:\n%s", name, view)
	return ""
}
