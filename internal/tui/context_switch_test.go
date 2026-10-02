package tui_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/state"
	"github.com/kute-dev/kute/internal/tui"
)

// changeCountingTask counts the informer events the root forwards to it.
type changeCountingTask struct {
	screenTask
	changes int
}

func (t *changeCountingTask) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(kube.ResourceChangedMsg); ok {
		t.changes++
	}
	return t, nil
}

// A target whose client can't even be built fails before SwitchContext tears
// anything down. Rolling back then rebuilt the healthy original cluster for
// nothing — throwing away every lazily started informer and, against a slow
// link, blocking for the whole cache re-sync.
func TestFailedContextSwitchBeforeTeardownSkipsRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	kubeconfig := `
apiVersion: v1
kind: Config
current-context: dev
contexts:
- name: dev
  context: {cluster: dev, namespace: default}
- name: broken
  context: {cluster: missing, namespace: default}
clusters:
- name: dev
  cluster: {server: https://dev.example.invalid}
users: []
`
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	cluster, err := kube.NewClusterForContext("dev")
	if err != nil {
		t.Fatalf("NewClusterForContext(dev): %v", err)
	}
	t.Cleanup(cluster.Stop)

	sess := &tui.Session{
		Theme:    tui.Dark(),
		Location: tui.Location{Context: "dev"},
		State:    state.State{PerContext: map[string]state.PerContext{}, RecentContexts: []string{"dev", "broken"}},
		Cluster:  cluster,
	}
	model := tui.NewWithSession(&screenTask{name: "browse"}, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	_, cmd := updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a switch-context cmd for the preselected broken context")
	}

	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		got, ok := msg.(tui.SwitchContextMsg)
		if !ok || got.Err == nil {
			t.Fatalf("cmd() = %#v, want a failed SwitchContextMsg", msg)
		}
		if got.RolledBack {
			t.Error("RolledBack = true for a switch that failed before touching the original context")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("switch to an unbuildable context blocked: it rolled back by rebuilding the original cluster")
	}
}

// While a switch is in flight, informer events belong to a target the screen
// doesn't show yet; forwarding them reloaded the old kind and namespace
// against half-filled caches. Either outcome of the switch reloads anyway.
func TestInformerEventsHeldBackDuringContextSwitch(t *testing.T) {
	writeContextTestKubeconfig(t)
	sess := &tui.Session{
		Theme:    tui.Dark(),
		Location: tui.Location{Context: "dev"},
		State:    state.State{PerContext: map[string]state.PerContext{}, RecentContexts: []string{"dev", "prod-eks"}},
		Cluster:  &kube.Cluster{},
	}
	task := &changeCountingTask{screenTask: screenTask{name: "browse"}}
	model := tui.NewWithSession(task, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	updated, _ = updated.(tui.Model).Update(kube.ResourceChangedMsg{Kind: kube.KindPod})
	if task.changes != 0 {
		t.Fatalf("task saw %d informer events while the switch was in flight, want 0", task.changes)
	}
	updated, _ = updated.(tui.Model).Update(tui.SwitchContextMsg{Err: os.ErrDeadlineExceeded, RolledBack: true})
	_, _ = updated.(tui.Model).Update(kube.ResourceChangedMsg{Kind: kube.KindPod})
	if task.changes != 1 {
		t.Fatalf("task saw %d informer events after the switch settled, want 1", task.changes)
	}
}

// The resting browse is rebuilt on a switch too, not just a pushed stack:
// it captures the context's user and cluster name at build, so keeping it
// stamped the previous context's identity on the new cluster's run-now Jobs.
func TestContextSwitchFromRestingBrowseRebuildsIt(t *testing.T) {
	sess := testSession()
	sess.Location = tui.Location{Context: "one", Namespace: "old", Kind: kube.KindPod}
	freshBrowse := &screenTask{name: "fresh-browse"}
	model := tui.NewWithSession(&screenTask{name: "old-browse"}, sess).
		WithRootFactories(nil, func() tui.Task { return freshBrowse })

	updated, _ := model.Update(tui.SwitchContextMsg{Context: "two", Namespace: "new", Kind: kube.KindPod})
	if view := updated.(tui.Model).View().Content; !strings.Contains(view, "fresh-browse") {
		t.Fatalf("context switch kept the old context's resting browse:\n%s", view)
	}
}
