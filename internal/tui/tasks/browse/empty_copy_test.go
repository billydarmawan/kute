package browse

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// TestAllNamespacesEmptyStateCopy: 10c in 6b's all-namespaces scope has no
// namespace to name and nowhere wider to send the user, and its "other
// kinds" counts are cluster-wide, not "this namespace".
func TestAllNamespacesEmptyStateCopy(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNamespace: {namespace("default"), namespace("aaa")},
		kube.KindSecret:    {secret("aaa", "s")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m = step(t, m, tui.SwitchNamespaceMsg{Namespace: ""})
	if m.state != tui.TaskStateEmpty || !m.grouped() {
		t.Fatalf("state=%s grouped=%v, want an empty all-namespaces list", m.state, m.grouped())
	}

	view := plain(m.Render())
	for _, want := range []string{"no pods in any namespace", "switch namespace", "the cluster has 1 secret"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	for _, unwanted := range []string{"the namespace exists", "a all namespaces", "this namespace has"} {
		if strings.Contains(view, unwanted) {
			t.Fatalf("all-namespaces empty state says %q:\n%s", unwanted, view)
		}
	}
}

// TestEmptyStateNamesADeletedNamespace: a namespace restored from state can
// have been deleted since; 10c must not claim it exists.
func TestEmptyStateNamesADeletedNamespace(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNamespace: {namespace("kube-system"), namespace("other")},
	}}
	m := New(Config{Session: newSession(), Lister: lister}) // Location.Namespace = default
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	if m.state != tui.TaskStateEmpty {
		t.Fatalf("state = %s, want empty", m.state)
	}

	view := plain(m.Render())
	if strings.Contains(view, "the namespace exists") {
		t.Fatalf("claimed a deleted namespace exists:\n%s", view)
	}
	if !strings.Contains(view, "the namespace no longer exists") {
		t.Fatalf("view doesn't say the namespace is gone:\n%s", view)
	}
}

// TestHelmEmptyStateNamesTheStorageDriver: kute reads only helm's default
// Secret storage, so "no releases" must say so — a release kept by the
// configmap or sql driver exists all the same.
func TestHelmEmptyStateNamesTheStorageDriver(t *testing.T) {
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	m := New(Config{Session: session, Lister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{}}})
	m.SetSize(160, 36)
	m = step(t, m, m.Init()())
	if m.state != tui.TaskStateEmpty {
		t.Fatalf("state=%s, want empty", m.state)
	}
	if view := plain(m.Render()); !strings.Contains(view, "HELM_DRIVER=configmap or sql") {
		t.Fatalf("helm empty state doesn't name the storage it read:\n%s", view)
	}
}
