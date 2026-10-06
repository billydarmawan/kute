package fake

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/kute-dev/kute/internal/helmrepo"
	"github.com/kute-dev/kute/internal/kube"
)

// NewDemo builds a fake cluster seeded with a representative fixture set —
// pods incl. crashloop/pending/completed, a deployment mid-rollout, nodes
// with pressure, and events tied to the crashlooping pod — for the --demo
// flag and any task test that wants a whole cluster rather than one-off
// stubs.
func NewDemo() *Cluster {
	c := New("default", "demo")
	c.AddContext("demo-prod", "default")
	// dev-readonly is who tasks/whocan (22a) pins as "current user" — a
	// read-only persona (bound to the "view" ClusterRole below, which
	// excludes secrets) so `g "who"`'s default "who can list secrets in
	// default" query has a real closest-miss row to show, the same persona
	// docs/design README.md's own 4b mockup names.
	c.SetUserName("dev-readonly")
	// …and the identity a real cluster's connect-time SelfSubjectReview
	// would report for it, so --demo's 33a sync is credited to dev-readonly
	// the way a live one is, not to the "kute" fallback.
	c.SetReviewedIdentity("dev-readonly", []string{"system:authenticated"})
	// --demo pretends to be a recent, fully-supported cluster: the
	// schedule editor's timezone field is editable rather than read-only,
	// matching what a real 1.27+ cluster (kind's own default today) would
	// answer.
	c.SetTimeZoneCapability(kube.TimeZoneCapabilitySupported)

	now := metav1.Now()
	age := func(d time.Duration) metav1.Time { return metav1.NewTime(now.Add(-d)) }
	// future is age's mirror image, for §35b's forward-looking
	// notAfter/renewalTime Certificate fields.
	future := func(d time.Duration) metav1.Time { return metav1.NewTime(now.Add(d)) }

	// apiPod/workerPod carry OwnerReferences + labels so poddetail's 'o'
	// (owning Deployment/StatefulSet) and 'i' (fronting Ingress) have real
	// chains to resolve in --demo: apiPod -> ReplicaSet api-7d9f6c8 ->
	// Deployment api -> Service api -> Ingress api; workerPod -> StatefulSet
	// worker directly (no intermediate ReplicaSet, same as a real cluster).
	apiPod := demoPod("api-7d9f6c8-abcde", "default", age(2*24*time.Hour), corev1.PodRunning, corev1.PodQOSGuaranteed, "node-a", true, 0, nil)
	apiPod.Labels = map[string]string{"app": "api"}
	apiPod.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "api-7d9f6c8"}}
	// demoPod's primary container is generically named "app"; rename it to
	// match demoMidRolloutDeployment("api", ...)'s own pod-template container
	// name ("api") below. 25a's Set Resources panel builds its container tabs
	// from the Deployment's declared template but reads live usage by joining
	// on this pod's real container name (setresources.go's containerUsage) —
	// a mismatch here means every field silently reads "metrics unavailable"
	// no matter what ContainerMetricsByNamespace seeds, since the join never
	// hits (CLAUDE.md: "the fake provider must stay feature-complete for
	// tests/demo mode").
	apiPod.Spec.Containers[0].Name = "api"
	apiPod.Status.ContainerStatuses[0].Name = "api"
	// A second, running sidecar container makes apiPod --demo's one
	// reachable exercise of 10a's exec-container-picker screen — a
	// single-container pod execs straight through instead, so without this
	// the picker screen could only ever be driven by synthetic unit-test
	// fixtures (CLAUDE.md: "the fake provider must stay feature-complete
	// for tests/demo mode").
	apiPod.Spec.Containers = append(apiPod.Spec.Containers, corev1.Container{Name: "metrics-sidecar", Image: "sidecar:0.9.1"})
	apiPod.Status.ContainerStatuses = append(apiPod.Status.ContainerStatuses, corev1.ContainerStatus{
		Name: "metrics-sidecar", Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: age(2 * 24 * time.Hour)}},
	})
	// apiPod is --demo's *multi-port* port-forward subject (13a): three
	// declared container ports, so the picker opens with a real list to move
	// through, each row carries its own editable local port, and the
	// free-port row below them is visibly an *addition* rather than the only
	// thing on screen. One port would only ever demo a single row, and
	// demoPod's single 8080 made the whole feature look like a one-liner. The
	// api Deployment's own template declares the identical three, so `f` on
	// the Deployment agrees with `f` on its Pod rather than the two fixtures
	// quietly disagreeing.
	apiPod.Spec.Containers[0].Ports = demoAPIPorts()

	// An ephemeral debug container someone attached earlier makes 5a's
	// EPHEMERAL CONTAINERS section and its ↵ (§41e) reachable in --demo.
	apiPod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debugger-x7k2p", Image: "busybox:1.36"},
		TargetContainerName:      "api",
	}}
	apiPod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{
		Name:  "debugger-x7k2p",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: age(20 * time.Minute)}},
	}}

	workerPod := demoCrashLoopPod("worker-0", "default", age(14*time.Hour), "node-a")
	workerPod.Labels = map[string]string{"app": "worker"}
	workerPod.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "worker"}}
	// Same rename as apiPod above, matching demoWorkerStatefulSet's own
	// template container name ("worker") so 25a's usage join hits for the
	// StatefulSet too.
	workerPod.Spec.Containers[0].Name = "worker"
	workerPod.Status.ContainerStatuses[0].Name = "worker"
	// 5a's RELATED Secret/ConfigMap links and 'v' ENV & MOUNTS need a pod
	// that actually references config: a connection string from a Secret
	// key, a whole ConfigMap via envFrom, and a Secret volume.
	workerPod.Spec.Containers[0].Env = []corev1.EnvVar{{
		Name: "ConnectionStrings__WorkerDb",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "worker-db"}, Key: "connectionString",
		}},
	}}
	workerPod.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{
		ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "worker-config"}},
	}}
	workerPod.Spec.Volumes = []corev1.Volume{{
		Name:         "tls",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "api-tls-secret"}},
	}}
	workerPod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "tls", MountPath: "/etc/tls", ReadOnly: true}}

	// gatewayPod/gatewayCanaryPod are §41b/§41c's one demo exercise of the
	// kubectl-debug fork (docs/design v.0.11.0.dc.html §41a-§41c): 'x' only
	// ever routes to tasks/debugpanel when every container in the pod comes
	// back genuinely shell-less (browse/debug.go's allShellless), and
	// shells.go's DetectShells only answers "no shell" for a container
	// *named* for a distroless image — no other seeded pod qualifies.
	// "envoy-distroless" is a real Envoy Proxy image variant, not a
	// contrived name. Deliberately bare (no Deployment/ReplicaSet/Service):
	// unlike apiPod/workerPod these don't need an owning-workload chain for
	// anything this pair demonstrates, and staying unowned keeps them out of
	// every other screen's captured backdrop.
	//
	// gatewayPod is also --demo's *portless* port-forward subject: its one
	// container declares no ports at all (demoPod's default is cleared
	// below), which is both a real Envoy deployment shape and the only way to
	// reach 13a's free-port row in demo mode on its own — every other seeded
	// pod carries a declared port, so `f` there always opens with a list.
	gatewayPod := demoPod("gateway-6f9c8d7b4-h2n8k", "default", age(6*24*time.Hour), corev1.PodRunning, corev1.PodQOSGuaranteed, "node-a", true, 0, nil)
	gatewayPod.Labels = map[string]string{"app": "gateway"}
	gatewayPod.Spec.Containers[0].Name = "envoy-distroless"
	gatewayPod.Spec.Containers[0].Image = "envoyproxy/envoy-distroless:v1.30.1"
	gatewayPod.Spec.Containers[0].Ports = nil
	gatewayPod.Status.ContainerStatuses[0].Name = "envoy-distroless"

	// gatewayCanaryPod: same shell-less container, a newer image tag that
	// crash-loops — §41c's target, where 'x' skips straight to copy mode
	// since the pod can't stay running for an ephemeral attach either.
	gatewayCanaryPod := demoCrashLoopPod("gateway-canary-5d7f9c8b6-q7m3s", "default", age(12*time.Minute), "node-a")
	gatewayCanaryPod.Labels = map[string]string{"app": "gateway", "track": "canary"}
	gatewayCanaryPod.Spec.Containers[0].Name = "envoy-distroless"
	gatewayCanaryPod.Spec.Containers[0].Image = "envoyproxy/envoy-distroless:v1.31.0"
	gatewayCanaryPod.Status.ContainerStatuses[0].Name = "envoy-distroless"

	c.Seed(kube.KindPod,
		apiPod,
		workerPod,
		demoPendingPod("cache-0", "default", age(3*time.Minute)),
		demoCompletedPod("migrate-job-x8z2p", "default", age(20*time.Minute)),
		gatewayPod,
		gatewayCanaryPod,
	)

	c.Seed(kube.KindDeployment, demoMidRolloutDeployment("api", "default", age(30*24*time.Hour)))
	c.Seed(kube.KindReplicaSet,
		demoReplicaSet("api-7d9f6c8", "default", "api", "api:2.0", age(30*24*time.Hour)),
		// The new ReplicaSet the mid-rollout Deployment above is scaling up
		// to — its own revision-2, current image, giving 24a's set-image
		// history table a real "current" row alongside api-7d9f6c8's
		// rollback-target row (docs/design README.md §24a: "TAG · SEEN ·
		// FROM table lists this workload's own ReplicaSet revision
		// history").
		demoReplicaSetRevision("api-9f4e2a1", "default", "api", "api:2.1", 2, age(20*time.Minute)),
	)
	// 17b: "api" is HPA-managed, so its scale prompt exercises the "managed
	// by hpa/<name>" yellow note live in --demo mode.
	c.Seed(kube.KindHorizontalPodAutoscaler, demoHPA("api-hpa", "default", "Deployment", "api"))
	c.Seed(kube.KindStatefulSet, demoWorkerStatefulSet("worker", "default", age(14*time.Hour)))
	c.Seed(kube.KindControllerRevision,
		// 24a's set-image history table for StatefulSet/DaemonSet reads
		// ControllerRevisions (apps/v1), the mechanism `kubectl rollout
		// history statefulset` itself reads — "worker" carries two so
		// --demo mode has a real rollback-target row, not just current.
		demoControllerRevision("worker-6b8f7c9", "default", "StatefulSet", "worker", "worker", "worker:0.9.2", 1, age(9*24*time.Hour)),
		demoControllerRevision("worker-7c9a1d2", "default", "StatefulSet", "worker", "worker", "worker:1.0.0", 2, age(14*time.Hour)),
	)
	c.Seed(kube.KindService, demoService("api", "default", map[string]string{"app": "api"}, age(30*24*time.Hour)))
	c.Seed(kube.KindIngress, demoIngress("api", "default", "api", "api.demo.local", age(30*24*time.Hour)))

	// §36a-§36e (0.8.0 plan Phase 8 task 7): five CronJobs, each pinned to
	// one of the outcomes the list/detail screens need to render for real.
	// §36a deliberately keeps CronJobs in fixed name order (never a
	// health-first sort), so ordering these by scenario rather than by
	// severity costs nothing.
	nightlyBackup := demoCronJob("nightly-backup", "default", "0 2 * * *", false, age(60*24*time.Hour))
	nightlyBackup.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
	nightlyBackup.Spec.SuccessfulJobsHistoryLimit = int32Val(3)
	nightlyBackup.Spec.FailedJobsHistoryLimit = int32Val(1)

	// hourlyReport starts suspended *by Kute*: SuspendedAt/
	// SuspendedGeneration validate against its own (unchanged) Generation,
	// so §36c's resume preflight computes a real suspended-duration and
	// missed-schedule count instead of falling back to "start unknown"
	// (§3.3) — the more interesting demo case than an externally-suspended
	// CronJob, and 's' still exercises resume first rather than every
	// CronJob needing an extra keypress to reach that state.
	hourlyReport := demoCronJob("hourly-report", "default", "0 * * * *", true, age(20*24*time.Hour))
	hourlyReport.Annotations = map[string]string{
		kube.AnnotationSuspendedAt:         age(3 * time.Hour).Time.UTC().Format(time.RFC3339),
		kube.AnnotationSuspendedGeneration: "1", // matches hourlyReport.Generation, set by demoCronJob
	}

	nightlyCleanup := demoCronJob("nightly-cleanup", "default", "30 3 * * *", false, age(45*24*time.Hour))
	nightlyCleanup.Spec.SuccessfulJobsHistoryLimit = int32Val(3)
	nightlyCleanup.Spec.FailedJobsHistoryLimit = int32Val(1)
	nightlyCleanup.Spec.JobTemplate.Spec.BackoffLimit = int32Val(3)

	// metricsRollup keeps an active run alive (below) with ForbidConcurrent,
	// so §36b's ctrl-r run-now demonstrates a real overlap warning rather
	// than the no-conflict path.
	metricsRollup := demoCronJob("metrics-rollup", "default", "*/15 * * * *", false, age(10*24*time.Hour))
	metricsRollup.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent

	// weeklyDigest is §3.8/§3.9's timezone exemplar: an explicit
	// spec.timeZone lets §36a compute an exact NEXT (rather than
	// "controller local") and lets §36d's editor exercise a populated
	// timezone field. It carries no retained Jobs — §36a/§36e's "no
	// retained runs" neutral state, distinct from every other CronJob here.
	weeklyDigest := demoCronJob("weekly-digest", "default", "0 9 * * 1", false, age(90*24*time.Hour))
	weeklyTZ := "America/New_York"
	weeklyDigest.Spec.TimeZone = &weeklyTZ

	c.Seed(kube.KindCronJob, nightlyBackup, hourlyReport, nightlyCleanup, metricsRollup, weeklyDigest)

	failedCleanupRun := demoScheduledFailedJob("nightly-cleanup-29070330", "default", nightlyCleanup.Name, nightlyCleanup.UID,
		age(2*time.Hour), "BackoffLimitExceeded", "Job has reached the specified backoff limit")
	failedCleanupRun.Spec.BackoffLimit = int32Val(3)
	failedCleanupRun.Status.Failed = 3

	c.Seed(kube.KindJob,
		// nightly-backup: two retained succeeded runs (§36a "successful
		// latest run" — the newer is LAST RUN), a manual annotated
		// standalone Job (§36b run-now, ownerless per §3.4), an unrelated
		// same-name-prefix Job that must never associate by name alone
		// (§4.2), and a controller-owned Job whose owner UID predates
		// nightly-backup's current one — the "delete/recreate must not
		// inherit stale history" rejection (§4.2, Phase 1 test 3).
		demoScheduledSucceededJob("nightly-backup-29070200", "default", nightlyBackup.Name, nightlyBackup.UID,
			age(22*time.Hour), age(22*time.Hour-5*time.Minute)),
		demoScheduledSucceededJob("nightly-backup-29060200", "default", nightlyBackup.Name, nightlyBackup.UID,
			age(46*time.Hour), age(46*time.Hour-5*time.Minute)),
		// Older than both scheduled runs (task 9's "latest run" story stays
		// the scheduled 29070200 Job, not this one-off).
		demoManualJob("nightly-backup-manual-1430", "default", nightlyBackup.Name, nightlyBackup.UID, c.userName,
			age(30*time.Hour), age(30*time.Hour), age(30*time.Hour-4*time.Minute)),
		demoUnrelatedJob("nightly-backup-migration", "default", age(5*24*time.Hour)),
		demoScheduledSucceededJob("nightly-backup-28060200", "default", nightlyBackup.Name,
			types.UID("demo-cronjob-default-nightly-backup-stale"), age(70*24*time.Hour), age(70*24*time.Hour)),

		// hourly-report: one succeeded run before Kute suspended it.
		demoScheduledSucceededJob("hourly-report-29071200", "default", hourlyReport.Name, hourlyReport.UID,
			age(4*time.Hour), age(4*time.Hour-2*time.Minute)),

		// nightly-cleanup: §3.5's failed-latest-run exemplar — the Job's
		// own Failed condition is authoritative; its retained Pods (seeded
		// below) add attempt-level exit codes/durations, never override it.
		failedCleanupRun,

		// metrics-rollup: a prior succeeded run plus a currently active one
		// — §4.3's "2m ago check while ACT says 1" case, where LAST RUN
		// must keep naming the newest *terminal* run, not the active one.
		demoScheduledSucceededJob("metrics-rollup-29071315", "default", metricsRollup.Name, metricsRollup.UID,
			age(20*time.Minute), age(17*time.Minute)),
		demoScheduledActiveJob("metrics-rollup-29071330", "default", metricsRollup.Name, metricsRollup.UID, age(2*time.Minute)),
	)

	c.Seed(kube.KindPod,
		demoFailedJobPod("nightly-cleanup-29070330-k2m4p", "default", age(2*time.Hour-time.Minute),
			failedCleanupRun.Name, failedCleanupRun.UID),
		demoFailedJobPod("nightly-cleanup-29070330-r5t8w", "default", age(2*time.Hour-4*time.Minute),
			failedCleanupRun.Name, failedCleanupRun.UID),
		demoFailedJobPod("nightly-cleanup-29070330-x7z2p", "default", age(2*time.Hour-7*time.Minute),
			failedCleanupRun.Name, failedCleanupRun.UID),
	)

	// §37d: a standalone, Indexed-completion-mode Job — see demoIndexedJob's
	// own doc comment for why this fixture must exist at all.
	reindexSearch := demoIndexedJob("reindex-search", "default", age(11*time.Minute))
	c.Seed(kube.KindJob, reindexSearch)

	// terminatedAt is a tiny local helper for the indexed-pod fixtures
	// below: a ContainerStateTerminated ending `after` its own pod's start.
	terminatedAt := func(start metav1.Time, after time.Duration, exitCode int32, reason string) *corev1.ContainerStateTerminated {
		return &corev1.ContainerStateTerminated{ExitCode: exitCode, Reason: reason, FinishedAt: metav1.NewTime(start.Add(after))}
	}
	idx0, idx1, idx2 := age(10*time.Minute), age(10*time.Minute), age(10*time.Minute)
	idx4, idx5, idx6, idx7 := age(7*time.Minute+50*time.Second), age(7*time.Minute+50*time.Second), age(7*time.Minute+50*time.Second), age(7*time.Minute+50*time.Second)
	idx3a, idx3b := age(9*time.Minute), age(7*time.Minute)
	idx8, idx9, idx10 := age(time.Minute+12*time.Second), age(48*time.Second), age(21*time.Second)

	c.Seed(kube.KindPod,
		// indexes 0-2, 4-7: complete (7 of 12) — §37d's "complete" cell state.
		demoIndexedJobPod("reindex-search-0-a1b2c", "default", 0, idx0, corev1.PodSucceeded,
			terminatedAt(idx0, 2*time.Minute+11*time.Second, 0, "Completed"), reindexSearch.Name, reindexSearch.UID),
		demoIndexedJobPod("reindex-search-1-b2c3d", "default", 1, idx1, corev1.PodSucceeded,
			terminatedAt(idx1, 2*time.Minute+4*time.Second, 0, "Completed"), reindexSearch.Name, reindexSearch.UID),
		demoIndexedJobPod("reindex-search-2-c3d4e", "default", 2, idx2, corev1.PodSucceeded,
			terminatedAt(idx2, time.Minute+58*time.Second, 0, "Completed"), reindexSearch.Name, reindexSearch.UID),
		// index 3: two attempts, both OOMKilled — §37d's "failed index with
		// >1 attempt" exemplar (mirrors the mockup's own "index 3 · exit
		// 137 · OOMKilled, both attempts"; pod name w82kq echoes the
		// mockup's own most-recent-attempt pod name).
		demoIndexedJobPod("reindex-search-3-k9m2p", "default", 3, idx3a, corev1.PodFailed,
			terminatedAt(idx3a, 85*time.Second, 137, "OOMKilled"), reindexSearch.Name, reindexSearch.UID),
		demoIndexedJobPod("reindex-search-3-w82kq", "default", 3, idx3b, corev1.PodFailed,
			terminatedAt(idx3b, 90*time.Second, 137, "OOMKilled"), reindexSearch.Name, reindexSearch.UID),
		demoIndexedJobPod("reindex-search-4-d4e5f", "default", 4, idx4, corev1.PodSucceeded,
			terminatedAt(idx4, 2*time.Minute+22*time.Second, 0, "Completed"), reindexSearch.Name, reindexSearch.UID),
		demoIndexedJobPod("reindex-search-5-e5f6g", "default", 5, idx5, corev1.PodSucceeded,
			terminatedAt(idx5, 2*time.Minute+9*time.Second, 0, "Completed"), reindexSearch.Name, reindexSearch.UID),
		demoIndexedJobPod("reindex-search-6-f6g7h", "default", 6, idx6, corev1.PodSucceeded,
			terminatedAt(idx6, 2*time.Minute+31*time.Second, 0, "Completed"), reindexSearch.Name, reindexSearch.UID),
		demoIndexedJobPod("reindex-search-7-g7h8i", "default", 7, idx7, corev1.PodSucceeded,
			terminatedAt(idx7, 2*time.Minute+3*time.Second, 0, "Completed"), reindexSearch.Name, reindexSearch.UID),
		// indexes 8-10: still running (no terminated state) — §37d's
		// "running" cell state, 3 of parallelism 4's slots (the 4th is
		// consumed by index 3's retry).
		demoIndexedJobPod("reindex-search-8-h8i9j", "default", 8, idx8, corev1.PodRunning, nil, reindexSearch.Name, reindexSearch.UID),
		demoIndexedJobPod("reindex-search-9-i9j0k", "default", 9, idx9, corev1.PodRunning, nil, reindexSearch.Name, reindexSearch.UID),
		demoIndexedJobPod("reindex-search-10-j0k1l", "default", 10, idx10, corev1.PodRunning, nil, reindexSearch.Name, reindexSearch.UID),
		// index 11: deliberately no pod at all — §37d's "queued" cell state
		// (ProjectJobIndexGrid pads to Completions when no attempt covers
		// an index yet).
	)

	// A production-like cluster has many namespaces beyond the one an
	// operator is actively working in: system/platform namespaces
	// (kube-system), operator-owned namespaces for the CRD-installing
	// add-ons below (cert-manager/monitoring/argocd/ingress-nginx/logging),
	// and app environments (production alongside default/staging).
	// "development" is seeded with zero resources of any kind — the
	// fully-empty-namespace case, distinct from staging's "no pods, some
	// config" case below.
	c.Seed(kube.KindNamespace,
		demoNamespace("default"), demoNamespace("staging"), demoNamespace("production"),
		demoNamespace("kube-system"), demoNamespace("cert-manager"), demoNamespace("monitoring"),
		demoNamespace("argocd"), demoNamespace("ingress-nginx"), demoNamespace("logging"),
		demoNamespace("development"),
	)

	// "staging" has no pods (10c empty-namespace preview) but does have
	// other kinds, so the empty state's "g other kinds" way-out has live
	// data to show rather than degrading to a plain line.
	c.Seed(kube.KindConfigMap,
		demoConfigMap("app-config", "staging", age(10*24*time.Hour)),
		demoConfigMap("feature-flags", "staging", age(2*24*time.Hour)),
	)
	c.Seed(kube.KindSecret, demoSecret("app-secret", "staging", age(10*24*time.Hour)))
	// worker-0's env references (5a RELATED / 'v' ENV & MOUNTS) resolve to
	// real objects, so their Data views open on something.
	workerDB := demoSecret("worker-db", "default", age(14*24*time.Hour))
	workerDB.Data = map[string][]byte{"connectionString": []byte("Server=db;Database=worker;User Id=worker;Password=demo")}
	c.Seed(kube.KindSecret, workerDB)
	c.Seed(kube.KindConfigMap, demoConfigMap("worker-config", "default", age(14*24*time.Hour)))

	c.Seed(kube.KindNode,
		demoNode("node-a", true, false, false),
		demoNode("node-b", true, true, false),   // MemoryPressure
		demoNode("node-c", true, false, true),   // cordoned
		demoNode("node-d", false, false, false), // NotReady
	)

	c.Seed(kube.KindEvent,
		demoEvent("worker-0.backoff1", "default", "Pod", "worker-0", "Warning", "BackOff",
			"Back-off restarting failed container worker in pod worker-0_default(...)", 5, age(30*time.Minute)),
		demoEvent("worker-0.scheduled", "default", "Pod", "worker-0", "Normal", "Scheduled",
			"Successfully assigned default/worker-0 to node-a", 1, age(14*time.Hour)),
		demoEvent("node-b.pressure", "", "Node", "node-b", "Warning", "MemoryPressure",
			"Node node-b status is now: MemoryPressure", 3, age(45*time.Minute)),
	)

	// apiPod's two containers each log their own lines (5b's container
	// switch shows a different stream, as on a real cluster).
	apiLogAt := func(ago time.Duration) string { return now.Add(-ago).UTC().Format(time.RFC3339) }
	c.SeedContainerLogs("default", apiPod.Name, "api", []string{
		apiLogAt(3*time.Minute) + " INF listening on :8080",
		apiLogAt(2*time.Minute) + " INF GET /healthz 200 1ms",
		apiLogAt(time.Minute) + " WRN slow query took 1.2s",
	})
	c.SeedContainerLogs("default", apiPod.Name, "metrics-sidecar", []string{
		apiLogAt(2*time.Minute) + " INF scraping api:8080/metrics every 15s",
	})

	c.SeedLogs("default", "worker-0", []string{
		"2024-01-01T00:00:00Z INF starting worker",
		"2024-01-01T00:00:05Z ERR panic: connection refused",
		"2024-01-01T00:00:06Z INF restarting",
	})

	demoRBACFixtures(c, age)
	demoCertManagerFixtures(c, age, future)
	demoKubeSystemFixtures(c, age)
	demoIngressNginxFixtures(c, age)
	demoProductionFixtures(c, age)
	demoLoggingFixtures(c, age)
	demoPrometheusFixtures(c, age)
	demoArgoCDFixtures(c, age)
	demoGatewayAPIFixtures(c, age)
	demoFluxFixtures(c, age)
	demoHelmReleaseFixtures(c, age)

	// "ghost" exercises 23a's ▲ "0 ready" backend state: a Service whose
	// selector matches no pod at all, fronted by an Ingress rule that routes
	// to it — staging otherwise has no pods (10c's empty-namespace preview).
	c.Seed(kube.KindService, demoService("empty-svc", "staging", map[string]string{"app": "ghost"}, age(5*24*time.Hour)))
	c.Seed(kube.KindIngress, demoIngress("ghost", "staging", "empty-svc", "ghost.demo.local", age(5*24*time.Hour)))

	return c
}

