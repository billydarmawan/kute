package kube

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func newTestCluster(objs ...runtime.Object) (*Cluster, *fake.Clientset) {
	cs := fake.NewSimpleClientset(objs...)
	return &Cluster{clientset: cs}, cs
}

func TestRolloutRestartPatchesTemplateAnnotation(t *testing.T) {
	t.Parallel()
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}}
	c, cs := newTestCluster(deploy)

	if err := c.RolloutRestart(t.Context(), KindDeployment, "default", "api"); err != nil {
		t.Fatalf("RolloutRestart: %v", err)
	}
	got, err := cs.AppsV1().Deployments("default").Get(t.Context(), "api", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := got.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"]; !ok {
		t.Fatalf("expected restartedAt annotation, got %+v", got.Spec.Template.Annotations)
	}
}

// TestRolloutRestartCoversStatefulSetAndDaemonSet pins 27a's own reason for
// generalizing RolloutRestart beyond Deployment: ctrl-r has to be able to
// restart a ConfigMap's consumer regardless of which of the three pod-
// template kinds it is.
func TestRolloutRestartCoversStatefulSetAndDaemonSet(t *testing.T) {
	t.Parallel()
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "default"}}
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default"}}
	c, cs := newTestCluster(sts, ds)
	ctx := t.Context()

	if err := c.RolloutRestart(ctx, KindStatefulSet, "default", "worker"); err != nil {
		t.Fatalf("RolloutRestart statefulset: %v", err)
	}
	gotSts, err := cs.AppsV1().StatefulSets("default").Get(ctx, "worker", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get statefulset: %v", err)
	}
	if _, ok := gotSts.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"]; !ok {
		t.Fatalf("expected restartedAt annotation on statefulset, got %+v", gotSts.Spec.Template.Annotations)
	}

	if err := c.RolloutRestart(ctx, KindDaemonSet, "default", "agent"); err != nil {
		t.Fatalf("RolloutRestart daemonset: %v", err)
	}
	gotDs, err := cs.AppsV1().DaemonSets("default").Get(ctx, "agent", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get daemonset: %v", err)
	}
	if _, ok := gotDs.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"]; !ok {
		t.Fatalf("expected restartedAt annotation on daemonset, got %+v", gotDs.Spec.Template.Annotations)
	}
}

func TestRolloutRestartRejectsEmptyName(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster()
	if err := c.RolloutRestart(t.Context(), KindDeployment, "default", ""); err == nil {
		t.Fatalf("expected an error for an empty name")
	}
}

// TestRetryJobClonesSpecIntoNewJob pins RetryJob's non-destructive contract
// (confirmed with the user over the delete+recreate alternative): the
// source Job is never touched, and the clone has the fields the API server
// must regenerate stripped so it doesn't collide with the still-existing
// source Job's own selector.
func TestRetryJobClonesSpecIntoNewJob(t *testing.T) {
	t.Parallel()
	suspend := true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "batch-1", Namespace: "default",
			Labels:          map[string]string{"app": "batch"},
			Annotations:     map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "CronJob", Name: "nightly"}},
		},
		Spec: batchv1.JobSpec{
			Suspend: &suspend,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"controller-uid": "abc-123"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "batch", "controller-uid": "abc-123",
						"batch.kubernetes.io/controller-uid": "abc-123",
						"job-name":                           "batch-1",
						"batch.kubernetes.io/job-name":       "batch-1",
					},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1.0"}}},
			},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	}
	c, cs := newTestCluster(job)

	stagedAt := time.Date(2026, 3, 4, 2, 0, 0, 0, time.UTC)
	if err := c.RetryJob(t.Context(), "default", "batch-1", "batch-1-retry-123", "operator@example.com", stagedAt); err != nil {
		t.Fatalf("RetryJob: %v", err)
	}

	// The source Job is untouched.
	src, err := cs.BatchV1().Jobs("default").Get(t.Context(), "batch-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get source: %v", err)
	}
	if src.Status.Succeeded != 1 {
		t.Errorf("source Job's Status was touched: %+v", src.Status)
	}

	clone, err := cs.BatchV1().Jobs("default").Get(t.Context(), "batch-1-retry-123", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get clone: %v", err)
	}
	if len(clone.OwnerReferences) != 0 {
		t.Errorf("expected the clone detached (no OwnerReferences), got %+v", clone.OwnerReferences)
	}
	if clone.Spec.Selector != nil {
		t.Errorf("expected Selector cleared for the API server to regenerate, got %+v", clone.Spec.Selector)
	}
	if clone.Spec.Suspend == nil || *clone.Spec.Suspend {
		t.Errorf("expected the clone to start unsuspended regardless of the source's state, got %+v", clone.Spec.Suspend)
	}
	for _, key := range []string{"controller-uid", "batch.kubernetes.io/controller-uid", "job-name", "batch.kubernetes.io/job-name"} {
		if _, ok := clone.Spec.Template.Labels[key]; ok {
			t.Errorf("expected %q stripped from the cloned template labels, got %+v", key, clone.Spec.Template.Labels)
		}
	}
	if clone.Spec.Template.Labels["app"] != "batch" {
		t.Errorf("expected non-generated template labels preserved, got %+v", clone.Spec.Template.Labels)
	}
	if clone.Labels["app"] != "batch" {
		t.Errorf("expected the clone's own Labels copied, got %+v", clone.Labels)
	}
	if _, ok := clone.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		t.Errorf("expected the stale last-applied-configuration annotation dropped, got %+v", clone.Annotations)
	}
	if clone.Annotations[AnnotationTriggeredBy] != "operator@example.com" {
		t.Errorf("AnnotationTriggeredBy = %q, want %q", clone.Annotations[AnnotationTriggeredBy], "operator@example.com")
	}
	if clone.Annotations[AnnotationTriggeredAt] != "2026-03-04T02:00:00Z" {
		t.Errorf("AnnotationTriggeredAt = %q, want %q", clone.Annotations[AnnotationTriggeredAt], "2026-03-04T02:00:00Z")
	}
}

// TestRetryJobStripsCronJobAssociation guards §37c: a rerun is always
// standalone, even when the source Job was itself cronjob-owned (via Kute's
// own AnnotationCronJobUID/AnnotationCronJobName, the only association
// mechanism a Job clone with no OwnerReferences could otherwise carry
// forward).
func TestRetryJobStripsCronJobAssociation(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "report-nightly-1", Namespace: "default",
			Annotations: map[string]string{
				AnnotationCronJobUID:  "cj-uid-1",
				AnnotationCronJobName: "report-nightly",
			},
		},
	}
	c, cs := newTestCluster(job)
	if err := c.RetryJob(t.Context(), "default", "report-nightly-1", "report-nightly-1-rerun-1", "you", time.Now()); err != nil {
		t.Fatalf("RetryJob: %v", err)
	}
	clone, err := cs.BatchV1().Jobs("default").Get(t.Context(), "report-nightly-1-rerun-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get clone: %v", err)
	}
	if _, ok := clone.Annotations[AnnotationCronJobUID]; ok {
		t.Errorf("expected AnnotationCronJobUID stripped from the clone, got %+v", clone.Annotations)
	}
	if _, ok := clone.Annotations[AnnotationCronJobName]; ok {
		t.Errorf("expected AnnotationCronJobName stripped from the clone, got %+v", clone.Annotations)
	}
}

// TestRetryJobStripsHelmHookIdentity guards §37c: a rerun of a Helm hook
// Job reads "manual · <creator>", not the hook it was cloned from.
func TestRetryJobStripsHelmHookIdentity(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "migrate", Namespace: "default",
			Annotations: map[string]string{
				HelmHookAnnotation:               "pre-upgrade",
				"helm.sh/hook-weight":            "-5",
				"helm.sh/hook-delete-policy":     "before-hook-creation",
				HelmReleaseNameAnnotation:        "shop",
				"meta.helm.sh/release-namespace": "default",
				"team":                           "payments",
			},
		},
	}
	c, cs := newTestCluster(job)
	if err := c.RetryJob(t.Context(), "default", "migrate", "migrate-rerun-1", "you", time.Now()); err != nil {
		t.Fatalf("RetryJob: %v", err)
	}
	clone, err := cs.BatchV1().Jobs("default").Get(t.Context(), "migrate-rerun-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get clone: %v", err)
	}
	for key := range clone.Annotations {
		if strings.HasPrefix(key, "helm.sh/hook") || strings.HasPrefix(key, "meta.helm.sh/") {
			t.Errorf("expected %s stripped from the clone, got %+v", key, clone.Annotations)
		}
	}
	if clone.Annotations["team"] != "payments" || clone.Annotations[AnnotationTriggeredBy] != "you" {
		t.Errorf("clone annotations = %+v, want unrelated ones kept and the manual tag set", clone.Annotations)
	}
}

func TestRetryJobRejectsEmptyNames(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "batch-1", Namespace: "default"}})
	if err := c.RetryJob(t.Context(), "default", "", "batch-1-retry-123", "you", time.Now()); err == nil {
		t.Fatalf("expected an error for an empty source name")
	}
	if err := c.RetryJob(t.Context(), "default", "batch-1", "", "you", time.Now()); err == nil {
		t.Fatalf("expected an error for an empty target name")
	}
}

// TestReplaceJobDeletesThenRecreatesUnderSameName pins §37c's "replace"
// choice: unlike RetryJob's clone, the original object is actually gone —
// same UID/creationTimestamp cannot survive a delete+create — but the
// replacement keeps the source's OwnerReferences (a replace stands in for
// the exact same object, never detaching it the way a rerun clone always
// does).
func TestReplaceJobDeletesThenRecreatesUnderSameName(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "migrate-schema-v42", Namespace: "default",
			Labels:          map[string]string{"app": "migrate"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "CronJob", Name: "nightly"}},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1.0"}}},
			},
		},
		Status: batchv1.JobStatus{Failed: 1},
	}
	c, cs := newTestCluster(job)
	if err := c.ReplaceJob(t.Context(), "default", "migrate-schema-v42"); err != nil {
		t.Fatalf("ReplaceJob: %v", err)
	}
	replacement, err := cs.BatchV1().Jobs("default").Get(t.Context(), "migrate-schema-v42", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get replacement: %v", err)
	}
	if replacement.Status.Failed != 0 {
		t.Errorf("expected a fresh Status on the replacement, got %+v", replacement.Status)
	}
	if len(replacement.OwnerReferences) != 1 || replacement.OwnerReferences[0].Name != "nightly" {
		t.Errorf("expected OwnerReferences preserved on the replacement, got %+v", replacement.OwnerReferences)
	}
}

