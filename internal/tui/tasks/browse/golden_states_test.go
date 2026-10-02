// golden_states_test.go pins the browse skeleton's other rendered states
// beyond 2a's resting Pods list (golden_test.go): 4a/4b/6b/9a/10c/11a/13c/
// 14a/14b/15a/18a/20a all share this one package (docs/design README.md:
// "9a — not a new screen", "13c — a registry kind, not a bespoke screen"),
// so each gets its own goldenXModel builder plus fixtures named
// "<state>-<W>x<H>.golden", following setup/golden_test.go's
// multiple-states-in-one-file convention.
package browse

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metaerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/helmrepo"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/testutil/goldentest"
	"github.com/kute-dev/kute/internal/tui"
)

func goldenStateDir() string {
	return filepath.Join("..", "..", "..", "..", "test", "golden", "browse")
}

// --- 4a: connection lost mid-session ---

func goldenOfflineModel(t *testing.T, width, height int) Model {
	t.Helper()
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			goldenPod("api-7d9f6c8-abcde", corev1.PodRunning, true, 0, "", "node-a"),
			goldenPod("worker-0", corev1.PodRunning, false, 6, "CrashLoopBackOff", "node-a"),
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	// m.fetchedAt/m.now are both real wall-clock reads (load()/the
	// ConnStateMsg handler each call time.Now()) — pinned to a fixed instant
	// here so the stale strip's absolute "showing snapshot from HH:MM:SS"
	// text and "· Ns old"/"next in Ns" countdowns don't drift between the
	// UPDATE_GOLDEN run and every later comparison run.
	fixedNow := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	m.fetchedAt = fixedNow.Add(-94 * time.Second)
	m = step(t, m, kube.ConnStateMsg{
		Phase:       kube.ConnReconnecting,
		Err:         "dial tcp 10.0.0.5:16443: i/o timeout",
		Attempt:     3,
		NextRetryAt: fixedNow.Add(4 * time.Second),
	})
	m.now = fixedNow
	return m
}

// --- 4b: RBAC / API error on one kind (403) ---

func goldenPermissionDeniedModel(t *testing.T, width, height int) Model {
	t.Helper()
	msg := `User "dev-readonly" cannot list resource "secrets" in namespace "nva-stage"`
	lister := forbiddenLister{
		kind: kube.KindSecret,
		err:  metaerrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New(msg)),
	}
	sess := newSession()
	sess.Location.Namespace = "nva-stage"
	sess.Location.Kind = kube.KindSecret
	m := New(Config{Session: sess, Lister: lister})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	return m
}

// --- 6b: all-namespaces mode ---

func goldenAllNSModel(t *testing.T, width, height int) Model {
	t.Helper()
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			pod("nva-prod", "api-1"),
			crashPod("nva-stage", "worker-0"),
			pod("nva-stage", "cache-0"),
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	m = step(t, m, tui.SwitchNamespaceMsg{Namespace: ""})
	return m
}

// --- 9a: deployments list (exemplar for every non-pod kind) ---

func goldenDeploymentsModel(t *testing.T, width, height int) Model {
	t.Helper()
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {
			deploymentObj("nva-stage", "nva-worker"),
			deploymentObj("nva-stage", "nva-gateway"),
		},
	}}
	sess := newSession()
	sess.Location.Namespace = "nva-stage"
	sess.Location.Kind = kube.KindDeployment
	m := New(Config{Session: sess, Lister: lister})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	return m
}

// --- 10c: empty namespace (connected, zero pods) ---

func goldenEmptyModel(t *testing.T, width, height int) Model {
	t.Helper()
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			pod("nva-stage", "api-1"),
			pod("nva-stage", "api-2"),
		},
		kube.KindNamespace: {
			namespace("default"),
			namespace("nva-stage"),
		},
		kube.KindConfigMap: {
			configMap("default", "app-config"),
			configMap("default", "other-config"),
		},
		kube.KindSecret: {secret("default", "app-secret")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	return m
}

// --- 11a: nodes list (cluster-scoped) ---

func goldenNodeObj(name string, ready bool, cordoned bool, cpu, mem string) *corev1.Node {
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: cordoned},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.30.1"},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
		},
	}
}

func goldenNodesModel(t *testing.T, width, height int) Model {
	t.Helper()
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {
			goldenNodeObj("node-a", true, false, "4", "16Gi"),
			goldenNodeObj("node-b", true, false, "4", "16Gi"),
			goldenNodeObj("node-c", false, false, "4", "16Gi"),
			goldenNodeObj("node-d", true, true, "4", "16Gi"),
		},
		kube.KindPod: {
			pod("default", "api-1"),
			pod("default", "api-2"),
		},
	}}
	sess := newSession()
	sess.Location.Kind = kube.KindNode
	m := New(Config{Session: sess, Lister: lister})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	return m
}

