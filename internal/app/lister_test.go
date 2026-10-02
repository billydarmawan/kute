package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/helmrepo"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/kube/fake"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui/tasks/helmhistory"
)

// recordingCluster stands in for *kube.Cluster at exactly the seams the
// decorator stack sees, recording which cache each read landed in. This is
// the app-layer counterpart of internal/kube's recorded-clientset-actions
// tests (lazy_test.go, helm_secrets_test.go): the question is never "did the
// right rows come back" — both paths decode to the same releases — but "what
// did the app pull down the wire to produce them".
type recordingCluster struct {
	listedKinds   []kube.ResourceKind
	helmReads     int
	syncedAsked   []kube.ResourceKind
	syncedAskedNS []string
	errorAsked    []kube.ResourceKind
	errorAskedNS  []string
	scoped        bool
}

// errStalledCache stands in for the reason a cache stopped filling — on the
// cluster this was diagnosed against, a release LIST that could not complete
// inside the API server's request window.
var errStalledCache = errors.New("stream error when reading response body")

func (c *recordingCluster) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	c.listedKinds = append(c.listedKinds, kind)
	return nil, nil
}

func (c *recordingCluster) ListHelmReleaseSecrets(context.Context, string) ([]runtime.Object, error) {
	c.helmReads++
	return nil, nil
}

func (c *recordingCluster) KindSynced(kind kube.ResourceKind, namespace string) bool {
	c.syncedAsked = append(c.syncedAsked, kind)
	c.syncedAskedNS = append(c.syncedAskedNS, namespace)
	return true
}

func (c *recordingCluster) KindError(kind kube.ResourceKind, namespace string) error {
	c.errorAsked = append(c.errorAsked, kind)
	c.errorAskedNS = append(c.errorAskedNS, namespace)
	return errStalledCache
}

func (c *recordingCluster) Scoped() bool { return c.scoped }

func (c *recordingCluster) listed(kind kube.ResourceKind) bool {
	for _, k := range c.listedKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// releaseCluster hands back one real release Secret, so the decorator's own
// decode-and-annotate path runs end to end. workloads answers the rolling
// caches the rollout annotation joins the release's manifest against.
type releaseCluster struct {
	release   kube.HelmRelease
	workloads map[kube.ResourceKind][]runtime.Object
}

func (c *releaseCluster) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	return c.workloads[kind], nil
}

func (c *releaseCluster) ListHelmReleaseSecrets(context.Context, string) ([]runtime.Object, error) {
	return []runtime.Object{kube.EncodeHelmReleaseSecret(c.release)}, nil
}