// demoKubeSystemFixtures seeds the platform namespace every real cluster
// has — coredns/kube-proxy — so "many namespaces" includes the one every
// operator recognizes on sight.
func demoKubeSystemFixtures(c *Cluster, age func(time.Duration) metav1.Time) {
	sysAge := age(120 * 24 * time.Hour)
	c.Seed(kube.KindDeployment, demoStableDeployment("coredns", "kube-system", "coredns:1.11.1", 2, sysAge))
	c.Seed(kube.KindDaemonSet, demoDaemonSetReady("kube-proxy", "kube-system", 4, sysAge))
	c.Seed(kube.KindPod, demoOwnedPod("coredns-6b7f9d5f8c-x2k9p", "kube-system", age(3*24*time.Hour), "node-a", "ReplicaSet", "coredns-6b7f9d5f8c"))
}

// demoIngressNginxFixtures seeds the ingress controller namespace — a
// LoadBalancer Service with an external IP is the other half of the
// "api" Ingress' own address, demonstrated end to end.
func demoIngressNginxFixtures(c *Cluster, age func(time.Duration) metav1.Time) {
	ingAge := age(60 * 24 * time.Hour)
	c.Seed(kube.KindDeployment, demoStableDeployment("ingress-nginx-controller", "ingress-nginx", "registry.k8s.io/ingress-nginx/controller:v1.10.1", 2, ingAge))
	c.Seed(kube.KindService, demoLoadBalancerService("ingress-nginx-controller", "ingress-nginx", 80, "203.0.113.15", ingAge))
	c.Seed(kube.KindPod, demoOwnedPod("ingress-nginx-controller-8f7d9c-k7m2q", "ingress-nginx", age(10*24*time.Hour), "node-b", "ReplicaSet", "ingress-nginx-controller-8f7d9c"))
}

