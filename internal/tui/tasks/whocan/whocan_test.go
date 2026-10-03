package whocan

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/kube/fake"
	"github.com/kute-dev/kute/internal/tui"
)

func plain(s string) string { return ansi.Strip(s) }

type fakeRBAC struct {
	result kube.WhoCanResult
	err    error
	calls  []kube.WhoCanQuery
}

func (f *fakeRBAC) WhoCan(_ context.Context, query kube.WhoCanQuery) (kube.WhoCanResult, error) {
	f.calls = append(f.calls, query)
	return f.result, f.err
}

func newSession() *tui.Session {
	return &tui.Session{Theme: tui.Dark(), Location: tui.Location{Context: "test-cluster"}}
}

// forbiddenRBAC extends fakeRBAC with the two health-check seams whocan's
// applyLoaded reads (tui.KindSyncChecker/KindForbiddenReporter) — mirrors
// overview_test.go's forbiddenKindLister, generalized with an independent
// unsynced-until-flipped kind so both the stall and denial paths can be
// exercised from the same fake.
type forbiddenRBAC struct {
	fakeRBAC
	unsynced kube.ResourceKind
	synced   bool
	denied   kube.ResourceKind
	err      error
}

func (f *forbiddenRBAC) KindSynced(kind kube.ResourceKind, _ string) bool {
	if kind == f.unsynced && !f.synced {
		return false
	}
	return true
}

func (f *forbiddenRBAC) KindForbidden(kind kube.ResourceKind, _ string) error {
	if kind == f.denied {
		return f.err
	}
	return nil
}

// step mirrors events_test.go's own helper: applies one message, draining
// any resulting tea.BatchMsg fan-out, so Init()'s tea.Batch(load(),
// spinner.Tick) resolves synchronously in tests.
func step(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c != nil {
				m = step(t, m, c())
			}
		}
		return m
	}
	updated, cmd := m.Update(msg)
	next := *updated.(*Model)
	if cmd != nil {
		return step(t, next, cmd())
	}
	return next
}

