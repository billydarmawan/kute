// Package fake is a feature-complete in-memory implementation of every
// consumer seam the UI uses against *kube.Cluster (resources.RawLister,
// kube.Mutator, GetYAML, ObjectEvents, pod metrics, log streaming, the
// namespace/context Switcher, and connection health) — mvp-plan.md §0.7.
// It powers task tests (a whole cluster of fixtures beats ad hoc stubs) and
// the --demo flag. Rule: every new seam method on *kube.Cluster gets its
// counterpart here in the same change that adds it.
package fake

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/kute-dev/kute/internal/kube"
)

// cronJobGroupResource is CronJob's GroupResource for constructing the same
// typed Conflict error apierrors.IsConflict recognizes against a real
// cluster — Plan Phase 2 task 13's "conflicts are testable" parity
// requirement.
var cronJobGroupResource = schema.GroupResource{Group: "batch", Resource: "cronjobs"}

// secretGroupResource/configMapGroupResource are the same, for
// PatchSecretData/PatchConfigMapData's resourceVersion precondition.
var (
	secretGroupResource    = schema.GroupResource{Resource: "secrets"}
	configMapGroupResource = schema.GroupResource{Resource: "configmaps"}
)

// Cluster is the in-memory stand-in for *kube.Cluster. The zero value (via
// New) is an empty cluster; NewDemo seeds it with the fixtures --demo shows.
type Cluster struct {
	mu      sync.Mutex
	objects map[kube.ResourceKind][]runtime.Object
	logs    map[string][]string // "namespace/pod" or "namespace/pod/container" -> lines to stream

	// discovered is the fake counterpart of kube.Cluster's own discovery
	// cache (discovery.go/dynamic.go) — seeded directly via SeedDiscovered
	// rather than parsed from CRD objects, since --demo/tests already build
	// whatever shape they want by hand (fixtures.go's demoCRD family).
	discovered []kube.DiscoveredKind

	namespace           string
	context             string
	contexts            []string
	perContextNamespace map[string]string
	// userName/userGroups are the fake counterpart of kube.Cluster's
	// Context.UserName/UserGroups — the identity tasks/whocan (22a) pins as
	// "current user". Set via SetUserName/SetUserGroups; zero value unless
	// a fixture sets them.
	userName   string
	userGroups []string
	// reviewed is the fake counterpart of kube.Cluster's connect-time
	// SelfSubjectReview answer (ReviewedIdentity) — nil, i.e. "the server
	// didn't answer one", unless SetReviewedIdentity sets it.
	reviewed *kube.ReviewedIdentity

	// notSynced/kindSynced mirror *kube.Cluster's cache-sync reporting so a
	// test can drive a screen's loading/retry path through the real seam.
	// Inverted so the zero value reads as synced, which is what --demo and
	// every fixture that never touches these want. kindErrors/kindForbidden
	// are their KindError/KindForbidden counterparts. The fake has no
	// informers to actually scope (SetNamespaceScope has nothing to do here),
	// but the *health* these three answer for is still keyed per (kind,
	// namespace) via fakeScopeKey/scopeKeyFor, exactly like a real Cluster's
	// scopeKey — a UI caller that asks the wrong namespace for one kind's
	// cache must get a different answer than the right one, or a scoped-mode
	// test asserting "asks about the same namespace its own read used"
	// (CLAUDE.md) can never fail. Cluster-scoped kinds (Node, Namespace,
	// discovered cluster-scoped CRDs, …) still normalize every namespace
	// argument to "" via kube.ClusterScopedKind, matching cacheScopeLocked —
	// there is exactly one of those caches, real or fake.
	notSynced     bool
	kindSynced    map[fakeScopeKey]bool
	kindErrors    map[fakeScopeKey]error
	kindForbidden map[fakeScopeKey]error

	events chan kube.ResourceChangedMsg
	connCh chan kube.ConnStateMsg
	conn   kube.ConnState

	// cronJobRVSeq is the fake cluster's own resourceVersion source for
	// CronJob mutations (SetCronJobSuspend/SetCronJobSchedule), and for
	// PatchSecretData/PatchConfigMapData's precondition too — Plan Phase
	// 2 task 13's "enforce/increment resourceVersion on mutations". The
	// shared k8s.io/client-go/kubernetes/fake clientset (used to unit-test
	// *kube.Cluster itself, a different package) doesn't check this at all;
	// this app-level fake has no apiserver underneath it either, so it has
	// to implement the same optimistic-concurrency contract itself for
	// --demo and task-level UI tests to exercise Conflict responses.
	cronJobRVSeq int

	// tzCapability is the fake counterpart of *kube.Cluster's own
	// tzCapability (capability.go) — the tri-state tasks/cronjobschedule
	// (36d) gates timezone editing on. Zero value is
	// kube.TimeZoneCapabilityUnknown, same as a real Cluster before its
	// first probe; NewDemo's own fixtures.go sets Supported so --demo shows
	// a fully editable timezone field, and tests can set any of the three
	// via SetTimeZoneCapability to exercise the Unknown/Unsupported
	// read-only paths without a real cluster.
	tzCapability kube.TimeZoneCapability

	// evictionBlocked names the pods ("namespace/name") Drain must fail to
	// evict, the fake's stand-in for a PodDisruptionBudget refusing an
	// eviction — so a partial drain is expressible in tests and --demo.
	// forcedDeletes records every DeleteResourceForced call in order, so a
	// test can tell a force delete from a graceful one.
	evictionBlocked map[string]bool
	forcedDeletes   []string
}

// TimeZoneCapability returns the fake cluster's own tri-state answer — the
// same seam *kube.Cluster.TimeZoneCapability() provides, so
// tasks/cronjobschedule's package-local capability interface is satisfied
// by both without a type switch.
func (c *Cluster) TimeZoneCapability() kube.TimeZoneCapability {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tzCapability
}

// SetTimeZoneCapability sets the fake cluster's tri-state answer (fixtures/
// tests only — a real Cluster's own value comes solely from its one-shot
// Discovery().ServerVersion() probe, never a setter).
func (c *Cluster) SetTimeZoneCapability(tz kube.TimeZoneCapability) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tzCapability = tz
}

// New builds an empty fake cluster scoped to namespace/context.
func New(namespace, contextName string) *Cluster {
	return &Cluster{
		objects:             make(map[kube.ResourceKind][]runtime.Object),
		logs:                make(map[string][]string),
		namespace:           namespace,
		context:             contextName,
		contexts:            []string{contextName},
		perContextNamespace: map[string]string{contextName: namespace},
		events:              make(chan kube.ResourceChangedMsg, 64),
		connCh:              make(chan kube.ConnStateMsg, 8),
		conn:                kube.ConnState{Phase: kube.ConnConnected},
	}
}

// Seed adds objects of kind to the fake cluster (fixtures.go's NewDemo, and
// tests, build a cluster by seeding per kind).
func (c *Cluster) Seed(kind kube.ResourceKind, objs ...runtime.Object) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if kind == kube.KindSecret || kind == kube.KindConfigMap {
		// A real apiserver never hands out an object without a
		// resourceVersion, and PatchSecretData/PatchConfigMapData require
		// one as their precondition — stamp fixtures that left it blank.
		for _, obj := range objs {
			if accessor, err := apimeta.Accessor(obj); err == nil && accessor.GetResourceVersion() == "" {
				accessor.SetResourceVersion("1")
			}
		}
	}
	c.objects[kind] = append(c.objects[kind], objs...)
}

// SeedLogs registers the lines StreamPodLogs replays for every container of
// namespace/pod that SeedContainerLogs doesn't override.
func (c *Cluster) SeedLogs(namespace, pod string, lines []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs[namespace+"/"+pod] = lines
}

// SeedContainerLogs registers the lines StreamPodLogs replays for one
// container of namespace/pod, so a multi-container pod's containers each
// show their own logs.
func (c *Cluster) SeedContainerLogs(namespace, pod, container string, lines []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs[namespace+"/"+pod+"/"+container] = lines
}

// --- resources.RawLister ---

func (c *Cluster) ListRaw(_ context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Every object is handed out as a deep copy, as an informer cache's
	// reader would never share its storage with a mutator: the fake's
	// mutators write in place under c.mu, and a reader holding the stored
	// pointer would race them outside it.
	all := c.objects[kind]
	out := make([]runtime.Object, 0, len(all))
	for _, obj := range all {
		if namespace != "" {
			accessor, err := apimeta.Accessor(obj)
			if err != nil || accessor.GetNamespace() != namespace {
				continue
			}
		}
		out = append(out, obj.DeepCopyObject())
	}
	return out, nil
}

