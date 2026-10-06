package fake

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
)

func TestSetImagePatchesNamedContainerOnly(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindDeployment, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app", Image: "app:1.0"},
			{Name: "sidecar", Image: "sidecar:1.0"},
		}}}},
	})

	if err := c.SetImage(t.Context(), kube.KindDeployment, "default", "api", "app", "app:2.0", false); err != nil {
		t.Fatalf("SetImage: %v", err)
	}
	objs, _ := c.ListRaw(t.Context(), kube.KindDeployment, "default")
	deploy := objs[0].(*appsv1.Deployment)
	if deploy.Spec.Template.Spec.Containers[0].Image != "app:2.0" {
		t.Fatalf("app image = %q, want app:2.0", deploy.Spec.Template.Spec.Containers[0].Image)
	}
	if deploy.Spec.Template.Spec.Containers[1].Image != "sidecar:1.0" {
		t.Fatalf("sidecar image = %q, want unchanged sidecar:1.0", deploy.Spec.Template.Spec.Containers[1].Image)
	}
}

func TestSetImagePatchesInitContainers(t *testing.T) {
	t.Parallel()
	restartAlways := corev1.ContainerRestartPolicyAlways
	c := New("default", "dev")
	c.Seed(kube.KindDeployment, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "app:1.0"}},
			InitContainers: []corev1.Container{
				{Name: "prepare", Image: "prepare:1.0"},
				{Name: "mesh", Image: "mesh:1.0", RestartPolicy: &restartAlways},
			},
		}}},
	})

	if err := c.SetImage(t.Context(), kube.KindDeployment, "default", "api", "prepare", "prepare:2.0", true); err != nil {
		t.Fatalf("SetImage conventional init container: %v", err)
	}
	if err := c.SetImage(t.Context(), kube.KindDeployment, "default", "api", "mesh", "mesh:2.0", true); err != nil {
		t.Fatalf("SetImage native sidecar: %v", err)
	}
	objs, _ := c.ListRaw(t.Context(), kube.KindDeployment, "default")
	deploy := objs[0].(*appsv1.Deployment)
	if deploy.Spec.Template.Spec.Containers[0].Image != "app:1.0" {
		t.Fatalf("regular container image = %q, want unchanged app:1.0", deploy.Spec.Template.Spec.Containers[0].Image)
	}
	if deploy.Spec.Template.Spec.InitContainers[0].Image != "prepare:2.0" {
		t.Fatalf("prepare image = %q, want prepare:2.0", deploy.Spec.Template.Spec.InitContainers[0].Image)
	}
	if deploy.Spec.Template.Spec.InitContainers[1].Image != "mesh:2.0" {
		t.Fatalf("mesh image = %q, want mesh:2.0", deploy.Spec.Template.Spec.InitContainers[1].Image)
	}
	if err := c.SetImage(t.Context(), kube.KindDeployment, "default", "api", "prepare", "prepare:3.0", false); err == nil {
		t.Fatal("expected a misclassified init-container target to be rejected")
	}
}

func TestSetImagePatchesCronJobTemplate(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindCronJob, &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"},
		Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "job", Image: "job:1.0"},
				{Name: "sidecar", Image: "sidecar:1.0"},
			}}},
		}}},
	})

	if err := c.SetImage(t.Context(), kube.KindCronJob, "default", "nightly", "job", "job:2.0", false); err != nil {
		t.Fatalf("SetImage: %v", err)
	}
	objs, _ := c.ListRaw(t.Context(), kube.KindCronJob, "default")
	containers := objs[0].(*batchv1.CronJob).Spec.JobTemplate.Spec.Template.Spec.Containers
	if containers[0].Image != "job:2.0" {
		t.Fatalf("job image = %q, want job:2.0", containers[0].Image)
	}
	if containers[1].Image != "sidecar:1.0" {
		t.Fatalf("sidecar image = %q, want unchanged sidecar:1.0", containers[1].Image)
	}
	select {
	case msg := <-c.Events():
		if msg.Kind != kube.KindCronJob {
			t.Fatalf("notify kind = %v, want CronJob", msg.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("expected a KindCronJob ResourceChangedMsg after SetImage")
	}
}

func TestDemoCronJobsSupportSetImage(t *testing.T) {
	t.Parallel()
	c := NewDemo()
	objs, err := c.ListRaw(t.Context(), kube.KindCronJob, "default")
	if err != nil {
		t.Fatalf("ListRaw(CronJob): %v", err)
	}
	if len(objs) == 0 {
		t.Fatal("demo has no CronJobs")
	}
	cronJob := objs[0].(*batchv1.CronJob)
	containers := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		t.Fatalf("demo CronJob %q has no set-image target", cronJob.Name)
	}
	if err := c.SetImage(t.Context(), kube.KindCronJob, cronJob.Namespace, cronJob.Name, containers[0].Name, "busybox:1.37", false); err != nil {
		t.Fatalf("SetImage on demo CronJob: %v", err)
	}
}

func TestSetImageRejectsUnknownContainer(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindDeployment, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1.0"}}}}},
	})
	if err := c.SetImage(t.Context(), kube.KindDeployment, "default", "api", "missing", "app:2.0", false); err == nil {
		t.Fatalf("expected an error for an unknown container name")
	}
}

// TestRolloutUndoPatchesTemplateAndAppendsNewRevision covers 16b's 'R'
// rollback against the fake cluster (--demo mode): RolloutUndo patches the
// Deployment's template to the target revision's, and — mirroring
// HelmRollback's own "rollback creates a new revision" shape — appends a
// synthesized new-highest-revision ReplicaSet so the rail visibly gains a
// new top entry.
func TestRolloutUndoPatchesTemplateAndAppendsNewRevision(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindDeployment, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "api:2.0.0"}},
		}}},
	})
	c.Seed(kube.KindReplicaSet,
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "api-old", Namespace: "default",
				Annotations:     map[string]string{"deployment.kubernetes.io/revision": "4"},
				OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api"}},
			},
			Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api", appsv1.DefaultDeploymentUniqueLabelKey: "old"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "api:1.0.0"}},
				},
			}},
		},
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "api-current", Namespace: "default",
				Annotations:     map[string]string{"deployment.kubernetes.io/revision": "5"},
				OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api"}},
			},
			Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "api:2.0.0"}},
			}}},
		},
	)

	if err := c.RolloutUndo(t.Context(), "default", "api", 4); err != nil {
		t.Fatalf("RolloutUndo: %v", err)
	}

	deployObjs, _ := c.ListRaw(t.Context(), kube.KindDeployment, "default")
	deploy := deployObjs[0].(*appsv1.Deployment)
	if deploy.Spec.Template.Spec.Containers[0].Image != "api:1.0.0" {
		t.Fatalf("expected the deployment's template patched to revision 4's image, got %q", deploy.Spec.Template.Spec.Containers[0].Image)
	}
	if _, ok := deploy.Spec.Template.Labels[appsv1.DefaultDeploymentUniqueLabelKey]; ok {
		t.Fatalf("expected pod-template-hash stripped from the restored template, got labels %v", deploy.Spec.Template.Labels)
	}

	rsObjs, _ := c.ListRaw(t.Context(), kube.KindReplicaSet, "default")
	if len(rsObjs) != 3 {
		t.Fatalf("expected a new synthesized ReplicaSet appended (3 total), got %d", len(rsObjs))
	}
	found := false
	for _, obj := range rsObjs {
		rs := obj.(*appsv1.ReplicaSet)
		if rs.Annotations["deployment.kubernetes.io/revision"] == "6" {
			found = true
			if rs.Spec.Template.Spec.Containers[0].Image != "api:1.0.0" {
				t.Fatalf("expected the new revision's template to carry the rolled-back image, got %q", rs.Spec.Template.Spec.Containers[0].Image)
			}
		}
	}
	if !found {
		t.Fatalf("expected a new revision 6 ReplicaSet among %+v", rsObjs)
	}
}

