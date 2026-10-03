package nodedetail

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
)

// errNodeNotFound is findNode's sentinel for "this node isn't in the cache
// right now" — deliberately not enough on its own to conclude the node is
// gone. applyLoaded checks the Node cache's own sync/error state before
// believing it: a cache that hasn't finished its initial fill, or one
// that's Forbidden, looks exactly like a missing node to a scan for one
// name.
var errNodeNotFound = errors.New("node not found")

// reloadDebounce is how long a still-syncing retry waits before re-running
// load() — same value as browse's own reloadDebounce, duplicated per the
// repo's package-local-seam convention.
const reloadDebounce = 250 * time.Millisecond

// reloadDueMsg fires scheduleReload's retry — epoch guards a stale reply
// (from an earlier retry, or a since-superseded node) against re-triggering
// a load() that's no longer wanted, mirroring browse's own reloadDueMsg.
type reloadDueMsg struct{ epoch int }

// scheduleReload arranges one reloadDueMsg reloadDebounce from now —
// mirrors browse's own scheduleReload.
func (m Model) scheduleReload(epoch int) tea.Cmd {
	return tea.Tick(reloadDebounce, func(time.Time) tea.Msg {
		return reloadDueMsg{epoch: epoch}
	})
}

// load fetches the node itself and its non-terminal pods (spec.nodeName
// match) and builds the facts panel's REQUESTED / ALLOCATABLE half: the
// pods' effective requests against what the scheduler may hand out.
// Requests, not usage, so this half still renders on a cluster with no
// metrics-server.
//
// USED / CAPACITY and the pod rows' own CPU/MEM cells are deliberately not
// read here: load runs on every Pod or Node watch event anywhere in the
// cluster, and metrics-server is polled on the sync interval only
// (loadMetrics). applyLoaded overlays the last poll's reading instead.
func (m Model) load() tea.Cmd {
	lister := m.lister
	nodeName := m.nodeName
	timeout := m.timeout
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()

		node, err := findNode(ctx, lister, nodeName)
		if err != nil {
			return loadedMsg{err: err}
		}

		podObjs, err := lister.ListRaw(ctx, kube.KindPod, "")
		if err != nil {
			return loadedMsg{err: err}
		}

		podDesc, _ := resources.DefaultRegistry().Descriptor(kube.KindPod)

		var allocated allocation
		var drainImpact kube.DrainImpact
		rows := make([]nodePodRow, 0, len(podObjs))
		for _, obj := range podObjs {
			p, ok := obj.(*corev1.Pod)
			if !ok || p.Spec.NodeName != nodeName || nodeDetailTerminalPod(p) {
				continue
			}
			pod := kube.PodFromObject(p)
			// The pod's *effective* request, not the bare sum over
			// spec.containers that kube.Pod carries: native sidecars, a
			// heavy init container and spec.overhead all reserve room on
			// the node too, and kubectl describe node counts them.
			cpuReq, memReq := kube.PodEffectiveRequests(p)
			allocated.cpuMilli += cpuReq
			allocated.memBytes += memReq
			drainImpact.Add(p)
			rows = append(rows, nodePodRow{pod: pod, row: podDesc.Project(p)})
		}
		slices.SortStableFunc(rows, func(a, b nodePodRow) int {
			return cmp.Or(
				cmp.Compare(healthRank(a.row.Status), healthRank(b.row.Status)),
				cmp.Compare(strings.ToLower(a.pod.Name), strings.ToLower(b.pod.Name)),
			)
		})

		return loadedMsg{
			node:        node,
			allocated:   allocated,
			allocatable: nodeAllocatable(node),
			capacity:    nodeCapacity(node),
			pods:        rows,
			drainImpact: drainImpact,
		}
	}
}