// CountLive mirrors *kube.Cluster.CountLive: a count with no informer behind
// it, which for the fake means counting the seeded map. Without it, --demo and
// every test built on the fake took the palette's cache-count fallback and the
// production LiveCounter path ran nowhere outside a real cluster. Like the
// real one it ignores namespace for a cluster-scoped kind.
func (c *Cluster) CountLive(_ context.Context, kind kube.ResourceKind, namespace string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	namespace = c.scopeKeyFor(kind, namespace).namespace
	n := 0
	for _, obj := range c.objects[kind] {
		if namespace != "" {
			accessor, err := apimeta.Accessor(obj)
			if err != nil || accessor.GetNamespace() != namespace {
				continue
			}
		}
		n++
	}
	return n, nil
}

// Synced mirrors *kube.Cluster.Synced. The fake's objects are seeded
// synchronously, so it reports connected unless a test says otherwise.
func (c *Cluster) Synced() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.notSynced
}

// fakeScopeKey mirrors *kube.Cluster's own (kind, namespace) health key —
// SetKindSynced(Pod, "team-a", …) must not also answer for
// KindSynced(Pod, "team-b") or KindSynced(Pod, "").
type fakeScopeKey struct {
	kind      kube.ResourceKind
	namespace string
}

// scopeKeyFor normalizes namespace for kind the same way *kube.Cluster's
// cacheScopeLocked does: a cluster-scoped kind (Node, Namespace, the CRD
// list, any discovered cluster-scoped CRD) always answers at "" regardless
// of what's passed, since there is exactly one such cache. c.discovered is
// only appended to (SeedDiscovered), never mutated in place, so reading it
// without c.mu here is safe; every caller already holds it anyway.
func (c *Cluster) scopeKeyFor(kind kube.ResourceKind, namespace string) fakeScopeKey {
	if kube.ClusterScopedKind(kind, c.discovered) {
		return fakeScopeKey{kind, ""}
	}
	return fakeScopeKey{kind, namespace}
}

// KindSynced mirrors *kube.Cluster.KindSynced. Every kind reads as synced
// unless a test gates one with SetKindSynced — which is the seam that lets a
// test drive a screen's loading/retry path through the real interface rather
// than a bespoke per-package lister double. Keyed per (kind, namespace) via
// scopeKeyFor, so a test can gate one namespace's cache without also gating
// another's — see the field comment on kindSynced.
func (c *Cluster) KindSynced(kind kube.ResourceKind, namespace string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if synced, ok := c.kindSynced[c.scopeKeyFor(kind, namespace)]; ok {
		return synced
	}
	return !c.notSynced
}

// KindStarted mirrors *kube.Cluster.KindStarted. The fake has no informers
// to start — every read is of an already-seeded in-memory map — so every
// read is free.
func (c *Cluster) KindStarted(kube.ResourceKind, string) bool { return true }

// ListHelmReleaseSecrets mirrors *kube.Cluster's filtered release cache. The
// real one is a separate, server-side-filtered Secret informer; the fake has
// one seeded map, so it filters by type here — same answer, and it keeps the
// Helm screens exercising the narrow path in --demo rather than the
// read-every-Secret fallback.
func (c *Cluster) ListHelmReleaseSecrets(_ context.Context, namespace string) ([]runtime.Object, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []runtime.Object
	for _, obj := range c.objects[kube.KindSecret] {
		secret, ok := obj.(*corev1.Secret)
		if !ok || secret.Type != kube.HelmReleaseSecretType {
			continue
		}
		if namespace != "" && secret.Namespace != namespace {
			continue
		}
		out = append(out, obj)
	}
	return out, nil
}

// ListTLSSecrets mirrors *kube.Cluster's filtered kubernetes.io/tls cache the
// way ListHelmReleaseSecrets mirrors the release one: filter the one seeded
// map by type, so --demo's TLS cells and cert chains exercise the narrow path.
func (c *Cluster) ListTLSSecrets(_ context.Context, namespace string) ([]runtime.Object, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []runtime.Object
	for _, obj := range c.objects[kube.KindSecret] {
		secret, ok := obj.(*corev1.Secret)
		if !ok || secret.Type != corev1.SecretTypeTLS {
			continue
		}
		if namespace != "" && secret.Namespace != namespace {
			continue
		}
		out = append(out, obj)
	}
	return out, nil
}

// SetSynced overrides what Synced reports.
func (c *Cluster) SetSynced(synced bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notSynced = !synced
}

// SetKindSynced overrides what KindSynced reports for one (kind, namespace)
// scope, leaving every other kind — and every other namespace of this one —
// alone. namespace is normalized through scopeKeyFor first, so gating a
// cluster-scoped kind always gates its one "" cache regardless of what's
// passed here.
func (c *Cluster) SetKindSynced(kind kube.ResourceKind, namespace string, synced bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kindSynced == nil {
		c.kindSynced = map[fakeScopeKey]bool{}
	}
	c.kindSynced[c.scopeKeyFor(kind, namespace)] = synced
}

// KindError mirrors *kube.Cluster.KindError. nil unless a test says
// otherwise via SetKindError, keyed per (kind, namespace) via scopeKeyFor —
// see KindSynced.
func (c *Cluster) KindError(kind kube.ResourceKind, namespace string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.kindErrors[c.scopeKeyFor(kind, namespace)]
}

// SetKindError overrides what KindError reports for one (kind, namespace)
// scope — the seam that lets a test drive a screen's "cache stopped filling"
// path through the real interface.
func (c *Cluster) SetKindError(kind kube.ResourceKind, namespace string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kindErrors == nil {
		c.kindErrors = map[fakeScopeKey]error{}
	}
	c.kindErrors[c.scopeKeyFor(kind, namespace)] = err
}

// KindForbidden mirrors *kube.Cluster.KindForbidden. nil unless a test says
// otherwise via SetKindForbidden, keyed per (kind, namespace) via
// scopeKeyFor — see KindSynced.
func (c *Cluster) KindForbidden(kind kube.ResourceKind, namespace string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.kindForbidden[c.scopeKeyFor(kind, namespace)]
}

// SetKindForbidden overrides what KindForbidden reports for one (kind,
// namespace) scope — the seam that lets a test drive a screen's
// permission-denied path (4b's card) through the real interface rather than
// a bespoke per-package double.
func (c *Cluster) SetKindForbidden(kind kube.ResourceKind, namespace string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kindForbidden == nil {
		c.kindForbidden = map[fakeScopeKey]error{}
	}
	c.kindForbidden[c.scopeKeyFor(kind, namespace)] = err
}

// --- kube.Mutator ---

func (c *Cluster) DeleteResource(_ context.Context, kind kube.ResourceKind, namespace, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	objs := c.objects[kind]
	for i, obj := range objs {
		accessor, err := apimeta.Accessor(obj)
		if err != nil {
			continue
		}
		if accessor.GetName() == name && (namespace == "" || accessor.GetNamespace() == namespace) {
			c.objects[kind] = append(objs[:i:i], objs[i+1:]...)
			c.notify(kind)
			return nil
		}
	}
	return fmt.Errorf("%s %q not found", kind, name)
}

func (c *Cluster) DeleteResourceForced(ctx context.Context, kind kube.ResourceKind, namespace, name string) error {
	if err := c.DeleteResource(ctx, kind, namespace, name); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forcedDeletes = append(c.forcedDeletes, string(kind)+" "+namespace+"/"+name)
	return nil
}

// ForcedDeletes reports every successful DeleteResourceForced call, in
// order, as "<kind> <namespace>/<name>" — the only way a test can tell a
// force delete from DeleteResource, since both remove the object.
func (c *Cluster) ForcedDeletes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.forcedDeletes)
}

