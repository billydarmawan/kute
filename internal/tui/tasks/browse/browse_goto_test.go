package browse

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// TestGotoKindMsgSwitchesKindKeepingNamespace covers the jump palette's
// kind-result Enter (mvp-plan.md Phase 2): kind changes in place, namespace
// doesn't, and the new kind's rows load.
func TestGotoKindMsgSwitchesKindKeepingNamespace(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod:       {pod("default", "api-1")},
		kube.KindConfigMap: {configMap("default", "app-config")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	if m.kind != kube.KindPod {
		t.Fatalf("kind = %s, want Pod before switching", m.kind)
	}

	m = step(t, m, tui.GotoKindMsg{Kind: kube.KindConfigMap})

	if m.kind != kube.KindConfigMap {
		t.Fatalf("kind = %s, want ConfigMap after GotoKindMsg", m.kind)
	}
	if m.namespace != "default" {
		t.Fatalf("namespace = %q, want unchanged %q", m.namespace, "default")
	}
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready", m.state)
	}
	row, ok := m.selectedRow()
	if !ok || row.Name != "app-config" {
		t.Fatalf("selectedRow() = %+v, %v, want app-config", row, ok)
	}
}

// TestGotoKindMsgSameKindIsNoop mirrors browse's own guard: re-selecting the
// kind already showing shouldn't blow away the loaded rows/selection.
func TestGotoKindMsgSameKindIsNoop(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1"), pod("default", "api-2")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	rowsBefore := len(m.rows)

	m = step(t, m, tui.GotoKindMsg{Kind: kube.KindPod})

	if m.state != tui.TaskStateReady || len(m.rows) != rowsBefore {
		t.Fatalf("re-selecting the active kind should be a no-op, got state=%s rows=%d", m.state, len(m.rows))
	}
}

// TestRevisitingKindShowsCachedRowsDimmedInsteadOfSkeleton pins 15a (docs/
// design README.md:232: "Revisiting a kind seen this session: cached rows
// dimmed instead of skeletons"): switching back to a kind already loaded
// once this session must render that kind's last-known rows (muted, the
// same treatment 4a's offline table gets) immediately on the switch, before
// the fresh reload's reply lands — not the skeleton-rows loading state a
// never-before-seen kind gets.
func TestRevisitingKindShowsCachedRowsDimmedInsteadOfSkeleton(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod:       {pod("default", "api-1")},
		kube.KindConfigMap: {configMap("default", "app-config")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()()) // Pods loaded once — now cached

	m = step(t, m, tui.GotoKindMsg{Kind: kube.KindConfigMap}) // fully switch+load ConfigMaps too

	// Switch back to Pods, but inspect state right after Update — before the
	// fresh load() reply (queued as a Cmd, not yet run) lands.
	updated, _ := m.Update(tui.GotoKindMsg{Kind: kube.KindPod})
	m = *updated.(*Model)

	if m.state != tui.TaskStateLoading {
		t.Fatalf("state = %s, want loading (a real reload is in flight)", m.state)
	}
	if !m.cachedView {
		t.Fatal("expected cachedView=true for a kind already loaded this session")
	}
	if len(m.rows) == 0 {
		t.Fatal("expected cached rows to be seeded immediately, not left empty")
	}
	view := plain(m.Render())
	if !strings.Contains(view, "api-1") {
		t.Fatalf("expected the cached row's name rendered immediately:\n%s", view)
	}
}

