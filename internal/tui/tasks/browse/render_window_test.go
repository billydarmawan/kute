package browse

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"k8s.io/apimachinery/pkg/runtime"
)

// loadedPodModel is a Ready Pods list of n pods in "default", with a metrics
// poll applied so the CPU/MEM bars (and their whole-list denominators) are in
// play, loaded through the model's own load command.
func loadedPodModel(tb testing.TB, n int) Model {
	tb.Helper()
	objs := make([]runtime.Object, 0, n)
	metrics := make(map[string]kube.PodMetrics, n)
	for i := range n {
		name := fmt.Sprintf("api-server-%04d", i)
		objs = append(objs, pod("default", name))
		metrics[kube.PodKey("default", name)] = kube.PodMetrics{CPU: fmt.Sprintf("%dm", i+1), CPUMilli: int64(i + 1), MEM: "1Mi", MemBytes: int64(i+1) << 20}
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: objs}}

	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	updated, _ := m.Update(m.load()())
	m = *updated.(*Model)
	updated, _ = m.Update(podMetricsLoadedMsg{epoch: m.metricsEpoch, namespace: m.countNamespace(), metrics: metrics})
	m = *updated.(*Model)
	if m.state != tui.TaskStateReady || len(m.display) != n {
		tb.Fatalf("model not loaded: state %v, %d display rows, want %d", m.state, len(m.display), n)
	}
	return m
}

// TestBrowseFrameCostIsBoundedByTheWindow is H5's regression guard
// (docs/review/03): a frame must cost the ~28 rows on screen, not the list.
// Building and styling a components.Row for every m.display entry made a
// 5,000-pod frame allocate ~33x what a 50-pod one did; windowed, the two are
// the same within noise. Allocations rather than time, so it's deterministic.
func TestBrowseFrameCostIsBoundedByTheWindow(t *testing.T) {
	small := loadedPodModel(t, 50)
	big := loadedPodModel(t, 5000)
	big.selected = 2500
	big.clampOffset()

	smallAllocs := testing.AllocsPerRun(3, func() { _ = small.Render() })
	bigAllocs := testing.AllocsPerRun(3, func() { _ = big.Render() })
	if bigAllocs > smallAllocs*1.25 {
		t.Fatalf("a 5000-pod frame allocated %.0f times against %.0f for 50 pods — render cost is scaling with the list, not the visible window", bigAllocs, smallAllocs)
	}
}

// TestRowAggregatesStayInStepThroughRealInteractions is what catches the
// aggregate cache going stale: View falls back to recomputing whenever the
// cache's key doesn't match, which would silently put the O(rows) pass back
// in every frame with every other test still passing. So after each real
// interaction the cache must both describe the current inputs (no fallback)
// and equal a fresh computation (not stale).
func TestRowAggregatesStayInStepThroughRealInteractions(t *testing.T) {
	m := loadedPodModel(t, 60)
	check := func(when string) {
		t.Helper()
		if m.aggs.key != m.aggregatesKey() {
			t.Fatalf("%s: aggregate cache key is stale, View would recompute every frame", when)
		}
		if got, want := m.aggs, m.computeRowAggregates(); got != want {
			t.Fatalf("%s: cached aggregates %+v, fresh %+v", when, got, want)
		}
	}
	check("after load + metrics")
	if m.aggs.cpuMax != 60 || m.aggs.health.OK != 60 {
		t.Fatalf("aggregates = %+v, want cpuMax 60 and 60 OK", m.aggs)
	}

	m = step(t, m, podMetricsLoadedMsg{epoch: m.metricsEpoch, namespace: m.countNamespace(), metrics: map[string]kube.PodMetrics{
		kube.PodKey("default", "api-server-0007"): {CPU: "900m", CPUMilli: 900},
	}})
	check("after a second metrics poll")
	if m.aggs.cpuMax != 900 {
		t.Fatalf("cpuMax = %d after the poll, want 900", m.aggs.cpuMax)
	}
	for _, key := range []string{"j", "j", "5", "/", "a", "p", "i"} {
		m = step(t, m, tea.KeyPressMsg{Text: key, Code: []rune(key)[0]})
		check("after key " + key)
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	check("after esc")
	m = step(t, m, m.load()())
	check("after a reload")
}