// demoProductionFixtures mirrors "default"'s api/worker pattern in a
// second app environment — a stable, fully-ready Deployment (no rollout in
// progress) so the health strip's "many namespaces" story isn't only
// crashloops and mid-rollouts.
func demoProductionFixtures(c *Cluster, age func(time.Duration) metav1.Time) {
	prodAge := age(90 * 24 * time.Hour)
	c.Seed(kube.KindDeployment, demoStableDeployment("web", "production", "web:4.2.0", 3, prodAge))
	c.Seed(kube.KindReplicaSet, demoReplicaSet("web-7c9f8d", "production", "web", "web:4.2.0", prodAge))
	c.Seed(kube.KindService, demoService("web", "production", map[string]string{"app": "web"}, prodAge))
	c.Seed(kube.KindIngress, demoIngress("web", "production", "web", "web.prod.demo.local", prodAge))
	webPodA := demoOwnedPod("web-7c9f8d-aaaaa", "production", age(9*24*time.Hour), "node-a", "ReplicaSet", "web-7c9f8d")
	webPodB := demoOwnedPod("web-7c9f8d-bbbbb", "production", age(9*24*time.Hour), "node-b", "ReplicaSet", "web-7c9f8d")
	// Labeled to match the "web" Service's selector above — 23a's Ingress
	// routing table (and 23b's canary HTTPRoute below) resolve backend
	// health by matching a Service's selector against real pods, so these
	// need the label a real ReplicaSet-managed pod would carry.
	webPodA.Labels = map[string]string{"app": "web"}
	webPodB.Labels = map[string]string{"app": "web"}
	c.Seed(kube.KindPod, webPodA, webPodB)

	// "web-secure" exercises 23a's TLS strip and both remaining backend
	// glyphs: /  -> web (● ready), /admin -> web-missing (✕ service not
	// found — no such Service is ever seeded). The TLS secret expires soon
	// (yellow) so the strip's <30d coloring has something to show.
	c.Seed(kube.KindSecret, demoTLSSecret("web-tls", "production", time.Now().Add(20*24*time.Hour), prodAge))
	c.Seed(kube.KindIngress, demoIngressWithTLS("web-secure", "production", "web", "web-missing", "secure.prod.demo.local", "web-tls", prodAge))

	// api-canary runs a newer tag of the same "api" image "default"'s own
	// api Deployment is on — 24a's set-image history table's "seen on other
	// workloads" row (docs/design README.md §24a: "the same image tag seen
	// on other workloads/namespaces ... the 'promote what prod runs' case")
	// needs a real cross-namespace sighting to surface in --demo mode.
	c.Seed(kube.KindDeployment, demoStableDeployment("api-canary", "production", "api:2.2", 1, age(40*time.Minute)))
}

// demoLoggingFixtures seeds a third add-on namespace shape: a DaemonSet
// (fluent-bit, one per node) feeding a StatefulSet (elasticsearch) behind a
// Deployment (kibana) — none of them CRD-backed, unlike cert-manager/
// monitoring/argocd, so the namespace list isn't only operator namespaces.
func demoLoggingFixtures(c *Cluster, age func(time.Duration) metav1.Time) {
	logAge := age(75 * 24 * time.Hour)
	c.Seed(kube.KindDaemonSet, demoDaemonSetReady("fluent-bit", "logging", 4, logAge))
	c.Seed(kube.KindStatefulSet, demoStatefulSetN("elasticsearch", "logging", 3, logAge))
	c.Seed(kube.KindDeployment, demoStableDeployment("kibana", "logging", "kibana:8.13.0", 1, logAge))
	c.Seed(kube.KindPod, demoOwnedPod("fluent-bit-9k2mp", "logging", age(5*24*time.Hour), "node-a", "DaemonSet", "fluent-bit"))
}

// demoRBACFixtures seeds tasks/whocan's (22a) resolution graph: a broad
// read-only ClusterRole ("view") deliberately excluding secrets — bound to
// dev-readonly (NewDemo's pinned "current user") and a "dev-team" Group —
// plus a secret-scoped Role and a cluster-wide admin ClusterRoleBinding, so
// `g "who can list secrets"` has real subjects across both SCOPE values
// (namespace and cluster) and a genuine closest-miss for dev-readonly.
func demoRBACFixtures(c *Cluster, age func(time.Duration) metav1.Time) {
	rbacAge := age(120 * 24 * time.Hour)

	c.Seed(kube.KindClusterRole,
		demoClusterRole("view", rbacAge,
			rbacv1.PolicyRule{
				APIGroups: []string{""},
				Resources: []string{"pods", "services", "configmaps", "namespaces", "nodes", "events"},
				Verbs:     []string{"get", "list", "watch"},
			},
			rbacv1.PolicyRule{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments", "replicasets", "statefulsets", "daemonsets"},
				Verbs:     []string{"get", "list", "watch"},
			},
		),
		demoClusterRole("admin", rbacAge,
			rbacv1.PolicyRule{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}},
		),
		// pod-debugger: the one narrow write "view" doesn't carry — real
		// teams commonly grant exactly this on top of a read-only role so an
		// SRE can `kubectl debug` without also getting delete/edit access.
		// Without it, §41's live access review correctly denies dev-readonly
		// inside the debug panel and --demo cannot exercise its launch flow.
		demoClusterRole("pod-debugger", rbacAge,
			rbacv1.PolicyRule{
				APIGroups: []string{""},
				Resources: []string{"pods", "pods/ephemeralcontainers"},
				Verbs:     []string{"create"},
			},
		),
	)

	c.Seed(kube.KindRole,
		demoRole("secret-reader", "default", rbacAge,
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list", "watch"}},
		),
	)

	c.Seed(kube.KindClusterRoleBinding,
		demoClusterRoleBinding("cluster-admins", "admin", rbacAge,
			rbacv1.Subject{Kind: rbacv1.UserKind, Name: "alice"},
		),
		demoClusterRoleBinding("dev-debuggers", "pod-debugger", rbacAge,
			rbacv1.Subject{Kind: rbacv1.UserKind, Name: "dev-readonly"},
		),
	)

	c.Seed(kube.KindRoleBinding,
		demoRoleBinding("dev-viewers", "default", "ClusterRole", "view", rbacAge,
			rbacv1.Subject{Kind: rbacv1.UserKind, Name: "dev-readonly"},
			rbacv1.Subject{Kind: rbacv1.GroupKind, Name: "dev-team"},
		),
		demoRoleBinding("secret-readers", "default", "Role", "secret-reader", rbacAge,
			rbacv1.Subject{Kind: rbacv1.UserKind, Name: "bob"},
			rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "vault-agent", Namespace: "default"},
		),
	)
}

func demoClusterRole(name string, created metav1.Time, rules ...rbacv1.PolicyRule) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: created},
		Rules:      rules,
	}
}

func demoRole(name, ns string, created metav1.Time, rules ...rbacv1.PolicyRule) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: created},
		Rules:      rules,
	}
}

func demoClusterRoleBinding(name, roleName string, created metav1.Time, subjects ...rbacv1.Subject) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: created},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: roleName},
		Subjects:   subjects,
	}
}

func demoRoleBinding(name, ns, roleKind, roleName string, created metav1.Time, subjects ...rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: created},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: roleKind, Name: roleName},
		Subjects:   subjects,
	}
}

// demoCertManagerFixtures seeds the design doc's own 14a/14b/14d exemplar —
// cert-manager.io's Certificate (namespaced, exercises READY/SECRET/ISSUER
// printer columns), CertificateRequest (namespaced), and ClusterIssuer
// (cluster-scoped) — so --demo exercises CRD support end to end without a
// real cluster.
func demoCertManagerFixtures(c *Cluster, age, future func(time.Duration) metav1.Time) {
	crdAge := age(90 * 24 * time.Hour)

	certCols := []kube.PrinterColumn{
		{Name: "Ready", Type: "string", JSONPath: `.status.conditions[?(@.type=="Ready")].status`},
		{Name: "Secret", Type: "string", JSONPath: ".spec.secretName"},
		{Name: "Issuer", Type: "string", JSONPath: ".spec.issuerRef.name"},
	}
	certReqCols := []kube.PrinterColumn{
		{Name: "Ready", Type: "string", JSONPath: `.status.conditions[?(@.type=="Ready")].status`},
		{Name: "Issuer", Type: "string", JSONPath: ".spec.issuerRef.name"},
	}
	issuerCols := []kube.PrinterColumn{
		{Name: "Ready", Type: "string", JSONPath: `.status.conditions[?(@.type=="Ready")].status`},
	}
	// Order/Challenge carry no Ready-style condition at all — status.state
	// is their own vocabulary (§35a's certchain reads it directly) — so
	// their printer columns are State/Reason rather than Ready.
	orderCols := []kube.PrinterColumn{
		{Name: "State", Type: "string", JSONPath: ".status.state"},
		{Name: "Reason", Type: "string", JSONPath: ".status.reason"},
	}
	challengeCols := []kube.PrinterColumn{
		{Name: "State", Type: "string", JSONPath: ".status.state"},
		{Name: "Reason", Type: "string", JSONPath: ".status.reason"},
	}

	c.Seed(kube.KindCustomResourceDefinition,
		demoCRD("certificates.cert-manager.io", "cert-manager.io", "Certificate", "certificates", "Namespaced", "v1", true, certCols, crdAge),
		demoCRD("certificaterequests.cert-manager.io", "cert-manager.io", "CertificateRequest", "certificaterequests", "Namespaced", "v1", true, certReqCols, crdAge),
		demoCRD("clusterissuers.cert-manager.io", "cert-manager.io", "ClusterIssuer", "clusterissuers", "Cluster", "v1", true, issuerCols, crdAge),
		// Order/Challenge are cert-manager's own ACME CRDs but live in the
		// sibling acme.cert-manager.io group — §35a's certchain walks them
		// by bare kind name only, so the group split doesn't affect routing.
		demoCRD("orders.acme.cert-manager.io", "acme.cert-manager.io", "Order", "orders", "Namespaced", "v1", true, orderCols, crdAge),
		demoCRD("challenges.acme.cert-manager.io", "acme.cert-manager.io", "Challenge", "challenges", "Namespaced", "v1", true, challengeCols, crdAge),
	)

	certKind := demoDiscoveredKind("Certificate", "certificates", "cert-manager.io", "v1", "certificates.cert-manager.io", false, certCols)
	certKind.ShortNames = []string{"cert", "certs"} // as cert-manager declares them — §14a's CERTS pill
	c.SeedDiscovered(certKind)
	c.SeedDiscovered(demoDiscoveredKind("CertificateRequest", "certificaterequests", "cert-manager.io", "v1", "certificaterequests.cert-manager.io", false, certReqCols))
	c.SeedDiscovered(demoDiscoveredKind("ClusterIssuer", "clusterissuers", "cert-manager.io", "v1", "clusterissuers.cert-manager.io", true, issuerCols))
	c.SeedDiscovered(demoDiscoveredKind("Order", "orders", "acme.cert-manager.io", "v1", "orders.acme.cert-manager.io", false, orderCols))
	c.SeedDiscovered(demoDiscoveredKind("Challenge", "challenges", "acme.cert-manager.io", "v1", "challenges.acme.cert-manager.io", false, challengeCols))

	c.Seed(kube.ResourceKind("Certificate"),
		demoCertificate("api-tls", "default", true, "", "api-tls-secret", "letsencrypt-prod",
			age(5*24*time.Hour), future(61*24*time.Hour), future(31*24*time.Hour), metav1.Time{}),
		demoCertificate("staging-tls", "staging", false, "", "staging-tls-secret", "letsencrypt-staging",
			age(2*time.Hour), metav1.Time{}, metav1.Time{}, metav1.Time{}),
		// §35a's own mockup scenario: a DNS-01 challenge stuck on a
		// propagation failure, four issuance attempts in, secretName
		// deliberately naming a Secret that's never seeded (the refs
		// strip's "missing" case) against an issuer that is Ready (the
		// refs strip's healthy case). lastFailure is set — §35b's own
		// signal that this is a real, repeated failure (StatusFail, ✕),
		// not just a first attempt in progress — matching the three
		// abandoned CertificateRequests already seeded for it below.
		demoCertificate("web-tls", "default", false, "Issuing", "web-tls-cert", "letsencrypt-prod",
			age(41*24*time.Hour), metav1.Time{}, metav1.Time{}, age(8*time.Minute)),
		// §35b's own remaining mockup rows.
		demoCertificate("admin-tls", "default", true, "", "admin-tls-secret", "letsencrypt-prod",
			age(70*24*time.Hour), future(22*24*time.Hour), age(8*24*time.Hour), metav1.Time{}),
		demoCertificate("new-svc-tls", "default", false, "", "new-svc-tls-secret", "letsencrypt-prod",
			age(4*time.Minute), metav1.Time{}, metav1.Time{}, metav1.Time{}),
		demoCertificate("grafana-tls", "default", true, "", "grafana-tls-secret", "letsencrypt-prod",
			age(62*24*time.Hour), future(62*24*time.Hour), future(32*24*time.Hour), metav1.Time{}),
		demoCertificate("internal-ca", "default", true, "", "internal-ca-secret", "selfsigned",
			age(90*24*time.Hour), future(8*365*24*time.Hour), future(7*365*24*time.Hour), metav1.Time{}),
	)
	c.Seed(kube.ResourceKind("CertificateRequest"),
		demoCertificateRequest("api-tls-abcd1", "default", true, "letsencrypt-prod", "api-tls", age(5*24*time.Hour)),
		// Three abandoned earlier attempts plus the current one — attempts
		// is an honest count of these, never a fabricated number.
		demoCertificateRequest("web-tls-0", "default", false, "letsencrypt-prod", "web-tls", age(41*24*time.Hour)),
		demoCertificateRequest("web-tls-1a", "default", false, "letsencrypt-prod", "web-tls", age(30*24*time.Hour)),
		demoCertificateRequest("web-tls-1b", "default", false, "letsencrypt-prod", "web-tls", age(15*24*time.Hour)),
		demoCertificateRequestApproved("web-tls-1", "default", "web-tls", "letsencrypt-prod", age(8*time.Minute)),
	)
	c.Seed(kube.ResourceKind("ClusterIssuer"),
		demoClusterIssuer("letsencrypt-prod", true, age(60*24*time.Hour)),
		demoClusterIssuer("letsencrypt-staging", true, age(60*24*time.Hour)),
		// §35b's internal-ca references this one.
		demoClusterIssuer("selfsigned", true, age(90*24*time.Hour)),
	)
	// api-tls is Ready — its target Secret genuinely exists, unlike
	// web-tls's deliberately-missing one (§35a's refs-strip "missing" case).
	c.Seed(kube.KindSecret, demoTLSSecret("api-tls-secret", "default", time.Now().Add(85*24*time.Hour), age(5*24*time.Hour)))
	c.Seed(kube.ResourceKind("Order"),
		demoOrder("web-tls-1-2847563921", "default", "web-tls-1", "errored", "authorization for app.nva.dev failed", age(8*time.Minute)),
	)
	c.Seed(kube.ResourceKind("Challenge"),
		demoChallenge("web-tls-1-2847563921-0", "default", "web-tls-1-2847563921", "dns-01", "app.nva.dev", "pending",
			"propagation check failed: NXDOMAIN looking up TXT _acme-challenge.app.nva.dev", age(8*time.Minute)),
	)

	// The operator itself lives in its own "cert-manager" namespace, same
	// as a real Helm install — the CRD instances above are what it manages,
	// not what it is.
	c.Seed(kube.KindDeployment,
		demoStableDeployment("cert-manager", "cert-manager", "quay.io/jetstack/cert-manager-controller:v1.14.4", 1, crdAge),
		demoStableDeployment("cert-manager-cainjector", "cert-manager", "quay.io/jetstack/cert-manager-cainjector:v1.14.4", 1, crdAge),
		demoStableDeployment("cert-manager-webhook", "cert-manager", "quay.io/jetstack/cert-manager-webhook:v1.14.4", 1, crdAge),
	)
	c.Seed(kube.KindPod, demoOwnedPod("cert-manager-7d8f9c-h6q2v", "cert-manager", age(20*24*time.Hour), "node-a", "ReplicaSet", "cert-manager-7d8f9c"))
}