// --- 13c: forwards manager ---
//
// Forwards flow through the same resources.List/Project pipeline as real
// API objects (kube.ForwardObject implements runtime.Object) — so the
// golden model injects *kube.ForwardObject rows straight into fakeLister
// rather than driving the real *kube.ForwardManager, which would need a
// live dialer goroutine and non-deterministic timing to settle into a
// steady state.
func goldenForwardsModel(t *testing.T, width, height int) Model {
	t.Helper()
	active := kube.ForwardSession{
		ID:          "1",
		Target:      kube.ForwardTarget{Kind: kube.KindPod, Namespace: "nva-stage", Name: "nva-worker-9k2ss"},
		ResolvedPod: "nva-worker-9k2ss",
		LocalPort:   8080, RemotePort: 80,
		State:          kube.ForwardActive,
		StartedAt:      time.Now().Add(-41 * time.Minute),
		LastActivityAt: time.Now().Add(-12 * time.Minute),
	}
	reconnecting := kube.ForwardSession{
		ID:          "2",
		Target:      kube.ForwardTarget{Kind: kube.KindService, Namespace: "nva-prod", Name: "postgres"},
		ResolvedPod: "postgres-0",
		LocalPort:   5432, RemotePort: 5432,
		State:       kube.ForwardReconnecting,
		Err:         "pod restarted",
		Attempt:     2,
		NextRetryAt: time.Now().Add(4 * time.Second),
		StartedAt:   time.Now().Add(-2 * time.Hour),
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindForward: {
			&kube.ForwardObject{Session: active},
			&kube.ForwardObject{Session: reconnecting},
		},
	}}
	sess := newSession()
	sess.Location.Kind = kube.KindForward
	m := New(Config{Session: sess, Lister: lister})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	return m
}

// --- 14a: custom resource list (exemplar: a generic CRD) ---
//
// Certificate itself is no longer this exemplar as of §35b — it now gets
// its own curated Descriptor (resources/certmanager.go), so this scenario
// moved to the still-fully-generic Widget kind (discoveredWidgetDK) to keep
// pinning 14a's actual generic-CRD-list rendering rather than 35b's.

func goldenCRDInstancesModel(t *testing.T, width, height int) Model {
	t.Helper()
	reg, _ := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{discoveredWidgetDK()}, nil)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Widget"): {
			widgetInstance("first-widget", "nva-stage"),
			widgetInstance("second-widget", "nva-stage"),
		},
	}}
	sess := newSession()
	sess.Location.Namespace = "nva-stage"
	sess.Registry = reg
	sess.Location.Kind = kube.ResourceKind("Widget")
	m := New(Config{Session: sess, Lister: lister})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	return m
}

// --- 14b: CustomResourceDefinitions list ---

func goldenCRDDefRow(plural, group string, established bool) *unstructured.Unstructured {
	condStatus := "True"
	if !established {
		condStatus = "False"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": plural + "." + group},
		"spec": map[string]any{
			"group":    group,
			"names":    map[string]any{"kind": plural, "plural": plural},
			"scope":    "Namespaced",
			"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true}},
		},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Established", "status": condStatus}},
		},
	}}
}

func goldenCRDListModel(t *testing.T, width, height int) Model {
	t.Helper()
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCustomResourceDefinition: {
			goldenCRDDefRow("certificates", "cert-manager.io", true),
			goldenCRDDefRow("certificaterequests", "cert-manager.io", true),
			goldenCRDDefRow("httproutes", "gateway.networking.k8s.io", false),
		},
	}}
	sess := newSession()
	sess.Location.Kind = kube.KindCustomResourceDefinition
	m := New(Config{Session: sess, Lister: lister})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	return m
}

// --- 15a: loading a kind ---

func goldenLoadingModel(t *testing.T, width, height int) Model {
	t.Helper()
	m := New(Config{Session: newSession(), Lister: fakeLister{}})
	m.SetSize(width, height)
	return m
}

// --- 18a: Helm releases list ---

