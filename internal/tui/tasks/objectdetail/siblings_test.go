package objectdetail

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// nsLister filters by namespace like the real cache does ("" = all).
type nsLister struct {
	objs   []runtime.Object
	synced bool
}

func (l nsLister) ListRaw(_ context.Context, _ kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	var out []runtime.Object
	for _, o := range l.objs {
		if namespace == "" || o.(*unstructured.Unstructured).GetNamespace() == namespace {
			out = append(out, o)
		}
	}
	return out, nil
}

func (l nsLister) KindSynced(kube.ResourceKind, string) bool { return l.synced }

func certIn(ns, name string) *unstructured.Unstructured {
	u := certObj(name, map[string]any{"type": "Ready", "status": "True"})
	u.SetNamespace(ns)
	return u
}

// TestMoveSiblingAcrossNamespaces: in all-namespaces mode the next sibling
// can live in another namespace; j must load it there instead of looking in
// the current one and calling it deleted.
func TestMoveSiblingAcrossNamespaces(t *testing.T) {
	lister := nsLister{objs: []runtime.Object{certIn("aaa", "api-tls"), certIn("bbb", "web-tls")}, synced: true}
	m := New(Config{
		Session: testSession(), Lister: lister, Events: fakeEvents{},
		Kind: certificateKind(), Namespace: "aaa", Name: "api-tls",
		Siblings:     []SiblingRef{{Namespace: "aaa", Name: "api-tls"}, {Namespace: "bbb", Name: "web-tls"}},
		SiblingIndex: 0,
	})
	if m.moveSibling(1) == nil {
		t.Fatal("expected a load cmd from moveSibling")
	}
	if m.namespace != "bbb" || m.name != "web-tls" {
		t.Fatalf("moved to %s/%s, want bbb/web-tls", m.namespace, m.name)
	}
	updated, _ := m.Update(m.load()())
	got := updated.(*Model)
	if got.gone || !got.found {
		t.Fatalf("gone=%v found=%v: the sibling in another namespace read as deleted", got.gone, got.found)
	}
}

// TestNotFoundInUnsyncedCacheIsNotDeleted: an object missing from a cache
// that hasn't filled is not yet known to be gone.
func TestNotFoundInUnsyncedCacheIsNotDeleted(t *testing.T) {
	lister := nsLister{synced: false}
	m := New(Config{
		Session: testSession(), Lister: lister, Events: fakeEvents{},
		Kind: certificateKind(), Namespace: "aaa", Name: "api-tls",
	})
	updated, cmd := m.Update(m.load()())
	got := updated.(*Model)
	if got.gone {
		t.Fatal("an unsynced cache's miss rendered the \"deleted\" banner")
	}
	if got.state != tui.TaskStateLoading || cmd == nil {
		t.Fatalf("state=%s retry=%v, want loading with a cache-sync retry", got.state, cmd != nil)
	}
}
