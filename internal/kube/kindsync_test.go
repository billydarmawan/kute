package kube

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
)

func newSyncTestCluster() *Cluster {
	cs := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}})
	return &Cluster{
		clientset: cs,
		factory:   informers.NewSharedInformerFactory(cs, 0),
		stopCh:    make(chan struct{}),
		events:    make(chan ResourceChangedMsg, 64),
		health:    newHealth(),
	}
}

// TestKindSyncedFalseBeforeRegistration: a real kind whose informer has not
// been registered has, by definition, an empty cache that means nothing. It
// must not read as "trustworthy empty".
func TestKindSyncedFalseBeforeRegistration(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)

	if c.KindSynced(KindSecret, "") {
		t.Fatal("KindSynced(Secret) = true before its informer was registered")
	}
}

func TestKindSyncedTrueAfterCacheFills(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)

	c.registerWatches(KindPod)
	c.factory.Start(c.stopCh)
	c.factory.WaitForCacheSync(c.stopCh)

	if !c.KindSynced(KindPod, "") {
		t.Fatal("KindSynced(Pod) = false after WaitForCacheSync")
	}
	// Registering one kind must not vouch for another.
	if c.KindSynced(KindSecret, "") {
		t.Fatal("KindSynced(Secret) = true after only Pod was registered")
	}
}

// TestReachedLatchesOnACacheEventBeforeSynced pins the distinction the 4c swap
// depends on: one object arriving from the apiserver is proof this cluster was
// reached, and it lands well before Synced — which waits for the *whole* eager
// set. An outage in between is a mid-session outage over useful cached state,
// not an unreachable launch.
func TestReachedLatchesOnACacheEventBeforeSynced(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)

	if c.Reached() {
		t.Fatal("Reached() = true before anything was read from the apiserver")
	}

	c.registerWatches(KindPod)
	c.factory.Start(c.stopCh)
	c.factory.WaitForCacheSync(c.stopCh)

	// Deliberately not the AddFunc path: a shared informer delivers to its
	// listeners on its own goroutine, so the callback can still be queued at
	// the moment the cache is filled and readable. This is the synchronous
	// source ping consults, and it must already answer yes here.
	c.mu.Lock()
	c.noteReachedIfAnyCacheSyncedLocked()
	c.mu.Unlock()

	if !c.Reached() {
		t.Fatal("Reached() = false after the Pod cache completed its initial LIST")
	}
	// The whole point: this is true while Synced — the connect-completion
	// latch Start sets only after every eager cache settles — is still false.
	if c.Synced() {
		t.Fatal("precondition broken: Synced() is already true, so this no longer tests the gap it exists for")
	}
}

// TestReachedSurvivesADroppedEvent: notifyScope's channel send is lossy by
// design (a full buffer drops and lets the next resync reconcile), so the
// latch must not depend on the message being delivered. The root shell's other
// proof-of-connection — ResourceChangedMsg — is exactly what this covers for.
func TestReachedSurvivesADroppedEvent(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)
	// A zero-capacity channel nothing reads: every send takes the default arm.
	c.events = make(chan ResourceChangedMsg)

	c.notify(c.generation, KindPod)

	if !c.Reached() {
		t.Fatal("Reached() = false after an event whose send was dropped; the latch must not ride the channel")
	}
}

// TestReachedIgnoresAStaleGeneration: a callback from an informer a completed
// SwitchContext has already torn down says nothing about the context now
// active — the same guard notify already applies to the reload message.
func TestReachedIgnoresAStaleGeneration(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)
	c.generation = 2

	c.notify(1, KindPod)

	if c.Reached() {
		t.Fatal("Reached() latched from a previous generation's informer callback")
	}
}

// TestKindSyncedTrueForSyntheticKinds: kinds with no informer at all have no
// cache to wait on, so gating a loading state on them would hang forever.
func TestKindSyncedTrueForSyntheticKinds(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)

	// Forwards are in-process state, not a cache.
	if !c.KindSynced(KindForward, "") {
		t.Error("KindSynced(Forward) = false; a kind with no informer has nothing to wait for")
	}
}

// TestKindSyncedFalseForDiscoveredButUnstartedKind: a kind CRD discovery
// already knows about, but whose instance informer nothing has read yet, must
// not read as "settled" — Goto's fuzzy resource corpus and the empty-state
// hints both skip a kind only once KindSynced says it's unsafe to list
// (goto.go's kindSynced guard), and reporting true here let both list a
// never-started discovered kind, starting its informer just to render a jump
// entry or an empty-state hint (the exact breadth-first read CLAUDE.md's
// informer invariant forbids). An undiscovered kind is unaffected: it still
// falls through to true, since there is nothing to wait on at all.
func TestKindSyncedFalseForDiscoveredButUnstartedKind(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)

	c.discovered = []DiscoveredKind{{
		Kind:   "Widget",
		Plural: "widgets",
		Group:  "example.com",
	}}

	if c.KindSynced(ResourceKind("Widget"), "") {
		t.Fatal("KindSynced(Widget) = true for a discovered kind whose informer was never started")
	}
}

