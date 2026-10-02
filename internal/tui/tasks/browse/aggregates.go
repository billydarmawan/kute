package browse

import (
	"reflect"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
)

// rowAggregates is everything a frame shows that has to look at *every*
// loaded row rather than just the visible window: the health strip's
// tallies, the CPU/MEM bar denominators, 11a's majority kubelet version, and
// 6b's namespace count. A render path that recomputed them would pay O(rows)
// per frame — every j/k, every metrics tick, every watch event — which is
// the cost tableBody's windowing exists to remove (docs/review/03 H5). So
// they're computed in Update, when their inputs change, and View only reads
// them.
type rowAggregates struct {
	key          aggregatesKey
	health       resources.HealthCounts
	cpuMax       int64
	memMax       int64
	nodeMajority string
	namespaces   int
}

// aggregatesKey identifies the inputs rowAggregates was computed from by
// identity, not content, so checking it is O(1). A wholesale reassignment of
// m.rows or m.podMetrics (every load, every metrics poll, a kind switch)
// changes it; an in-place rewrite of m.rows (the CronJob/Job one-second
// re-projection) doesn't, which is why recomputeVisible — the path every
// such rewrite already ends in — refreshes unconditionally.
type aggregatesKey struct {
	rows     *resources.Row
	nRows    int
	metrics  uintptr
	nMetrics int
	kind     kube.ResourceKind
	nColumns int
}

func (m Model) aggregatesKey() aggregatesKey {
	k := aggregatesKey{
		nRows:    len(m.rows),
		nMetrics: len(m.podMetrics),
		kind:     m.kind,
		nColumns: len(m.desc.Columns),
	}
	if len(m.rows) > 0 {
		k.rows = &m.rows[0]
	}
	if m.podMetrics != nil {
		k.metrics = reflect.ValueOf(m.podMetrics).Pointer()
	}
	return k
}

// computeRowAggregates does the whole-list pass. Pure.
func (m Model) computeRowAggregates() rowAggregates {
	a := rowAggregates{key: m.aggregatesKey()}
	if m.desc.Health != nil {
		a.health = m.desc.Health(m.rows)
	}
	a.cpuMax, a.memMax = m.metricsMax()
	if m.kind == kube.KindNode {
		a.nodeMajority = m.nodeMajorityVersion()
	}
	a.namespaces = distinctNamespaces(m.rows)
	return a
}

// refreshAggregates recomputes the cache unconditionally — for a caller that
// just rewrote m.rows in place, where the identity key can't tell.
func (m *Model) refreshAggregates() {
	m.aggs = m.computeRowAggregates()
}

// syncAggregates recomputes the cache only when its inputs were reassigned
// since it was last filled. Update's epilogue: one choke point covering
// every handler that swaps m.rows or m.podMetrics, without each having to
// remember to.
func (m *Model) syncAggregates() {
	if m.aggs.key != m.aggregatesKey() {
		m.refreshAggregates()
	}
}

// aggregates is the render path's read. It never writes the cache (render
// stays pure): when the cache doesn't describe the current rows — only ever
// a hand-built test model in practice, since Update keeps it in step — it
// falls back to computing them, so the screen is slow rather than wrong.
func (m Model) aggregates() rowAggregates {
	if m.aggs.key == m.aggregatesKey() {
		return m.aggs
	}
	return m.computeRowAggregates()
}
