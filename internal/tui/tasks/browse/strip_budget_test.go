package browse

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// TestStripLineCountMatchesStrips: the table viewport is budgeted from
// stripLineCount, the frame from Strips — any line one counts and the other
// doesn't scrolls the cursor off the bottom of the table.
func TestStripLineCountMatchesStrips(t *testing.T) {
	var objs []runtime.Object
	for i := range 60 {
		objs = append(objs, pod("default", fmt.Sprintf("p-%02d", i)))
	}
	m := New(Config{Session: newSession(), Lister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: objs}}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "boom"})
	m.auxKindsDeniedNote = "still loading"
	m.followNote = "followed"
	if got, want := m.stripLineCount(), len(m.Strips(m.width)); got != want {
		t.Fatalf("stripLineCount = %d, Strips = %d lines offline with both notes", got, want)
	}

	for range 59 {
		m = step(t, m, tea.KeyPressMsg{Text: "j"})
	}
	sel := m.display[m.selected].row.row.Name
	if sel != "p-59" {
		t.Fatalf("selected %s, want p-59", sel)
	}
	if view := plain(m.Render()); !strings.Contains(view, sel) {
		t.Fatalf("selected row %s scrolled off the rendered table:\n%s", sel, view)
	}
}