func TestReplaceJobRejectsEmptyName(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "batch-1", Namespace: "default"}})
	if err := c.ReplaceJob(t.Context(), "default", ""); err == nil {
		t.Fatalf("expected an error for an empty name")
	}
}

// TestSetJobSuspendPatchesSpecSuspend mirrors
// TestSetFluxSuspendPatchesSpecSuspend, but through the typed clientset a
// Job (unlike a Flux CRD) already has.
func TestSetJobSuspendPatchesSpecSuspend(t *testing.T) {
	t.Parallel()
	for _, suspend := range []bool{true, false} {
		c, cs := newTestCluster(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "batch-1", Namespace: "default"}})
		if err := c.SetJobSuspend(t.Context(), "default", "batch-1", suspend); err != nil {
			t.Fatalf("SetJobSuspend(%t): %v", suspend, err)
		}
		got, err := cs.BatchV1().Jobs("default").Get(t.Context(), "batch-1", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Spec.Suspend == nil || *got.Spec.Suspend != suspend {
			t.Errorf("Spec.Suspend = %v, want %t", got.Spec.Suspend, suspend)
		}
	}
}

func TestSetJobSuspendRejectsEmptyName(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster()
	if err := c.SetJobSuspend(t.Context(), "default", "", true); err == nil {
		t.Fatalf("expected an error for an empty name")
	}
}

// TestTriggerCronJobClonesJobTemplateIntoNewJob pins TriggerCronJob's
// non-destructive contract, mirroring TestRetryJobClonesSpecIntoNewJob: the
// source CronJob is never touched, the new Job carries the template's own
// metadata/spec, and it's stamped with the same
// "cronjob.kubernetes.io/instantiate: manual" annotation the real `kubectl
// create job --from=cronjob` recipe uses, detached from any parent so it's
// never swept by the CronJob's own history-limit GC. Also pins Phase 2 task
// 1/4: the Kute source/creator/time annotations (Plan §4.2/§36b, Phase 2
// test 4).
func TestTriggerCronJobClonesJobTemplateIntoNewJob(t *testing.T) {
	t.Parallel()
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", UID: types.UID("cj-uid-1")},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 2 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app": "batch"},
					Annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"},
				},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1.0"}}},
					},
				},
			},
		},
	}
	c, cs := newTestCluster(cj)

	triggeredAt := time.Date(2026, 3, 4, 2, 0, 0, 0, time.UTC)
	if err := c.TriggerCronJob(t.Context(), "default", "nightly", "nightly-manual-0200", "operator@example.com", triggeredAt); err != nil {
		t.Fatalf("TriggerCronJob: %v", err)
	}

	// The source CronJob is untouched.
	src, err := cs.BatchV1().CronJobs("default").Get(t.Context(), "nightly", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get source: %v", err)
	}
	if src.Status.LastScheduleTime != nil {
		t.Errorf("source CronJob's Status was touched: %+v", src.Status)
	}

	job, err := cs.BatchV1().Jobs("default").Get(t.Context(), "nightly-manual-0200", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get triggered job: %v", err)
	}
	if len(job.OwnerReferences) != 0 {
		t.Errorf("expected the triggered job detached (no OwnerReferences), got %+v", job.OwnerReferences)
	}
	if job.Annotations["cronjob.kubernetes.io/instantiate"] != "manual" {
		t.Errorf("expected the manual-instantiate annotation, got %+v", job.Annotations)
	}
	if job.Annotations[AnnotationCronJobName] != "nightly" {
		t.Errorf("AnnotationCronJobName = %q, want %q", job.Annotations[AnnotationCronJobName], "nightly")
	}
	if job.Annotations[AnnotationCronJobUID] != "cj-uid-1" {
		t.Errorf("AnnotationCronJobUID = %q, want %q", job.Annotations[AnnotationCronJobUID], "cj-uid-1")
	}
	if job.Annotations[AnnotationTriggeredBy] != "operator@example.com" {
		t.Errorf("AnnotationTriggeredBy = %q, want %q", job.Annotations[AnnotationTriggeredBy], "operator@example.com")
	}
	if job.Annotations[AnnotationTriggeredAt] != "2026-03-04T02:00:00Z" {
		t.Errorf("AnnotationTriggeredAt = %q, want %q", job.Annotations[AnnotationTriggeredAt], "2026-03-04T02:00:00Z")
	}
	if _, ok := job.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		t.Errorf("expected the stale last-applied-configuration annotation dropped, got %+v", job.Annotations)
	}
	if job.Labels["app"] != "batch" {
		t.Errorf("expected the template's own Labels copied, got %+v", job.Labels)
	}
	if len(job.Spec.Template.Spec.Containers) != 1 || job.Spec.Template.Spec.Containers[0].Image != "app:1.0" {
		t.Errorf("expected the template's own container spec copied, got %+v", job.Spec.Template.Spec)
	}
}

func TestTriggerCronJobRejectsEmptyNames(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster(&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"}})
	if err := c.TriggerCronJob(t.Context(), "default", "", "nightly-manual-0200", "op", time.Now()); err == nil {
		t.Fatalf("expected an error for an empty source name")
	}
	if err := c.TriggerCronJob(t.Context(), "default", "nightly", "", "op", time.Now()); err == nil {
		t.Fatalf("expected an error for an empty target name")
	}
}

// TestTriggerCronJobWrapsAlreadyExists pins Phase 2 task 14: a real
// AlreadyExists race on the staged manual name (e.g. a leftover Job from a
// prior run, or two operators racing the same CronJob) surfaces as
// ErrManualJobNameConflict so a caller can distinguish it via errors.Is and
// offer restaging, rather than treating it like any other failure.
func TestTriggerCronJobWrapsAlreadyExists(t *testing.T) {
	t.Parallel()
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"}}
	existing := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "nightly-manual-0200", Namespace: "default"}}
	c, _ := newTestCluster(cj, existing)

	err := c.TriggerCronJob(t.Context(), "default", "nightly", "nightly-manual-0200", "op", time.Now())
	if err == nil {
		t.Fatal("expected an error for a name already taken")
	}
	if !errors.Is(err, ErrManualJobNameConflict) {
		t.Fatalf("expected errors.Is(err, ErrManualJobNameConflict), got %v", err)
	}
}

// TestSetCronJobSuspendPatchesSpecSuspend mirrors
// TestSetJobSuspendPatchesSpecSuspend, on the CronJob client instead of
// Job, and pins Phase 2 task 5: suspend stamps the Kute timestamp/
// generation annotations atomically with spec.suspend; resume clears both
// (Phase 2 tests 6-7).
func TestSetCronJobSuspendPatchesSpecSuspend(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 5, 1, 9, 30, 0, 0, time.UTC)

	// Suspend: spec + both annotations land atomically, generation stamped
	// as currentGeneration+1.
	c, cs := newTestCluster(&batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", ResourceVersion: "10", Generation: 3},
	})
	if err := c.SetCronJobSuspend(t.Context(), "default", "nightly", true, "10", 3, at); err != nil {
		t.Fatalf("SetCronJobSuspend(true): %v", err)
	}
	got, err := cs.BatchV1().CronJobs("default").Get(t.Context(), "nightly", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.Suspend == nil || !*got.Spec.Suspend {
		t.Errorf("Spec.Suspend = %v, want true", got.Spec.Suspend)
	}
	if got.Annotations[AnnotationSuspendedAt] != "2026-05-01T09:30:00Z" {
		t.Errorf("AnnotationSuspendedAt = %q, want %q", got.Annotations[AnnotationSuspendedAt], "2026-05-01T09:30:00Z")
	}
	if got.Annotations[AnnotationSuspendedGeneration] != "4" {
		t.Errorf("AnnotationSuspendedGeneration = %q, want %q (currentGeneration+1)", got.Annotations[AnnotationSuspendedGeneration], "4")
	}

	// Resume: spec flips back, both annotations are cleared, active Jobs
	// (there are none seeded here, but the point is this patch never
	// touches KindJob) are untouched — see TestSetCronJobSuspendResumeLeavesActiveJobsUntouched.
	c2, cs2 := newTestCluster(&batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nightly", Namespace: "default", ResourceVersion: "20", Generation: 4,
			Annotations: map[string]string{
				AnnotationSuspendedAt:         "2026-05-01T09:30:00Z",
				AnnotationSuspendedGeneration: "4",
			},
		},
	})
	if err := c2.SetCronJobSuspend(t.Context(), "default", "nightly", false, "20", 4, time.Time{}); err != nil {
		t.Fatalf("SetCronJobSuspend(false): %v", err)
	}
	got2, err := cs2.BatchV1().CronJobs("default").Get(t.Context(), "nightly", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after resume: %v", err)
	}
	if got2.Spec.Suspend == nil || *got2.Spec.Suspend {
		t.Errorf("Spec.Suspend after resume = %v, want false", got2.Spec.Suspend)
	}
	if _, ok := got2.Annotations[AnnotationSuspendedAt]; ok {
		t.Errorf("expected AnnotationSuspendedAt cleared on resume, got %+v", got2.Annotations)
	}
	if _, ok := got2.Annotations[AnnotationSuspendedGeneration]; ok {
		t.Errorf("expected AnnotationSuspendedGeneration cleared on resume, got %+v", got2.Annotations)
	}
}

// TestSetCronJobSuspendResumeLeavesActiveJobsUntouched pins Phase 2 test 7's
// other half explicitly: resume patches only the CronJob, never anything
// under KindJob.
func TestSetCronJobSuspendResumeLeavesActiveJobsUntouched(t *testing.T) {
	t.Parallel()
	activeJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly-28112233", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	c, cs := newTestCluster(
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", ResourceVersion: "1", Generation: 1}},
		activeJob,
	)
	if err := c.SetCronJobSuspend(t.Context(), "default", "nightly", false, "1", 1, time.Time{}); err != nil {
		t.Fatalf("SetCronJobSuspend(false): %v", err)
	}
	got, err := cs.BatchV1().Jobs("default").Get(t.Context(), "nightly-28112233", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get active job: %v", err)
	}
	if got.Status.Active != 1 {
		t.Errorf("expected the active Job untouched, got Status.Active=%d", got.Status.Active)
	}
}

func TestSetCronJobSuspendRejectsEmptyName(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster()
	if err := c.SetCronJobSuspend(t.Context(), "default", "", true, "1", 1, time.Now()); err == nil {
		t.Fatalf("expected an error for an empty name")
	}
}

