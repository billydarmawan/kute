package configmapdata

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
	"github.com/kute-dev/kute/internal/testutil/goldentest"
	"github.com/kute-dev/kute/internal/tui"
)

// goldenConfigMapDataModel builds a deterministic 27a Data view: docs/
// design/v.0.2.0.dc.html's own §27a mockup scenario — cm/nva-config in
// nva-stage with a short key, one consumer of each reference kind, so the
// consumer strip has something real to show, plus a bare Pod consumer to
// pin the not-restartable rendering.
func goldenConfigMapDataModel(t *testing.T, width, height int) Model {
	t.Helper()
	cm := cmObj("nva-stage", "nva-config", map[string]string{
		"LOG_LEVEL": "info",
		"FEATURE_X": "on",
	})
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindConfigMap:   {cm},
		kube.KindDeployment:  {deploymentEnvFrom("nva-stage", "nva-worker", "nva-config")},
		kube.KindStatefulSet: {statefulSetVolume("nva-stage", "nva-gateway", "nva-config")},
		// A bare Pod: listed, but marked not restartable.
		kube.KindPod: {&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "nva-debug", Namespace: "nva-stage"}, Spec: volumeSpec("nva-config")}},
	}}
	sess := newSession()
	sess.Location.Namespace = "nva-stage"
	m := New(Config{
		Session: sess, Lister: lister, Mutator: &fakeMutator{cm: cm},
		Namespace: "nva-stage", Name: "nva-config",
	})
	m.SetSize(width, height)
	m = step(t, m, m.Init()())
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnConnected, Latency: 12 * time.Millisecond})
	return m
}

// goldenConfigMapDataAddModel drives the same scenario into 27a's own
// mid-add screenshot.
func goldenConfigMapDataAddModel(t *testing.T, width, height int) Model {
	t.Helper()
	m := goldenConfigMapDataModel(t, width, height)
	m = step(t, m, tea.KeyPressMsg{Text: "a"})
	for _, r := range "RETRIES" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	for _, r := range "3" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	return m
}

// goldenConfigMapDataEditModel drives the scenario into a single-line
// in-place edit in flight, buffer visibly diverged from "was info ·".
func goldenConfigMapDataEditModel(t *testing.T, width, height int) Model {
	t.Helper()
	m := goldenConfigMapDataModel(t, width, height)
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	for range "info" {
		m = step(t, m, tea.KeyPressMsg{Text: "backspace"})
	}
	for _, r := range "debug" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	return m
}

// goldenConfigMapDataMultilineModel opens the buffer editor on a seeded
// multi-line key — the "simpler solution" screenshot in place of 17a's own
// shared buffer editor.
func goldenConfigMapDataMultilineModel(t *testing.T, width, height int) Model {
	t.Helper()
	cm := cmObj("nva-stage", "nva-config", map[string]string{
		"nginx.conf": "server {\n  listen 80;\n  server_name nva.internal;\n}",
		"LOG_LEVEL":  "info",
	})
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindConfigMap: {cm}}}
	sess := newSession()
	sess.Location.Namespace = "nva-stage"
	m := New(Config{
		Session: sess, Lister: lister, Mutator: &fakeMutator{cm: cm},
		Namespace: "nva-stage", Name: "nva-config",
	})
	m.SetSize(width, height)
	m = step(t, m, m.Init()())
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnConnected, Latency: 12 * time.Millisecond})
	// Keys sort alphabetically ("LOG_LEVEL" before "nginx.conf"), so move
	// down onto the multi-line row before opening the buffer editor.
	m = step(t, m, tea.KeyPressMsg{Text: "down"})
	m = step(t, m, tea.KeyPressMsg{Text: "e"})
	return m
}

func goldenConfigMapDataFixtures(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"120x36.golden":           goldentest.Plain(goldenConfigMapDataModel(t, 120, 36).Render()),
		"80x24.golden":            goldentest.Plain(goldenConfigMapDataModel(t, 80, 24).Render()),
		"add-120x36.golden":       goldentest.Plain(goldenConfigMapDataAddModel(t, 120, 36).Render()),
		"add-80x24.golden":        goldentest.Plain(goldenConfigMapDataAddModel(t, 80, 24).Render()),
		"edit-120x36.golden":      goldentest.Plain(goldenConfigMapDataEditModel(t, 120, 36).Render()),
		"multiline-120x36.golden": goldentest.Plain(goldenConfigMapDataMultilineModel(t, 120, 36).Render()),
	}
}

func goldenDir() string {
	return filepath.Join("..", "..", "..", "..", "test", "golden", "configmapdata")
}

func TestGenerateGoldenFixtures(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate configmapdata golden fixtures")
	}
	if err := os.MkdirAll(goldenDir(), 0o755); err != nil {
		t.Fatalf("mkdir golden dir: %v", err)
	}
	for name, got := range goldenConfigMapDataFixtures(t) {
		if err := os.WriteFile(filepath.Join(goldenDir(), name), []byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestGoldenFixtures(t *testing.T) {
	for name, got := range goldenConfigMapDataFixtures(t) {
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

// truecolorGoldenFixtures renders 27a's Data view with a forced truecolor
// profile in both themes — see secretdata's own doc comment for why.
func truecolorGoldenFixtures(t *testing.T) map[string]string {
	t.Helper()
	dark := goldenConfigMapDataModel(t, 120, 36)
	light := goldenConfigMapDataModel(t, 120, 36)
	light.session.Theme = tui.Light()
	return map[string]string{
		"120x36-dark.golden":  goldentest.Truecolor(dark.Render()),
		"120x36-light.golden": goldentest.Truecolor(light.Render()),
	}
}

func TestGenerateTruecolorGoldenFixtures(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate configmapdata golden fixtures")
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