func TestRolloutUndoRejectsUnknownDeployment(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	if err := c.RolloutUndo(t.Context(), "default", "missing", 1); err == nil {
		t.Fatalf("expected an error for an unknown deployment")
	}
}

func TestListRawFiltersByNamespace(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindPod,
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "other"}},
	)

	all, err := c.ListRaw(t.Context(), kube.KindPod, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("ListRaw(all) = %v, %v, want 2 objects", all, err)
	}
	scoped, err := c.ListRaw(t.Context(), kube.KindPod, "default")
	if err != nil || len(scoped) != 1 {
		t.Fatalf("ListRaw(default) = %v, %v, want 1 object", scoped, err)
	}
}

func TestDeleteResourceRemovesAndNotifies(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindPod, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}})

	if err := c.DeleteResource(t.Context(), kube.KindPod, "default", "a"); err != nil {
		t.Fatalf("DeleteResource: %v", err)
	}
	remaining, _ := c.ListRaw(t.Context(), kube.KindPod, "default")
	if len(remaining) != 0 {
		t.Fatalf("expected pod removed, got %d remaining", len(remaining))
	}
	select {
	case msg := <-c.Events():
		if msg.Kind != kube.KindPod {
			t.Fatalf("notify kind = %v, want Pod", msg.Kind)
		}
	default:
		t.Fatalf("expected a ResourceChangedMsg after delete")
	}
}

func TestDeleteResourceMissingReturnsError(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	if err := c.DeleteResource(t.Context(), kube.KindPod, "default", "missing"); err == nil {
		t.Fatalf("expected an error deleting a nonexistent pod")
	}
}

func TestCordonAndDrainSkipDaemonSetAndMirrorPods(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindNode, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}})
	c.Seed(kube.KindPod,
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}, Spec: corev1.PodSpec{NodeName: "node-1"}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "ds", Namespace: "default", OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet"}}},
			Spec:       corev1.PodSpec{NodeName: "node-1"},
		},
	)

	evicted, err := c.Drain(t.Context(), "node-1")
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if evicted != 1 {
		t.Fatalf("evicted = %d, want 1", evicted)
	}
	remaining, _ := c.ListRaw(t.Context(), kube.KindPod, "default")
	if len(remaining) != 1 {
		t.Fatalf("expected the DaemonSet pod to survive, got %d pods remaining", len(remaining))
	}
}

// TestTriggerCronJobStampsManualAnnotationsAndNotifies pins Plan Phase 2
// task 13's fake/live parity for TriggerCronJob: the same source/creator/
// time annotations kube.Cluster's real implementation stamps, an ownerless
// standalone Job, and a KindJob change notification (Phase 2 test 9).
func TestTriggerCronJobStampsManualAnnotationsAndNotifies(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindCronJob, &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", UID: types.UID("cj-uid-1")},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 2 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "batch"}},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1.0"}}},
					},
				},
			},
		},
	})

	at := time.Date(2026, 3, 4, 2, 30, 0, 0, time.UTC)
	if err := c.TriggerCronJob(t.Context(), "default", "nightly", "nightly-manual-0230", "operator@example.com", at); err != nil {
		t.Fatalf("TriggerCronJob: %v", err)
	}

	objs, _ := c.ListRaw(t.Context(), kube.KindJob, "default")
	if len(objs) != 1 {
		t.Fatalf("expected one triggered Job, got %d", len(objs))
	}
	job := objs[0].(*batchv1.Job)
	if len(job.OwnerReferences) != 0 {
		t.Errorf("expected the triggered job detached (no OwnerReferences), got %+v", job.OwnerReferences)
	}
	if job.Annotations[kube.AnnotationCronJobName] != "nightly" {
		t.Errorf("AnnotationCronJobName = %q, want %q", job.Annotations[kube.AnnotationCronJobName], "nightly")
	}
	if job.Annotations[kube.AnnotationCronJobUID] != "cj-uid-1" {
		t.Errorf("AnnotationCronJobUID = %q, want %q", job.Annotations[kube.AnnotationCronJobUID], "cj-uid-1")
	}
	if job.Annotations[kube.AnnotationTriggeredBy] != "operator@example.com" {
		t.Errorf("AnnotationTriggeredBy = %q, want %q", job.Annotations[kube.AnnotationTriggeredBy], "operator@example.com")
	}
	if job.Annotations[kube.AnnotationTriggeredAt] != "2026-03-04T02:30:00Z" {
		t.Errorf("AnnotationTriggeredAt = %q, want %q", job.Annotations[kube.AnnotationTriggeredAt], "2026-03-04T02:30:00Z")
	}
	if job.Annotations["cronjob.kubernetes.io/instantiate"] != "manual" {
		t.Errorf("expected the manual-instantiate annotation, got %+v", job.Annotations)
	}

	select {
	case msg := <-c.Events():
		if msg.Kind != kube.KindJob {
			t.Fatalf("notify kind = %v, want Job", msg.Kind)
		}
	default:
		t.Fatalf("expected a KindJob ResourceChangedMsg after TriggerCronJob")
	}
}

// TestTriggerCronJobDeepCopiesTemplate pins Phase 2 test 1: unlike
// kube.Cluster's own equivalent test, this one is meaningful precisely
// because kube/fake.Cluster has no client-go serialization boundary between
// Create and Get — objects are stored as the same in-memory pointers that
// were passed in, so an aliased (not cloned) Labels map or Spec would be
// directly observable by mutating the returned Job and re-reading the
// source CronJob's own jobTemplate.
func TestTriggerCronJobDeepCopiesTemplate(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindCronJob, &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 2 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "batch"}},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1.0", Env: []corev1.EnvVar{{Name: "X", Value: "1"}}}}},
					},
				},
			},
		},
	})

	if err := c.TriggerCronJob(t.Context(), "default", "nightly", "nightly-manual-0230", "op", time.Now()); err != nil {
		t.Fatalf("TriggerCronJob: %v", err)
	}
	objs, _ := c.ListRaw(t.Context(), kube.KindJob, "default")
	job := objs[0].(*batchv1.Job)

	// Mutate the created Job's own copies...
	job.Labels["app"] = "mutated"
	job.Spec.Template.Spec.Containers[0].Image = "mutated:2.0"
	job.Spec.Template.Spec.Containers[0].Env[0].Value = "mutated"

	// ...and confirm the source CronJob's own template is untouched.
	cjObjs, _ := c.ListRaw(t.Context(), kube.KindCronJob, "default")
	tpl := cjObjs[0].(*batchv1.CronJob).Spec.JobTemplate
	if tpl.Labels["app"] != "batch" {
		t.Errorf("source template Labels mutated through the created Job's own copy: %+v", tpl.Labels)
	}
	if tpl.Spec.Template.Spec.Containers[0].Image != "app:1.0" {
		t.Errorf("source template container image mutated through the created Job's own copy: %q", tpl.Spec.Template.Spec.Containers[0].Image)
	}
	if tpl.Spec.Template.Spec.Containers[0].Env[0].Value != "1" {
		t.Errorf("source template container env mutated through the created Job's own copy: %q", tpl.Spec.Template.Spec.Containers[0].Env[0].Value)
	}
}

// TestTriggerCronJobRejectsDuplicateName pins task 13's "fake create rejects
// duplicate names" requirement and task 14's restage seam: the error wraps
// kube.ErrManualJobNameConflict just like a real AlreadyExists race would.
func TestTriggerCronJobRejectsDuplicateName(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindCronJob, &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"}})
	c.Seed(kube.KindJob, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "nightly-manual-0230", Namespace: "default"}})

	err := c.TriggerCronJob(t.Context(), "default", "nightly", "nightly-manual-0230", "op", time.Now())
	if err == nil {
		t.Fatal("expected an error for a name already taken")
	}
	if !errors.Is(err, kube.ErrManualJobNameConflict) {
		t.Fatalf("expected errors.Is(err, kube.ErrManualJobNameConflict), got %v", err)
	}
}