// TestSetCronJobSuspendRequiresResourceVersion pins the required-precondition
// contract: an empty resourceVersion is refused client-side rather than
// silently patching without one.
func TestSetCronJobSuspendRequiresResourceVersion(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster(&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"}})
	if err := c.SetCronJobSuspend(t.Context(), "default", "nightly", true, "", 1, time.Now()); err == nil {
		t.Fatalf("expected an error for a missing resourceVersion precondition")
	}
}

// TestSetCronJobSchedulePatchesSpecSchedule pins SetCronJobSchedule's merge
// patch against the real typed clientset, including task 10's returned
// CronJobScheduleResult reflecting the server's own accepted values.
func TestSetCronJobSchedulePatchesSpecSchedule(t *testing.T) {
	t.Parallel()
	c, cs := newTestCluster(&batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", ResourceVersion: "5"},
		Spec:       batchv1.CronJobSpec{Schedule: "0 2 * * *"},
	})
	result, err := c.SetCronJobSchedule(t.Context(), "default", "nightly", CronJobScheduleEdit{
		Schedule: "*/15 * * * *", ResourceVersion: "5",
	})
	if err != nil {
		t.Fatalf("SetCronJobSchedule: %v", err)
	}
	got, err := cs.BatchV1().CronJobs("default").Get(t.Context(), "nightly", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.Schedule != "*/15 * * * *" {
		t.Errorf("Spec.Schedule = %q, want %q", got.Spec.Schedule, "*/15 * * * *")
	}
	if result.Schedule != "*/15 * * * *" {
		t.Errorf("result.Schedule = %q, want %q", result.Schedule, "*/15 * * * *")
	}
	if result.ResourceVersion != got.ResourceVersion {
		t.Errorf("result.ResourceVersion = %q, want the patched object's own %q", result.ResourceVersion, got.ResourceVersion)
	}
}

// TestSetCronJobScheduleSetsAndClearsTimeZone pins task 8: timezone set and
// clear are distinct operations, and leaving TimeZone nil never touches an
// existing value.
func TestSetCronJobScheduleSetsAndClearsTimeZone(t *testing.T) {
	t.Parallel()

	c, cs := newTestCluster(&batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", ResourceVersion: "1"},
		Spec:       batchv1.CronJobSpec{Schedule: "0 2 * * *"},
	})
	tz := "America/New_York"
	result, err := c.SetCronJobSchedule(t.Context(), "default", "nightly", CronJobScheduleEdit{
		Schedule: "0 2 * * *", TimeZone: &tz, ResourceVersion: "1",
	})
	if err != nil {
		t.Fatalf("SetCronJobSchedule (set timezone): %v", err)
	}
	if result.TimeZone != tz {
		t.Errorf("result.TimeZone = %q, want %q", result.TimeZone, tz)
	}
	got, err := cs.BatchV1().CronJobs("default").Get(t.Context(), "nightly", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.TimeZone == nil || *got.Spec.TimeZone != tz {
		t.Errorf("Spec.TimeZone = %v, want %q", got.Spec.TimeZone, tz)
	}

	// A nil TimeZone (only the schedule changes) must never touch the
	// existing value.
	result2, err := c.SetCronJobSchedule(t.Context(), "default", "nightly", CronJobScheduleEdit{
		Schedule: "*/30 * * * *", ResourceVersion: got.ResourceVersion,
	})
	if err != nil {
		t.Fatalf("SetCronJobSchedule (schedule only): %v", err)
	}
	if result2.TimeZone != tz {
		t.Errorf("result2.TimeZone = %q, want the untouched %q", result2.TimeZone, tz)
	}
	got2, err := cs.BatchV1().CronJobs("default").Get(t.Context(), "nightly", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after schedule-only edit: %v", err)
	}
	if got2.Spec.TimeZone == nil || *got2.Spec.TimeZone != tz {
		t.Errorf("expected Spec.TimeZone left untouched at %q, got %v", tz, got2.Spec.TimeZone)
	}

	// An explicit clear (pointer to "") removes it — never serialized as a
	// stale timezone string.
	emptyTZ := ""
	result3, err := c.SetCronJobSchedule(t.Context(), "default", "nightly", CronJobScheduleEdit{
		Schedule: "*/30 * * * *", TimeZone: &emptyTZ, ResourceVersion: got2.ResourceVersion,
	})
	if err != nil {
		t.Fatalf("SetCronJobSchedule (clear timezone): %v", err)
	}
	if result3.TimeZone != "" {
		t.Errorf("result3.TimeZone = %q, want cleared \"\"", result3.TimeZone)
	}
	got3, err := cs.BatchV1().CronJobs("default").Get(t.Context(), "nightly", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after clear: %v", err)
	}
	if got3.Spec.TimeZone != nil {
		t.Errorf("expected Spec.TimeZone cleared, got %v", *got3.Spec.TimeZone)
	}
}

// TestCronJobSetScheduleCommandStringMirrorsPatchBody pins the will-run
// line's three-state timezone contract (schedule.go's own doc comment):
// nil omits spec.timeZone, a pointer to "" renders JSON null, a pointer to
// a name renders that string — matching SetCronJobSchedule's own
// json.Marshal exactly.
func TestCronJobSetScheduleCommandStringMirrorsPatchBody(t *testing.T) {
	t.Parallel()
	untouched := CronJobSetScheduleCommandString("default", "nightly", "*/15 * * * *", nil)
	if !strings.Contains(untouched, `"schedule":"*/15 * * * *"`) || strings.Contains(untouched, "timeZone") {
		t.Errorf("untouched command = %q, want schedule only, no timeZone key", untouched)
	}
	tz := "America/New_York"
	set := CronJobSetScheduleCommandString("default", "nightly", "0 2 * * *", &tz)
	if !strings.Contains(set, `"timeZone":"America/New_York"`) {
		t.Errorf("set command = %q, want timeZone %q", set, tz)
	}
	empty := ""
	cleared := CronJobSetScheduleCommandString("default", "nightly", "0 2 * * *", &empty)
	if !strings.Contains(cleared, `"timeZone":null`) {
		t.Errorf("cleared command = %q, want an explicit timeZone null", cleared)
	}
}

func TestSetCronJobScheduleRejectsEmptyName(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster()
	if _, err := c.SetCronJobSchedule(t.Context(), "default", "", CronJobScheduleEdit{Schedule: "*/15 * * * *", ResourceVersion: "1"}); err == nil {
		t.Fatalf("expected an error for an empty name")
	}
}

// TestSetCronJobScheduleRequiresResourceVersion mirrors the suspend
// counterpart: an unconditional schedule write is exactly the
// silent-overwrite behavior the precondition exists to prevent.
func TestSetCronJobScheduleRequiresResourceVersion(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster(&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"}})
	if _, err := c.SetCronJobSchedule(t.Context(), "default", "nightly", CronJobScheduleEdit{Schedule: "*/15 * * * *"}); err == nil {
		t.Fatalf("expected an error for a missing resourceVersion precondition")
	}
}

// TestSetCronJobSchedulePropagatesConflict pins Phase 2 test 8's other half:
// when the server rejects the patch as a resourceVersion conflict (a
// concurrent external edit), SetCronJobSchedule surfaces that error
// untouched rather than retrying or masking it — apierrors.IsConflict must
// still answer true on what this method returns. A PrependReactor stands in
// for the live API server's own precondition check, which the shared fake
// clientset (unlike a real apiserver) does not implement — see
// TestSetCronJobScheduleConflictsAgainstFakeCluster in fake_test.go for the
// same contract enforced end-to-end against kube/fake's own Cluster.
func TestSetCronJobSchedulePropagatesConflict(t *testing.T) {
	t.Parallel()
	c, cs := newTestCluster(&batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", ResourceVersion: "5"},
		Spec:       batchv1.CronJobSpec{Schedule: "0 2 * * *"},
	})
	cs.PrependReactor("patch", "cronjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Group: "batch", Resource: "cronjobs"}, "nightly",
			fmt.Errorf("the object has been modified"))
	})
	_, err := c.SetCronJobSchedule(t.Context(), "default", "nightly", CronJobScheduleEdit{
		Schedule: "*/15 * * * *", ResourceVersion: "1", // stale on purpose
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected apierrors.IsConflict(err), got %v", err)
	}
}

// TestRolloutUndoPatchesTemplateToTargetRevision covers 16b's 'R' rollback
// (docs/design README.md §16b): RolloutUndo finds the ReplicaSet carrying
// the target revision annotation and copies its pod template onto the
// Deployment's own spec.template.
func TestRolloutUndoPatchesTemplateToTargetRevision(t *testing.T) {
	t.Parallel()
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default", ResourceVersion: "1"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "api:2.0.0"}},
		}}},
	}
	rsOld := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-old", Namespace: "default",
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": "4"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api"}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "api:1.0.0"}},
		}}},
	}
	rsCurrent := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-current", Namespace: "default",
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": "5"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api"}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "api:2.0.0"}},
		}}},
	}
	c, cs := newTestCluster(deploy, rsOld, rsCurrent)

	if err := c.RolloutUndo(t.Context(), "default", "api", 4); err != nil {
		t.Fatalf("RolloutUndo: %v", err)
	}
	got, err := cs.AppsV1().Deployments("default").Get(t.Context(), "api", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Spec.Template.Spec.Containers) != 1 || got.Spec.Template.Spec.Containers[0].Image != "api:1.0.0" {
		t.Fatalf("expected the deployment's template to match revision 4's image, got %+v", got.Spec.Template.Spec.Containers)
	}
}

