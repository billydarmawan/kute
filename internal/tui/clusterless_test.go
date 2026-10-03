package tui_test

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// clusterlessTask stands in for the 4c/10b setup root.
type clusterlessTask struct{ capturingTask }

func (t *clusterlessTask) CapturingInput() bool { return false }
func (t *clusterlessTask) Clusterless() bool    { return true }
func (t *clusterlessTask) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	t.capturingTask.Update(msg)
	return t, nil
}

// On the setup root there's nothing for a goto or namespace pick to act on,
// so g/:/n reach the screen instead of opening a palette whose choice would
// be dropped (while still overwriting Session.Location). c stays: switching
// context is how setup recovers.
func TestClusterlessScreenKeepsGotoAndNamespaceKeys(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"g", ":", "n"} {
		task := &clusterlessTask{capturingTask{screenTask: screenTask{name: "setup"}}}
		sess := &tui.Session{Theme: tui.Dark(), Location: tui.Location{Context: "c", Kind: kube.KindPod}}
		var model tea.Model = tui.NewWithSession(task, sess)
		model, _ = model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
		model, _ = model.Update(tea.KeyPressMsg{Code: rune(key[0]), Text: key})
		if model.(tui.Model).Mode() != tui.ModeBrowse {
			t.Fatalf("%q opened an overlay (mode %q) on the setup screen", key, model.(tui.Model).Mode())
		}
		if len(task.received) != 1 {
			t.Fatalf("%q: setup received %v, want the key passed through", key, task.received)
		}
	}

	task := &clusterlessTask{capturingTask{screenTask: screenTask{name: "setup"}}}
	sess := &tui.Session{Theme: tui.Dark(), Location: tui.Location{Context: "c"}}
	var model tea.Model = tui.NewWithSession(task, sess)
	model, _ = model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model, _ = model.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	if model.(tui.Model).Mode() == tui.ModeBrowse {
		t.Fatal("expected c to still open the context palette on setup")
	}
}
