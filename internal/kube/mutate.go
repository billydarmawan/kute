package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

// Mutator is the write seam to the cluster: every mutating verb (delete,
// cordon, drain, rollout restart, …) gets its own method here as screens
// grow to need them, so every write funnels through one contract that
// app.NewModel wires and screens depend on only as an interface.
type Mutator interface {
	// DeleteResource deletes a single named resource of kind in namespace. For
	// cluster-scoped kinds namespace is ignored.
	DeleteResource(ctx context.Context, kind ResourceKind, namespace, name string) error
	// DeleteResourceForced deletes immediately (grace period 0 — ctrl-k).
	DeleteResourceForced(ctx context.Context, kind ResourceKind, namespace, name string) error
	// RolloutRestart patches kind's pod template with a fresh restartedAt
	// annotation, the same mechanism `kubectl rollout restart` uses to
	// trigger a new rollout without changing the spec. Deployment,
	// StatefulSet, and DaemonSet are the three kinds with a pod template
	// browse exposes it on (9a's own 'r'; 27a's ctrl-r restarts a
	// ConfigMap's consumers regardless of which of the three they are).
	RolloutRestart(ctx context.Context, kind ResourceKind, namespace, name string) error
	// Cordon sets (cordon=true) or clears spec.unschedulable on node.
	Cordon(ctx context.Context, node string, cordon bool) error
	// Drain cordons node then evicts every evictable pod on it (skipping
	// DaemonSet-owned and mirror pods), returning the number evicted.
	Drain(ctx context.Context, node string) (int, error)
	// HelmRollback rolls release back to toRevision (0 = Helm's own default:
	// the previous revision) — the one Helm verb that shells out to the real
	// `helm` binary rather than reading the watch cache (docs/design
	// README.md §18a).
	HelmRollback(ctx context.Context, namespace, name string, toRevision int) error
	// RolloutUndo rolls a Deployment back to toRevision by copying that
	// revision's owned ReplicaSet pod template onto the Deployment's own
	// spec.template — the same client-side mechanism modern kubectl uses
	// since the apps/v1 rollback subresource was removed. The deployment
	// controller recognizes the restored template's hash against the
	// existing (scaled-down) ReplicaSet and reuses it rather than creating
	// a new one, then bumps the revision annotation itself (16b's 'R',
	// docs/design README.md §16b).
	RolloutUndo(ctx context.Context, namespace, name string, toRevision int) error
	// Scale patches kind's spec.replicas to replicas — 17b's +/− inline
	// prompt. Deployment and StatefulSet are the only kinds with a
	// spec.replicas field browse exposes.
	Scale(ctx context.Context, kind ResourceKind, namespace, name string, replicas int32) error
	// SetImage patches container's image on kind's pod template — 24a's
	// tag-first inline editor. initContainer selects spec.initContainers
	// instead of spec.containers. Deployment, StatefulSet, DaemonSet, and
	// CronJob are the four kinds with a pod template browse exposes it on.
	SetImage(ctx context.Context, kind ResourceKind, namespace, name, container, image string, initContainer bool) error
	// SetResources patches container's resources.requests/limits on kind's
	// pod template — 25a's editor. edits' non-nil fields are the changed
	// ones ("only changed fields go into the command"); a pointer to "" is
	// an explicit unset (the 'u' key), removing that key entirely rather
	// than leaving it untouched. dryRun runs the same patch with the API
	// server's DryRun option set, surfacing an admission rejection (a
	// namespace LimitRange/ResourceQuota violation) without mutating
	// anything — 25a's caller runs this once before the real patch.
	SetResources(ctx context.Context, kind ResourceKind, namespace, name, container string, edits ResourceEdits, dryRun bool) error
	// PatchMeta sets or removes a single label or annotation key on
	// kind/namespace/name — 26a's editor. remove=true deletes the key
	// (kubectl label/annotate's "key-" syntax, patched as a JSON-merge-patch
	// null); otherwise sets key=value ("--overwrite" when the key already
	// existed). Works on every kind including discovered CRDs: kinds outside
	// the typed clientset switch fall back to the dynamic client by GVR.
	PatchMeta(ctx context.Context, kind ResourceKind, namespace, name string, isAnnotation bool, key, value string, remove bool) error
	// PatchSecretData sets or removes a single key in a Secret's data — 27b's
	// add-key editor. remove=true deletes the key (a JSON-merge-patch null
	// under .data, the same null-removes-a-key idiom PatchMeta uses);
	// otherwise sets key=value via .stringData, so the API server does the
	// base64 encoding, not the caller. resourceVersion is the Secret's
	// resourceVersion as the caller observed it, sent as a precondition in
	// the patch body, so a concurrent write returns Conflict
	// (apierrors.IsConflict) instead of being silently overwritten — the
	// same precondition SetCronJobSchedule carries. It is required. On
	// success it returns the Secret's new resourceVersion as the server
	// reported it (the same authoritative read-back CronJobScheduleResult
	// carries), so the editor's next write can precondition on it before
	// the informer cache has caught up with this one.
	PatchSecretData(ctx context.Context, namespace, name, resourceVersion, key, value string, remove bool) (string, error)
	// PatchConfigMapData sets or removes a single key in a ConfigMap's data —
	// 27a's value-edit editor. remove=true deletes the key (a JSON-merge-patch
	// null under .data, same idiom as PatchSecretData's removal); otherwise
	// sets key=value directly under .data — unlike Secret, ConfigMap's .data
	// is already plain text, so there's no .stringData split to make.
	// resourceVersion is the same required precondition PatchSecretData
	// carries, and the returned string is the ConfigMap's new
	// resourceVersion, as PatchSecretData returns.
	PatchConfigMapData(ctx context.Context, namespace, name, resourceVersion, key, value string, remove bool) (string, error)
	// SetFluxSuspend sets or clears spec.suspend on a Flux object — what
	// `flux suspend`/`flux resume` do, as a plain API patch with no flux
	// binary anywhere in the path (docs/design README.md §30a; contrast
	// §18a's HelmRollback, which does shell out to helm). kind is the
	// registry kind and resolves to its real GVR through the dynamic
	// client, the same resolution PatchMeta's CRD fallback uses.
	SetFluxSuspend(ctx context.Context, kind ResourceKind, namespace, name string, suspend bool) error
	// RequestFluxReconcile stamps reconcile.fluxcd.io/requestedAt with a
	// fresh RFC3339 timestamp — what `flux reconcile` does, and the same
	// annotate-to-trigger mechanism RolloutRestart already uses on a
	// workload. Non-destructive: it asks for the reconciliation that would
	// have happened on the next interval anyway.
	RequestFluxReconcile(ctx context.Context, kind ResourceKind, namespace, name string) error
	// RequestArgoRefresh stamps argocd.argoproj.io/refresh=normal — what
	// `argocd app get --refresh` does, and the same annotate-to-trigger
	// mechanism RequestFluxReconcile uses on a Flux object (docs/design
	// README.md §33a). Non-destructive: it asks argocd-application-
	// controller to re-diff live state against git, which it already does
	// on its own poll interval.
	RequestArgoRefresh(ctx context.Context, kind ResourceKind, namespace, name string) error
	// RequestArgoSync merge-patches operation.sync.revision on an Argo CD
	// Application — what `argocd app sync` does as a plain API write, no
	// argocd binary anywhere in the path (§33a, same "kute just shows it"
	// stance §30a takes on `flux suspend`/`flux reconcile`). revision is
	// the Application's own already-configured target (spec.source.
	// targetRevision, or "HEAD" when unset) — this re-applies what git
	// already says to run, never a move to a different ref. initiator is
	// recorded as operation.initiatedBy.username (ArgoSyncInitiator: the
	// reviewed identity, else "kute") so Argo — and 34a's timeline — credit
	// the sync to a person rather than to nobody or to auto-sync.
	RequestArgoSync(ctx context.Context, kind ResourceKind, namespace, name, revision, initiator string) error
	// RenewCertificate forces cert-manager to reissue a Certificate: a plain
	// merge patch setting the Issuing status condition, the same trigger
	// cert-manager's own trigger controller reacts to (docs/design/
	// v.0.7.0.dc.html §35c). Not `cmctl renew` — RenewCertificateCommandString's
	// own doc comment has the reasoning for why the will-run line shows the
	// literal patch instead. A plain merge patch does replace
	// status.conditions wholesale rather than updating one entry among
	// several, which would normally risk dropping Ready along with it — the
	// design accepts that trade deliberately: cert-manager's own controller
	// reconciles status again within moments of any trigger, the same
	// "asks for what the controller would do anyway" territory
	// FluxReconcile/ArgoRefresh already stand in, just confirmed rather than
	// TierNone because this one visibly changes what's served.
	RenewCertificate(ctx context.Context, namespace, name string) error
	// RetryJob clones name's spec into a new Job called newName — get, strip
	// the fields the API server must regenerate (selector, controller-uid/
	// job-name template labels), create. The original Job (its Status,
	// events, history) is never touched — this isn't a delete+recreate, it's
	// what `kubectl create job newName --from=job/name` does.
	//
	// §37c: the clone is always a standalone copy, even when name itself was
	// cronjob-owned/associated — Kute's own AnnotationCronJobUID/
	// AnnotationCronJobName are stripped from the clone (never carried over
	// via the cloned annotation map) so it never counts toward that
	// CronJob's history limits or garbage collection, and creator/at are
	// stamped as AnnotationTriggeredBy/AnnotationTriggeredAt (the same
	// attribution TriggerCronJob stamps on a manual run) so
	// resources.BuildJobListSummaries' SOURCE resolution reads the rerun as
	// "manual · <creator>", never silently re-inheriting "cronjob/x".
	RetryJob(ctx context.Context, namespace, name, newName, creator string, at time.Time) error
	// ReplaceJob is §37c's "replace" choice: delete name, then create a new
	// Job of the same name from the deleted Job's own spec — unlike RetryJob,
	// this destroys the original (its Status, events, and §37b's own
	// attempt-ledger history are gone), which is why it is gated behind
	// a real Controller-driven confirm (the type-the-name modal in PROD) rather than
	// RetryJob's staged-then-TierNone shape. There is no single kubectl
	// one-liner this is equivalent to (the source is gone once deleted) —
	// ReplaceCommandString documents the two-step recipe honestly instead of
	// claiming one.
	ReplaceJob(ctx context.Context, namespace, name string) error
	// SetJobSuspend patches spec.suspend on a Job — same JSON body
	// SetFluxSuspend uses, but through the typed clientset (KindJob has one;
	// Flux's discovered CRDs don't), matching deleteResource/PatchMeta's
	// existing KindJob branches. Setting suspend=true tears down the Job's
	// currently-active pods immediately; suspend=false lets it resume
	// creating pods toward its completion target.
	SetJobSuspend(ctx context.Context, namespace, name string, suspend bool) error
	// TriggerCronJob creates a new, standalone Job called newJobName from
	// name's own spec.jobTemplate — get, copy ObjectMeta.Labels/Annotations
	// and Spec off the template, create: exactly what
	// `kubectl create job newJobName --from=cronjob/name` does, stamped with
	// Kute's own manual-run annotations (§36b/§4.2): the source CronJob's
	// name/UID (AnnotationCronJobName/AnnotationCronJobUID — how
	// resources.BuildCronJobSummaries associates this Job back to its
	// CronJob without an owner reference), who asked
	// (AnnotationTriggeredBy = creator) and when (AnnotationTriggeredAt =
	// at, RFC3339 UTC — the moment the run was staged/confirmed, not
	// whenever the API server happens to accept the Create), plus the
	// kubectl-compatible `cronjob.kubernetes.io/instantiate=manual`
	// annotation for CLI parity. No OwnerReferences are set (kubectl's own
	// recipe sets none either), so the manual run is a standalone object the
	// CronJob's own successfulJobsHistoryLimit/failedJobsHistoryLimit GC
	// never touches — the same reasoning RetryJob's own doc comment gives
	// for a manual Job retry. The CronJob itself (its schedule, its own
	// spawned-Job history) is never touched.
	//
	// Returns an error wrapping ErrManualJobNameConflict when newJobName is
	// already taken (a real AlreadyExists race on the staged name — two
	// operators racing the same CronJob in the same minute, or a leftover
	// Job from a prior run) so a caller can distinguish it via errors.Is and
	// offer restaging against refreshed Job names, rather than silently
	// picking a different name after the user already confirmed this one.
	TriggerCronJob(ctx context.Context, namespace, name, newJobName, creator string, at time.Time) error
	// SetCronJobSuspend patches spec.suspend on a CronJob, atomically
	// alongside Kute's own suspend-timestamp annotations (§3.3): suspending
	// stamps AnnotationSuspendedAt = at (RFC3339 UTC, the moment the suspend
	// was staged/confirmed) and AnnotationSuspendedGeneration =
	// currentGeneration+1 — the generation the CronJob will have once this
	// very patch lands, precomputed by the caller from its own already-
	// loaded CronJobSummary.Object.Generation rather than re-derived here,
	// the same "preview and execution agree" contract TriggerCronJob's
	// newJobName follows. Resuming clears both annotations in the same
	// patch that unsets spec.suspend (at is unused on that path).
	// resourceVersion is a required precondition: the patch also carries
	// metadata.resourceVersion, so a concurrent external spec change since
	// the caller last read the object returns Conflict (see
	// apierrors.IsConflict) instead of silently overwriting it or stamping
	// a generation guess that no longer matches reality — the same reason a
	// stale suspendedAt annotation is deliberately never trusted
	// (resources.suspendedAt's own generation check).
	//
	// Setting suspend=true only stops the controller from creating *future*
	// Jobs on schedule; any Job (and any pod) already spawned keeps running
	// untouched — unlike SetJobSuspend, nothing currently active is torn
	// down (verbs.CronJobSuspend's own doc comment has the full reasoning).
	SetCronJobSuspend(ctx context.Context, namespace, name string, suspend bool, resourceVersion string, currentGeneration int64, at time.Time) error
	// SetCronJobSchedule atomically patches spec.schedule and (optionally)
	// spec.timeZone on a CronJob — the server-side equivalent of
	// `kubectl patch cronjob/name --type merge -p '{"spec":{"schedule":"...","timeZone":...}}'`.
	// edit.ResourceVersion is a required precondition, the same
	// metadata.resourceVersion-in-the-patch-body mechanism
	// SetCronJobSuspend uses, so a concurrent external edit returns Conflict
	// rather than being silently overwritten. edit.TimeZone distinguishes
	// "leave spec.timeZone untouched" (nil) from "clear it" (pointer to
	// "", serialized as an explicit JSON null under the merge patch) from
	// "set it" (pointer to a non-empty IANA name) — §3.8/§3.9's set/clear
	// distinction, so an empty or capability-unsupported value can never
	// accidentally serialize as a stale timezone string. The caller
	// (tasks/cronjobschedule, Phase 6) has already validated schedule
	// parses via robfig/cron/v3's ParseStandard and timezone via
	// time.LoadLocation before this is ever reached, so this method trusts
	// the caller instead of re-validating and returning a second,
	// differently-worded error for the same bad input.
	//
	// The returned CronJobScheduleResult carries the API's own accepted
	// schedule/timezone plus the CronJob's new resourceVersion — the
	// editor's refresh/undo must key off this authoritative value rather
	// than assume an immediate, possibly-stale informer cache read is
	// ready.
	SetCronJobSchedule(ctx context.Context, namespace, name string, edit CronJobScheduleEdit) (CronJobScheduleResult, error)
}