// RolloutRestart stamps kind's pod template with a fresh restartedAt
// annotation in place — Deployment, StatefulSet, and DaemonSet all carry a
// pod template, so all three are supported (27a's ctrl-r restarts whichever
// kind a ConfigMap's consumer happens to be).
func (c *Cluster) RolloutRestart(_ context.Context, kind kube.ResourceKind, namespace, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, obj := range c.objects[kind] {
		var tpl *metav1.ObjectMeta
		switch o := obj.(type) {
		case *appsv1.Deployment:
			if o.Name != name || o.Namespace != namespace {
				continue
			}
			tpl = &o.Spec.Template.ObjectMeta
		case *appsv1.StatefulSet:
			if o.Name != name || o.Namespace != namespace {
				continue
			}
			tpl = &o.Spec.Template.ObjectMeta
		case *appsv1.DaemonSet:
			if o.Name != name || o.Namespace != namespace {
				continue
			}
			tpl = &o.Spec.Template.ObjectMeta
		default:
			continue
		}
		if tpl.Annotations == nil {
			tpl.Annotations = map[string]string{}
		}
		tpl.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)
		c.notify(kind)
		return nil
	}
	return fmt.Errorf("%s %q not found", kind, name)
}

// RetryJob is RolloutRestart's shape applied to kube.Cluster's own RetryJob:
// find the source Job, clone it via the same kube.CloneJobSpec field-
// stripping the real implementation uses (so the two never drift), and
// append the clone rather than mutating anything — the source Job is left
// untouched.
func (c *Cluster) RetryJob(_ context.Context, namespace, name, newName, creator string, at time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, obj := range c.objects[kube.KindJob] {
		job, ok := obj.(*batchv1.Job)
		if !ok || job.Name != name || job.Namespace != namespace {
			continue
		}
		annotations := maps.Clone(job.Annotations)
		if annotations == nil {
			annotations = map[string]string{}
		}
		// Mirrors kube.Cluster.RetryJob: a rerun is always standalone, never
		// carrying over a source CronJob's association or Helm hook identity.
		kube.StripRerunSourceAnnotations(annotations)
		annotations[kube.AnnotationTriggeredBy] = creator
		annotations[kube.AnnotationTriggeredAt] = at.UTC().Format(time.RFC3339)
		clone := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:        newName,
				Namespace:   namespace,
				Labels:      maps.Clone(job.Labels),
				Annotations: annotations,
			},
			Spec: *kube.CloneJobSpec(&job.Spec),
		}
		c.objects[kube.KindJob] = append(c.objects[kube.KindJob], clone)
		c.notify(kube.KindJob)
		return nil
	}
	return fmt.Errorf("%s %q not found", kube.KindJob, name)
}

// ReplaceJob mirrors kube.Cluster.ReplaceJob: find, remove, append a
// same-name replacement built from the removed Job's own spec — keeping
// OwnerReferences/annotations, unlike RetryJob's deliberately-detached
// clone.
func (c *Cluster) ReplaceJob(_ context.Context, namespace, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	objs := c.objects[kube.KindJob]
	for i, obj := range objs {
		job, ok := obj.(*batchv1.Job)
		if !ok || job.Name != name || job.Namespace != namespace {
			continue
		}
		replacement := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       namespace,
				Labels:          maps.Clone(job.Labels),
				Annotations:     maps.Clone(job.Annotations),
				OwnerReferences: job.OwnerReferences,
			},
			Spec: *kube.CloneJobSpec(&job.Spec),
		}
		c.objects[kube.KindJob] = append(append(objs[:i:i], objs[i+1:]...), replacement)
		c.notify(kube.KindJob)
		return nil
	}
	return fmt.Errorf("%s %q not found", kube.KindJob, name)
}

// SetJobSuspend patches spec.suspend on a Job in place.
func (c *Cluster) SetJobSuspend(_ context.Context, namespace, name string, suspend bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, obj := range c.objects[kube.KindJob] {
		job, ok := obj.(*batchv1.Job)
		if !ok || job.Name != name || job.Namespace != namespace {
			continue
		}
		s := suspend
		job.Spec.Suspend = &s
		c.notify(kube.KindJob)
		return nil
	}
	return fmt.Errorf("%s %q not found", kube.KindJob, name)
}

// TriggerCronJob is RetryJob's shape applied to a CronJob's own jobTemplate:
// find the source CronJob, build a new standalone Job from its template,
// stamp Kute's manual-run annotations, and append it to KindJob's own object
// set — the source CronJob is left untouched. Matches
// kube.Cluster.TriggerCronJob's own annotation set and last-applied
// cleanup, and rejects a name already in use (via
// kube.ErrManualJobNameConflict) the same way a real Create call would with
// AlreadyExists — Plan Phase 2 task 13/14.
func (c *Cluster) TriggerCronJob(_ context.Context, namespace, name, newJobName, creator string, at time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, obj := range c.objects[kube.KindJob] {
		if job, ok := obj.(*batchv1.Job); ok && job.Name == newJobName && job.Namespace == namespace {
			return fmt.Errorf("%w: job %q already exists in namespace %q", kube.ErrManualJobNameConflict, newJobName, namespace)
		}
	}
	for _, obj := range c.objects[kube.KindCronJob] {
		cj, ok := obj.(*batchv1.CronJob)
		if !ok || cj.Name != name || cj.Namespace != namespace {
			continue
		}
		tpl := cj.Spec.JobTemplate
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:        newJobName,
				Namespace:   namespace,
				Labels:      maps.Clone(tpl.Labels),
				Annotations: maps.Clone(tpl.Annotations),
			},
			Spec: *tpl.Spec.DeepCopy(),
		}
		if job.Annotations == nil {
			job.Annotations = map[string]string{}
		}
		job.Annotations["cronjob.kubernetes.io/instantiate"] = "manual"
		job.Annotations[kube.AnnotationCronJobName] = cj.Name
		job.Annotations[kube.AnnotationCronJobUID] = string(cj.UID)
		job.Annotations[kube.AnnotationTriggeredBy] = creator
		job.Annotations[kube.AnnotationTriggeredAt] = at.UTC().Format(time.RFC3339)
		delete(job.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
		c.objects[kube.KindJob] = append(c.objects[kube.KindJob], job)
		c.notify(kube.KindJob)
		return nil
	}
	return fmt.Errorf("%s %q not found", kube.KindCronJob, name)
}

// checkCronJobResourceVersion is SetCronJobSuspend/SetCronJobSchedule's
// shared optimistic-concurrency check: resourceVersion is required, and a
// mismatch against cj's own current value returns a typed Conflict error
// (apierrors.IsConflict) rather than a plain fmt.Errorf, so a caller can
// tell a stale-precondition failure apart from any other error the same way
// it would against a real apiserver. Must be called with c.mu already held.
func checkCronJobResourceVersion(cj *batchv1.CronJob, resourceVersion string) error {
	if resourceVersion == "" {
		return fmt.Errorf("cannot patch cronjob %q: missing resourceVersion precondition", cj.Name)
	}
	if cj.ResourceVersion != resourceVersion {
		return apierrors.NewConflict(cronJobGroupResource, cj.Name,
			fmt.Errorf("the object has been modified; please apply your changes to the latest version and try again"))
	}
	return nil
}

// bumpCronJob advances cj's resourceVersion and generation together — the
// same "any write changes both" behavior a real apiserver guarantees, and
// what makes the resourceVersion precondition check above meaningful across
// repeated calls (Plan Phase 2 task 13). The next resourceVersion is always
// strictly greater than both the cluster's own running counter and cj's
// current numeric resourceVersion (when it parses as one, which every
// fixture/test in this repo seeds), so a caller-chosen initial seed value
// (e.g. "1") can never coincidentally collide with the sequence's own early
// numbers. Must be called with c.mu already held.
func (c *Cluster) bumpCronJob(cj *batchv1.CronJob) {
	next := c.cronJobRVSeq + 1
	if n, err := strconv.Atoi(cj.ResourceVersion); err == nil && n >= next {
		next = n + 1
	}
	c.cronJobRVSeq = next
	cj.ResourceVersion = strconv.Itoa(next)
	cj.Generation++
}

// checkAndBumpResourceVersion is PatchSecretData/PatchConfigMapData's
// counterpart of checkCronJobResourceVersion + bumpCronJob: resourceVersion
// is required, a mismatch is a typed Conflict, and a match advances the
// object's resourceVersion off the same counter so a second write against
// the first one's precondition conflicts. Must be called with c.mu held.
func (c *Cluster) checkAndBumpResourceVersion(meta *metav1.ObjectMeta, gr schema.GroupResource, resourceVersion string) error {
	if resourceVersion == "" {
		return fmt.Errorf("cannot patch %s %q: missing resourceVersion precondition", gr.Resource, meta.Name)
	}
	if meta.ResourceVersion != resourceVersion {
		return apierrors.NewConflict(gr, meta.Name,
			fmt.Errorf("the object has been modified; please apply your changes to the latest version and try again"))
	}
	next := c.cronJobRVSeq + 1
	if n, err := strconv.Atoi(meta.ResourceVersion); err == nil && n >= next {
		next = n + 1
	}
	c.cronJobRVSeq = next
	meta.ResourceVersion = strconv.Itoa(next)
	return nil
}

