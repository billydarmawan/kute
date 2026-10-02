package browse

import (
	"fmt"
	"testing"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"k8s.io/apimachinery/pkg/runtime"
)

// benchModel builds a loaded browse list of n pods at a realistic terminal
// size. It drives the model's own load command rather than Init's: Init
// returns a tea.Batch, which Update ignores, so feeding it Init()() benchmarks
// the loading skeleton with zero rows — a flat number that says nothing about
// how a frame scales with the list (docs/review/03 H5).
func benchModel(b *testing.B, n int) Model {
	b.Helper()
	objs := make([]runtime.Object, 0, n)
	for i := range n {
		objs = append(objs, pod("default", fmt.Sprintf("api-server-%04d", i)))
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: objs}}

	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	updated, _ := m.Update(m.load()())
	m = *updated.(*Model)
	if m.state != tui.TaskStateReady || len(m.display) != n {
		b.Fatalf("bench model not loaded: state %v, %d display rows, want %d", m.state, len(m.display), n)
	}
	return m
}

// BenchmarkBrowseRender measures a full frame of the one resting screen —
// the allocation path a TUI actually spends its life in, since every watch
// event redraws it. Run with -benchmem; the alloc count is the number that
// matters more than ns/op. A frame should cost the visible window, not the
// list: pods=5000 diverging from pods=50 is the regression this catches.
func BenchmarkBrowseRender(b *testing.B) {
	for _, n := range []int{50, 500, 5000} {
		b.Run(fmt.Sprintf("pods=%d", n), func(b *testing.B) {
			m := benchModel(b, n)
			b.ReportAllocs()
			for b.Loop() {
				_ = m.Render()
			}
		})
	}
	b.Run("pods=5000/scrolled", func(b *testing.B) {
		m := benchModel(b, 5000)
		m.selected = 2500
		m.clampOffset()
		b.ReportAllocs()
		for b.Loop() {
			_ = m.Render()
		}
	})
}
