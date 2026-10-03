package setup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/testutil/goldentest"
	"github.com/kute-dev/kute/internal/tui"
)

// goldenUnreachableModel builds a deterministic 4c screen: a named failing
// context, a live retry countdown, and a SWITCH CONTEXT list with all three
// row states (current/bad, reachable/good, still-probing/warn) each
// represented once — the case that mixes them under one selection is the
// one worth pinning, since that's what §4c's row-highlight covers edge to
// edge (docs/design README.md §4c).
func goldenUnreachableModel(width, height int) Model {
	model := New(Config{
		State:         Unreachable,
		ClusterName:   "microk8s-cluster",
		OtherContexts: []string{"prod-eks", "kind-local"},
	})
	model.SetSize(width, height)
	model.conn = kube.ConnState{
		Phase:       kube.ConnReconnecting,
		Err:         "dial tcp 10.0.0.5:16443: i/o timeout",
		Attempt:     2,
		NextRetryAt: model.now.Add(8 * time.Second),
	}
	model.probes = map[string]kube.ProbeResult{
		"prod-eks": {Name: "prod-eks", Latency: 32 * time.Millisecond},
	}
	// Selects "prod-eks" (row 1, reachable/good) rather than New's own
	// default, so the fixture pins the selection highlight against a
	// resolved (non-"probing…") row.
	model.switchSel = 1
	return model
}

// goldenNoConfigModel builds a deterministic 10b screen: no kubeconfig
// found anywhere, rendered as the LOOKED IN box with two checked paths.
func goldenNoConfigModel(width, height int) Model {
	lookup := &kube.ConfigLookupError{Paths: []kube.PathCheck{
		{Label: "$KUBECONFIG", Reason: "not set"},
		{Label: "~/.kube/config", Path: "/home/dev/.kube/config", Reason: "no such file"},
	}}
	model := New(Config{State: NoConfig, Err: lookup})
	model.SetSize(width, height)
	return model
}

// goldenNoConfigErrModel builds a 10b screen for a kubeconfig that exists
// but failed some other way — the title names it and the LOOKED IN box
// carries the real message.
func goldenNoConfigErrModel(err error, width, height int) Model {
	model := New(Config{State: NoConfig, Err: err})
	model.SetSize(width, height)
	return model
}

func goldenFixtures() map[string]string {
	return map[string]string{
		"unreachable-120x36.golden": goldentest.Plain(goldenUnreachableModel(120, 36).Render()),
		"unreachable-80x24.golden":  goldentest.Plain(goldenUnreachableModel(80, 24).Render()),
		"noconfig-120x36.golden":    goldentest.Plain(goldenNoConfigModel(120, 36).Render()),
		"noconfig-80x24.golden":     goldentest.Plain(goldenNoConfigModel(80, 24).Render()),
		"noconfig-context-not-found-120x36.golden": goldentest.Plain(goldenNoConfigErrModel(
			&kube.ContextNotFoundError{Name: "typo"}, 120, 36).Render()),
		"noconfig-invalid-120x36.golden": goldentest.Plain(goldenNoConfigErrModel(
			kube.NewKubeconfigLoadError("/home/dev/.kube/config", errors.New("error loading config file \"/home/dev/.kube/config\": yaml: line 4: did not find expected node content")), 120, 36).Render()),
	}
}

func TestGenerateGoldenFixtures(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate setup golden fixtures")
	}
	for name, got := range goldenFixtures() {
		path := filepath.Join("..", "..", "..", "..", "test", "golden", "setup", name)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestGoldenFixtures(t *testing.T) {
	for name, got := range goldenFixtures() {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "..", "..", "test", "golden", "setup", name)
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

// truecolorGoldenFixtures renders 4c with a forced truecolor profile in
// both themes, pinning the Theme-token-to-cell color mapping the plain
// goldens above can't see — same pattern as browse's 2a and podlogs' 5b
// (browse/golden_test.go, podlogs/golden_test.go). In particular this is
// what pins the selected SWITCH CONTEXT row's background spanning the full
// row (not just its text) and the raw-error box rendering with no
// background fill. The profile swap is global, so this package must not
// run these in parallel with other renders (none of them do).
func truecolorGoldenFixtures(t *testing.T) map[string]string {
	t.Helper()
	dark := goldenUnreachableModel(120, 36)
	dark.session = &tui.Session{Theme: tui.Dark()}
	light := goldenUnreachableModel(120, 36)
	light.session = &tui.Session{Theme: tui.Light()}
	return map[string]string{
		"unreachable-120x36-dark.golden":  goldentest.Truecolor(dark.Render()),
		"unreachable-120x36-light.golden": goldentest.Truecolor(light.Render()),
	}
}

func TestGenerateTruecolorGoldenFixtures(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate setup golden fixtures")
	}
	for name, got := range truecolorGoldenFixtures(t) {
		path := filepath.Join("..", "..", "..", "..", "test", "golden", "setup", name)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestTruecolorGoldenFixtures(t *testing.T) {
	for name, got := range truecolorGoldenFixtures(t) {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "..", "..", "test", "golden", "setup", name)
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
