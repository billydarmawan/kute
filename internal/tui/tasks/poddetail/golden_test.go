package poddetail

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/testutil/goldentest"
	"github.com/kute-dev/kute/internal/tui"
)

type fakeGoldenMetrics struct {
	metrics map[string]kube.PodMetrics
}

func (f fakeGoldenMetrics) PodMetricsByNamespace(context.Context, string) (map[string]kube.PodMetrics, error) {
	return f.metrics, nil
}

// goldenCrashLoopPod mirrors the 5a mock's nva-worker-9k2ss: CrashLoopBackOff
// with 6 restarts, an OOMKilled last termination 4 minutes ago, a running
// metrics sidecar, labels, owner, and a toleration. All timestamps are fixed
// *ages* (relative to now) so AGE/"4m ago" render deterministically across
// runs — a pinned absolute date would drift daily (see browse's goldenPod).
func goldenCrashLoopPod() *corev1.Pod {
	created := metav1.NewTime(time.Now().Add(-3 * time.Hour))
	terminated := metav1.NewTime(time.Now().Add(-4 * time.Minute))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nva-worker-9k2ss", Namespace: "nva-stage",
			Labels:            map[string]string{"app": "nva-worker", "tier": "backend"},
			OwnerReferences:   []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "nva-worker-7f9d2"}},
			CreationTimestamp: created,
		},
		Spec: corev1.PodSpec{
			NodeName: "worker-01",
			InitContainers: []corev1.Container{{
				Name: "prepare-config", Image: "nva/config-init:1.2.0",
			}},
			Containers: []corev1.Container{
				{
					Name:  "worker",
					Image: "nva/worker:1.42.0",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
				{Name: "metrics-sidecar", Image: "nva/sidecar:0.9.1"},
			},
			Tolerations: []corev1.Toleration{{
				Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists,
				Effect: corev1.TaintEffectNoExecute, TolerationSeconds: new(int64(300)),
			}},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			QOSClass: corev1.PodQOSBurstable,
			PodIP:    "10.1.34.19",
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "prepare-config",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0, Reason: "Completed", FinishedAt: metav1.NewTime(time.Now().Add(-3 * time.Hour)),
				}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "worker", Ready: false, RestartCount: 6,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 137, Reason: "OOMKilled", FinishedAt: terminated,
						},
					},
				},
				{
					Name: "metrics-sidecar", Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				},
			},
		},
	}
}

func goldenEvents() []kube.Event {
	return []kube.Event{
		{Type: "Warning", Reason: "BackOff", Message: "Back-off restarting failed container worker", LastSeen: time.Now().Add(-time.Minute)},
		{Type: "Warning", Reason: "OOMKilled", Message: "Container worker exceeded memory limit", LastSeen: time.Now().Add(-4 * time.Minute)},
		{Type: "Normal", Reason: "Pulled", Message: "Image already present on machine", LastSeen: time.Now().Add(-4 * time.Minute)},
	}
}

func goldenPodDetailModel(t *testing.T, width, height int) Model {
	t.Helper()
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {goldenCrashLoopPod()},
		// docs/design README.md §5a: CONTROLLER resolves a ReplicaSet owner
		// one hop further to its own Deployment ("deploy/nva-worker ↗") —
		// this ReplicaSet mirrors goldenCrashLoopPod's own OwnerReferences
		// (Kind: "ReplicaSet", Name: "nva-worker-7f9d2") so the golden
		// fixture actually exercises the resolution instead of falling back
		// to the raw ReplicaSet owner.
		kube.KindReplicaSet: {&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "nva-worker-7f9d2", Namespace: "nva-stage",
				OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "nva-worker"}},
			},
		}},
	}}
	metrics := fakeGoldenMetrics{metrics: map[string]kube.PodMetrics{
		// mock 5a: CPU 4m / 500m, MEM 246Mi / 256Mi (≥96% ⇒ red bar + text).
		kube.PodKey("nva-stage", "nva-worker-9k2ss"): {CPU: "4m", MEM: "246Mi", CPUMilli: 4, MemBytes: 246 * 1024 * 1024},
	}}
	sess := newSession()
	sess.Location.Namespace = "nva-stage"
	// The open seams + mutator + siblings are wired (no-op stubs) so the
	// keybar renders §5a's full hint set, not the degraded subset.
	openStub := func(kube.Pod, string, int, int) (tea.Model, tea.Cmd) { return sentinelTask{}, nil }
	openObjStub := func(kube.ResourceKind, string, string, int, int) (tea.Model, tea.Cmd) { return sentinelTask{}, nil }
	m := New(Config{
		Session: sess, Lister: lister, Metrics: metrics,
		Events:     fakeEvents{events: goldenEvents()},
		Mutator:    &fakeMutator{},
		OpenLogs:   openStub,
		OpenYAML:   openObjStub,
		OpenEvents: openObjStub,
		Namespace:  "nva-stage", Name: "nva-worker-9k2ss",
		Siblings: []SiblingRef{{Namespace: "nva-stage", Name: "nva-worker-9k2ss"}, {Namespace: "nva-stage", Name: "nva-worker-x4b7t"}},
	})
	m.SetSize(width, height)
	// Init's batch by hand: step follows every Cmd, and the usage poll's
	// next tick would keep it polling forever — one reading is enough.
	m = step(t, m, m.load()())
	updated, _ := m.Update(m.loadMetrics()())
	m = *updated.(*Model)
	// The header badge's "· 12ms" comes from the conn-state ping loop — a
	// fixed latency keeps the golden deterministic.
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnConnected, Latency: 12 * time.Millisecond})
	return m
}

