package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stesting "k8s.io/client-go/testing"
)

func secretOfType(name, namespace string, typ corev1.SecretType) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       typ,
	}
}

// secretListFieldSelectors returns the field selector carried by each
// recorded Secret list, which is what proves the narrowing happened at the
// API server rather than after the bytes arrived.
func secretListFieldSelectors(cs interface{ Actions() []k8stesting.Action }) []string {
	var out []string
	for _, a := range cs.Actions() {
		if a.GetVerb() != "list" || a.GetResource().Resource != "secrets" {
			continue
		}
		la, ok := a.(k8stesting.ListAction)
		if !ok {
			continue
		}
		out = append(out, la.GetListRestrictions().Fields.String())
	}
	return out
}

// TestListHelmReleaseSecretsFiltersServerSide is the whole point of the
// separate cache. Releases used to be decoded from the shared Secret cache,
// so listing them pulled every Secret in the namespace — image-pull
// credentials, TLS material, ServiceAccount tokens — to find a handful of
// release revisions. The request must carry a field selector so the server
// never sends the rest.
func TestListHelmReleaseSecretsFiltersServerSide(t *testing.T) {
	c, cs := newLazyTestCluster(
		secretOfType("sh.helm.release.v1.web.v1", "default", HelmReleaseSecretType),
		secretOfType("registry-creds", "default", corev1.SecretTypeDockerConfigJson),
		secretOfType("tls-cert", "default", corev1.SecretTypeTLS),
	)
	defer c.Stop()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := c.ListHelmReleaseSecrets(t.Context(), "default"); err != nil {
		t.Fatalf("ListHelmReleaseSecrets: %v", err)
	}
	waitFor(t, "the release-secret informer to list", func() bool {
		return len(secretListFieldSelectors(cs)) > 0
	})

	want := "type=" + string(HelmReleaseSecretType)
	for _, got := range secretListFieldSelectors(cs) {
		if got != want {
			t.Fatalf("Secret list carried field selector %q, want %q — an unfiltered list pulls every Secret in the namespace", got, want)
		}
	}
}

// TestListHelmReleaseSecretsDoesNotStartTheSharedSecretCache: the two caches
// are separate on purpose. Opening the Helm list must not drag in every
// Secret by starting the shared informer as a side effect.
func TestListHelmReleaseSecretsDoesNotStartTheSharedSecretCache(t *testing.T) {
	c, cs := newLazyTestCluster(
		secretOfType("sh.helm.release.v1.web.v1", "default", HelmReleaseSecretType),
	)
	defer c.Stop()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := c.ListHelmReleaseSecrets(t.Context(), "default"); err != nil {
		t.Fatalf("ListHelmReleaseSecrets: %v", err)
	}
	waitFor(t, "the release-secret informer to list", func() bool {
		return len(secretListFieldSelectors(cs)) > 0
	})

	c.mu.Lock()
	_, sharedStarted := c.kindInformers[scopeKey{KindSecret, ""}]
	c.mu.Unlock()
	if sharedStarted {
		t.Fatal("listing Helm releases started the shared Secret informer; that pulls every Secret in the cluster")
	}

	// An unfiltered list would show up here as a selector-less Secret list.
	for _, sel := range secretListFieldSelectors(cs) {
		if sel == "" {
			t.Fatal("an unfiltered Secret list was issued alongside the filtered one")
		}
	}
}

// TestSharedSecretCacheStaysUnfiltered: browsing Secrets is a screen about
// Secrets, so that cache must keep holding all of them. The filter belongs
// to the release cache alone.
func TestSharedSecretCacheStaysUnfiltered(t *testing.T) {
	c, cs := newLazyTestCluster(
		secretOfType("registry-creds", "default", corev1.SecretTypeDockerConfigJson),
	)
	defer c.Stop()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := c.ListRaw(t.Context(), KindSecret, "default"); err != nil {
		t.Fatalf("ListRaw(Secret): %v", err)
	}
	waitFor(t, "the shared Secret informer to list", func() bool {
		return len(secretListFieldSelectors(cs)) > 0
	})

	for _, sel := range secretListFieldSelectors(cs) {
		if sel != "" {
			t.Fatalf("the shared Secret list carried field selector %q; browsing Secrets must show all of them", sel)
		}
	}
}

// secretListNamespaces returns the namespace each recorded Secret list was
// issued against ("" = cluster-wide), which is what proves the scope of the
// read matched the scope of the screen asking for it.
func secretListNamespaces(cs interface{ Actions() []k8stesting.Action }) []string {
	var out []string
	for _, a := range cs.Actions() {
		if a.GetVerb() != "list" || a.GetResource().Resource != "secrets" {
			continue
		}
		out = append(out, a.GetNamespace())
	}
	return out
}

// TestListHelmReleaseSecretsScopesToTheNamespace: the type filter narrows
// which Secrets come back, not how many namespaces are scanned, and release
// Secrets carry the release's whole gzipped manifest. Listing one namespace's
// releases cluster-wide was 8.19 MB against 4 MB on the cluster this was
// measured against — and on a link where the bigger read can't finish inside
// the API server's 60s window, it never completes at all: the reflector
// retries the same doomed LIST forever and the Helm list spins for as long as
// you leave it open.
func TestListHelmReleaseSecretsScopesToTheNamespace(t *testing.T) {
	c, cs := newLazyTestCluster(
		secretOfType("sh.helm.release.v1.web.v1", "default", HelmReleaseSecretType),
		secretOfType("sh.helm.release.v1.api.v1", "other", HelmReleaseSecretType),
	)
	defer c.Stop()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := c.ListHelmReleaseSecrets(t.Context(), "default"); err != nil {
		t.Fatalf("ListHelmReleaseSecrets: %v", err)
	}
	waitFor(t, "the release-secret informer to list", func() bool {
		return len(secretListNamespaces(cs)) > 0
	})

	for _, ns := range secretListNamespaces(cs) {
		if ns != "default" {
			t.Fatalf("release Secrets listed in namespace %q, want \"default\" — a cluster-wide read pulls every namespace's release manifests to answer for one", ns)
		}
	}
}

