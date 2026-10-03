package nodedetail

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// errMetricsGone is what the metrics client returns when the metrics.k8s.io
// API isn't served at all — a 404, i.e. no metrics-server.
var errMetricsGone = apierrors.NewNotFound(schema.GroupResource{Group: "metrics.k8s.io", Resource: "nodes"}, "")

// allocationModel is a ready 11b model with both bar groups populated.
func allocationModel(t *testing.T, usedOK bool) Model {
	t.Helper()
	m := New(Config{Session: newSession(), Lister: fakeLister{}, NodeName: "worker-01"})
	m.SetSize(120, 36)
	m = step(t, m, loadedMsg{
		node:        goldenNode(),
		allocated:   allocation{cpuMilli: 2200, memBytes: 5 * giByte},
		allocatable: allocation{cpuMilli: 3800, memBytes: 13 * giByte, pods: 110},
		capacity:    allocation{cpuMilli: 4000, memBytes: 16 * giByte},
		pods:        goldenNodePods(),
	})
	msg := metricsLoadedMsg{epoch: m.metricsEpoch, used: allocation{cpuMilli: 940, memBytes: 8 * giByte}, usedOK: true}
	if !usedOK {
		msg = metricsLoadedMsg{epoch: m.metricsEpoch, usedErr: errMetricsGone}
	}
	return step(t, m, msg)
}

// TestUsedBlockKeepsLastReadingOnFailedPoll: one timed-out or 503'd poll
// says nothing about the node's usage, so the bars it last drew stay up —
// they used to flip to "no metrics-server installed" until the next poll.
func TestUsedBlockKeepsLastReadingOnFailedPoll(t *testing.T) {
	m := allocationModel(t, true)
	m = step(t, m, metricsLoadedMsg{epoch: m.metricsEpoch, usedErr: context.DeadlineExceeded})

	view := ansi.Strip(m.Render())
	if !strings.Contains(view, "940m / 4000m") {
		t.Errorf("a failed poll dropped the last good reading:\n%s", view)
	}
	if strings.Contains(view, "no metrics-server installed") {
		t.Errorf("a failed poll claimed metrics-server isn't installed:\n%s", view)
	}
}

// TestUsedBlockNamesWhyThereIsNoReading: only a missing metrics.k8s.io API
// is "not installed" — Forbidden, a transient failure and a node
// metrics-server simply hasn't scraped each say what actually happened.
func TestUsedBlockNamesWhyThereIsNoReading(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "metrics.k8s.io", Resource: "nodes"}, "", nil)
	tests := []struct {
		name string
		msg  metricsLoadedMsg
		want string
	}{
		{"api not served", metricsLoadedMsg{usedErr: errMetricsGone}, "no metrics-server installed"},
		{"forbidden", metricsLoadedMsg{usedErr: forbidden}, "not permitted to read metrics-server"},
		{"timeout", metricsLoadedMsg{usedErr: context.DeadlineExceeded}, "metrics-server unavailable"},
		{"node not scraped", metricsLoadedMsg{}, "no reading for this node yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New(Config{Session: newSession(), Lister: fakeLister{}, NodeName: "worker-01"})
			m.SetSize(120, 36)
			m = step(t, m, loadedMsg{node: goldenNode(), capacity: allocation{cpuMilli: 4000, memBytes: 16 * giByte}, pods: goldenNodePods()})
			tt.msg.epoch = m.metricsEpoch
			view := ansi.Strip(step(t, m, tt.msg).Render())
			if !strings.Contains(view, "cpu / mem — "+tt.want) {
				t.Errorf("view missing %q:\n%s", tt.want, view)
			}
		})
	}
}

// TestBothAllocationGroupsRender is the fix for the report that started
// this: the nodes list showed one memory number and this screen a different
// one, with nothing saying they measured different things. Both now appear
// here, each under a title naming its own denominator.
func TestBothAllocationGroupsRender(t *testing.T) {
	view := ansi.Strip(allocationModel(t, true).Render())

	for _, want := range []string{
		"REQUESTED / ALLOCATABLE",
		"2200m / 3800m",  // requests over what the scheduler may hand out
		"5.0Gi / 13.0Gi", // ditto, memory
		"USED / CAPACITY",
		"940m / 4000m",   // live usage over the whole machine
		"8.0Gi / 16.0Gi", // the number the nodes list shows for this node
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
}

// TestUsedBlockDegradesWithoutMetricsServer — a zeroed bar would claim the
// node is idle, which is a wrong answer rather than a missing one.
func TestUsedBlockDegradesWithoutMetricsServer(t *testing.T) {
	view := ansi.Strip(allocationModel(t, false).Render())

	if !strings.Contains(view, "no metrics-server installed") {
		t.Errorf("view missing the no-metrics-server note:\n%s", view)
	}
	// Nothing may be drawn against the capacity denominators at all — not
	// even a zeroed bar. (Both differ from the allocatable ones above, so
	// these are unambiguous.)
	for _, bad := range []string{"/ 4000m", "/ 16.0Gi"} {
		if strings.Contains(view, bad) {
			t.Errorf("view drew a flatlined usage bar against %q:\n%s", bad, view)
		}
	}
	// The requests half doesn't depend on metrics-server and must survive.
	if !strings.Contains(view, "5.0Gi / 13.0Gi") {
		t.Errorf("view lost REQUESTED / ALLOCATABLE:\n%s", view)
	}
}

// TestPodsCountIgnoresTheFilter: "pods 3 / 110" is a statement about the
// node, so filtering the table below it must not change it. It used to read
// len(m.pods) — the filtered slice — and tracked whatever was typed.
func TestPodsCountIgnoresTheFilter(t *testing.T) {
	m := allocationModel(t, true)
	if want := "3 / 110"; !strings.Contains(ansi.Strip(m.Render()), want) {
		t.Fatalf("view missing %q before filtering", want)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	for _, r := range "cache" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}

	view := ansi.Strip(m.Render())
	if !strings.Contains(view, "3 / 110") {
		t.Errorf("pods count moved with the filter (want 3 / 110):\n%s", view)
	}
}