func goldenHelmModel(t *testing.T, width, height int) Model {
	t.Helper()
	// Mixed on purpose: one release the repo cache has a newer chart for
	// (deployed *and* outdated — the case the ▲ glyph, the LATEST cell and
	// the strip's cross-cutting count all have to render at once), one it
	// knows and finds current, and two it has never heard of.
	lister := chartStatusLister{
		fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
			kube.KindHelmRelease: {
				kube.NewHelmReleaseObject(kube.HelmRelease{
					Namespace: "nva-stage", Name: "postgresql", Chart: "postgresql", ChartVersion: "12.1.9",
					AppVersion: "15.4.0", Revision: 3, Status: "deployed",
				}.WithLatest("12.2.1", "bitnami", false)),
				kube.NewHelmReleaseObject(kube.HelmRelease{
					Namespace: "nva-stage", Name: "redis", Chart: "redis", ChartVersion: "18.1.5",
					AppVersion: "7.2.4", Revision: 2, Status: "deployed",
				}.WithLatest("18.1.5", "bitnami", false)),
				helmRelease("nva-stage", "nva-gateway", "kube-prometheus-stack", "58.2.1", "0.73.0", "pending-upgrade", 2),
				kube.NewHelmReleaseObject(kube.HelmRelease{
					Namespace: "nva-stage", Name: "broken-app", Chart: "mychart", ChartVersion: "1.0.0",
					AppVersion: "2.1.0", Revision: 2, Status: "failed", StatusReason: "hook timeout",
				}),
			},
		}},
		status: helmrepo.Status{Configured: true, Repos: 3, Charts: 40, Oldest: time.Now().Add(-6 * 24 * time.Hour)},
	}
	sess := newSession()
	sess.Location.Namespace = "nva-stage"
	sess.Location.Kind = kube.KindHelmRelease
	m := New(Config{Session: sess, Lister: lister, OpenHelmDetail: func(kube.HelmRelease, int, int) (tea.Model, tea.Cmd) { return stubTask{}, nil }})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	return m
}

// --- 30a: Flux reconcilers (Kustomizations) ---

// goldenFluxObject builds one Kustomization with deterministic ages.
//
// Both timestamps are relative to time.Now() because AGE and RECONCILED are
// both `time.Since` reads (resources/flux.go): an absolute fixture instant
// would render a different number every day the golden aged. The offsets are
// deliberately far from shortAge's rounding boundaries (12d, 3h) so a slow
// run can't tip a cell into the next unit mid-suite.
func goldenFluxObject(name string, ageDays int, suspend bool, conds ...map[string]any) *unstructured.Unstructured {
	created := time.Now().Add(-time.Duration(ageDays)*24*time.Hour - 7*time.Hour)
	spec := map[string]any{
		"interval":  "10m0s",
		"path":      "./clusters/nebula",
		"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"},
	}
	if suspend {
		spec["suspend"] = true
	}
	status := map[string]any{
		"lastAppliedRevision": "master@sha1:efd398bed98a38348c7702355ecd98fc11ac2bef",
	}
	if len(conds) > 0 {
		status["conditions"] = anySlice(conds)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kube.FluxGroupKustomize + "/v1",
		"kind":       "Kustomization",
		"metadata": map[string]any{
			"name": name, "namespace": "flux-system",
			"creationTimestamp": created.Format(time.RFC3339),
		},
		"spec":   spec,
		"status": status,
	}}
}

func anySlice(conds []map[string]any) []any {
	out := make([]any, 0, len(conds))
	for _, c := range conds {
		out = append(out, c)
	}
	return out
}

// goldenFluxCondition builds one status condition, transitioned hoursAgo.
func goldenFluxCondition(typ, status, message string, hoursAgo int) map[string]any {
	return map[string]any{
		"type": typ, "status": status, "message": message,
		"lastTransitionTime": time.Now().Add(-time.Duration(hoursAgo)*time.Hour - 20*time.Minute).Format(time.RFC3339),
	}
}