// SetCronJobSuspend patches spec.suspend on a CronJob in place, stamping/
// clearing Kute's own suspend-timestamp annotations atomically — matches
// kube.Cluster.SetCronJobSuspend's own contract (Plan Phase 2 task 13).
func (c *Cluster) SetCronJobSuspend(_ context.Context, namespace, name string, suspend bool, resourceVersion string, currentGeneration int64, at time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, obj := range c.objects[kube.KindCronJob] {
		cj, ok := obj.(*batchv1.CronJob)
		if !ok || cj.Name != name || cj.Namespace != namespace {
			continue
		}
		if err := checkCronJobResourceVersion(cj, resourceVersion); err != nil {
			return err
		}
		s := suspend
		cj.Spec.Suspend = &s
		if cj.Annotations == nil {
			cj.Annotations = map[string]string{}
		}
		if suspend {
			cj.Annotations[kube.AnnotationSuspendedAt] = at.UTC().Format(time.RFC3339)
			cj.Annotations[kube.AnnotationSuspendedGeneration] = strconv.FormatInt(currentGeneration+1, 10)
		} else {
			delete(cj.Annotations, kube.AnnotationSuspendedAt)
			delete(cj.Annotations, kube.AnnotationSuspendedGeneration)
		}
		c.bumpCronJob(cj)
		c.notify(kube.KindCronJob)
		return nil
	}
	return fmt.Errorf("%s %q not found", kube.KindCronJob, name)
}

// SetCronJobSchedule patches spec.schedule (and optionally spec.timeZone) on
// a CronJob in place, matching kube.Cluster.SetCronJobSchedule's set/clear/
// untouched timezone contract and resourceVersion precondition, and
// returning the same kube.CronJobScheduleResult shape (Plan Phase 2 task
// 13).
func (c *Cluster) SetCronJobSchedule(_ context.Context, namespace, name string, edit kube.CronJobScheduleEdit) (kube.CronJobScheduleResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, obj := range c.objects[kube.KindCronJob] {
		cj, ok := obj.(*batchv1.CronJob)
		if !ok || cj.Name != name || cj.Namespace != namespace {
			continue
		}
		if err := checkCronJobResourceVersion(cj, edit.ResourceVersion); err != nil {
			return kube.CronJobScheduleResult{}, err
		}
		cj.Spec.Schedule = edit.Schedule
		if edit.TimeZone != nil {
			if *edit.TimeZone == "" {
				cj.Spec.TimeZone = nil
			} else {
				tz := *edit.TimeZone
				cj.Spec.TimeZone = &tz
			}
		}
		c.bumpCronJob(cj)
		c.notify(kube.KindCronJob)
		result := kube.CronJobScheduleResult{Schedule: cj.Spec.Schedule, ResourceVersion: cj.ResourceVersion}
		if cj.Spec.TimeZone != nil {
			result.TimeZone = *cj.Spec.TimeZone
		}
		return result, nil
	}
	return kube.CronJobScheduleResult{}, fmt.Errorf("%s %q not found", kube.KindCronJob, name)
}

func (c *Cluster) Cordon(_ context.Context, node string, cordon bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, obj := range c.objects[kube.KindNode] {
		n, ok := obj.(*corev1.Node)
		if !ok || n.Name != node {
			continue
		}
		n.Spec.Unschedulable = cordon
		c.notify(kube.KindNode)
		return nil
	}
	return fmt.Errorf("node %q not found", node)
}

func (c *Cluster) Drain(ctx context.Context, node string) (int, error) {
	if err := c.Cordon(ctx, node, true); err != nil {
		return 0, err
	}
	c.mu.Lock()
	pods := append([]runtime.Object(nil), c.objects[kube.KindPod]...)
	c.mu.Unlock()

	evicted := 0
	var errs []error
	for _, obj := range pods {
		pod, ok := obj.(*corev1.Pod)
		if !ok || pod.Spec.NodeName != node {
			continue
		}
		if !kube.DrainEvicts(pod) {
			continue
		}
		// Joined per pod exactly as kube.Cluster.Drain does, so a partial
		// drain reaches the UI's error path against the fake too.
		if err := c.evict(ctx, pod.Namespace, pod.Name); err != nil {
			errs = append(errs, fmt.Errorf("evict %s/%s: %w", pod.Namespace, pod.Name, err))
			continue
		}
		evicted++
	}
	return evicted, errors.Join(errs...)
}

func (c *Cluster) evict(ctx context.Context, namespace, name string) error {
	c.mu.Lock()
	blocked := c.evictionBlocked[namespace+"/"+name]
	c.mu.Unlock()
	if blocked {
		return apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 0)
	}
	return c.DeleteResource(ctx, kube.KindPod, namespace, name)
}

// BlockEviction makes Drain fail to evict namespace/name with the 429 a
// PodDisruptionBudget answers, leaving the pod in place (tests and --demo
// only — a real Cluster's refusals come from the API server).
func (c *Cluster) BlockEviction(namespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.evictionBlocked == nil {
		c.evictionBlocked = map[string]bool{}
	}
	c.evictionBlocked[namespace+"/"+name] = true
}

// Scale sets Deployment/StatefulSet spec.Replicas in place — 17b's +/−
// inline prompt against the fake cluster.
func (c *Cluster) Scale(_ context.Context, kind kube.ResourceKind, namespace, name string, replicas int32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, obj := range c.objects[kind] {
		switch o := obj.(type) {
		case *appsv1.Deployment:
			if o.Name != name || o.Namespace != namespace {
				continue
			}
			o.Spec.Replicas = &replicas
			c.notify(kind)
			return nil
		case *appsv1.StatefulSet:
			if o.Name != name || o.Namespace != namespace {
				continue
			}
			o.Spec.Replicas = &replicas
			c.notify(kind)
			return nil
		}
	}
	return fmt.Errorf("%s %q not found", kind, name)
}

// SetImage sets one named container's image on an apps workload or CronJob
// pod template in place — 24a's tag-first inline editor against the fake
// cluster.
func (c *Cluster) SetImage(_ context.Context, kind kube.ResourceKind, namespace, name, container, image string, initContainer bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, obj := range c.objects[kind] {
		var spec *corev1.PodSpec
		switch o := obj.(type) {
		case *appsv1.Deployment:
			if o.Name != name || o.Namespace != namespace {
				continue
			}
			spec = &o.Spec.Template.Spec
		case *appsv1.StatefulSet:
			if o.Name != name || o.Namespace != namespace {
				continue
			}
			spec = &o.Spec.Template.Spec
		case *appsv1.DaemonSet:
			if o.Name != name || o.Namespace != namespace {
				continue
			}
			spec = &o.Spec.Template.Spec
		case *batchv1.CronJob:
			if o.Name != name || o.Namespace != namespace {
				continue
			}
			spec = &o.Spec.JobTemplate.Spec.Template.Spec
		default:
			continue
		}
		containers := spec.Containers
		if initContainer {
			containers = spec.InitContainers
		}
		for i := range containers {
			if containers[i].Name == container {
				containers[i].Image = image
				c.notify(kind)
				return nil
			}
		}
		return fmt.Errorf("container %q not found on %s %q", container, kind, name)
	}
	return fmt.Errorf("%s %q not found", kind, name)
}

