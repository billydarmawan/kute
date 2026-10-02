package overview

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

type countingNodeMetrics struct {
	calls int
	cpu   int64
}

func (c *countingNodeMetrics) NodeMetrics(context.Context) (map[string]kube.NodeMetric, error) {
	c.calls++
	return map[string]kube.NodeMetric{"node-a": {CPU: "x", CPUMilli: c.cpu, MemBytes: 1}}, nil
}

// update applies msg without following the returned Cmd.
func update(m Model, msg tea.Msg) (Model, tea.Cmd) {
	updated, cmd := m.Update(msg)
	return *updated.(*Model), cmd
}

// TestWatchEventsDebounceReloadsAndNeverQueryMetricsServer: a burst of
// events costs one load, and that load reads the caches only — node usage
// comes from its own poll chain, which keeps CAPACITY live on a quiet
// cluster.
func TestWatchEventsDebounceReloadsAndNeverQueryMetricsServer(t *testing.T) {
	metrics := &countingNodeMetrics{cpu: 100}
	m := New(Config{Session: newSession(), Lister: baseLister(), NodeMetrics: metrics})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	if metrics.calls != 1 || m.capCPUUsed != 100 {
		t.Fatalf("after open: node metrics calls=%d cpu=%d, want 1 and 100", metrics.calls, m.capCPUUsed)
	}

	m, first := update(m, kube.ResourceChangedMsg{Kind: kube.KindPod})
	if first == nil {
		t.Fatal("a watch event must schedule a reload")
	}
	for _, kind := range []kube.ResourceKind{kube.KindNode, kube.KindPod, kube.KindReplicaSet} {
		var cmd tea.Cmd
		if m, cmd = update(m, kube.ResourceChangedMsg{Kind: kind}); cmd != nil {
			t.Fatalf("a %s event inside the debounce window scheduled a second reload", kind)
		}
	}
	m, load := update(m, reloadDueMsg{})
	m = step(t, m, load())
	if metrics.calls != 1 {
		t.Fatalf("a watch-driven reload queried metrics-server: %d calls, want 1", metrics.calls)
	}
	if m.capCPUUsed != 100 || !m.metricsAvailable {
		t.Fatalf("reload dropped the last usage reading: cpu=%d available=%v", m.capCPUUsed, m.metricsAvailable)
	}
	if _, cmd := update(m, kube.ResourceChangedMsg{Kind: kube.KindPod}); cmd == nil {
		t.Fatal("an event after the reload fired must schedule the next one")
	}

	metrics.cpu = 900
	m, poll := update(m, metricsTickMsg{epoch: m.metricsEpoch})
	m = step(t, m, poll())
	if metrics.calls != 2 || m.capCPUUsed != 900 {
		t.Fatalf("tick poll: calls=%d cpu=%d, want 2 and 900", metrics.calls, m.capCPUUsed)
	}
}

// TestStaleLoadReplyNeverOverwritesANewerOne: loads race, so a slower reply
// to an older load must not put its snapshot back over a newer one.
func TestStaleLoadReplyNeverOverwritesANewerOne(t *testing.T) {
	lister := baseLister()
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	older := m.startLoad()
	olderReply := older()
	lister.objects = map[kube.ResourceKind][]runtime.Object{kube.KindNode: lister.objects[kube.KindNode][:1]}
	newer := m.startLoad()
	m = step(t, m, newer())
	if m.nodeCount != 1 {
		t.Fatalf("newer reply: nodeCount=%d, want 1", m.nodeCount)
	}
	m = step(t, m, olderReply)
	if m.nodeCount != 1 {
		t.Fatalf("the older reply landing last restored a stale snapshot: nodeCount=%d, want 1", m.nodeCount)
	}
}