// TestRolloutUndoReplacesTemplateWholesale is the H1 regression: rolling
// back must restore revision 1's template exactly, as `kubectl rollout
// undo` does — an env var, a sidecar and a template annotation added in
// revision 2 must be gone (a strategic merge patch kept all three), the
// ReplicaSet's pod-template-hash must not leak into the Deployment, and
// the Deployment's annotations follow kubectl's copy/skip rule.
func TestRolloutUndoReplacesTemplateWholesale(t *testing.T) {
	t.Parallel()
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web", Namespace: "default", ResourceVersion: "42",
			Annotations: map[string]string{
				"deployment.kubernetes.io/revision": "2",
				corev1.LastAppliedConfigAnnotation:  "{}",
				"kubernetes.io/change-cause":        "bump to v2",
			},
		},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      map[string]string{"app": "web"},
				Annotations: map[string]string{"new": "yes"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "app", Image: "web:2", Env: []corev1.EnvVar{{Name: "OLD", Value: "1"}, {Name: "NEW", Value: "2"}}},
				{Name: "sidecar", Image: "proxy:1"},
			}},
		}},
	}
	rev1Template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"app": "web", appsv1.DefaultDeploymentUniqueLabelKey: "abc"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app", Image: "web:1", Env: []corev1.EnvVar{{Name: "OLD", Value: "1"}}},
		}},
	}
	rs1 := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-abc", Namespace: "default",
			Annotations: map[string]string{
				"deployment.kubernetes.io/revision": "1",
				"kubernetes.io/change-cause":        "initial",
			},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: rev1Template},
	}
	c, cs := newTestCluster(deploy, rs1)

	if err := c.RolloutUndo(t.Context(), "default", "web", 1); err != nil {
		t.Fatalf("RolloutUndo: %v", err)
	}
	got, err := cs.AppsV1().Deployments("default").Get(t.Context(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := rev1Template.DeepCopy()
	delete(want.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	if !reflect.DeepEqual(got.Spec.Template, *want) {
		t.Fatalf("template after rollback = %+v\nwant revision 1's minus pod-template-hash = %+v", got.Spec.Template, *want)
	}
	wantAnn := map[string]string{
		"deployment.kubernetes.io/revision": "2",
		corev1.LastAppliedConfigAnnotation:  "{}",
		"kubernetes.io/change-cause":        "initial",
	}
	if !reflect.DeepEqual(got.Annotations, wantAnn) {
		t.Fatalf("annotations after rollback = %v, want %v", got.Annotations, wantAnn)
	}
}

// TestRolloutUndoRefusesPausedDeployment matches kubectl rollout undo,
// which refuses a paused Deployment rather than queue a hidden rollback.
func TestRolloutUndoRefusesPausedDeployment(t *testing.T) {
	t.Parallel()
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Paused: true},
	}
	c, _ := newTestCluster(deploy)
	if err := c.RolloutUndo(t.Context(), "default", "web", 1); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("expected a paused-deployment error, got %v", err)
	}
}

func TestRolloutUndoRejectsUnknownRevision(t *testing.T) {
	t.Parallel()
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}}
	c, _ := newTestCluster(deploy)
	if err := c.RolloutUndo(t.Context(), "default", "api", 99); err == nil {
		t.Fatalf("expected an error for a revision with no matching ReplicaSet")
	}
}

func TestRolloutUndoRejectsEmptyName(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster()
	if err := c.RolloutUndo(t.Context(), "default", "", 1); err == nil {
		t.Fatalf("expected an error for an empty name")
	}
}

func TestCordonSetsUnschedulable(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	c, cs := newTestCluster(node)

	if err := c.Cordon(t.Context(), "node-1", true); err != nil {
		t.Fatalf("Cordon: %v", err)
	}
	got, err := cs.CoreV1().Nodes().Get(t.Context(), "node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Spec.Unschedulable {
		t.Fatalf("expected node to be unschedulable after Cordon(true)")
	}

	if err := c.Cordon(t.Context(), "node-1", false); err != nil {
		t.Fatalf("Uncordon: %v", err)
	}
	got, _ = cs.CoreV1().Nodes().Get(t.Context(), "node-1", metav1.GetOptions{})
	if got.Spec.Unschedulable {
		t.Fatalf("expected node schedulable after Cordon(false)")
	}
}

func TestDrainCordonsAndSkipsDaemonSetAndMirrorPods(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	evictable := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
	}
	daemonPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ds-1", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds"}},
		},
		Spec: corev1.PodSpec{NodeName: "node-1"},
	}
	mirrorPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "static-1", Namespace: "default",
			Annotations: map[string]string{corev1.MirrorPodAnnotationKey: "true"},
		},
		Spec: corev1.PodSpec{NodeName: "node-1"},
	}
	otherNodePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: "node-2"},
	}
	c, cs := newTestCluster(node, evictable, daemonPod, mirrorPod, otherNodePod)

	evicted, err := c.Drain(t.Context(), "node-1")
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if evicted != 1 {
		t.Fatalf("evicted = %d, want 1 (only the non-daemonset, non-mirror pod)", evicted)
	}

	gotNode, _ := cs.CoreV1().Nodes().Get(t.Context(), "node-1", metav1.GetOptions{})
	if !gotNode.Spec.Unschedulable {
		t.Fatalf("expected Drain to cordon the node")
	}
}

// TestDrainListsOnlyTheNodesPods pins review 10 M5: drain found one node's
// pods with an unfiltered cluster-wide list, pulling every pod in the
// cluster over the wire. The list must carry a spec.nodeName selector.
func TestDrainListsOnlyTheNodesPods(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	c, cs := newTestCluster(node)

	if _, err := c.Drain(t.Context(), "node-1"); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	var selectors []string
	for _, a := range cs.Actions() {
		if la, ok := a.(k8stesting.ListAction); ok && la.GetResource().Resource == "pods" {
			selectors = append(selectors, la.GetListRestrictions().Fields.String())
		}
	}
	if len(selectors) != 1 || selectors[0] != "spec.nodeName=node-1" {
		t.Fatalf("pod list field selectors = %q, want one list with spec.nodeName=node-1", selectors)
	}
}

func TestDrainRejectsEmptyNodeName(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster()
	if _, err := c.Drain(t.Context(), ""); err == nil {
		t.Fatalf("expected an error for an empty node name")
	}
}

func TestDeleteResourceForcedUsesZeroGracePeriod(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}}
	c, cs := newTestCluster(pod)

	if err := c.DeleteResourceForced(t.Context(), KindPod, "default", "api"); err != nil {
		t.Fatalf("DeleteResourceForced: %v", err)
	}
	if _, err := cs.CoreV1().Pods("default").Get(t.Context(), "api", metav1.GetOptions{}); err == nil {
		t.Fatalf("expected pod to be deleted")
	}
}

// deletePolicies returns the PropagationPolicy of every delete action
// recorded on actions ("<nil>" for none).
func deletePolicies(actions []k8stesting.Action) []string {
	var out []string
	for _, a := range actions {
		d, ok := a.(k8stesting.DeleteAction)
		if !ok {
			continue
		}
		p := "<nil>"
		if pp := d.GetDeleteOptions().PropagationPolicy; pp != nil {
			p = string(*pp)
		}
		out = append(out, p)
	}
	return out
}

// TestDeleteSendsBackgroundPropagation pins H4: an empty DeleteOptions
// leaves a batch/v1 Job on its server default, orphan, so ctrl-d on a running
// Job left its pods running ownerless. Every delete — plain or forced, typed
// or dynamic — sends kubectl's Background explicitly.
func TestDeleteSendsBackgroundPropagation(t *testing.T) {
	t.Parallel()
	for _, kind := range []ResourceKind{KindJob, KindNamespace, KindDeployment, KindPod} {
		for _, forced := range []bool{false, true} {
			c, cs := newTestCluster(
				&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"}},
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "x"}},
				&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"}},
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"}},
			)
			del := c.DeleteResource
			if forced {
				del = c.DeleteResourceForced
			}
			if err := del(t.Context(), kind, "default", "x"); err != nil {
				t.Fatalf("delete %s (forced=%t): %v", kind, forced, err)
			}
			if got := deletePolicies(cs.Actions()); !reflect.DeepEqual(got, []string{"Background"}) {
				t.Errorf("delete %s (forced=%t) propagation = %v, want [Background]", kind, forced, got)
			}
		}
	}
	c, dyn := newDynTestCluster(newWidget("thing", "default"))
	if err := c.DeleteResource(t.Context(), ResourceKind("Widget"), "default", "thing"); err != nil {
		t.Fatalf("DeleteResource(Widget): %v", err)
	}
	if got := deletePolicies(dyn.Actions()); !reflect.DeepEqual(got, []string{"Background"}) {
		t.Errorf("CRD delete propagation = %v, want [Background]", got)
	}
}

func replaceTestJob() *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "default", UID: "old-uid"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1.0"}}},
		}},
	}
}

// TestReplaceJobDeletesInBackgroundWithUIDPrecondition pins 06 H3: the
// replace's delete takes the old attempt's pods with it and can only hit the
// exact Job the user confirmed.
func TestReplaceJobDeletesInBackgroundWithUIDPrecondition(t *testing.T) {
	t.Parallel()
	c, cs := newTestCluster(replaceTestJob())
	if err := c.ReplaceJob(t.Context(), "default", "migrate"); err != nil {
		t.Fatalf("ReplaceJob: %v", err)
	}
	var verbs []string
	for _, a := range cs.Actions() {
		verbs = append(verbs, a.GetVerb())
		if d, ok := a.(k8stesting.DeleteAction); ok {
			o := d.GetDeleteOptions()
			if o.PropagationPolicy == nil || *o.PropagationPolicy != metav1.DeletePropagationBackground {
				t.Errorf("delete propagation = %v, want Background", o.PropagationPolicy)
			}
			if o.Preconditions == nil || o.Preconditions.UID == nil || *o.Preconditions.UID != "old-uid" {
				t.Errorf("delete preconditions = %+v, want UID old-uid", o.Preconditions)
			}
		}
	}
	// get, delete, then at least one get confirming it's gone, then create.
	if len(verbs) < 4 || verbs[1] != "delete" || verbs[2] != "get" || verbs[len(verbs)-1] != "create" {
		t.Errorf("verbs = %v, want get, delete, get…, create", verbs)
	}
}

// TestReplaceJobWaitsAndNeverCreatesOverALingeringJob: when the deleted Job
// is still there (an orphan/foreground finalizer pending), ReplaceJob must
// not race a Create into AlreadyExists — it waits, bounded, and then says
// plainly that the original is gone and no replacement was made.
func TestReplaceJobWaitsAndNeverCreatesOverALingeringJob(t *testing.T) {
	t.Parallel()
	c, cs := newTestCluster(replaceTestJob())
	c.replaceJobGoneTimeout = 300 * time.Millisecond
	lingering := replaceTestJob()
	cs.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil // accepted, but the object stays (finalizer pending)
	})
	cs.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, lingering, nil
	})
	err := c.ReplaceJob(t.Context(), "default", "migrate")
	if err == nil || !strings.Contains(err.Error(), "original deleted, replacement not created") {
		t.Fatalf("ReplaceJob err = %v, want an explicit not-created error", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			t.Fatalf("Create issued while the old Job still existed: %v", cs.Actions())
		}
	}
}

// TestReplaceJobReportsARecreateFailureExplicitly: a Create that still fails
// (AlreadyExists from a racing writer, quota, …) is reported as such, never
// as a bare error that reads like the delete failed.
func TestReplaceJobReportsARecreateFailureExplicitly(t *testing.T) {
	t.Parallel()
	c, cs := newTestCluster(replaceTestJob())
	cs.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(batchv1.Resource("jobs"), "migrate")
	})
	err := c.ReplaceJob(t.Context(), "default", "migrate")
	if err == nil || !strings.Contains(err.Error(), "original deleted, recreate failed") || !apierrors.IsAlreadyExists(err) {
		t.Fatalf("ReplaceJob err = %v, want a wrapped AlreadyExists naming the lost original", err)
	}
}