// demoPrometheusFixtures seeds the prometheus-operator (monitoring.coreos.com)
// CRD family: Prometheus/Alertmanager (namespaced, do carry a Ready
// condition in this fixture set) plus ServiceMonitor/PrometheusRule
// (namespaced, no status subresource at all in the real CRD — left with no
// conditions here too, so their glyph is the 14a "never fake health"
// neutral "·" rather than a fabricated one), alongside the operator's own
// workloads in "monitoring".
func demoPrometheusFixtures(c *Cluster, age func(time.Duration) metav1.Time) {
	crdAge := age(80 * 24 * time.Hour)
	group := "monitoring.coreos.com"

	c.Seed(kube.KindCustomResourceDefinition,
		demoCRD("prometheuses.monitoring.coreos.com", group, "Prometheus", "prometheuses", "Namespaced", "v1", true, nil, crdAge),
		demoCRD("alertmanagers.monitoring.coreos.com", group, "Alertmanager", "alertmanagers", "Namespaced", "v1", true, nil, crdAge),
		demoCRD("servicemonitors.monitoring.coreos.com", group, "ServiceMonitor", "servicemonitors", "Namespaced", "v1", true, nil, crdAge),
		demoCRD("prometheusrules.monitoring.coreos.com", group, "PrometheusRule", "prometheusrules", "Namespaced", "v1", true, nil, crdAge),
	)
	c.SeedDiscovered(demoDiscoveredKind("Prometheus", "prometheuses", group, "v1", "prometheuses.monitoring.coreos.com", false, nil))
	c.SeedDiscovered(demoDiscoveredKind("Alertmanager", "alertmanagers", group, "v1", "alertmanagers.monitoring.coreos.com", false, nil))
	c.SeedDiscovered(demoDiscoveredKind("ServiceMonitor", "servicemonitors", group, "v1", "servicemonitors.monitoring.coreos.com", false, nil))
	c.SeedDiscovered(demoDiscoveredKind("PrometheusRule", "prometheusrules", group, "v1", "prometheusrules.monitoring.coreos.com", false, nil))

	c.Seed(kube.ResourceKind("Prometheus"),
		demoCR("monitoring.coreos.com/v1", "Prometheus", "k8s", "monitoring", age(80*24*time.Hour), nil, readyCondition(true, "")),
	)
	c.Seed(kube.ResourceKind("Alertmanager"),
		demoCR("monitoring.coreos.com/v1", "Alertmanager", "main", "monitoring", age(80*24*time.Hour), nil, readyCondition(true, "")),
	)
	c.Seed(kube.ResourceKind("ServiceMonitor"),
		demoCR("monitoring.coreos.com/v1", "ServiceMonitor", "api", "monitoring", age(30*24*time.Hour), nil),
		demoCR("monitoring.coreos.com/v1", "ServiceMonitor", "grafana", "monitoring", age(30*24*time.Hour), nil),
	)
	c.Seed(kube.ResourceKind("PrometheusRule"),
		demoCR("monitoring.coreos.com/v1", "PrometheusRule", "k8s-rules", "monitoring", age(30*24*time.Hour), nil),
	)

	c.Seed(kube.KindDeployment,
		demoStableDeployment("prometheus-operator", "monitoring", "quay.io/prometheus-operator/prometheus-operator:v0.74.0", 1, crdAge),
		// Mid-rollout on purpose: it is the workload behind the `grafana`
		// Helm release, which helm reports as plain `deployed`. Without a
		// workload that is actually moving, 18a's rollout arrow has nothing
		// to render in --demo and the signal is invisible.
		demoRollingDeployment("grafana", "monitoring", "grafana/grafana:10.4.2", 3, crdAge),
		demoStableDeployment("kube-state-metrics", "monitoring", "registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.12.0", 1, crdAge),
	)
	c.Seed(kube.KindStatefulSet,
		demoStatefulSetN("prometheus-k8s", "monitoring", 2, crdAge),
		demoStatefulSetN("alertmanager-main", "monitoring", 3, crdAge),
	)
	c.Seed(kube.KindService, demoService("grafana", "monitoring", map[string]string{"app": "grafana"}, crdAge))
	c.Seed(kube.KindPod,
		demoOwnedPod("prometheus-k8s-0", "monitoring", age(15*24*time.Hour), "node-a", "StatefulSet", "prometheus-k8s"),
		demoOwnedPod("grafana-5c8d9f-t9x4r", "monitoring", age(15*24*time.Hour), "node-b", "ReplicaSet", "grafana-5c8d9f"),
	)
}

// demoArgoCDFixtures seeds the argoproj.io CRD family: Application
// (namespaced, curated onto §33a's descriptor by kube.IsArgoGroup — Sync/
// Health read directly off status.sync.status/status.health.status, no
// Ready-style condition at all) and AppProject (which carries neither field
// and stays on the generic 14a custom-resource path, per §33a's own
// group-plus-Kind recognition), plus the argocd operator's own workloads.
// One row per §33a sort tier — Degraded, OutOfSync, Progressing, Healthy —
// so --demo mode exercises the whole precedence live.
func demoArgoCDFixtures(c *Cluster, age func(time.Duration) metav1.Time) {
	crdAge := age(45 * 24 * time.Hour)
	group := "argoproj.io"

	appCols := []kube.PrinterColumn{
		{Name: "Sync Status", Type: "string", JSONPath: ".status.sync.status"},
		{Name: "Health Status", Type: "string", JSONPath: ".status.health.status"},
	}

	c.Seed(kube.KindCustomResourceDefinition,
		demoCRD("applications.argoproj.io", group, "Application", "applications", "Namespaced", "v1alpha1", true, appCols, crdAge),
		demoCRD("appprojects.argoproj.io", group, "AppProject", "appprojects", "Namespaced", "v1alpha1", true, nil, crdAge),
	)
	c.SeedDiscovered(demoDiscoveredKind("Application", "applications", group, "v1alpha1", "applications.argoproj.io", false, appCols))
	c.SeedDiscovered(demoDiscoveredKind("AppProject", "appprojects", group, "v1alpha1", "appprojects.argoproj.io", false, nil))

	// billing: Synced + Degraded — git is right, the workload is sick.
	// Carries a real managed-resource health message so §33a's sub-line
	// (argoSubLine) has something to render.
	billing := demoArgoApplication("billing", "argocd", "default", age(90*24*time.Hour),
		"main", "e41b90c1f2a3b4c5d6e7f8091a2b3c4d5e6f7081", "Synced", "Degraded", age(11*time.Minute))
	setArgoResourceHealth(billing, "Deployment", "billing-api", "argocd", "Degraded",
		`container "api" is in CrashLoopBackOff — exit 1, 2m ago`)

	c.Seed(kube.ResourceKind("Application"),
		billing,
		// worker: OutOfSync + Healthy — drift, nothing actually broken.
		demoArgoApplication("worker", "argocd", "default", age(90*24*time.Hour),
			"main", "e41b90c1f2a3b4c5d6e7f8091a2b3c4d5e6f7081", "OutOfSync", "Healthy", age(2*time.Hour)),
		// web: Syncing + Progressing — a sync is actively running.
		demoArgoApplication("web", "argocd", "default", age(90*24*time.Hour),
			"main", "f77d215a8b9c0d1e2f30415263748596071829a0", "Syncing", "Progressing", age(30*time.Second)),
		// api: Synced + Healthy — the quiet state, folds behind the others.
		demoArgoApplication("api", "argocd", "default", age(90*24*time.Hour),
			"main", "f77d215a8b9c0d1e2f30415263748596071829a0", "Synced", "Healthy", age(18*time.Minute)),
	)
	c.Seed(kube.ResourceKind("AppProject"),
		demoCR(group+"/v1alpha1", "AppProject", "default", "argocd", age(45*24*time.Hour), nil),
	)

	// argocd-cm carries the dashboard's own base URL — §33a's 'u' reads
	// this exact key, the same one argocd-server itself uses to build its
	// own UI links.
	c.Seed(kube.KindConfigMap, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "argocd-cm", Namespace: "argocd", CreationTimestamp: crdAge},
		Data:       map[string]string{"url": "https://argocd.demo.local"},
	})

	c.Seed(kube.KindDeployment,
		demoStableDeployment("argocd-server", "argocd", "quay.io/argoproj/argocd:v2.10.7", 1, crdAge),
		demoStableDeployment("argocd-repo-server", "argocd", "quay.io/argoproj/argocd:v2.10.7", 1, crdAge),
		demoStableDeployment("argocd-redis", "argocd", "redis:7.0.15-alpine", 1, crdAge),
	)
	c.Seed(kube.KindStatefulSet, demoStatefulSetN("argocd-application-controller", "argocd", 1, crdAge))
	c.Seed(kube.KindService, demoLoadBalancerService("argocd-server", "argocd", 443, "203.0.113.20", crdAge))
	c.Seed(kube.KindPod,
		demoOwnedPod("argocd-server-6f9c8d-p4k9w", "argocd", age(12*24*time.Hour), "node-a", "ReplicaSet", "argocd-server-6f9c8d"),
		demoOwnedPod("argocd-application-controller-0", "argocd", age(12*24*time.Hour), "node-b", "StatefulSet", "argocd-application-controller"),
	)
}

// demoArgoApplication builds an Application instance whose printer-column
// fields (Sync/Health Status) carry the meaningful state — no synthetic
// Ready condition, per demoArgoCDFixtures' doc comment. targetRevision/
// revision feed §33a's REVISION cell (argoRevisionCell); syncedAt feeds its
// SYNCED cell (argoSyncedCell, status.operationState.finishedAt).
func demoArgoApplication(name, ns, project string, created metav1.Time, targetRevision, revision, syncStatus, healthStatus string, syncedAt metav1.Time) *unstructured.Unstructured {
	u := demoCR("argoproj.io/v1alpha1", "Application", name, ns, created, map[string]any{
		"project":     project,
		"source":      map[string]any{"targetRevision": targetRevision},
		"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": ns},
	})
	u.Object["status"] = map[string]any{
		"sync":           map[string]any{"status": syncStatus, "revision": revision},
		"health":         map[string]any{"status": healthStatus},
		"operationState": map[string]any{"finishedAt": syncedAt.UTC().Format(time.RFC3339)},
	}
	return u
}

// setArgoResourceHealth attaches one status.resources[] entry to an
// Application — §33a's argoSubLine reads the first non-Healthy entry's own
// health message verbatim, the same field a real Application's per-object
// health rollup carries.
func setArgoResourceHealth(u *unstructured.Unstructured, kind, name, ns, health, message string) {
	u.Object["status"].(map[string]any)["resources"] = []any{
		map[string]any{
			"kind": kind, "name": name, "namespace": ns,
			"health": map[string]any{"status": health, "message": message},
		},
	}
}

// demoCR builds a generic custom-resource instance: metadata + optional
// spec + optional status.conditions. The one shared constructor behind
// every non-cert-manager CRD instance below (cert-manager's own
// demoCertificate/demoCertificateRequest/demoClusterIssuer predate it and
// are left as they are) — CRD support being data, not code, extends to the
// fixtures that exercise it.
func demoCR(apiVersion, kind, name, ns string, created metav1.Time, spec map[string]any, conditions ...map[string]any) *unstructured.Unstructured {
	meta := map[string]any{
		"name":              name,
		"creationTimestamp": created.UTC().Format(time.RFC3339),
	}
	if ns != "" {
		meta["namespace"] = ns
	}
	obj := map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   meta,
	}
	if spec != nil {
		obj["spec"] = spec
	}
	if len(conditions) > 0 {
		conds := make([]any, len(conditions))
		for i, cond := range conditions {
			conds[i] = cond
		}
		obj["status"] = map[string]any{"conditions": conds}
	}
	return &unstructured.Unstructured{Object: obj}
}

// readyCondition builds a status.conditions entry of type "Ready" — the
// condition type projectCustomResource (internal/resources/crd.go) scans
// for on every discovered kind.
func readyCondition(ready bool, message string) map[string]any {
	status := "True"
	if !ready {
		status = "False"
	}
	cond := map[string]any{"type": "Ready", "status": status}
	if message != "" {
		cond["message"] = message
	}
	return cond
}

// demoDiscoveredKind builds the DiscoveredKind counterpart of one demoCRD
// call — kept as a small parallel constructor rather than re-deriving it
// from the unstructured object (fake fixtures build both representations by
// hand, same as every other demo* helper in this file).
//
// group/version are parameters rather than the cert-manager constants this
// started with: every consumer of a DiscoveredKind's Group renders it
// (14a's breadcrumb API tag, 14c's goto type label, the descriptor's own
// "custom resource · <group>" Describe), so hardcoding one group made the
// Prometheus and Argo fixtures claim to be cert-manager kinds on screen.
func demoDiscoveredKind(kind, plural, group, version, crdName string, clusterScoped bool, cols []kube.PrinterColumn) kube.DiscoveredKind {
	return kube.DiscoveredKind{
		GVR:            schema.GroupVersionResource{Group: group, Version: version, Resource: plural},
		Kind:           kind,
		Plural:         plural,
		Group:          group,
		Versions:       []kube.CRDVersion{{Name: version, Served: true, Storage: true}},
		ClusterScoped:  clusterScoped,
		PrinterColumns: cols,
		Established:    true,
		CRDName:        crdName,
	}
}