// TestSetCronJobSuspendStampsAndClearsAnnotations pins task 13's suspend/
// resume parity: annotations stamped atomically with spec.suspend on
// suspend, cleared on resume, resourceVersion/generation bumped either way.
func TestSetCronJobSuspendStampsAndClearsAnnotations(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindCronJob, &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", ResourceVersion: "1", Generation: 3},
	})

	at := time.Date(2026, 5, 1, 9, 30, 0, 0, time.UTC)
	if err := c.SetCronJobSuspend(t.Context(), "default", "nightly", true, "1", 3, at); err != nil {
		t.Fatalf("SetCronJobSuspend(true): %v", err)
	}
	objs, _ := c.ListRaw(t.Context(), kube.KindCronJob, "default")
	cj := objs[0].(*batchv1.CronJob)
	if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
		t.Fatalf("Spec.Suspend = %v, want true", cj.Spec.Suspend)
	}
	if cj.Annotations[kube.AnnotationSuspendedAt] != "2026-05-01T09:30:00Z" {
		t.Errorf("AnnotationSuspendedAt = %q, want %q", cj.Annotations[kube.AnnotationSuspendedAt], "2026-05-01T09:30:00Z")
	}
	if cj.Annotations[kube.AnnotationSuspendedGeneration] != "4" {
		t.Errorf("AnnotationSuspendedGeneration = %q, want %q", cj.Annotations[kube.AnnotationSuspendedGeneration], "4")
	}
	if cj.Generation != 4 {
		t.Errorf("Generation = %d, want 4 (bumped by the suspend patch)", cj.Generation)
	}
	suspendedRV := cj.ResourceVersion
	if suspendedRV == "1" {
		t.Errorf("expected resourceVersion bumped past the original %q", "1")
	}

	if err := c.SetCronJobSuspend(t.Context(), "default", "nightly", false, suspendedRV, 4, time.Time{}); err != nil {
		t.Fatalf("SetCronJobSuspend(false): %v", err)
	}
	objs, _ = c.ListRaw(t.Context(), kube.KindCronJob, "default")
	cj = objs[0].(*batchv1.CronJob)
	if cj.Spec.Suspend == nil || *cj.Spec.Suspend {
		t.Fatalf("Spec.Suspend after resume = %v, want false", cj.Spec.Suspend)
	}
	if _, ok := cj.Annotations[kube.AnnotationSuspendedAt]; ok {
		t.Errorf("expected AnnotationSuspendedAt cleared on resume, got %+v", cj.Annotations)
	}
	if _, ok := cj.Annotations[kube.AnnotationSuspendedGeneration]; ok {
		t.Errorf("expected AnnotationSuspendedGeneration cleared on resume, got %+v", cj.Annotations)
	}
	if cj.ResourceVersion == suspendedRV {
		t.Errorf("expected resourceVersion bumped again on resume, still %q", cj.ResourceVersion)
	}
}

// TestSetCronJobSuspendConflictsOnStaleResourceVersion pins task 13's
// "conflicts are testable" requirement: a stale resourceVersion precondition
// returns a typed Conflict error, and the CronJob is left untouched.
func TestSetCronJobSuspendConflictsOnStaleResourceVersion(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindCronJob, &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", ResourceVersion: "5", Generation: 1},
	})

	err := c.SetCronJobSuspend(t.Context(), "default", "nightly", true, "stale", 1, time.Now())
	if err == nil {
		t.Fatal("expected an error for a stale resourceVersion")
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected apierrors.IsConflict(err), got %v", err)
	}
	objs, _ := c.ListRaw(t.Context(), kube.KindCronJob, "default")
	cj := objs[0].(*batchv1.CronJob)
	if cj.Spec.Suspend != nil {
		t.Errorf("expected Spec.Suspend untouched after a conflict, got %v", cj.Spec.Suspend)
	}
	if cj.ResourceVersion != "5" {
		t.Errorf("expected ResourceVersion untouched after a conflict, got %q", cj.ResourceVersion)
	}
}

// TestSetCronJobScheduleConflictsAgainstFakeCluster is the fake-cluster
// counterpart to mutate_test.go's TestSetCronJobSchedulePropagatesConflict —
// this one exercises kube/fake's own resourceVersion enforcement rather
// than a reactor standing in for one, so a caller can drive a schedule-
// editor Conflict path (§36d) entirely against --demo/task tests.
func TestSetCronJobScheduleConflictsAgainstFakeCluster(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindCronJob, &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", ResourceVersion: "5"},
		Spec:       batchv1.CronJobSpec{Schedule: "0 2 * * *"},
	})

	_, err := c.SetCronJobSchedule(t.Context(), "default", "nightly", kube.CronJobScheduleEdit{
		Schedule: "*/15 * * * *", ResourceVersion: "stale",
	})
	if err == nil {
		t.Fatal("expected an error for a stale resourceVersion")
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected apierrors.IsConflict(err), got %v", err)
	}
	objs, _ := c.ListRaw(t.Context(), kube.KindCronJob, "default")
	cj := objs[0].(*batchv1.CronJob)
	if cj.Spec.Schedule != "0 2 * * *" {
		t.Errorf("expected Spec.Schedule untouched after a conflict, got %q", cj.Spec.Schedule)
	}
}

// TestSetCronJobScheduleSetsAndClearsTimeZone is the fake-cluster
// counterpart to mutate_test.go's TestSetCronJobScheduleSetsAndClearsTimeZone
// — same set/clear/untouched contract, plus the returned
// kube.CronJobScheduleResult and a KindCronJob notification each time
// (Phase 2 test 9).
func TestSetCronJobScheduleSetsAndClearsTimeZone(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindCronJob, &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", ResourceVersion: "1"},
		Spec:       batchv1.CronJobSpec{Schedule: "0 2 * * *"},
	})

	tz := "America/New_York"
	result, err := c.SetCronJobSchedule(t.Context(), "default", "nightly", kube.CronJobScheduleEdit{
		Schedule: "0 2 * * *", TimeZone: &tz, ResourceVersion: "1",
	})
	if err != nil {
		t.Fatalf("SetCronJobSchedule (set timezone): %v", err)
	}
	if result.TimeZone != tz {
		t.Errorf("result.TimeZone = %q, want %q", result.TimeZone, tz)
	}
	select {
	case msg := <-c.Events():
		if msg.Kind != kube.KindCronJob {
			t.Fatalf("notify kind = %v, want CronJob", msg.Kind)
		}
	default:
		t.Fatalf("expected a KindCronJob ResourceChangedMsg after SetCronJobSchedule")
	}

	// A nil TimeZone (schedule-only edit) must never touch the existing
	// value.
	result2, err := c.SetCronJobSchedule(t.Context(), "default", "nightly", kube.CronJobScheduleEdit{
		Schedule: "*/30 * * * *", ResourceVersion: result.ResourceVersion,
	})
	if err != nil {
		t.Fatalf("SetCronJobSchedule (schedule only): %v", err)
	}
	if result2.TimeZone != tz {
		t.Errorf("result2.TimeZone = %q, want the untouched %q", result2.TimeZone, tz)
	}

	// An explicit clear removes it.
	emptyTZ := ""
	result3, err := c.SetCronJobSchedule(t.Context(), "default", "nightly", kube.CronJobScheduleEdit{
		Schedule: "*/30 * * * *", TimeZone: &emptyTZ, ResourceVersion: result2.ResourceVersion,
	})
	if err != nil {
		t.Fatalf("SetCronJobSchedule (clear timezone): %v", err)
	}
	if result3.TimeZone != "" {
		t.Errorf("result3.TimeZone = %q, want cleared \"\"", result3.TimeZone)
	}
}