// chartCache writes a one-repo Helm cache offering chart at version, and
// returns a Cache over it.
func chartCache(t *testing.T, chart, version string) *helmrepo.Cache {
	t.Helper()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "repositories.yaml")
	if err := os.WriteFile(configPath, []byte("repositories:\n- name: testrepo\n  url: https://example.test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "apiVersion: v1\nentries:\n  " + chart + ":\n  - name: " + chart + "\n    version: " + version + "\n"
	if err := os.WriteFile(filepath.Join(cacheDir, "testrepo-index.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return helmrepo.NewCache(helmrepo.Loader{ConfigPath: configPath, CachePath: cacheDir})
}

// TestListedReleasesCarryTheAvailableChartVersion pins 18a's outdated signal
// to the one place releases are decoded. Annotating here rather than in each
// screen is what makes the LATEST column, the strip's outdated count and the
// 19a overview agree without three copies of the lookup.
func TestListedReleasesCarryTheAvailableChartVersion(t *testing.T) {
	cluster := &releaseCluster{release: kube.HelmRelease{
		Namespace: "default", Name: "certs", Chart: "cert-manager", ChartVersion: "1.14.4",
		Revision: 3, Status: "deployed",
	}}
	lister := newSessionLister(cluster, kube.NewForwardManager(), chartCache(t, "cert-manager", "1.16.2"))

	objs, err := lister.ListRaw(t.Context(), kube.KindHelmRelease, "default")
	if err != nil {
		t.Fatalf("ListRaw(KindHelmRelease): %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("got %d releases, want 1", len(objs))
	}
	release := objs[0].(*kube.HelmReleaseObject).Release
	if release.LatestVersion != "1.16.2" {
		t.Errorf("LatestVersion = %q, want 1.16.2", release.LatestVersion)
	}
	if release.LatestRepo != "testrepo" {
		t.Errorf("LatestRepo = %q, want testrepo", release.LatestRepo)
	}
	if !release.Outdated() {
		t.Error("Outdated() = false for 1.14.4 against an available 1.16.2")
	}
	if status := lister.ChartIndexStatus(); !status.Configured || status.Repos != 1 {
		t.Errorf("ChartIndexStatus() = %+v, want a configured single-repo cache", status)
	}
}

// TestListedReleasesCarryTheirWorkloadRolloutState is 18a's "helm is done,
// Kubernetes isn't" signal, end to end through the decorator that annotates
// it. Helm flips a release back to `deployed` the moment it has applied the
// manifests — without --wait it never watches the rollout at all — so a
// release the user just upgraded reads `deployed` while its pods are still
// being replaced, and rendered green through the whole rollout.
func TestListedReleasesCarryTheirWorkloadRolloutState(t *testing.T) {
	manifest := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: nva-api\n"
	release := kube.HelmRelease{
		Namespace: "nva", Name: "nva", Chart: "nva", ChartVersion: "1.4.2",
		Revision: 7, Status: "deployed", Manifest: manifest,
	}
	deployment := func(updated, ready int32) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "nva", Name: "nva-api", Generation: 4},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr32(3)},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration: 4, Replicas: 3,
				UpdatedReplicas: updated, ReadyReplicas: ready, AvailableReplicas: ready,
			},
		}
	}

	rollingOut := func(t *testing.T, d *appsv1.Deployment) bool {
		t.Helper()
		cluster := &releaseCluster{
			release:   release,
			workloads: map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {d}},
		}
		lister := newSessionLister(cluster, kube.NewForwardManager(), nil)
		objs, err := lister.ListRaw(t.Context(), kube.KindHelmRelease, "nva")
		if err != nil {
			t.Fatalf("ListRaw(KindHelmRelease): %v", err)
		}
		if len(objs) != 1 {
			t.Fatalf("got %d releases, want 1", len(objs))
		}
		return objs[0].(*kube.HelmReleaseObject).Release.RolloutPending
	}

	if !rollingOut(t, deployment(1, 2)) {
		t.Error("RolloutPending = false while the release's own Deployment is mid-rollout — this is the release rendering green through an upgrade")
	}
	if rollingOut(t, deployment(3, 3)) {
		t.Error("RolloutPending = true for a settled Deployment — the rollout arrow would never clear")
	}
}

func ptr32(v int32) *int32 { return &v }

// TestDemoHelmReleaseCarriesItsRolloutState keeps --demo honest about the
// signal: the demo fixtures seed a `deployed` release whose Deployment is
// still rolling, and the two halves live in different files, so this is what
// notices when one of them drifts and the arrow quietly stops appearing.
func TestDemoHelmReleaseCarriesItsRolloutState(t *testing.T) {
	demo := fake.NewDemo()
	lister := newSessionLister(demo, kube.NewForwardManager(), nil)

	objs, err := lister.ListRaw(t.Context(), kube.KindHelmRelease, "")
	if err != nil {
		t.Fatalf("ListRaw(KindHelmRelease): %v", err)
	}
	var pending []string
	for _, obj := range objs {
		if r := obj.(*kube.HelmReleaseObject).Release; r.RolloutPending {
			pending = append(pending, r.Name)
		}
	}
	if len(pending) != 1 || pending[0] != "grafana" {
		t.Errorf("releases mid-rollout in --demo = %v, want just grafana", pending)
	}
}

// TestListedReleasesWithoutAChartIndex covers the common case of no Helm repo
// cache at all: the release still lists, and simply carries no opinion about
// what's available. An unknown must never masquerade as "current".
func TestListedReleasesWithoutAChartIndex(t *testing.T) {
	cluster := &releaseCluster{release: kube.HelmRelease{
		Namespace: "default", Name: "certs", Chart: "cert-manager", ChartVersion: "1.14.4",
		Revision: 3, Status: "deployed",
	}}
	lister := newSessionLister(cluster, kube.NewForwardManager(), nil)

	objs, err := lister.ListRaw(t.Context(), kube.KindHelmRelease, "default")
	if err != nil {
		t.Fatalf("ListRaw(KindHelmRelease): %v", err)
	}
	release := objs[0].(*kube.HelmReleaseObject).Release
	if release.LatestVersion != "" || release.Outdated() {
		t.Errorf("release carries %+v with no chart index, want no opinion", release)
	}
	if lister.ChartIndexStatus().Configured {
		t.Error("ChartIndexStatus().Configured = true with no chart index")
	}
}

// narrowlessCluster has a per-kind sync signal but no filtered release cache
// — the "optional seam absent" shape helmSecretLister documents.
type narrowlessCluster struct {
	listedKinds []kube.ResourceKind
	syncedAsked []kube.ResourceKind
}