// TestListHelmReleaseSecretsAllNamespacesStaysClusterWide: the scoping above
// follows the read, it isn't a blanket narrowing. 19a's overview asks for
// every namespace's releases and must get them.
func TestListHelmReleaseSecretsAllNamespacesStaysClusterWide(t *testing.T) {
	c, cs := newLazyTestCluster(
		secretOfType("sh.helm.release.v1.web.v1", "default", HelmReleaseSecretType),
		secretOfType("sh.helm.release.v1.api.v1", "other", HelmReleaseSecretType),
	)
	defer c.Stop()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := c.ListHelmReleaseSecrets(t.Context(), ""); err != nil {
		t.Fatalf("ListHelmReleaseSecrets: %v", err)
	}
	waitFor(t, "the release-secret informer to list", func() bool {
		return len(secretListNamespaces(cs)) > 0
	})
	waitFor(t, "the all-namespaces release cache to fill", func() bool {
		return c.KindSynced(KindHelmRelease, "")
	})

	for _, ns := range secretListNamespaces(cs) {
		if ns != "" {
			t.Fatalf("all-namespaces release list was scoped to %q", ns)
		}
	}
	objs, err := c.ListHelmReleaseSecrets(t.Context(), "")
	if err != nil {
		t.Fatalf("ListHelmReleaseSecrets: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("all-namespaces read returned %d release Secrets, want both", len(objs))
	}
}

// TestKindSyncedFollowsTheScopeOfTheLastRead: with one cache per namespace,
// answering off the wrong one is the failure KindSynced exists to prevent —
// a screen that just read a namespace nothing has cached yet would be told
// its empty answer is trustworthy and flash "no releases".
func TestKindSyncedFollowsTheScopeOfTheLastRead(t *testing.T) {
	c, _ := newLazyTestCluster(
		secretOfType("sh.helm.release.v1.web.v1", "default", HelmReleaseSecretType),
	)
	defer c.Stop()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := c.ListHelmReleaseSecrets(t.Context(), "default"); err != nil {
		t.Fatalf("ListHelmReleaseSecrets: %v", err)
	}
	waitFor(t, "the default-namespace release cache to fill", func() bool {
		return c.KindSynced(KindHelmRelease, "default")
	})

	// A namespace that has never been read has no cache started at all.
	if c.KindSynced(KindHelmRelease, "other") {
		t.Fatal("KindSynced answered off another namespace's cache; an empty first read there would render as \"no releases\"")
	}
}

// TestHelmReleaseSecretsAfterStop mirrors the lazy-kind contract: a
// torn-down cluster reads empty rather than starting an informer that could
// never be stopped.
func TestHelmReleaseSecretsAfterStop(t *testing.T) {
	c, cs := newLazyTestCluster(
		secretOfType("sh.helm.release.v1.web.v1", "default", HelmReleaseSecretType),
	)
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	c.Stop()

	before := len(cs.Actions())
	if _, err := c.ListHelmReleaseSecrets(t.Context(), "default"); err != nil {
		t.Fatalf("ListHelmReleaseSecrets after Stop should read empty, not error: %v", err)
	}
	if got := len(cs.Actions()); got != before {
		t.Fatalf("issued %d API actions after Stop, want 0", got-before)
	}
}

// TestNamespacedReleaseReadReusesTheSyncedAllNamespacesCache: once the
// all-namespaces release cache has filled it already holds every namespace's
// revisions, so a namespaced read must be served from it — not issue a
// second LIST (and start a second watch) for objects already in memory.
func TestNamespacedReleaseReadReusesTheSyncedAllNamespacesCache(t *testing.T) {
	c, cs := newLazyTestCluster(
		secretOfType("sh.helm.release.v1.web.v1", "default", HelmReleaseSecretType),
		secretOfType("sh.helm.release.v1.api.v1", "other", HelmReleaseSecretType),
	)
	defer c.Stop()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := c.ListHelmReleaseSecrets(t.Context(), ""); err != nil {
		t.Fatalf("ListHelmReleaseSecrets: %v", err)
	}
	waitFor(t, "the all-namespaces release cache to fill", func() bool {
		return c.KindSynced(KindHelmRelease, "")
	})
	before := len(secretListNamespaces(cs))

	objs, err := c.ListHelmReleaseSecrets(t.Context(), "default")
	if err != nil {
		t.Fatalf("ListHelmReleaseSecrets: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("namespaced read returned %d release Secrets, want default's one", len(objs))
	}
	if !c.KindSynced(KindHelmRelease, "default") || !c.KindStarted(KindHelmRelease, "default") {
		t.Fatal("the namespaced read is served by a synced cache, so KindSynced/KindStarted must say so")
	}
	if got := secretListNamespaces(cs)[before:]; len(got) != 0 {
		t.Fatalf("namespaced read issued Secret lists %q — the all-namespaces cache already holds them", got)
	}
}
