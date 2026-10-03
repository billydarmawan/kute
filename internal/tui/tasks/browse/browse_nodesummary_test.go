package browse

import (
	"testing"

	"github.com/kute-dev/kute/internal/kube"
)

// TestNodeSummaryDividesOnlyByReportingNodes: a NotReady or just-joined
// node has capacity but no usage, so counting its capacity understated the
// cluster percentage for every node that does report. The clause divides by
// the reporting nodes' capacity and says how many that is.
func TestNodeSummaryDividesOnlyByReportingNodes(t *testing.T) {
	m := Model{
		nodeCapacity: map[string]nodeCapacity{
			"a": {capacity: kube.NodeResources{CPUMilli: 1000, MemBytes: 1000}},
			"b": {capacity: kube.NodeResources{CPUMilli: 1000, MemBytes: 1000}},
			"c": {capacity: kube.NodeResources{CPUMilli: 1000, MemBytes: 1000}},
		},
		nodeMetrics: map[string]kube.NodeMetric{
			"a": {CPU: "500m", CPUMilli: 500, MemBytes: 250},
			"b": {CPU: "500m", CPUMilli: 500, MemBytes: 750},
			"c": {CPU: "n/a"},
		},
	}
	if got, want := m.nodeSummaryText(), "0 nodes · 0 pods · cluster cpu 50% · mem 50% (2/3 nodes)"; got != want {
		t.Errorf("nodeSummaryText = %q, want %q", got, want)
	}

	m.nodeMetrics["c"] = kube.NodeMetric{CPU: "500m", CPUMilli: 500, MemBytes: 500}
	if got, want := m.nodeSummaryText(), "0 nodes · 0 pods · cluster cpu 50% · mem 50%"; got != want {
		t.Errorf("nodeSummaryText = %q, want %q", got, want)
	}
}