// TestKindSyncedForHelmReleasesTracksItsOwnCache: releases are no longer
// derived from the shared Secret cache — they have their own filtered
// informer, and this must answer for that one. Answering for KindSecret
// would report "settled" off a cache the Helm screens never read.
func TestKindSyncedForHelmReleasesTracksItsOwnCache(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)

	if c.KindSynced(KindHelmRelease, "") {
		t.Fatal("KindSynced(HelmRelease) = true before its informer was started")
	}

	// Starting the shared Secret cache must not vouch for releases.
	c.registerWatches(KindSecret)
	c.factory.Start(c.stopCh)
	c.factory.WaitForCacheSync(c.stopCh)
	if c.KindSynced(KindHelmRelease, "") {
		t.Fatal("the shared Secret cache wrongly vouched for HelmRelease")
	}

	if _, err := c.ListHelmReleaseSecrets(t.Context(), ""); err != nil {
		t.Fatalf("ListHelmReleaseSecrets: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !c.KindSynced(KindHelmRelease, "") && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !c.KindSynced(KindHelmRelease, "") {
		t.Fatal("KindSynced(HelmRelease) never settled after its own cache filled")
	}
}

// TestKindSyncedTrueAfterStop: a torn-down cluster will never sync anything
// again, so callers must be told to render what they have rather than spin.
func TestKindSyncedTrueAfterStop(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	c.Stop()

	if !c.KindSynced(KindSecret, "") {
		t.Fatal("KindSynced(Secret) = false after Stop; a stopped cluster never syncs")
	}
}

// TestKindSyncedTrueAfterPermissionError is the anti-hang guard: an informer
// for a kind the user may not list never syncs, so without this browse would
// retry every 250ms forever instead of showing the permission-denied card.
func TestKindSyncedTrueAfterPermissionError(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)

	if c.KindSynced(KindHorizontalPodAutoscaler, "") {
		t.Fatal("precondition: unregistered kind should not report synced")
	}
	c.markKindFailed(c.generation, KindHorizontalPodAutoscaler, "", apierrors.NewForbidden(
		schema.GroupResource{Resource: "horizontalpodautoscalers"}, "", errors.New("nope")))

	if !c.KindSynced(KindHorizontalPodAutoscaler, "") {
		t.Fatal("KindSynced = false after a permission failure; the cache will never arrive")
	}
	// A denial for one kind says nothing about any other.
	if c.KindSynced(KindSecret, "") {
		t.Fatal("markKindFailed(HPA) wrongly vouched for Secret")
	}
}

// TestKindSyncedTrueAfterAStalledInitialList is the same anti-hang guard for
// the failure that isn't a permission denial: a LIST big enough (or a link
// slow enough) that the request never completes inside the API server's
// window. The reflector retries the same doomed request indefinitely, so
// "not synced yet" is true forever — measured on a real cluster as a Helm
// Releases screen that had been loading for over 2000 seconds.
func TestKindSyncedTrueAfterAStalledInitialList(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)

	streamErr := errors.New("stream error when reading response body, may be caused by closed connection")
	c.noteWatchError(c.generation, KindSecret, "", streamErr)

	if !c.KindSynced(KindSecret, "") {
		t.Fatal("KindSynced = false for a cache whose initial LIST keeps failing; nothing is coming, so the spinner never ends")
	}
	if got := c.KindError(KindSecret, ""); got == nil {
		t.Fatal("KindError = nil for a stalled cache; without a reason the screen claims the cluster is empty")
	}
	// The stall belongs to the kind that failed, not to the session.
	if c.KindSynced(KindConfigMap, "") || c.KindError(KindConfigMap, "") != nil {
		t.Fatal("one kind's failed LIST vouched for another")
	}
}

// TestStalledKindRecoversWhenTheCacheFinallyFills: the stall is a status, not
// a verdict. The reflector keeps retrying, and a retry that lands has to
// leave the kind reading as genuinely synced with no error attached — or one
// slow LIST would caption a working screen with a failure forever.
func TestStalledKindRecoversWhenTheCacheFinallyFills(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)

	c.noteWatchError(c.generation, KindPod, "", errors.New("connect: connection refused"))
	if c.KindError(KindPod, "") == nil {
		t.Fatal("precondition: the failed LIST should be recorded")
	}

	c.registerWatches(KindPod)
	c.factory.Start(c.stopCh)
	c.factory.WaitForCacheSync(c.stopCh)

	if err := c.KindError(KindPod, ""); err != nil {
		t.Fatalf("KindError = %v after the cache filled; the stall must clear", err)
	}
	if !c.KindSynced(KindPod, "") {
		t.Fatal("KindSynced = false after the cache filled")
	}
}

