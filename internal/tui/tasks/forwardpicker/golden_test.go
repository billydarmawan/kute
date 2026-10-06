package forwardpicker

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/testutil/goldentest"
	"github.com/kute-dev/kute/internal/tui"
)

// goldenPod mirrors docs/design README.md §13a's mock: a pod in nva-stage
// with two candidate ports — a privileged HTTP port (80, pre-fills 8080 per
// the "80 → 8080 since 80 is privileged" rule) and a plain metrics port
// (9090, pre-fills unchanged). Both containers are named so the NAME column
// also exercises the dim "container <name>" suffix (§13a: "NAME (+ container
// <name> in #55556e)").
//
// The busy→bump display ("8080 busy → 18080") isn't reproduced here:
// pickLocalPort probes real OS sockets, so pinning that path to a byte-exact
// golden would codepend on ambient port availability in whatever
// environment runs the test — the same nondeterminism forwardpicker_test.go
// already sidesteps with a ">=" assertion in TestInitLoadsPortsAndResolvesPod
// rather than an exact one. A clean pre-fill keeps the golden deterministic
// (task note: "otherwise a plain clean pre-fill is fine").
func goldenPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "nva-worker-9k2ss", Namespace: "nva-stage"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "web", Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 80}}},
			{Name: "worker", Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 9090}}},
		}},
	}
}

func goldenForwardPickerModel(t *testing.T, width, height int) Model {
	t.Helper()
	m := New(Config{
		Session: &tui.Session{Theme: tui.Dark()},
		Lister:  fakeLister{objs: []runtime.Object{goldenPod()}},
		Target:  kube.ForwardTarget{Kind: kube.KindPod, Namespace: "nva-stage", Name: "nva-worker-9k2ss"},
	})
	m.SetSize(width, height)
	m = loadPorts(t, m)
	return m
}

// goldenFreePortModel is the state 13a's free-port row exists for: an object
// that declares no ports at all, so the panel is that one row and it is
// selected. It's the only fixture where the row's own tokens (accent `+`, the
// dimmed label, the ghost "e.g." hint, the unassigned `localhost:-`) are on
// screen, and — having no remote port to pre-fill against — the one render
// with no `pickLocalPort` probe in it at all, so it can't drift with whatever
// the test machine happens to be listening on.
func goldenFreePortModel(t *testing.T, width, height int) Model {
	t.Helper()
	m := New(Config{
		Session: &tui.Session{Theme: tui.Dark()},
		Lister:  fakeLister{objs: []runtime.Object{goldenBarePod()}},
		Target:  kube.ForwardTarget{Kind: kube.KindPod, Namespace: "nva-stage", Name: "nva-worker-9k2ss"},
	})
	m.SetSize(width, height)
	return loadPorts(t, m)
}

func goldenBarePod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "nva-worker-9k2ss", Namespace: "nva-stage"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}}},
	}
}

func goldenForwardPickerFixtures(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"120x36.golden": goldentest.Plain(goldenForwardPickerModel(t, 120, 36).Render()),
		"80x24.golden":  goldentest.Plain(goldenForwardPickerModel(t, 80, 24).Render()),
		"no-ports.golden": goldentest.Plain(
			goldenFreePortModel(t, 120, 36).Render()),
	}
}

func goldenDir() string {
	return filepath.Join("..", "..", "..", "..", "test", "golden", "forwardpicker")
}

func TestGenerateGoldenFixtures(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate forwardpicker golden fixtures")
	}
	if err := os.MkdirAll(goldenDir(), 0o755); err != nil {
		t.Fatalf("mkdir golden dir: %v", err)
	}
	for name, got := range goldenForwardPickerFixtures(t) {
		if err := os.WriteFile(filepath.Join(goldenDir(), name), []byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestGoldenFixtures(t *testing.T) {
	for name, got := range goldenForwardPickerFixtures(t) {
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

// truecolorGoldenFixtures renders 13a with a forced truecolor profile in
// both themes, pinning the per-cell color mapping (accent header, selected
// row's SelBg highlight, dim container suffix, "will run" line) that the
// plain goldens above can't see. The profile swap is global, so these tests
// must not run parallel with other renders in this package (none of them
// do).
func truecolorGoldenFixtures(t *testing.T) map[string]string {
	t.Helper()
	dark := goldenForwardPickerModel(t, 120, 36)
	light := goldenForwardPickerModel(t, 120, 36)
	light.session.Theme = tui.Light()
	bareDark := goldenFreePortModel(t, 120, 36)
	bareLight := goldenFreePortModel(t, 120, 36)
	bareLight.session.Theme = tui.Light()
	return map[string]string{
		"120x36-dark.golden":    goldentest.Truecolor(dark.Render()),
		"120x36-light.golden":   goldentest.Truecolor(light.Render()),
		"no-ports-dark.golden":  goldentest.Truecolor(bareDark.Render()),
		"no-ports-light.golden": goldentest.Truecolor(bareLight.Render()),
	}
}

func TestGenerateTruecolorGoldenFixtures(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate forwardpicker golden fixtures")
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
