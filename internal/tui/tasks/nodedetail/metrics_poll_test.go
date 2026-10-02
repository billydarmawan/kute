package nodedetail

import (
	"context"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// countingPodMetrics records every namespace it's asked for and answers
// with cpu for every pod named in pods.
type countingPodMetrics struct {
	asked []string
	cpu   string
	pods  [][2]string // namespace, name
}

func (c *countingPodMetrics) PodMetricsByNamespace(_ context.Context, ns string) (map[string]kube.PodMetrics, error) {
	c.asked = append(c.asked, ns)
	out := map[string]kube.PodMetrics{}
	for _, p := range c.pods {
		if ns == "" || ns == p[0] {
			out[kube.PodKey(p[0], p[1])] = kube.PodMetrics{CPU: c.cpu, CPUMilli: 1}
		}
	}
	return out, nil
}

type countingNodeMetrics struct{ calls int }

func (c *countingNodeMetrics) NodeMetrics(context.Context) (map[string]kube.NodeMetric, error) {
	c.calls++
	return map[string]kube.NodeMetric{"node-a": {CPU: "500m", CPUMilli: 500, MemBytes: 1}}, nil
}

// update applies msg without following the returned Cmd, which would block
// on the poll chain's next tick.
func update(m Model, msg tea.Msg) (Model, tea.Cmd) {
	updated, cmd := m.Update(msg)
	return *updated.(*Model), cmd
}

// TestUsagePollsOnlyTheNodesNamespacesAndNotOnWatchEvents: the poll reads
// pod usage per namespace this node's pods live in — never cluster-wide —
// and a watch-driven reload reads only the caches, keeping the last usage
// on the rows rather than querying metrics-server once per event.
func TestUsagePollsOnlyTheNodesNamespacesAndNotOnWatchEvents(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-a")},
		kube.KindPod: {
			schedPod("shop", "api", "node-a", "64Mi"),
			schedPod("kube-system", "proxy", "node-a", "64Mi"),
			schedPod("elsewhere", "other", "node-b", "64Mi"),
		},
	}}
	podMetrics := &countingPodMetrics{cpu: "4m", pods: [][2]string{{"shop", "api"}, {"kube-system", "proxy"}}}
	nodeMetrics := &countingNodeMetrics{}
	m := New(Config{Session: newSession(), Lister: lister, Metrics: podMetrics, NodeMetrics: nodeMetrics, NodeName: "node-a"})
	m.SetSize(120, 40)

	m, poll := update(m, m.load()())
	if poll == nil {
		t.Fatal("the first load must start the usage poll")
	}
	if block := plain(strings.Join(m.allocationBlock(m.Theme(), false), "\n")); !strings.Contains(block, "reading metrics-server") {
		t.Fatalf("USED must not claim anything before the first poll answers:\n%s", block)
	}
	m, tick := update(m, poll())
	if tick == nil {
		t.Fatal("a poll reply must arm the next tick")
	}
	if want := []string{"kube-system", "shop"}; !slices.Equal(podMetrics.asked, want) {
		t.Fatalf("pod metrics read for %q, want only the node's namespaces %q", podMetrics.asked, want)
	}
	if nodeMetrics.calls != 1 || !m.usedOK || m.allPods[0].pod.CPU != "4m" {
		t.Fatalf("after open: node reads=%d usedOK=%v cpu=%q", nodeMetrics.calls, m.usedOK, m.allPods[0].pod.CPU)
	}

	for range 3 {
		m, _ = update(m, kube.ResourceChangedMsg{Kind: kube.KindPod})
		m, _ = update(m, m.load()())
	}
	if len(podMetrics.asked) != 2 || nodeMetrics.calls != 1 {
		t.Fatalf("watch reloads queried metrics-server: pod reads=%q node reads=%d", podMetrics.asked, nodeMetrics.calls)
	}
	if m.allPods[0].pod.CPU != "4m" || !m.usedOK {
		t.Fatalf("reload dropped the last usage reading: cpu=%q usedOK=%v", m.allPods[0].pod.CPU, m.usedOK)
	}

	podMetrics.cpu = "9m"
	m, cmd := update(m, metricsTickMsg{epoch: m.metricsEpoch})
	m, _ = update(m, cmd())
	if len(podMetrics.asked) != 4 || nodeMetrics.calls != 2 || m.allPods[0].pod.CPU != "9m" {
		t.Fatalf("tick poll: pod reads=%q node reads=%d cpu=%q", podMetrics.asked, nodeMetrics.calls, m.allPods[0].pod.CPU)
	}
}
