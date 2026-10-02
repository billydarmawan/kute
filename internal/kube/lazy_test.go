package kube

import (
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// newLazyTestCluster builds a Cluster over fake clients whose recorded
// actions are the assertion surface: which resources got listed, and when.
func newLazyTestCluster(objs ...runtime.Object) (*Cluster, *fake.Clientset) {
	cs := fake.NewSimpleClientset(objs...)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	// The CRD informer Start opens is a dynamic one, and the dynamic fake
	// needs each GVR's list kind spelled out up front.
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		crdGVR: "CustomResourceDefinitionList",
	})
	return &Cluster{
		clientset:  cs,
		factory:    newTypedFactory(cs),
		dynClient:  dyn,
		dynFactory: dynamicinformer.NewDynamicSharedInformerFactory(dyn, 0),
		stopCh:     make(chan struct{}),
		events:     make(chan ResourceChangedMsg, 256),
		health:     newHealth(),
	}, cs
}

// listedResources reports which resource types the clientset was asked to
// list, deduplicated and sorted.
func listedResources(cs *fake.Clientset) []string {
	seen := map[string]bool{}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "list" {
			seen[a.GetResource().Resource] = true
		}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestStartOnlyListsEagerKinds is the regression test for the whole feature:
// connecting must not pull every kind in the cluster down the wire. Before
// this, all 21 typed informers listed before Start returned.
func TestStartOnlyListsEagerKinds(t *testing.T) {
	c, cs := newLazyTestCluster(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}},
	)
	defer c.Stop()

	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := listedResources(cs)
	want := []string{"namespaces", "nodes", "pods"}
	if len(got) != len(want) {
		t.Fatalf("connect listed %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("connect listed %v, want %v", got, want)
		}
	}
}

// TestListRawStartsTheKindOnFirstRead: reading a lazy kind is what starts
// its informer, so no caller has to declare anything.
func TestListRawStartsTheKindOnFirstRead(t *testing.T) {
	c, cs := newLazyTestCluster()
	defer c.Stop()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for _, r := range listedResources(cs) {
		if r == "secrets" {
			t.Fatal("secrets were listed at connect; Secret is supposed to be lazy")
		}
	}

	if _, err := c.ListRaw(t.Context(), KindSecret, "default"); err != nil {
		t.Fatalf("ListRaw(Secret): %v", err)
	}

	waitFor(t, "the Secret informer to list", func() bool {
		for _, r := range listedResources(cs) {
			if r == "secrets" {
				return true
			}
		}
		return false
	})
}

// TestRepeatedReadsStartTheInformerOnce guards ensureKind's idempotence:
// browse re-reads on every change event and every 250ms retry, so a
// non-idempotent start would relist the world continuously.
func TestRepeatedReadsStartTheInformerOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, cs := newLazyTestCluster()
		defer c.Stop()
		if err := c.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}

		ctx := t.Context()
		for i := 0; i < 20; i++ {
			if _, err := c.ListRaw(ctx, KindConfigMap, ""); err != nil {
				t.Fatalf("ListRaw(ConfigMap): %v", err)
			}
		}
		// Exactly, not approximately: Wait returns once every goroutine in the
		// bubble is blocked, so a duplicate informer start has either happened
		// by now or never will. The 50ms sleep this replaces was a guess that a
		// loaded CI box could outrun.
		synctest.Wait()

		if n := countListActions(cs, "configmaps"); n != 1 {
			t.Fatalf("configmaps listed %d times across 20 reads, want exactly 1", n)
		}
	})
}

func countListActions(cs *fake.Clientset, resource string) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == "list" && a.GetResource().Resource == resource {
			n++
		}
	}
	return n
}

// TestLazyReadIsRaceFree: ListRaw is called from the Bubble Tea update loop
// while informers and the health loop run on their own goroutines, so the
// start path has to be safe under concurrent first-reads of the same kind.
func TestLazyReadIsRaceFree(t *testing.T) {
	c, _ := newLazyTestCluster()
	defer c.Stop()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx := t.Context()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, kind := range []ResourceKind{KindSecret, KindConfigMap, KindEvent, KindJob} {
				_, _ = c.ListRaw(ctx, kind, "default")
			}
		}()
	}
	wg.Wait()
}