func (c *narrowlessCluster) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	c.listedKinds = append(c.listedKinds, kind)
	return nil, nil
}

func (c *narrowlessCluster) KindSynced(kind kube.ResourceKind, _ string) bool {
	c.syncedAsked = append(c.syncedAsked, kind)
	return true
}

// TestListingHelmReleasesReadsTheReleaseCacheNotEverySecret is the
// regression test for the seam that went unforwarded. forwardAwareLister
// embeds the resources.RawLister *interface*, so it promoted only ListRaw;
// helmAwareLister.helmSecrets type-asserted against it, missed, and fell
// through to the shared Secret cache — pulling the largest kind on most
// clusters to find a handful of releases, and leaving the narrow release
// cache cold until the 'h' key started it mid-session.
func TestListingHelmReleasesReadsTheReleaseCacheNotEverySecret(t *testing.T) {
	rec := &recordingCluster{}
	lister := newSessionLister(rec, kube.NewForwardManager(), nil)

	if _, err := lister.ListRaw(t.Context(), kube.KindHelmRelease, "default"); err != nil {
		t.Fatalf("ListRaw(KindHelmRelease): %v", err)
	}

	if rec.helmReads != 1 {
		t.Errorf("release cache read %d times, want 1", rec.helmReads)
	}
	if rec.listed(kube.KindSecret) {
		t.Errorf("listing Helm releases read every Secret in the namespace (%v); "+
			"that pulls the largest kind on most clusters through the shared informer, "+
			"which is exactly what the filtered release cache exists to avoid", rec.listedKinds)
	}
}

// TestHelmReleaseSyncStateAsksAboutTheReleaseCache: the loading gate has to
// name the cache the rows actually came from. Asking about KindSecret while
// reading the release cache is how a screen hears "settled" from an informer
// it isn't using and claims "no releases" about one that is still filling.
func TestHelmReleaseSyncStateAsksAboutTheReleaseCache(t *testing.T) {
	rec := &recordingCluster{}
	lister := newSessionLister(rec, kube.NewForwardManager(), nil)

	lister.KindSynced(kube.KindHelmRelease, "default")

	if len(rec.syncedAsked) != 1 || rec.syncedAsked[0] != kube.KindHelmRelease {
		t.Fatalf("KindSynced asked about %v, want [%s]", rec.syncedAsked, kube.KindHelmRelease)
	}
}

// TestHelmReleaseErrorAsksAboutTheReleaseCache: KindError travels with
// KindSynced and has to name the same cache. A screen told "settled" by one
// informer and "no problem" by another renders an empty cluster over a read
// that failed — the spinner-that-never-ends traded for a lie.
func TestHelmReleaseErrorAsksAboutTheReleaseCache(t *testing.T) {
	rec := &recordingCluster{}
	lister := newSessionLister(rec, kube.NewForwardManager(), nil)

	if err := lister.KindError(kube.KindHelmRelease, "default"); err == nil {
		t.Fatal("KindError returned nil; the seam is not forwarded through the decorator stack")
	}
	if len(rec.errorAsked) != 1 || rec.errorAsked[0] != kube.KindHelmRelease {
		t.Fatalf("KindError asked about %v, want [%s]", rec.errorAsked, kube.KindHelmRelease)
	}
}

// TestKindSyncAndErrorForwardTheNamespaceArgument is
// docs/lazy-informers.md §5.6's rule stated as a decorator
// test: KindSynced/KindError must carry the caller's namespace through both
// layers of the decorator stack unchanged, not silently normalize it to ""
// or the release cache's own scope.
func TestKindSyncAndErrorForwardTheNamespaceArgument(t *testing.T) {
	rec := &recordingCluster{}
	lister := newSessionLister(rec, kube.NewForwardManager(), nil)

	lister.KindSynced(kube.KindPod, "team-a")
	if len(rec.syncedAskedNS) != 1 || rec.syncedAskedNS[0] != "team-a" {
		t.Fatalf("KindSynced forwarded namespace %v, want [\"team-a\"]", rec.syncedAskedNS)
	}
	if _, err := lister.ListRaw(t.Context(), kube.KindPod, "team-a"); err != nil {
		t.Fatalf("ListRaw: %v", err)
	}
	if err := lister.KindError(kube.KindPod, "team-b"); err == nil {
		t.Fatal("KindError returned nil; the seam is not forwarded through the decorator stack")
	}
	if len(rec.errorAskedNS) != 1 || rec.errorAskedNS[0] != "team-b" {
		t.Fatalf("KindError forwarded namespace %v, want [\"team-b\"]", rec.errorAskedNS)
	}
}