// SetResources sets one named container's resources.requests/limits on
// Deployment/StatefulSet/DaemonSet's pod template in place — 25a's editor
// against the fake cluster. dryRun is accepted for interface parity but
// otherwise ignored: the fake tracker performs no admission, so there's
// nothing a dry-run would catch here that a real apply wouldn't (unlike a
// real cluster, kube/fake never returns a LimitRange/ResourceQuota
// rejection — see kube.Cluster.SetResources's doc comment).
func (c *Cluster) SetResources(_ context.Context, kind kube.ResourceKind, namespace, name, container string, edits kube.ResourceEdits, _ bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, obj := range c.objects[kind] {
		var containers []corev1.Container
		switch o := obj.(type) {
		case *appsv1.Deployment:
			if o.Name != name || o.Namespace != namespace {
				continue
			}
			containers = o.Spec.Template.Spec.Containers
		case *appsv1.StatefulSet:
			if o.Name != name || o.Namespace != namespace {
				continue
			}
			containers = o.Spec.Template.Spec.Containers
		case *appsv1.DaemonSet:
			if o.Name != name || o.Namespace != namespace {
				continue
			}
			containers = o.Spec.Template.Spec.Containers
		default:
			continue
		}
		for i := range containers {
			if containers[i].Name != container {
				continue
			}
			if err := applyResourceEdits(&containers[i], edits); err != nil {
				return err
			}
			c.notify(kind)
			return nil
		}
		return fmt.Errorf("container %q not found on %s %q", container, kind, name)
	}
	return fmt.Errorf("%s %q not found", kind, name)
}

// PatchMeta sets or removes a single label/annotation key in place — 26a's
// editor against the fake cluster. Unlike the real Cluster.PatchMeta (which
// must pick a typed client per kind, or fall back to the dynamic client, to
// issue a wire patch) this mutates the already-materialized object directly,
// so apimeta.Accessor's generic GetLabels/SetLabels(orAnnotations) covers
// every kind — including a discovered CRD's *unstructured.Unstructured —
// with no per-kind switch at all.
func (c *Cluster) PatchMeta(_ context.Context, kind kube.ResourceKind, namespace, name string, isAnnotation bool, key, value string, remove bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, obj := range c.objects[kind] {
		acc, err := apimeta.Accessor(obj)
		if err != nil {
			continue
		}
		if acc.GetName() != name || (namespace != "" && acc.GetNamespace() != namespace) {
			continue
		}
		m := acc.GetLabels()
		if isAnnotation {
			m = acc.GetAnnotations()
		}
		if remove {
			delete(m, key)
		} else {
			if m == nil {
				m = map[string]string{}
			}
			m[key] = value
		}
		if isAnnotation {
			acc.SetAnnotations(m)
		} else {
			acc.SetLabels(m)
		}
		c.notify(kind)
		return nil
	}
	return fmt.Errorf("%s %q not found", kind, name)
}

// SetFluxSuspend flips spec.suspend on a seeded Flux object in place —
// §30a's 's' against the fake cluster.
func (c *Cluster) SetFluxSuspend(_ context.Context, kind kube.ResourceKind, namespace, name string, suspend bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, err := c.findUnstructuredLocked(kind, namespace, name)
	if err != nil {
		return err
	}
	if err := unstructured.SetNestedField(u.Object, suspend, "spec", "suspend"); err != nil {
		return err
	}
	c.notify(kind)
	return nil
}

// RequestFluxReconcile stamps the reconcile annotation on a seeded Flux
// object — §30a's 'r'. Goes through PatchMeta for the same reason the real
// cluster does: the request is an ordinary metadata write.
func (c *Cluster) RequestFluxReconcile(ctx context.Context, kind kube.ResourceKind, namespace, name string) error {
	return c.PatchMeta(ctx, kind, namespace, name, true,
		kube.FluxReconcileAnnotation, time.Now().UTC().Format(time.RFC3339), false)
}

// RequestArgoRefresh stamps the refresh annotation on a seeded Argo CD
// Application — §33a's 'r'. Goes through PatchMeta for the same reason
// RequestFluxReconcile does: the request is an ordinary metadata write.
func (c *Cluster) RequestArgoRefresh(ctx context.Context, kind kube.ResourceKind, namespace, name string) error {
	return c.PatchMeta(ctx, kind, namespace, name, true, kube.ArgoRefreshAnnotation, "normal", false)
}

// RequestArgoSync sets operation.sync.revision and operation.initiatedBy on
// a seeded Argo CD Application in place — §33a's 'S' against the fake
// cluster, the same two fields the real merge patch writes.
func (c *Cluster) RequestArgoSync(_ context.Context, kind kube.ResourceKind, namespace, name, revision, initiator string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, err := c.findUnstructuredLocked(kind, namespace, name)
	if err != nil {
		return err
	}
	if err := unstructured.SetNestedField(u.Object, revision, "operation", "sync", "revision"); err != nil {
		return err
	}
	if initiator == "" {
		initiator = kube.DefaultArgoSyncInitiator
	}
	if err := unstructured.SetNestedMap(u.Object, map[string]any{"username": initiator, "automated": false}, "operation", "initiatedBy"); err != nil {
		return err
	}
	c.notify(kind)
	return nil
}

// RenewCertificate simulates §35c's 'r' against a seeded Certificate: no
// cert-manager controller is running in demo mode to react to the Issuing
// condition the real Cluster.RenewCertificate writes, so this flips Ready
// to False/reason "Issuing" directly — the visible state a real controller
// would settle into moments after the same trigger, without lastFailure
// (so the row reads Warn/▲, never Fail/✕ — a manual renew in progress, not
// a stuck one).
func (c *Cluster) RenewCertificate(_ context.Context, namespace, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, err := c.findUnstructuredLocked(kube.KindCertificate, namespace, name)
	if err != nil {
		return err
	}
	cond := map[string]any{
		"type": "Ready", "status": "False", "reason": "Issuing",
		"message": "Certificate re-issuance manually triggered",
	}
	if err := unstructured.SetNestedSlice(u.Object, []any{cond}, "status", "conditions"); err != nil {
		return err
	}
	c.notify(kube.KindCertificate)
	return nil
}

// findUnstructuredLocked resolves one seeded custom resource by kind/name.
// Callers hold c.mu.
func (c *Cluster) findUnstructuredLocked(kind kube.ResourceKind, namespace, name string) (*unstructured.Unstructured, error) {
	for _, obj := range c.objects[kind] {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if u.GetName() != name || (namespace != "" && u.GetNamespace() != namespace) {
			continue
		}
		return u, nil
	}
	return nil, fmt.Errorf("%s %q not found", kind, name)
}

// PatchSecretData sets or removes a single key in a Secret's .Data map in
// place — 27b's add-key editor against the fake cluster. resourceVersion is
// checked the way a real apiserver checks the merge patch's
// metadata.resourceVersion: a mismatch is a typed Conflict, and a success
// returns the bumped resourceVersion, as the real apiserver's response does.
func (c *Cluster) PatchSecretData(_ context.Context, namespace, name, resourceVersion, key, value string, remove bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, obj := range c.objects[kube.KindSecret] {
		s, ok := obj.(*corev1.Secret)
		if !ok || s.Name != name || s.Namespace != namespace {
			continue
		}
		if err := c.checkAndBumpResourceVersion(&s.ObjectMeta, secretGroupResource, resourceVersion); err != nil {
			return "", err
		}
		if remove {
			delete(s.Data, key)
		} else {
			if s.Data == nil {
				s.Data = map[string][]byte{}
			}
			s.Data[key] = []byte(value)
		}
		c.notify(kube.KindSecret)
		return s.ResourceVersion, nil
	}
	return "", fmt.Errorf("%s %q not found", kube.KindSecret, name)
}

// PatchConfigMapData sets or removes a single key in a ConfigMap's .Data map
// in place — 27a's value-edit editor against the fake cluster, with the same
// resourceVersion precondition PatchSecretData checks.
func (c *Cluster) PatchConfigMapData(_ context.Context, namespace, name, resourceVersion, key, value string, remove bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, obj := range c.objects[kube.KindConfigMap] {
		cm, ok := obj.(*corev1.ConfigMap)
		if !ok || cm.Name != name || cm.Namespace != namespace {
			continue
		}
		if err := c.checkAndBumpResourceVersion(&cm.ObjectMeta, configMapGroupResource, resourceVersion); err != nil {
			return "", err
		}
		if remove {
			delete(cm.Data, key)
		} else {
			if cm.Data == nil {
				cm.Data = map[string]string{}
			}
			cm.Data[key] = value
		}
		c.notify(kube.KindConfigMap)
		return cm.ResourceVersion, nil
	}
	return "", fmt.Errorf("%s %q not found", kube.KindConfigMap, name)
}

