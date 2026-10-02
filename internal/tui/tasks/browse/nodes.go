// Node-specific browse machinery for 11a (docs/design README.md §11a):
// live cluster/node CPU/MEM bars, PODS counts, version-skew flagging, the
// cordon/drain verbs, and 11b's node-detail push. Kept in its own file
// (browse's per-concern split, like sort.go/grouping.go/metrics.go) rather
// than sprinkled through model.go/view.go/update.go.
package browse

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// NodeMetricsReader is the live node-usage seam browse needs for 11a's
// CPU/MEM mini-bars and the health strip's "cluster cpu/mem %" — satisfied
// by *kube.Cluster and *fake.Cluster.
type NodeMetricsReader interface {
	NodeMetrics(ctx context.Context) (map[string]kube.NodeMetric, error)
}

// nodeCapacity carries both of a node's denominators, because 11a needs
// different ones for different columns: PODS divides by Allocatable (what
// the scheduler will actually place, and kubectl's own denominator), while
// the CPU/MEM bars divide by Capacity, since their numerator is
// metrics-server's whole-node usage — kubelet and container runtime
// included — and dividing that by Allocatable mixes two units of measure
// and reads hotter than `kubectl top node`. resources.Row only carries
// display Cells, so this rides alongside rowsLoadedMsg the same way
// podMetrics does for Pods.
type nodeCapacity struct {
	allocatable kube.NodeResources
	capacity    kube.NodeResources
}

// nodeMetricsLoadedMsg carries one Nodes-kind metrics poll's result. epoch
// guards it the same way podMetricsLoadedMsg guards the Pods poll.
type nodeMetricsLoadedMsg struct {
	epoch   int
	metrics map[string]kube.NodeMetric
	err     error
}

// loadMetricsCmd dispatches Init/resetAndLoad/metricsTickMsg's poll to
// whichever kind's loader applies — Pods' PodMetricsByNamespace or Nodes'
// NodeMetrics — keeping the epoch/tick scaffolding (metrics.go) shared
// across both.
func (m Model) loadMetricsCmd(epoch int) tea.Cmd {
	if m.kind == kube.KindNode {
		return m.loadNodeMetrics(epoch)
	}
	return m.loadMetrics(epoch)
}

func (m Model) loadNodeMetrics(epoch int) tea.Cmd {
	reader := m.nodeMetricsSrc
	timeout := m.timeout
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		result, err := reader.NodeMetrics(ctx)
		return nodeMetricsLoadedMsg{epoch: epoch, metrics: result, err: err}
	}
}

// loadNodeExtras re-lists raw Node/Pod objects for data resources.Row can't
// carry: each node's Capacity/Allocatable pair, how many non-terminal pods are
// currently scheduled on it (kubectl describe node's convention), the
// cluster-wide non-terminal pod total for the health strip's right side, and
// each node's pod-health tally for the HEALTH column (nodeHealthCell) —
// projectPod classifies each pod the same way the Pods list itself does, so
// a node's tally always agrees with what browse would show if you jumped
// into that node's pods.
//
// drainCount is the subset of each node's pods a drain would evict
// (kube.DrainEvicts) — the drain confirm's count, which must not include the
// DaemonSet and static pods Drain leaves in place.
func loadNodeExtras(ctx context.Context, lister resources.RawLister, projectPod func(runtime.Object) resources.Row) (capacity map[string]nodeCapacity, podCount map[string]int, total int, podHealth map[string]resources.HealthCounts, drainCount map[string]int) {
	capacity = map[string]nodeCapacity{}
	if objs, err := lister.ListRaw(ctx, kube.KindNode, ""); err == nil {
		for _, obj := range objs {
			n, ok := obj.(*corev1.Node)
			if !ok {
				continue
			}
			capacity[n.Name] = nodeCapacity{
				allocatable: kube.NodeAllocatable(n),
				capacity:    kube.NodeCapacity(n),
			}
		}
	}

	podCount = map[string]int{}
	podHealth = map[string]resources.HealthCounts{}
	drainCount = map[string]int{}
	if objs, err := lister.ListRaw(ctx, kube.KindPod, ""); err == nil {
		for _, obj := range objs {
			p, ok := obj.(*corev1.Pod)
			if !ok || p.Spec.NodeName == "" || nodeTerminalPod(p) {
				continue
			}
			podCount[p.Spec.NodeName]++
			total++
			if kube.DrainEvicts(p) {
				drainCount[p.Spec.NodeName]++
			}
			if projectPod != nil {
				h := podHealth[p.Spec.NodeName]
				switch projectPod(obj).Status {
				case resources.StatusOK:
					h.OK++
				case resources.StatusWarn:
					h.Warn++
				case resources.StatusFail:
					h.Fail++
				default:
					h.Neutral++
				}
				podHealth[p.Spec.NodeName] = h
			}
		}
	}
	return capacity, podCount, total, podHealth, drainCount
}