func TestGetYAMLStripsManagedFields(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindPod, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api", Namespace: "default", ResourceVersion: "7",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
	})

	yaml, rv, err := c.GetYAML(t.Context(), kube.KindPod, "default", "api")
	if err != nil {
		t.Fatalf("GetYAML: %v", err)
	}
	if rv != "7" {
		t.Fatalf("resourceVersion = %q, want 7", rv)
	}
	if strings.Contains(yaml, "managedFields") {
		t.Fatalf("expected managedFields stripped from yaml:\n%s", yaml)
	}
	if !strings.Contains(yaml, "name: api") {
		t.Fatalf("expected the pod name in the yaml:\n%s", yaml)
	}
}

func TestObjectEventsFiltersByInvolvedObject(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindEvent,
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "ev-1", Namespace: "default"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "api"},
			Reason:         "BackOff",
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "ev-2", Namespace: "default"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "other"},
		},
	)

	got, err := c.ObjectEvents(t.Context(), "default", kube.KindPod, "api")
	if err != nil {
		t.Fatalf("ObjectEvents: %v", err)
	}
	if len(got) != 1 || got[0].Reason != "BackOff" {
		t.Fatalf("ObjectEvents = %+v, want one BackOff event", got)
	}
}

// The fake projects events exactly as the real cluster does: an
// events.k8s.io-native event's last occurrence lives in Series, not in the
// legacy LastTimestamp/Count, which it never sets.
func TestNamespaceEventsReadSeriesLikeTheRealCluster(t *testing.T) {
	t.Parallel()
	first := time.Now().Add(-3 * time.Hour)
	last := time.Now().Add(-time.Minute)
	c := New("default", "dev")
	c.Seed(kube.KindEvent, &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "ev-series", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "api"},
		Reason:         "BackOff",
		EventTime:      metav1.NewMicroTime(first),
		Series:         &corev1.EventSeries{Count: 42, LastObservedTime: metav1.NewMicroTime(last)},
	})

	got, err := c.NamespaceEvents(t.Context(), "default")
	if err != nil {
		t.Fatalf("NamespaceEvents: %v", err)
	}
	if len(got) != 1 || got[0].Count != 42 || !got[0].LastSeen.Equal(last) {
		t.Fatalf("NamespaceEvents = %+v, want Count 42 last seen at the series' last observation", got)
	}
}

func TestSwitchNamespaceAndContext(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.AddContext("prod", "prod-ns")

	c.SwitchNamespace("kube-system")
	if c.CurrentNamespace() != "kube-system" {
		t.Fatalf("CurrentNamespace = %q, want kube-system", c.CurrentNamespace())
	}

	if err := c.SwitchContext(t.Context(), "prod"); err != nil {
		t.Fatalf("SwitchContext: %v", err)
	}
	if c.CurrentContext() != "prod" || c.CurrentNamespace() != "prod-ns" {
		t.Fatalf("after SwitchContext: context=%q namespace=%q, want prod/prod-ns", c.CurrentContext(), c.CurrentNamespace())
	}

	if err := c.SwitchContext(t.Context(), "does-not-exist"); err == nil {
		t.Fatalf("expected an error switching to an unknown context")
	}
}

func TestStreamPodLogsReplaysSeededLines(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.SeedLogs("default", "api", []string{"line one", "line two"})

	rc, err := c.StreamPodLogs(t.Context(), kube.LogStreamRequest{Namespace: "default", PodName: "api"})
	if err != nil {
		t.Fatalf("StreamPodLogs: %v", err)
	}
	defer func() { _ = rc.Close() }()
	buf := make([]byte, 1024)
	n, _ := rc.Read(buf)
	got := string(buf[:n])
	if !strings.Contains(got, "line one") || !strings.Contains(got, "line two") {
		t.Fatalf("streamed content = %q, want both seeded lines", got)
	}
}

func TestSetConnStateEmitsOnChannel(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.SetConnState(kube.ConnState{Phase: kube.ConnReconnecting, Err: "dial timeout"})

	if got := c.ConnState().Phase; got != kube.ConnReconnecting {
		t.Fatalf("ConnState().Phase = %v, want Reconnecting", got)
	}
	select {
	case msg := <-c.ConnEvents():
		if msg.Phase != kube.ConnReconnecting {
			t.Fatalf("ConnEvents phase = %v, want Reconnecting", msg.Phase)
		}
	default:
		t.Fatalf("expected a ConnStateMsg after SetConnState")
	}
}

func TestNewDemoIsFeatureComplete(t *testing.T) {
	t.Parallel()
	c := NewDemo()
	ctx := t.Context()

	pods, err := c.ListRaw(ctx, kube.KindPod, "")
	if err != nil || len(pods) != 35 {
		t.Fatalf("ListRaw(Pod) = %d, %v, want 35 fixture pods", len(pods), err)
	}
	if pod, ok := findPod(pods, "api-7d9f6c8-abcde"); !ok || len(pod.Spec.Containers) < 2 {
		t.Fatalf("expected a multi-container pod (10a's exec-picker is otherwise unreachable in --demo), got %+v (ok=%v)", pod, ok)
	}
	deploys, _ := c.ListRaw(ctx, kube.KindDeployment, "")
	if len(deploys) != 16 {
		t.Fatalf("expected 16 deployment fixtures, got %d", len(deploys))
	}
	nodes, _ := c.ListRaw(ctx, kube.KindNode, "")
	if len(nodes) != 4 {
		t.Fatalf("expected 4 node fixtures, got %d", len(nodes))
	}
	namespaces, _ := c.ListRaw(ctx, kube.KindNamespace, "")
	if len(namespaces) != 10 {
		t.Fatalf("expected 10 namespace fixtures, got %d", len(namespaces))
	}
	events, err := c.ObjectEvents(ctx, "default", kube.KindPod, "worker-0")
	if err != nil || len(events) == 0 {
		t.Fatalf("expected events for the crashlooping pod, got %d, %v", len(events), err)
	}
	nsEvents, err := c.NamespaceEvents(ctx, "default")
	if err != nil || len(nsEvents) != len(events) {
		t.Fatalf("NamespaceEvents(default) = %d, %v, want %d", len(nsEvents), err, len(events))
	}

	// Every consumer seam must at least be callable without panicking.
	podMetrics, err := c.PodMetricsByNamespace(ctx, "default")
	if err != nil {
		t.Fatalf("PodMetricsByNamespace: %v", err)
	}
	if pm, ok := podMetrics[kube.PodKey("default", "api-7d9f6c8-abcde")]; !ok || pm.CPU == "n/a" || pm.CPUMilli == 0 {
		t.Fatalf("expected real (non-n/a) CPU usage for a Running pod, got %+v (ok=%v)", pm, ok)
	}
	if nm, err := c.NodeMetrics(ctx); err != nil || len(nm) != 4 {
		t.Fatalf("NodeMetrics = %d, %v, want 4 fixture nodes", len(nm), err)
	}
	if _, _, err := c.GetYAML(ctx, kube.KindPod, "default", "worker-0"); err != nil {
		t.Fatalf("GetYAML: %v", err)
	}
	if _, err := c.StreamPodLogs(ctx, kube.LogStreamRequest{Namespace: "default", PodName: "worker-0"}); err != nil {
		t.Fatalf("StreamPodLogs: %v", err)
	}
}

