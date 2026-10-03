package overview

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

// bestEffort reports the sync/error state of one of overview's secondary
// caches (Namespace, HelmRelease, ReplicaSet) — loadOverview's own reads for
// these three swallow every error, so without this a merely-still-filling
// or genuinely-stalled cache is indistinguishable from "the cluster truly
// has none of these." pending covers both "hasn't finished its initial fill
// yet" and "stalled on a non-permission error" — both read the same to the
// view (an "unknown right now" dash, not a false zero), and both may still
// resolve on the next reload, unlike denied.
func bestEffort(lister resources.RawLister, kind kube.ResourceKind) (pending, denied bool) {
	if !tui.KindsSynced(lister, "", kind) {
		return true, false
	}
	err := tui.KindsError(lister, "", kind)
	switch {
	case err == nil:
		return false, false
	case kube.IsPermissionError(err):
		return false, true
	default:
		return true, false
	}
}

// load fetches every source 19a's panels need — Node/Pod projections (via
// the registry, so status/glyph derivation never drifts from 11a/2a's own),
// raw Node capacity, and the ReplicaSet-derived rollout feed windowed to the
// last 30m — in one pass, from the caches only. CAPACITY's cpu/mem usage is
// deliberately not read here: load runs on watch events, and metrics-server
// is polled on the sync interval only (loadMetrics).
//
// The reply carries m.loadSeq, which the caller bumps (startLoad) — Init
// runs on a value copy and uses New's seq directly.
func (m Model) load() tea.Cmd {
	seq := m.loadSeq
	lister := m.lister
	registry := resources.Registry{}
	if m.session != nil {
		registry = m.session.Registry
	}
	timeout := m.timeout
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		data, err := loadOverview(ctx, lister, registry)
		return loadedMsg{seq: seq, data: data, err: err}
	}
}

// startLoad issues a load newer than every one already in flight.
func (m *Model) startLoad() tea.Cmd {
	m.loadSeq++
	return m.load()
}

// reloadDebounce coalesces a burst of watch events into one load — the same
// window browse and nodedetail reload on.
const reloadDebounce = 250 * time.Millisecond

// reloadDueMsg fires the debounced reload scheduleReload armed.
type reloadDueMsg struct{}

// scheduleReload arms one reload reloadDebounce from now unless one is
// already armed, so a burst of events costs one load. Unlike a trailing-edge
// debounce, a steady stream of events can't keep pushing it back.
func (m *Model) scheduleReload() tea.Cmd {
	if m.reloadPending {
		return nil
	}
	m.reloadPending = true
	return tea.Tick(reloadDebounce, func(time.Time) tea.Msg { return reloadDueMsg{} })
}

// pollInterval is how often CAPACITY's cpu/mem refresh — the same cadence
// browse and nodedetail poll node usage at, so a screen away from 11a never
// shows an older number.
const pollInterval = 2 * time.Second

// loadMetrics is one node-usage poll on the current chain.
func (m Model) loadMetrics() tea.Cmd {
	src := m.nodeMetrics
	if src == nil {
		return nil
	}
	epoch := m.metricsEpoch
	timeout := m.timeout
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		msg := metricsLoadedMsg{epoch: epoch}
		msg.usage, msg.err = readClusterUsage(ctx, src)
		return msg
	}
}

// scheduleMetricsTick arranges the next tick of an existing poll chain.
func (m Model) scheduleMetricsTick(epoch int) tea.Cmd {
	if !m.pollsMetrics() {
		return nil
	}
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return metricsTickMsg{epoch: epoch} })
}

// restartMetrics retires any running poll chain and reads usage now — on
// restore from the stack (the chain's ticks went to whichever screen was
// active and never came back) and on reconnect (pollsMetrics let it lapse
// while offline).
func (m *Model) restartMetrics() tea.Cmd {
	m.metricsEpoch++
	if !m.pollsMetrics() {
		return nil
	}
	return m.loadMetrics()
}