func findNode(ctx context.Context, lister resources.RawLister, name string) (*corev1.Node, error) {
	objs, err := lister.ListRaw(ctx, kube.KindNode, "")
	if err != nil {
		return nil, err
	}
	for _, obj := range objs {
		if n, ok := obj.(*corev1.Node); ok && n.Name == name {
			return n, nil
		}
	}
	return nil, fmt.Errorf("node %q not found: %w", name, errNodeNotFound)
}

func nodeAllocatable(n *corev1.Node) allocation {
	return fromNodeResources(kube.NodeAllocatable(n))
}

// nodeCapacity is the USED bar's denominator — the whole machine, since
// node usage is measured across the whole machine (kube.NodeCapacity).
func nodeCapacity(n *corev1.Node) allocation {
	return fromNodeResources(kube.NodeCapacity(n))
}

func fromNodeResources(r kube.NodeResources) allocation {
	return allocation{cpuMilli: r.CPUMilli, memBytes: r.MemBytes, pods: r.Pods}
}

// readNodeUsage pulls this node's live usage out of one NodeMetrics poll.
// ok is false for every way the reading can be absent — no seam wired, the
// poll failed (no metrics-server), the node isn't in the map, or it reports
// kube.NodeMetric's own ""/"n/a" sentinel — so the caller has a single flag
// to decide between a real bar and the "no metrics-server" note. This is
// the same sentinel check overview's loadOverview applies.
func readNodeUsage(ctx context.Context, src NodeMetricsReader, nodeName string) (allocation, bool) {
	if src == nil {
		return allocation{}, false
	}
	metrics, err := src.NodeMetrics(ctx)
	if err != nil {
		return allocation{}, false
	}
	nm, found := metrics[nodeName]
	if !found || nm.CPU == "" || nm.CPU == "n/a" {
		return allocation{}, false
	}
	return allocation{cpuMilli: nm.CPUMilli, memBytes: nm.MemBytes}, true
}

func nodeDetailTerminalPod(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// healthRank orders StatusClass worst-first — failing/warning rows sort to
// the top, neutral rows sink to the bottom — duplicated from browse's own
// healthRank per the repo's package-local-seam convention, so this screen's
// pods sort exactly like 2a's own Pods list (unhealthy-first, then name).
func healthRank(class resources.StatusClass) int {
	switch class {
	case resources.StatusFail:
		return 0
	case resources.StatusWarn:
		return 1
	case resources.StatusOK:
		return 2
	default:
		return 3
	}
}

// pollInterval is how often the USED / CAPACITY block and the pod rows'
// own CPU/MEM cells refresh — the same cadence browse polls at, so walking
// from the nodes list into a node doesn't change how fresh the number is.
const pollInterval = 2 * time.Second

// scheduleMetricsTick arranges the next tick of an existing chain.
func (m Model) scheduleMetricsTick(epoch int) tea.Cmd {
	if !m.pollsMetrics() {
		return nil
	}
	return tea.Tick(pollInterval, func(time.Time) tea.Msg {
		return metricsTickMsg{epoch: epoch}
	})
}

// armMetricsTick starts a *new* poll chain, retiring any chain already
// running. Called when the screen first appears and whenever it is restored
// from the stack, where the previous chain's tick was delivered to whatever
// screen was active instead and so never came back.
func (m *Model) armMetricsTick() tea.Cmd {
	m.metricsEpoch++
	return m.scheduleMetricsTick(m.metricsEpoch)
}

// pollsMetrics reports whether a usage poll is worth making: there has to
// be a seam to read, and the cluster has to be reachable — polling a
// cluster we know is offline just burns a timeout per tick, the same gate
// browse.pollsMetrics applies.
func (m Model) pollsMetrics() bool {
	if m.metrics == nil && m.nodeMetrics == nil && m.nodeDisk == nil {
		return false
	}
	return !m.conn.Offline()
}

// maxNamespaceReads caps the per-namespace pod-metrics reads one poll
// makes. A node's pods usually span a handful of namespaces, where one List
// each costs a fraction of the cluster-wide one; past this many round trips
// a single cluster-wide List is the cheaper of the two.
const maxNamespaceReads = 8

// loadMetrics is the poll itself: usage only, deliberately not a full
// load(). A node's pod set changes on a watch event, which already
// triggers a reload — re-listing every pod in the cluster every two
// seconds to refresh a number would make this screen more expensive than
// the list it was opened from. For the same reason pod usage is read only
// for the namespaces this node's pods live in, not cluster-wide: the table
// holds at most a node's worth of pods, and metrics-server serializes every
// pod in whatever it's asked for.
func (m Model) loadMetrics() tea.Cmd {
	metrics := m.metrics
	nodeMetrics := m.nodeMetrics
	nodeName := m.nodeName
	timeout := m.timeout
	epoch := m.metricsEpoch
	namespaces := podNamespaces(m.allPods)
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()

		msg := metricsLoadedMsg{epoch: epoch}
		msg.used, msg.usedOK = readNodeUsage(ctx, nodeMetrics, nodeName)
		if metrics != nil {
			msg.podMetrics = readPodUsage(ctx, metrics, namespaces)
		}
		return msg
	}
}