// TestNewDemoCoversEveryCronJobOutcome pins 0.8.0 plan Phase 8 task 7: five
// CronJobs, each exercising one scenario §36a-§36e need real data for. It
// asserts through resources.BuildCronJobSummaries — the same aggregation
// browse/cronjobdetail call — rather than re-deriving the outcomes by hand,
// so a fixture regression here is exactly the join a real screen would get
// wrong.
func TestNewDemoCoversEveryCronJobOutcome(t *testing.T) {
	t.Parallel()
	c := NewDemo()
	ctx := t.Context()

	cronJobs, err := c.ListRaw(ctx, kube.KindCronJob, "")
	if err != nil || len(cronJobs) != 5 {
		t.Fatalf("ListRaw(CronJob) = %d, %v, want 5 fixture cronjobs", len(cronJobs), err)
	}
	jobs, err := c.ListRaw(ctx, kube.KindJob, "")
	if err != nil {
		t.Fatalf("ListRaw(Job): %v", err)
	}
	pods, err := c.ListRaw(ctx, kube.KindPod, "")
	if err != nil {
		t.Fatalf("ListRaw(Pod): %v", err)
	}

	summaries := resources.BuildCronJobSummaries(cronJobs, jobs, pods)
	byName := map[string]resources.CronJobSummary{}
	for _, s := range summaries {
		byName[s.Object.Name] = s
	}

	// nightly-backup: successful latest run, plus the manual/unrelated/
	// stale-owner Jobs must resolve to exactly the two real scheduled runs
	// — never 5 (which would mean the unrelated or stale-owner Job leaked
	// in) and never 3 (which would mean the manual Job's annotation-based
	// association was missed).
	nb := byName["nightly-backup"]
	if len(nb.Runs) != 3 {
		t.Fatalf("nightly-backup Runs = %d, want 3 (2 scheduled + 1 manual)", len(nb.Runs))
	}
	if nb.LastTerminal == nil || !nb.LastTerminal.Succeeded || nb.LastTerminal.Name != "nightly-backup-29070200" {
		t.Fatalf("nightly-backup LastTerminal = %+v, want the newer succeeded run", nb.LastTerminal)
	}
	var sawManual bool
	for _, r := range nb.Runs {
		if r.Source == resources.JobSourceManual {
			sawManual = true
			if r.Name != "nightly-backup-manual-1430" {
				t.Fatalf("unexpected manual run %q", r.Name)
			}
		}
	}
	if !sawManual {
		t.Fatal("expected nightly-backup's manual annotated Job to associate")
	}

	// hourly-report: suspended by Kute, with a trustworthy SuspendedAt.
	hr := byName["hourly-report"]
	if hr.SuspendedAt == nil {
		t.Fatal("hourly-report: expected a validated SuspendedAt (Kute-stamped annotations, unchanged generation)")
	}

	// nightly-cleanup: failed latest run, Job condition authoritative,
	// exit code supplied by the associated Pod.
	nc := byName["nightly-cleanup"]
	if nc.LastTerminal == nil || !nc.LastTerminal.Failed || nc.LastTerminal.Reason != "BackoffLimitExceeded" {
		t.Fatalf("nightly-cleanup LastTerminal = %+v, want a Failed run with the Job's own reason", nc.LastTerminal)
	}
	if nc.LastTerminal.ExitCode == nil || *nc.LastTerminal.ExitCode != 1 {
		t.Fatalf("nightly-cleanup LastTerminal.ExitCode = %v, want 1 from the associated Pod", nc.LastTerminal.ExitCode)
	}
	var cleanupJob *batchv1.Job
	for _, obj := range jobs {
		if job, ok := obj.(*batchv1.Job); ok && job.Name == "nightly-cleanup-29070330" {
			cleanupJob = job
			break
		}
	}
	attempts, ok := resources.BuildJobAttempts(cleanupJob, pods)
	if !ok || len(attempts.Attempts) != 3 {
		t.Fatalf("nightly-cleanup attempts = %d, ok=%v, want 3 retained attempts", len(attempts.Attempts), ok)
	}
	for _, attempt := range attempts.Attempts {
		if attempt.StartedAt.IsZero() || attempt.EndedAt.IsZero() {
			t.Fatalf("nightly-cleanup attempt %+v has no duration", attempt)
		}
	}

	// metrics-rollup: an active run alongside a prior succeeded terminal
	// run — LAST RUN must still name the terminal run, not the active one.
	mr := byName["metrics-rollup"]
	if len(mr.ActiveRuns) != 1 {
		t.Fatalf("metrics-rollup ActiveRuns = %d, want 1", len(mr.ActiveRuns))
	}
	if mr.LastTerminal == nil || !mr.LastTerminal.Succeeded || mr.LastTerminal.Name != "metrics-rollup-29071315" {
		t.Fatalf("metrics-rollup LastTerminal = %+v, want the prior succeeded run despite the active one", mr.LastTerminal)
	}

	// weekly-digest: an explicit timezone and no retained runs at all.
	wd := byName["weekly-digest"]
	if wd.Object.Spec.TimeZone == nil || *wd.Object.Spec.TimeZone != "America/New_York" {
		t.Fatalf("weekly-digest Spec.TimeZone = %v, want America/New_York", wd.Object.Spec.TimeZone)
	}
	if len(wd.Runs) != 0 {
		t.Fatalf("weekly-digest Runs = %d, want 0 (the no-retained-runs exemplar)", len(wd.Runs))
	}
}

// TestPodAndNodeMetricsAreDeterministicNotNA pins the §6a/CLAUDE.md
// feature-completeness fix: --demo's CPU/MEM columns used to be
// unconditionally "n/a" for every pod and node, so those bars/columns could
// never actually be exercised by driving --demo mode. Usage is now
// synthesized from each object's own limits/allocatable, and must be the
// same across repeated calls (a stable demo, not flickering random noise).
func TestPodAndNodeMetricsAreDeterministicNotNA(t *testing.T) {
	t.Parallel()
	c := NewDemo()
	ctx := t.Context()

	first, err := c.PodMetricsByNamespace(ctx, "default")
	if err != nil {
		t.Fatalf("PodMetricsByNamespace: %v", err)
	}
	pm, ok := first[kube.PodKey("default", "api-7d9f6c8-abcde")]
	if !ok {
		t.Fatal("expected api-7d9f6c8-abcde in the fixture set")
	}
	if pm.CPU == "n/a" || pm.MEM == "n/a" || pm.CPUMilli <= 0 || pm.MemBytes <= 0 {
		t.Fatalf("expected real usage for a Running pod, got %+v", pm)
	}

	second, _ := c.PodMetricsByNamespace(ctx, "default")
	if second[kube.PodKey("default", "api-7d9f6c8-abcde")] != pm {
		t.Fatalf("expected deterministic usage across calls, got %+v then %+v", pm, second[kube.PodKey("default", "api-7d9f6c8-abcde")])
	}

	nodeMetrics, err := c.NodeMetrics(ctx)
	if err != nil {
		t.Fatalf("NodeMetrics: %v", err)
	}
	for name, nm := range nodeMetrics {
		if nm.CPU == "n/a" || nm.MEM == "n/a" || nm.CPUMilli <= 0 || nm.MemBytes <= 0 {
			t.Fatalf("expected real usage for node %q, got %+v", name, nm)
		}
	}
}