// pollsMetrics reports whether a usage poll is worth making: a seam to read,
// and a reachable cluster — the same gate nodedetail.pollsMetrics applies.
func (m Model) pollsMetrics() bool {
	return m.nodeMetrics != nil && !m.conn.Offline()
}

// readClusterUsage reads every node's live usage from one NodeMetrics
// poll, keeping only the nodes with a real reading. "n/a"/"" is
// kube.NodeMetric's own no-reading sentinel (browse/nodes.go's
// nodeMetricCell checks the same thing per row) — a node reporting it is
// left out, and if every node does the map comes back empty, so
// capacityLines shows a note instead of a misleadingly flatlined 0-used bar.
func readClusterUsage(ctx context.Context, src NodeMetricsReader) (map[string]kube.NodeMetric, error) {
	metrics, err := src.NodeMetrics(ctx)
	if err != nil {
		return nil, err
	}
	usage := make(map[string]kube.NodeMetric, len(metrics))
	for name, nm := range metrics {
		if nm.CPU == "" || nm.CPU == "n/a" {
			continue
		}
		usage[name] = nm
	}
	return usage, nil
}

// usageSum is CAPACITY's cpu/mem over the nodes that reported usage.
type usageSum struct {
	cpuUsed, cpuTotal int64
	memUsed, memTotal int64
	// reporting is how many of the cluster's nodes that covers.
	reporting int
}

// clusterUsage sums usage and capacity over the nodes present in both the
// last poll and the last load — only those: a NotReady or just-joined node
// has capacity but no usage, and counting its capacity anyway would
// understate the percentage for every node that does report.
func (m Model) clusterUsage() usageSum {
	var sum usageSum
	for name, nm := range m.nodeUsage {
		capacity, ok := m.nodeCapacity[name]
		if !ok {
			continue
		}
		sum.cpuUsed += nm.CPUMilli
		sum.memUsed += nm.MemBytes
		sum.cpuTotal += capacity.CPUMilli
		sum.memTotal += capacity.MemBytes
		sum.reporting++
	}
	return sum
}