func TestSetImagePatchesNamedContainer(t *testing.T) {
	t.Parallel()
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app", Image: "app:1.0"},
			{Name: "sidecar", Image: "sidecar:1.0"},
		}}}},
	}
	c, cs := newTestCluster(deploy)

	if err := c.SetImage(t.Context(), KindDeployment, "default", "api", "app", "app:2.0", false); err != nil {
		t.Fatalf("SetImage: %v", err)
	}
	got, err := cs.AppsV1().Deployments("default").Get(t.Context(), "api", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.Template.Spec.Containers[0].Image != "app:2.0" {
		t.Fatalf("app image = %q, want app:2.0", got.Spec.Template.Spec.Containers[0].Image)
	}
	if got.Spec.Template.Spec.Containers[1].Image != "sidecar:1.0" {
		t.Fatalf("sidecar image = %q, want unchanged sidecar:1.0", got.Spec.Template.Spec.Containers[1].Image)
	}
}

func TestSetImagePatchesInitContainers(t *testing.T) {
	t.Parallel()
	restartAlways := corev1.ContainerRestartPolicyAlways
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "app:1.0"}},
			InitContainers: []corev1.Container{
				{Name: "prepare", Image: "prepare:1.0"},
				{Name: "mesh", Image: "mesh:1.0", RestartPolicy: &restartAlways},
			},
		}}},
	}
	c, cs := newTestCluster(deploy)

	if err := c.SetImage(t.Context(), KindDeployment, "default", "api", "prepare", "prepare:2.0", true); err != nil {
		t.Fatalf("SetImage conventional init container: %v", err)
	}
	if err := c.SetImage(t.Context(), KindDeployment, "default", "api", "mesh", "mesh:2.0", true); err != nil {
		t.Fatalf("SetImage native sidecar: %v", err)
	}
	got, err := cs.AppsV1().Deployments("default").Get(t.Context(), "api", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.Template.Spec.Containers[0].Image != "app:1.0" {
		t.Fatalf("regular container image = %q, want unchanged app:1.0", got.Spec.Template.Spec.Containers[0].Image)
	}
	if got.Spec.Template.Spec.InitContainers[0].Image != "prepare:2.0" {
		t.Fatalf("prepare image = %q, want prepare:2.0", got.Spec.Template.Spec.InitContainers[0].Image)
	}
	if got.Spec.Template.Spec.InitContainers[1].Image != "mesh:2.0" {
		t.Fatalf("mesh image = %q, want mesh:2.0", got.Spec.Template.Spec.InitContainers[1].Image)
	}
	if got.Spec.Template.Spec.InitContainers[1].RestartPolicy == nil || *got.Spec.Template.Spec.InitContainers[1].RestartPolicy != restartAlways {
		t.Fatal("native sidecar restart policy was not preserved")
	}
}

func TestSetImagePatchesCronJobTemplate(t *testing.T) {
	t.Parallel()
	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"},
		Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "job", Image: "job:1.0"},
				{Name: "sidecar", Image: "sidecar:1.0"},
			}}},
		}}},
	}
	c, cs := newTestCluster(cronJob)

	if err := c.SetImage(t.Context(), KindCronJob, "default", "nightly", "job", "job:2.0", false); err != nil {
		t.Fatalf("SetImage: %v", err)
	}
	got, err := cs.BatchV1().CronJobs("default").Get(t.Context(), "nightly", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	containers := got.Spec.JobTemplate.Spec.Template.Spec.Containers
	if containers[0].Image != "job:2.0" {
		t.Fatalf("job image = %q, want job:2.0", containers[0].Image)
	}
	if containers[1].Image != "sidecar:1.0" {
		t.Fatalf("sidecar image = %q, want unchanged sidecar:1.0", containers[1].Image)
	}
}

func TestSetImagePatchesCronJobInitContainer(t *testing.T) {
	t.Parallel()
	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"},
		Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers:     []corev1.Container{{Name: "job", Image: "job:1.0"}},
				InitContainers: []corev1.Container{{Name: "prepare", Image: "prepare:1.0"}},
			}},
		}}},
	}
	c, cs := newTestCluster(cronJob)

	if err := c.SetImage(t.Context(), KindCronJob, "default", "nightly", "prepare", "prepare:2.0", true); err != nil {
		t.Fatalf("SetImage: %v", err)
	}
	got, err := cs.BatchV1().CronJobs("default").Get(t.Context(), "nightly", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.JobTemplate.Spec.Template.Spec.InitContainers[0].Image != "prepare:2.0" {
		t.Fatalf("prepare image = %q, want prepare:2.0", got.Spec.JobTemplate.Spec.Template.Spec.InitContainers[0].Image)
	}
	if got.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image != "job:1.0" {
		t.Fatalf("job image = %q, want unchanged job:1.0", got.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestSetImageRejectsEmptyName(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster()
	if err := c.SetImage(t.Context(), KindDeployment, "default", "", "app", "app:2.0", false); err == nil {
		t.Fatalf("expected an error for an empty resource name")
	}
}

func TestSetImageCommandStringAcrossKinds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind ResourceKind
		want string
	}{
		{KindDeployment, "kubectl set image deploy/api app=app:2.0 -n default"},
		{KindStatefulSet, "kubectl set image sts/api app=app:2.0 -n default"},
		{KindDaemonSet, "kubectl set image ds/api app=app:2.0 -n default"},
		{KindCronJob, "kubectl set image cronjob/api app=app:2.0 -n default"},
	}
	for _, tt := range tests {
		if got := SetImageCommandString(tt.kind, "default", "api", "app", "app:2.0"); got != tt.want {
			t.Errorf("SetImageCommandString(%s) = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

func strPtr(s string) *string { return &s }

func TestSetResourcesPatchesRequestsAndLimits(t *testing.T) {
	t.Parallel()
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			}},
		}}}},
	}
	c, cs := newTestCluster(deploy)

	edits := ResourceEdits{MEMLimit: strPtr("768Mi")}
	if err := c.SetResources(t.Context(), KindDeployment, "default", "api", "app", edits, false); err != nil {
		t.Fatalf("SetResources: %v", err)
	}
	got, err := cs.AppsV1().Deployments("default").Get(t.Context(), "api", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	res := got.Spec.Template.Spec.Containers[0].Resources
	if q := res.Limits[corev1.ResourceMemory]; q.String() != "768Mi" {
		t.Fatalf("mem limit = %s, want 768Mi", q.String())
	}
	if q := res.Requests[corev1.ResourceMemory]; q.String() != "512Mi" {
		t.Fatalf("mem request = %s, want unchanged 512Mi", q.String())
	}
}

func TestSetResourcesUnsetsField(t *testing.T) {
	t.Parallel()
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app", Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			}},
		}}}},
	}
	c, cs := newTestCluster(deploy)

	edits := ResourceEdits{CPULimit: strPtr("")}
	if err := c.SetResources(t.Context(), KindDeployment, "default", "api", "app", edits, false); err != nil {
		t.Fatalf("SetResources: %v", err)
	}
	got, err := cs.AppsV1().Deployments("default").Get(t.Context(), "api", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := got.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]; ok {
		t.Fatalf("expected cpu limit removed, still present: %+v", got.Spec.Template.Spec.Containers[0].Resources.Limits)
	}
}

func TestSetResourcesRejectsEmptyName(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster()
	if err := c.SetResources(t.Context(), KindDeployment, "default", "", "app", ResourceEdits{MEMLimit: strPtr("768Mi")}, false); err == nil {
		t.Fatalf("expected an error for an empty resource name")
	}
}

// TestSetResourcesDryRunStillMutatesFakeClientset documents (rather than
// prescribes) client-go's fake Clientset behavior: it has no admission
// simulation, and it doesn't special-case metav1.PatchOptions.DryRun either
// — a dry-run patch against it mutates the tracked object exactly like a
// real one. kube.Cluster.SetResources's own dry-run therefore only proves
// anything against a real API server; 25a's own client-side validation
// (quantity parsing, request>limit) is what's actually exercised in tests.
func TestSetResourcesDryRunStillMutatesFakeClientset(t *testing.T) {
	t.Parallel()
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app"},
		}}}},
	}
	c, cs := newTestCluster(deploy)

	if err := c.SetResources(t.Context(), KindDeployment, "default", "api", "app", ResourceEdits{MEMLimit: strPtr("768Mi")}, true); err != nil {
		t.Fatalf("SetResources(dryRun): %v", err)
	}
	got, err := cs.AppsV1().Deployments("default").Get(t.Context(), "api", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if q := got.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]; q.String() != "768Mi" {
		t.Fatalf("fake clientset dry-run mutated = %s — if this ever changes, update kube/fake and 25a's commitSetResources accordingly", q.String())
	}
}

func TestSetResourcesRejectsNoChangedFields(t *testing.T) {
	t.Parallel()
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}}
	c, _ := newTestCluster(deploy)
	if err := c.SetResources(t.Context(), KindDeployment, "default", "api", "app", ResourceEdits{}, false); err == nil {
		t.Fatalf("expected an error when edits has no changed fields")
	}
}

func TestSetResourcesCommandStringPlainSet(t *testing.T) {
	t.Parallel()
	got := SetResourcesCommandString(KindDeployment, "nva-stage", "nva-worker", "worker", ResourceEdits{MEMLimit: strPtr("768Mi")})
	want := "kubectl set resources deploy/nva-worker -c worker --limits=memory=768Mi -n nva-stage"
	if got != want {
		t.Errorf("SetResourcesCommandString = %q, want %q", got, want)
	}
}

func TestSetResourcesCommandStringMultipleFields(t *testing.T) {
	t.Parallel()
	got := SetResourcesCommandString(KindStatefulSet, "default", "db", "db", ResourceEdits{
		CPURequest: strPtr("100m"), MEMRequest: strPtr("256Mi"), CPULimit: strPtr("500m"),
	})
	want := "kubectl set resources sts/db -c db --limits=cpu=500m --requests=cpu=100m,memory=256Mi -n default"
	if got != want {
		t.Errorf("SetResourcesCommandString = %q, want %q", got, want)
	}
}

