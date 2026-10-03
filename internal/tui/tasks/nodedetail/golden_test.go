package nodedetail

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/testutil/goldentest"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

const giByte = 1024 * 1024 * 1024

// goldenNode mirrors 11b's mock: a node under MemoryPressure (active,
// yellow, with a kubelet message + age) alongside a healthy Ready condition
// and inactive/dim conditions, plus the automatic memory-pressure taint a
// real cluster applies for the same signal. LastTransitionTime is a
// relative offset (not a pinned absolute date) so the rendered "· 5m" age
// stays stable across days — see browse/golden_test.go's goldenPod comment
// for why a pinned date would drift.
func goldenNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-01"},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{
				{Key: "node.kubernetes.io/memory-pressure", Effect: corev1.TaintEffectNoSchedule},
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				{
					Type:               corev1.NodeMemoryPressure,
					Status:             corev1.ConditionTrue,
					Message:            "kubelet has insufficient memory available",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-5 * time.Minute)),
				},
				{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse},
				{Type: corev1.NodePIDPressure, Status: corev1.ConditionFalse},
			},
		},
	}
}

// goldenNodePods is the node's own pods, pre-sorted unhealthy-first then
// name (the order load() would produce, same as 2a's own Pods list) — the
// crashlooping pod sits on top, the two healthy ones follow alphabetically.
// Built as nodePodRow literals directly (bypassing load()'s corev1.Pod →
// kube.PodFromObject/resources.Project path) so the AGE cell (row.Cells'
// last entry) is a fixed, deterministic string rather than something a real
// Project call would derive from time.Since(creationTimestamp) — that would
// make the golden bytes drift between the UPDATE_GOLDEN run and every later
// comparison run.
func goldenNodePods() []nodePodRow {
	return []nodePodRow{
		{
			pod: kube.Pod{
				Namespace: "nva-stage", Name: "cache-redis-6f8c2", Node: "worker-01", Age: "42m",
				CPU: "180m", MEM: "2.1Gi", CPUMilli: 180, MEMBytes: 2254857830,
			},
			row: resources.Row{
				Namespace: "nva-stage", Name: "cache-redis-6f8c2",
				Cells:      []string{"cache-redis-6f8c2", "0/1", "CrashLoopBackOff", "5", "–", "–", "worker-01", "42m"},
				Status:     resources.StatusFail,
				Glyph:      "✕",
				GlyphClass: resources.StatusFail,
			},
		},
		{
			pod: kube.Pod{
				Namespace: "nva-stage", Name: "nva-worker-9k2ss", Node: "worker-01", Age: "3h",
				CPU: "410m", MEM: "3.6Gi", CPUMilli: 410, MEMBytes: 3865470157,
			},
			row: resources.Row{
				Namespace: "nva-stage", Name: "nva-worker-9k2ss",
				Cells:      []string{"nva-worker-9k2ss", "1/1", "Running", "0", "–", "–", "worker-01", "3h"},
				Status:     resources.StatusOK,
				Glyph:      "●",
				GlyphClass: resources.StatusOK,
			},
		},
		{
			pod: kube.Pod{
				Namespace: "istio-system", Name: "sidecar-envoy-x4b2q", Node: "worker-01", Age: "6h",
				CPU: "35m", MEM: "512Mi", CPUMilli: 35, MEMBytes: giByte / 2,
			},
			row: resources.Row{
				Namespace: "istio-system", Name: "sidecar-envoy-x4b2q",
				Cells:      []string{"sidecar-envoy-x4b2q", "1/1", "Running", "0", "–", "–", "worker-01", "6h"},
				Status:     resources.StatusOK,
				Glyph:      "●",
				GlyphClass: resources.StatusOK,
			},
		},
	}
}

// goldenNodeDetailModel builds one deterministic 11b model: a node with an
// active MemoryPressure condition, allocated/allocatable bars (mem hot at
// ~92%, cpu cool), one taint, and 3 of the node's own pods sorted
// unhealthy-first then name. It skips m.Init()/load() and feeds the state
// straight in via loadedMsg — see goldenNodePods' comment for why
// (podsPanel's raw Age string).
func goldenNodeDetailModel(t *testing.T, width, height int) Model {
	t.Helper()
	return goldenNodeDetailModelWithImpact(t, width, height, kube.DrainImpact{})
}

