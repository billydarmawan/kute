package tui_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

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
	model := tui.NewWithSession(&frameTask{screenTask: screenTask{name: "browse"}}, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	updated, cmd := updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a switch-context cmd for the preselected broken context")
	}

	done := make(chan tea.Msg, 1)
	go func() { done <- switchResult(cmd) }()
	select {
	case msg := <-done:
		got, ok := msg.(tui.SwitchContextMsg)
		if !ok || got.Err == nil {
			t.Fatalf("cmd() = %#v, want a failed SwitchContextMsg", msg)
		}
		if got.RolledBack {
			t.Error("RolledBack = true for a switch that failed before touching the original context")
		}
		after, _ := updated.(tui.Model).Update(got)
		if last := frameLines(after.(tui.Model)); !strings.Contains(last[len(last)-1], "✕ couldn't switch to broken: ") {
			t.Errorf("keybar after a not-started switch = %q, want the failure line", last[len(last)-1])
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

// frameTask is a screenTask that renders through the real Chrome v2 Frame,
// so the root's header and keybar splices are visible in View.
type frameTask struct{ screenTask }

func (t *frameTask) View() tea.View                      { return tea.NewView(tui.Frame(120, 36, t)) }
func (t *frameTask) Update(tea.Msg) (tea.Model, tea.Cmd) { return t, nil }
func (t *frameTask) Header() tui.HeaderState {
	return tui.HeaderState{Crumbs: tui.BrandCrumbs(tui.Dark()), Conn: tui.ConnBadge{Text: "● connected"}}
}

func frameLines(m tui.Model) []string {
	return strings.Split(ansi.Strip(m.View().Content), "\n")
}

// A 7a switch can take up to 30s (switch plus rollback); the header has to
// say so, and a failure has to be said at all — it used to vanish, leaving
// the old context on screen as if the switch had never been asked for.
func TestContextSwitchShowsSwitchingBadgeThenFailureResult(t *testing.T) {
	writeContextTestKubeconfig(t)
	sess := &tui.Session{
		Theme:    tui.Dark(),
		Location: tui.Location{Context: "dev"},
		State:    state.State{PerContext: map[string]state.PerContext{}, RecentContexts: []string{"dev", "prod-eks"}},
		Cluster:  &kube.Cluster{},
	}
	model := tui.NewWithSession(&frameTask{screenTask: screenTask{name: "browse"}}, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	if lines := frameLines(updated.(tui.Model)); strings.Contains(lines[0], "switching") {
		t.Fatalf("header says switching before any switch: %q", lines[0])
	}
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	updated, cmd := updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a switch-context cmd")
	}
	lines := frameLines(updated.(tui.Model))
	if !strings.Contains(lines[0], "switching…") || strings.Contains(lines[0], "connected") {
		t.Fatalf("header while switching = %q, want the switching badge in place of connected", lines[0])
	}

	updated, _ = updated.(tui.Model).Update(tui.SwitchContextMsg{Context: "prod-eks", Err: errors.New("dial tcp 10.0.0.1:443: i/o timeout")})
	lines = frameLines(updated.(tui.Model))
	if strings.Contains(lines[0], "switching") {
		t.Fatalf("header still says switching after the switch failed: %q", lines[0])
	}
	want := "✕ couldn't switch to prod-eks: dial tcp 10.0.0.1:443: i/o timeout"
	if last := lines[len(lines)-1]; !strings.Contains(last, want) {
		t.Fatalf("keybar = %q, want %q", last, want)
	}

	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "j"})
	if last := frameLines(updated.(tui.Model)); strings.Contains(last[len(last)-1], "couldn't switch") {
		t.Fatalf("failure line outlived the next key: %q", last[len(last)-1])
	}
}

// The failure line names the reason, folded and truncated to one keybar
// line — a rollback's joined error has a newline in it.
func TestContextSwitchRollbackFailureFitsOneKeybarLine(t *testing.T) {
	writeContextTestKubeconfig(t)
	sess := &tui.Session{
		Theme:    tui.Dark(),
		Location: tui.Location{Context: "dev"},
		State:    state.State{PerContext: map[string]state.PerContext{}},
		Cluster:  &kube.Cluster{},
	}
	model := tui.NewWithSession(&frameTask{screenTask: screenTask{name: "browse"}}, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	err := errors.Join(errors.New("timed out waiting for caches"), errors.New("restore context dev: "+strings.Repeat("x", 200)))
	updated, _ = updated.(tui.Model).Update(tui.SwitchContextMsg{Context: "prod-eks", Err: err, RolledBack: true})
	lines := frameLines(updated.(tui.Model))
	if len(lines) != 36 {
		t.Fatalf("frame has %d lines, want 36: %q", len(lines), lines)
	}
	last := lines[len(lines)-1]
	if !strings.Contains(last, "✕ couldn't switch to prod-eks: timed out waiting for caches") {
		t.Fatalf("keybar = %q", last)
	}
	if w := ansi.StringWidth(last); w > 120 {
		t.Fatalf("keybar is %d cells wide, want <= 120", w)
	}
}

// switchResult runs a 7a dispatch's commands (the switch itself, batched
// with the switching badge's spinner tick) and returns the switch's result.
func switchResult(cmd tea.Cmd) tea.Msg {
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return msg
	}
	for _, c := range batch {
		if c == nil {
			continue
		}
		if got := switchResult(c); got != nil {
			if _, ok := got.(tui.SwitchContextMsg); ok {
				return got
			}
		}
	}
	return nil
}

