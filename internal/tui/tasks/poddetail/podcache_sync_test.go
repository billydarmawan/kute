package poddetail

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// podCacheLister serves pods from a mutable slice and gives the Pod cache
// its own sync state and failure reason — *kube.Cluster under
// --namespace-scoped, where opening a pod in another namespace starts a
// fresh per-namespace Pod cache that answers empty until its LIST lands.
type podCacheLister struct {
	pods   *[]runtime.Object
	synced *bool
	err    error
}

func (l podCacheLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	if kind == kube.KindPod {
		return *l.pods, nil
	}
	return nil, nil
}

func (l podCacheLister) KindSynced(kind kube.ResourceKind, _ string) bool {
	return kind != kube.KindPod || *l.synced
}

func (l podCacheLister) KindError(kind kube.ResourceKind, _ string) error {
	if kind == kube.KindPod {
		return l.err
	}
	return nil
}

// TestUnsyncedPodCacheIsNotPodDeleted: an empty read from a Pod cache that
// hasn't filled must not latch the "Pod deleted" banner (where any key goes
// back); it keeps loading and retries until the pod arrives.
func TestUnsyncedPodCacheIsNotPodDeleted(t *testing.T) {
	var pods []runtime.Object
	synced := false
	m := New(Config{Session: newSession(), Lister: podCacheLister{pods: &pods, synced: &synced}, Namespace: "other", Name: "api-0"})
	m.SetSize(120, 40)
	updated, cmd := m.Update(m.load()())
	m = *updated.(*Model)
	if m.gone || strings.Contains(plain(m.Render()), "Pod deleted") {
		t.Fatal("an unfilled Pod cache was reported as the pod being deleted")
	}
	if m.state != tui.TaskStateLoading || cmd == nil {
		t.Fatalf("state=%s retry scheduled=%v, want loading with a cache-sync retry", m.state, cmd != nil)
	}

	pods = []runtime.Object{runningPod("api-0", "other", "node-a")}
	synced = true
	updated, cmd = m.Update(tui.CacheSyncRetryMsg{Gen: m.syncRetryGen})
	updated, _ = updated.(*Model).Update(cmd())
	m = *updated.(*Model)
	if m.gone || !m.found || m.state != tui.TaskStateReady {
		t.Fatalf("after the cache filled: gone=%v found=%v state=%s", m.gone, m.found, m.state)
	}
}

// TestGoneClearsWhenThePodIsFoundAgain: the banner doesn't latch — a later
// load that finds the pod puts the screen back.
func TestGoneClearsWhenThePodIsFoundAgain(t *testing.T) {
	var pods []runtime.Object
	synced := true
	m := New(Config{Session: newSession(), Lister: podCacheLister{pods: &pods, synced: &synced}, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.load()())
	if !m.gone {
		t.Fatal("a settled cache without the pod should report it deleted")
	}
	pods = []runtime.Object{runningPod("api-0", "default", "node-a")}
	m = step(t, m, m.load()())
	if m.gone || strings.Contains(plain(m.Render()), "Pod deleted") {
		t.Fatal("gone stayed latched after the pod was found")
	}
}

// TestFailingPodCacheShowsWhy: a Pod cache that stopped filling is not a
// deleted pod either — the screen says why.
func TestFailingPodCacheShowsWhy(t *testing.T) {
	var pods []runtime.Object
	synced := true
	lister := podCacheLister{pods: &pods, synced: &synced, err: errors.New("list pods: connection refused")}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.load()())
	if m.gone || m.state != tui.TaskStateError || !strings.Contains(m.feedback, "connection refused") {
		t.Fatalf("gone=%v state=%s feedback=%q, want the cache's error", m.gone, m.state, m.feedback)
	}
}