// goldenNodeDetailModelWithImpact is goldenNodeDetailModel with the
// loadedMsg's drain impact supplied — the one field the resting 11b render
// never reads, so only the drain-confirm fixture passes a non-zero one.
func goldenNodeDetailModelWithImpact(t *testing.T, width, height int, impact kube.DrainImpact) Model {
	t.Helper()
	node := goldenNode()
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {node},
	}}
	// The open seams + mutator are wired (no-op stubs) so the keybar renders
	// 11b's full hint set, not the degraded subset — same reasoning as
	// poddetail's own golden model.
	openPod := func(kube.Pod, int, int) (tea.Model, tea.Cmd) { return sentinelTask{}, nil }
	openLogs := func(kube.Pod, string, int, int) (tea.Model, tea.Cmd) { return sentinelTask{}, nil }
	openExec := func(string, string, []kube.ContainerInfo, int, int) (tea.Model, tea.Cmd) { return sentinelTask{}, nil }
	openYAML := func(kube.ResourceKind, string, string, int, int) (tea.Model, tea.Cmd) { return sentinelTask{}, nil }
	openEvents := func(kube.ResourceKind, string, string, int, int) (tea.Model, tea.Cmd) { return sentinelTask{}, nil }
	openTimeline := func(kube.ResourceKind, string, string, int, int) (tea.Model, tea.Cmd) { return sentinelTask{}, nil }
	openForward := func(kube.ForwardTarget, int, int) (tea.Model, tea.Cmd) { return sentinelTask{}, nil }

	m := New(Config{
		Session:      newSession(),
		Lister:       lister,
		Mutator:      &fakeMutator{},
		OpenPod:      openPod,
		OpenLogs:     openLogs,
		OpenExec:     openExec,
		OpenYAML:     openYAML,
		OpenEvents:   openEvents,
		OpenTimeline: openTimeline,
		OpenForward:  openForward,
		NodeDisk:     fakeDisk{},
		NodeName:     "worker-01",
	})
	m.SetSize(width, height)

	// cpu 2200m / 4000m = 55% — cool (dim). mem ~7.4Gi / 8.0Gi ≈ 92% — hot
	// (yellow), pinning 11b's "hot values yellow" bar+text treatment.
	memAllocatable := int64(8 * giByte)
	memAllocated := int64(float64(memAllocatable) * 0.925)
	// USED / CAPACITY is deliberately a different shape from REQUESTED /
	// ALLOCATABLE above — lower cpu, and a capacity larger than allocatable
	// — because the whole point of the second block is that the two are not
	// the same number, and a fixture where they agreed would let them
	// silently collapse back together.
	//
	// Update by hand rather than step: step follows every Cmd, and the
	// usage poll's next tick would keep it polling forever.
	updated, _ := m.Update(loadedMsg{
		node:        node,
		allocated:   allocation{cpuMilli: 2200, memBytes: memAllocated},
		allocatable: allocation{cpuMilli: 4000, memBytes: memAllocatable, pods: 110},
		capacity:    allocation{cpuMilli: 4000, memBytes: int64(10 * giByte)},
		pods:        goldenNodePods(),
		drainImpact: impact,
	})
	m = *updated.(*Model)
	updated, _ = m.Update(metricsLoadedMsg{
		epoch:  m.metricsEpoch,
		used:   allocation{cpuMilli: 940, memBytes: 5*giByte + giByte*2/5},
		usedOK: true,
	})
	m = *updated.(*Model)
	// disk ~81% of a 100Gi root fs — hot, so the row's yellow treatment is
	// pinned alongside mem's. The runtime sits on a separate, cool 200Gi
	// disk, pinning the img row.
	m = step(t, m, diskLoadedMsg{disk: kube.NewNodeDisk(81*giByte, 100*giByte, 19*giByte, 200*giByte, 140*giByte)})
	// The header badge's "· 8ms" comes from the conn-state ping loop — a
	// fixed latency keeps the golden deterministic (same reasoning as
	// poddetail's own golden model).
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnConnected, Latency: 8 * time.Millisecond})
	return m
}

