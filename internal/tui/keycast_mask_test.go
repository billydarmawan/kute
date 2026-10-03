package tui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/tui"
)

// maskingTask is a capturingTask whose buffer holds a secret value.
type maskingTask struct {
	capturingTask
	masks bool
}

func (t *maskingTask) MasksInput() bool { return t.masks }
func (t *maskingTask) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	t.capturingTask.Update(msg)
	return t, nil
}

// --keycast exists for demo recordings, so it must not spell out a value
// typed into a masked Secret buffer (27b: every message masks the value).
func TestKeycastSkipsKeysTypedIntoMaskedBuffer(t *testing.T) {
	t.Parallel()
	for _, masks := range []bool{false, true} {
		task := &maskingTask{capturingTask: capturingTask{screenTask: screenTask{name: "secretdata"}}, masks: masks}
		var model tea.Model = tui.New(task).WithKeycast(true)
		model, _ = model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
		for _, r := range "hunter" {
			model, _ = model.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		view := model.(tui.Model).View().Content
		if shown := strings.Contains(view, "h-u-n-t-e-r"); shown == masks {
			t.Fatalf("masks=%v: keycast chip shown=%v, want %v:\n%s", masks, shown, !masks, view)
		}
		if len(task.received) != 6 {
			t.Fatalf("masks=%v: task received %v, want every key still delivered", masks, task.received)
		}
	}
}