// applyResourceEdits mutates ctr's Resources.Requests/Limits in place per
// edits — a nil field is untouched, a pointer to "" removes that resource
// key entirely (25a's explicit unset), otherwise it's parsed and set.
func applyResourceEdits(ctr *corev1.Container, edits kube.ResourceEdits) error {
	if err := applyResourceField(&ctr.Resources.Requests, corev1.ResourceCPU, edits.CPURequest); err != nil {
		return err
	}
	if err := applyResourceField(&ctr.Resources.Requests, corev1.ResourceMemory, edits.MEMRequest); err != nil {
		return err
	}
	if err := applyResourceField(&ctr.Resources.Limits, corev1.ResourceCPU, edits.CPULimit); err != nil {
		return err
	}
	if err := applyResourceField(&ctr.Resources.Limits, corev1.ResourceMemory, edits.MEMLimit); err != nil {
		return err
	}
	return nil
}

func applyResourceField(list *corev1.ResourceList, name corev1.ResourceName, edit *string) error {
	if edit == nil {
		return nil
	}
	if *edit == "" {
		delete(*list, name)
		return nil
	}
	q, err := resource.ParseQuantity(*edit)
	if err != nil {
		return fmt.Errorf("parse %s quantity %q: %w", name, *edit, err)
	}
	if *list == nil {
		*list = corev1.ResourceList{}
	}
	(*list)[name] = q
	return nil
}

// HelmRollback simulates `helm rollback` against the fake cluster's seeded
// helm.sh/release.v1 Secrets: it decodes every revision of name, picks the
// target (toRevision, or the previous revision when 0 — Helm's own
// default), marks the currently-deployed revision superseded, and appends a
// new highest-revision Secret carrying the target's chart/values/manifest
// with Status "deployed" — the same "rollback creates a new revision"
// semantics real Helm has, without needing a real helm binary or the Helm
// SDK (kube/helm.go's EncodeHelmReleaseSecret does the inverse encode).
func (c *Cluster) HelmRollback(_ context.Context, namespace, name string, toRevision int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	all := kube.DecodeHelmReleases(c.objects[kube.KindSecret])
	history := kube.HelmReleaseHistory(all, namespace, name)
	if len(history) == 0 {
		return fmt.Errorf("release %q not found in namespace %q", name, namespace)
	}
	current := history[0] // HelmReleaseHistory sorts newest-first
	var target *kube.HelmRelease
	for i := range history {
		if (toRevision == 0 && history[i].Revision == current.Revision-1) || (toRevision != 0 && history[i].Revision == toRevision) {
			target = &history[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("release %q has no revision to roll back to", name)
	}

	objs := c.objects[kube.KindSecret]
	for i, obj := range objs {
		secret, ok := obj.(*corev1.Secret)
		if !ok || secret.Type != kube.HelmReleaseSecretType {
			continue
		}
		r, err := kube.DecodeHelmReleaseSecret(secret)
		// Like helm: the revision being replaced and every revision still
		// marked deployed (a failed upgrade leaves the last good one so)
		// become superseded.
		if err != nil || r.Namespace != namespace || r.Name != name || (r.Revision != current.Revision && r.Status != "deployed") {
			continue
		}
		r.Status = "superseded"
		objs[i] = kube.EncodeHelmReleaseSecret(r)
	}

	next := *target
	next.Revision = current.Revision + 1
	next.Status = "deployed"
	next.StatusReason = fmt.Sprintf("Rollback to %d", target.Revision) // helm's own description
	next.Updated = time.Now()
	secret := kube.EncodeHelmReleaseSecret(next)
	if accessor, err := apimeta.Accessor(secret); err == nil && accessor.GetResourceVersion() == "" {
		accessor.SetResourceVersion("1") // PatchSecretData's precondition, as in Seed
	}
	c.objects[kube.KindSecret] = append(objs, secret)

	c.notify(kube.KindSecret)
	return nil
}

// RolloutUndo mirrors HelmRollback's "rollback creates a new revision"
// shape against the fake cluster: it patches the Deployment's own template
// to the target revision's, then appends a synthesized new-highest-
// revision ReplicaSet carrying that same template — so 16b's rail visibly
// gains a new top entry in --demo mode, matching how a real rollback
// behaves (the deployment controller would bump the revision annotation
// itself; nothing here reads that back from a real controller).
func (c *Cluster) RolloutUndo(_ context.Context, namespace, name string, toRevision int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var dep *appsv1.Deployment
	for _, obj := range c.objects[kube.KindDeployment] {
		d, ok := obj.(*appsv1.Deployment)
		if ok && d.Name == name && d.Namespace == namespace {
			dep = d
			break
		}
	}
	if dep == nil {
		return fmt.Errorf("deployment %q not found in namespace %q", name, namespace)
	}
	if dep.Spec.Paused {
		return fmt.Errorf("cannot roll back a paused deployment; resume it first")
	}

	var target *appsv1.ReplicaSet
	maxRevision := 0
	for _, obj := range c.objects[kube.KindReplicaSet] {
		rs, ok := obj.(*appsv1.ReplicaSet)
		if !ok || rs.Namespace != namespace || len(rs.OwnerReferences) == 0 ||
			rs.OwnerReferences[0].Kind != "Deployment" || rs.OwnerReferences[0].Name != name {
			continue
		}
		rev, _ := strconv.Atoi(rs.Annotations["deployment.kubernetes.io/revision"])
		if rev > maxRevision {
			maxRevision = rev
		}
		if rev == toRevision {
			target = rs
		}
	}
	if target == nil {
		return fmt.Errorf("deployment %q has no revision %d to roll back to", name, toRevision)
	}

	// Same wholesale replace as kube.Cluster.RolloutUndo (kubectl rollout
	// undo semantics): template and annotations, not a merge.
	template, annotations := kube.RolloutUndoTarget(dep, target)
	dep.Spec.Template = *template
	dep.Annotations = annotations

	next := target.DeepCopy()
	next.Name = fmt.Sprintf("%s-%x", name, time.Now().UnixNano())
	next.ResourceVersion = ""
	next.CreationTimestamp = metav1.NewTime(time.Now())
	next.Annotations = map[string]string{"deployment.kubernetes.io/revision": strconv.Itoa(maxRevision + 1)}
	c.objects[kube.KindReplicaSet] = append(c.objects[kube.KindReplicaSet], next)

	c.notify(kube.KindDeployment)
	c.notify(kube.KindReplicaSet)
	return nil
}

// --- GetYAML / ObjectEvents ---

func (c *Cluster) GetYAML(ctx context.Context, kind kube.ResourceKind, namespace, name string) (string, string, error) {
	objs, err := c.ListRaw(ctx, kind, namespace)
	if err != nil {
		return "", "", err
	}
	for _, obj := range objs {
		accessor, err := apimeta.Accessor(obj)
		if err != nil || accessor.GetName() != name {
			continue
		}
		copyObj := obj.DeepCopyObject()
		copyAccessor, err := apimeta.Accessor(copyObj)
		if err != nil {
			return "", "", err
		}
		rv := copyAccessor.GetResourceVersion()
		copyAccessor.SetManagedFields(nil)
		data, err := sigsyaml.Marshal(copyObj)
		if err != nil {
			return "", "", err
		}
		return string(data), rv, nil
	}
	return "", "", fmt.Errorf("%s %q not found", kind, name)
}

func (c *Cluster) ObjectEvents(ctx context.Context, namespace string, kind kube.ResourceKind, name string) ([]kube.Event, error) {
	objs, err := c.ListRaw(ctx, kube.KindEvent, namespace)
	if err != nil {
		return nil, err
	}
	out := make([]kube.Event, 0, len(objs))
	for _, obj := range objs {
		ev, ok := obj.(*corev1.Event)
		if !ok || ev.InvolvedObject.Kind != kind.APIKind() || ev.InvolvedObject.Name != name {
			continue
		}
		out = append(out, kube.Event{
			Type:             ev.Type,
			Reason:           ev.Reason,
			Message:          ev.Message,
			Object:           ev.InvolvedObject.Kind + "/" + ev.InvolvedObject.Name,
			ObjectAPIVersion: ev.InvolvedObject.APIVersion,
			Namespace:        ev.Namespace,
			Count:            max32(ev.Count, 1),
			FirstSeen:        ev.FirstTimestamp.Time,
			LastSeen:         ev.LastTimestamp.Time,
			// Carried through because §32a's revision rows live in the
			// event's annotations, not its message — dropping them here
			// makes those rows silently vanish under the fake.
			Annotations: ev.Annotations,
		})
	}
	slices.SortFunc(out, func(a, b kube.Event) int { return b.LastSeen.Compare(a.LastSeen) })
	return out, nil
}

func (c *Cluster) NamespaceEvents(ctx context.Context, namespace string) ([]kube.Event, error) {
	objs, err := c.ListRaw(ctx, kube.KindEvent, namespace)
	if err != nil {
		return nil, err
	}
	out := make([]kube.Event, 0, len(objs))
	for _, obj := range objs {
		ev, ok := obj.(*corev1.Event)
		if !ok {
			continue
		}
		out = append(out, kube.Event{
			Type:             ev.Type,
			Reason:           ev.Reason,
			Message:          ev.Message,
			Object:           ev.InvolvedObject.Kind + "/" + ev.InvolvedObject.Name,
			ObjectAPIVersion: ev.InvolvedObject.APIVersion,
			Namespace:        ev.Namespace,
			Count:            max32(ev.Count, 1),
			FirstSeen:        ev.FirstTimestamp.Time,
			LastSeen:         ev.LastTimestamp.Time,
			// Carried through because §32a's revision rows live in the
			// event's annotations, not its message — dropping them here
			// makes those rows silently vanish under the fake.
			Annotations: ev.Annotations,
		})
	}
	slices.SortFunc(out, func(a, b kube.Event) int { return b.LastSeen.Compare(a.LastSeen) })
	return out, nil
}

func max32(v, floor int32) int32 {
	if v == 0 {
		return floor
	}
	return v
}

// --- pod metrics ---

// demoUsageRatio derives a stable per-object usage ratio from name (FNV-1a
// hash mod 96, offset by 0.10) — deterministic, not actually random, so the
// same pod/node always renders the same bar/text across repeated Renders.
// Spread over roughly [0.10, 1.05] so --demo mode can actually exercise the
// Fill/Warn/Bad bar states (docs/design README.md §Design Tokens: Warn
// ≥70%, Bad at/over limit) — previously every usage was the literal string
// "n/a", so 6a's CPU-share column and the main table's CPU/MEM columns
// could never be exercised in demo mode at all (CLAUDE.md: "the fake
// provider must stay feature-complete for tests/demo mode").
func demoUsageRatio(name string) float64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return 0.10 + float64(h.Sum32()%96)/100
}

func (c *Cluster) PodMetricsByNamespace(_ context.Context, namespace string) (map[string]kube.PodMetrics, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]kube.PodMetrics)
	for _, obj := range c.objects[kube.KindPod] {
		pod, ok := obj.(*corev1.Pod)
		if !ok || (namespace != "" && pod.Namespace != namespace) {
			continue
		}
		if pod.Status.Phase != corev1.PodRunning {
			// Real metrics-server has nothing to report for a pod that
			// isn't running yet (or anymore) either.
			out[kube.PodKey(pod.Namespace, pod.Name)] = kube.PodMetrics{CPU: "n/a", MEM: "n/a"}
			continue
		}
		var cpuLimMilli, memLimBytes int64
		for _, ctr := range pod.Spec.Containers {
			cpuLimMilli += ctr.Resources.Limits.Cpu().MilliValue()
			memLimBytes += ctr.Resources.Limits.Memory().Value()
		}
		ratio := demoUsageRatio(pod.Name)
		cpuMilli := int64(float64(cpuLimMilli) * ratio)
		memBytes := int64(float64(memLimBytes) * ratio)
		out[kube.PodKey(pod.Namespace, pod.Name)] = kube.PodMetrics{
			CPU:      kube.FormatCPU(*resource.NewMilliQuantity(cpuMilli, resource.DecimalSI)),
			MEM:      kube.FormatMemory(*resource.NewQuantity(memBytes, resource.BinarySI)),
			CPUMilli: cpuMilli,
			MemBytes: memBytes,
		}
	}
	return out, nil
}