// demoCRD builds a CustomResourceDefinition unstructured object shaped the
// same way a real apiserver would serve it — the 14b CRDs list's row source.
func demoCRD(name, group, kind, plural, scope, version string, established bool, printerCols []kube.PrinterColumn, created metav1.Time) *unstructured.Unstructured {
	status := "True"
	if !established {
		status = "False"
	}
	cols := make([]any, 0, len(printerCols))
	for _, c := range printerCols {
		cols = append(cols, map[string]any{"name": c.Name, "type": c.Type, "jsonPath": c.JSONPath})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata": map[string]any{
			"name":              name,
			"creationTimestamp": created.UTC().Format(time.RFC3339),
		},
		"spec": map[string]any{
			"group": group,
			"names": map[string]any{"kind": kind, "plural": plural},
			"scope": scope,
			"versions": []any{
				map[string]any{
					"name": version, "served": true, "storage": true,
					"additionalPrinterColumns": cols,
				},
			},
		},
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "Established", "status": status},
			},
		},
	}}
}

// demoCertificate builds a cert-manager.io/v1 Certificate instance. The
// not-ready message ("Issuing certificate as Secret does not exist") is the
// design doc's own §14d example — CONDITIONS renders it verbatim, never
// paraphrased. reason feeds §35a's certchain STATE cell ("Ready=False ·
// Issuing") — empty for the plain 14a/14d fixtures, which predate it.
//
// notAfter/renewalTime/lastFailure feed §35b's own EXPIRES/RENEWAL columns
// and READY/Fail-vs-Warn split — all three are the zero Time (omitted from
// status entirely, cert-manager's own "not set yet" for a field that hasn't
// been written) on the plain 14a/14d/35a fixtures that predate 35b.
func demoCertificate(name, ns string, ready bool, reason, secret, issuer string, created, notAfter, renewalTime, lastFailure metav1.Time) *unstructured.Unstructured {
	status, message := "True", "Certificate is up to date and has not expired"
	if !ready {
		status, message = "False", "Issuing certificate as Secret does not exist"
	}
	cond := map[string]any{"type": "Ready", "status": status, "message": message}
	if reason != "" {
		cond["reason"] = reason
	}
	certStatus := map[string]any{"conditions": []any{cond}}
	if !notAfter.IsZero() {
		certStatus["notAfter"] = notAfter.UTC().Format(time.RFC3339)
	}
	if !renewalTime.IsZero() {
		certStatus["renewalTime"] = renewalTime.UTC().Format(time.RFC3339)
	}
	if !lastFailure.IsZero() {
		certStatus["lastFailureTime"] = lastFailure.UTC().Format(time.RFC3339)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name": name, "namespace": ns,
			"creationTimestamp": created.UTC().Format(time.RFC3339),
		},
		"spec": map[string]any{
			"secretName": secret,
			"issuerRef":  map[string]any{"name": issuer, "kind": "ClusterIssuer"},
		},
		"status": certStatus,
	}}
}

// demoCertificateRequest builds a plain CertificateRequest, Ready true or
// false with no Approved/Denied condition set — certOwner wires the
// OwnerReference §35a's certchain walks Certificate → CertificateRequest by
// (Kind+Name, never UID — the same match style every demo fixture's
// OwnerReferences already use).
func demoCertificateRequest(name, ns string, ready bool, issuer, certOwner string, created metav1.Time) *unstructured.Unstructured {
	status := "True"
	if !ready {
		status = "False"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "CertificateRequest",
		"metadata": map[string]any{
			"name": name, "namespace": ns,
			"creationTimestamp": created.UTC().Format(time.RFC3339),
			"ownerReferences":   []any{ownerRefMap("cert-manager.io/v1", "Certificate", certOwner)},
		},
		"spec": map[string]any{
			"issuerRef": map[string]any{"name": issuer, "kind": "ClusterIssuer"},
		},
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "Ready", "status": status},
			},
		},
	}}
}

// demoCertificateRequestApproved is §35a's "Approved · not Ready" chain
// row — approved by the ACME issuer's auto-approver but not yet Ready,
// which is normal mid-issuance progress (certchain's certRequestNode never
// treats this alone as a failure).
func demoCertificateRequestApproved(name, ns, certOwner, issuer string, created metav1.Time) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "CertificateRequest",
		"metadata": map[string]any{
			"name": name, "namespace": ns,
			"creationTimestamp": created.UTC().Format(time.RFC3339),
			"ownerReferences":   []any{ownerRefMap("cert-manager.io/v1", "Certificate", certOwner)},
		},
		"spec": map[string]any{
			"issuerRef": map[string]any{"name": issuer, "kind": "ClusterIssuer"},
		},
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "Approved", "status": "True"},
				map[string]any{"type": "Ready", "status": "False"},
			},
		},
	}}
}

// demoOrder builds an acme.cert-manager.io/v1 Order, owned by its
// CertificateRequest. Order carries status.state, not a Ready-style
// condition — §35a's certchain reads it directly (orderNode).
func demoOrder(name, ns, certRequestOwner, state, reason string, created metav1.Time) *unstructured.Unstructured {
	status := map[string]any{"state": state}
	if reason != "" {
		status["reason"] = reason
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "acme.cert-manager.io/v1",
		"kind":       "Order",
		"metadata": map[string]any{
			"name": name, "namespace": ns,
			"creationTimestamp": created.UTC().Format(time.RFC3339),
			"ownerReferences":   []any{ownerRefMap("cert-manager.io/v1", "CertificateRequest", certRequestOwner)},
		},
		"status": status,
	}}
}

// demoChallenge builds an acme.cert-manager.io/v1 Challenge, owned by its
// Order. reason mirrors real cert-manager behavior: it's populated only to
// explain something currently going wrong, never as routine narration — the
// signal §35a's acmeStateClass reads to call a "pending" Challenge with a
// reason a failure.
func demoChallenge(name, ns, orderOwner, typ, dnsName, state, reason string, created metav1.Time) *unstructured.Unstructured {
	status := map[string]any{"state": state}
	if reason != "" {
		status["reason"] = reason
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "acme.cert-manager.io/v1",
		"kind":       "Challenge",
		"metadata": map[string]any{
			"name": name, "namespace": ns,
			"creationTimestamp": created.UTC().Format(time.RFC3339),
			"ownerReferences":   []any{ownerRefMap("acme.cert-manager.io/v1", "Order", orderOwner)},
		},
		"spec": map[string]any{
			"type":    typ,
			"dnsName": dnsName,
		},
		"status": status,
	}}
}

// ownerRefMap builds one metadata.ownerReferences entry in unstructured
// form — read back by (*unstructured.Unstructured).GetOwnerReferences(),
// which every demo cert-manager fixture above relies on for §35a's
// ownerRef-chain walk.
func ownerRefMap(apiVersion, kind, name string) map[string]any {
	return map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"name":       name,
		"controller": true,
	}
}

func demoClusterIssuer(name string, ready bool, created metav1.Time) *unstructured.Unstructured {
	status := "True"
	if !ready {
		status = "False"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "ClusterIssuer",
		"metadata": map[string]any{
			"name":              name,
			"creationTimestamp": created.UTC().Format(time.RFC3339),
		},
		"spec": map[string]any{
			"acme": map[string]any{"email": "ops@demo.local"},
		},
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "Ready", "status": status},
			},
		},
	}}
}

func demoPod(name, ns string, created metav1.Time, phase corev1.PodPhase, qos corev1.PodQOSClass, node string, ready bool, restarts int32, terminated *corev1.ContainerStateTerminated) *corev1.Pod {
	state := corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: created}}
	if terminated != nil {
		state = corev1.ContainerState{Terminated: terminated}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: created},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name: "app", Image: "app:1.0",
				// Every demo pod exposes a container port so 13a/13c's
				// forward picker has something to offer. gatewayPod above
				// deliberately takes its own away — it is --demo's
				// free-port subject — and every other seeded pod keeps one
				// because a single declared row is the cheapest way to reach
				// the declared-port half of the picker at all.
				Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
				// Every demo pod carries a request/limit pair so 2a/5a/6a's
				// CPU/MEM bars have a real denominator in --demo mode
				// (CLAUDE.md: "the fake provider must stay feature-complete
				// for tests/demo mode") — PodMetricsByNamespace below
				// synthesizes the matching usage numerator.
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase:    phase,
			PodIP:    "10.0.0.5",
			QOSClass: qos,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", Ready: ready, RestartCount: restarts, State: state},
			},
		},
	}
}

func demoCrashLoopPod(name, ns string, created metav1.Time, node string) *corev1.Pod {
	pod := demoPod(name, ns, created, corev1.PodRunning, corev1.PodQOSBurstable, node, false, 6, nil)
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
	}
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{
			ExitCode:    1,
			Reason:      "Error",
			FinishedAt:  metav1.NewTime(created.Add(4 * time.Minute)),
			ContainerID: "containerd://" + name + "-previous",
		},
	}
	return pod
}

func demoPendingPod(name, ns string, created metav1.Time) *corev1.Pod {
	pod := demoPod(name, ns, created, corev1.PodPending, corev1.PodQOSBurstable, "", false, 0, nil)
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
	}
	return pod
}

func demoCompletedPod(name, ns string, created metav1.Time) *corev1.Pod {
	return demoPod(name, ns, created, corev1.PodSucceeded, corev1.PodQOSBestEffort, "node-a", true, 0,
		&corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed", FinishedAt: metav1.NewTime(created.Add(2 * time.Minute))})
}

// demoAPIPorts is the "api" container's declared port set — three ports, one
// container, shared by apiPod and the api Deployment's own pod template so the
// two fixtures can't drift into disagreeing about what `f` finds on each.
// Declared in ascending order so 13a's list reads like it sorts. The numbers
// are all non-privileged on purpose: 8080 has to stay the "http" port
// demoService's targetPort resolves to, and inventing a port below 1024 here
// would buy nothing — the privileged→8080 pre-fill branch is pinned by
// TestPreferredLocalPort, and the busy→bump branch depends on whatever the
// demo machine happens to be listening on, so neither belongs in a fixture.
func demoAPIPorts() []corev1.ContainerPort {
	return []corev1.ContainerPort{
		{Name: "http", ContainerPort: 8080},
		{Name: "admin", ContainerPort: 8081},
		{Name: "metrics", ContainerPort: 9090},
	}
}

func demoMidRolloutDeployment(name, ns string, created metav1.Time) *appsv1.Deployment {
	replicas := int32(3)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: created, Generation: 2},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "api", Image: "api:2.1",
					// The same three ports apiPod's own "api" container
					// declares, so 13a offers an identical list whether `f`
					// was pressed on this Deployment or on its pod.
					Ports: demoAPIPorts(),
					// Same request/limit pair demoPod's generic container
					// carries, so 25a's Set Resources CURRENT column isn't
					// empty and its usage bar has a real denominator to
					// render a fill against (docs/design README.md §25a:
					// "requests and limits edit next to the live usage bar
					// for that exact container") — apiPod's own "api"
					// container (renamed to match, above) reports the
					// matching live usage.
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				}}},
			},
		},
		Status: appsv1.DeploymentStatus{
			Replicas:           3,
			UpdatedReplicas:    1,
			ReadyReplicas:      2,
			AvailableReplicas:  2,
			ObservedGeneration: 1, // < Generation: rollout still in progress
		},
	}
}

func demoReplicaSet(name, ns, deployment, image string, created metav1.Time) *appsv1.ReplicaSet {
	return demoReplicaSetRevision(name, ns, deployment, image, 1, created)
}

// demoReplicaSetRevision is demoReplicaSet generalized to a caller-chosen
// revision, so a Deployment fixture can seed more than one owned ReplicaSet
// (e.g. "api"'s own revision-1/revision-2 pair — 24a's set-image history
// table reads real revision history off exactly this shape).
func demoReplicaSetRevision(name, ns, deployment, image string, revision int, created metav1.Time) *appsv1.ReplicaSet {
	replicas := int32(1)
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, CreationTimestamp: created,
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: deployment}},
			// The revision annotation is what a real kube-controller-manager
			// stamps on every ReplicaSet it owns — tasks/timeline's 16b
			// revision rail reads it (kube.TimelineFromRollouts), so demo
			// mode needs it too for that rail to have anything to show.
			Annotations: map[string]string{"deployment.kubernetes.io/revision": strconv.Itoa(revision)},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			// A real image, not an empty template — 9a's live "new ← old"
			// rollout transition (resources.previousReplicaSetImage) needs
			// this ReplicaSet to actually carry the deployment's previous
			// image, the same way a real cluster's own ReplicaSets do.
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: deployment, Image: image}}},
			},
		},
		Status: appsv1.ReplicaSetStatus{Replicas: 1, ReadyReplicas: 1},
	}
}

// demoHPA is 17b's HPA-managed-workload fixture: a HorizontalPodAutoscaler
// whose scaleTargetRef points at targetKind/targetName, so beginScale's
// hpaManaging lookup finds it live in --demo mode.
func demoHPA(name, ns, targetKind, targetName string) *autoscalingv2.HorizontalPodAutoscaler {
	minReplicas := int32(2)
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: targetKind, Name: targetName},
			MinReplicas:    &minReplicas,
			MaxReplicas:    10,
		},
	}
}

func demoStatefulSetN(name, ns string, replicas int32, created metav1.Time) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: created},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
		// UpdatedReplicas matters as much as ReadyReplicas: a settled
		// StatefulSet reports every replica on the current revision, and
		// leaving it zero makes a fully-ready fixture read as permanently
		// mid-rollout (resources.UnsettledWorkloads, 18a's rollout glyph).
		Status: appsv1.StatefulSetStatus{Replicas: replicas, ReadyReplicas: replicas, UpdatedReplicas: replicas},
	}
}

// demoWorkerStatefulSet is "worker"'s own exemplar (like demoMidRolloutDeployment
// is "api"'s) — a real container image, so 24a's set-image editor has
// something to open on the one StatefulSet --demo mode ships.
func demoWorkerStatefulSet(name, ns string, created metav1.Time) *appsv1.StatefulSet {
	s := demoStatefulSetN(name, ns, 1, created)
	s.Spec.Template = corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: "worker:1.0.0"}}},
	}
	return s
}

// demoCronJob is CronJobs' own exemplar builder (§36a) — schedule/suspend
// are the two fields projectCronJob's NEXT RUN/SUSPEND columns actually
// read, so --demo mode exercises the real column derivation rather than a
// zero-value stub. UID/ResourceVersion are set (unlike most of this file's
// other demo builders) because Phase 2's SetCronJobSuspend/SetCronJobSchedule
// both require a non-empty resourceVersion precondition — a CronJob with the
// zero value would make every --demo suspend/resume/schedule-edit fail with
// "missing resourceVersion precondition" before ever reaching the fake
// cluster's own optimistic-concurrency check.
func demoCronJob(name, ns, schedule string, suspend bool, created metav1.Time) *batchv1.CronJob {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, CreationTimestamp: created,
			UID: types.UID("demo-cronjob-" + ns + "-" + name), ResourceVersion: "1", Generation: 1,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: schedule,
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: "job", Image: "busybox:1.36"}},
			}}}},
		},
	}
	if suspend {
		cj.Spec.Suspend = &suspend
	}
	return cj
}