// ErrManualJobNameConflict wraps a Create AlreadyExists error for a staged
// manual CronJob run's target Job name (§36b) — TriggerCronJob's newJobName
// was already taken by the time the create actually ran. Not a bug in
// ManualJobName's own collision handling: the name was correct against the
// Job cache the caller staged against, and something else (a racing
// operator, a leftover fixture) claimed it in between. Callers check
// errors.Is(err, ErrManualJobNameConflict) to offer restaging against
// refreshed Job names (Plan Phase 2 task 14) rather than silently picking a
// different name after the user already confirmed the one shown in the
// preview.
var ErrManualJobNameConflict = errors.New("a job with this name already exists")

// CronJobScheduleEdit is SetCronJobSchedule's input (Plan Phase 2 task 6-8).
// TimeZone is a three-state pointer distinguishing "don't touch
// spec.timeZone" (nil) from "clear it" (pointer to "") from "set it"
// (pointer to a non-empty IANA name) — the same nil-vs-pointer-to-""
// convention ResourceEdits already uses for an explicit unset.
// ResourceVersion is required: SetCronJobSchedule refuses to patch without
// one, since an unconditional schedule write is exactly the silent-overwrite
// behavior §4.2/Phase 2 exists to prevent.
type CronJobScheduleEdit struct {
	Schedule        string
	TimeZone        *string
	ResourceVersion string
}

// CronJobScheduleResult is SetCronJobSchedule's successful return: the
// server's own accepted schedule/timezone reflected back (never just an
// echo of the request — a caller must read what the API actually stored)
// plus the CronJob's new resourceVersion after the patch. TimeZone is ""
// when spec.timeZone is unset after the edit. §36d's editor uses this,
// never an immediate informer-cache read, to know refresh/undo is ready
// (Plan Phase 2 task 10).
type CronJobScheduleResult struct {
	Schedule        string
	TimeZone        string
	ResourceVersion string
}

// ConfigMapConsumerRef is one workload that references a ConfigMap from its
// pod template (27a's consumer strip / ctrl-r restart set) — the kind is
// carried alongside the name since ctrl-r's RolloutRestart call needs it and
// a consumer can be a Deployment, StatefulSet, or DaemonSet.
type ConfigMapConsumerRef struct {
	Kind ResourceKind
	Name string
}

// DeleteResource implements Mutator against the live clientset. It maps the kind
// to the appropriate typed client. This is the only place the client-go delete
// verbs are called, so authorization failures surface uniformly through
// IsPermissionError at the call site.
func (c *Cluster) DeleteResource(ctx context.Context, kind ResourceKind, namespace, name string) error {
	return c.deleteResource(ctx, kind, namespace, name, backgroundDelete())
}

// DeleteResourceForced deletes with GracePeriodSeconds 0 (ctrl-k force
// delete — always modal-confirmed, see actions.Tier).
func (c *Cluster) DeleteResourceForced(ctx context.Context, kind ResourceKind, namespace, name string) error {
	zero := int64(0)
	opts := backgroundDelete()
	opts.GracePeriodSeconds = &zero
	return c.deleteResource(ctx, kind, namespace, name, opts)
}

// backgroundDelete is the DeleteOptions every kute delete starts from:
// explicit Background propagation, which is kubectl's default
// (--cascade=background). An empty DeleteOptions leaves the choice to the
// kind's REST strategy, and for batch/v1 Jobs that default is still
// orphan — deleting a running Job would leave its pods running, ownerless
// and invisible to every Job view (they join by owner UID). Background is
// accepted by every kind, built-in or CRD, and is already the default for
// the rest, so it is sent unconditionally.
func backgroundDelete() metav1.DeleteOptions {
	policy := metav1.DeletePropagationBackground
	return metav1.DeleteOptions{PropagationPolicy: &policy}
}