// ContainerMetricsByNamespace is the fake equivalent of
// kube.Cluster.ContainerMetricsByNamespace — 25a's per-container usage seam.
// Each container gets its own demoUsageRatio (keyed by pod+container name,
// not just pod name, so sibling containers in the same pod render distinct
// numbers rather than an identical bar) applied to that container's own
// limits.
func (c *Cluster) ContainerMetricsByNamespace(_ context.Context, namespace string) (map[string]map[string]kube.PodMetrics, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]map[string]kube.PodMetrics)
	for _, obj := range c.objects[kube.KindPod] {
		pod, ok := obj.(*corev1.Pod)
		if !ok || (namespace != "" && pod.Namespace != namespace) {
			continue
		}
		containers := make(map[string]kube.PodMetrics, len(pod.Spec.Containers))
		if pod.Status.Phase != corev1.PodRunning {
			for _, ctr := range pod.Spec.Containers {
				containers[ctr.Name] = kube.PodMetrics{CPU: "n/a", MEM: "n/a"}
			}
			out[kube.PodKey(pod.Namespace, pod.Name)] = containers
			continue
		}
		for _, ctr := range pod.Spec.Containers {
			cpuLimMilli := ctr.Resources.Limits.Cpu().MilliValue()
			memLimBytes := ctr.Resources.Limits.Memory().Value()
			ratio := demoUsageRatio(pod.Name + "/" + ctr.Name)
			cpuMilli := int64(float64(cpuLimMilli) * ratio)
			memBytes := int64(float64(memLimBytes) * ratio)
			containers[ctr.Name] = kube.PodMetrics{
				CPU:      kube.FormatCPU(*resource.NewMilliQuantity(cpuMilli, resource.DecimalSI)),
				MEM:      kube.FormatMemory(*resource.NewQuantity(memBytes, resource.BinarySI)),
				CPUMilli: cpuMilli,
				MemBytes: memBytes,
			}
		}
		out[kube.PodKey(pod.Namespace, pod.Name)] = containers
	}
	return out, nil
}

// NodeDiskUsage synthesizes a believable root-filesystem reading for demo
// mode: the node's ephemeral-storage capacity (100Gi when it declares none)
// at a stable per-node fraction, the same hash NodeMetrics uses so a node
// that runs hot on memory doesn't look idle on disk by coincidence. Every
// demo node also carries its container runtime on a separate 200Gi disk, so
// 11b's img row has something to show.
func (c *Cluster) NodeDiskUsage(_ context.Context, nodeName string) (kube.NodeDisk, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, obj := range c.objects[kube.KindNode] {
		n, ok := obj.(*corev1.Node)
		if !ok || n.Name != nodeName {
			continue
		}
		capacity := n.Status.Capacity.StorageEphemeral().Value()
		if capacity == 0 {
			capacity = 100 << 30
		}
		used := int64(float64(capacity) * demoUsageRatio(n.Name) * 0.8)
		const imageCapacity = 200 << 30
		imageAvailable := int64(float64(imageCapacity) * (1 - demoUsageRatio(n.Name+"/imagefs")*0.9))
		return kube.NewNodeDisk(used, capacity, capacity-used, imageCapacity, imageAvailable), nil
	}
	return kube.NodeDisk{}, fmt.Errorf("node %q not found", nodeName)
}

func (c *Cluster) NodeMetrics(_ context.Context) (map[string]kube.NodeMetric, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]kube.NodeMetric)
	for _, obj := range c.objects[kube.KindNode] {
		n, ok := obj.(*corev1.Node)
		if !ok {
			continue
		}
		ratio := demoUsageRatio(n.Name)
		cpuMilli := int64(float64(n.Status.Allocatable.Cpu().MilliValue()) * ratio)
		memBytes := int64(float64(n.Status.Allocatable.Memory().Value()) * ratio)
		out[n.Name] = kube.NodeMetric{
			CPU:      kube.FormatCPU(*resource.NewMilliQuantity(cpuMilli, resource.DecimalSI)),
			MEM:      kube.FormatMemory(*resource.NewQuantity(memBytes, resource.BinarySI)),
			CPUMilli: cpuMilli,
			MemBytes: memBytes,
		}
	}
	return out, nil
}

// --- log streaming ---