// podNamespaces is the sorted set of namespaces rows' pods live in.
func podNamespaces(rows []nodePodRow) []string {
	var namespaces []string
	for _, r := range rows {
		namespaces = append(namespaces, r.pod.Namespace)
	}
	slices.Sort(namespaces)
	return slices.Compact(namespaces)
}

// readPodUsage reads pod usage for namespaces — one List each, or one
// cluster-wide List past maxNamespaceReads. nil means nothing was read (no
// pods yet, or every read failed), which applyMetrics treats as "keep the
// last reading" rather than blanking the column.
func readPodUsage(ctx context.Context, src MetricsReader, namespaces []string) map[string]kube.PodMetrics {
	if len(namespaces) == 0 {
		return nil
	}
	if len(namespaces) > maxNamespaceReads {
		usage, _ := src.PodMetricsByNamespace(ctx, "")
		return usage
	}
	var merged map[string]kube.PodMetrics
	for _, ns := range namespaces {
		usage, err := src.PodMetricsByNamespace(ctx, ns)
		if err != nil {
			continue
		}
		if merged == nil {
			merged = make(map[string]kube.PodMetrics, len(usage))
		}
		maps.Copy(merged, usage)
	}
	return merged
}

// diskPollEvery is how many usage-poll ticks pass between disk reads — 30s
// at pollInterval. The kubelet's own filesystem stats refresh on roughly a
// minute's housekeeping cycle, and each read returns every pod's stats
// alongside the node's, so polling it at the cpu/mem rate would pay for a
// number that hasn't moved.
const diskPollEvery = 15

// diskDue reports whether this tick should also read the disk. A Forbidden
// answer ends the reads for this screen: RBAC isn't going to change under a
// 30s retry, and each attempt is still a round trip.
func (m Model) diskDue() bool {
	return m.nodeDisk != nil && m.diskState != diskForbidden && m.diskTicks >= diskPollEvery
}

// loadDisk reads this node's root filesystem usage once.
func (m Model) loadDisk() tea.Cmd {
	reader := m.nodeDisk
	if reader == nil {
		return nil
	}
	nodeName := m.nodeName
	timeout := m.timeout
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		disk, err := reader.NodeDiskUsage(ctx, nodeName)
		return diskLoadedMsg{disk: disk, err: err}
	}
}

// applyDisk folds one disk read into the model. A failed read after a good
// one keeps the last good figure rather than flapping the row to an error
// on one dropped proxy call; Forbidden always wins, since it is permanent.
func (m *Model) applyDisk(msg diskLoadedMsg) {
	switch {
	case msg.err == nil:
		m.disk, m.diskState = msg.disk, diskOK
	case errors.Is(msg.err, kube.ErrNodeStatsForbidden):
		m.diskState = diskForbidden
	case m.diskState != diskOK:
		m.diskState = diskUnavailable
	}
}