func (c *Cluster) deleteResource(ctx context.Context, kind ResourceKind, namespace, name string, opts metav1.DeleteOptions) error {
	if name == "" {
		return fmt.Errorf("cannot delete %s: empty name", kind)
	}
	cs := c.clientset
	switch kind {
	case KindPod:
		return cs.CoreV1().Pods(namespace).Delete(ctx, name, opts)
	case KindService:
		return cs.CoreV1().Services(namespace).Delete(ctx, name, opts)
	case KindIngress:
		return cs.NetworkingV1().Ingresses(namespace).Delete(ctx, name, opts)
	case KindConfigMap:
		return cs.CoreV1().ConfigMaps(namespace).Delete(ctx, name, opts)
	case KindSecret:
		return cs.CoreV1().Secrets(namespace).Delete(ctx, name, opts)
	case KindPersistentVolumeClaim:
		return cs.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name, opts)
	case KindEvent:
		return cs.CoreV1().Events(namespace).Delete(ctx, name, opts)
	case KindDeployment:
		return cs.AppsV1().Deployments(namespace).Delete(ctx, name, opts)
	case KindDaemonSet:
		return cs.AppsV1().DaemonSets(namespace).Delete(ctx, name, opts)
	case KindStatefulSet:
		return cs.AppsV1().StatefulSets(namespace).Delete(ctx, name, opts)
	case KindReplicaSet:
		return cs.AppsV1().ReplicaSets(namespace).Delete(ctx, name, opts)
	case KindJob:
		return cs.BatchV1().Jobs(namespace).Delete(ctx, name, opts)
	case KindCronJob:
		return cs.BatchV1().CronJobs(namespace).Delete(ctx, name, opts)
	case KindNamespace:
		return cs.CoreV1().Namespaces().Delete(ctx, name, opts)
	case KindNode:
		return cs.CoreV1().Nodes().Delete(ctx, name, opts)
	default:
		// Every discovered CRD lands here. Without this fallback ctrl-d
		// failed on every custom-resource row while the keybar advertised
		// it — the verb registry offers Delete on any kind, so the write
		// seam has to be able to honour that on any kind.
		ri, err := c.dynamicResourceFor(kind, namespace)
		if err != nil {
			return err
		}
		return ri.Delete(ctx, name, opts)
	}
}

// dynamicResourceFor resolves kind to a dynamic client handle, scoped to
// namespace when the kind is namespaced.
//
// It goes through resourceFor rather than getDynKind alone, so it answers
// for a discovered kind whose instance informer has never started — which
// is the ordinary case for a write reached from a screen that didn't have
// to list the kind first.
func (c *Cluster) dynamicResourceFor(kind ResourceKind, namespace string) (dynamic.ResourceInterface, error) {
	if c.dynClient == nil {
		return nil, fmt.Errorf("%s is not supported: no dynamic client", kind)
	}
	gvr, clusterScoped, ok := c.resourceFor(kind)
	if !ok {
		return nil, fmt.Errorf("%s is not a known resource kind", kind)
	}
	res := c.dynClient.Resource(gvr)
	if clusterScoped || namespace == "" {
		return res, nil
	}
	return res.Namespace(namespace), nil
}