// TestScopedForwardsThroughTheDecoratorStack pins the seam the 403 card's
// --namespace-scoped hint depends on: an unforwarded Scoped would always
// read false through sess.Lister, regardless of what the underlying cluster
// answers, and the hint would keep suggesting a flag to a session already
// running it.
func TestScopedForwardsThroughTheDecoratorStack(t *testing.T) {
	rec := &recordingCluster{scoped: true}
	lister := newSessionLister(rec, kube.NewForwardManager(), nil)
	if !lister.Scoped() {
		t.Fatal("Scoped() = false through the decorator stack, want true")
	}

	rec2 := &recordingCluster{scoped: false}
	lister2 := newSessionLister(rec2, kube.NewForwardManager(), nil)
	if lister2.Scoped() {
		t.Fatal("Scoped() = true through the decorator stack for an unscoped cluster")
	}
}

// TestForwardsNeverStall: forwards are in-process state with no cache to
// fail, and the decorator answers for them itself rather than passing the
// question down to a kind the cluster has never heard of.
func TestForwardsNeverStall(t *testing.T) {
	rec := &recordingCluster{}
	lister := newSessionLister(rec, kube.NewForwardManager(), nil)

	if err := lister.KindError(kube.KindForward, ""); err != nil {
		t.Fatalf("KindError(Forward) = %v, want nil", err)
	}
	if len(rec.errorAsked) != 0 {
		t.Fatalf("the forward question reached the cluster as %v", rec.errorAsked)
	}
}

// TestDecoratedListerFallsBackWithoutTheReleaseCache pins the other half of
// the contract: once a decorator advertises ListHelmReleaseSecrets every
// caller's type assertion succeeds, so the method must never be a dead end.
// With no narrow cache underneath, the shared Secret cache is the documented
// answer — and KindSynced has to name *that* cache to match.
func TestDecoratedListerFallsBackWithoutTheReleaseCache(t *testing.T) {
	plain := &narrowlessCluster{}
	lister := newSessionLister(plain, kube.NewForwardManager(), nil)

	if _, err := lister.ListHelmReleaseSecrets(t.Context(), "default"); err != nil {
		t.Fatalf("ListHelmReleaseSecrets should degrade to the shared cache, got: %v", err)
	}
	if len(plain.listedKinds) != 1 || plain.listedKinds[0] != kube.KindSecret {
		t.Fatalf("fallback listed %v, want [%s]", plain.listedKinds, kube.KindSecret)
	}

	lister.KindSynced(kube.KindHelmRelease, "default")
	if len(plain.syncedAsked) != 1 || plain.syncedAsked[0] != kube.KindSecret {
		t.Fatalf("KindSynced asked about %v, want [%s] to match the fallback's source",
			plain.syncedAsked, kube.KindSecret)
	}
}

var (
	_ resources.RawLister          = (*recordingCluster)(nil)
	_ helmhistory.HelmSecretLister = (*recordingCluster)(nil)
	_ resources.RawLister          = (*narrowlessCluster)(nil)
)

// startedCluster implements only the KindStarted seam, reporting which kinds
// it was asked about.
type startedCluster struct {
	recordingCluster
	startedAsked []kube.ResourceKind
}

func (c *startedCluster) KindStarted(kind kube.ResourceKind, _ string) bool {
	c.startedAsked = append(c.startedAsked, kind)
	return false
}

// TestDecoratorsForwardKindStarted: tui.KindCached type-asserts KindStarted
// against the fully decorated lister, and an unforwarded seam silently falls
// back to KindSynced alone — which vouches for kinds nothing has started, so
// the goto palette's fuzzy corpus would start their informers (review 01 H1).
// The cluster here has a narrow release cache, so HelmRelease is asked about
// as itself.
func TestDecoratorsForwardKindStarted(t *testing.T) {
	cluster := &startedCluster{}
	lister := newSessionLister(cluster, kube.NewForwardManager(), nil)

	if lister.KindStarted(kube.KindCustomResourceDefinition, "") {
		t.Fatal("KindStarted(CRD) = true through the decorators; the cluster said false")
	}
	if !lister.KindStarted(kube.KindForward, "") {
		t.Fatal("KindStarted(Forward) = false; forwards are in-process and free to read")
	}
	lister.KindStarted(kube.KindHelmRelease, "default")
	want := []kube.ResourceKind{kube.KindCustomResourceDefinition, kube.KindHelmRelease}
	if len(cluster.startedAsked) != len(want) || cluster.startedAsked[0] != want[0] || cluster.startedAsked[1] != want[1] {
		t.Fatalf("KindStarted asked about %v, want %v", cluster.startedAsked, want)
	}
}
