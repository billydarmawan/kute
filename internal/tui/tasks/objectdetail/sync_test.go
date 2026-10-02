package objectdetail

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// syncingLister is a fakeLister whose Event cache reports unsynced until
// eventsSynced flips — *kube.Cluster's lazy Event informer on 14d's first
// open, which answers empty until its initial LIST lands.
type syncingLister struct {
	fakeLister
	eventsSynced *bool
}

func (s syncingLister) KindSynced(kind kube.ResourceKind, _ string) bool {
	if kind == kube.KindEvent {
		return *s.eventsSynced
	}
	return true
}

// switchableEvents serves rows only once the cache has "filled".
type switchableEvents struct {
	synced *bool
	rows   []kube.Event
}

func (s switchableEvents) ObjectEvents(context.Context, string, kube.ResourceKind, string) ([]kube.Event, error) {
	if !*s.synced {
		return nil, nil
	}
	return s.rows, nil
}

// TestUnsyncedEventCacheDoesNotRedirectOrClaimNoEvents: an object with no
// conditions whose Event cache hasn't filled yet must not be redirected to
// YAML as "neither conditions nor events" — it waits on a sync retry, then
// renders the events once they arrive.
func TestUnsyncedEventCacheDoesNotRedirectOrClaimNoEvents(t *testing.T) {
	synced := false
	lister := syncingLister{
		fakeLister:   fakeLister{objs: map[kube.ResourceKind][]runtime.Object{certificateKind(): {certObj("bare", nil)}}},
		eventsSynced: &synced,
	}
	yamlCalled := false
	m := New(Config{
		Session: testSession(), Lister: lister,
		Events: switchableEvents{synced: &synced, rows: []kube.Event{{Reason: "Issuing", Message: "issuing"}}},
		Kind:   certificateKind(), Namespace: "default", Name: "bare",
		OpenYAML: func(kube.ResourceKind, string, string, int, int) (tea.Model, tea.Cmd) {
			yamlCalled = true
			return sentinelTask{}, nil
		},
	})
	updated, cmd := m.Update(m.load()())
	got, ok := updated.(*Model)
	if !ok || yamlCalled {
		t.Fatal("an unsynced Event cache read as \"no events\" and redirected to YAML")
	}
	if got.state != tui.TaskStateLoading {
		t.Fatalf("state = %s, want loading while the Event cache fills", got.state)
	}
	if cmd == nil {
		t.Fatal("no cache-sync retry scheduled; the screen would wait on a change event that may never come")
	}

	synced = true
	updated, cmd = got.Update(tui.CacheSyncRetryMsg{Gen: got.syncRetryGen})
	if cmd == nil {
		t.Fatal("the sync retry didn't reload")
	}
	updated, _ = updated.(*Model).Update(cmd())
	got, ok = updated.(*Model)
	if !ok || yamlCalled {
		t.Fatal("redirected to YAML even though the object has events")
	}
	if got.state != tui.TaskStateReady || len(got.eventRows) != 1 {
		t.Fatalf("state=%s events=%d, want ready with the object's one event", got.state, len(got.eventRows))
	}
}

// TestReloadsOnEventChanges: EVENTS is read from a cache this screen doesn't
// list, so an Event change must reload it.
func TestReloadsOnEventChanges(t *testing.T) {
	m := New(Config{
		Session: testSession(), Lister: fakeLister{}, Events: fakeEvents{},
		Kind: certificateKind(), Namespace: "default", Name: "a",
	})
	if _, cmd := m.Update(kube.ResourceChangedMsg{Kind: kube.KindEvent}); cmd == nil {
		t.Fatal("an Event change didn't reload 14d; its EVENTS grid goes stale")
	}
}