// goldenFluxModel renders §30a with one row per branch of its status
// precedence, which is the whole reason the kind is curated rather than
// generic: a failing reconciler (✕, message verbatim on the sub-line), one
// mid-reconcile (◌ — Ready=False with Reconciling=True is *progress*, and
// the generic descriptor's flat Ready read would paint it red), a suspended
// one carrying a stale Ready=True (‖ — the fixture that proves suspension
// outranks a frozen condition), and a healthy tail folded away behind them.
//
// The strip, the sub-lines, the fold and all three departures from the
// generic glyph set therefore land in one frame.
func goldenFluxModel(t *testing.T, width, height int) Model {
	t.Helper()
	reg, groups := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{kustomizationDK()}, nil)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Kustomization"): {
			goldenFluxObject("nebula-workers", 12, false,
				goldenFluxCondition("Ready", "False",
					"health check failed after 2m0s: Deployment/nebula-stage/nebula-worker status: 'InProgress'", 3)),
			goldenFluxObject("nebula-config", 40, false,
				goldenFluxCondition("Ready", "False", "Reconciliation in progress", 1),
				goldenFluxCondition("Reconciling", "True", "Building manifests", 1)),
			goldenFluxObject("nebula-infra", 40, true,
				goldenFluxCondition("Ready", "True", "Applied revision: master@sha1:efd398be", 9)),
			goldenFluxObject("flux-system", 40, false,
				goldenFluxCondition("Ready", "True", "Applied revision: master@sha1:efd398be", 2)),
			goldenFluxObject("nebula-apps", 12, false,
				goldenFluxCondition("Ready", "True", "Applied revision: master@sha1:efd398be", 2)),
			goldenFluxObject("observability", 8, false,
				goldenFluxCondition("Ready", "True", "Applied revision: master@sha1:efd398be", 5)),
		},
	}}
	sess := newSession()
	sess.Registry, sess.Groups = reg, groups
	sess.Location.Namespace = "flux-system"
	sess.Location.Kind = kube.ResourceKind("Kustomization")
	// A mutator is wired so the keybar renders §30a's own verb block
	// (r/s/o) — fluxVerbsApply gates it on one, and the suspend hint's
	// label flipping with the cursor row is part of what the fixture pins.
	m := New(Config{Session: sess, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	return m
}

// --- §35b: Certificates list ---

// goldenCertificateObject builds one Certificate with deterministic
// offsets — AGE, EXPIRES and RENEWAL are all time.Since/Until reads
// (resources/certmanager.go), so an absolute fixture instant would render a
// different number every day the golden aged, same reasoning as
// goldenFluxObject's own doc comment. A zero notAfterDays/renewalDays/
// lastFailureAgo omits that status field entirely, matching
// demoCertificate's own "not set yet" convention; the "+ time.Hour" buffers
// keep each duration solidly inside its intended day, clear of certExpiryCell's
// own floor-division boundary.
func goldenCertificateObject(name string, ageDays int, readyStatus string, notAfterDays, renewalDays int, lastFailureAgo time.Duration, issuer string) *unstructured.Unstructured {
	created := time.Now().Add(-time.Duration(ageDays)*24*time.Hour - 7*time.Hour)
	status := map[string]any{}
	if readyStatus != "" {
		status["conditions"] = []any{map[string]any{"type": "Ready", "status": readyStatus}}
	}
	if notAfterDays != 0 {
		status["notAfter"] = time.Now().Add(time.Duration(notAfterDays)*24*time.Hour + time.Hour).UTC().Format(time.RFC3339)
	}
	if renewalDays != 0 {
		status["renewalTime"] = time.Now().Add(time.Duration(renewalDays)*24*time.Hour + time.Hour).UTC().Format(time.RFC3339)
	}
	if lastFailureAgo != 0 {
		status["lastFailureTime"] = time.Now().Add(-lastFailureAgo).UTC().Format(time.RFC3339)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name": name, "namespace": "default",
			"creationTimestamp": created.UTC().Format(time.RFC3339),
		},
		"spec":   map[string]any{"issuerRef": map[string]any{"name": issuer}},
		"status": status,
	}}
}

// goldenCertificateModel renders §35b with one row per branch of
// projectCertificate's precedence: a real failure (✕, lastFailureTime set —
// web-tls), a first-attempt issuance (▲, Ready=False with no prior failure —
// new-svc-tls), a ready-but-expiring cert (◷, the glyph override that still
// counts and sorts as ready — admin-tls, whose renewalTime is already past,
// rendering RENEWAL as "due · auto"), and a comfortably-OK tail (api-tls,
// internal-ca — the latter exercising the EXPIRES "Ny" year format).
func goldenCertificateModel(t *testing.T, width, height int) Model {
	t.Helper()
	reg, groups := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{discoveredCertificateDK()}, nil)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Certificate"): {
			goldenCertificateObject("web-tls", 41, "False", 0, 0, 8*time.Minute, "letsencrypt-prod"),
			goldenCertificateObject("new-svc-tls", 0, "False", 0, 0, 0, "letsencrypt-prod"),
			goldenCertificateObject("admin-tls", 70, "True", 22, -8, 0, "letsencrypt-prod"),
			goldenCertificateObject("api-tls", 41, "True", 61, 31, 0, "letsencrypt-prod"),
			goldenCertificateObject("internal-ca", 90, "True", 8*365, 7*365, 0, "selfsigned"),
		},
	}}
	sess := newSession()
	sess.Registry, sess.Groups = reg, groups
	sess.Location.Namespace = "default"
	sess.Location.Kind = kube.ResourceKind("Certificate")
	m := New(Config{Session: sess, Lister: lister})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	return m
}

// --- 33a: Argo CD Applications ---