// TestWatchErrorAfterSyncIsNotAStall: a watch dropping once the cache holds
// data is ordinary churn — the informer re-establishes it and the screen goes
// on rendering real rows. Reporting that as a load failure would paste an
// error over a perfectly good table.
func TestWatchErrorAfterSyncIsNotAStall(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)

	c.registerWatches(KindPod)
	c.factory.Start(c.stopCh)
	c.factory.WaitForCacheSync(c.stopCh)

	c.noteWatchError(c.generation, KindPod, "", errors.New("watch closed unexpectedly"))
	if err := c.KindError(KindPod, ""); err != nil {
		t.Fatalf("KindError = %v for a post-sync watch drop; the cache still holds data", err)
	}
}

// TestNoteWatchErrorDropsStaleGeneration pins the fix for the SwitchContext
// race (docs/lazy-informers.md §5.6's own generation-guard
// section): a callback registered against an old context can still be
// mid-flight when SwitchContext bumps c.generation and clears kindStalled —
// its own up-front generationCurrent check happened before that, on a
// separate lock acquisition, so only a re-check inside the same critical
// section as the write actually stops a stale generation from landing.
func TestNoteWatchErrorDropsStaleGeneration(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)

	staleGen := c.generation
	c.generation++ // simulates a SwitchContext that ran between the
	// callback's own preflight check and this call.

	c.noteWatchError(staleGen, KindSecret, "", errors.New("stream error when reading response body"))

	if err := c.KindError(KindSecret, ""); err != nil {
		t.Fatalf("KindError = %v; a stale-generation watch error must not be recorded", err)
	}
}

func TestWatchFailureKeepsHealthUnreadyUntilWatchEstablished(t *testing.T) {
	t.Parallel()
	c := &Cluster{health: newHealth(), events: make(chan ResourceChangedMsg, 1)}
	c.recordWatchError(0, KindPod, "", errors.New("watch connection closed"))
	if c.allStartedKindsSynced() {
		t.Fatal("watch failure still reported all started kinds ready")
	}

	// Object events are not authoritative: a streaming LIST may deliver
	// initial objects before its replacement WATCH is actually established.
	c.notify(0, KindPod)
	if c.allStartedKindsSynced() {
		t.Fatal("object event cleared watch-unhealthy state")
	}
	c.recordWatchEstablished(0, KindPod, "")
	if !c.allStartedKindsSynced() {
		t.Fatal("successful WATCH did not clear watch-unhealthy state")
	}
	select {
	case <-c.health.retry:
	default:
		t.Fatal("final recovered WATCH did not request an immediate health probe")
	}
}

func TestInitialListFailureDoesNotAwaitInformerEvent(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)

	// Register the informer without starting it so its cache is known but has
	// not completed the initial LIST. An empty successful retry may produce no
	// object event, so recovery must depend on HasSynced rather than notify.
	c.registerWatches(KindPod)
	c.recordWatchError(c.generation, KindPod, "", errors.New("initial list failed"))

	key := scopeKey{KindPod, ""}
	if c.watchUnhealthy[key] {
		t.Fatal("initial LIST failure entered post-sync watch-unhealthy state")
	}
	if c.kindStalled[key] == nil {
		t.Fatal("initial LIST failure was not recorded as a stalled kind")
	}
}

func TestWatchRecoveryClearsOnlyItsInformerScope(t *testing.T) {
	t.Parallel()
	c := &Cluster{
		health: newHealth(), events: make(chan ResourceChangedMsg, 1),
		watchUnhealthy: map[scopeKey]bool{
			{KindConfigMap, "team-a"}: true,
			{KindConfigMap, "team-b"}: true,
		},
	}
	c.recordWatchEstablished(0, KindConfigMap, "team-a")
	if c.watchUnhealthy[scopeKey{KindConfigMap, "team-a"}] {
		t.Fatal("recovered scope team-a stayed unhealthy")
	}
	if !c.watchUnhealthy[scopeKey{KindConfigMap, "team-b"}] {
		t.Fatal("recovering team-a cleared team-b's independent watch failure")
	}
}

func TestWatchEstablishmentDropsStaleGeneration(t *testing.T) {
	t.Parallel()
	c := &Cluster{
		generation: 2, health: newHealth(), events: make(chan ResourceChangedMsg, 1),
		watchUnhealthy: map[scopeKey]bool{{KindPod, ""}: true},
	}
	c.recordWatchEstablished(1, KindPod, "")
	if !c.watchUnhealthy[scopeKey{KindPod, ""}] {
		t.Fatal("old-context WATCH establishment cleared current generation health")
	}
}