// int32Val is the pointer-int32 constructor batchv1's Completions/
// BackoffLimit/history-limit fields all need — a tiny local helper rather
// than a `v := int32(n); &v` pair repeated at every call site.
func int32Val(v int32) *int32 { return &v }

// demoJobBase is every demo Job's shared skeleton — UID/ResourceVersion
// stable and derived from name (Plan Phase 8 task 9), matching
// demoCronJob's own reasoning: resources.BuildCronJobSummaries joins on
// these, so a zero-value UID would make every association below resolve by
// the sparse-fixture namespace/name fallback instead of the UID path
// real clusters actually use.
func demoJobBase(name, ns string, created metav1.Time) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, CreationTimestamp: created,
			UID: types.UID("demo-job-" + ns + "-" + name), ResourceVersion: "1",
		},
		Spec: batchv1.JobSpec{Completions: int32Val(1), BackoffLimit: int32Val(2)},
	}
}

// jobOwnerRef builds the controller owner reference
// resources.controllerOwnerRef looks for (§4.2 rule 1) — Kind read from
// kube.KindCronJob.APIKind() by every real caller in this file, never a
// bare string literal, matching the invariant the association code itself
// documents.
func jobOwnerRef(cronJobName string, cronJobUID types.UID) metav1.OwnerReference {
	isController := true
	return metav1.OwnerReference{
		APIVersion: "batch/v1", Kind: kube.KindCronJob.APIKind(),
		Name: cronJobName, UID: cronJobUID, Controller: &isController,
	}
}

// demoScheduledSucceededJob is one CronJob-controller-owned Job (§4.2 rule
// 1) that ran to completion — §36a's "successful latest run" exemplar, and
// (passed a stale cronJobUID) also the "current and stale same-name owner
// UID" exclusion fixture, since the association is keyed on UID, not name.
func demoScheduledSucceededJob(name, ns, cronJobName string, cronJobUID types.UID, started, completed metav1.Time) *batchv1.Job {
	j := demoJobBase(name, ns, started)
	j.OwnerReferences = []metav1.OwnerReference{jobOwnerRef(cronJobName, cronJobUID)}
	j.Status = batchv1.JobStatus{
		Succeeded: 1, StartTime: &started, CompletionTime: &completed,
		Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: completed}},
	}
	return j
}

// demoScheduledFailedJob is §3.5's failed-latest-run exemplar: a Failed
// condition with a real reason/message, no CompletionTime (the API never
// sets one on failure) — the stable, authoritative source
// resources.jobTerminalOutcome reads, which demoFailedJobPod's Pod
// termination only supplements.
func demoScheduledFailedJob(name, ns, cronJobName string, cronJobUID types.UID, started metav1.Time, reason, message string) *batchv1.Job {
	j := demoJobBase(name, ns, started)
	j.OwnerReferences = []metav1.OwnerReference{jobOwnerRef(cronJobName, cronJobUID)}
	failedAt := metav1.NewTime(started.Add(90 * time.Second))
	j.Status = batchv1.JobStatus{
		Failed: 1, StartTime: &started,
		Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: reason, Message: message, LastTransitionTime: failedAt}},
	}
	return j
}

// demoScheduledActiveJob is a CronJob-controller-owned Job still running —
// §4.3's "ACT shows 1" case, paired with a prior demoScheduledSucceededJob
// so the CronJob's own LAST RUN keeps naming the newest *terminal* run
// rather than this one.
func demoScheduledActiveJob(name, ns, cronJobName string, cronJobUID types.UID, started metav1.Time) *batchv1.Job {
	j := demoJobBase(name, ns, started)
	j.OwnerReferences = []metav1.OwnerReference{jobOwnerRef(cronJobName, cronJobUID)}
	j.Status = batchv1.JobStatus{Active: 1, StartTime: &started}
	return j
}

// demoManualJob is §36b's run-now exemplar: ownerless (§3.4 — Kute never
// fakes an owner reference for a manual run, so CronJob history GC can't
// delete it) and associated purely through Kute's own
// AnnotationCronJobName/AnnotationCronJobUID annotations (§4.2 rule 2),
// matching kube.Cluster.TriggerCronJob/fake.Cluster.TriggerCronJob's own
// annotation set.
func demoManualJob(name, ns, cronJobName string, cronJobUID types.UID, creator string, triggeredAt, started, completed metav1.Time) *batchv1.Job {
	j := demoJobBase(name, ns, triggeredAt)
	j.Annotations = map[string]string{
		"cronjob.kubernetes.io/instantiate": "manual",
		kube.AnnotationCronJobName:          cronJobName,
		kube.AnnotationCronJobUID:           string(cronJobUID),
		kube.AnnotationTriggeredBy:          creator,
		kube.AnnotationTriggeredAt:          triggeredAt.Time.UTC().Format(time.RFC3339),
	}
	j.Status = batchv1.JobStatus{
		Succeeded: 1, StartTime: &started, CompletionTime: &completed,
		Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: completed}},
	}
	return j
}

// demoUnrelatedJob carries neither a CronJob-kind owner reference nor
// either Kute annotation — §4.2's "never associate by name prefix" proof:
// its own name deliberately collides with nightlyBackup's, and
// cronJobAssociation must still return ok=false for it.
func demoUnrelatedJob(name, ns string, created metav1.Time) *batchv1.Job {
	j := demoJobBase(name, ns, created)
	completed := metav1.NewTime(created.Add(3 * time.Minute))
	j.Status = batchv1.JobStatus{
		Succeeded: 1, StartTime: &created, CompletionTime: &completed,
		Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: completed}},
	}
	return j
}

// demoFailedJobPod is §3.5 point 2's Pod-termination supplement: a Job-
// owned Pod whose own terminated container carries the exit code/reason
// the owning Job's condition alone can't (a Job condition has no exit
// code). Association is by Job controller owner reference, the same
// indexPodsByJob reads for every real cluster's Pods.
func demoFailedJobPod(name, ns string, created metav1.Time, jobName string, jobUID types.UID) *corev1.Pod {
	p := demoPod(name, ns, created, corev1.PodFailed, corev1.PodQOSBurstable, "node-a", false, 0,
		&corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", Message: "backoff limit exceeded", FinishedAt: metav1.NewTime(created.Add(90 * time.Second))})
	p.Status.StartTime = &created
	isController := true
	p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: kube.KindJob.APIKind(), Name: jobName, UID: jobUID, Controller: &isController}}
	return p
}

// demoIndexedJob is §37d's own exemplar — the only demo Job with
// CompletionMode: Indexed, so tasks/jobattempts' completion-map/index-grid
// widget (resources.ProjectJobIndexGrid) is reachable at all in --demo
// mode; every other demo Job above leaves CompletionMode nil
// (NonIndexed), which only ever exercises §37b's flat attempt list.
// Standalone like demoUnrelatedJob (no CronJob owner) — the design
// mockup's own "reindex-search" job is ownerless too
// (docs/design/v.0.9.0.dc.html id="37d": "job/reindex-search"). Stays
// Active overall (no Complete/Failed condition): only index 3 has
// retried, well under BackoffLimit, so the Job itself hasn't given up —
// the mockup's own "◐ Running" header state. Completions/Parallelism
// mirror the mockup exactly: 12 indexes, parallelism 4.
func demoIndexedJob(name, ns string, started metav1.Time) *batchv1.Job {
	j := demoJobBase(name, ns, started)
	indexed := batchv1.IndexedCompletion
	j.Spec.CompletionMode = &indexed
	j.Spec.Completions = int32Val(12)
	j.Spec.Parallelism = int32Val(4)
	// 6, not demoJobBase's default 2 — index 3's second OOMKilled attempt
	// would otherwise read as "about to give up"; 6 keeps it a mid-flight
	// retry, matching the mockup's own "counts against backoffLimit 6".
	j.Spec.BackoffLimit = int32Val(6)
	j.Status = batchv1.JobStatus{Active: 3, Succeeded: 7, Failed: 2, StartTime: &started}
	return j
}

// demoIndexedJobPod is §37d's own indexed-pod builder — a Job-owned Pod
// stamped with the completion-index annotation the Job controller sets on
// every Indexed job's pods (jobCompletionIndexAnnotation in
// resources/jobattempts.go — fixtures.go is a different package, so the
// literal is repeated here rather than imported). Association to the
// owning Job is by controller owner reference alone, the same as
// demoFailedJobPod; index association is by annotation alone, the same as
// resources.jobIndex reads.
func demoIndexedJobPod(name, ns string, index int32, created metav1.Time, phase corev1.PodPhase, terminated *corev1.ContainerStateTerminated, jobName string, jobUID types.UID) *corev1.Pod {
	ready := phase != corev1.PodFailed
	p := demoPod(name, ns, created, phase, corev1.PodQOSBurstable, "node-a", ready, 0, terminated)
	p.Status.StartTime = &created
	p.Annotations = map[string]string{"batch.kubernetes.io/job-completion-index": strconv.Itoa(int(index))}
	isController := true
	p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: kube.KindJob.APIKind(), Name: jobName, UID: jobUID, Controller: &isController}}
	return p
}

// demoControllerRevision is a StatefulSet/DaemonSet ControllerRevision
// fixture — the apps/v1 rollout-history mechanism those two controllers use
// in place of a Deployment's owned ReplicaSets (24a's set-image history
// table reads it the same way `kubectl rollout history` does). Data.Raw
// mirrors the real patch shape controllerRevisionContainerImage (browse's
// setimage.go) decodes.
func demoControllerRevision(name, ns, ownerKind, owner, container, image string, revision int64, created metav1.Time) *appsv1.ControllerRevision {
	data, _ := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []map[string]any{{"name": container, "image": image}},
				},
			},
		},
	})
	return &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, CreationTimestamp: created,
			OwnerReferences: []metav1.OwnerReference{{Kind: ownerKind, Name: owner}},
		},
		Revision: revision,
		Data:     runtime.RawExtension{Raw: data},
	}
}

// demoStableDeployment is a fully-ready Deployment with no rollout in
// progress — every add-on namespace's steady-state workloads use this
// rather than demoMidRolloutDeployment, which stays "api"'s own exemplar.
func demoStableDeployment(name, ns, image string, replicas int32, created metav1.Time) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: created, Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: image}}},
			},
		},
		Status: appsv1.DeploymentStatus{
			Replicas:           replicas,
			UpdatedReplicas:    replicas,
			ReadyReplicas:      replicas,
			AvailableReplicas:  replicas,
			ObservedGeneration: 1,
		},
	}
}

// demoRollingDeployment is demoStableDeployment part-way through a rollout:
// the new revision is applied and observed, but only one replica has been
// replaced so far. Distinct from demoMidRolloutDeployment, which sits in the
// earlier window where the controller hasn't observed the new generation at
// all — both are "progressing", and having one of each keeps 9a's ROLLOUT
// cell honest about the two ways it gets there.
func demoRollingDeployment(name, ns, image string, replicas int32, created metav1.Time) *appsv1.Deployment {
	d := demoStableDeployment(name, ns, image, replicas, created)
	d.Generation = 2
	d.Status.ObservedGeneration = 2
	d.Status.UpdatedReplicas = 1
	d.Status.ReadyReplicas = replicas - 1
	d.Status.AvailableReplicas = replicas - 1
	return d
}

// demoDaemonSetReady is a fully-scheduled, fully-ready DaemonSet — every
// node in the demo cluster running its pod.
func demoDaemonSetReady(name, ns string, desired int32, created metav1.Time) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: created},
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: desired,
			NumberReady:            desired,
			NumberAvailable:        desired,
			// Same reason as demoStatefulSetN's UpdatedReplicas: without it
			// a fully-ready DaemonSet reads as still rolling.
			UpdatedNumberScheduled: desired,
		},
	}
}

// demoOwnedPod is a running, ready pod owned by ownerKind/ownerName — the
// representative single pod each add-on Deployment/DaemonSet/StatefulSet
// above gets, the same asymmetry "api"'s own single apiPod already has
// against its 3-replica Deployment (a whole cluster of fixtures beats
// exhaustive replica-for-replica pod objects for a demo dataset).
func demoOwnedPod(name, ns string, created metav1.Time, node, ownerKind, ownerName string) *corev1.Pod {
	pod := demoPod(name, ns, created, corev1.PodRunning, corev1.PodQOSBurstable, node, true, 0, nil)
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: ownerKind, Name: ownerName}}
	return pod
}

func demoService(name, ns string, selector map[string]string, created metav1.Time) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: created},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "10.0.0.10",
			Selector:  selector,
			// Port 80 routes to every demo pod's named "http" container
			// port (8080) — the common shape, and the one 13a's forward
			// picker must translate (Service port → targetPort) rather
			// than dial the pod on 80.
			Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromString("http")}},
		},
	}
}

// demoLoadBalancerService is a Service with an assigned external IP — the
// shape an ingress controller/API-gateway Service takes, distinct from
// demoService's internal ClusterIP shape.
func demoLoadBalancerService(name, ns string, port int32, externalIP string, created metav1.Time) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: created},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeLoadBalancer,
			ClusterIP: "10.0.0.20",
			Ports:     []corev1.ServicePort{{Port: port}},
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{IP: externalIP}},
			},
		},
	}
}

func demoIngress(name, ns, backendService, host string, created metav1.Time) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: created},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: backendService,
									Port: networkingv1.ServiceBackendPort{Number: 80},
								},
							},
						}},
					},
				},
			}},
		},
		Status: networkingv1.IngressStatus{
			LoadBalancer: networkingv1.IngressLoadBalancerStatus{
				Ingress: []networkingv1.IngressLoadBalancerIngress{{IP: "203.0.113.10"}},
			},
		},
	}
}

// demoIngressWithTLS exercises 23a's TLS strip and both remaining backend
// glyphs beyond demoIngress' plain single-rule/single-● shape: goodBackend's
// path resolves ●, brokenBackend's path resolves ✕ (no such Service exists).
func demoIngressWithTLS(name, ns, goodBackend, brokenBackend, host, secretName string, created metav1.Time) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: created},
		Spec: networkingv1.IngressSpec{
			TLS: []networkingv1.IngressTLS{{Hosts: []string{host}, SecretName: secretName}},
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{
								Path: "/", PathType: &pathType,
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{Name: goodBackend, Port: networkingv1.ServiceBackendPort{Number: 80}},
								},
							},
							{
								Path: "/admin", PathType: &pathType,
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{Name: brokenBackend, Port: networkingv1.ServiceBackendPort{Number: 80}},
								},
							},
						},
					},
				},
			}},
		},
		Status: networkingv1.IngressStatus{
			LoadBalancer: networkingv1.IngressLoadBalancerStatus{
				Ingress: []networkingv1.IngressLoadBalancerIngress{{IP: "203.0.113.11"}},
			},
		},
	}
}