// goldenArgoObject builds one Application with deterministic ages — AGE and
// SYNCED are both time.Since reads (resources/argo.go), so an absolute
// fixture instant would render a different number every day the golden
// aged, same reasoning as goldenFluxObject's own doc comment.
func goldenArgoObject(name string, ageDays int, targetRevision, revision, sync, health string, syncedMinutesAgo int, resourceEntries ...map[string]any) *unstructured.Unstructured {
	created := time.Now().Add(-time.Duration(ageDays)*24*time.Hour - 7*time.Hour)
	status := map[string]any{
		"sync":   map[string]any{"status": sync, "revision": revision},
		"health": map[string]any{"status": health},
		"operationState": map[string]any{
			"finishedAt": time.Now().Add(-time.Duration(syncedMinutesAgo) * time.Minute).Format(time.RFC3339),
		},
	}
	if len(resourceEntries) > 0 {
		status["resources"] = anySlice(resourceEntries)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kube.ArgoGroup + "/v1alpha1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name": name, "namespace": "argocd",
			"creationTimestamp": created.Format(time.RFC3339),
		},
		"spec":   map[string]any{"source": map[string]any{"targetRevision": targetRevision}},
		"status": status,
	}}
}

// goldenArgoResource builds one status.resources[] entry for goldenArgoSubLineObject.
func goldenArgoResource(kind, name, health, message string) map[string]any {
	return map[string]any{
		"kind": kind, "name": name,
		"health": map[string]any{"status": health, "message": message},
	}
}

