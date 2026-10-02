package browse

import (
	"context"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// firstEmptyHints picks the emptyHintsMsg out of a bare Cmd — unlike
// firstRowsLoaded's rowsLoadedMsg, loadEmptyHints is never batched with
// anything else, so there's no fan-out to unwrap.
func firstEmptyHints(t *testing.T, cmd tea.Cmd) emptyHintsMsg {
	t.Helper()
	msg, ok := cmd().(emptyHintsMsg)
	if !ok {
		t.Fatalf("expected emptyHintsMsg, got %T", msg)
	}
	return msg
}

// TestStaleEmptyHintsDoNotOverwriteNewNamespace tests a namespace-switch
// race. Kind alone does not detect a namespace switch that returns to the
// same kind: two empty namespaces of the same kind both leave m.state ==
// TaskStateEmpty and msg.kind unchanged. A slow hints reply for the
// namespace just left can still arrive after the switch's own faster reply
// has already set m.hints for the new namespace. The guard must check the
// epoch and the namespace, not the kind. rowsLoadedMsg uses the same fix —
// see TestStaleNamespaceLoadDoesNotOverwriteNewNamespace.
func TestStaleEmptyHintsDoNotOverwriteNewNamespace(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		// Pods: empty in both "default" and "nva-stage", so both namespace
		// switches land on TaskStateEmpty directly. One configmap lives in
		// "default" only, so its hints are observably different from
		// nva-stage's (otherwise-identical) empty hints.
		kube.KindConfigMap: {configMap("default", "cfg-1")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	if m.namespace != "default" {
		t.Fatalf("namespace = %q, want default", m.namespace)
	}
	if m.state != tui.TaskStateEmpty {
		t.Fatalf("state = %s, want empty", m.state)
	}

	// Capture "default"'s in-flight hints load before it resolves — this is
	// the request that's about to be superseded.
	staleCmd := m.loadEmptyHints()

	// The user switches to nva-stage (also empty for Pods, no configmaps)
	// before that load lands; this bumps reloadEpoch and issues its own
	// fresh hints load, which resolves within this same step since
	// nva-stage has nothing to hint about either.
	m = step(t, m, tui.SwitchNamespaceMsg{Namespace: "nva-stage"})
	if m.namespace != "nva-stage" {
		t.Fatalf("namespace = %q, want nva-stage", m.namespace)
	}
	if m.state != tui.TaskStateEmpty {
		t.Fatalf("state = %s, want empty", m.state)
	}
	if len(m.hints.otherKinds) != 0 {
		t.Fatalf("nva-stage's fresh hints should be empty, got %+v", m.hints)
	}

	// The stale "default" hints reply now resolves late. Without the
	// epoch/namespace guard this overwrites m.hints with default's configmap
	// hint under nva-stage's breadcrumb.
	staleMsg := firstEmptyHints(t, staleCmd)
	if staleMsg.namespace != "default" {
		t.Fatalf("staleMsg.namespace = %q, want default", staleMsg.namespace)
	}
	if len(staleMsg.hints.otherKinds) == 0 {
		t.Fatal("stale message must carry a real hint to make the guard's effect observable")
	}
	updated, cmd := m.Update(staleMsg)
	m = *updated.(*Model)
	if cmd != nil {
		t.Fatal("a stale hints reply must not schedule anything either")
	}

	if len(m.hints.otherKinds) != 0 {
		t.Fatalf("stale default-namespace hints leaked into nva-stage: %+v", m.hints)
	}
}

// scopedRecordingLister wraps fakeLister with tui.ScopedChecker and
// tui.LiveCounter — the shape *kube.Cluster's decorator stack presents under
// --namespace-scoped, where CountLive answers server-side without touching a
// cache — and records every ListRaw call so a test can see whether one
// asked for a kind cluster-wide.
type scopedRecordingLister struct {
	fakeLister
	mu        sync.Mutex
	listCalls []scopedListCall
	liveCount int
}

type scopedListCall struct {
	kind      kube.ResourceKind
	namespace string
}

func (l *scopedRecordingLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	l.mu.Lock()
	l.listCalls = append(l.listCalls, scopedListCall{kind, namespace})
	l.mu.Unlock()
	return l.fakeLister.ListRaw(ctx, kind, namespace)
}

func (l *scopedRecordingLister) Scoped() bool { return true }

func (l *scopedRecordingLister) CountLive(_ context.Context, _ kube.ResourceKind, _ string) (int, error) {
	return l.liveCount, nil
}

func (l *scopedRecordingLister) reset() {
	l.mu.Lock()
	l.listCalls = nil
	l.mu.Unlock()
}

func (l *scopedRecordingLister) globalReads() []kube.ResourceKind {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []kube.ResourceKind
	for _, c := range l.listCalls {
		if c.namespace == "" {
			out = append(out, c.kind)
		}
	}
	return out
}

// TestScopedEmptyHintsSkipGlobalRead tests the empty-state hint under
// --namespace-scoped mode. The "N cluster-wide" hint detail normally calls
// resources.Count(kind, ""). That call goes through ListRaw and starts a new
// cluster-wide informer for kind. Namespace-scoped mode exists to prevent
// exactly this kind of implicit, breadth-first global read. loadEmptyHints
// must answer the same question from CountLive instead. It must never call
// ListRaw with namespace == "" for any kind.
func TestScopedEmptyHintsSkipGlobalRead(t *testing.T) {
	lister := &scopedRecordingLister{
		fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{}},
		liveCount:  5,
	}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	if m.state != tui.TaskStateEmpty {
		t.Fatalf("state = %s, want empty", m.state)
	}
	// Only loadEmptyHints's own reads matter here; the initial load already
	// issued its own (legitimate, cluster-scoped) Node count for the health
	// strip.
	lister.reset()

	msg := firstEmptyHints(t, m.loadEmptyHints())

	if reads := lister.globalReads(); len(reads) != 0 {
		t.Fatalf("loadEmptyHints issued a cluster-wide ListRaw for %v under --namespace-scoped; CountLive should have answered instead", reads)
	}
	if msg.hints.allCount != 5 {
		t.Fatalf("hints.allCount = %d, want 5 from CountLive", msg.hints.allCount)
	}
}

// perNamespaceCacheLister models KindHelmRelease's per-namespace release
// caches (docs/lazy-informers.md §5.5): a read of (kind, namespace) starts
// that exact cache, and KindStarted answers only for caches already read.
// Every other kind is one cluster-wide cache that is already running.
type perNamespaceCacheLister struct {
	fakeLister
	mu      sync.Mutex
	started map[scopedListCall]bool
	reads   []scopedListCall
}

func (l *perNamespaceCacheLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	l.mu.Lock()
	l.reads = append(l.reads, scopedListCall{kind, namespace})
	l.started[scopedListCall{kind, namespace}] = true
	l.mu.Unlock()
	return l.fakeLister.ListRaw(ctx, kind, namespace)
}

func (l *perNamespaceCacheLister) KindStarted(kind kube.ResourceKind, namespace string) bool {
	if kind != kube.KindHelmRelease {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.started[scopedListCall{kind, namespace}]
}

// TestEmptyHelmHintsStartNoReleaseInformers is review 03 H4: an empty Helm
// Releases list must not compute its 10c hints by counting releases in every
// namespace — each of those reads starts a per-namespace release-Secret
// informer, and the cluster-wide total starts the 8.19 MB cluster-wide one.
// Only release caches this session already paid for may be read.
func TestEmptyHelmHintsStartNoReleaseInformers(t *testing.T) {
	lister := &perNamespaceCacheLister{
		fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
			kube.KindNamespace: {namespace("default"), namespace("team-a"), namespace("team-b")},
			kube.KindHelmRelease: {
				helmRelease("team-a", "redis", "redis", "18.1.5", "7.2.4", "deployed", 1),
				helmRelease("team-b", "pg", "postgresql", "15.0.0", "16.1", "deployed", 1),
				helmRelease("team-b", "kafka", "kafka", "26.0.0", "3.6", "deployed", 1),
			},
		}},
		// default is the list on screen; team-a was visited earlier.
		started: map[scopedListCall]bool{
			{kube.KindHelmRelease, "default"}: true,
			{kube.KindHelmRelease, "team-a"}:  true,
		},
	}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m.kind = kube.KindHelmRelease
	m.desc, _ = m.session.Registry.Descriptor(kube.KindHelmRelease)

	msg := firstEmptyHints(t, m.loadEmptyHints())

	for _, r := range lister.reads {
		if r.kind == kube.KindHelmRelease && r.namespace != "default" && r.namespace != "team-a" {
			t.Errorf("empty-state hints read %s in %q, starting a release informer nothing on screen needs", r.kind, r.namespace)
		}
	}
	// The hint is still computed from what's already cached.
	if msg.hints.altNamespace != "team-a" || msg.hints.altCount != 1 {
		t.Errorf("alt namespace = %q (%d), want team-a (1) from the already-running cache", msg.hints.altNamespace, msg.hints.altCount)
	}
	if msg.hints.allCount != 0 {
		t.Errorf("allCount = %d, want 0: no free cluster-wide release count exists", msg.hints.allCount)
	}
}