// TestFilterOpensOverCachedRowsBeforeReloadLands: 15a's dimmed cached rows
// are on screen, so '/' must open the filter over them rather than be
// dropped — otherwise the query typed next runs as shortcuts ('a' flipped
// the list to all namespaces), and the filter has to survive the fresh
// reload landing.
func TestFilterOpensOverCachedRowsBeforeReloadLands(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod:       {pod("default", "api-1"), pod("default", "web-1")},
		kube.KindConfigMap: {configMap("default", "app-config")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m = step(t, m, tui.GotoKindMsg{Kind: kube.KindConfigMap})

	updated, load := m.Update(tui.GotoKindMsg{Kind: kube.KindPod})
	m = *updated.(*Model)
	if !m.cachedView || m.state != tui.TaskStateLoading {
		t.Fatalf("precondition: want the cached loading view, got state=%s cachedView=%v", m.state, m.cachedView)
	}
	namespace := m.namespace

	for _, k := range []tea.KeyPressMsg{{Code: '/', Text: "/"}, {Code: 'a', Text: "a"}, {Code: 'p', Text: "p"}, {Code: 'i', Text: "i"}} {
		updated, _ = m.Update(k)
		m = *updated.(*Model)
	}
	if m.namespace != namespace {
		t.Fatalf("namespace = %q, want %q — the typed query ran as shortcuts", m.namespace, namespace)
	}
	if !m.filterActive || m.filterInput.Value() != "api" {
		t.Fatalf("filterActive=%v query=%q, want the filter open with %q", m.filterActive, m.filterInput.Value(), "api")
	}

	m = step(t, m, load())
	if m.state != tui.TaskStateReady || !m.filterActive || m.filterInput.Value() != "api" {
		t.Fatalf("after reload: state=%s filterActive=%v query=%q, want ready with the filter kept", m.state, m.filterActive, m.filterInput.Value())
	}
	if view := plain(m.Render()); strings.Contains(view, "web-1") {
		t.Fatalf("filter not applied after reload:\n%s", view)
	}
}

// TestFreshKindNeverSeenBeforeStillShowsSkeletonNotCache pins the converse
// of the above: a kind that's never been loaded this session (no rowCache
// hit) still gets 15a's plain skeleton-rows loading state.
func TestFreshKindNeverSeenBeforeStillShowsSkeletonNotCache(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod:       {pod("default", "api-1")},
		kube.KindConfigMap: {configMap("default", "app-config")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()()) // Pods loaded — ConfigMaps never visited

	updated, _ := m.Update(tui.GotoKindMsg{Kind: kube.KindConfigMap})
	m = *updated.(*Model)

	if m.cachedView {
		t.Fatal("expected cachedView=false for a kind never loaded this session")
	}
	if len(m.rows) != 0 {
		t.Fatalf("expected no seeded rows for a never-visited kind, got %d", len(m.rows))
	}
}

// TestGotoKindMsgWithFilterAppliesQuery pins 23b's routetable→browse jump
// (docs/design README.md:292: "↵ on a listener filters to attached
// routes"): a GotoKindMsg with a non-empty Filter must switch kind and
// apply that filter query — switchKind's own resetAndLoad would otherwise
// clear it, which is right for a bare kind switch but wrong for this one.
func TestGotoKindMsgWithFilterAppliesQuery(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod:       {pod("default", "api-1")},
		kube.KindConfigMap: {configMap("default", "app-config"), configMap("default", "other-config")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tui.GotoKindMsg{Kind: kube.KindConfigMap, Filter: "app-"})

	if m.kind != kube.KindConfigMap {
		t.Fatalf("kind = %s, want ConfigMap", m.kind)
	}
	if !m.filterActive || m.filterInput.Value() != "app-" {
		t.Fatalf("filterActive=%v filterQuery=%q, want true/\"app-\"", m.filterActive, m.filterInput.Value())
	}
	view := plain(m.Render())
	if !strings.Contains(view, "app-config") {
		t.Fatalf("expected the filtered-in row in view:\n%s", view)
	}
	if strings.Contains(view, "other-config") {
		t.Fatalf("expected the filtered-out row absent from view:\n%s", view)
	}
}

// TestSwitchNamespaceMsgChangesNamespaceKeepsKind covers the jump palette's
// namespace-result Enter.
func TestSwitchNamespaceMsgChangesNamespaceKeepsKind(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			pod("default", "api-1"),
			pod("nva-stage", "worker-1"),
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tui.SwitchNamespaceMsg{Namespace: "nva-stage"})

	if m.namespace != "nva-stage" {
		t.Fatalf("namespace = %q, want nva-stage", m.namespace)
	}
	if m.kind != kube.KindPod {
		t.Fatalf("kind = %s, want unchanged Pod", m.kind)
	}
	row, ok := m.selectedRow()
	if !ok || row.Name != "worker-1" {
		t.Fatalf("selectedRow() = %+v, %v, want worker-1", row, ok)
	}
}