func nodeTerminalPod(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// nodeMetricCell renders one Node row's CPU/MEM cell: a MiniBar of live
// metrics-server usage against the node's Capacity (unlike Pod's
// busiest-visible-pod relative bar, this is a true zone bar — Warn ≥70%,
// Bad at/over capacity), then the compact usage value — "–" for both while
// metrics haven't loaded. Capacity rather than Allocatable is what makes
// this percentage agree with `kubectl top node`; see kube.NodeCapacity.
func (m Model) nodeMetricCell(name string, cpu bool, st rowCellStyles) components.Cell {
	const barWidth = 6
	valWidth := resources.MetricColumnWidth - barWidth - 1

	cap := m.nodeCapacity[name]
	denom := cap.capacity.MemBytes
	if cpu {
		denom = cap.capacity.CPUMilli
	}

	nm, ok := m.nodeMetrics[name]
	value, used := "–", int64(0)
	if ok {
		if cpu {
			value, used = nm.CPU, nm.CPUMilli
		} else {
			value, used = nm.MEM, nm.MemBytes
		}
		if value == "" || value == "n/a" {
			value = "–"
		}
	} else {
		denom = 0 // metrics not loaded yet: MiniBar renders "–" for denom<=0
	}
	valText := st.dim.Render(" " + components.Truncate(value, valWidth))
	return components.Cell{Text: components.MiniBar(used, denom, barWidth, st.bars) + valText}
}

// nodePodsCell renders the PODS column: "62/110" against the node's
// Allocatable pod capacity, or a bare count when capacity is unknown.
func (m Model) nodePodsCell(name string) string {
	cap, ok := m.nodeCapacity[name]
	if !ok || cap.allocatable.Pods == 0 {
		return strconv.Itoa(m.podCountByNode[name])
	}
	return fmt.Sprintf("%d/%d", m.podCountByNode[name], cap.allocatable.Pods)
}

// nodeHealthCell renders the HEALTH column: a compact per-status glyph tally
// of the pods scheduled on this node (OK/Warn/Fail/Neutral order, zero
// classes skipped) — the same glyphs/order/coloring as the namespace
// palette's own HEALTH column and the 2a health strip, scaled down to one
// node's pods instead of a whole namespace or cluster. A ghost dash for a
// node carrying no non-terminal pods.
func (m Model) nodeHealthCell(name string, st rowCellStyles) components.Cell {
	h := m.nodePodHealth[name]
	segments := []struct {
		class resources.StatusClass
		n     int
	}{
		{resources.StatusOK, h.OK},
		{resources.StatusWarn, h.Warn},
		{resources.StatusFail, h.Fail},
		{resources.StatusNeutral, h.Neutral},
	}
	var parts []string
	for _, seg := range segments {
		if seg.n == 0 {
			continue
		}
		parts = append(parts, st.status[seg.class].Render(fmt.Sprintf("%s%d", defaultGlyphFor(seg.class), seg.n)))
	}
	if len(parts) == 0 {
		return components.Cell{Text: "–", Style: st.dim}
	}
	return components.Cell{Text: strings.Join(parts, st.dim.Render(" "))}
}

// nodeVersionColumn finds Version's position within a Node row's Cells, so
// version-skew flagging doesn't hardcode the column order.
func nodeVersionColumn(desc resources.Descriptor) int {
	return slices.Index(desc.Columns, "Version")
}

// nodeMajorityVersion is the most common KubeletVersion across the loaded
// rows — 11a's "quiet yellow ▲" flags any node whose version differs from
// it. Empty when Version isn't a column or no rows carry one.
func (m Model) nodeMajorityVersion() string {
	idx := nodeVersionColumn(m.desc)
	if idx < 0 {
		return ""
	}
	counts := map[string]int{}
	for _, r := range m.rows {
		if idx < len(r.Cells) && r.Cells[idx] != "" {
			counts[r.Cells[idx]]++
		}
	}
	// The tie-break is load-bearing, not tidiness: map iteration order is
	// randomised, so a cluster split evenly across two kubelet versions used
	// to pick a different "majority" on each redraw and move the ▲ markers
	// with it. Lowest version wins a tie — arbitrary, but stable.
	best, bestN := "", 0
	for v, n := range counts {
		if n > bestN || (n == bestN && v < best) {
			best, bestN = v, n
		}
	}
	return best
}

// nodeSummaryText is 11a's health-strip right side: "5 nodes · 125 pods ·
// cluster cpu 46% · mem 71%" — the cluster cpu/mem clause is omitted
// entirely until the first metrics poll lands (no metrics-server installed
// degrades to just the node/pod counts).
func (m Model) nodeSummaryText() string {
	text := fmt.Sprintf("%d nodes · %d pods", len(m.rows), m.clusterPodTotal)
	if cpuPct, memPct, ok := m.clusterUsagePct(); ok {
		text += fmt.Sprintf(" · cluster cpu %d%% · mem %d%%", cpuPct, memPct)
	}
	return text
}

func (m Model) clusterUsagePct() (cpuPct, memPct int, ok bool) {
	if len(m.nodeMetrics) == 0 {
		return 0, 0, false
	}
	var usedCPU, usedMem, capCPU, capMem int64
	for name, cap := range m.nodeCapacity {
		// Capacity, matching the per-row bars above — this clause and those
		// bars are the same statement at two scales, so they must divide by
		// the same thing.
		capCPU += cap.capacity.CPUMilli
		capMem += cap.capacity.MemBytes
		if nm, found := m.nodeMetrics[name]; found {
			usedCPU += nm.CPUMilli
			usedMem += nm.MemBytes
		}
	}
	if capCPU == 0 || capMem == 0 {
		return 0, 0, false
	}
	return int(usedCPU * 100 / capCPU), int(usedMem * 100 / capMem), true
}

// beginCordon toggles row's schedulable state — TierNone (verbs.Cordon), so
// actions.Controller.Begin executes immediately with no confirmation.
func (m *Model) beginCordon(row resources.Row) tea.Cmd {
	verb, label := "cordon", fmt.Sprintf("Cordon %s?", row.Name)
	if row.Cordoned {
		verb, label = "uncordon", fmt.Sprintf("Uncordon %s?", row.Name)
	}
	return m.actions.Begin(verbs.Cordon.Tier, tui.TaskAction{
		ID:    "node-" + verb + "-" + row.Name,
		Label: label,
		Scope: tui.TaskScope{ResourceKind: string(kube.KindNode), ResourceName: row.Name, Verb: verb, IsMutating: true},
	})
}

// beginDrain confirms draining row — TierModal (verbs.Drain) — showing how
// many pods the cache says will be evicted.
func (m *Model) beginDrain(row resources.Row) tea.Cmd {
	return m.actions.Begin(verbs.Drain.Tier, tui.TaskAction{
		ID:    "node-drain-" + row.Name,
		Label: fmt.Sprintf("Drain %s? %d pods will be evicted.", row.Name, m.drainCountByNode[row.Name]),
		Scope: tui.TaskScope{ResourceKind: string(kube.KindNode), ResourceName: row.Name, Verb: "drain", IsMutating: true},
	})
}

// confirmBody renders the drain (or any future TierModal verb) confirmation
// card in place of the normal table body while m.actions is Active — the
// minimal inline shape ahead of 8b's full type-the-name modal
// (components.ConfirmCard's doc comment).
func (m Model) confirmBody(width, height int) string {
	theme := m.Theme()
	title := "Confirm"
	detail := ""
	if pending := m.actions.Pending(); pending != nil {
		title = pending.Label
	}
	styles := components.ConfirmStyles{
		Border: lipgloss.NewStyle().BorderForeground(theme.ConfirmBorder).Background(theme.ConfirmHeaderBg),
		Title:  lipgloss.NewStyle().Foreground(theme.Text).Bold(true).Background(theme.ConfirmHeaderBg),
		Detail: lipgloss.NewStyle().Foreground(theme.TextSecondary).Background(theme.ConfirmHeaderBg),
		Rule:   lipgloss.NewStyle().Foreground(theme.TextGhost).Background(theme.ConfirmHeaderBg),
		Key:    lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.ConfirmHeaderBg),
		Label:  lipgloss.NewStyle().Foreground(theme.TextDim).Background(theme.ConfirmHeaderBg),
	}
	return components.ConfirmCard(title, detail, styles, width, height)
}

// openSelectedNodeDetail pushes 11b for the selected Node row. ok is false
// when nodedetail isn't wired or nothing's selected, mirroring
// openSelectedLogs' contract so 'enter' stays a no-op rather than pushing a
// broken screen.
func (m Model) openSelectedNodeDetail() (tea.Model, tea.Cmd, bool) {
	if m.openNodeDetail == nil || m.kind != kube.KindNode {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	task, cmd := m.openNodeDetail(row.Name, m.width, m.height)
	return task, cmd, task != nil
}