// goldenArgoModel renders §33a with one row per branch of its sort order —
// the whole reason the kind is curated rather than generic: a Degraded app
// with its sickest managed resource's message on the sub-line, an OutOfSync/
// Healthy drift case, a Syncing/Progressing in-flight case, and a healthy
// tail folded away behind them. The strip, the sub-line, the fold and the
// two-axis Sync/Health columns therefore land in one frame.
func goldenArgoModel(t *testing.T, width, height int) Model {
	t.Helper()
	reg, groups := resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{applicationDK()}, nil)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Application"): {
			goldenArgoObject("billing", 92, "main", "e41b90c1f2a3b4c5d6e7f8091a2b3c4d5e6f7081", "Synced", "Degraded", 11,
				goldenArgoResource("Deployment", "billing-api", "Degraded", `container "api" is in CrashLoopBackOff — exit 1, 2m ago`)),
			goldenArgoObject("frontend", 92, "main", "e41b90c1f2a3b4c5d6e7f8091a2b3c4d5e6f7081", "OutOfSync", "Healthy", 120),
			goldenArgoObject("search", 92, "main", "f77d215a8b9c0d1e2f30415263748596071829a0", "Syncing", "Progressing", 0),
			goldenArgoObject("api", 92, "main", "f77d215a8b9c0d1e2f30415263748596071829a0", "Synced", "Healthy", 18),
		},
	}}
	sess := newSession()
	sess.Registry, sess.Groups = reg, groups
	sess.Location.Namespace = "argocd"
	sess.Location.Kind = kube.ResourceKind("Application")
	// A mutator is wired so the keybar renders §33a's own verb block (r/S/u).
	m := New(Config{Session: sess, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	return m
}

// --- 8b: destructive-action confirm (ctrl-d delete) ---
//
// Both friction tiers render inline in this same package's Body()/keybar —
// TierInline never overrides Body (the table stays visible under the y/N
// keybar prompt), TierModal's components.TypeNameModal does cover it (the
// one red-bordered surface). Both are pinned here rather than in a separate
// component-level golden file since the confirming state only ever exists
// wired into a real task.

func goldenConfirmInlineModel(t *testing.T, width, height int) Model {
	t.Helper()
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("nva-stage", "api-0")},
	}}
	sess := newSession()
	sess.Location.Namespace = "nva-stage"
	m := New(Config{Session: sess, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	return m
}

func goldenConfirmModalModel(t *testing.T, width, height int) Model {
	t.Helper()
	grace := int64(30)
	podWithGrace := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "nva-worker-9k2ss", Namespace: "nva-stage"},
		Spec: corev1.PodSpec{
			Containers:                    []corev1.Container{{Name: "worker"}},
			TerminationGracePeriodSeconds: &grace,
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Ready: true}}},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {podWithGrace},
	}}
	sess := newSession()
	sess.Location.Namespace = "nva-stage"
	sess.Config = config.Config{ProdContexts: []string{sess.Location.Context}}
	m := New(Config{Session: sess, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	// Type a partial name so the modal's "N/M" progress indicator and
	// partial-match text both render, rather than an empty input row.
	for _, r := range "nva-worker" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	return m
}

// --- 20a: bulk operations (marked set) ---

func goldenMarksModel(t *testing.T, width, height int) Model {
	t.Helper()
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			pod("default", "api-0"),
			pod("default", "api-1"),
			pod("default", "worker-0"),
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	m = step(t, m, tea.KeyPressMsg{Text: "space"})
	m = step(t, m, tea.KeyPressMsg{Text: "space"})
	return m
}

// --- 24a: set-image inline editor ---

func goldenSetImageModel(t *testing.T, width, height int) Model {
	t.Helper()
	dep := twoContainerDeployment("default", "nva-worker", "registry.nva.dev/nva-worker:3.4.2")
	dep.Spec.Template.Spec.Containers = dep.Spec.Template.Spec.Containers[:1]
	restartAlways := corev1.ContainerRestartPolicyAlways
	dep.Spec.Template.Spec.InitContainers = []corev1.Container{
		{Name: "prepare", Image: "registry.nva.dev/prepare:1.2.0"},
		{Name: "mesh", Image: "registry.nva.dev/mesh:1.4.0", RestartPolicy: &restartAlways},
	}
	rsOldest := replicaSetRevision("default", "nva-worker-r41", "nva-worker", "registry.nva.dev/nva-worker:3.4.0", 41, 23*24*time.Hour)
	rsOld := replicaSetRevision("default", "nva-worker-r42", "nva-worker", "registry.nva.dev/nva-worker:3.4.1", 42, 21*24*time.Hour)
	rsCur := replicaSetRevision("default", "nva-worker-r43", "nva-worker", "registry.nva.dev/nva-worker:3.4.2", 43, 2*24*time.Hour)
	sighting := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "nva-worker", Namespace: "nva-prod", CreationTimestamp: setImageAge(40 * time.Minute)},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Image: "registry.nva.dev/nva-worker:3.4.3"}}},
		}},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {dep, sighting},
		kube.KindReplicaSet: {rsOldest, rsOld, rsCur},
	}}
	sess := newSession()
	sess.Location.Kind = kube.KindDeployment
	m := New(Config{Session: sess, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	return m
}

// --- 25a: resources inline editor ---

// goldenSetResourcesModel builds the exact scenario docs/design/
// v.0.2.0.dc.html's 25a mockup illustrates: a single-container "worker"
// Deployment whose live pod OOMKilled ~4m ago, cpu/mem request+limit
// matching the mockup's own CURRENT values (250m/1, 512Mi/512Mi), and live
// per-container usage that pins mem at its limit (Bad) while cpu stays
// comfortably under (neutral/Warn depending on the row) — exercising every
// bar-color state in one screenshot. The cursor is moved to the mem limit
// row and nudged up by 64Mi four times (512Mi -> 768Mi) to also exercise the
// edited-NEW-cell/will-run-line rendering, matching the mockup's own
// "768Mi" edited example.
func goldenSetResourcesModel(t *testing.T, width, height int) Model {
	t.Helper()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "nva-worker", Namespace: "default", Generation: 1, CreationTimestamp: setImageAge(30 * 24 * time.Hour)},
		Spec: appsv1.DeploymentSpec{Replicas: replicasPtr(4), Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "worker", Image: "registry.nva.dev/nva-worker:3.4.2", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
				}},
			}},
		}},
		Status: appsv1.DeploymentStatus{Replicas: 4, ReadyReplicas: 4, UpdatedReplicas: 4, AvailableReplicas: 4, ObservedGeneration: 1},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nva-worker-r43", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "nva-worker"}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nva-worker-r43-x8z2p", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs.Name}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "worker",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "OOMKilled", FinishedAt: setImageAge(4 * time.Minute),
				}},
			}},
		},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {dep},
		kube.KindReplicaSet: {rs},
		kube.KindPod:        {pod},
	}}
	metrics := fakeMetrics{containerMetrics: map[string]map[string]kube.PodMetrics{
		kube.PodKey(pod.Namespace, pod.Name): {"worker": {CPU: "182m", MEM: "512Mi", CPUMilli: 182, MemBytes: 512 * 1024 * 1024}},
	}}
	sess := newSession()
	sess.Location.Kind = kube.KindDeployment
	m := New(Config{Session: sess, Lister: lister, Metrics: metrics, Mutator: &fakeMutator{}})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	for range 4 {
		m = step(t, m, tea.KeyPressMsg{Text: "+"})
	}
	return m
}

// --- 26a: labels/annotations inline editor ---