// TestSwitchNamespaceMsgPreservesFilter pins the fix for "filter doesn't
// survive a namespace switch" (mvp-tasks.md's known gap, docs/design
// README.md §6a: "switching keeps kind + filter"): resetAndLoad normally
// clears the filter along with everything else per-view, but switchNamespace
// snapshots it first and restores it after, so an active "/" query keeps
// narrowing the list in the new namespace too.
func TestSwitchNamespaceMsgPreservesFilter(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			pod("default", "api-1"),
			pod("default", "worker-1"),
			pod("nva-stage", "api-2"),
			pod("nva-stage", "worker-2"),
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m = step(t, m, tea.KeyPressMsg{Code: 'a', Text: "api"})
	if !m.filterActive || m.filterInput.Value() != "api" {
		t.Fatalf("filterActive=%v filterQuery=%q, want active with 'api'", m.filterActive, m.filterInput.Value())
	}

	m = step(t, m, tui.SwitchNamespaceMsg{Namespace: "nva-stage"})

	if m.namespace != "nva-stage" {
		t.Fatalf("namespace = %q, want nva-stage", m.namespace)
	}
	if !m.filterActive || m.filterInput.Value() != "api" {
		t.Fatalf("filter not preserved across namespace switch: filterActive=%v filterQuery=%q, want active with 'api'", m.filterActive, m.filterInput.Value())
	}
	if len(m.visible) != 1 || m.visible[0].row.Name != "api-2" {
		t.Fatalf("visible = %+v, want just api-2 (nva-stage's row matching 'api')", m.visible)
	}
}

// TestSwitchContextMsgRestoresMsgFilter pins the other half of "filter
// doesn't survive a switch" — 7a's context palette (docs/design README.md:
// "each context remembers its own namespace + kind + filter; switching
// restores them"). Unlike a namespace switch, a context switch doesn't
// preserve the outgoing filter verbatim: it applies whatever filter
// context.go's switchContextCmd resolved for the *target* context from
// state.PerContext and put on SwitchContextMsg.Filter — empty here, so an
// active filter from the old context must NOT leak into the new one.
func TestSwitchContextMsgRestoresMsgFilter(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m = step(t, m, tea.KeyPressMsg{Code: 'a', Text: "api"})
	if !m.filterActive || m.filterInput.Value() != "api" {
		t.Fatalf("filterActive=%v filterQuery=%q, want active with 'api'", m.filterActive, m.filterInput.Value())
	}

	m = step(t, m, tui.SwitchContextMsg{Context: "other-cluster", Namespace: "default", Kind: kube.KindPod})

	if m.filterActive || m.filterInput.Value() != "" {
		t.Fatalf("filterActive=%v filterQuery=%q, want cleared (target context has no remembered filter)", m.filterActive, m.filterInput.Value())
	}
}

// TestSwitchContextMsgAppliesRestoredFilter is the positive case: a target
// context with a non-empty remembered filter (SwitchContextMsg.Filter,
// resolved from state.PerContext by switchContextCmd) comes back active and
// applied against the newly loaded rows.
func TestSwitchContextMsgAppliesRestoredFilter(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1"), pod("default", "worker-1")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tui.SwitchContextMsg{Context: "other-cluster", Namespace: "default", Kind: kube.KindPod, Filter: "api"})

	if !m.filterActive || !m.filterListFocused || m.filterInput.Value() != "api" {
		t.Fatalf("filterActive=%v filterListFocused=%v filterQuery=%q, want a committed restored 'api' filter", m.filterActive, m.filterListFocused, m.filterInput.Value())
	}
	if len(m.visible) != 1 || m.visible[0].row.Name != "api-1" {
		t.Fatalf("visible = %+v, want just api-1", m.visible)
	}
}

// TestGotoResourceMsgSameKindSelectsWithoutReload covers jumping straight to
// a resource of the kind already showing — no reload needed, just a
// selection change.
func TestGotoResourceMsgSameKindSelectsWithoutReload(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			pod("default", "api-1"),
			pod("default", "api-2"),
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, cmd := m.Update(tui.GotoResourceMsg{Kind: kube.KindPod, Namespace: "default", Name: "api-2"})
	m = *updated.(*Model)
	if cmd != nil {
		t.Fatalf("expected no reload command for a same-kind resource jump")
	}
	row, ok := m.selectedRow()
	if !ok || row.Name != "api-2" {
		t.Fatalf("selectedRow() = %+v, %v, want api-2", row, ok)
	}
}

// TestGotoResourceMsgDifferentKindSwitchesThenSelects covers jumping to a
// resource of a kind other than the one showing: switch kind, then select
// the target once its rows land.
func TestGotoResourceMsgDifferentKindSwitchesThenSelects(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1")},
		kube.KindConfigMap: {
			configMap("default", "app-config"),
			configMap("default", "other-config"),
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tui.GotoResourceMsg{Kind: kube.KindConfigMap, Namespace: "default", Name: "other-config"})

	if m.kind != kube.KindConfigMap {
		t.Fatalf("kind = %s, want ConfigMap", m.kind)
	}
	row, ok := m.selectedRow()
	if !ok || row.Name != "other-config" {
		t.Fatalf("selectedRow() = %+v, %v, want other-config", row, ok)
	}
}