// demoTLSSecret builds a kubernetes.io/tls Secret whose tls.crt is a real
// (self-signed) certificate expiring at notAfter — routetable's cert-expiry
// parsing (crypto/x509) needs actual DER/PEM data to exercise, not a stub
// byte slice.
func demoTLSSecret(name, ns string, notAfter time.Time, created metav1.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: created},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": demoTLSCert(notAfter), "tls.key": []byte("demo-key")},
	}
}

// demoTLSCert self-signs a throwaway certificate valid from one year before
// notAfter through notAfter.
func demoTLSCert(notAfter time.Time) []byte {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "demo"},
		NotBefore:    notAfter.Add(-365 * 24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// demoGatewayAPIFixtures seeds 23b's Gateway API fixtures — discovered like
// any CRD (Gateway/HTTPRoute are never DefaultRegistry entries): a Gateway
// with an HTTPS+HTTP listener pair, an accepted HTTPRoute with a weighted
// canary split (demonstrates the "└ same match" + "%" rows), and a
// not-accepted HTTPRoute (demonstrates the ATTACHED red-row footgun
// docs/design README.md §23b calls out).
func demoGatewayAPIFixtures(c *Cluster, age func(time.Duration) metav1.Time) {
	const group = "gateway.networking.k8s.io"
	gwAge := age(45 * 24 * time.Hour)

	c.Seed(kube.KindCustomResourceDefinition,
		demoCRD("gateways."+group, group, "Gateway", "gateways", "Namespaced", "v1", true, nil, gwAge),
		demoCRD("httproutes."+group, group, "HTTPRoute", "httproutes", "Namespaced", "v1", true, nil, gwAge),
	)
	c.SeedDiscovered(demoDiscoveredKind("Gateway", "gateways", group, "v1", "gateways."+group, false, nil))
	c.SeedDiscovered(demoDiscoveredKind("HTTPRoute", "httproutes", group, "v1", "httproutes."+group, false, nil))

	// web-canary has no matching pods (Ready=0) so its split leg renders ▲,
	// distinct from web's ● (the same Service demoProductionFixtures' pods
	// already match).
	c.Seed(kube.KindService, demoService("web-canary", "production", map[string]string{"app": "web-canary"}, gwAge))
	c.Seed(kube.KindSecret, demoTLSSecret("gw-tls", "production", time.Now().Add(100*24*time.Hour), gwAge))
	c.Seed(kube.ResourceKind("Gateway"), demoGateway("public", "production", gwAge, 1))
	c.Seed(kube.ResourceKind("HTTPRoute"),
		demoHTTPRoute("web-route", "production", "public", gwAge, true, []map[string]any{
			{"name": "web", "port": int64(80), "weight": int64(90)},
			{"name": "web-canary", "port": int64(80), "weight": int64(10)},
		}),
		demoHTTPRoute("orphan-route", "production", "public", gwAge, false, []map[string]any{
			{"name": "web", "port": int64(80), "weight": int64(1)},
		}),
	)
}

// demoGateway builds a Gateway with an HTTPS (TLS-terminating) and an HTTP
// listener; attachedHTTPS is the https listener's status.listeners
// attachedRoutes count (the 23b "N routes attached" cell).
func demoGateway(name, ns string, created metav1.Time, attachedHTTPS int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata": map[string]any{
			"name":              name,
			"namespace":         ns,
			"creationTimestamp": created.UTC().Format(time.RFC3339),
		},
		"spec": map[string]any{
			"gatewayClassName": "nginx",
			"listeners": []any{
				map[string]any{
					"name": "https", "protocol": "HTTPS", "port": int64(443), "hostname": "*.demo.local",
					"tls": map[string]any{"certificateRefs": []any{map[string]any{"name": "gw-tls"}}},
				},
				map[string]any{
					"name": "http", "protocol": "HTTP", "port": int64(80), "hostname": "*.demo.local",
				},
			},
		},
		"status": map[string]any{
			"listeners": []any{
				map[string]any{"name": "https", "attachedRoutes": attachedHTTPS},
				map[string]any{"name": "http", "attachedRoutes": int64(0)},
			},
		},
	}}
}

// demoHTTPRoute builds an HTTPRoute attached to parentName's "https"
// listener, one rule matching "/" with the given weighted backendRefs.
// accepted false renders the design's "verbatim condition message" ATTACHED
// footgun copy.
func demoHTTPRoute(name, ns, parentName string, created metav1.Time, accepted bool, backends []map[string]any) *unstructured.Unstructured {
	condStatus := "True"
	cond := map[string]any{"type": "Accepted", "status": condStatus}
	if !accepted {
		cond["status"] = "False"
		cond["message"] = "no matching listener hostname"
	}

	refs := make([]any, len(backends))
	for i, b := range backends {
		refs[i] = b
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata": map[string]any{
			"name":              name,
			"namespace":         ns,
			"creationTimestamp": created.UTC().Format(time.RFC3339),
		},
		"spec": map[string]any{
			"parentRefs": []any{map[string]any{"name": parentName}},
			"hostnames":  []any{name + ".demo.local"},
			"rules": []any{
				map[string]any{
					"matches":     []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": "/"}}},
					"backendRefs": refs,
				},
			},
		},
		"status": map[string]any{
			"parents": []any{
				map[string]any{
					"parentRef":  map[string]any{"name": parentName, "sectionName": "https"},
					"conditions": []any{cond},
				},
			},
		},
	}}
}

func demoNode(name string, ready, memoryPressure, cordoned bool) *corev1.Node {
	conditions := []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: boolCondition(ready)},
	}
	if memoryPressure {
		conditions = append(conditions, corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue})
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: cordoned},
		Status: corev1.NodeStatus{
			Conditions: conditions,
			// Capacity is the machine; Allocatable is what's left after
			// kube-reserved/system-reserved/eviction-threshold. The gap is
			// modelled on a real managed node (AKS reserves ~3Gi of 16Gi)
			// because the two are different denominators — usage bars
			// divide by Capacity, request bars by Allocatable — and a
			// fixture where they're equal would let that distinction
			// regress without any test noticing.
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("3860m"),
				corev1.ResourceMemory: resource.MustParse("13Gi"),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
		},
	}
}

func boolCondition(v bool) corev1.ConditionStatus {
	if v {
		return corev1.ConditionTrue
	}
	return corev1.ConditionFalse
}

func demoNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
}

func demoConfigMap(name, ns string, created metav1.Time) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: created},
		Data:       map[string]string{"key": "value"},
	}
}

func demoSecret(name, ns string, created metav1.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: created},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"key": []byte("value")},
	}
}

func demoEvent(name, ns, kind, objName, typ, reason, message string, count int32, last metav1.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: objName, Namespace: ns},
		Type:           typ,
		Reason:         reason,
		Message:        message,
		Count:          count,
		FirstTimestamp: last,
		LastTimestamp:  last,
	}
}

// demoHelmReleaseFixtures seeds 18a's Helm Releases list — encoded as real
// helm.sh/release.v1 Secrets (kube.EncodeHelmReleaseSecret) so browsing goes
// through the exact same decode path a real cluster's release secrets would.
// Five releases match docs/design README.md §18a's own strip example
// verbatim ("3 deployed · 1 pending-upgrade · 1 failed"); postgresql and
// redis carry multiple superseded revisions so 'h' history has a real rail
// to show.
func demoHelmReleaseFixtures(c *Cluster, age func(time.Duration) metav1.Time) {
	rev := func(name, namespace, chart, chartVersion, appVersion, status, reason string, revision int, values string, d time.Duration) *corev1.Secret {
		return kube.EncodeHelmReleaseSecret(kube.HelmRelease{
			Name: name, Namespace: namespace, Chart: chart, ChartVersion: chartVersion, AppVersion: appVersion,
			Revision: revision, Status: status, StatusReason: reason, Updated: age(d).Time, Values: values,
			Manifest: demoReleaseManifest(chart, name),
		})
	}

	// postgresql (production): three revisions, the newest deployed — 'h'
	// shows a real superseded/superseded/deployed rail.
	c.Seed(kube.KindSecret,
		rev("postgresql", "production", "postgresql", "12.1.7", "15.3.0", "superseded", "", 1, "auth:\n  enablePostgresUser: true\n", 20*24*time.Hour),
		rev("postgresql", "production", "postgresql", "12.1.8", "15.4.0", "superseded", "", 2, "auth:\n  enablePostgresUser: true\nprimary:\n  persistence:\n    size: 8Gi\n", 10*24*time.Hour),
		rev("postgresql", "production", "postgresql", "12.1.9", "15.4.0", "deployed", "", 3, "auth:\n  enablePostgresUser: true\nprimary:\n  persistence:\n    size: 8Gi\n", 2*24*time.Hour),
	)
	// redis (production): two revisions, deployed.
	c.Seed(kube.KindSecret,
		rev("redis", "production", "redis", "18.1.4", "7.2.3", "superseded", "", 1, "architecture: standalone\n", 15*24*time.Hour),
		rev("redis", "production", "redis", "18.1.5", "7.2.4", "deployed", "", 2, "architecture: standalone\nauth:\n  enabled: true\n", 6*24*time.Hour),
	)
	// grafana (monitoring): deployed, single revision — and the one release
	// whose own workload is still rolling (demoPrometheusFixtures seeds the
	// grafana Deployment mid-rollout). Helm calls this release `deployed`
	// because it has applied its manifests; the cluster hasn't finished
	// acting on them, which is the whole reason 18a carries the rollout
	// arrow in the glyph column.
	c.Seed(kube.KindSecret,
		rev("grafana", "monitoring", "grafana", "7.3.0", "10.4.2", "deployed", "", 1, "adminUser: admin\npersistence:\n  enabled: true\n", 20*24*time.Hour),
	)
	// prometheus (monitoring): mid-upgrade — the strip's "◌ 1 pending-upgrade".
	c.Seed(kube.KindSecret,
		rev("prometheus", "monitoring", "kube-prometheus-stack", "58.2.1", "0.73.0", "pending-upgrade", "", 2, "grafana:\n  enabled: false\nalertmanager:\n  enabled: true\n", 3*time.Minute),
	)
	// broken-app (default): the strip's "✕ 1 failed" — STATUS carries the
	// reason verbatim per §18a ("failed · hook timeout").
	c.Seed(kube.KindSecret,
		rev("broken-app", "default", "mychart", "1.0.0", "2.1.0", "failed", "hook timeout", 2, "replicaCount: 2\n", 40*time.Minute),
	)
}

// demoReleaseWorkloads is the workload each demo release's chart renders,
// as a `kind/name` pair. Real charts render a full manifest of Services,
// ConfigMaps and RBAC alongside it; only the workloads matter here, because
// they are the only part 18a's rollout signal reads
// (kube.HelmReleaseWorkloads).
//
// A release absent from this map renders a manifest with no workload at all,
// which is honest for the demo's own fixtures: broken-app has nothing behind
// it, and postgresql/redis name no workload the demo cluster actually has.
// An unresolvable name would simply never match a cache, so it would be a
// silently dead fixture rather than a wrong one — but a fixture that names
// something real is the one that proves the join works.
var demoReleaseWorkloads = map[string]string{
	"grafana":    "Deployment/grafana",
	"prometheus": "StatefulSet/prometheus-k8s",
}

// demoReleaseManifest renders the slice of a real rendered manifest that
// HelmReleaseWorkloads reads: the source comment every helm template carries,
// plus the release's own workload document where it has one.
func demoReleaseManifest(chart, release string) string {
	manifest := "---\n# Source: " + chart + "/templates/deployment.yaml\n"
	workload, ok := demoReleaseWorkloads[release]
	if !ok {
		return manifest
	}
	kind, name, _ := strings.Cut(workload, "/")
	return manifest + "apiVersion: apps/v1\nkind: " + kind + "\nmetadata:\n  name: " + name + "\n"
}

// DemoChartIndex is the local-Helm-repo-cache side of the demo: the chart
// versions --demo pretends the user's `helm repo update` last fetched.
//
// It is a fixture rather than the machine's real ~/.cache/helm for the same
// reason every other demo fixture is one — the demo has to render the same
// on any laptop, and on most of them a real repo cache has never heard of
// these charts, so every LATEST cell would read "–" and the feature would be
// invisible.
//
// Deliberately mixed against demoHelmReleaseFixtures' deployed versions:
// postgresql (12.1.9) and grafana (7.3.0) are behind, redis (18.1.5) is
// current, and mychart — the failed release — is in no repo at all, so 18a
// shows an outdated row, a current row and an unknown row at once.
func DemoChartIndex() *helmrepo.Cache {
	return helmrepo.NewStaticCache("bitnami", map[string]string{
		"postgresql":            "12.2.1",
		"grafana":               "8.5.2",
		"redis":                 "18.1.5",
		"kube-prometheus-stack": "58.2.1",
	})
}

