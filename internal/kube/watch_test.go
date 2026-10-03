package kube

import (
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
)

// TestTypedKindsCoversEveryTypedKind pins the table's contents. typedKinds is
// the single registration path for typed informers, so a kind missing from it
// is not a cosmetic gap: ListRaw would fall through to the dynamic branch and
// report "no informer registered", and nothing would ever watch it.
func TestTypedKindsCoversEveryTypedKind(t *testing.T) {
	t.Parallel()
	want := []ResourceKind{
		KindPod, KindService, KindIngress, KindConfigMap, KindSecret,
		KindPersistentVolumeClaim, KindEvent, KindNode, KindNamespace,
		KindDeployment, KindDaemonSet, KindStatefulSet, KindReplicaSet,
		KindControllerRevision, KindJob, KindCronJob,
		KindHorizontalPodAutoscaler, KindRole, KindRoleBinding,
		KindClusterRole, KindClusterRoleBinding,
	}
	if len(typedKinds) != len(want) {
		t.Errorf("typedKinds has %d entries, want %d", len(typedKinds), len(want))
	}
	for _, kind := range want {
		tk, ok := typedKinds[kind]
		if !ok {
			t.Errorf("typedKinds is missing %s", kind)
			continue
		}
		if tk.informer == nil {
			t.Errorf("%s has no informer func", kind)
		}
		if tk.list == nil {
			t.Errorf("%s has no list func", kind)
		}
		if tk.gvr.Resource == "" {
			t.Errorf("%s has no GVR resource — live counts address kinds by GVR, not by lister", kind)
		}
	}
}

// TestTypedKindsClusterScopedFlags: the four kinds with no per-namespace
// lister must be flagged, since their list funcs ignore the namespace
// argument entirely and a caller passing one would otherwise expect filtering.
func TestTypedKindsClusterScopedFlags(t *testing.T) {
	t.Parallel()
	clusterScoped := map[ResourceKind]bool{
		KindNode: true, KindNamespace: true,
		KindClusterRole: true, KindClusterRoleBinding: true,
	}
	for kind, tk := range typedKinds {
		if tk.clusterScoped != clusterScoped[kind] {
			t.Errorf("%s clusterScoped = %v, want %v", kind, tk.clusterScoped, clusterScoped[kind])
		}
	}
}

// TestListRawReadsThroughTheTable exercises every entry's list func against a
// real informer factory, which is what catches a copy-paste slip pairing one
// kind's informer with another's lister.
func TestListRawReadsThroughTheTable(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
	)
	c := &Cluster{
		clientset: cs,
		factory:   informers.NewSharedInformerFactory(cs, 0),
		stopCh:    make(chan struct{}),
		events:    make(chan ResourceChangedMsg, 64),
		health:    newHealth(),
	}
	defer close(c.stopCh)

	c.registerWatches(slices.Collect(maps.Keys(typedKinds))...)
	c.factory.Start(c.stopCh)
	c.factory.WaitForCacheSync(c.stopCh)

	ctx := t.Context()
	for kind := range typedKinds {
		if _, err := c.ListRaw(ctx, kind, ""); err != nil {
			t.Errorf("ListRaw(%s, all namespaces): %v", kind, err)
		}
		if _, err := c.ListRaw(ctx, kind, "default"); err != nil {
			t.Errorf("ListRaw(%s, default): %v", kind, err)
		}
	}

	pods, err := c.ListRaw(ctx, KindPod, "default")
	if err != nil || len(pods) != 1 {
		t.Fatalf("ListRaw(Pod, default) = %d objects, %v; want 1, nil", len(pods), err)
	}
	// A cluster-scoped kind must ignore the namespace rather than filter to
	// nothing with it.
	nodes, err := c.ListRaw(ctx, KindNode, "default")
	if err != nil || len(nodes) != 1 {
		t.Fatalf("ListRaw(Node, \"default\") = %d objects, %v; want 1, nil", len(nodes), err)
	}
}

func TestListRawUnknownKindErrors(t *testing.T) {
	t.Parallel()
	c := &Cluster{}
	if _, err := c.ListRaw(t.Context(), ResourceKind("Nonexistent"), ""); err == nil {
		t.Fatal("expected an error for a kind with no informer, got nil")
	}
}

// TestListRawIsRaceFreeAgainstFactorySwap covers the reason ListRaw snapshots
// c.factory under c.mu: SwitchContext replaces the factory wholesale, and the
// previous unlocked read could tear across two clusters' caches. Run with
// -race; this fails on the pre-table code.
func TestListRawIsRaceFreeAgainstFactorySwap(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}})
	c := &Cluster{
		clientset: cs,
		factory:   informers.NewSharedInformerFactory(cs, 0),
		stopCh:    make(chan struct{}),
		events:    make(chan ResourceChangedMsg, 64),
		health:    newHealth(),
	}
	defer close(c.stopCh)

	ctx := t.Context()
	var wg sync.WaitGroup
	done := make(chan struct{})

	for _, kind := range []ResourceKind{KindPod, KindSecret, KindNode, KindDeployment} {
		wg.Add(1)
		go func(k ResourceKind) {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					_, _ = c.ListRaw(ctx, k, "default")
				}
			}
		}(kind)
	}

	// Swap the factory the way SwitchContext does while those reads run.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			c.mu.Lock()
			c.factory = informers.NewSharedInformerFactory(cs, 0)
			c.mu.Unlock()
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(done)
	wg.Wait()
}