func TestLoadResolvesSubjectsAndPinsGrantedCurrentUser(t *testing.T) {
	rbac := &fakeRBAC{result: kube.WhoCanResult{
		Subjects: []kube.WhoCanSubject{
			{Name: "dev-readonly", Kind: "User", Via: "clusterrole/view ← clusterrolebinding/viewers", ClusterScope: true},
			{Name: "bob", Kind: "User", Via: "role/secret-reader ← rolebinding/secret-readers"},
		},
		CurrentUser:        "dev-readonly",
		CurrentUserGranted: true,
	}}
	m := New(Config{Session: newSession(), RBAC: rbac, Verb: "list", Resource: "secrets", Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready", m.state)
	}
	if len(rbac.calls) != 1 || rbac.calls[0].Verb != "list" || rbac.calls[0].Resource != "secrets" || rbac.calls[0].Namespace != "default" {
		t.Fatalf("unexpected WhoCan call(s): %+v", rbac.calls)
	}
	// dev-readonly's own subject row is promoted to the pinned first row and
	// dropped from the plain list below it, so it appears exactly once.
	if len(m.rows) != 2 {
		t.Fatalf("rows = %d, want 2 (pinned dev-readonly + bob)", len(m.rows))
	}
	if !m.rows[0].pinned || !m.rows[0].granted || m.rows[0].subject.Name != "dev-readonly" {
		t.Fatalf("rows[0] = %+v, want the pinned granted dev-readonly row", m.rows[0])
	}
	if m.rows[1].pinned || m.rows[1].subject.Name != "bob" {
		t.Fatalf("rows[1] = %+v, want bob", m.rows[1])
	}

	view := plain(m.Render())
	if !strings.Contains(view, "who can") || !strings.Contains(view, "list") || !strings.Contains(view, "secrets") {
		t.Fatalf("expected the question strip in view:\n%s", view)
	}
	if !strings.Contains(view, "dev-readonly (you)") {
		t.Fatalf("expected the pinned (you) row in view:\n%s", view)
	}
	if !strings.Contains(view, "kubectl auth can-i list secrets") {
		t.Fatalf("expected the same-as strip in view:\n%s", view)
	}
}

func TestDeniedCurrentUserGetsRedPinnedRowWithClosestMiss(t *testing.T) {
	rbac := &fakeRBAC{result: kube.WhoCanResult{
		Subjects:           []kube.WhoCanSubject{{Name: "bob", Kind: "User", Via: "role/secret-reader ← rolebinding/secret-readers"}},
		CurrentUser:        "dev-readonly",
		CurrentUserGranted: false,
		CurrentUserVia:     "clusterrole/view grants get, list on pods — not secrets",
	}}
	m := New(Config{Session: newSession(), RBAC: rbac, Verb: "list", Resource: "secrets", Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) != 2 || !m.rows[0].pinned || m.rows[0].granted {
		t.Fatalf("expected a denied pinned row first, got %+v", m.rows)
	}
	view := plain(m.Render())
	if !strings.Contains(view, "clusterrole/view grants get, list on pods") {
		t.Fatalf("expected the closest-miss VIA text in view:\n%s", view)
	}
}

func TestServiceAccountSubjectRendersItsNamespace(t *testing.T) {
	rbac := &fakeRBAC{result: kube.WhoCanResult{
		Subjects: []kube.WhoCanSubject{
			{Name: "default", Kind: "ServiceAccount", Namespace: "team-a", Via: "clusterrole/reader ← clusterrolebinding/a", ClusterScope: true},
			{Name: "default", Kind: "ServiceAccount", Namespace: "team-b", Via: "clusterrole/reader ← clusterrolebinding/b", ClusterScope: true},
		},
	}}
	m := New(Config{Session: newSession(), RBAC: rbac, Verb: "get", Resource: "secrets", Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	view := plain(m.Render())
	for _, want := range []string{"team-a/default", "team-b/default"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected %q in view:\n%s", want, view)
		}
	}
}

func TestGroupQualifiedResourceQueriesItsAPIGroup(t *testing.T) {
	rbac := &fakeRBAC{result: kube.WhoCanResult{
		Subjects: []kube.WhoCanSubject{{Name: "deployer", Kind: "User", Via: "clusterrole/deployer ← clusterrolebinding/deployers", ClusterScope: true}},
	}}
	m := New(Config{Session: newSession(), RBAC: rbac, Verb: "list", Resource: "deployments.apps", Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(rbac.calls) != 1 || rbac.calls[0].Resource != "deployments" || rbac.calls[0].Group != "apps" {
		t.Fatalf("WhoCan call(s) = %+v, want resource deployments in group apps", rbac.calls)
	}
	if view := plain(m.Render()); !strings.Contains(view, "kubectl auth can-i list deployments.apps") {
		t.Fatalf("expected the group-qualified can-i line in view:\n%s", view)
	}
}

func TestAllNamespacesQuestionSaysOnlyClusterWideGrantsCount(t *testing.T) {
	rbac := &fakeRBAC{result: kube.WhoCanResult{
		Subjects: []kube.WhoCanSubject{{Name: "admin", Kind: "User", Via: "clusterrole/admin ← clusterrolebinding/admins", ClusterScope: true}},
	}}
	m := New(Config{Session: newSession(), RBAC: rbac, Verb: "list", Resource: "secrets", Namespace: ""})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if view := plain(m.Render()); !strings.Contains(view, "cluster-wide grants only") {
		t.Fatalf("an all-namespaces query skips RoleBindings and must say so:\n%s", view)
	}
}

func TestUnknownIdentityPinsANeutralRowNotADenial(t *testing.T) {
	rbac := &fakeRBAC{result: kube.WhoCanResult{
		CurrentUser:        "gke_proj_zone_c",
		CurrentUserUnknown: true,
		CurrentUserVia:     "identity unknown — gke_proj_zone_c is the kubeconfig user entry, not the authenticated user",
	}}
	m := New(Config{Session: newSession(), RBAC: rbac, Verb: "list", Resource: "secrets", Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) != 1 || !m.rows[0].unknown {
		t.Fatalf("rows = %+v, want one unknown pinned row", m.rows)
	}
	view := plain(m.Render())
	if strings.Contains(view, tui.GlyphFailed) {
		t.Fatalf("an unknown identity must not render a ✕ denial:\n%s", view)
	}
	if !strings.Contains(view, "identity unknown") {
		t.Fatalf("expected the identity-unknown VIA in view:\n%s", view)
	}
}

func TestEmptyResultEntersEmptyState(t *testing.T) {
	rbac := &fakeRBAC{result: kube.WhoCanResult{}}
	m := New(Config{Session: newSession(), RBAC: rbac, Verb: "delete", Resource: "namespaces", Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateEmpty {
		t.Fatalf("state = %s, want empty", m.state)
	}
	view := plain(m.Render())
	if !strings.Contains(view, "no subject can delete namespaces") {
		t.Fatalf("expected the empty explainer in view:\n%s", view)
	}
}

func TestSetVerbAndResourceMsgsReload(t *testing.T) {
	rbac := &fakeRBAC{result: kube.WhoCanResult{Subjects: []kube.WhoCanSubject{{Name: "alice", Kind: "User"}}}}
	m := New(Config{Session: newSession(), RBAC: rbac, Verb: "list", Resource: "pods", Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tui.SetWhoCanVerbMsg{Verb: "delete"})
	m = step(t, m, tui.SetWhoCanResourceMsg{Resource: "secrets"})
	m = step(t, m, tui.SwitchNamespaceMsg{Namespace: "staging"})

	if v, r := m.WhoCanQuery(); v != "delete" || r != "secrets" {
		t.Fatalf("WhoCanQuery() = (%q, %q), want (delete, secrets)", v, r)
	}
	if m.namespace != "staging" {
		t.Fatalf("namespace = %q, want staging", m.namespace)
	}
	last := rbac.calls[len(rbac.calls)-1]
	if last.Verb != "delete" || last.Resource != "secrets" || last.Namespace != "staging" {
		t.Fatalf("last WhoCan call = %+v, want delete/secrets/staging", last)
	}
}

func TestEnterOpensSelectedBindingYAML(t *testing.T) {
	rbac := &fakeRBAC{result: kube.WhoCanResult{
		Subjects: []kube.WhoCanSubject{
			{Name: "bob", Kind: "User", BindingKind: kube.KindRoleBinding, BindingNamespace: "default", BindingName: "secret-readers"},
		},
	}}
	var openedKind kube.ResourceKind
	var openedNS, openedName string
	m := New(Config{
		Session: newSession(), RBAC: rbac, Verb: "list", Resource: "secrets", Namespace: "default",
		OpenYAML: func(kind kube.ResourceKind, ns, name string, w, h int) (tea.Model, tea.Cmd) {
			openedKind, openedNS, openedName = kind, ns, name
			return &fakeTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "enter"})
	if _, ok := updated.(*fakeTask); !ok {
		t.Fatalf("expected Enter to push the YAML task, got %T", updated)
	}
	if openedKind != kube.KindRoleBinding || openedNS != "default" || openedName != "secret-readers" {
		t.Fatalf("openYAML called with (%s, %s, %s), want (RoleBinding, default, secret-readers)", openedKind, openedNS, openedName)
	}
}

// TestRendersInBothThemes is the CLAUDE.md "both themes always" smoke check
// every screen owes, independent of golden_test.go's own dark/light
// truecolor fixtures — just a render that doesn't panic and produces real
// content under both palettes, exercised against a different fixture shape
// than the golden suite's.
func TestRendersInBothThemes(t *testing.T) {
	rbac := &fakeRBAC{result: kube.WhoCanResult{
		Subjects:           []kube.WhoCanSubject{{Name: "bob", Kind: "User", Via: "role/secret-reader ← rolebinding/secret-readers"}},
		CurrentUser:        "dev-readonly",
		CurrentUserGranted: false,
		CurrentUserVia:     "clusterrole/view grants get, list on pods — not secrets",
	}}
	for _, theme := range []tui.Theme{tui.Dark(), tui.Light()} {
		sess := newSession()
		sess.Theme = theme
		m := New(Config{Session: sess, RBAC: rbac, Verb: "list", Resource: "secrets", Namespace: "default"})
		m.SetSize(120, 36)
		m = step(t, m, m.Init()())
		view := plain(m.Render())
		if !strings.Contains(view, "WHO CAN") || !strings.Contains(view, "bob") {
			t.Fatalf("theme render missing expected content:\n%s", view)
		}
	}
}

// TestMoveSelectionScrollsOffset guards against the table's Offset staying
// pinned at 0 while Selected scrolls off the bottom of the rendered
// viewport — a real gap this screen had (unlike nodedetail/routetable,
// which always tracked m.offset alongside m.selected).
func TestMoveSelectionScrollsOffset(t *testing.T) {
	subjects := make([]kube.WhoCanSubject, 40)
	for i := range subjects {
		subjects[i] = kube.WhoCanSubject{Name: fmt.Sprintf("user-%02d", i), Kind: "User"}
	}
	rbac := &fakeRBAC{result: kube.WhoCanResult{Subjects: subjects}}
	m := New(Config{Session: newSession(), RBAC: rbac, Verb: "list", Resource: "pods", Namespace: "default"})
	m.SetSize(120, 24)
	m = step(t, m, m.Init()())

	if m.offset != 0 {
		t.Fatalf("initial offset = %d, want 0", m.offset)
	}
	rows := m.tableDataRows()
	if rows >= len(m.rows) {
		t.Fatalf("tableDataRows() = %d, want fewer than %d rows so scrolling is exercised", rows, len(m.rows))
	}

	for i := 0; i < len(m.rows)-1; i++ {
		m.moveSelection(1)
	}
	if m.selected != len(m.rows)-1 {
		t.Fatalf("selected = %d, want %d", m.selected, len(m.rows)-1)
	}
	if m.offset == 0 {
		t.Fatalf("offset stayed 0 after scrolling selection to the last row (selected=%d, rows=%d) — the viewport never followed the cursor", m.selected, len(m.rows))
	}
	if m.selected < m.offset || m.selected >= m.offset+rows {
		t.Fatalf("selected %d not within rendered viewport [%d, %d)", m.selected, m.offset, m.offset+rows)
	}
}

// TestRoleBindingDeniedIsVisibleEvenWithClusterRoleRowsPresent pins §5 of
// docs/lazy-informers.md §5.6: who-can's RBAC health checks
// used to run only when the merged result was empty, so a denied RoleBinding
// cache was invisible whenever the ClusterRole/ClusterRoleBinding pair still
// resolved a non-empty answer — exactly backwards for a security tool. The
// checks must run unconditionally and win even over rows already resolved.
func TestRoleBindingDeniedIsVisibleEvenWithClusterRoleRowsPresent(t *testing.T) {
	rbac := &forbiddenRBAC{
		fakeRBAC: fakeRBAC{result: kube.WhoCanResult{
			Subjects: []kube.WhoCanSubject{
				{Name: "dev-readonly", Kind: "User", Via: "clusterrole/view ← clusterrolebinding/viewers", ClusterScope: true},
			},
		}},
		denied: kube.KindRoleBinding,
		err:    fmt.Errorf("rolebindings is forbidden: user cannot list"),
	}
	m := New(Config{Session: newSession(), RBAC: rbac, Verb: "list", Resource: "secrets", Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStatePermissionDenied {
		t.Fatalf("state = %s, want permission-denied — a denied RoleBinding cache must be visible even though the ClusterRole pair already resolved rows", m.state)
	}
}

// TestUnsyncedRBACCacheStaysLoadingOnFirstOpen pins the cold-open half of §5:
// all four RBAC informers start lazily on first read, so a cold session's
// first answer is empty until they fill — that must render as loading, never
// as a confident (and wrong) "no one can do this".
func TestUnsyncedRBACCacheStaysLoadingOnFirstOpen(t *testing.T) {
	rbac := &forbiddenRBAC{
		fakeRBAC: fakeRBAC{result: kube.WhoCanResult{Subjects: []kube.WhoCanSubject{{Name: "bob", Kind: "User"}}}},
		unsynced: kube.KindClusterRole,
	}
	m := New(Config{Session: newSession(), RBAC: rbac, Verb: "list", Resource: "pods", Namespace: "default"})
	m.SetSize(120, 36)
	// Deliberately not step(): applyLoaded's own retry cmd is a real tea.Tick
	// (tui.ScheduleCacheSyncRetry), and draining it would really sleep 250ms
	// per bounce until rbac.synced flips — the same hazard every other
	// synchronous-cmd-draining test in this repo avoids around a recurring
	// tick.
	updated, _ := m.Update(m.load()())
	m = *updated.(*Model)

	if m.state != tui.TaskStateLoading {
		t.Fatalf("state = %s, want loading while the ClusterRole cache reports unsynced", m.state)
	}
}

// TestStalledCacheOnBackgroundReloadKeepsExistingRowsVisible pins the
// stall-vs-rerender rule: once rows are already on screen, a background
// reload (e.g. a watch event) finding a required cache merely *stalled* —
// not yet re-synced, no error yet — must not discard the rendered result
// back into a loading spinner. Only a confirmed denial may do that.
func TestStalledCacheOnBackgroundReloadKeepsExistingRowsVisible(t *testing.T) {
	rbac := &forbiddenRBAC{
		fakeRBAC: fakeRBAC{result: kube.WhoCanResult{Subjects: []kube.WhoCanSubject{{Name: "bob", Kind: "User"}}}},
	}
	m := New(Config{Session: newSession(), RBAC: rbac, Verb: "list", Resource: "pods", Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready after the first load", m.state)
	}
	if len(m.rows) == 0 {
		t.Fatal("expected rows after the first load")
	}

	rbac.unsynced = kube.KindRole
	// Deliberately not step(): the reload finds KindRole still unsynced (it
	// never flips back), so applyLoaded keeps returning a real
	// tui.ScheduleCacheSyncRetry tick — draining that recursively would sleep
	// 250ms per bounce forever, the same hazard called out above
	// TestUnsyncedRBACCacheStaysLoadingOnFirstOpen. So run the reload's own
	// load() once by hand and apply just the resulting loadedMsg, without
	// touching the retry cmd applyLoaded schedules on top of it.
	_, reloadCmd := m.Update(kube.ResourceChangedMsg{Kind: kube.KindRole})
	updated, _ := m.Update(reloadCmd())
	m = *updated.(*Model)

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready — an already-rendered result must not be discarded by a background stall", m.state)
	}
	if len(m.rows) == 0 {
		t.Fatal("rows emptied by a background stall — the stall-vs-rerender regression")
	}
}

// fakeTask is a minimal tea.Model stand-in for asserting Update pushed a
// new task (mirrors poddetail/routetable's own such-and-such push tests).
type fakeTask struct{}

func (f *fakeTask) Init() tea.Cmd                       { return nil }
func (f *fakeTask) Update(tea.Msg) (tea.Model, tea.Cmd) { return f, nil }
func (f *fakeTask) View() tea.View                      { return tea.NewView("") }

// TestCacheSyncRetryTickEventuallyLeavesLoading drives the retry chain the two
// tests above deliberately refuse to touch.
//
// Those two assert the *first* frame — loading, not a confident empty answer —
// and stop there, because draining applyLoaded's retry cmd meant sleeping a
// real CacheSyncRetryInterval per bounce. That left the half that matters
// untested: the tick has to actually fire, reload, and let the screen out of
// loading once the cache fills. tui.ScheduleCacheSyncRetry exists precisely
// because a kind with genuinely zero objects syncs without ever emitting a
// change event, so nothing else would ever wake the screen.
//
// Inside a synctest bubble the clock is fake and advances as soon as every
// goroutine is blocked, so the whole chain drains instantly and exactly.
func TestCacheSyncRetryTickEventuallyLeavesLoading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rbac := &forbiddenRBAC{
			fakeRBAC: fakeRBAC{result: kube.WhoCanResult{Subjects: []kube.WhoCanSubject{{Name: "bob", Kind: "User"}}}},
			unsynced: kube.KindClusterRole,
		}
		m := New(Config{Session: newSession(), RBAC: rbac, Verb: "list", Resource: "pods", Namespace: "default"})
		m.SetSize(120, 36)

		updated, cmd := m.Update(m.load()())
		m = *updated.(*Model)
		if m.state != tui.TaskStateLoading {
			t.Fatalf("state = %s, want loading while the ClusterRole cache reports unsynced", m.state)
		}
		if cmd == nil {
			t.Fatal("no retry scheduled while the cache was unsynced — nothing would ever wake this screen")
		}

		// The cache fills while the retry is in flight, emitting no change
		// event: the exact case the retry is for.
		rbac.synced = true

		for range 5 {
			if cmd == nil {
				break
			}
			updated, cmd = m.Update(cmd())
			m = *updated.(*Model)
		}

		if m.state != tui.TaskStateReady {
			t.Fatalf("state = %s, want ready once the cache synced and the retry fired", m.state)
		}
		if len(m.rows) == 0 {
			t.Fatal("expected rows after the retry resolved")
		}
	})
}

// TestReviewedIdentityIsPinnedAsYou: with a connect-time SelfSubjectReview
// answer, "(you)" is the server's username and its groups count — the
// kubeconfig entry name never appears.
func TestReviewedIdentityIsPinnedAsYou(t *testing.T) {
	cluster := fake.New("default", "test-cluster")
	cluster.SetUserName("gke_proj_zone_c")
	cluster.SetReviewedIdentity("alice@example.com", []string{"devs", "system:authenticated"})
	cluster.Seed(kube.KindClusterRole, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "secret-reader"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"list"}}},
	})
	cluster.Seed(kube.KindClusterRoleBinding, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "devs-read"},
		RoleRef:    rbacv1.RoleRef{Kind: "ClusterRole", Name: "secret-reader"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, Name: "devs"}},
	})
	m := New(Config{Session: newSession(), RBAC: cluster, Verb: "list", Resource: "secrets", Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) == 0 || !m.rows[0].pinned || !m.rows[0].granted || m.rows[0].unknown || m.rows[0].subject.Name != "alice@example.com" {
		t.Fatalf("rows = %+v, want a granted pinned alice@example.com", m.rows)
	}
	view := plain(m.Render())
	if !strings.Contains(view, "alice@example.com (you)") || strings.Contains(view, "gke_proj_zone_c") {
		t.Fatalf("expected the reviewed identity pinned, not the kubeconfig name:\n%s", view)
	}
}