// goldenDrainImpact tallies one of each pod shape tui.DrainImpactLine
// distinguishes through kube.DrainImpactOf — the same Add load() runs — so
// the fixture's counts come from real pod specs rather than a hand-written
// struct: a ReplicaSet pod, a bare (unmanaged) pod, a ReplicaSet pod with
// an emptyDir, and a DaemonSet pod that drain leaves in place.
func goldenDrainImpact() kube.DrainImpact {
	controlled := func(kind, name string) []metav1.OwnerReference {
		isController := true
		return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: name, Controller: &isController}}
	}
	onNode := func(name string, owners []metav1.OwnerReference, volumes []corev1.Volume) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "nva-stage", Name: name, OwnerReferences: owners},
			Spec:       corev1.PodSpec{NodeName: "worker-01", Containers: []corev1.Container{{Name: "app"}}, Volumes: volumes},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	scratch := []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	return kube.DrainImpactOf([]*corev1.Pod{
		onNode("nva-worker-9k2ss", controlled("ReplicaSet", "nva-worker-7c4d"), nil),
		onNode("debug-shell", nil, nil),
		onNode("cache-redis-6f8c2", controlled("ReplicaSet", "cache-redis-5d9b"), scratch),
		onNode("kube-proxy-q7m4n", controlled("DaemonSet", "kube-proxy"), nil),
	})
}

// goldenDrainConfirmModel is 11b with ctrl-d's drain confirm open, pinning
// the red ConfirmBorder and the impact line's Bad (unmanaged) and Warn
// (emptyDir) spans in the truecolor fixtures.
func goldenDrainConfirmModel(t *testing.T, width, height int) Model {
	t.Helper()
	m := goldenNodeDetailModelWithImpact(t, width, height, goldenDrainImpact())
	m = step(t, m, tea.KeyPressMsg{Text: verbs.Drain.Key})
	if !m.actions.Active() {
		t.Fatal("expected ctrl-d to open the drain confirm")
	}
	return m
}

func goldenNodeDetailFixtures(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"120x36.golden":       goldentest.Plain(goldenNodeDetailModel(t, 120, 36).Render()),
		"80x24.golden":        goldentest.Plain(goldenNodeDetailModel(t, 80, 24).Render()),
		"drain-120x36.golden": goldentest.Plain(goldenDrainConfirmModel(t, 120, 36).Render()),
		"drain-80x24.golden":  goldentest.Plain(goldenDrainConfirmModel(t, 80, 24).Render()),
	}
}

func goldenDir() string {
	return filepath.Join("..", "..", "..", "..", "test", "golden", "nodedetail")
}

func TestGenerateGoldenFixtures(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate nodedetail golden fixtures")
	}
	if err := os.MkdirAll(goldenDir(), 0o755); err != nil {
		t.Fatalf("mkdir golden dir: %v", err)
	}
	for name, got := range goldenNodeDetailFixtures(t) {
		if err := os.WriteFile(filepath.Join(goldenDir(), name), []byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestGoldenFixtures(t *testing.T) {
	for name, got := range goldenNodeDetailFixtures(t) {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(goldenDir(), name)
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v", path, err)
			}
			if got != string(want) {
				t.Fatalf("golden mismatch for %s\nwant:\n%s\ngot:\n%s", name, string(want), got)
			}
		})
	}
}

// truecolorGoldenFixtures renders 11b with a forced truecolor profile in
// both themes, pinning the per-cell color mapping (CONDITIONS good/warn/dim,
// hot ALLOCATED bar+text, crashlooping pod row) the plain goldens above
// can't see. The profile swap is global, so these tests must not run
// parallel with other renders in this package (none of them do).
func truecolorGoldenFixtures(t *testing.T) map[string]string {
	t.Helper()
	dark := goldenNodeDetailModel(t, 120, 36)
	light := goldenNodeDetailModel(t, 120, 36)
	light.session.Theme = tui.Light()
	drainDark := goldenDrainConfirmModel(t, 120, 36)
	drainLight := goldenDrainConfirmModel(t, 120, 36)
	drainLight.session.Theme = tui.Light()
	return map[string]string{
		"120x36-dark.golden":        goldentest.Truecolor(dark.Render()),
		"120x36-light.golden":       goldentest.Truecolor(light.Render()),
		"drain-120x36-dark.golden":  goldentest.Truecolor(drainDark.Render()),
		"drain-120x36-light.golden": goldentest.Truecolor(drainLight.Render()),
	}
}

func TestGenerateTruecolorGoldenFixtures(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate nodedetail golden fixtures")
	}
	if err := os.MkdirAll(goldenDir(), 0o755); err != nil {
		t.Fatalf("mkdir golden dir: %v", err)
	}
	for name, got := range truecolorGoldenFixtures(t) {
		if err := os.WriteFile(filepath.Join(goldenDir(), name), []byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestTruecolorGoldenFixtures(t *testing.T) {
	for name, got := range truecolorGoldenFixtures(t) {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(goldenDir(), name)
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v", path, err)
			}
			if got != string(want) {
				t.Fatalf("golden mismatch for %s\nwant:\n%s\ngot:\n%s", name, string(want), got)
			}
		})
	}
}