// goldenPodDetailMetaModel is the same fixture with 26a's 'm' panel open —
// pins the detail-screen hosting of the shared metapanel (title line frozen
// above the grid, META keybar).
func goldenPodDetailMetaModel(t *testing.T, width, height int) Model {
	t.Helper()
	m := goldenPodDetailModel(t, width, height)
	return step(t, m, tea.KeyPressMsg{Text: "m"})
}

// goldenConfigErrorPod is a pod that can't start because a referenced
// Secret key is missing: pins the CreateContainerConfigError banner and,
// with 'v' pressed, the ENV & MOUNTS section.
func goldenConfigErrorPod() *corev1.Pod {
	pod := goldenCrashLoopPod()
	pod.Name = "timesheet-api-6c9f7"
	worker := &pod.Spec.Containers[0]
	worker.Env = []corev1.EnvVar{
		secretKeyEnv("ConnectionStrings__TimeSheetDb", "timesheet-backend-db", "connectionString"),
		secretKeyEnv("Email__ResendApiKey", "resend", "apiKey"),
	}
	worker.EnvFrom = []corev1.EnvFromSource{{
		ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "timesheet-config"}},
	}}
	worker.VolumeMounts = []corev1.VolumeMount{{Name: "tls", MountPath: "/etc/tls", ReadOnly: true}}
	pod.Spec.Volumes = []corev1.Volume{{
		Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "timesheet-tls"}},
	}}
	pod.Status.Phase = corev1.PodPending
	pod.Status.ContainerStatuses[0].RestartCount = 0
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
		Reason:  "CreateContainerConfigError",
		Message: `couldn't find key connectionString in Secret nva-stage/timesheet-backend-db`,
	}}
	return pod
}

func goldenPodDetailSourcesModel(t *testing.T, width, height int) Model {
	t.Helper()
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: {goldenConfigErrorPod()}}}
	sess := newSession()
	sess.Location.Namespace = "nva-stage"
	m := New(Config{Session: sess, Lister: lister, Namespace: "nva-stage", Name: "timesheet-api-6c9f7"})
	m.SetSize(width, height)
	m = step(t, m, m.Init()())
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnConnected, Latency: 12 * time.Millisecond})
	return step(t, m, tea.KeyPressMsg{Code: 'v', Text: "v"})
}

func goldenPodDetailFixtures(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"120x36.golden":         goldentest.Plain(goldenPodDetailModel(t, 120, 36).Render()),
		"80x24.golden":          goldentest.Plain(goldenPodDetailModel(t, 80, 24).Render()),
		"meta-120x36.golden":    goldentest.Plain(goldenPodDetailMetaModel(t, 120, 36).Render()),
		"meta-80x24.golden":     goldentest.Plain(goldenPodDetailMetaModel(t, 80, 24).Render()),
		"sources-120x44.golden": goldentest.Plain(goldenPodDetailSourcesModel(t, 120, 44).Render()),
		"sources-80x24.golden":  goldentest.Plain(goldenPodDetailSourcesModel(t, 80, 24).Render()),
	}
}

func goldenDir() string {
	return filepath.Join("..", "..", "..", "..", "test", "golden", "poddetail")
}

func TestGenerateGoldenFixtures(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate poddetail golden fixtures")
	}
	if err := os.MkdirAll(goldenDir(), 0o755); err != nil {
		t.Fatalf("mkdir golden dir: %v", err)
	}
	for name, got := range goldenPodDetailFixtures(t) {
		if err := os.WriteFile(filepath.Join(goldenDir(), name), []byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestGoldenFixtures(t *testing.T) {
	for name, got := range goldenPodDetailFixtures(t) {
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

// truecolorGoldenFixtures renders 5a with a forced truecolor profile in both
// themes, pinning the per-cell color mapping (termination banner, conn
// badge, MEM-at-96% red, RELATED/TOLERATIONS tokens) that the profile-less goldens above
// can't see. The profile swap is global, so these tests must not run
// parallel with other renders in this package (none of them do).
func truecolorGoldenFixtures(t *testing.T) map[string]string {
	t.Helper()
	dark := goldenPodDetailModel(t, 120, 36)
	light := goldenPodDetailModel(t, 120, 36)
	light.session.Theme = tui.Light()
	metaDark := goldenPodDetailMetaModel(t, 120, 36)
	metaLight := goldenPodDetailModel(t, 120, 36)
	metaLight.session.Theme = tui.Light()
	metaLight = step(t, metaLight, tea.KeyPressMsg{Text: "m"})
	sourcesDark := goldenPodDetailSourcesModel(t, 120, 44)
	sourcesLight := goldenPodDetailSourcesModel(t, 120, 44)
	sourcesLight.session.Theme = tui.Light()
	return map[string]string{
		"sources-120x44-dark.golden":  goldentest.Truecolor(sourcesDark.Render()),
		"sources-120x44-light.golden": goldentest.Truecolor(sourcesLight.Render()),
		"120x36-dark.golden":          goldentest.Truecolor(dark.Render()),
		"120x36-light.golden":         goldentest.Truecolor(light.Render()),
		"meta-120x36-dark.golden":     goldentest.Truecolor(metaDark.Render()),
		"meta-120x36-light.golden":    goldentest.Truecolor(metaLight.Render()),
	}
}

func TestGenerateTruecolorGoldenFixtures(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate poddetail golden fixtures")
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