func TestSetResourcesCommandStringUnsetFallsBackToPatch(t *testing.T) {
	t.Parallel()
	got := SetResourcesCommandString(KindDaemonSet, "default", "agent", "agent", ResourceEdits{CPULimit: strPtr("")})
	want := `kubectl patch ds/agent --type strategic -p '{"spec":{"template":{"spec":{"containers":[{"name":"agent","resources":{"limits":{"cpu":null}}}]}}}}' -n default`
	if got != want {
		t.Errorf("SetResourcesCommandString = %q, want %q", got, want)
	}
}

func TestMetaCommandStringSetOverwriteAndRemove(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		isAnnotation      bool
		key, value        string
		remove, overwrite bool
		want              string
	}{
		{
			name: "overwrite existing label", key: "env", value: "staging", overwrite: true,
			want: "kubectl label deploy/nva-worker env=staging --overwrite -n nva-stage",
		},
		{
			name: "new label, no overwrite flag", key: "tier", value: "gold",
			want: "kubectl label deploy/nva-worker tier=gold -n nva-stage",
		},
		{
			name: "annotation set", isAnnotation: true, key: "kute.dev/owner", value: "platform-oncall", overwrite: true,
			want: "kubectl annotate deploy/nva-worker kute.dev/owner=platform-oncall --overwrite -n nva-stage",
		},
		{
			name: "label removal ignores overwrite", key: "team", remove: true, overwrite: true,
			want: "kubectl label deploy/nva-worker team- -n nva-stage",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := MetaCommandString(KindDeployment, "nva-stage", "nva-worker", tt.isAnnotation, tt.key, tt.value, tt.remove, tt.overwrite)
			if got != tt.want {
				t.Errorf("MetaCommandString = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMetaCommandStringOmitsNamespaceForClusterScopedKind(t *testing.T) {
	t.Parallel()
	got := MetaCommandString(KindNode, "", "node-a", false, "env", "prod", false, false)
	want := "kubectl label node/node-a env=prod"
	if got != want {
		t.Errorf("MetaCommandString = %q, want %q", got, want)
	}
}

func TestPatchMetaSetsAndRemovesLabelsAndAnnotations(t *testing.T) {
	t.Parallel()
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "nva-worker", Namespace: "default", Labels: map[string]string{"env": "stage"},
	}}
	c, cs := newTestCluster(deploy)
	ctx := t.Context()

	if err := c.PatchMeta(ctx, KindDeployment, "default", "nva-worker", false, "env", "staging", false); err != nil {
		t.Fatalf("PatchMeta set: %v", err)
	}
	got, err := cs.AppsV1().Deployments("default").Get(ctx, "nva-worker", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Labels["env"] != "staging" {
		t.Errorf("labels[env] = %q, want staging", got.Labels["env"])
	}

	if err := c.PatchMeta(ctx, KindDeployment, "default", "nva-worker", true, "kute.dev/owner", "platform-oncall", false); err != nil {
		t.Fatalf("PatchMeta annotate: %v", err)
	}
	got, err = cs.AppsV1().Deployments("default").Get(ctx, "nva-worker", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Annotations["kute.dev/owner"] != "platform-oncall" {
		t.Errorf("annotations[kute.dev/owner] = %q, want platform-oncall", got.Annotations["kute.dev/owner"])
	}

	if err := c.PatchMeta(ctx, KindDeployment, "default", "nva-worker", false, "env", "", true); err != nil {
		t.Fatalf("PatchMeta remove: %v", err)
	}
	got, err = cs.AppsV1().Deployments("default").Get(ctx, "nva-worker", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := got.Labels["env"]; ok {
		t.Errorf("expected env label removed, got %+v", got.Labels)
	}
}

func TestPatchMetaUnsupportedKindReturnsError(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster()
	if err := c.PatchMeta(t.Context(), ResourceKind("Widget"), "default", "thing", false, "k", "v", false); err == nil {
		t.Fatal("expected an error for a kind with no typed client and no discovered dynamic GVR")
	}
}

// widgetGVR/newDynTestCluster back the custom-resource write paths: a
// cluster whose only knowledge of "Widget" is the discovery snapshot, with
// no informer ever started for it. That is deliberately the harder case —
// resolution through resourceFor is what makes it work, and getDynKind
// alone would not.
var widgetGVR = schema.GroupVersionResource{Group: "example.test.io", Version: "v1", Resource: "widgets"}

func newDynTestCluster(objs ...runtime.Object) (*Cluster, *dynamicfake.FakeDynamicClient) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme, map[schema.GroupVersionResource]string{widgetGVR: "WidgetList"}, objs...)
	return &Cluster{
		clientset: fake.NewSimpleClientset(),
		dynClient: dyn,
		discovered: []DiscoveredKind{{
			GVR: widgetGVR, Kind: "Widget", Plural: "widgets", Group: "example.test.io",
			Versions:    []CRDVersion{{Name: "v1", Served: true, Storage: true}},
			Established: true, CRDName: "widgets.example.test.io",
		}},
	}, dyn
}

func newWidget(name, ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.test.io/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": name, "namespace": ns},
	}}
}

// TestDeleteResourceFallsBackToTheDynamicClient covers the gap that made
// ctrl-d fail on every discovered CRD row while the keybar advertised it.
func TestDeleteResourceFallsBackToTheDynamicClient(t *testing.T) {
	t.Parallel()
	c, dyn := newDynTestCluster(newWidget("thing", "default"))

	if err := c.DeleteResource(t.Context(), ResourceKind("Widget"), "default", "thing"); err != nil {
		t.Fatalf("DeleteResource: %v", err)
	}
	if _, err := dyn.Resource(widgetGVR).Namespace("default").Get(t.Context(), "thing", metav1.GetOptions{}); err == nil {
		t.Fatal("expected the custom resource to be gone")
	}
}