// goldenMetaModel builds the exact scenario docs/design/v.0.2.0.dc.html's
// 26a mockup illustrates: a "deploy/nva-worker" Deployment in "nva-stage"
// whose labels/annotations match the mockup's own rows (app=nva-worker
// carrying the Service-selector join warning, env=stage, team=platform,
// app.kubernetes.io/managed-by=Helm carrying the Helm-owned note; annotations
// kute.dev/owner=platform-oncall and deployment.kubernetes.io/revision=42
// read-only), a svc/nva-worker Service whose selector matches app=nva-worker,
// and 4 Pods that Service currently selects (the mockup's "detaches 4 pods"
// figure). The cursor is moved to the env= row (rows sort alphabetically:
// app, app.kubernetes.io/managed-by, env, team) and "ing" is typed onto the
// prefilled "stage" buffer, landing on "staging" — matching the mockup's own
// mid-edit screenshot ("was stage · staging▎") without needing to clear the
// buffer first.
func goldenMetaModel(t *testing.T, width, height int) Model {
	t.Helper()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nva-worker", Namespace: "nva-stage",
			Labels: map[string]string{
				"app":                          "nva-worker",
				"env":                          "stage",
				"team":                         "platform",
				"app.kubernetes.io/managed-by": "Helm",
			},
			Annotations: map[string]string{
				"kute.dev/owner":                    "platform-oncall",
				"deployment.kubernetes.io/revision": "42",
			},
		},
		Spec: appsv1.DeploymentSpec{Replicas: replicasPtr(4), Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Image: "registry.nva.dev/nva-worker:3.4.2"}}},
		}},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "nva-worker", Namespace: "nva-stage"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "nva-worker"}},
	}
	pods := make([]runtime.Object, 0, 4)
	for i := range 4 {
		pods = append(pods, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "nva-worker-" + string(rune('a'+i)), Namespace: "nva-stage",
				Labels: map[string]string{"app": "nva-worker"},
			},
		})
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {dep},
		kube.KindService:    {svc},
		kube.KindPod:        pods,
	}}
	sess := newSession()
	sess.Location.Kind = kube.KindDeployment
	sess.Location.Namespace = "nva-stage"
	m := New(Config{Session: sess, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(width, height)
	m = step(t, m, m.load()())
	m = step(t, m, tea.KeyPressMsg{Text: "m"})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})     // navigation -> editing mode on env=
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace}) // "stage" -> "stag"
	for _, r := range "ing" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	return m
}

// goldenMetaConfirmModel drives goldenMetaModel's own scenario one step
// further: the app= row (the Service-selector-joined one) gets edited, and
// ↵ opens the TierInline confirm — pinning the "the panel stays open under
// the inline y/N instead of closing to the generic table+confirm view" fix
// (meta.go's own doc comment) at the pixel level: the row shows its
// about-to-apply value with a "confirm to apply · y/N" note, and the keybar
// carries the join-detach warning + will-run line, all with the panel still
// framing it.
func goldenMetaConfirmModel(t *testing.T, width, height int) Model {
	t.Helper()
	m := goldenMetaModel(t, width, height)
	m = step(t, m, tea.KeyPressMsg{Text: "esc"}) // back out of the in-progress env= edit
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyUp}) // up to app= (labels sort: app, app.kubernetes.io/managed-by, env, team)
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = step(t, m, tea.KeyPressMsg{Text: "2"}) // "nva-worker" -> "nva-worker2"
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	return m
}

// goldenStates maps each fixture-name prefix to its model builder — shared
// by the plain and (for the color-heaviest states) truecolor fixture maps
// below.
var goldenStatePrefixes = []string{
	"offline", "denied", "allns", "deployments", "empty", "nodes",
	"forwards", "crd-instances", "crd-list", "loading", "helm", "marks",
	"confirm-inline", "confirm-modal", "set-image", "set-resources", "meta", "meta-confirm",
	"flux", "argo", "certificates",
	"cronjobs", "cronjobs-failed", "cronjobs-suspended", "cronjobs-runnow",
	"cronjobs-suspend-confirm", "cronjobs-resume",
}

func goldenStateModel(t *testing.T, prefix string, width, height int) Model {
	t.Helper()
	switch prefix {
	case "offline":
		return goldenOfflineModel(t, width, height)
	case "denied":
		return goldenPermissionDeniedModel(t, width, height)
	case "allns":
		return goldenAllNSModel(t, width, height)
	case "deployments":
		return goldenDeploymentsModel(t, width, height)
	case "empty":
		return goldenEmptyModel(t, width, height)
	case "nodes":
		return goldenNodesModel(t, width, height)
	case "forwards":
		return goldenForwardsModel(t, width, height)
	case "crd-instances":
		return goldenCRDInstancesModel(t, width, height)
	case "crd-list":
		return goldenCRDListModel(t, width, height)
	case "loading":
		return goldenLoadingModel(t, width, height)
	case "helm":
		return goldenHelmModel(t, width, height)
	case "marks":
		return goldenMarksModel(t, width, height)
	case "confirm-inline":
		return goldenConfirmInlineModel(t, width, height)
	case "confirm-modal":
		return goldenConfirmModalModel(t, width, height)
	case "set-image":
		return goldenSetImageModel(t, width, height)
	case "set-resources":
		return goldenSetResourcesModel(t, width, height)
	case "meta":
		return goldenMetaModel(t, width, height)
	case "meta-confirm":
		return goldenMetaConfirmModel(t, width, height)
	case "flux":
		return goldenFluxModel(t, width, height)
	case "argo":
		return goldenArgoModel(t, width, height)
	case "certificates":
		return goldenCertificateModel(t, width, height)
	case "cronjobs":
		return goldenCronJobModel(t, width, height)
	case "cronjobs-failed":
		return goldenCronJobFailedModel(t, width, height)
	case "cronjobs-suspended":
		return goldenCronJobSuspendedModel(t, width, height)
	case "cronjobs-runnow":
		return goldenCronJobRunNowModel(t, width, height)
	case "cronjobs-suspend-confirm":
		return goldenCronJobSuspendConfirmModel(t, width, height)
	case "cronjobs-resume":
		return goldenCronJobResumeModel(t, width, height)
	default:
		t.Fatalf("unknown golden state prefix %q", prefix)
		return Model{}
	}
}

