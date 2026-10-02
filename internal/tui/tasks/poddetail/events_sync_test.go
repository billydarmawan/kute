package poddetail

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// eventCacheLister is a fakeLister whose Event cache reports its own sync
// state and failure reason — *kube.Cluster's lazy Event informer, which
// answers empty both before its first LIST lands and when it's Forbidden.
type eventCacheLister struct {
	fakeLister
	synced    *bool
	forbidden error
}

func (l eventCacheLister) KindSynced(kind kube.ResourceKind, _ string) bool {
	return kind != kube.KindEvent || *l.synced
}

func (l eventCacheLister) KindForbidden(kind kube.ResourceKind, _ string) error {
	if kind == kube.KindEvent {
		return l.forbidden
	}
	return nil
}

func eventCachePodModel(t *testing.T, lister eventCacheLister) (Model, tea.Cmd) {
	t.Helper()
	m := New(Config{Session: newSession(), Lister: lister, Events: fakeEvents{}, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	updated, cmd := m.Update(m.load()())
	return *updated.(*Model), cmd
}

// TestUnsyncedEventCacheDoesNotClaimNoEvents: an empty read from an Event
// cache still on its first LIST says it's loading and retries, and only the
// settled cache gets to say "no events".
func TestUnsyncedEventCacheDoesNotClaimNoEvents(t *testing.T) {
	synced := false
	lister := eventCacheLister{
		fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: {runningPod("api-0", "default", "node-a")}}},
		synced:     &synced,
	}
	m, cmd := eventCachePodModel(t, lister)
	if view := plain(m.Render()); strings.Contains(view, "no events") || !strings.Contains(view, "loading events") {
		t.Fatalf("unsynced Event cache rendered as settled:\n%s", view)
	}
	if cmd == nil {
		t.Fatal("no cache-sync retry scheduled; a pod with no events never emits the change event that would reload it")
	}

	synced = true
	updated, cmd := m.Update(tui.CacheSyncRetryMsg{Gen: m.syncRetryGen})
	if cmd == nil {
		t.Fatal("the sync retry didn't reload")
	}
	updated, _ = updated.(*Model).Update(cmd())
	if view := plain(updated.(*Model).Render()); !strings.Contains(view, "no events") {
		t.Fatalf("settled empty Event cache should say no events:\n%s", view)
	}
}

// TestForbiddenEventCacheShowsWhy: a Forbidden Event list is not "no events".
func TestForbiddenEventCacheShowsWhy(t *testing.T) {
	synced := true
	lister := eventCacheLister{
		fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: {runningPod("api-0", "default", "node-a")}}},
		synced:     &synced,
		forbidden:  errors.New(`events is forbidden: cannot list resource "events"`),
	}
	m, _ := eventCachePodModel(t, lister)
	view := plain(m.Render())
	if strings.Contains(view, "no events") || !strings.Contains(view, "events unavailable · events is forbidden") {
		t.Fatalf("Forbidden Event cache must say why, not no events:\n%s", view)
	}
}

// TestReloadsOnEventChanges pins the reload that keeps EVENTS live.
func TestReloadsOnEventChanges(t *testing.T) {
	m := New(Config{Session: newSession(), Lister: fakeLister{}, Namespace: "default", Name: "api-0"})
	if _, cmd := m.Update(kube.ResourceChangedMsg{Kind: kube.KindEvent}); cmd == nil {
		t.Fatal("an Event change didn't reload 5a; its EVENTS grid goes stale")
	}
}