// findPod locates name among objs (runtime.Object values from ListRaw),
// returning the underlying *corev1.Pod.
// TestDemoCoversBothPortForwardShapes pins the two halves of 13a's picker are
// both reachable in --demo, which is the only way anyone sees the feature
// without a cluster of their own: the trailing free-port row needs a pod that
// declares nothing at all, and the declared-port list needs one with more
// than a single row. It asserts through kube.ForwardablePorts — the function
// the picker actually calls — rather than counting ContainerPorts itself, and
// through the Deployment template too, since `f` offers its ports rather than
// the pod's.
func TestDemoCoversBothPortForwardShapes(t *testing.T) {
	t.Parallel()
	c := NewDemo()
	ctx := t.Context()
	pods, err := c.ListRaw(ctx, kube.KindPod, "default")
	if err != nil {
		t.Fatalf("ListRaw(Pod): %v", err)
	}

	// The portless subject: `f` opens straight onto `+ other port`, the state
	// that used to be unreachable and that a pod listening on an undeclared
	// port is the real-world reason for.
	portless, ok := findPod(pods, "gateway-6f9c8d7b4-h2n8k")
	if !ok {
		t.Fatal("gateway-6f9c8d7b4-h2n8k is not a --demo fixture pod")
	}
	if got := kube.ForwardablePorts(portless); len(got) != 0 {
		t.Fatalf("gateway's ports = %+v, want none — it is --demo's free-port subject", got)
	}

	// The multi-port subject: three rows, and a spread of numbers so the
	// local-port pre-fill makes three different decisions.
	multi, ok := findPod(pods, "api-7d9f6c8-abcde")
	if !ok {
		t.Fatal("api-7d9f6c8-abcde is not a --demo fixture pod")
	}
	ports := kube.ForwardablePorts(multi)
	if len(ports) != 3 {
		t.Fatalf("api's forwardable ports = %+v, want three", ports)
	}
	want := map[int32]bool{8080: false, 8081: false, 9090: false}
	for _, p := range ports {
		if _, ok := want[p.Port]; !ok {
			t.Fatalf("unexpected port %d in %+v", p.Port, ports)
		}
		want[p.Port] = true
		if p.Container != "api" {
			t.Fatalf("port %d = container %q, want the pod's api container", p.Port, p.Container)
		}
	}

	// And the Deployment agrees with its own Pod, so `f` on either offers the
	// same list rather than the two fixtures drifting apart.
	deploys, err := c.ListRaw(ctx, kube.KindDeployment, "default")
	if err != nil {
		t.Fatalf("ListRaw(Deployment): %v", err)
	}
	for _, obj := range deploys {
		dep, ok := obj.(*appsv1.Deployment)
		if !ok || dep.Name != "api" {
			continue
		}
		depPorts := kube.ForwardablePorts(dep)
		if len(depPorts) != len(ports) {
			t.Fatalf("api Deployment's ports = %+v, want the same %d its pod declares", depPorts, len(ports))
		}
		return
	}
	t.Fatal("the api Deployment is not a --demo fixture")
}

func findPod(objs []runtime.Object, name string) (*corev1.Pod, bool) {
	for _, obj := range objs {
		if pod, ok := obj.(*corev1.Pod); ok && pod.Name == name {
			return pod, true
		}
	}
	return nil, false
}

// TestDemoDiscoveredKindsCarryTheirOwnAPIGroup guards the fix for a fixture
// bug that reached the screen: demoDiscoveredKind hardcoded cert-manager.io
// as every demo kind's Group, so in --demo a ServiceMonitor's 14a breadcrumb
// tag, its 14c goto type label and its descriptor's "custom resource · …"
// Describe all claimed it was a cert-manager type. Each demo kind must
// report the group its own CRD declares.
func TestDemoDiscoveredKindsCarryTheirOwnAPIGroup(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"Certificate":    "cert-manager.io",
		"ServiceMonitor": "monitoring.coreos.com",
		"Prometheus":     "monitoring.coreos.com",
		"Application":    "argoproj.io",
		"HTTPRoute":      "gateway.networking.k8s.io",
	}
	got := map[string]string{}
	for _, dk := range NewDemo().DiscoveredKinds() {
		got[dk.Kind] = dk.Group
		// The GVR the dynamic client would use has to agree with it, or a
		// read of this kind goes to the wrong endpoint entirely.
		if dk.GVR.Group != dk.Group {
			t.Errorf("%s: GVR group %q disagrees with Group %q", dk.Kind, dk.GVR.Group, dk.Group)
		}
	}
	for kind, group := range want {
		if got[kind] != group {
			t.Errorf("%s: got group %q, want %q", kind, got[kind], group)
		}
	}
}

// Compile-time interface satisfaction: the fake must actually implement
// kube.Mutator, the only formally exported multi-method consumer contract
// it stands in for today.
var _ kube.Mutator = (*Cluster)(nil)

// TestDemoSeedsFluxKinds guards --demo's §30a fixtures. The demo cluster is
// the only way to exercise the Flux screens without a live Flux install, so
// a fixture that silently stops being seeded takes the screens with it.
func TestDemoSeedsFluxKinds(t *testing.T) {
	t.Parallel()
	c := NewDemo()
	ctx := t.Context()

	for kind, wantMin := range map[kube.ResourceKind]int{
		"Kustomization":          5,
		kube.KindFluxHelmRelease: 2,
		"GitRepository":          1,
	} {
		objs, err := c.ListRaw(ctx, kind, "flux-system")
		if err != nil {
			t.Fatalf("ListRaw(%s): %v", kind, err)
		}
		if len(objs) < wantMin {
			t.Errorf("%s: seeded %d objects, want at least %d", kind, len(objs), wantMin)
		}
	}

	// Flux's HelmReleases must be seeded under the substituted registry
	// kind. Seeding the bare "HelmRelease" would route them into §18a's
	// Helm-3 list — the exact collision §30a exists to prevent.
	if objs, _ := c.ListRaw(ctx, kube.ResourceKind("HelmRelease"), "flux-system"); len(objs) != 0 {
		t.Errorf("Flux HelmReleases must not be seeded under the bare API Kind, found %d", len(objs))
	}

	// Every branch of §30a's status precedence needs a fixture, or the demo
	// can't show what the feature is for.
	var suspended, stalled, reconciling bool
	objs, _ := c.ListRaw(ctx, kube.ResourceKind("Kustomization"), "flux-system")
	for _, o := range objs {
		u := o.(*unstructured.Unstructured)
		if v, _, _ := unstructured.NestedBool(u.Object, "spec", "suspend"); v {
			suspended = true
		}
		conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		for _, cd := range conds {
			cm := cd.(map[string]any)
			typ, _, _ := unstructured.NestedString(cm, "type")
			st, _, _ := unstructured.NestedString(cm, "status")
			if typ == "Reconciling" && st == "True" {
				reconciling = true
			}
		}
	}
	hrs, _ := c.ListRaw(ctx, kube.KindFluxHelmRelease, "flux-system")
	for _, o := range hrs {
		conds, _, _ := unstructured.NestedSlice(o.(*unstructured.Unstructured).Object, "status", "conditions")
		for _, cd := range conds {
			cm := cd.(map[string]any)
			typ, _, _ := unstructured.NestedString(cm, "type")
			st, _, _ := unstructured.NestedString(cm, "status")
			if typ == "Stalled" && st == "True" {
				stalled = true
			}
		}
	}
	if !suspended || !stalled || !reconciling {
		t.Errorf("demo must cover every status branch: suspended=%v stalled=%v reconciling=%v",
			suspended, stalled, reconciling)
	}
}

// TestDemoFluxEventsCarryTheRevisionAnnotation pins the fake's event
// readers: §32a reads the revision from metadata.annotations, so a reader
// that drops them makes every revision row disappear under the fake while
// the real cluster still works.
func TestDemoFluxEventsCarryTheRevisionAnnotation(t *testing.T) {
	t.Parallel()
	c := NewDemo()
	events, err := c.NamespaceEvents(t.Context(), "flux-system")
	if err != nil {
		t.Fatalf("NamespaceEvents: %v", err)
	}
	var withRevision, withSubject int
	for _, e := range events {
		if kube.FluxEventRevision(e) != "" {
			withRevision++
		}
		if kube.FluxCommitSubject(e.Message) != "" {
			withSubject++
		}
	}
	if withRevision < 2 {
		t.Errorf("expected at least 2 events carrying a Flux revision annotation, got %d", withRevision)
	}
	if withSubject < 1 {
		t.Errorf("expected a source-controller event carrying a commit subject, got %d", withSubject)
	}
}

// TestFakeKindSyncedIsScopedPerNamespace: SetKindSynced/SetKindForbidden for
// one namespace must not answer for another, or a scoped-mode UI test that
// asks the wrong namespace by mistake would get the right-looking answer
// anyway and never fail — the fake's job here is to be as strict as a real
// Cluster's own (kind, namespace) health keying.
func TestFakeKindSyncedIsScopedPerNamespace(t *testing.T) {
	t.Parallel()
	c := New("team-a", "dev")

	c.SetKindSynced(kube.KindPod, "team-a", false)
	if c.KindSynced(kube.KindPod, "team-a") {
		t.Error("KindSynced(Pod, team-a) = true after SetKindSynced(Pod, team-a, false)")
	}
	if !c.KindSynced(kube.KindPod, "team-b") {
		t.Error("SetKindSynced(Pod, team-a, false) wrongly poisoned KindSynced(Pod, team-b)")
	}
	if !c.KindSynced(kube.KindPod, "") {
		t.Error("SetKindSynced(Pod, team-a, false) wrongly poisoned the cluster-wide KindSynced(Pod, \"\") cache")
	}
}