// StreamPodLogs replays the seeded lines the way the kubelet would answer
// req: the container's own lines (else the pod's), only those newer than
// SinceSeconds when a line carries a timestamp, the last TailLines of them,
// and — for Follow — a stream that stays open until ctx is cancelled rather
// than ending (a real follow only ends when the container does), so a
// history read followed by a follow shows each line once.
func (c *Cluster) StreamPodLogs(ctx context.Context, req kube.LogStreamRequest) (io.ReadCloser, error) {
	c.mu.Lock()
	lines, ok := c.logs[req.Namespace+"/"+req.PodName+"/"+req.Container]
	if !ok {
		lines = c.logs[req.Namespace+"/"+req.PodName]
	}
	c.mu.Unlock()
	if req.SinceSeconds > 0 {
		cutoff := time.Now().Add(-time.Duration(req.SinceSeconds) * time.Second)
		kept := make([]string, 0, len(lines))
		for _, line := range lines {
			stamp, _, _ := strings.Cut(line, " ")
			if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil && t.Before(cutoff) {
				continue
			}
			kept = append(kept, line)
		}
		lines = kept
	}
	if req.TailLines > 0 && int64(len(lines)) > req.TailLines {
		lines = lines[int64(len(lines))-req.TailLines:]
	}
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n"
	}
	var r io.Reader = strings.NewReader(body)
	if req.Follow {
		r = followReader{ctx: ctx, r: r}
	}
	return io.NopCloser(r), nil
}

// followReader serves r, then blocks at its end until ctx is cancelled —
// a follow stream on a container that has nothing more to say.
type followReader struct {
	ctx context.Context
	r   io.Reader
}

func (f followReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if errors.Is(err, io.EOF) && n == 0 {
		<-f.ctx.Done()
		return 0, io.EOF
	}
	return n, err
}

// --- Switcher (home.Switcher successor) ---

func (c *Cluster) Contexts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.contexts...)
}

func (c *Cluster) CurrentContext() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.context
}

func (c *Cluster) CurrentNamespace() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.namespace
}

func (c *Cluster) SwitchNamespace(namespace string) {
	if namespace == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.namespace = namespace
	c.perContextNamespace[c.context] = namespace
}

func (c *Cluster) SwitchContext(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Contains(c.contexts, name) {
		return fmt.Errorf("context %q not found", name)
	}
	c.context = name
	if ns, ok := c.perContextNamespace[name]; ok && ns != "" {
		c.namespace = ns
	}
	return nil
}

// AddContext registers an additional switchable context (for palette/probe
// fixtures and tests).
func (c *Cluster) AddContext(name, namespace string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.contexts = append(c.contexts, name)
	c.perContextNamespace[name] = namespace
}

// SetUserName sets the identity tasks/whocan (22a) pins as "current user"
// (fixtures.go's NewDemo calls this so --demo has someone to pin).
func (c *Cluster) SetUserName(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userName = name
}

// CurrentUser answers the identity SetUserName pinned — the fake
// counterpart of a real *kube.Cluster's Context.UserName field, which app.go
// reads directly since kube.Cluster.Context is a plain struct field with no
// getter of its own. app.go's composition root (0.8.0 plan Phase 8) uses
// this to wire browse.Config.CurrentUser in --demo, the identity §36b's
// run-now stamps into kube.AnnotationTriggeredBy.
func (c *Cluster) CurrentUser() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.userName
}

// SetUserGroups sets the pinned current user's known group memberships —
// the fake counterpart of a client cert's Subject Organization fields
// (kube.Context.UserGroups), so --demo can also exercise a Group-only
// grant (the common cluster-admin/system:masters shape).
func (c *Cluster) SetUserGroups(groups []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userGroups = groups
}

// SetReviewedIdentity sets the identity a real cluster's connect-time
// SelfSubjectReview would have answered. While set, WhoCan pins it as
// "(you)" in place of SetUserName/SetUserGroups' kubeconfig-style identity.
func (c *Cluster) SetReviewedIdentity(username string, groups []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reviewed = &kube.ReviewedIdentity{Username: username, Groups: slices.Clone(groups)}
}

// ReviewedIdentity mirrors *kube.Cluster.ReviewedIdentity: false until
// SetReviewedIdentity is called.
func (c *Cluster) ReviewedIdentity() (kube.ReviewedIdentity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reviewed == nil {
		return kube.ReviewedIdentity{}, false
	}
	return kube.ReviewedIdentity{Username: c.reviewed.Username, Groups: slices.Clone(c.reviewed.Groups)}, true
}

// --- WhoCan (22a) ---

// WhoCan resolves query against the fake cluster's seeded RBAC objects,
// mirroring kube.Cluster.WhoCan's shape over kube.ResolveWhoCan's shared,
// cluster-agnostic matching logic.
func (c *Cluster) WhoCan(ctx context.Context, query kube.WhoCanQuery) (kube.WhoCanResult, error) {
	crObjs, err := c.ListRaw(ctx, kube.KindClusterRole, "")
	if err != nil {
		return kube.WhoCanResult{}, err
	}
	rObjs, err := c.ListRaw(ctx, kube.KindRole, query.Namespace)
	if err != nil {
		return kube.WhoCanResult{}, err
	}
	crbObjs, err := c.ListRaw(ctx, kube.KindClusterRoleBinding, "")
	if err != nil {
		return kube.WhoCanResult{}, err
	}
	rbObjs, err := c.ListRaw(ctx, kube.KindRoleBinding, query.Namespace)
	if err != nil {
		return kube.WhoCanResult{}, err
	}

	clusterRoles := make([]*rbacv1.ClusterRole, 0, len(crObjs))
	for _, o := range crObjs {
		if cr, ok := o.(*rbacv1.ClusterRole); ok {
			clusterRoles = append(clusterRoles, cr)
		}
	}
	roles := make([]*rbacv1.Role, 0, len(rObjs))
	for _, o := range rObjs {
		if r, ok := o.(*rbacv1.Role); ok {
			roles = append(roles, r)
		}
	}
	clusterRoleBindings := make([]*rbacv1.ClusterRoleBinding, 0, len(crbObjs))
	for _, o := range crbObjs {
		if crb, ok := o.(*rbacv1.ClusterRoleBinding); ok {
			clusterRoleBindings = append(clusterRoleBindings, crb)
		}
	}
	roleBindings := make([]*rbacv1.RoleBinding, 0, len(rbObjs))
	for _, o := range rbObjs {
		if rb, ok := o.(*rbacv1.RoleBinding); ok {
			roleBindings = append(roleBindings, rb)
		}
	}

	c.mu.Lock()
	user, groups := c.userName, c.userGroups
	if c.reviewed != nil {
		user, groups = c.reviewed.Username, c.reviewed.Groups
	}
	c.mu.Unlock()
	return kube.ResolveWhoCan(query, user, groups, clusterRoles, roles, clusterRoleBindings, roleBindings), nil
}

// CanI is the fake counterpart of kube.Cluster's live
// SelfSubjectAccessReview. The fake has no API-server authorizer chain, so
// its seeded RBAC graph is the authoritative answer for --demo and tests.
func (c *Cluster) CanI(ctx context.Context, query kube.WhoCanQuery) (kube.AccessReviewResult, error) {
	result, err := c.WhoCan(ctx, query)
	if err != nil {
		return kube.AccessReviewResult{}, err
	}
	if result.CurrentUser == "" {
		return kube.AccessReviewResult{EvaluationError: "current user is unknown"}, nil
	}
	// Like RBAC on a real API server, a miss is allowed=false with denied
	// unset — never the explicit Denied only a deny-capable authorizer sets.
	return kube.AccessReviewResult{
		Allowed: result.CurrentUserGranted,
		Reason:  result.CurrentUserVia,
	}, nil
}

// --- connection health / events ---

func (c *Cluster) Events() <-chan kube.ResourceChangedMsg { return c.events }
func (c *Cluster) ConnEvents() <-chan kube.ConnStateMsg   { return c.connCh }

func (c *Cluster) ConnState() kube.ConnState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn
}

// RetryNow is a no-op for the fake — there's nothing to retry against.
func (c *Cluster) RetryNow() {}

// SetConnState lets tests drive 4a/4b/4c states through the fake.
func (c *Cluster) SetConnState(s kube.ConnState) {
	c.mu.Lock()
	c.conn = s
	c.mu.Unlock()
	select {
	case c.connCh <- kube.ConnStateMsg(s):
	default:
	}
}

func (c *Cluster) notify(kind kube.ResourceKind) {
	select {
	case c.events <- kube.ResourceChangedMsg{Kind: kind}:
	default:
	}
}