// rebuildingTask stands in for 4c's setup screen: r starts a rebuild under
// the Session's gate, and its result is the task's own (owned) message.
type rebuildingTask struct {
	frameTask
	sess *tui.Session
}

func (t *rebuildingTask) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "r" {
		token, ok := t.sess.BeginClusterRebuild()
		if !ok {
			return t, nil
		}
		return t, func() tea.Msg { return tui.ClusterRebuildFailedMsg{Token: token, Err: errors.New("unreachable")} }
	}
	return t, nil
}

// 02 M4: a 4c rebuild landing while a 7a switch was still running revived
// the cluster the rebuild had just stopped, and the switch's result then
// relabelled the session for a context the new cluster isn't on. One of
// them now has to wait for the other: a switch asked for while a rebuild
// runs is refused before it writes anything, and says so.
func TestContextSwitchRefusedWhileRebuildInFlight(t *testing.T) {
	writeContextTestKubeconfig(t)
	sess := &tui.Session{
		Theme:    tui.Dark(),
		Location: tui.Location{Context: "dev", Namespace: "default"},
		State:    state.State{PerContext: map[string]state.PerContext{}, RecentContexts: []string{"dev", "prod-eks"}},
		Cluster:  &kube.Cluster{},
	}
	token, ok := sess.BeginClusterRebuild()
	if !ok {
		t.Fatal("BeginClusterRebuild refused with nothing running")
	}
	model := tui.NewWithSession(&frameTask{screenTask: screenTask{name: "setup"}}, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	updated, cmd := updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected the refused switch to report back")
	}
	got, ok := switchResult(cmd).(tui.SwitchContextMsg)
	if !ok || !errors.Is(got.Err, tui.ErrClusterRebuildInFlight) {
		t.Fatalf("switch result = %#v, want ErrClusterRebuildInFlight", got)
	}
	if sess.Location.Context != "dev" || len(sess.State.RecentContexts) != 2 || sess.State.RecentContexts[0] != "dev" {
		t.Fatalf("a refused switch wrote the Session: location %+v, recents %v", sess.Location, sess.State.RecentContexts)
	}
	updated, _ = updated.(tui.Model).Update(got)
	if lines := frameLines(updated.(tui.Model)); !strings.Contains(lines[len(lines)-1], "✕ couldn't switch to prod-eks: another context switch or reconnect is still running") {
		t.Fatalf("keybar = %q, want the refusal", lines[len(lines)-1])
	}
	if !sess.ClusterRebuildInFlight() {
		t.Fatal("the refused switch's result released the rebuild it was refused for")
	}

	_, _ = updated.(tui.Model).Update(tui.ReplaceRootMsg{Token: token, Task: &frameTask{screenTask: screenTask{name: "browse"}}})
	if sess.ClusterRebuildInFlight() {
		t.Fatal("the rebuild's ReplaceRootMsg didn't release the gate")
	}
}

// The other order: a rebuild asked for while a 7a switch runs is refused
// (before it could stop the cluster being switched), until the switch's
// own result lands.
func TestRebuildRefusedWhileContextSwitchInFlight(t *testing.T) {
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
	model := tui.NewWithSession(&rebuildingTask{sess: sess}, sess)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	updated, _ = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "c"})
	updated, cmd := updated.(tui.Model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a switch-context cmd")
	}
	if _, ok := sess.BeginClusterRebuild(); ok {
		t.Fatal("a rebuild was allowed to start while the switch was in flight")
	}
	msg := switchResult(cmd)
	if _, ok := sess.BeginClusterRebuild(); ok {
		t.Fatal("the gate reopened before the switch's result reached the root")
	}
	updated, _ = updated.(tui.Model).Update(msg)
	if sess.ClusterRebuildInFlight() {
		t.Fatal("the switch's result didn't release the gate")
	}

	// And a rebuild's failure releases it on the way to its task, even
	// though that message is routed to the task that started it.
	updated, cmd = updated.(tui.Model).Update(tea.KeyPressMsg{Text: "r"})
	if cmd == nil || !sess.ClusterRebuildInFlight() {
		t.Fatal("expected the task's rebuild to hold the gate")
	}
	_, _ = updated.(tui.Model).Update(cmd())
	if sess.ClusterRebuildInFlight() {
		t.Fatal("a failed rebuild's owned result didn't release the gate")
	}
}