// TestFakeKindForbiddenIsScopedPerNamespace mirrors the sync case for
// KindForbidden/SetKindForbidden.
func TestFakeKindForbiddenIsScopedPerNamespace(t *testing.T) {
	t.Parallel()
	c := New("team-a", "dev")

	denied := errors.New("forbidden")
	c.SetKindForbidden(kube.KindSecret, "team-a", denied)
	if c.KindForbidden(kube.KindSecret, "team-a") == nil {
		t.Error("KindForbidden(Secret, team-a) = nil after SetKindForbidden(Secret, team-a, err)")
	}
	if c.KindForbidden(kube.KindSecret, "team-b") != nil {
		t.Error("SetKindForbidden(Secret, team-a, err) wrongly poisoned KindForbidden(Secret, team-b)")
	}
}

// TestFakeKindSyncedNormalizesClusterScopedKinds: Node has exactly one cache
// regardless of namespace, real or fake — gating it under any namespace
// argument must gate every namespace argument.
func TestFakeKindSyncedNormalizesClusterScopedKinds(t *testing.T) {
	t.Parallel()
	c := New("team-a", "dev")

	c.SetKindSynced(kube.KindNode, "team-a", false)
	if c.KindSynced(kube.KindNode, "") {
		t.Error("SetKindSynced(Node, team-a, false) did not normalize to the cluster-wide \"\" cache")
	}
	if c.KindSynced(kube.KindNode, "team-b") {
		t.Error("SetKindSynced(Node, team-a, false) did not normalize across namespace arguments for a cluster-scoped kind")
	}
}

// TestFakeKindErrorIsScopedPerNamespace mirrors
// TestFakeKindSyncedIsScopedPerNamespace for the third health seam: an error
// set for one namespace's cache must not leak into another namespace's, or a
// scoped-mode test asking the wrong namespace would see the same error
// anyway and never catch the mistake.
func TestFakeKindErrorIsScopedPerNamespace(t *testing.T) {
	t.Parallel()
	c := New("team-a", "dev")

	stalled := errors.New("stalled list")
	c.SetKindError(kube.KindPod, "team-a", stalled)
	if c.KindError(kube.KindPod, "team-a") == nil {
		t.Error("KindError(Pod, team-a) = nil after SetKindError(Pod, team-a, err)")
	}
	if c.KindError(kube.KindPod, "team-b") != nil {
		t.Error("SetKindError(Pod, team-a, err) wrongly poisoned KindError(Pod, team-b)")
	}
	if c.KindError(kube.KindPod, "") != nil {
		t.Error("SetKindError(Pod, team-a, err) wrongly poisoned the cluster-wide KindError(Pod, \"\") cache")
	}
}

// TestFakeKindErrorNormalizesClusterScopedKinds mirrors
// TestFakeKindSyncedNormalizesClusterScopedKinds for KindError: Node has
// exactly one cache regardless of namespace, so gating it under any
// namespace argument must gate every namespace argument.
func TestFakeKindErrorNormalizesClusterScopedKinds(t *testing.T) {
	t.Parallel()
	c := New("team-a", "dev")

	stalled := errors.New("stalled list")
	c.SetKindError(kube.KindNode, "team-a", stalled)
	if c.KindError(kube.KindNode, "") == nil {
		t.Error("SetKindError(Node, team-a, err) did not normalize to the cluster-wide \"\" cache")
	}
	if c.KindError(kube.KindNode, "team-b") == nil {
		t.Error("SetKindError(Node, team-a, err) did not normalize across namespace arguments for a cluster-scoped kind")
	}
}

// TestFakeKindForbiddenNormalizesClusterScopedKinds mirrors
// TestFakeKindSyncedNormalizesClusterScopedKinds for KindForbidden.
func TestFakeKindForbiddenNormalizesClusterScopedKinds(t *testing.T) {
	t.Parallel()
	c := New("team-a", "dev")

	denied := errors.New("forbidden")
	c.SetKindForbidden(kube.KindNode, "team-a", denied)
	if c.KindForbidden(kube.KindNode, "") == nil {
		t.Error("SetKindForbidden(Node, team-a, err) did not normalize to the cluster-wide \"\" cache")
	}
	if c.KindForbidden(kube.KindNode, "team-b") == nil {
		t.Error("SetKindForbidden(Node, team-a, err) did not normalize across namespace arguments for a cluster-scoped kind")
	}
}

// TestDemoCrashLoopPodHasReadablePreviousRun keeps --demo's crash-looping
// pods shaped like the real thing: a terminated last state with a container
// ID, which is what podlogs streams from instead of parking on "waiting for
// container to start" (review 04 H2).
func TestDemoCrashLoopPodHasReadablePreviousRun(t *testing.T) {
	t.Parallel()
	pod := demoCrashLoopPod("worker-0", "default", metav1.Now(), "node-a")
	infos := kube.PodFromObject(pod).ContainerInfos
	if len(infos) == 0 || infos[0].State != "Waiting" || !infos[0].HasPreviousInstance {
		t.Fatalf("ContainerInfos = %+v, want a Waiting container with a readable previous run", infos)
	}
}

// TestPatchDataEnforcesResourceVersion is the fake-cluster side of review
// finding 09 M5: a seeded ConfigMap/Secret gets a resourceVersion, a patch
// against it bumps it, and a second patch against the old one is a typed
// Conflict that leaves the data untouched — so --demo exercises the same
// 409 path a real apiserver does.
func TestPatchDataEnforcesResourceVersion(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindConfigMap, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "default"},
		Data:       map[string]string{"K": "a"},
	})
	c.Seed(kube.KindSecret, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sec", Namespace: "default"},
		Data:       map[string][]byte{"K": []byte("a")},
	})

	if _, err := c.PatchConfigMapData(t.Context(), "default", "cfg", "", "K", "b", false); err == nil || apierrors.IsConflict(err) {
		t.Fatalf("expected a missing precondition refused (not a Conflict), got %v", err)
	}
	rv, err := c.PatchConfigMapData(t.Context(), "default", "cfg", "1", "K", "b", false)
	if err != nil {
		t.Fatalf("PatchConfigMapData at the seeded version: %v", err)
	}
	if _, err := c.PatchConfigMapData(t.Context(), "default", "cfg", "1", "K", "c", false); !apierrors.IsConflict(err) {
		t.Fatalf("expected a Conflict against the stale version, got %v", err)
	}
	objs, _ := c.ListRaw(t.Context(), kube.KindConfigMap, "default")
	if got := objs[0].(*corev1.ConfigMap).Data["K"]; got != "b" {
		t.Errorf("Data[K] = %q, want b (the conflicted write must not land)", got)
	}
	if stored := objs[0].(*corev1.ConfigMap).ResourceVersion; rv != stored || rv == "1" {
		t.Errorf("returned resourceVersion = %q, want the bumped stored %q", rv, stored)
	}
	if again, err := c.PatchConfigMapData(t.Context(), "default", "cfg", rv, "K", "d", false); err != nil || again == rv {
		t.Errorf("patch at the returned resourceVersion = (%q, %v), want a success with a new one", again, err)
	}

	if _, err := c.PatchSecretData(t.Context(), "default", "sec", "1", "K", "", true); err != nil {
		t.Fatalf("PatchSecretData at the seeded version: %v", err)
	}
	if _, err := c.PatchSecretData(t.Context(), "default", "sec", "1", "K", "x", false); !apierrors.IsConflict(err) {
		t.Fatalf("expected a Conflict against the stale version, got %v", err)
	}
}