func goldenStateFixtures(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, prefix := range goldenStatePrefixes {
		out[prefix+"-120x36.golden"] = goldentest.Plain(goldenStateModel(t, prefix, 120, 36).Render())
		out[prefix+"-80x24.golden"] = goldentest.Plain(goldenStateModel(t, prefix, 80, 24).Render())
	}
	return out
}

func TestGenerateGoldenStateFixtures(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate browse golden fixtures")
	}
	for name, got := range goldenStateFixtures(t) {
		if err := os.WriteFile(filepath.Join(goldenStateDir(), name), []byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestGoldenStateFixtures(t *testing.T) {
	for name, got := range goldenStateFixtures(t) {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(goldenStateDir(), name)
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v", path, err)
			}
			if got != string(want) {
				t.Fatalf("golden mismatch for %s\nwant:\n%s\ngot:\n%s", name, string(want), got)
			}
		})
	}
}

// truecolorStatePrefixes are the states worth pinning at the per-cell color
// level — the ones with the richest status-color semantics (red/yellow/
// blue mixes, selection highlighting, mode pills). "empty"/"loading"/
// "crd-list"/"crd-instances" are left plain-only: mostly text, and their
// color tokens (health glyphs, selection bar) are already pinned by other
// truecolor fixtures in this package.
var truecolorStatePrefixes = []string{
	"offline", "denied", "allns", "deployments", "nodes", "forwards", "helm", "marks",
	"confirm-inline", "confirm-modal", "set-image", "set-resources", "meta", "meta-confirm",
	// §30a's status mapping is the entire justification for the curated
	// descriptor, and it is a colour claim: suspended must not read as
	// failed, reconciling must not read as failed. A plain golden renders
	// all three identically.
	"flux",
	// §33a's Sync/Health split is the same kind of colour claim: Degraded
	// must read red, OutOfSync amber, and quiet states secondary — a plain
	// golden can't tell any of them apart.
	"argo",
	// §35b's EXPIRES/RENEWAL coloring (yellow <30d, red not-ready/expired)
	// and the ready-but-expiring ◷ glyph override are colour claims too — a
	// plain golden can't distinguish "ready" from "ready but expiring".
	"certificates",
	// §36a/§36b/§36c's own colour claims: a suspended row's dim/strike name
	// and amber SUSP, a failed row's red LAST RUN + continuation line, the
	// WarnBanner-token overlap card, and the red-bordered PROD suspend
	// modal — a plain golden can't tell any of these apart from their
	// neutral/healthy counterparts (0.8.0 plan Phase 9 golden state 9).
	"cronjobs", "cronjobs-failed", "cronjobs-suspended", "cronjobs-runnow", "cronjobs-suspend-confirm",
}

func truecolorStateFixtures(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, prefix := range truecolorStatePrefixes {
		dark := goldenStateModel(t, prefix, 120, 36)
		light := goldenStateModel(t, prefix, 120, 36)
		light.session.Theme = tui.Light()
		out[prefix+"-120x36-dark.golden"] = goldentest.Truecolor(dark.Render())
		out[prefix+"-120x36-light.golden"] = goldentest.Truecolor(light.Render())
	}
	return out
}

func TestGenerateTruecolorStateFixtures(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate browse golden fixtures")
	}
	for name, got := range truecolorStateFixtures(t) {
		if err := os.WriteFile(filepath.Join(goldenStateDir(), name), []byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestTruecolorStateFixtures(t *testing.T) {
	for name, got := range truecolorStateFixtures(t) {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(goldenStateDir(), name)
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v", path, err)
			}
			if got != string(want) {
				t.Fatalf("golden mismatch for %s\nwant:\n%s\ngot:\n%s", name, string(want), got)
			}
		})
	}
}