func loadOverview(ctx context.Context, lister resources.RawLister, registry resources.Registry) (loadedData, error) {
	var data loadedData

	nodeObjs, err := lister.ListRaw(ctx, kube.KindNode, "")
	if err != nil {
		return data, err
	}
	nodeDesc, _ := registry.Descriptor(kube.KindNode)
	versions := map[string]int{}
	data.nodeCount = len(nodeObjs)
	for _, obj := range nodeObjs {
		if nodeDesc.Project != nil {
			row := nodeDesc.Project(obj)
			if row.Status != resources.StatusOK || row.Cordoned {
				data.nodeTrouble = append(data.nodeTrouble, row)
			}
		}
		if n, ok := obj.(*corev1.Node); ok {
			// cpu/mem divide by Capacity because their numerator below is
			// metrics-server's whole-node usage (kube.NodeCapacity explains
			// why mixing that with Allocatable reads hot); pods divides by
			// Allocatable, which is what the scheduler will actually place.
			if data.nodeCapacity == nil {
				data.nodeCapacity = make(map[string]kube.NodeResources, len(nodeObjs))
			}
			data.nodeCapacity[n.Name] = kube.NodeCapacity(n)
			data.capPodsTotal += kube.NodeAllocatable(n).Pods
			if v := n.Status.NodeInfo.KubeletVersion; v != "" {
				versions[v]++
			}
		}
	}
	data.version = majorityVersion(versions)
	data.nodeHealthy = data.nodeCount - len(data.nodeTrouble)
	sortTrouble(data.nodeTrouble)

	podObjs, err := lister.ListRaw(ctx, kube.KindPod, "")
	if err != nil {
		return data, err
	}
	podDesc, _ := registry.Descriptor(kube.KindPod)
	data.podCount = len(podObjs)
	for _, obj := range podObjs {
		if podDesc.Project == nil {
			continue
		}
		row := podDesc.Project(obj)
		if row.Status == resources.StatusWarn || row.Status == resources.StatusFail {
			data.podTrouble = append(data.podTrouble, row)
		}
		if p, ok := obj.(*corev1.Pod); ok && !podTerminal(p) {
			data.capPodsUsed++
		}
	}
	data.podHealthy = data.podCount - len(data.podTrouble)
	sortTrouble(data.podTrouble)

	if nsObjs, nsErr := lister.ListRaw(ctx, kube.KindNamespace, ""); nsErr == nil {
		data.nsCount = len(nsObjs)
	}

	// 18a's outdated releases, for the tail of the TROUBLE panel. Reading
	// this kind here starts the (server-side-filtered) release Secret
	// informer on first open of the overview — one named kind, deliberately,
	// not the breadth-first sweep the lazy-informer rule forbids. Nothing
	// here fails the screen: a cluster with no Helm at all just contributes
	// no rows.
	if helmObjs, helmErr := lister.ListRaw(ctx, kube.KindHelmRelease, ""); helmErr == nil {
		helmDesc, _ := registry.Descriptor(kube.KindHelmRelease)
		if helmDesc.Project != nil {
			for _, obj := range helmObjs {
				if row := helmDesc.Project(obj); row.Outdated {
					data.helmOutdated = append(data.helmOutdated, row)
				}
			}
			// Same cache-order reason as the two lists above: this tail of
			// the panel must not reshuffle between reloads either.
			sortTrouble(data.helmOutdated)
		}
	}

	if rsObjs, rsErr := lister.ListRaw(ctx, kube.KindReplicaSet, ""); rsErr == nil {
		cutoff := time.Now().Add(-changesWindow)
		for _, e := range kube.TimelineFromRollouts(rsObjs) {
			if e.Time.After(cutoff) {
				data.changes = append(data.changes, e)
			}
		}
		// Newest first, then namespace/object — ReplicaSet-derived rollout
		// entries routinely share a timestamp (one `kubectl apply` touching
		// several deployments), and the cache order behind them is unstable,
		// so time alone leaves those runs reshuffling on every reload.
		slices.SortStableFunc(data.changes, func(a, b kube.TimelineEntry) int {
			return cmp.Or(
				b.Time.Compare(a.Time),
				cmp.Compare(a.Namespace, b.Namespace),
				cmp.Compare(a.Object, b.Object),
			)
		})
	}

	return data, nil
}

// sortTrouble orders unhealthy rows Fail-then-Warn(-then-cordoned Neutral),
// namespace/name within each — "unhealthy-first" (docs/design README.md
// §19a) applied to the aggregated trouble list itself, not just relative to
// a healthy tail already dropped.
//
// The namespace/name tie-break is not cosmetic. These rows are projected
// straight off the informer caches here rather than through resources.List,
// so they arrive in the store's map-iteration order — a different order on
// every reload. Ranking by status alone left same-status rows to shuffle on
// every watch event, which both makes the panel unreadable and moves the row
// under the selection out from under it.
func sortTrouble(rows []resources.Row) {
	rank := func(s resources.StatusClass) int {
		switch s {
		case resources.StatusFail:
			return 0
		case resources.StatusWarn:
			return 1
		default:
			return 2
		}
	}
	slices.SortStableFunc(rows, func(a, b resources.Row) int {
		return cmp.Or(
			cmp.Compare(rank(a.Status), rank(b.Status)),
			cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)),
		)
	})
}

func podTerminal(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

func majorityVersion(counts map[string]int) string {
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

// splitObject splits a kube.TimelineEntry.Object string ("Deployment/nva-
// worker") into its Kind and Name — mirrors tasks/timeline/tasks/events' own
// helper of the same name (duplicated per the repo's package-local-seam
// convention).
func splitObject(object string) (kube.ResourceKind, string) {
	kind, name, ok := strings.Cut(object, "/")
	if !ok {
		return "", ""
	}
	return kube.ResourceKind(kind), name
}