// TestCountLiveMatchesListRaw: the fake's CountLive is what --demo's goto
// palette counts through, so it must agree with what the list then shows —
// namespace-filtered for a namespaced kind, ignoring the namespace for a
// cluster-scoped one, as *kube.Cluster.CountLive does.
func TestCountLiveMatchesListRaw(t *testing.T) {
	t.Parallel()
	c := NewDemo()
	pods, _ := c.ListRaw(t.Context(), kube.KindPod, "")
	if len(pods) == 0 {
		t.Fatal("demo fixtures have no pods to count")
	}
	accessor, _ := apimeta.Accessor(pods[0])
	ns := accessor.GetNamespace()
	nsPods, _ := c.ListRaw(t.Context(), kube.KindPod, ns)
	nodes, _ := c.ListRaw(t.Context(), kube.KindNode, "")

	for _, tc := range []struct {
		kind      kube.ResourceKind
		namespace string
		want      int
	}{
		{kube.KindPod, "", len(pods)},
		{kube.KindPod, ns, len(nsPods)},
		{kube.KindNode, ns, len(nodes)},
	} {
		got, err := c.CountLive(t.Context(), tc.kind, tc.namespace)
		if err != nil {
			t.Fatalf("CountLive(%s, %q): %v", tc.kind, tc.namespace, err)
		}
		if got != tc.want {
			t.Errorf("CountLive(%s, %q) = %d, want %d", tc.kind, tc.namespace, got, tc.want)
		}
	}
}

// TestStreamPodLogsHonoursRequest pins L7: the fake answers per container,
// trims to TailLines and SinceSeconds, and keeps a follow stream open until
// its context is cancelled instead of replaying the log as a second copy.
func TestStreamPodLogsHonoursRequest(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().UTC().Format(time.RFC3339)
	c.SeedLogs("default", "api", []string{old + " one", recent + " two", recent + " three"})
	c.SeedContainerLogs("default", "api", "sidecar", []string{"sidecar line"})

	read := func(req kube.LogStreamRequest) string {
		t.Helper()
		rc, err := c.StreamPodLogs(t.Context(), req)
		if err != nil {
			t.Fatalf("StreamPodLogs: %v", err)
		}
		defer func() { _ = rc.Close() }()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return string(b)
	}
	base := kube.LogStreamRequest{Namespace: "default", PodName: "api", Container: "app"}
	if got := read(base); strings.Count(got, "\n") != 3 {
		t.Fatalf("unfiltered = %q, want all three lines", got)
	}
	tail := base
	tail.TailLines = 1
	if got := read(tail); !strings.HasSuffix(got, " three\n") || strings.Count(got, "\n") != 1 {
		t.Fatalf("TailLines=1 = %q, want only the last line", got)
	}
	since := base
	since.SinceSeconds = 60
	if got := read(since); strings.Contains(got, " one") || !strings.Contains(got, " two") {
		t.Fatalf("SinceSeconds=60 = %q, want only the recent lines", got)
	}
	sidecar := base
	sidecar.Container = "sidecar"
	if got := read(sidecar); got != "sidecar line\n" {
		t.Fatalf("sidecar container = %q, want its own lines", got)
	}

	ctx, cancel := context.WithCancel(t.Context())
	follow := base
	follow.Follow = true
	rc, err := c.StreamPodLogs(ctx, follow)
	if err != nil {
		t.Fatalf("StreamPodLogs follow: %v", err)
	}
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(rc)
		done <- string(b)
	}()
	select {
	case <-done:
		t.Fatal("a follow stream ended before its context was cancelled")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if got := <-done; strings.Count(got, "\n") != 3 {
		t.Fatalf("follow = %q, want the three lines then EOF on cancel", got)
	}
}

// TestHelmRollbackMatchesHelm: like real helm, the new revision carries
// "Rollback to N" as its description, every revision still marked deployed
// becomes superseded (a failed upgrade leaves the last good one deployed),
// and rolling back revision 1 has nothing to roll back to.
func TestHelmRollbackMatchesHelm(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	rel := func(rev int, status string) runtime.Object {
		return kube.EncodeHelmReleaseSecret(kube.HelmRelease{Namespace: "default", Name: "web", Chart: "web", ChartVersion: "1.0.0", Revision: rev, Status: status})
	}
	c.Seed(kube.KindSecret, rel(1, "superseded"), rel(2, "deployed"), rel(3, "failed"))

	if err := c.HelmRollback(t.Context(), "default", "web", 1); err != nil {
		t.Fatalf("HelmRollback: %v", err)
	}
	objs, _ := c.ListRaw(t.Context(), kube.KindSecret, "default")
	history := kube.HelmReleaseHistory(kube.DecodeHelmReleases(objs), "default", "web")
	got := map[int]kube.HelmRelease{}
	for _, r := range history {
		got[r.Revision] = r
	}
	if r := got[4]; r.Status != "deployed" || r.StatusReason != "Rollback to 1" {
		t.Fatalf("new revision = %+v, want deployed with \"Rollback to 1\"", r)
	}
	for _, rev := range []int{2, 3} {
		if got[rev].Status != "superseded" {
			t.Fatalf("revision %d status = %q, want superseded", rev, got[rev].Status)
		}
	}

	solo := New("default", "dev")
	solo.Seed(kube.KindSecret, rel(1, "deployed"))
	if err := solo.HelmRollback(t.Context(), "default", "web", 0); err == nil {
		t.Fatal("rolling back a release's only revision should fail like helm's \"no 0 version\"")
	}
}

// TestDrainReportsBlockedEvictions pins the fake's partial drain: a pod whose
// eviction is blocked survives, the rest are evicted, and the error names
// the blocked pod — the same joined shape kube.Cluster.Drain returns.
func TestDrainReportsBlockedEvictions(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindNode, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}})
	c.Seed(kube.KindPod,
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}, Spec: corev1.PodSpec{NodeName: "node-1"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"}, Spec: corev1.PodSpec{NodeName: "node-1"}},
	)
	c.BlockEviction("default", "db")

	evicted, err := c.Drain(t.Context(), "node-1")
	if evicted != 1 {
		t.Fatalf("evicted = %d, want 1", evicted)
	}
	if err == nil || !strings.Contains(err.Error(), "default/db") || !apierrors.IsTooManyRequests(err) {
		t.Fatalf("Drain err = %v, want a joined 429 naming default/db", err)
	}
	remaining, _ := c.ListRaw(t.Context(), kube.KindPod, "default")
	if len(remaining) != 1 {
		t.Fatalf("expected the blocked pod to survive, got %d pods remaining", len(remaining))
	}
}

func TestDeleteResourceForcedIsRecorded(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindPod,
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "default"}},
	)
	if err := c.DeleteResource(t.Context(), kube.KindPod, "default", "a"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteResourceForced(t.Context(), kube.KindPod, "default", "b"); err != nil {
		t.Fatal(err)
	}
	if got := c.ForcedDeletes(); !slices.Equal(got, []string{"Pod default/b"}) {
		t.Fatalf("ForcedDeletes = %v, want only the forced delete", got)
	}
}

// TestListRawDoesNotShareStorageWithMutators pins that a reader holding a
// listed object never sees (or races) a later in-place mutation — run under
// -race, the read below races Cordon's write if ListRaw hands out the
// stored pointer.
func TestListRawDoesNotShareStorageWithMutators(t *testing.T) {
	t.Parallel()
	c := New("default", "dev")
	c.Seed(kube.KindNode, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}})
	listed, _ := c.ListRaw(t.Context(), kube.KindNode, "")
	node := listed[0].(*corev1.Node)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Cordon(t.Context(), "node-1", true)
	}()
	unschedulable := node.Spec.Unschedulable
	<-done
	if unschedulable || node.Spec.Unschedulable {
		t.Fatalf("a listed Node changed under its reader")
	}
	again, _ := c.ListRaw(t.Context(), kube.KindNode, "")
	if !again[0].(*corev1.Node).Spec.Unschedulable {
		t.Fatalf("Cordon did not reach the next list")
	}
}
