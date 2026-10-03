package kube

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// Tests for docs/lazy-informers.md §5.7: a TLS cell or cert chain reads one
// Secret's tls.crt, and must not pay for every Secret in the cluster to do it.

// TestListTLSSecretsListsOnlyTLSSecretsInTheReadNamespace is the regression
// test for 11 M8. In the default cluster-wide mode a TLS read used to go
// through ListRaw(KindSecret, ns), which normalizes to the shared cluster-wide
// Secret informer with full payloads — every Secret in every namespace, Helm
// release manifests included. Every Secret LIST this path issues must carry
// the type=kubernetes.io/tls field selector and the read's own namespace, and
// the shared cache must stay unstarted.
func TestListTLSSecretsListsOnlyTLSSecretsInTheReadNamespace(t *testing.T) {
	c, cs := newLazyTestCluster(
		secretOfType("web-tls", "default", corev1.SecretTypeTLS),
		secretOfType("sh.helm.release.v1.web.v1", "default", HelmReleaseSecretType),
		secretOfType("api-tls", "other", corev1.SecretTypeTLS),
	)
	defer c.Stop()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := c.ListTLSSecrets(t.Context(), "default"); err != nil {
		t.Fatalf("ListTLSSecrets: %v", err)
	}
	waitFor(t, "the TLS Secret cache to fill", func() bool {
		return c.KindSynced(KindTLSSecret, "default")
	})

	selectors := secretListFieldSelectors(cs)
	if len(selectors) == 0 {
		t.Fatal("no Secret list was issued")
	}
	want := "type=" + string(corev1.SecretTypeTLS)
	for _, got := range selectors {
		if got != want {
			t.Fatalf("Secret list carried field selector %q, want %q — an unfiltered list pulls every Secret, Helm release payloads included", got, want)
		}
	}
	for _, ns := range secretListNamespaces(cs) {
		if ns != "default" {
			t.Fatalf("TLS Secrets listed in namespace %q, want \"default\" — the read was for one namespace", ns)
		}
	}

	c.mu.Lock()
	_, sharedStarted := c.kindInformers[scopeKey{KindSecret, ""}]
	_, sharedScoped := c.kindInformers[scopeKey{KindSecret, "default"}]
	c.mu.Unlock()
	if sharedStarted || sharedScoped {
		t.Fatal("a TLS read started the shared Secret informer")
	}
}

// TestTLSSecretCacheIsPerNamespaceRead: the cache key is the namespace the
// read used, in cluster-wide mode too — it must not normalize to "" like the
// typed kinds do, or a second namespace's TLS cell would be told "settled"
// by a cache that never held its Secrets.
func TestTLSSecretCacheIsPerNamespaceRead(t *testing.T) {
	c, cs := newLazyTestCluster(secretOfType("web-tls", "default", corev1.SecretTypeTLS))
	defer c.Stop()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if c.KindStarted(KindTLSSecret, "default") {
		t.Fatal("KindStarted reported a TLS cache nothing has read")
	}
	if c.KindSynced(KindTLSSecret, "default") {
		t.Fatal("KindSynced reported a never-started TLS cache as settled")
	}
	if _, err := c.ListTLSSecrets(t.Context(), "default"); err != nil {
		t.Fatalf("ListTLSSecrets: %v", err)
	}
	waitFor(t, "the default TLS cache to fill", func() bool {
		return c.KindSynced(KindTLSSecret, "default")
	})
	if !c.KindStarted(KindTLSSecret, "default") {
		t.Fatal("KindStarted(TLSSecret, default) false after the read")
	}
	if c.KindStarted(KindTLSSecret, "other") || c.KindSynced(KindTLSSecret, "other") {
		t.Fatal("another namespace's TLS cache answered off default's")
	}

	objs, err := c.ListTLSSecrets(t.Context(), "default")
	if err != nil || len(objs) != 1 {
		t.Fatalf("ListTLSSecrets(default) = %d objects, %v; want the one TLS Secret", len(objs), err)
	}
	if n := len(secretListNamespaces(cs)); n != 1 {
		t.Fatalf("issued %d Secret lists for two reads of one namespace, want 1", n)
	}
}

// TestScopedModeTLSSecretCacheUsesTheReadNamespace: under --namespace-scoped
// the key is still the read's own namespace, so the gate asks about exactly
// the cache the read filled.
func TestScopedModeTLSSecretCacheUsesTheReadNamespace(t *testing.T) {
	c, cs := newLazyTestCluster(secretOfType("web-tls", "team-a", corev1.SecretTypeTLS))
	defer c.Stop()
	c.SetNamespaceScope("team-a")
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := c.ListTLSSecrets(t.Context(), "team-a"); err != nil {
		t.Fatalf("ListTLSSecrets: %v", err)
	}
	waitFor(t, "the team-a TLS cache to fill", func() bool {
		return c.KindSynced(KindTLSSecret, "team-a")
	})
	for _, ns := range secretListNamespaces(cs) {
		if ns != "team-a" {
			t.Fatalf("TLS Secrets listed in namespace %q under --namespace-scoped=team-a", ns)
		}
	}
}

// TestTLSSecretCacheForbiddenIsReportedNotEmpty: a denied TLS cache reports
// settled with the denial attached, so a TLS cell shows "permission denied"
// rather than spinning or claiming the Secret is missing.
func TestTLSSecretCacheForbiddenIsReportedNotEmpty(t *testing.T) {
	c, cs := newLazyTestCluster()
	defer c.Stop()
	cs.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", fmt.Errorf("nope"))
	})
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := c.ListTLSSecrets(t.Context(), "default"); err != nil {
		t.Fatalf("ListTLSSecrets: %v", err)
	}
	waitFor(t, "the TLS cache to be marked forbidden", func() bool {
		return c.KindForbidden(KindTLSSecret, "default") != nil
	})
	if !c.KindSynced(KindTLSSecret, "default") {
		t.Fatal("a forbidden TLS cache must read as settled, or its spinner never ends")
	}
	if c.KindForbidden(KindSecret, "default") != nil {
		t.Fatal("the TLS cache's denial leaked onto the shared Secret cache's key")
	}
}

// TestTLSSecretsAfterStop: a torn-down cluster reads empty rather than
// starting an informer that could never be stopped.
func TestTLSSecretsAfterStop(t *testing.T) {
	c, cs := newLazyTestCluster(secretOfType("web-tls", "default", corev1.SecretTypeTLS))
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	c.Stop()

	before := len(cs.Actions())
	if _, err := c.ListTLSSecrets(t.Context(), "default"); err != nil {
		t.Fatalf("ListTLSSecrets after Stop should read empty, not error: %v", err)
	}
	if got := len(cs.Actions()); got != before {
		t.Fatalf("issued %d API actions after Stop, want 0", got-before)
	}
}