// patchDynamic issues one JSON merge patch against a dynamically-resolved
// kind — the shared body of every write that has to reach a kind outside
// the typed clientset switches above.
func (c *Cluster) patchDynamic(ctx context.Context, kind ResourceKind, namespace, name string, patch []byte) error {
	ri, err := c.dynamicResourceFor(kind, namespace)
	if err != nil {
		return err
	}
	_, err = ri.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// RolloutRestart patches kind's pod template annotations with a fresh
// restartedAt timestamp — the same strategic-merge patch `kubectl rollout
// restart` issues — which the owning controller treats as a template change
// and rolls out. Deployment, StatefulSet, and DaemonSet all accept the
// identical patch shape via their own typed AppsV1 clients.
func (c *Cluster) RolloutRestart(ctx context.Context, kind ResourceKind, namespace, name string) error {
	if name == "" {
		return fmt.Errorf("cannot restart rollout: empty name")
	}
	patch := fmt.Appendf(nil,
		`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		time.Now().Format(time.RFC3339),
	)
	var err error
	switch kind {
	case KindStatefulSet:
		_, err = c.clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	case KindDaemonSet:
		_, err = c.clientset.AppsV1().DaemonSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	default:
		_, err = c.clientset.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	}
	return err
}

// jobTemplateGeneratedLabels are the labels the Job controller injects onto
// spec.template.metadata.labels (and mirrors into spec.selector.matchLabels)
// when they're unset at creation time — a clone must strip these so the API
// server regenerates fresh ones instead of colliding with the still-existing
// source Job's selector.
var jobTemplateGeneratedLabels = []string{
	"controller-uid", "batch.kubernetes.io/controller-uid",
	"job-name", "batch.kubernetes.io/job-name",
}

// CloneJobSpec builds a new JobSpec from src suitable for a from-scratch
// Create: the selector and its generated template labels are cleared so the
// API server assigns fresh ones (reusing the source Job's own would collide
// with it, since that Job still exists), and Suspend is forced to false
// regardless of src's own state — a retry should actually run. Exported so
// fake.Cluster's own RetryJob can share it rather than drifting on which
// fields get stripped.
func CloneJobSpec(src *batchv1.JobSpec) *batchv1.JobSpec {
	spec := src.DeepCopy()
	spec.Selector = nil
	spec.ManualSelector = nil
	suspend := false
	spec.Suspend = &suspend
	for _, key := range jobTemplateGeneratedLabels {
		delete(spec.Template.Labels, key)
	}
	return spec
}

// StripRerunSourceAnnotations removes from a rerun clone's annotations
// everything that ties it to where the source Job came from. A rerun is
// always standalone (§37c doc comment on the Mutator interface): no CronJob
// association, real or Kute's own, so the clone never inherits history-limit
// GC or a stale "cronjob/x" SOURCE reading; and no Helm hook/ownership
// annotations (helm.sh/hook*, meta.helm.sh/*), which would otherwise outrank
// Kute's own manual tag and read "helm/<release> <hook>" instead of
// "manual · <creator>". Exported so fake.Cluster's RetryJob strips the same.
func StripRerunSourceAnnotations(annotations map[string]string) {
	delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
	delete(annotations, AnnotationCronJobUID)
	delete(annotations, AnnotationCronJobName)
	for key := range annotations {
		if strings.HasPrefix(key, "helm.sh/hook") || strings.HasPrefix(key, "meta.helm.sh/") {
			delete(annotations, key)
		}
	}
}

// RetryJob clones name's spec into a new Job called newName — get, strip
// the fields the API server must regenerate, create. The source Job (its
// Status, events, history) is never touched: this is what
// `kubectl create job newName --from=job/name` does, not a delete+recreate.
// The clone is deliberately detached from any parent (no OwnerReferences),
// so a manual retry of a CronJob-spawned Job becomes a standalone object
// rather than something the CronJob's own history limit can silently GC.
func (c *Cluster) RetryJob(ctx context.Context, namespace, name, newName, creator string, at time.Time) error {
	if name == "" || newName == "" {
		return fmt.Errorf("cannot retry job: empty name")
	}
	old, err := c.clientset.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	clone := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        newName,
			Namespace:   namespace,
			Labels:      maps.Clone(old.Labels),
			Annotations: maps.Clone(old.Annotations),
		},
		Spec: *CloneJobSpec(&old.Spec),
	}
	if clone.Annotations == nil {
		clone.Annotations = map[string]string{}
	}
	StripRerunSourceAnnotations(clone.Annotations)
	clone.Annotations[AnnotationTriggeredBy] = creator
	clone.Annotations[AnnotationTriggeredAt] = at.UTC().Format(time.RFC3339)
	_, err = c.clientset.BatchV1().Jobs(namespace).Create(ctx, clone, metav1.CreateOptions{})
	return err
}

// ReplaceJob implements the Mutator interface: get, delete (Background
// propagation, UID precondition), wait until the old object is actually
// gone, then create-under-the-same-name from the deleted Job's own spec. A
// failure after the delete says so explicitly ("original deleted, …") rather
// than surfacing a bare AlreadyExists. Like CloneJobSpec/RetryJob,
// Suspend is forced false and the selector/generated template labels are
// stripped so the API server assigns fresh ones — a replace should actually
// run, not silently inherit a paused/selector-collision state. Unlike
// RetryJob's clone, this keeps whatever OwnerReferences/CronJob-association
// annotations the source carried (a replace is meant to stand in for the
// exact same object, not detach it) — only Retry's clone is deliberately
// always standalone.
func (c *Cluster) ReplaceJob(ctx context.Context, namespace, name string) error {
	if name == "" {
		return fmt.Errorf("cannot replace job: empty name")
	}
	jobs := c.clientset.BatchV1().Jobs(namespace)
	old, err := jobs.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	// Background propagation (not the Job default, orphan) so the old
	// attempt's pods go with it, and a UID precondition so a Job someone
	// else recreated between the Get and here is never the one deleted.
	opts := backgroundDelete()
	opts.Preconditions = metav1.NewUIDPreconditions(string(old.UID))
	if err := jobs.Delete(ctx, name, opts); err != nil {
		return err
	}
	// The delete returns while the object can still exist (finalizers,
	// graceful deletion); a Create in that window fails AlreadyExists and
	// leaves the user with neither Job. Wait, bounded, until it's gone.
	if err := c.waitJobGone(ctx, namespace, name, old.UID); err != nil {
		return fmt.Errorf("replace job/%s: original deleted, replacement not created: %w", name, err)
	}
	replacement := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          maps.Clone(old.Labels),
			Annotations:     maps.Clone(old.Annotations),
			OwnerReferences: old.OwnerReferences,
		},
		Spec: *CloneJobSpec(&old.Spec),
	}
	delete(replacement.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
	if _, err := jobs.Create(ctx, replacement, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("replace job/%s: original deleted, recreate failed: %w", name, err)
	}
	return nil
}

// replaceJobGoneTimeout bounds ReplaceJob's wait for the deleted Job to
// disappear; Cluster.replaceJobGoneTimeout overrides it (tests).
const replaceJobGoneTimeout = 30 * time.Second

// waitJobGone polls until job name with UID uid no longer exists — NotFound,
// or the name now belongs to a different object, which is reported as an
// error since creating over it would fail anyway. It stops on ctx and on
// replaceJobGoneTimeout, whichever comes first.
func (c *Cluster) waitJobGone(ctx context.Context, namespace, name string, uid types.UID) error {
	timeout := c.replaceJobGoneTimeout
	if timeout <= 0 {
		timeout = replaceJobGoneTimeout
	}
	jobs := c.clientset.BatchV1().Jobs(namespace)
	var recreated bool
	err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		cur, err := jobs.Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return true, nil
		case err == nil && cur.UID != uid:
			recreated = true
			return true, nil
		}
		// Still there, or a transient read failure: keep waiting until the bound.
		return false, nil
	})
	if recreated {
		return fmt.Errorf("a different job/%s was created while the original was deleting", name)
	}
	if err != nil {
		return fmt.Errorf("still terminating after %s: %w", timeout, err)
	}
	return nil
}

// SetJobSuspend patches spec.suspend on a Job — the same JSON body
// SetFluxSuspend uses, but through the typed clientset (KindJob has one;
// Flux's discovered CRDs don't), matching deleteResource/PatchMeta's
// existing KindJob branches.
func (c *Cluster) SetJobSuspend(ctx context.Context, namespace, name string, suspend bool) error {
	if name == "" {
		return fmt.Errorf("cannot suspend job: empty name")
	}
	patch := fmt.Appendf(nil, `{"spec":{"suspend":%t}}`, suspend)
	_, err := c.clientset.BatchV1().Jobs(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// TriggerCronJob creates newJobName from name's own spec.jobTemplate — get,
// copy the template's metadata/spec, stamp Kute's manual-run annotations,
// create. The source CronJob (its schedule, its own spawned-Job history) is
// never touched: this is what `kubectl create job newJobName --from=cronjob/name`
// does, plus the association/attribution annotations documented on the
// Mutator interface. Like RetryJob, the new Job is deliberately detached
// from any parent (no OwnerReferences), so a manual trigger doesn't fall
// under the CronJob's own history-limit GC.
func (c *Cluster) TriggerCronJob(ctx context.Context, namespace, name, newJobName, creator string, at time.Time) error {
	if name == "" || newJobName == "" {
		return fmt.Errorf("cannot trigger cronjob: empty name")
	}
	cj, err := c.clientset.BatchV1().CronJobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
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
	// The real annotation `kubectl create job --from=cronjob` stamps, so a
	// manually-triggered run reads the same in kute or the CLI.
	job.Annotations["cronjob.kubernetes.io/instantiate"] = "manual"
	// Kute's own association/attribution annotations (§4.2/§36b) — how
	// resources.cronJobAssociation finds this ownerless Job again, and who/
	// when JobSummary.Creator reports.
	job.Annotations[AnnotationCronJobName] = cj.Name
	job.Annotations[AnnotationCronJobUID] = string(cj.UID)
	job.Annotations[AnnotationTriggeredBy] = creator
	job.Annotations[AnnotationTriggeredAt] = at.UTC().Format(time.RFC3339)
	delete(job.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
	_, err = c.clientset.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("%w: %w", ErrManualJobNameConflict, err)
	}
	return err
}

// SetCronJobSuspend patches spec.suspend on a CronJob, stamping/clearing
// Kute's own suspend-timestamp annotations in the same atomic patch — see
// the Mutator interface doc comment for the full reasoning. Unlike
// SetJobSuspend, this touches nothing already running: suspend=true only
// stops the controller from creating *future* Jobs on schedule.
func (c *Cluster) SetCronJobSuspend(ctx context.Context, namespace, name string, suspend bool, resourceVersion string, currentGeneration int64, at time.Time) error {
	if name == "" {
		return fmt.Errorf("cannot suspend cronjob: empty name")
	}
	if resourceVersion == "" {
		return fmt.Errorf("cannot suspend cronjob %q: missing resourceVersion precondition", name)
	}
	var annotations map[string]any
	if suspend {
		annotations = map[string]any{
			AnnotationSuspendedAt:         at.UTC().Format(time.RFC3339),
			AnnotationSuspendedGeneration: strconv.FormatInt(currentGeneration+1, 10),
		}
	} else {
		// A JSON merge patch value of null removes the key entirely — the
		// same idiom metaPatchJSON/secretDataPatchJSON use for a removal.
		annotations = map[string]any{
			AnnotationSuspendedAt:         nil,
			AnnotationSuspendedGeneration: nil,
		}
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"resourceVersion": resourceVersion,
			"annotations":     annotations,
		},
		"spec": map[string]any{"suspend": suspend},
	})
	if err != nil {
		return err
	}
	_, err = c.clientset.BatchV1().CronJobs(namespace).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}

// SetCronJobSchedule atomically patches spec.schedule and (optionally)
// spec.timeZone on a CronJob — see the Mutator interface doc comment for
// the set/clear/untouched timezone contract and the resourceVersion
// precondition.
func (c *Cluster) SetCronJobSchedule(ctx context.Context, namespace, name string, edit CronJobScheduleEdit) (CronJobScheduleResult, error) {
	if name == "" {
		return CronJobScheduleResult{}, fmt.Errorf("cannot set schedule: empty cronjob name")
	}
	if edit.ResourceVersion == "" {
		return CronJobScheduleResult{}, fmt.Errorf("cannot set schedule for %q: missing resourceVersion precondition", name)
	}
	spec := map[string]any{"schedule": edit.Schedule}
	if edit.TimeZone != nil {
		if *edit.TimeZone == "" {
			spec["timeZone"] = nil // explicit clear, never a stale leftover string
		} else {
			spec["timeZone"] = *edit.TimeZone
		}
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": edit.ResourceVersion},
		"spec":     spec,
	})
	if err != nil {
		return CronJobScheduleResult{}, err
	}
	updated, err := c.clientset.BatchV1().CronJobs(namespace).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
	if err != nil {
		return CronJobScheduleResult{}, err
	}
	result := CronJobScheduleResult{
		Schedule:        updated.Spec.Schedule,
		ResourceVersion: updated.ResourceVersion,
	}
	if updated.Spec.TimeZone != nil {
		result.TimeZone = *updated.Spec.TimeZone
	}
	return result, nil
}

// Cordon sets (cordon=true) or clears spec.unschedulable on node via a
// strategic-merge patch.
func (c *Cluster) Cordon(ctx context.Context, node string, cordon bool) error {
	if node == "" {
		return fmt.Errorf("cannot cordon: empty node name")
	}
	patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, cordon)
	_, err := c.clientset.CoreV1().Nodes().Patch(ctx, node, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// Drain cordons node, then evicts every pod scheduled on it except
// DaemonSet-owned and mirror (static) pods, which a cordon+evict cycle
// can't move anywhere else. Returns the count evicted; a partial failure
// returns that count alongside a joined error for whichever pods failed.
func (c *Cluster) Drain(ctx context.Context, node string) (int, error) {
	if node == "" {
		return 0, fmt.Errorf("cannot drain: empty node name")
	}
	if err := c.Cordon(ctx, node, true); err != nil {
		return 0, fmt.Errorf("cordon %s before drain: %w", node, err)
	}

	// The server narrows the list to this node's pods (spec.nodeName is a
	// field selector every API server honors for Pods — what kubectl drain
	// sends), so draining one node never pulls the whole cluster's pods over
	// the wire. The NodeName check below stays, since the client-go fake
	// clientset ignores field selectors.
	pods, err := c.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", node).String(),
	})
	if err != nil {
		return 0, fmt.Errorf("list pods on %s: %w", node, err)
	}

	evicted := 0
	var errs []error
	for _, pod := range pods.Items {
		if pod.Spec.NodeName != node || !DrainEvicts(&pod) {
			continue
		}
		eviction := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		}
		if err := c.clientset.PolicyV1().Evictions(pod.Namespace).Evict(ctx, eviction); err != nil {
			errs = append(errs, fmt.Errorf("evict %s/%s: %w", pod.Namespace, pod.Name, err))
			continue
		}
		evicted++
	}
	return evicted, errors.Join(errs...)
}

// HelmRollback shells out to the real `helm` binary (kube/helm.go's
// HelmRollback) — the live clientset has no equivalent API call to make;
// Helm's own storage/rollback logic isn't reproduced here. Pinned to this
// Cluster's own context (CommandTarget), never helm's current-context.
func (c *Cluster) HelmRollback(ctx context.Context, namespace, name string, toRevision int) error {
	return HelmRollback(ctx, c.CommandTarget(), namespace, name, toRevision)
}

// SetFluxSuspend implements Mutator: a one-field merge patch on spec.suspend.
func (c *Cluster) SetFluxSuspend(ctx context.Context, kind ResourceKind, namespace, name string, suspend bool) error {
	if name == "" {
		return fmt.Errorf("cannot suspend %s: empty name", kind)
	}
	return c.patchDynamic(ctx, kind, namespace, name, fmt.Appendf(nil, `{"spec":{"suspend":%t}}`, suspend))
}

// RequestFluxReconcile implements Mutator by reusing PatchMeta rather than
// opening a second annotation path: the reconcile request *is* an ordinary
// metadata write, and PatchMeta already owns the typed-vs-dynamic dispatch
// for every kind.
func (c *Cluster) RequestFluxReconcile(ctx context.Context, kind ResourceKind, namespace, name string) error {
	return c.PatchMeta(ctx, kind, namespace, name, true,
		FluxReconcileAnnotation, time.Now().UTC().Format(time.RFC3339), false)
}

// RequestArgoRefresh implements Mutator the same way RequestFluxReconcile
// does: the refresh request is an ordinary metadata write, so it reuses
// PatchMeta rather than opening a second annotation path.
func (c *Cluster) RequestArgoRefresh(ctx context.Context, kind ResourceKind, namespace, name string) error {
	return c.PatchMeta(ctx, kind, namespace, name, true, ArgoRefreshAnnotation, "normal", false)
}

// RequestArgoSync implements Mutator: a merge patch on operation.sync.
// revision plus operation.initiatedBy, the API write `argocd app sync`
// performs (the CLI fills initiatedBy.username from its session too). Not a spec field —
// .operation is Argo's own imperative trigger the application-controller
// watches for and clears once handled — so patchDynamic's plain merge
// patch is the right tool, same idiom SetFluxSuspend uses for spec.suspend.
func (c *Cluster) RequestArgoSync(ctx context.Context, kind ResourceKind, namespace, name, revision, initiator string) error {
	if name == "" {
		return fmt.Errorf("cannot sync %s: empty name", kind)
	}
	return c.patchDynamic(ctx, kind, namespace, name, ArgoSyncPatch(revision, initiator))
}

// DefaultArgoSyncInitiator is who a sync is credited to when the
// connect-time SelfSubjectReview didn't answer: the tool, never a guessed
// person and never auto-sync.
const DefaultArgoSyncInitiator = "kute"

// ArgoSyncInitiator is the operation.initiatedBy.username for a sync kute
// starts: the reviewed identity's username when known, else
// DefaultArgoSyncInitiator.
func ArgoSyncInitiator(id ReviewedIdentity, reviewed bool) string {
	if reviewed && id.Username != "" {
		return id.Username
	}
	return DefaultArgoSyncInitiator
}

// ArgoSyncPatch is the literal merge-patch body RequestArgoSync sends and
// ArgoSyncCommandString prints — one builder so the two can't drift. Its
// initiatedBy matches Argo CD's OperationInitiator (username, automated):
// a manual sync, attributed. An empty initiator is DefaultArgoSyncInitiator.
func ArgoSyncPatch(revision, initiator string) []byte {
	if initiator == "" {
		initiator = DefaultArgoSyncInitiator
	}
	patch, _ := json.Marshal(map[string]any{
		"operation": map[string]any{
			"initiatedBy": map[string]any{"username": initiator, "automated": false},
			"sync":        map[string]any{"revision": revision},
		},
	})
	return patch
}

// renewCertificatePatch is the literal merge-patch body RenewCertificate
// sends and RenewCertificateCommandString documents — kept as one constant
// so the two can never drift apart.
const renewCertificatePatch = `{"status":{"conditions":[{"type":"Issuing","status":"True","reason":"ManuallyTriggered"}]}}`

// RenewCertificate implements Mutator: one merge patch against the status
// subresource. See the Mutator interface doc comment for why a plain merge
// patch — rather than a read-modify-write that preserves every other
// condition — is the deliberate choice here.
func (c *Cluster) RenewCertificate(ctx context.Context, namespace, name string) error {
	if name == "" {
		return fmt.Errorf("cannot renew certificate: empty name")
	}
	ri, err := c.dynamicResourceFor(KindCertificate, namespace)
	if err != nil {
		return err
	}
	_, err = ri.Patch(ctx, name, types.MergePatchType, []byte(renewCertificatePatch), metav1.PatchOptions{}, "status")
	return err
}

// FluxSuspendCommandString renders the kubectl patch kute actually issues —
// not the `flux suspend kustomization podinfo` it is equivalent to.
// §27a's rule (the will-run line names the command that *executes*)
// outranks naming the more familiar one, and kute runs no flux binary.
func FluxSuspendCommandString(kind ResourceKind, namespace, name string, suspend bool) string {
	cmd := fmt.Sprintf(`kubectl patch %s/%s --type merge -p '{"spec":{"suspend":%t}}'`,
		kind.ResourceArg(), name, suspend)
	if namespace != "" {
		cmd += " -n " + namespace
	}
	return cmd
}

// JobRetryCommandString renders the kubectl equivalent of RetryJob — the
// real `kubectl create job` idiom for cloning an existing Job, even though
// kute's own implementation is a get+strip+create rather than a single CLI
// invocation. newName is precomputed by the caller so this line and the
// actual Create call always agree.
func JobRetryCommandString(namespace, name, newName string) string {
	return fmt.Sprintf("kubectl create job %s --from=job/%s -n %s", newName, name, namespace)
}

// JobSuspendCommandString renders the kubectl patch kute actually issues for
// SetJobSuspend — same JSON body as FluxSuspendCommandString, just on a Job
// rather than a Flux kind.
func JobSuspendCommandString(namespace, name string, suspend bool) string {
	return fmt.Sprintf(`kubectl patch job/%s --type merge -p '{"spec":{"suspend":%t}}' -n %s`, name, suspend, namespace)
}

// JobReplaceCommandString documents ReplaceJob's two-step recipe honestly
// rather than inventing a single kubectl line — there is no such one-liner,
// since the source is gone the moment the delete lands.
func JobReplaceCommandString(namespace, name string) string {
	return fmt.Sprintf("kubectl delete job/%s -n %s && kubectl create job %s --from=job/%s -n %s (kute's own recipe — no source remains for the second step by the time it runs)",
		name, namespace, name, name, namespace)
}

// CronJobTriggerCommandString renders the kubectl equivalent of
// TriggerCronJob — newJobName is precomputed by the caller so this line and
// the actual Create call always agree, the same reason JobRetryCommandString
// takes newName rather than deriving it.
func CronJobTriggerCommandString(namespace, name, newJobName string) string {
	return fmt.Sprintf("kubectl create job %s --from=cronjob/%s -n %s", newJobName, name, namespace)
}

// CronJobSuspendCommandString renders the kubectl patch kute actually issues
// for SetCronJobSuspend — same JSON body JobSuspendCommandString uses, on a
// CronJob rather than a Job.
func CronJobSuspendCommandString(namespace, name string, suspend bool) string {
	return fmt.Sprintf(`kubectl patch cronjob/%s --type merge -p '{"spec":{"suspend":%t}}' -n %s`, name, suspend, namespace)
}

// CronJobSetScheduleCommandString renders the kubectl patch kute actually
// issues for SetCronJobSchedule. timezone follows CronJobScheduleEdit.TimeZone's
// own three-state contract: nil omits spec.timeZone from the patch entirely
// (untouched), a pointer to "" renders an explicit JSON null (clear), and a
// pointer to a non-empty IANA name renders that string (set) — so the
// will-run line shown before a commit is byte-for-byte what
// SetCronJobSchedule's own json.Marshal produces, not a hand-approximated
// second rendering that could drift from it.
func CronJobSetScheduleCommandString(namespace, name, schedule string, timezone *string) string {
	spec := map[string]any{"schedule": schedule}
	if timezone != nil {
		if *timezone == "" {
			spec["timeZone"] = nil
		} else {
			spec["timeZone"] = *timezone
		}
	}
	body, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		// Never reached — spec is a small map of strings/nil built above,
		// which always marshals — but a will-run line must never panic.
		return fmt.Sprintf(`kubectl patch cronjob/%s --type merge -p '{"spec":{"schedule":%q}}' -n %s`, name, schedule, namespace)
	}
	return fmt.Sprintf("kubectl patch cronjob/%s --type merge -p '%s' -n %s", name, body, namespace)
}

// FluxReconcileCommandString renders the kubectl annotate kute issues for
// §30a's 'r'. at is passed in rather than read from the clock so the
// caller's will-run line and the patch it previews carry the same instant.
func FluxReconcileCommandString(kind ResourceKind, namespace, name string, at time.Time) string {
	cmd := fmt.Sprintf(`kubectl annotate %s/%s %s=%q --overwrite`,
		kind.ResourceArg(), name, FluxReconcileAnnotation, at.UTC().Format(time.RFC3339))
	if namespace != "" {
		cmd += " -n " + namespace
	}
	return cmd
}

// ArgoRefreshCommandString renders the kubectl annotate kute issues for
// §33a's 'r' — not the `argocd app get --refresh` it is equivalent to,
// same §27a "names the command that executes" rule FluxReconcileCommandString
// follows.
func ArgoRefreshCommandString(kind ResourceKind, namespace, name string) string {
	cmd := fmt.Sprintf(`kubectl annotate %s/%s %s=normal --overwrite`,
		kind.ResourceArg(), name, ArgoRefreshAnnotation)
	if namespace != "" {
		cmd += " -n " + namespace
	}
	return cmd
}

// ArgoSyncCommandString renders the kubectl patch kute actually issues for
// §33a's 'S' — not the `argocd app sync` it is equivalent to, same rule.
func ArgoSyncCommandString(kind ResourceKind, namespace, name, revision, initiator string) string {
	cmd := fmt.Sprintf(`kubectl patch %s/%s --type merge -p '%s'`,
		kind.ResourceArg(), name, ArgoSyncPatch(revision, initiator))
	if namespace != "" {
		cmd += " -n " + namespace
	}
	return cmd
}

// RenewCertificateCommandString renders the exact kubectl patch
// RenewCertificate issues — not `cmctl renew`, the §27a rule every other
// CommandString here follows: cmctl is what a human would type, but kute
// never shells out to it, and printing a command the user may not have
// installed breaks the will-run contract (docs/design/v.0.7.0.dc.html
// §35c: "what's shown is what runs"). --subresource=status is what makes
// this a status write rather than a no-op spec patch.
func RenewCertificateCommandString(namespace, name string) string {
	return fmt.Sprintf(`kubectl patch %s/%s --type merge -p '%s' -n %s --subresource=status`,
		KindCertificate.ResourceArg(), name, renewCertificatePatch, namespace)
}

// rolloutUndoSkippedAnnotations are the Deployment annotations RolloutUndo
// keeps from the Deployment itself rather than copying from the target
// ReplicaSet — kubectl's DeploymentRollbacker annotationsToSkip, verbatim.
var rolloutUndoSkippedAnnotations = map[string]bool{
	corev1.LastAppliedConfigAnnotation:          true,
	"deployment.kubernetes.io/revision":         true,
	"deployment.kubernetes.io/revision-history": true,
	"deployment.kubernetes.io/desired-replicas": true,
	"deployment.kubernetes.io/max-replicas":     true,
	appsv1.DeprecatedRollbackTo:                 true,
}

// RolloutUndo finds the ReplicaSet owned by name whose
// "deployment.kubernetes.io/revision" annotation equals toRevision and
// restores it exactly as `kubectl rollout undo` does: a JSON patch that
// *replaces* /spec/template with the ReplicaSet's template (minus the
// controller-added pod-template-hash label) and /metadata/annotations
// with the ReplicaSet's own, keeping kubectl's skip-list from the
// Deployment. A strategic merge patch is wrong here — it merges lists by
// key and never drops absent map keys, so env vars, containers and
// annotations added after toRevision would survive the "rollback". A
// leading `test` op on metadata.resourceVersion makes the Get the patch
// was computed from a precondition. See the Mutator interface doc comment
// for why this (rather than a dedicated rollback API call) is the correct
// mechanism.
func (c *Cluster) RolloutUndo(ctx context.Context, namespace, name string, toRevision int) error {
	if name == "" {
		return fmt.Errorf("cannot roll back: empty deployment name")
	}
	dep, err := c.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get deployment %s: %w", name, err)
	}
	if dep.Spec.Paused {
		return fmt.Errorf("cannot roll back a paused deployment; resume it first")
	}
	rsList, err := c.clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list replicasets for %s: %w", name, err)
	}
	target, ok := replicaSetForRevision(rsList.Items, name, toRevision)
	if !ok {
		return fmt.Errorf("deployment %q has no revision %d to roll back to", name, toRevision)
	}
	template, annotations := RolloutUndoTarget(dep, target)

	patch, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/resourceVersion", "value": dep.ResourceVersion},
		{"op": "replace", "path": "/spec/template", "value": template},
		{"op": "replace", "path": "/metadata/annotations", "value": annotations},
	})
	if err != nil {
		return fmt.Errorf("encode revision %d template: %w", toRevision, err)
	}
	_, err = c.clientset.AppsV1().Deployments(namespace).Patch(ctx, name, types.JSONPatchType, patch, metav1.PatchOptions{})
	return err
}

// RolloutUndoTarget computes what `kubectl rollout undo` writes back onto
// dep to restore rs's revision: rs's pod template minus the controller's
// pod-template-hash label, and rs's annotations with kubectl's skip-list
// carried over from dep instead. Shared by Cluster.RolloutUndo and
// fake.Cluster's so --demo restores exactly what a real cluster would.
func RolloutUndoTarget(dep *appsv1.Deployment, rs *appsv1.ReplicaSet) (*corev1.PodTemplateSpec, map[string]string) {
	template := rs.Spec.Template.DeepCopy()
	delete(template.Labels, appsv1.DefaultDeploymentUniqueLabelKey)

	annotations := map[string]string{}
	for k := range rolloutUndoSkippedAnnotations {
		if v, ok := dep.Annotations[k]; ok {
			annotations[k] = v
		}
	}
	for k, v := range rs.Annotations {
		if !rolloutUndoSkippedAnnotations[k] {
			annotations[k] = v
		}
	}
	return template, annotations
}

// replicaSetForRevision finds the ReplicaSet owned by deploymentName whose
// revision annotation equals toRevision — shared by Cluster.RolloutUndo and
// fake.Cluster's own lookup would duplicate this, but the fake cluster
// works over its own in-memory []runtime.Object slice rather than a
// clientset List result, so it keeps its own copy per the repo's
// package-local-seam convention.
func replicaSetForRevision(items []appsv1.ReplicaSet, deploymentName string, toRevision int) (*appsv1.ReplicaSet, bool) {
	for i := range items {
		rs := &items[i]
		if len(rs.OwnerReferences) == 0 || rs.OwnerReferences[0].Kind != "Deployment" || rs.OwnerReferences[0].Name != deploymentName {
			continue
		}
		rev, _ := strconv.Atoi(rs.Annotations["deployment.kubernetes.io/revision"])
		if rev == toRevision {
			return rs, true
		}
	}
	return nil, false
}

// RolloutUndoCommandString renders the exact `kubectl rollout undo`
// invocation RolloutUndo runs — 16b's "will run" documentation line (the
// same copyable-command idiom as 10a/13a/17b/18a).
func RolloutUndoCommandString(namespace, name string, toRevision int) string {
	return fmt.Sprintf("kubectl rollout undo %s/%s --to-revision=%d -n %s", workloadResourceArg(KindDeployment), name, toRevision, namespace)
}

// Scale patches kind's spec.replicas via the same strategic-merge-patch
// idiom as Cordon/RolloutRestart, rather than the clientset's dedicated
// UpdateScale call — one fewer API shape to special-case per kind.
func (c *Cluster) Scale(ctx context.Context, kind ResourceKind, namespace, name string, replicas int32) error {
	if name == "" {
		return fmt.Errorf("cannot scale %s: empty name", kind)
	}
	patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
	var err error
	switch kind {
	case KindDeployment:
		_, err = c.clientset.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	case KindStatefulSet:
		_, err = c.clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	default:
		err = fmt.Errorf("scale is not supported for kind %s", kind)
	}
	return err
}

// ScaleCommandString renders the exact `kubectl scale` invocation Scale
// runs — 17b's "will run" documentation line (the same copyable-command
// idiom as 10a/13a/18a).
func ScaleCommandString(kind ResourceKind, namespace, name string, replicas int32) string {
	return fmt.Sprintf("kubectl scale %s/%s --replicas=%d -n %s", workloadResourceArg(kind), name, replicas, namespace)
}

// SetImage patches container's image on kind's pod template via the same
// strategic-merge-patch idiom as Scale/Cordon — the container list patches
// by its "name" merge key, so no per-kind container index lookup is needed.
// CronJob nests the pod template one level deeper under spec.jobTemplate.
func (c *Cluster) SetImage(ctx context.Context, kind ResourceKind, namespace, name, container, image string, initContainer bool) error {
	if name == "" {
		return fmt.Errorf("cannot set image on %s: empty name", kind)
	}
	containerField := "containers"
	if initContainer {
		containerField = "initContainers"
	}
	patch := fmt.Sprintf(
		`{"spec":{"template":{"spec":{"%s":[{"name":%s,"image":%s}]}}}}`,
		containerField, jsonString(container), jsonString(image),
	)
	if kind == KindCronJob {
		patch = fmt.Sprintf(
			`{"spec":{"jobTemplate":{"spec":{"template":{"spec":{"%s":[{"name":%s,"image":%s}]}}}}}}`,
			containerField, jsonString(container), jsonString(image),
		)
	}
	var err error
	switch kind {
	case KindDeployment:
		_, err = c.clientset.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	case KindStatefulSet:
		_, err = c.clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	case KindDaemonSet:
		_, err = c.clientset.AppsV1().DaemonSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	case KindCronJob:
		_, err = c.clientset.BatchV1().CronJobs(namespace).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	default:
		err = fmt.Errorf("set image is not supported for kind %s", kind)
	}
	return err
}

// SetImageCommandString renders the exact `kubectl set image` invocation
// SetImage runs — 24a's "will run" documentation line (the same
// copyable-command idiom as 10a/13a/17b/18a).
func SetImageCommandString(kind ResourceKind, namespace, name, container, image string) string {
	return fmt.Sprintf("kubectl set image %s/%s %s=%s -n %s", workloadResourceArg(kind), name, container, image, namespace)
}

// ResourceEdits is 25a's set of changed container resources.requests/limits
// fields. nil = unchanged (omitted from the patch entirely); a pointer to
// "" = an explicit unset (the 'u' key — patches that resource key to JSON
// null, removing it rather than merely not mentioning it, since "no limit"
// is a real and dangerous state the editor must be able to set
// deliberately); a pointer to a quantity string ("250m", "512Mi") = the new
// value.
type ResourceEdits struct {
	CPURequest *string
	CPULimit   *string
	MEMRequest *string
	MEMLimit   *string
}

// anyUnset reports whether edits carries at least one explicit unset (a
// non-nil pointer to "").
func (e ResourceEdits) anyUnset() bool {
	return isUnsetField(e.CPURequest) || isUnsetField(e.CPULimit) || isUnsetField(e.MEMRequest) || isUnsetField(e.MEMLimit)
}

func isUnsetField(p *string) bool {
	return p != nil && *p == ""
}

// SetResources patches container's resources.requests/limits via the same
// containers-by-name strategic-merge-patch idiom as SetImage/Scale — the
// container list patches by its "name" merge key, so one patch shape covers
// Deployment/StatefulSet/DaemonSet without a per-kind container index
// lookup. Only fields edits sets are included in the patch; an explicit
// unset serializes as JSON null under its resource key, which both
// strategic-merge-patch and a plain JSON merge patch interpret as "remove
// this key".
//
// LimitRange/ResourceQuota admission is real-API-server-only behavior — the
// fake clientset's tracker performs no admission checks, so a dry-run
// against kube/fake always succeeds regardless of the edited values; only
// the client-side request>limit check (25a's own validation, before this is
// ever called) is exercisable against the fake cluster.
func (c *Cluster) SetResources(ctx context.Context, kind ResourceKind, namespace, name, container string, edits ResourceEdits, dryRun bool) error {
	if name == "" {
		return fmt.Errorf("cannot set resources on %s: empty name", kind)
	}
	patch, ok := resourcesPatchJSON(container, edits)
	if !ok {
		return fmt.Errorf("cannot set resources on %s/%s: no changed fields", kind, name)
	}
	opts := metav1.PatchOptions{}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	var err error
	switch kind {
	case KindDeployment:
		_, err = c.clientset.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), opts)
	case KindStatefulSet:
		_, err = c.clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), opts)
	case KindDaemonSet:
		_, err = c.clientset.AppsV1().DaemonSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), opts)
	default:
		err = fmt.Errorf("set resources is not supported for kind %s", kind)
	}
	return err
}

// resourcesPatchJSON builds the strategic-merge-patch document SetResources
// applies (and SetResourcesCommandString's kubectl-patch fallback documents
// verbatim) — shared so the "will run" line always names the literal patch
// that executes. ok is false when edits carries no changed fields at all.
func resourcesPatchJSON(container string, edits ResourceEdits) (patch string, ok bool) {
	requests := resourceFieldsJSON(edits.CPURequest, edits.MEMRequest)
	limits := resourceFieldsJSON(edits.CPULimit, edits.MEMLimit)
	if requests == "" && limits == "" {
		return "", false
	}
	var parts []string
	if requests != "" {
		parts = append(parts, `"requests":`+requests)
	}
	if limits != "" {
		parts = append(parts, `"limits":`+limits)
	}
	resourcesObj := "{" + strings.Join(parts, ",") + "}"
	return fmt.Sprintf(
		`{"spec":{"template":{"spec":{"containers":[{"name":%s,"resources":%s}]}}}}`,
		jsonString(container), resourcesObj,
	), true
}

// resourceFieldsJSON renders {"cpu":...,"memory":...} for a requests/limits
// pair, omitting nil fields and rendering an explicit unset (a pointer to
// "") as JSON null. Returns "" when both fields are nil.
func resourceFieldsJSON(cpu, mem *string) string {
	var parts []string
	if cpu != nil {
		parts = append(parts, `"cpu":`+quantityJSONLiteral(*cpu))
	}
	if mem != nil {
		parts = append(parts, `"memory":`+quantityJSONLiteral(*mem))
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func quantityJSONLiteral(v string) string {
	if v == "" {
		return "null"
	}
	return jsonString(v)
}

// jsonString renders s as a JSON string literal. Patch bodies are built
// with it rather than Go's %q/strconv.Quote, whose \x.. and \U........
// escapes (for control bytes, invalid UTF-8 and non-printable astral runes)
// are not JSON and get the whole patch rejected by the API server. HTML
// escaping is off so a will-run line that prints the body stays readable.
func jsonString(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// Never reached — encoding a Go string always succeeds (invalid
		// UTF-8 is coerced to U+FFFD).
		return `""`
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// SetResourcesCommandString renders the exact command SetResources runs —
// 25a's "will run" documentation line. A plain set (no unsets) renders as
// `kubectl set resources`, matching docs/design README.md §25a's own
// example; an explicit unset falls back to the literal `kubectl patch`
// invocation instead, since `kubectl set resources` has no clean field-
// removal flag — the will-run line always names the command that actually
// executes (27a/27b's same principle for their own patches).
func SetResourcesCommandString(kind ResourceKind, namespace, name, container string, edits ResourceEdits) string {
	if edits.anyUnset() {
		patch, _ := resourcesPatchJSON(container, edits)
		return fmt.Sprintf("kubectl patch %s/%s --type strategic -p '%s' -n %s", workloadResourceArg(kind), name, patch, namespace)
	}
	var reqParts, limParts []string
	if edits.CPURequest != nil {
		reqParts = append(reqParts, "cpu="+*edits.CPURequest)
	}
	if edits.MEMRequest != nil {
		reqParts = append(reqParts, "memory="+*edits.MEMRequest)
	}
	if edits.CPULimit != nil {
		limParts = append(limParts, "cpu="+*edits.CPULimit)
	}
	if edits.MEMLimit != nil {
		limParts = append(limParts, "memory="+*edits.MEMLimit)
	}
	cmd := fmt.Sprintf("kubectl set resources %s/%s -c %s", workloadResourceArg(kind), name, container)
	if len(limParts) > 0 {
		cmd += " --limits=" + strings.Join(limParts, ",")
	}
	if len(reqParts) > 0 {
		cmd += " --requests=" + strings.Join(reqParts, ",")
	}
	return cmd + " -n " + namespace
}

// PatchMeta implements Mutator against the live clientset. It mirrors
// deleteResource's per-kind switch (same kind list — the typed clients this
// app talks to), and like it falls back to the dynamic client by discovered
// GVR for any kind outside that switch, which is what makes both work on
// CRDs.
func (c *Cluster) PatchMeta(ctx context.Context, kind ResourceKind, namespace, name string, isAnnotation bool, key, value string, remove bool) error {
	if name == "" {
		return fmt.Errorf("cannot patch %s: empty name", kind)
	}
	patch := []byte(metaPatchJSON(isAnnotation, key, value, remove))
	cs := c.clientset
	var err error
	switch kind {
	case KindPod:
		_, err = cs.CoreV1().Pods(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindService:
		_, err = cs.CoreV1().Services(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindIngress:
		_, err = cs.NetworkingV1().Ingresses(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindConfigMap:
		_, err = cs.CoreV1().ConfigMaps(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindSecret:
		_, err = cs.CoreV1().Secrets(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindPersistentVolumeClaim:
		_, err = cs.CoreV1().PersistentVolumeClaims(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindEvent:
		_, err = cs.CoreV1().Events(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindDeployment:
		_, err = cs.AppsV1().Deployments(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindDaemonSet:
		_, err = cs.AppsV1().DaemonSets(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindStatefulSet:
		_, err = cs.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindReplicaSet:
		_, err = cs.AppsV1().ReplicaSets(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindJob:
		_, err = cs.BatchV1().Jobs(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindCronJob:
		_, err = cs.BatchV1().CronJobs(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindNamespace:
		_, err = cs.CoreV1().Namespaces().Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindNode:
		_, err = cs.CoreV1().Nodes().Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	default:
		// Via patchDynamic (and so resourceFor) rather than getDynKind, so
		// this works on a discovered kind whose instance informer never
		// started — 26a is reachable on an object the user arrived at
		// without browsing its list.
		err = c.patchDynamic(ctx, kind, namespace, name, patch)
	}
	return err
}

// metaPatchJSON builds the JSON merge patch (RFC 7386) body PatchMeta sends:
// a null value under the target key removes it (both the typed clientset's
// and the dynamic client's Patch accept types.MergePatchType identically, so
// one body shape covers every kind).
func metaPatchJSON(isAnnotation bool, key, value string, remove bool) string {
	field := "labels"
	if isAnnotation {
		field = "annotations"
	}
	kv := jsonString(key) + ":" + jsonString(value)
	if remove {
		kv = jsonString(key) + ":null"
	}
	return fmt.Sprintf(`{"metadata":{"%s":{%s}}}`, field, kv)
}

// MetaCommandString renders the exact `kubectl label`/`kubectl annotate`
// invocation PatchMeta runs — 26a's "will run" documentation line.
// `--overwrite` only appears when overwrite is true (an existing key being
// changed, not a brand-new one); a removal renders kubectl's `key-` suffix
// instead. `-n namespace` is omitted for a cluster-scoped kind (namespace
// == "").
func MetaCommandString(kind ResourceKind, namespace, name string, isAnnotation bool, key, value string, remove, overwrite bool) string {
	verb := "label"
	if isAnnotation {
		verb = "annotate"
	}
	arg := metaResourceArg(kind) + "/" + name
	cmd := fmt.Sprintf("kubectl %s %s %s-", verb, arg, key)
	if !remove {
		cmd = fmt.Sprintf("kubectl %s %s %s=%s", verb, arg, key, value)
		if overwrite {
			cmd += " --overwrite"
		}
	}
	if namespace != "" {
		cmd += " -n " + namespace
	}
	return cmd
}

// metaResourceArg renders kind as kubectl's resource arg for
// MetaCommandString — the same short forms workloadResourceArg already gives
// Deployment/StatefulSet/DaemonSet (matching docs/design README.md §26a's own
// `deploy/nva-worker` example), falling back to the lowercased kind name for
// every other kind (DeleteCommandString's own convention for arbitrary
// kinds, CRDs included).
func metaResourceArg(kind ResourceKind) string {
	switch kind {
	case KindDeployment, KindStatefulSet, KindDaemonSet:
		return workloadResourceArg(kind)
	default:
		return kind.ResourceArg()
	}
}

// PatchSecretData implements Mutator against the live clientset — a JSON
// merge patch (RFC 7386) targeting .stringData to add/edit (the server
// base64-encodes it and merges the result into .data) or .data with a null
// value to remove (.stringData is write-only and never itself holds a
// delete signal). Secret has no dynamic-client CRD fallback to speak of —
// it's always a typed core/v1 kind. The body carries
// metadata.resourceVersion, so the API server rejects it with 409 Conflict
// if the Secret changed since the caller read it.
func (c *Cluster) PatchSecretData(ctx context.Context, namespace, name, resourceVersion, key, value string, remove bool) (string, error) {
	if name == "" {
		return "", fmt.Errorf("cannot patch secret: empty name")
	}
	if resourceVersion == "" {
		return "", fmt.Errorf("cannot patch secret %q: missing resourceVersion precondition", name)
	}
	patch, err := secretDataPatchJSON(resourceVersion, key, value, remove)
	if err != nil {
		return "", err
	}
	updated, err := c.clientset.CoreV1().Secrets(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return "", err
	}
	return updated.ResourceVersion, nil
}

// secretDataPatchJSON builds the JSON merge patch body PatchSecretData
// sends — mirrors metaPatchJSON's null-removes-a-key shape, targeting
// .stringData to set (the server encodes/merges it into .data) or .data
// directly to remove (the only field a merge patch can null out — a
// Secret's stored representation never carries .stringData back), plus
// the metadata.resourceVersion precondition.
func secretDataPatchJSON(resourceVersion, key, value string, remove bool) ([]byte, error) {
	body := map[string]any{"metadata": map[string]any{"resourceVersion": resourceVersion}}
	if remove {
		body["data"] = map[string]any{key: nil}
	} else {
		body["stringData"] = map[string]any{key: value}
	}
	return json.Marshal(body)
}

// SecretDataCommandString renders the exact `kubectl patch` invocation
// PatchSecretData runs — 27b's "will run" documentation line. The value
// itself is always masked as a fixed six-dot placeholder, never the real
// secret text (docs/design README.md §27b: "copyable documentation must not
// leak the secret into scrollback or a shared screen") — a removal's null
// literal carries no secret material, so it renders verbatim.
func SecretDataCommandString(namespace, name, key string, remove bool) string {
	arg := "secret/" + name
	if remove {
		return fmt.Sprintf("kubectl patch %s --type merge -p '{\"data\":{%q:null}}' -n %s", arg, key, namespace)
	}
	return fmt.Sprintf("kubectl patch %s --type merge -p '{\"stringData\":{%q:\"••••••\"}}' -n %s", arg, key, namespace)
}

// PatchConfigMapData implements Mutator against the live clientset — a JSON
// merge patch (RFC 7386) targeting .data directly to set or (with a null
// value) remove a key. Unlike Secret, ConfigMap's .data is already plain
// text server-side, so there's no .stringData split to make. Like
// PatchSecretData, the body carries the metadata.resourceVersion
// precondition, so a concurrent change returns 409 Conflict.
func (c *Cluster) PatchConfigMapData(ctx context.Context, namespace, name, resourceVersion, key, value string, remove bool) (string, error) {
	if name == "" {
		return "", fmt.Errorf("cannot patch configmap: empty name")
	}
	if resourceVersion == "" {
		return "", fmt.Errorf("cannot patch configmap %q: missing resourceVersion precondition", name)
	}
	patch, err := configMapDataPatchJSON(resourceVersion, key, value, remove)
	if err != nil {
		return "", err
	}
	updated, err := c.clientset.CoreV1().ConfigMaps(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return "", err
	}
	return updated.ResourceVersion, nil
}

// configMapDataPatchJSON builds the JSON merge patch body PatchConfigMapData
// sends — a null value under the target key removes it (same idiom
// metaPatchJSON/secretDataPatchJSON use), otherwise sets it — plus the
// metadata.resourceVersion precondition.
func configMapDataPatchJSON(resourceVersion, key, value string, remove bool) ([]byte, error) {
	var v any = value
	if remove {
		v = nil
	}
	return json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": resourceVersion},
		"data":     map[string]any{key: v},
	})
}

// ConfigMapDataCommandString renders the exact `kubectl patch` invocation
// PatchConfigMapData runs — 27a's "will run" documentation line. Unlike
// SecretDataCommandString, the value is never masked: ConfigMap data isn't
// sensitive, and docs/design README.md §27a's own mockup line shows it
// verbatim (`kubectl patch cm/nva-config --type merge -p
// '{"data":{"LOG_LEVEL":"debug"}}' -n nva-stage`).
func ConfigMapDataCommandString(namespace, name, key, value string, remove bool) string {
	arg := "cm/" + name
	if remove {
		return fmt.Sprintf("kubectl patch %s --type merge -p '{\"data\":{%q:null}}' -n %s", arg, key, namespace)
	}
	return fmt.Sprintf("kubectl patch %s --type merge -p '{\"data\":{%q:%q}}' -n %s", arg, key, value, namespace)
}

// ConfigMapConsumerRestartCommandString renders the `kubectl rollout
// restart` invocation ctrl-r runs for one consumer — 27a's "prints every
// command it runs," one line per entry in ConfigMapConsumers alongside the
// patch itself.
func ConfigMapConsumerRestartCommandString(namespace string, ref ConfigMapConsumerRef) string {
	return fmt.Sprintf("kubectl rollout restart %s/%s -n %s", workloadResourceArg(ref.Kind), ref.Name, namespace)
}

// RolloutRestartCommandString renders the exact `kubectl rollout restart`
// invocation 9a's ctrl-r confirm runs on a Deployment/StatefulSet/DaemonSet
// row — same idiom as ConfigMapConsumerRestartCommandString, for the
// "prints every command it runs" documentation line the confirm shows
// before it executes.
func RolloutRestartCommandString(kind ResourceKind, namespace, name string) string {
	return fmt.Sprintf("kubectl rollout restart %s/%s -n %s", workloadResourceArg(kind), name, namespace)
}

// DeleteCommandString renders the exact `kubectl delete` invocation a 20a
// bulk delete runs — one call naming every marked object, the same
// copyable-documentation idiom as 10a/13a/17b/18a. namespace == "" (a
// cluster-scoped kind, or a marked set spanning more than one namespace in
// 6b's all-namespaces triage) omits `-n` rather than naming a namespace the
// command wouldn't actually be scoped to.
func DeleteCommandString(kind ResourceKind, namespace string, names []string) string {
	cmd := fmt.Sprintf("kubectl delete %s %s", kind.ResourceArg(), strings.Join(names, " "))
	if namespace != "" {
		cmd += " -n " + namespace
	}
	return cmd
}

// ForceDeleteCommandString renders the exact `kubectl delete ... --grace-period=0
// --force` invocation the non-prod inline confirm's force-delete sub-state
// (ctrl-k) runs, DeleteCommandString plus the two flags DeleteResourceForced
// actually passes to the API.
func ForceDeleteCommandString(kind ResourceKind, namespace, name string) string {
	return DeleteCommandString(kind, namespace, []string{name}) + " --grace-period=0 --force"
}

// workloadResourceArg renders kind as kubectl's short resource arg for a
// "will run" line (ScaleCommandString/SetImageCommandString) — Deployment is
// the default for any kind that isn't StatefulSet/DaemonSet/CronJob.
func workloadResourceArg(kind ResourceKind) string {
	switch kind {
	case KindStatefulSet:
		return "sts"
	case KindDaemonSet:
		return "ds"
	case KindCronJob:
		return "cronjob"
	default:
		return "deploy"
	}
}

// DrainEvicts reports whether Drain evicts pod: everything except
// DaemonSet-owned pods (the controller would recreate them on the cordoned
// node anyway) and mirror pods (the kubelet's static pods, which the API
// server can't evict). It is the one definition the drain confirm's "N pods
// will be evicted" count shares with Drain itself, so the two can't drift.
func DrainEvicts(pod *corev1.Pod) bool {
	if _, mirror := pod.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
		return false
	}
	return !slices.ContainsFunc(pod.OwnerReferences, func(ref metav1.OwnerReference) bool {
		return ref.Kind == "DaemonSet"
	})
}

// DrainImpact is what a drain would do to a node's pods — the drain
// confirm's consequence line ("4 evictable · 1 unmanaged (will be lost) · 1
// emptyDir (data lost)"). kubectl drain refuses the two risk buckets unless
// forced; kute evicts them, so the confirm has to say so before `y`.
//
//   - Evict counts every pod DrainEvicts says Drain evicts.
//   - Unmanaged is the subset of Evict with no controller ownerRef
//     (metav1.GetControllerOf — kubectl drain's own test): nothing
//     recreates it, so the pod is gone for good.
//   - EmptyDir is the subset of Evict mounting an emptyDir volume, whose
//     contents are deleted with the pod even when a controller recreates it.
//
// The risk buckets are independent, not a partition: a bare pod with an
// emptyDir counts once in each, since it loses both the pod and its scratch
// data, and each bucket must read as "how many pods have this problem".
// Neither ever counts a pod Drain leaves in place (DaemonSet, mirror).
type DrainImpact struct {
	Evict, Unmanaged, EmptyDir int
}

// Add tallies pod into d. Callers filter out terminal pods and pods on other
// nodes themselves, the same as for DrainEvicts.
func (d *DrainImpact) Add(pod *corev1.Pod) {
	if !DrainEvicts(pod) {
		return
	}
	d.Evict++
	if metav1.GetControllerOf(pod) == nil {
		d.Unmanaged++
	}
	if slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.EmptyDir != nil }) {
		d.EmptyDir++
	}
}

// DrainImpactOf tallies every pod in pods — DrainImpact.Add over a slice.
func DrainImpactOf(pods []*corev1.Pod) DrainImpact {
	var d DrainImpact
	for _, p := range pods {
		d.Add(p)
	}
	return d
}