// TestListRawAfterStopDoesNotStartInformers: Stop nils stopCh, and an
// informer started with a nil channel would never stop — a goroutine leak
// on the way out.
func TestListRawAfterStopDoesNotStartInformers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, cs := newLazyTestCluster()
		if err := c.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		c.Stop()

		before := len(cs.Actions())
		if _, err := c.ListRaw(t.Context(), KindSecret, "default"); err != nil {
			t.Fatalf("ListRaw after Stop should read an empty cache, not error: %v", err)
		}
		// Wait, not a sleep: it returns once every goroutine in the bubble is
		// blocked, which is exactly the "nothing more is going to happen"
		// condition a negative assertion needs.
		synctest.Wait()

		if got := len(cs.Actions()); got != before {
			t.Fatalf("ListRaw after Stop issued %d new API actions, want 0", got-before)
		}
		c.mu.Lock()
		_, registered := c.kindInformers[scopeKey{KindSecret, ""}]
		c.mu.Unlock()
		if registered {
			t.Fatal("ListRaw after Stop registered an informer that can never be stopped")
		}
	})
}

// TestLazyStartReArmsTheConnectGrace pins the health companion fix. A lazy
// start produces the same LIST contention a cold connect does, so a /livez
// timeout during it must stay forgiven — otherwise opening Secrets on a slow
// link paints an OFFLINE banner over a healthy cluster.
func TestLazyStartReArmsTheConnectGrace(t *testing.T) {
	c, _ := newLazyTestCluster()
	defer c.Stop()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Age the window past the grace period, as a long session would.
	c.health.mu.Lock()
	c.health.startedAt = time.Now().Add(-2 * connectGrace)
	c.health.mu.Unlock()

	c.ensureKind(KindSecret, "")

	c.health.mu.Lock()
	age := time.Since(c.health.startedAt)
	c.health.mu.Unlock()
	if age > connectGrace {
		t.Fatalf("grace window still %v old after a lazy start; a mid-session LIST burst is unforgiven", age)
	}
}

// TestStartIgnoresAStrayListerRegistration is the hazard the typedKinds
// table was built to close: a read registers an informer, and the next
// factory.Start would run it. Every registration now goes through
// registerWatches, so the informer that read started is fully wired.
func TestStartIgnoresAStrayListerRegistration(t *testing.T) {
	c, _ := newLazyTestCluster()
	defer c.Stop()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := c.ListRaw(t.Context(), KindReplicaSet, ""); err != nil {
		t.Fatalf("ListRaw(ReplicaSet): %v", err)
	}
	c.mu.Lock()
	inf, ok := c.kindInformers[scopeKey{KindReplicaSet, ""}]
	c.mu.Unlock()
	if !ok || inf == nil {
		t.Fatal("reading a kind must register its informer, not leave it dangling on the factory")
	}
	// A registered informer is a watched one: its handler feeds notify.
	waitFor(t, "the ReplicaSet informer to sync", func() bool { return c.KindSynced(KindReplicaSet, "") })
}

// crdListActions counts full CRD object lists — the 2.76 MB read this work
// exists to remove. Counted off the dynamic client, since that is the only
// path that pulls whole CRDs (the metadata client's name-only list goes
// through a different client entirely).
func crdListActions(dyn *dynamicfake.FakeDynamicClient) int {
	n := 0
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "list" && a.GetResource() == crdGVR {
			n++
		}
	}
	return n
}

// TestStartDoesNotListCRDObjects is the §5.1 regression test. A
// CustomResourceDefinition carries the full OpenAPI schema for every version
// it serves; listing all of them at connect was, after lazy informers
// landed, the single largest thing a connect pulled — and 98% of it was
// schema nothing reads.
func TestStartDoesNotListCRDObjects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := newLazyTestCluster()
		dyn := c.dynClient.(*dynamicfake.FakeDynamicClient)
		defer c.Stop()

		if err := c.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		// Wait, not a sleep: it returns once every goroutine in the bubble is
		// blocked, which is exactly the "nothing more is going to happen"
		// condition a negative assertion needs.
		synctest.Wait()

		if n := crdListActions(dyn); n != 0 {
			t.Fatalf("connect issued %d full CRD list(s); discovery is supposed to read names and the discovery API instead", n)
		}
	})
}