// TestPatchMetaReachesACustomResourceWithNoInformerStarted pins the second
// half of the same fix: PatchMeta used to resolve through getDynKind, which
// only knows kinds whose informer already started, so 26a failed on an
// object the user reached without browsing its list first.
func TestPatchMetaReachesACustomResourceWithNoInformerStarted(t *testing.T) {
	t.Parallel()
	c, dyn := newDynTestCluster(newWidget("thing", "default"))

	if _, ok := c.getDynKind(ResourceKind("Widget"), ""); ok {
		t.Fatal("precondition: no informer should be registered for Widget")
	}
	err := c.PatchMeta(t.Context(), ResourceKind("Widget"), "default", "thing", true, "team", "platform", false)
	if err != nil {
		t.Fatalf("PatchMeta: %v", err)
	}
	got, err := dyn.Resource(widgetGVR).Namespace("default").Get(t.Context(), "thing", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetAnnotations()["team"] != "platform" {
		t.Errorf("annotation not applied, got %+v", got.GetAnnotations())
	}
}

func TestSecretDataCommandStringMasksValueAndRendersRemoval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		key    string
		remove bool
		want   string
	}{
		{
			name: "add masks the value",
			key:  "SMTP_PASSWORD",
			want: `kubectl patch secret/nva-secrets --type merge -p '{"stringData":{"SMTP_PASSWORD":"••••••"}}' -n nva-stage`,
		},
		{
			name:   "removal renders the null literal, no mask needed",
			key:    "SMTP_PASSWORD",
			remove: true,
			want:   `kubectl patch secret/nva-secrets --type merge -p '{"data":{"SMTP_PASSWORD":null}}' -n nva-stage`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := SecretDataCommandString("nva-stage", "nva-secrets", tt.key, tt.remove)
			if got != tt.want {
				t.Errorf("SecretDataCommandString = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPatchSecretDataSetsAndRemovesKeys pins the patch shape PatchSecretData
// sends, not the real apiserver's stringData→data folding: the fake
// clientset's tracker applies a merge patch as a raw structural JSON merge,
// with no admission/REST-strategy pass to speak of, so a patched
// .stringData key lands in got.StringData here rather than got.Data — the
// same class of gap SetResources' own dry-run test already documents (the
// fake clientset performs no admission). Only a real cluster does the
// actual base64-encode-and-merge-into-.data kute relies on in practice.
func TestPatchSecretDataSetsAndRemovesKeys(t *testing.T) {
	t.Parallel()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "nva-secrets", Namespace: "default"},
		Data:       map[string][]byte{"DATABASE_URL": []byte("postgres://old")},
	}
	c, cs := newTestCluster(secret)
	ctx := t.Context()

	if _, err := c.PatchSecretData(ctx, "default", "nva-secrets", "1", "SMTP_PASSWORD", "hunter2-staging", false); err != nil {
		t.Fatalf("PatchSecretData add: %v", err)
	}
	got, err := cs.CoreV1().Secrets("default").Get(ctx, "nva-secrets", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.StringData["SMTP_PASSWORD"] != "hunter2-staging" {
		t.Errorf("stringData[SMTP_PASSWORD] = %q, want hunter2-staging", got.StringData["SMTP_PASSWORD"])
	}
	if string(got.Data["DATABASE_URL"]) != "postgres://old" {
		t.Errorf("existing key data[DATABASE_URL] = %q, want unchanged", got.Data["DATABASE_URL"])
	}

	if _, err := c.PatchSecretData(ctx, "default", "nva-secrets", "1", "DATABASE_URL", "", true); err != nil {
		t.Fatalf("PatchSecretData remove: %v", err)
	}
	got, err = cs.CoreV1().Secrets("default").Get(ctx, "nva-secrets", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := got.Data["DATABASE_URL"]; ok {
		t.Errorf("expected DATABASE_URL removed, got %+v", got.Data)
	}
}

func TestPatchSecretDataRejectsEmptyName(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster()
	if _, err := c.PatchSecretData(t.Context(), "default", "", "1", "k", "v", false); err == nil {
		t.Fatal("expected an error for an empty secret name")
	}
}

// TestConfigMapDataCommandStringRendersValueVerbatim pins 27a's own
// deliberate departure from SecretDataCommandString: a ConfigMap value isn't
// sensitive, so the will-run line prints it as-is rather than masking it.
func TestConfigMapDataCommandStringRendersValueVerbatim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		key    string
		value  string
		remove bool
		want   string
	}{
		{
			name:  "add renders the real value",
			key:   "LOG_LEVEL",
			value: "debug",
			want:  `kubectl patch cm/nva-config --type merge -p '{"data":{"LOG_LEVEL":"debug"}}' -n nva-stage`,
		},
		{
			name:   "removal renders the null literal",
			key:    "LOG_LEVEL",
			remove: true,
			want:   `kubectl patch cm/nva-config --type merge -p '{"data":{"LOG_LEVEL":null}}' -n nva-stage`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ConfigMapDataCommandString("nva-stage", "nva-config", tt.key, tt.value, tt.remove)
			if got != tt.want {
				t.Errorf("ConfigMapDataCommandString = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigMapConsumerRestartCommandStringAcrossKinds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ref  ConfigMapConsumerRef
		want string
	}{
		{ConfigMapConsumerRef{Kind: KindDeployment, Name: "nva-worker"}, "kubectl rollout restart deploy/nva-worker -n nva-stage"},
		{ConfigMapConsumerRef{Kind: KindStatefulSet, Name: "nva-db"}, "kubectl rollout restart sts/nva-db -n nva-stage"},
		{ConfigMapConsumerRef{Kind: KindDaemonSet, Name: "nva-agent"}, "kubectl rollout restart ds/nva-agent -n nva-stage"},
	}
	for _, tt := range tests {
		got := ConfigMapConsumerRestartCommandString("nva-stage", tt.ref)
		if got != tt.want {
			t.Errorf("ConfigMapConsumerRestartCommandString(%+v) = %q, want %q", tt.ref, got, tt.want)
		}
	}
}

// TestPatchConfigMapDataSetsAndRemovesKeys pins the patch shape
// PatchConfigMapData sends against the fake clientset's tracker.
func TestPatchConfigMapDataSetsAndRemovesKeys(t *testing.T) {
	t.Parallel()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "nva-config", Namespace: "default"},
		Data:       map[string]string{"LOG_LEVEL": "info"},
	}
	c, cs := newTestCluster(cm)
	ctx := t.Context()

	if _, err := c.PatchConfigMapData(ctx, "default", "nva-config", "1", "FEATURE_X", "on", false); err != nil {
		t.Fatalf("PatchConfigMapData add: %v", err)
	}
	got, err := cs.CoreV1().ConfigMaps("default").Get(ctx, "nva-config", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Data["FEATURE_X"] != "on" {
		t.Errorf("data[FEATURE_X] = %q, want on", got.Data["FEATURE_X"])
	}
	if got.Data["LOG_LEVEL"] != "info" {
		t.Errorf("existing key data[LOG_LEVEL] = %q, want unchanged", got.Data["LOG_LEVEL"])
	}

	if _, err := c.PatchConfigMapData(ctx, "default", "nva-config", "1", "LOG_LEVEL", "", true); err != nil {
		t.Fatalf("PatchConfigMapData remove: %v", err)
	}
	got, err = cs.CoreV1().ConfigMaps("default").Get(ctx, "nva-config", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := got.Data["LOG_LEVEL"]; ok {
		t.Errorf("expected LOG_LEVEL removed, got %+v", got.Data)
	}
}

func TestPatchConfigMapDataRejectsEmptyName(t *testing.T) {
	t.Parallel()
	c, _ := newTestCluster()
	if _, err := c.PatchConfigMapData(t.Context(), "default", "", "1", "k", "v", false); err == nil {
		t.Fatal("expected an error for an empty configmap name")
	}
}

// TestPatchDataCarriesResourceVersionPrecondition pins review finding 09
// M5: every 27a/27b data patch carries the caller's observed
// metadata.resourceVersion, so a concurrent writer's change to the same key
// is a 409 Conflict rather than a silent lost update — and a missing one is
// refused rather than sent unconditionally.
func TestPatchDataCarriesResourceVersionPrecondition(t *testing.T) {
	t.Parallel()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default", ResourceVersion: "7"}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default", ResourceVersion: "7"}}
	tests := []struct {
		name   string
		call   func(c *Cluster, rv string) error
		remove bool
	}{
		{"secret set", func(c *Cluster, rv string) error {
			_, err := c.PatchSecretData(t.Context(), "default", "s", rv, "K", "v", false)
			return err
		}, false},
		{"secret remove", func(c *Cluster, rv string) error {
			_, err := c.PatchSecretData(t.Context(), "default", "s", rv, "K", "", true)
			return err
		}, true},
		{"configmap set", func(c *Cluster, rv string) error {
			_, err := c.PatchConfigMapData(t.Context(), "default", "c", rv, "K", "v", false)
			return err
		}, false},
		{"configmap remove", func(c *Cluster, rv string) error {
			_, err := c.PatchConfigMapData(t.Context(), "default", "c", rv, "K", "", true)
			return err
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cs := newTestCluster(secret.DeepCopy(), cm.DeepCopy())
			if err := tt.call(c, ""); err == nil {
				t.Fatal("expected a missing resourceVersion to be refused")
			}
			if err := tt.call(c, "7"); err != nil {
				t.Fatalf("patch: %v", err)
			}
			var patches int
			for _, a := range cs.Actions() {
				p, ok := a.(k8stesting.PatchAction)
				if !ok {
					continue
				}
				patches++
				if p.GetPatchType() != types.MergePatchType {
					t.Errorf("patch type = %v, want a merge patch", p.GetPatchType())
				}
				var body struct {
					Metadata struct {
						ResourceVersion string `json:"resourceVersion"`
					} `json:"metadata"`
					Data map[string]*string `json:"data"`
				}
				if err := json.Unmarshal(p.GetPatch(), &body); err != nil {
					t.Fatalf("patch body %s is not JSON: %v", p.GetPatch(), err)
				}
				if body.Metadata.ResourceVersion != "7" {
					t.Errorf("patch body %s: metadata.resourceVersion = %q, want 7", p.GetPatch(), body.Metadata.ResourceVersion)
				}
				if v, ok := body.Data["K"]; tt.remove && (!ok || v != nil) {
					t.Errorf("patch body %s: want data.K null for a removal", p.GetPatch())
				}
			}
			if patches != 1 {
				t.Errorf("sent %d patches, want exactly 1 (the empty-precondition call must not reach the server)", patches)
			}
		})
	}
}

func TestScaleCommandStringAcrossKinds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind ResourceKind
		want string
	}{
		{KindDeployment, "kubectl scale deploy/api --replicas=3 -n default"},
		{KindStatefulSet, "kubectl scale sts/api --replicas=3 -n default"},
	}
	for _, tt := range tests {
		if got := ScaleCommandString(tt.kind, "default", "api", 3); got != tt.want {
			t.Errorf("ScaleCommandString(%s) = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

func TestDrainEvicts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		pod  corev1.Pod
		want bool
	}{
		{name: "plain", pod: corev1.Pod{}, want: true},
		{name: "replicaset-owned", pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet"}}}}, want: true},
		{name: "daemonset-owned", pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet"}}}}, want: false},
		{name: "mirror", pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{corev1.MirrorPodAnnotationKey: "true"}}}, want: false},
	}
	for _, tt := range tests {
		if got := DrainEvicts(&tt.pod); got != tt.want {
			t.Errorf("DrainEvicts(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDrainImpact(t *testing.T) {
	t.Parallel()
	ctrl := true
	owned := func(kind string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{Kind: kind, Name: "x", Controller: &ctrl}}
	}
	emptyDir := []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	configMap := []corev1.Volume{{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}}}
	tests := []struct {
		name string
		pod  corev1.Pod
		want DrainImpact
	}{
		{name: "daemonset", pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: owned("DaemonSet")}, Spec: corev1.PodSpec{Volumes: emptyDir}}, want: DrainImpact{}},
		{name: "mirror", pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{corev1.MirrorPodAnnotationKey: "x"}}, Spec: corev1.PodSpec{Volumes: emptyDir}}, want: DrainImpact{}},
		{name: "replicaset-owned", pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: owned("ReplicaSet")}, Spec: corev1.PodSpec{Volumes: configMap}}, want: DrainImpact{Evict: 1}},
		{name: "bare", pod: corev1.Pod{}, want: DrainImpact{Evict: 1, Unmanaged: 1}},
		{name: "non-controller ownerRef", pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "x"}}}}, want: DrainImpact{Evict: 1, Unmanaged: 1}},
		{name: "replicaset-owned with emptyDir", pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: owned("ReplicaSet")}, Spec: corev1.PodSpec{Volumes: emptyDir}}, want: DrainImpact{Evict: 1, EmptyDir: 1}},
		{name: "bare with emptyDir", pod: corev1.Pod{Spec: corev1.PodSpec{Volumes: append(append([]corev1.Volume{}, configMap...), emptyDir...)}}, want: DrainImpact{Evict: 1, Unmanaged: 1, EmptyDir: 1}},
	}
	all := make([]*corev1.Pod, 0, len(tests))
	for _, tt := range tests {
		var got DrainImpact
		got.Add(&tt.pod)
		if got != tt.want {
			t.Errorf("DrainImpact.Add(%s) = %+v, want %+v", tt.name, got, tt.want)
		}
		all = append(all, &tt.pod)
	}
	if got, want := DrainImpactOf(all), (DrainImpact{Evict: 5, Unmanaged: 3, EmptyDir: 2}); got != want {
		t.Errorf("DrainImpactOf(all) = %+v, want %+v", got, want)
	}
}