// demoFluxFixtures seeds §30a's Flux kinds: the three CRDs a real cluster
// serves for kustomize/source/helm, plus one instance per branch of §30a's
// status precedence so the list, its strip and its sub-lines are all
// exercised in --demo.
//
// Printer columns match what real Flux CRDs declare, measured across all 11
// on a live cluster: Ready, Status and Age everywhere, plus URL on the
// source kinds. §30a's own list ignores them (it is a curated kind), but
// 14b's CRDs list renders them, so getting them wrong would show there.
//
// The HelmRelease instances are seeded under kube.KindFluxHelmRelease, not
// "HelmRelease" — fake.Cluster keys its object map by registry kind while
// the real cluster resolves by API group, and this is the one place that
// difference is visible. Seeding the bare Kind would put Flux's releases
// into §18a's Helm-3 list, which is the exact bug §30a exists to prevent.
func demoFluxFixtures(c *Cluster, age func(time.Duration) metav1.Time) {
	crdAge := age(60 * 24 * time.Hour)
	readyCols := []kube.PrinterColumn{
		{Name: "Ready", Type: "string", JSONPath: `.status.conditions[?(@.type=="Ready")].status`},
		{Name: "Status", Type: "string", JSONPath: `.status.conditions[?(@.type=="Ready")].message`},
	}
	sourceCols := append([]kube.PrinterColumn{
		{Name: "URL", Type: "string", JSONPath: ".spec.url"},
	}, readyCols...)

	c.Seed(kube.KindCustomResourceDefinition,
		demoCRD("kustomizations."+kube.FluxGroupKustomize, kube.FluxGroupKustomize,
			"Kustomization", "kustomizations", "Namespaced", "v1", true, readyCols, crdAge),
		demoCRD("helmreleases."+kube.FluxGroupHelm, kube.FluxGroupHelm,
			"HelmRelease", "helmreleases", "Namespaced", "v2", true, readyCols, crdAge),
		demoCRD("gitrepositories."+kube.FluxGroupSource, kube.FluxGroupSource,
			"GitRepository", "gitrepositories", "Namespaced", "v1", true, sourceCols, crdAge),
		demoCRD("helmrepositories."+kube.FluxGroupSource, kube.FluxGroupSource,
			"HelmRepository", "helmrepositories", "Namespaced", "v1", true, sourceCols, crdAge),
	)
	c.SeedDiscovered(demoDiscoveredKind("Kustomization", "kustomizations",
		kube.FluxGroupKustomize, "v1", "kustomizations."+kube.FluxGroupKustomize, false, readyCols))
	c.SeedDiscovered(demoDiscoveredKind("HelmRelease", "helmreleases",
		kube.FluxGroupHelm, "v2", "helmreleases."+kube.FluxGroupHelm, false, readyCols))
	c.SeedDiscovered(demoDiscoveredKind("GitRepository", "gitrepositories",
		kube.FluxGroupSource, "v1", "gitrepositories."+kube.FluxGroupSource, false, sourceCols))
	c.SeedDiscovered(demoDiscoveredKind("HelmRepository", "helmrepositories",
		kube.FluxGroupSource, "v1", "helmrepositories."+kube.FluxGroupSource, false, sourceCols))

	const (
		ns      = "flux-system"
		gitRepo = "flux-system"
		headRev = "main@sha1:8f3c2a1b4d5e6f708192a3b4c5d6e7f809102030"
		oldRev  = "main@sha1:2b91f04c5d6e7f8091a2b3c4d5e6f70819203040"
	)

	c.Seed(kube.ResourceKind("GitRepository"),
		demoFluxSource("GitRepository", kube.FluxGroupSource+"/v1", gitRepo, ns,
			"https://github.com/example/nebula-config", headRev, age(148*24*time.Hour), age(3*time.Minute)),
	)

	// The two chart repositories the HelmReleases below actually name. §30b
	// nests each reconciler under the source it reconciles *from*, so
	// without these the Helm half of the tree would render as two orphaned
	// chains — and a HelmRepository's artifact revision is a bare index
	// digest, the case §30b's REVISION cell renders as the word "index".
	c.Seed(kube.ResourceKind("HelmRepository"),
		demoFluxSource("HelmRepository", kube.FluxGroupSource+"/v1", "bitnami", ns,
			"https://charts.bitnami.com/bitnami",
			"sha256:d83a8a3354d98907a55beba524407a0b94d319623d0370a65fcd390e68db9852",
			age(148*24*time.Hour), age(14*time.Minute)),
		demoFluxSource("HelmRepository", kube.FluxGroupSource+"/v1", "podinfo", ns,
			"https://stefanprodan.github.io/podinfo",
			"sha256:9d1e5f0a2c3b4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f70819203040506",
			age(120*24*time.Hour), age(52*time.Minute)),
	)

	c.Seed(kube.ResourceKind("Kustomization"),
		// ✕ failed — the design's own example, health check verbatim. Its
		// inventory names the workload the health check is waiting on, so
		// §31a's drill-through has something real to resolve.
		demoKustomizationWithInventory("nebula-workers", ns, gitRepo, "./clusters/stage/workers", headRev, false,
			[]string{
				ns + "_nebula-worker_apps_Deployment",
				ns + "_nebula-worker-config__ConfigMap",
				ns + "_nebula-worker__Service",
			},
			age(148*24*time.Hour), age(4*time.Minute), fluxCond("Ready", false, "HealthCheckFailed",
				"health check failed after 2m0s: Deployment/nebula-stage/nebula-worker status: 'InProgress'", age(4*time.Minute))),
		// ‖ suspended, carrying a *stale* Ready=True — the fixture that
		// proves suspension outranks a frozen condition.
		demoKustomization("nebula-infra", ns, gitRepo, "./clusters/stage/infra", oldRev, true,
			age(148*24*time.Hour), age(6*24*time.Hour), fluxCond("Ready", true, "ReconciliationSucceeded",
				"Applied revision: "+oldRev, age(6*24*time.Hour))),
		// ◌ reconciling — Ready=False *and* Reconciling=True, which the
		// generic CRD read renders as a red failure.
		//
		// Deliberately minute-scale, not seconds: Flux writes
		// lastTransitionTime as RFC3339, which truncates to the second, so a
		// sub-minute age renders "20s"/"21s" depending on where in the
		// current second a render lands — a coin flip for any golden that
		// includes this row.
		demoKustomization("nebula-apps", ns, gitRepo, "./clusters/stage/apps", headRev, false,
			age(148*24*time.Hour), age(90*time.Second),
			fluxCond("Ready", false, "Progressing", "building manifests", age(90*time.Second)),
			fluxCond("Reconciling", true, "ProgressingWithRetry", "building manifests", age(90*time.Second))),
		// ● ready
		demoKustomization("flux-system", ns, gitRepo, "./flux", headRev, false,
			age(148*24*time.Hour), age(time.Minute), fluxCond("Ready", true, "ReconciliationSucceeded",
				"Applied revision: "+headRev, age(time.Minute))),
		demoKustomization("observability", ns, gitRepo, "./clusters/stage/observability", headRev, false,
			age(120*24*time.Hour), age(2*time.Minute), fluxCond("Ready", true, "ReconciliationSucceeded",
				"Applied revision: "+headRev, age(2*time.Minute))),
	)

	c.Seed(kube.KindFluxHelmRelease,
		demoFluxHelmRelease("podinfo", ns, "podinfo", "6.5.4", "HelmRepository", "podinfo",
			age(90*24*time.Hour), age(31*time.Minute), fluxCond("Ready", true, "InstallSucceeded",
				"Helm install succeeded for release podinfo/podinfo.v1 with chart podinfo@6.5.4", age(31*time.Minute))),
		// ✕ stalled — terminal, distinct from a retrying failure.
		demoFluxHelmRelease("nebula-redis", ns, "redis", "19.6.1", "HelmRepository", "bitnami",
			age(60*24*time.Hour), age(12*time.Minute),
			fluxCond("Ready", false, "InstallFailed", "Helm install failed: timed out waiting for the condition", age(12*time.Minute)),
			fluxCond("Stalled", true, "RetriesExhausted", "Failed to install after 3 attempts", age(12*time.Minute))),
	)

	// The workload nebula-workers manages: 3 of 4 replicas ready, which is
	// what its health check is failing on and what §31a's failure card
	// resolves the reconcile failure to.
	workerReplicas := int32(4)
	workerLabels := map[string]string{"app": "nebula-worker"}
	c.Seed(kube.KindDeployment, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "nebula-worker", Namespace: ns, CreationTimestamp: age(148 * 24 * time.Hour)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &workerReplicas,
			Selector: &metav1.LabelSelector{MatchLabels: workerLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: workerLabels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "worker", Image: "nebula-worker:3.5.0"}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{Replicas: 4, ReadyReplicas: 3, UpdatedReplicas: 4},
	})
	// Its four pods — the fourth is the one replica the health check is
	// waiting on, OOMKilled into CrashLoopBackOff, so §31a's drill-through
	// follows the Deployment to a real reason ("exit 137, OOMKilled").
	for _, suffix := range []string{"7c9d4-2xq8p", "7c9d4-9fk2m", "7c9d4-tw4ld"} {
		pod := demoPod("nebula-worker-"+suffix, ns, age(4*time.Minute), corev1.PodRunning, corev1.PodQOSBurstable, "node-a", true, 0, nil)
		pod.Labels = workerLabels
		c.Seed(kube.KindPod, pod)
	}
	oomPod := demoCrashLoopPod("nebula-worker-7c9d4-h7zrn", ns, age(4*time.Minute), "node-a")
	oomPod.Labels = workerLabels
	oomPod.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 137
	oomPod.Status.ContainerStatuses[0].LastTerminationState.Terminated.Reason = "OOMKilled"
	c.Seed(kube.KindPod, oomPod)
	c.Seed(kube.KindConfigMap, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "nebula-worker-config", Namespace: ns, CreationTimestamp: age(148 * 24 * time.Hour)},
		Data:       map[string]string{"LOG_LEVEL": "info"},
	})
	c.Seed(kube.KindService, demoService("nebula-worker", ns, map[string]string{"app": "nebula-worker"}, age(148*24*time.Hour)))

	// §32a: reconcile events carrying the revision annotation, plus the
	// source-controller message the commit subject is parsed out of.
	c.Seed(kube.KindEvent,
		demoFluxRevisionEvent("flux-system-rev-1", ns, "Kustomization", "flux-system",
			"kustomize.toolkit.fluxcd.io/revision", headRev,
			"Reconciliation finished in 1.4s, next run in 10m0s", age(time.Minute)),
		demoFluxRevisionEvent("nebula-workers-rev-1", ns, "Kustomization", "nebula-workers",
			"kustomize.toolkit.fluxcd.io/revision", headRev,
			"HelmRelease/nebula-stage/nebula-worker configured", age(4*time.Minute)),
		demoEvent("git-artifact-1", ns, "GitRepository", gitRepo, "Normal", "GitOperationSucceeded",
			"stored artifact for commit 'fix: raise worker memory limit (#412)'", 1, age(3*time.Minute)),
	)
}

// demoKustomizationWithInventory is demoKustomization plus a
// status.inventory — the entry-id shape Flux really writes,
// "<namespace>_<name>_<group>_<Kind>", with an empty group for core kinds.
func demoKustomizationWithInventory(name, ns, source, path, applied string, suspend bool, inventory []string, created, reconciled metav1.Time, conds ...map[string]any) *unstructured.Unstructured {
	u := demoKustomization(name, ns, source, path, applied, suspend, created, reconciled, conds...)
	entries := make([]any, len(inventory))
	for i, id := range inventory {
		entries[i] = map[string]any{"id": id, "v": "v1"}
	}
	status, _, _ := unstructured.NestedMap(u.Object, "status")
	status["inventory"] = map[string]any{"entries": entries}
	u.Object["status"] = status
	return u
}

// fluxCond builds one Flux status condition. lastTransitionTime is set
// because §30a's RECONCILED column reads it — a condition without one
// renders "–", which is right on a real object that has none and wrong as a
// fixture default.
func fluxCond(typ string, ok bool, reason, message string, transition metav1.Time) map[string]any {
	status := "True"
	if !ok {
		status = "False"
	}
	return map[string]any{
		"type": typ, "status": status, "reason": reason, "message": message,
		"lastTransitionTime": transition.UTC().Format(time.RFC3339),
	}
}

// fluxStatus assembles a status block from conditions plus extra fields.
func fluxStatus(conds []map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range extra {
		out[k] = v
	}
	list := make([]any, len(conds))
	for i, c := range conds {
		list[i] = c
	}
	out["conditions"] = list
	return out
}

func demoFluxObject(apiVersion, kind, name, ns string, created metav1.Time, spec, status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name": name, "namespace": ns,
			"creationTimestamp": created.UTC().Format(time.RFC3339),
		},
		"spec":   spec,
		"status": status,
	}}
}

// demoKustomization builds one kustomize.toolkit.fluxcd.io/v1 Kustomization.
// spec.suspend is only set when true, matching the real shape where an
// unsuspended object omits the field rather than setting it false.
func demoKustomization(name, ns, source, path, applied string, suspend bool, created, _ metav1.Time, conds ...map[string]any) *unstructured.Unstructured {
	spec := map[string]any{
		"interval":  "5m",
		"path":      path,
		"prune":     true,
		"sourceRef": map[string]any{"kind": "GitRepository", "name": source},
	}
	if suspend {
		spec["suspend"] = true
	}
	return demoFluxObject(kube.FluxGroupKustomize+"/v1", "Kustomization", name, ns, created,
		spec, fluxStatus(conds, map[string]any{"lastAppliedRevision": applied, "lastAttemptedRevision": applied}))
}

// demoFluxHelmRelease builds one helm.toolkit.fluxcd.io/v2 HelmRelease.
// lastAppliedRevision is deliberately absent — a real HelmRelease leaves it
// null and reports its version through status.history, which is exactly the
// fallback §30a's REVISION column has to take.
func demoFluxHelmRelease(name, ns, chart, version, sourceKind, sourceName string, created, _ metav1.Time, conds ...map[string]any) *unstructured.Unstructured {
	spec := map[string]any{
		"interval": "15m",
		"chart": map[string]any{"spec": map[string]any{
			"chart": chart, "version": version,
			"sourceRef": map[string]any{"kind": sourceKind, "name": sourceName, "namespace": "flux-system"},
		}},
	}
	status := fluxStatus(conds, map[string]any{
		"lastAttemptedRevision": version,
		"history": []any{map[string]any{
			"chartName": chart, "chartVersion": version, "version": int64(1),
		}},
	})
	return demoFluxObject(kube.FluxGroupHelm+"/v2", "HelmRelease", name, ns, created, spec, status)
}

// demoFluxSource builds one source.toolkit.fluxcd.io GitRepository, whose
// revision lives in status.artifact rather than a lastAppliedRevision.
func demoFluxSource(kind, apiVersion, name, ns, url, revision string, created, fetched metav1.Time) *unstructured.Unstructured {
	return demoFluxObject(apiVersion, kind, name, ns, created,
		map[string]any{"interval": "1m", "url": url, "ref": map[string]any{"branch": "main"}},
		fluxStatus(
			[]map[string]any{fluxCond("Ready", true, "Succeeded", "stored artifact for revision '"+revision+"'", fetched)},
			map[string]any{"artifact": map[string]any{
				"revision":       revision,
				"lastUpdateTime": fetched.UTC().Format(time.RFC3339),
			}},
		))
}

// demoFluxRevisionEvent builds a reconcile Event carrying §32a's revision
// annotation — the exact shape a real Flux controller emits, where the
// revision is data on the event rather than prose inside its message.
func demoFluxRevisionEvent(name, ns, kind, objName, annKey, revision, message string, last metav1.Time) *corev1.Event {
	ev := demoEvent(name, ns, kind, objName, "Normal", "ReconciliationSucceeded", message, 1, last)
	ev.Annotations = map[string]string{annKey: revision}
	return ev
}