// TestCRDListStartsItsInformerOnDemand: 14b is the one screen that wants
// whole CRD objects, so that informer starts when it opens — not before.
func TestCRDListStartsItsInformerOnDemand(t *testing.T) {
	c, _ := newLazyTestCluster()
	dyn := c.dynClient.(*dynamicfake.FakeDynamicClient)
	defer c.Stop()

	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := c.ListRaw(t.Context(), KindCustomResourceDefinition, ""); err != nil {
		t.Fatalf("ListRaw(CustomResourceDefinition): %v", err)
	}

	waitFor(t, "the CRD informer to list", func() bool { return crdListActions(dyn) > 0 })
}

// TestBreadthFirstReadGatedOnKindStartedStartsNothing is the review 01 H1
// regression test. The goto palette's fuzzy resource corpus and browse's
// empty-state hints walk every kind they know and read only the ones whose
// cache is already running and filled (tui.KindCached: KindStarted, then
// KindSynced). KindSynced alone vouched for KindCustomResourceDefinition
// before 14b had ever been opened — no cache key matched it, so it fell
// through to "unknown kind, settled" — and the first fuzzy keystroke in `g`
// started the full-CRD informer, the 2.76 MB read §5.1 took out of connect.
//
// Walks the same gate over every typed kind plus the CRD list and Helm, at
// both the cluster-wide and a namespaced scope, and asserts on the fake
// clients' recorded actions: nothing beyond the eager set is listed, and no
// CRD object at all.
func TestBreadthFirstReadGatedOnKindStartedStartsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, cs := newLazyTestCluster()
		dyn := c.dynClient.(*dynamicfake.FakeDynamicClient)
		defer c.Stop()

		if err := c.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		synctest.Wait()

		kinds := []ResourceKind{KindCustomResourceDefinition, KindHelmRelease}
		for k := range typedKinds {
			kinds = append(kinds, k)
		}
		for _, ns := range []string{"", "default"} {
			for _, k := range kinds {
				if c.KindStarted(k, ns) && c.KindSynced(k, ns) {
					_, _ = c.ListRaw(t.Context(), k, ns)
				}
			}
		}
		synctest.Wait()

		if n := crdListActions(dyn); n != 0 {
			t.Fatalf("a breadth-first walk gated on KindStarted issued %d full CRD list(s); the CRD informer must start only when 14b is opened", n)
		}
		if got, want := listedResources(cs), []string{"namespaces", "nodes", "pods"}; !slices.Equal(got, want) {
			t.Fatalf("a breadth-first walk gated on KindStarted listed %v, want only the eager set %v", got, want)
		}
	})
}

// TestKindStartedFlipsOnFirstRead: KindStarted is the "is this read free?"
// answer, so it must be false for exactly the kinds a read would start —
// typed, the CRD list, Helm — and true once that read has happened.
func TestKindStartedFlipsOnFirstRead(t *testing.T) {
	c, _ := newLazyTestCluster()
	defer c.Stop()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !c.KindStarted(KindPod, "") {
		t.Fatal("KindStarted(Pod) = false after connect; Pod is in the eager set")
	}
	for _, k := range []ResourceKind{KindReplicaSet, KindCustomResourceDefinition} {
		if c.KindStarted(k, "") {
			t.Fatalf("KindStarted(%s) = true before anything read it", k)
		}
		if _, err := c.ListRaw(t.Context(), k, ""); err != nil {
			t.Fatalf("ListRaw(%s): %v", k, err)
		}
		if !c.KindStarted(k, "") {
			t.Fatalf("KindStarted(%s) = false after a read started its informer", k)
		}
	}
	if c.KindStarted(KindHelmRelease, "default") {
		t.Fatal("KindStarted(HelmRelease) = true before any release cache was read")
	}
	if !c.KindStarted("NoSuchKind", "") {
		t.Fatal("KindStarted(unknown kind) = false; nothing could back it, so reading it is free")
	}
}