// TestSetFluxSuspendPatchesSpecSuspend asserts the recorded dynamic-client
// action, not just the absence of an error: the patch has to reach the
// right GVR with the right body, on a kind whose informer never started.
func TestSetFluxSuspendPatchesSpecSuspend(t *testing.T) {
	t.Parallel()
	for _, suspend := range []bool{true, false} {
		c, dyn := newDynTestCluster(newWidget("podinfo", "flux-system"))
		if err := c.SetFluxSuspend(t.Context(), ResourceKind("Widget"), "flux-system", "podinfo", suspend); err != nil {
			t.Fatalf("SetFluxSuspend(%v): %v", suspend, err)
		}
		var patched bool
		for _, a := range dyn.Actions() {
			p, ok := a.(k8stesting.PatchAction)
			if !ok {
				continue
			}
			patched = true
			if got := a.GetResource(); got != widgetGVR {
				t.Errorf("patched %v, want %v", got, widgetGVR)
			}
			if got, want := string(p.GetPatch()), fmt.Sprintf(`{"spec":{"suspend":%t}}`, suspend); got != want {
				t.Errorf("patch body = %s, want %s", got, want)
			}
			if p.GetPatchType() != types.MergePatchType {
				t.Errorf("patch type = %v, want a merge patch", p.GetPatchType())
			}
		}
		if !patched {
			t.Fatal("no patch reached the dynamic client")
		}
		got, err := dyn.Resource(widgetGVR).Namespace("flux-system").Get(t.Context(), "podinfo", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if v, _, _ := unstructured.NestedBool(got.Object, "spec", "suspend"); v != suspend {
			t.Errorf("spec.suspend = %v, want %v", v, suspend)
		}
	}
}

// TestRequestArgoSyncCreditsTheInitiator pins §33a's whole patch body: the
// sync carries operation.initiatedBy in Argo CD's OperationInitiator shape,
// so Argo records who synced instead of nobody (which 34a would otherwise
// have to leave blank), and the will-run line prints that same body.
func TestRequestArgoSyncCreditsTheInitiator(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ initiator, want string }{
		{"alice@example.com", "alice@example.com"},
		{"", DefaultArgoSyncInitiator},
	} {
		c, dyn := newDynTestCluster(newWidget("shop", "argocd"))
		if err := c.RequestArgoSync(t.Context(), ResourceKind("Widget"), "argocd", "shop", "main", tc.initiator); err != nil {
			t.Fatalf("RequestArgoSync: %v", err)
		}
		wantBody := `{"operation":{"initiatedBy":{"automated":false,"username":"` + tc.want + `"},"sync":{"revision":"main"}}}`
		var patched bool
		for _, a := range dyn.Actions() {
			p, ok := a.(k8stesting.PatchAction)
			if !ok {
				continue
			}
			patched = true
			if got := string(p.GetPatch()); got != wantBody {
				t.Errorf("patch body = %s, want %s", got, wantBody)
			}
			if p.GetPatchType() != types.MergePatchType {
				t.Errorf("patch type = %v, want a merge patch", p.GetPatchType())
			}
		}
		if !patched {
			t.Fatal("no patch reached the dynamic client")
		}
		got, err := dyn.Resource(widgetGVR).Namespace("argocd").Get(t.Context(), "shop", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if v, _, _ := unstructured.NestedString(got.Object, "operation", "initiatedBy", "username"); v != tc.want {
			t.Errorf("operation.initiatedBy.username = %q, want %q", v, tc.want)
		}
		cmd := ArgoSyncCommandString(KindPod, "argocd", "shop", "main", tc.initiator)
		if !strings.Contains(cmd, "'"+wantBody+"'") {
			t.Errorf("will-run %q does not print the body RequestArgoSync sends", cmd)
		}
	}
}

func TestArgoSyncInitiatorPrefersTheReviewedUsername(t *testing.T) {
	if got := ArgoSyncInitiator(ReviewedIdentity{Username: "alice"}, true); got != "alice" {
		t.Errorf("reviewed: got %q, want alice", got)
	}
	if got := ArgoSyncInitiator(ReviewedIdentity{}, false); got != "kute" {
		t.Errorf("unreviewed: got %q, want kute", got)
	}
}

// TestRequestFluxReconcileStampsAFreshTimestamp pins that the annotation
// lands and parses as RFC3339 — a malformed stamp is silently ignored by
// the Flux controllers, so "it wrote something" is not enough.
func TestRequestFluxReconcileStampsAFreshTimestamp(t *testing.T) {
	t.Parallel()
	c, dyn := newDynTestCluster(newWidget("podinfo", "flux-system"))
	before := time.Now().Add(-time.Second)

	if err := c.RequestFluxReconcile(t.Context(), ResourceKind("Widget"), "flux-system", "podinfo"); err != nil {
		t.Fatalf("RequestFluxReconcile: %v", err)
	}
	got, err := dyn.Resource(widgetGVR).Namespace("flux-system").Get(t.Context(), "podinfo", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	stamp, ok := got.GetAnnotations()[FluxReconcileAnnotation]
	if !ok {
		t.Fatalf("no %s annotation, got %+v", FluxReconcileAnnotation, got.GetAnnotations())
	}
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("annotation %q is not RFC3339 — the controllers ignore a malformed stamp: %v", stamp, err)
	}
	if at.Before(before) {
		t.Errorf("stamp %v is older than the call", at)
	}
}

// TestFluxCommandStringsNameTheCommandThatRuns: the will-run line documents
// the kubectl kute actually issues, not the `flux` equivalent it has no
// binary for. The resource arg is fully qualified for a substituted kind.
func TestFluxCommandStringsNameTheCommandThatRuns(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 1, 14, 7, 2, 0, time.UTC)

	got := FluxSuspendCommandString(ResourceKind("Kustomization"), "flux-system", "nebula-workers", true)
	want := `kubectl patch kustomization/nebula-workers --type merge -p '{"spec":{"suspend":true}}' -n flux-system`
	if got != want {
		t.Errorf("suspend:\n got %s\nwant %s", got, want)
	}

	got = FluxReconcileCommandString(ResourceKind("Kustomization"), "flux-system", "nebula-workers", at)
	want = `kubectl annotate kustomization/nebula-workers reconcile.fluxcd.io/requestedAt="2026-08-01T14:07:02Z" --overwrite -n flux-system`
	if got != want {
		t.Errorf("reconcile:\n got %s\nwant %s", got, want)
	}

	// A substituted kind must render its fully-qualified resource, or the
	// pasted command resolves to 18a's Helm-3 kind or to nothing. Reads the
	// real table's own entry — this test is parallel, so it must not go
	// through withSubstitution, which writes the package global.
	got = FluxSuspendCommandString(KindFluxHelmRelease, "flux-system", "podinfo", true)
	if !strings.Contains(got, "helmreleases.helm.toolkit.fluxcd.io/podinfo") {
		t.Errorf("expected the fully-qualified resource arg, got %s", got)
	}
}

// certificateGVR/newCertDynTestCluster/newCertificate back §35c's
// RenewCertificate tests — a Certificate whose only informer-free knowledge
// is the discovery snapshot, the same "resolution through resourceFor,
// never getDynKind alone" shape widgetGVR/newDynTestCluster exist for.
var certificateGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}

func newCertDynTestCluster(objs ...runtime.Object) (*Cluster, *dynamicfake.FakeDynamicClient) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme, map[schema.GroupVersionResource]string{certificateGVR: "CertificateList"}, objs...)
	return &Cluster{
		clientset: fake.NewSimpleClientset(),
		dynClient: dyn,
		discovered: []DiscoveredKind{{
			GVR: certificateGVR, Kind: "Certificate", Plural: "certificates", Group: "cert-manager.io",
			Versions:    []CRDVersion{{Name: "v1", Served: true, Storage: true}},
			Established: true, CRDName: "certificates.cert-manager.io",
		}},
	}, dyn
}

func newCertificate(name, ns string, conditions ...map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata":   map[string]any{"name": name, "namespace": ns},
	}
	if len(conditions) > 0 {
		conds := make([]any, len(conditions))
		for i, c := range conditions {
			conds[i] = c
		}
		obj["status"] = map[string]any{"conditions": conds}
	}
	return &unstructured.Unstructured{Object: obj}
}

// TestRenewCertificatePatchesStatusSubresource asserts the recorded dynamic-
// client action, not just the absence of an error: the patch has to be a
// merge patch, reach the status subresource specifically (a spec-only patch
// would be a silent no-op against a real API server), and carry the exact
// body renewCertificatePatch/RenewCertificateCommandString both name.
func TestRenewCertificatePatchesStatusSubresource(t *testing.T) {
	t.Parallel()
	c, dyn := newCertDynTestCluster(newCertificate("web-tls", "default",
		map[string]any{"type": "Ready", "status": "True"}))

	if err := c.RenewCertificate(t.Context(), "default", "web-tls"); err != nil {
		t.Fatalf("RenewCertificate: %v", err)
	}

	var patched bool
	for _, a := range dyn.Actions() {
		p, ok := a.(k8stesting.PatchAction)
		if !ok {
			continue
		}
		patched = true
		if got := a.GetResource(); got != certificateGVR {
			t.Errorf("patched %v, want %v", got, certificateGVR)
		}
		if p.GetPatchType() != types.MergePatchType {
			t.Errorf("patch type = %v, want a merge patch", p.GetPatchType())
		}
		if p.GetSubresource() != "status" {
			t.Errorf("subresource = %q, want %q", p.GetSubresource(), "status")
		}
		if got := string(p.GetPatch()); got != renewCertificatePatch {
			t.Errorf("patch body = %s, want %s", got, renewCertificatePatch)
		}
	}
	if !patched {
		t.Fatal("no patch reached the dynamic client")
	}
}

func TestRenewCertificateRejectsEmptyName(t *testing.T) {
	t.Parallel()
	c, _ := newCertDynTestCluster()
	if err := c.RenewCertificate(t.Context(), "default", ""); err == nil {
		t.Fatal("expected an error for an empty certificate name")
	}
}

// TestRenewCertificateCommandStringNamesTheLiteralPatch pins §35c's own
// will-run contract: the line shows kubectl, never `cmctl renew` — cmctl is
// what a human would type, but kute doesn't shell out to it, and its body
// must be byte-identical to renewCertificatePatch or the copyable line would
// lie about what ↵ actually runs.
func TestRenewCertificateCommandStringNamesTheLiteralPatch(t *testing.T) {
	t.Parallel()
	got := RenewCertificateCommandString("nva-stage", "admin-tls")
	want := `kubectl patch certificate/admin-tls --type merge -p '{"status":{"conditions":[{"type":"Issuing","status":"True","reason":"ManuallyTriggered"}]}}' -n nva-stage --subresource=status`
	if got != want {
		t.Errorf("RenewCertificateCommandString():\n got %s\nwant %s", got, want)
	}
	if strings.Contains(got, "cmctl") {
		t.Errorf("will-run line must not name the cmctl binary: %q", got)
	}
}

// TestPatchDataReturnsServerResourceVersion: a 27a/27b data patch returns
// the resourceVersion the API server answered with — the object after the
// write — so the editor can precondition its next write on it before the
// informer cache delivers this one. A reactor stands in for the server's
// bump, which the fake clientset's tracker doesn't perform.
func TestPatchDataReturnsServerResourceVersion(t *testing.T) {
	t.Parallel()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default", ResourceVersion: "7"}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default", ResourceVersion: "7"}}
	tests := []struct {
		name     string
		resource string
		answer   runtime.Object
		call     func(c *Cluster) (string, error)
	}{
		{"secret", "secrets",
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default", ResourceVersion: "8"}},
			func(c *Cluster) (string, error) {
				return c.PatchSecretData(t.Context(), "default", "s", "7", "K", "v", false)
			}},
		{"configmap", "configmaps",
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default", ResourceVersion: "8"}},
			func(c *Cluster) (string, error) {
				return c.PatchConfigMapData(t.Context(), "default", "c", "7", "K", "", true)
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cs := newTestCluster(secret.DeepCopy(), cm.DeepCopy())
			cs.PrependReactor("patch", tt.resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, tt.answer, nil
			})
			rv, err := tt.call(c)
			if err != nil {
				t.Fatalf("patch: %v", err)
			}
			if rv != "8" {
				t.Fatalf("returned resourceVersion = %q, want the server's new 8", rv)
			}
		})
	}
}