// TestGotoResourceMsgDifferentNamespaceSwitchesThenSelects covers a
// cluster-wide jump (tasks/overview's TROUBLE/RECENT CHANGES rows, 19a) that
// lands on a resource of the kind already showing but in a different
// namespace than the one active — the namespace must switch too, or the
// target simply never appears in the freshly (still old-namespace-scoped)
// loaded rows.
func TestGotoResourceMsgDifferentNamespaceSwitchesThenSelects(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			pod("default", "api-1"),
			pod("other-ns", "worker-0"),
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tui.GotoResourceMsg{Kind: kube.KindPod, Namespace: "other-ns", Name: "worker-0"})

	if m.namespace != "other-ns" {
		t.Fatalf("namespace = %q, want other-ns", m.namespace)
	}
	if m.session.Location.Namespace != "other-ns" {
		t.Fatalf("Session.Location.Namespace = %q, want other-ns", m.session.Location.Namespace)
	}
	row, ok := m.selectedRow()
	if !ok || row.Name != "worker-0" {
		t.Fatalf("selectedRow() = %+v, %v, want worker-0", row, ok)
	}
}

// TestGotoResourceMsgDifferentKindAndNamespaceSwitchesBoth covers the same
// cluster-wide jump when the target is also a different kind (e.g. 19a's
// RECENT CHANGES rows land on a Deployment while browse shows Pods).
func TestGotoResourceMsgDifferentKindAndNamespaceSwitchesBoth(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1")},
		kube.KindConfigMap: {
			configMap("other-ns", "app-config"),
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tui.GotoResourceMsg{Kind: kube.KindConfigMap, Namespace: "other-ns", Name: "app-config"})

	if m.kind != kube.KindConfigMap {
		t.Fatalf("kind = %s, want ConfigMap", m.kind)
	}
	if m.namespace != "other-ns" {
		t.Fatalf("namespace = %q, want other-ns", m.namespace)
	}
	row, ok := m.selectedRow()
	if !ok || row.Name != "app-config" {
		t.Fatalf("selectedRow() = %+v, %v, want app-config", row, ok)
	}
}

// TestGotoResourceMsgClusterScopedKindIgnoresNamespace covers a jump to a
// cluster-scoped kind (Nodes) — msg.Namespace is meaningless there and must
// never be applied, unlike the namespaced-kind cases above.
func TestGotoResourceMsgClusterScopedKindIgnoresNamespace(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod:  {pod("default", "api-1")},
		kube.KindNode: {nodeObj("node-a", true, false)},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tui.GotoResourceMsg{Kind: kube.KindNode, Namespace: "some-namespace-string", Name: "node-a"})

	if m.kind != kube.KindNode {
		t.Fatalf("kind = %s, want Node", m.kind)
	}
	if m.namespace != "default" {
		t.Fatalf("namespace = %q, want unchanged (default) for a cluster-scoped kind jump", m.namespace)
	}
}

// A failed 7a switch that had to roll back rebuilt every cache: browse must
// re-read its list, or the rows sit frozen with nothing left to update them.
// A failure before teardown left the caches alone, so it needs nothing.
func TestFailedSwitchContextReloadsOnlyAfterRollback(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if _, cmd := m.Update(tui.SwitchContextMsg{Err: errors.New("no such context")}); cmd != nil {
		t.Error("a switch that failed before teardown reloaded the list")
	}
	if _, cmd := m.Update(tui.SwitchContextMsg{Err: errors.New("timed out"), RolledBack: true}); cmd == nil {
		t.Error("a rolled-back switch did not reload the list")
	}
}

// TestContextSwitchNeverSeedsPreviousClusterRows: rowCache is per context —
// switching to another cluster must show the skeleton, not the old cluster's
// rows dimmed as "cached" under the new context's header.
func TestContextSwitchNeverSeedsPreviousClusterRows(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1")},
	}}
	session := newSession()
	session.Location.Context = "cluster-a"
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()()) // Pods loaded under cluster-a

	session.Location.Context = "cluster-b" // the root updates Location first
	updated, _ := m.Update(tui.SwitchContextMsg{Context: "cluster-b", Namespace: "default", Kind: kube.KindPod})
	m = *updated.(*Model)
	if m.cachedView || len(m.rows) != 0 {
		t.Fatalf("cachedView=%v rows=%d: cluster-a's rows seeded cluster-b's loading view", m.cachedView, len(m.rows))
	}
}