// TestMarkKindFailedDropsStaleGeneration is noteWatchError's Forbidden-path
// counterpart above — markKindFailed re-checks gen itself for the same
// reason.
func TestMarkKindFailedDropsStaleGeneration(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)

	staleGen := c.generation
	c.generation++

	c.markKindFailed(staleGen, KindSecret, "", apierrors.NewForbidden(
		schema.GroupResource{Resource: "secrets"}, "", errors.New("nope")))

	if err := c.KindForbidden(KindSecret, ""); err != nil {
		t.Fatalf("KindForbidden = %v; a stale-generation denial must not be recorded", err)
	}
}

// TestStalledPermissionErrorStaysAPermissionError: Forbidden has its own
// path (kindFailed, and a permission-denied card at the other end). Routing
// it through the stall channel too would show a load-failure caption where
// the screen already has a better answer.
func TestStalledPermissionErrorStaysAPermissionError(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)

	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("nope"))
	c.noteWatchError(c.generation, KindSecret, "", forbidden)

	if !c.KindSynced(KindSecret, "") {
		t.Fatal("a forbidden kind must still read as settled")
	}
	if err := c.KindError(KindSecret, ""); err != nil {
		t.Fatalf("KindError = %v for a forbidden kind; that failure has its own state", err)
	}
	// The denial travels KindForbidden instead, carrying the reason — which
	// is what lets a screen say "you may not read this" rather than "there
	// are none".
	if got := c.KindForbidden(KindSecret, ""); !IsPermissionError(got) {
		t.Fatalf("KindForbidden = %v for a forbidden kind, want the denial itself", got)
	}
	// And says nothing about a kind that was never refused.
	if got := c.KindForbidden(KindPod, ""); got != nil {
		t.Fatalf("KindForbidden(Pod) = %v, want nil — only Secret was refused", got)
	}
}

func TestPermissionErrorsAreDistinguishedFromOutages(t *testing.T) {
	t.Parallel()
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Resource: "horizontalpodautoscalers"}, "", errors.New("nope"))
	if !IsPermissionError(forbidden) {
		t.Fatal("a Forbidden status error must count as a permission error")
	}
	if IsPermissionError(errors.New("connect: connection refused")) {
		t.Fatal("a refused connection is an outage, not a permission error — it must stay retryable")
	}
}

// TestSyncedIsALatch pins Synced's redefined meaning: it answers "has this
// cluster finished connecting", so a kind registered later must not drag it
// back to false.
func TestSyncedIsALatch(t *testing.T) {
	t.Parallel()
	c := newSyncTestCluster()
	defer close(c.stopCh)

	c.mu.Lock()
	c.synced = true
	c.mu.Unlock()

	c.registerWatches(KindSecret) // registered, definitely not filled
	if !c.Synced() {
		t.Fatal("Synced() went false after a later registration; it is a connect latch, not a cache state")
	}
	if c.KindSynced(KindSecret, "") {
		t.Fatal("KindSynced(Secret) = true for a registered-but-unstarted informer")
	}
}

// TestCleanWatchRotationIsNotAnOutage pins M3: the apiserver ends every WATCH
// cleanly when the reflector's timeoutSeconds expires, so an established
// body reaching io.EOF is a routine rotation. It may hold the header off
// green until the replacement WATCH is up, but it must never flip a synced,
// connected cluster to Reconnecting — while a body that breaks mid-stream
// still does.
func TestCleanWatchRotationIsNotAnOutage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		body      io.Reader
		wantPhase ConnPhase
	}{
		{"clean EOF", strings.NewReader(""), ConnConnected},
		{"broken stream", iotest.ErrReader(io.ErrUnexpectedEOF), ConnReconnecting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newSyncTestCluster()
			defer close(c.stopCh)
			c.watcher = newWatchObserver()
			c.registerWatches(KindPod)
			c.factory.Start(c.stopCh)
			c.factory.WaitForCacheSync(c.stopCh)

			rt := &observingRoundTripper{
				gate: &authenticationGate{}, observer: c.watcher,
				next: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(tc.body)}, nil
				}),
			}
			req, _ := http.NewRequest(http.MethodGet, "https://cluster/api/v1/pods?watch=true", nil)
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if got := c.ConnState().Phase; got != tc.wantPhase {
				t.Fatalf("ConnState().Phase after the WATCH body ended = %v, want %v", got, tc.wantPhase)
			}
			if c.allStartedKindsSynced() {
				t.Fatal("an ended WATCH left the header free to turn green before its replacement was established")
			}
		})
	}
}
