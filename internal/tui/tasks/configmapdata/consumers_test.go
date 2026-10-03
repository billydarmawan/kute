package configmapdata

import (
	"context"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

func volumeSpec(cmName string) corev1.PodSpec {
	return corev1.PodSpec{Volumes: []corev1.Volume{{
		Name:         "config",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cmName}}},
	}}}
}

func controlledBy(kind, name string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &yes}}
}

// fixedConsumerObjs is one of each not-restartable consumer shape, plus the
// owned objects that must not be listed in their own right.
func fixedConsumerObjs() map[kube.ResourceKind][]runtime.Object {
	return map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {deploymentEnvFrom("nva-stage", "nva-worker", "nva-config")},
		kube.KindCronJob: {&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "nva-stage"},
			Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: volumeSpec("nva-config")}}}}}},
		kube.KindJob: {
			&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "nva-stage"},
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: volumeSpec("nva-config")}}},
			// A CronJob's run: the CronJob is the consumer, not each Job.
			&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "nightly-123", Namespace: "nva-stage", OwnerReferences: controlledBy("CronJob", "nightly")},
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: volumeSpec("nva-config")}}},
		},
		kube.KindPod: {
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "debug", Namespace: "nva-stage"}, Spec: volumeSpec("nva-config")},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "nva-worker-abc", Namespace: "nva-stage", OwnerReferences: controlledBy("ReplicaSet", "nva-worker-5d")}, Spec: volumeSpec("nva-config")},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "migrate-xyz", Namespace: "nva-stage", OwnerReferences: controlledBy("Job", "migrate")}, Spec: volumeSpec("nva-config")},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "nva-stage"}, Spec: volumeSpec("other")},
		},
	}
}

func consumerNames(cs []configMapConsumer) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = shortWorkloadArg(c.Kind) + "/" + c.Name
	}
	return out
}

func TestFixedConsumersListBareObjectsNotOwnedOnes(t *testing.T) {
	restartable, fixed := findConfigMapConsumers(t.Context(), fakeLister{objs: fixedConsumerObjs()}, "nva-stage", "nva-config")
	if got := consumerNames(restartable); !slices.Equal(got, []string{"deploy/nva-worker"}) {
		t.Fatalf("restartable = %v, want only the Deployment", got)
	}
	if got, want := consumerNames(fixed), []string{"cj/nightly", "job/migrate", "pod/debug"}; !slices.Equal(got, want) {
		t.Fatalf("fixed = %v, want %v (owned Pods and the CronJob's own Job excluded)", got, want)
	}
}

// startedLister is a lazily started lister: only kinds in started have a
// running cache. reads records every kind ListRaw was asked for.
type startedLister struct {
	fakeLister
	started map[kube.ResourceKind]bool
	reads   *[]kube.ResourceKind
}

func (s startedLister) ListRaw(ctx context.Context, kind kube.ResourceKind, ns string) ([]runtime.Object, error) {
	*s.reads = append(*s.reads, kind)
	return s.fakeLister.ListRaw(ctx, kind, ns)
}

func (s startedLister) KindStarted(kind kube.ResourceKind, _ string) bool { return s.started[kind] }

func TestFixedConsumersReadOnlyStartedCaches(t *testing.T) {
	var reads []kube.ResourceKind
	lister := startedLister{
		fakeLister: fakeLister{objs: fixedConsumerObjs()},
		started: map[kube.ResourceKind]bool{
			kube.KindDeployment: true, kube.KindStatefulSet: true, kube.KindDaemonSet: true, kube.KindPod: true,
		},
		reads: &reads,
	}
	_, fixed := findConfigMapConsumers(t.Context(), lister, "nva-stage", "nva-config")
	if got := consumerNames(fixed); !slices.Equal(got, []string{"pod/debug"}) {
		t.Fatalf("fixed = %v, want only the bare Pod while Job/CronJob caches aren't running", got)
	}
	if slices.Contains(reads, kube.KindJob) || slices.Contains(reads, kube.KindCronJob) {
		t.Fatalf("read %v; an un-started Job/CronJob cache must never be read", reads)
	}

	// Once the Job cache runs (but not CronJob's), its bare Job shows, and
	// the CronJob-owned run does too — nothing else would name it.
	lister.started[kube.KindJob] = true
	_, fixed = findConfigMapConsumers(t.Context(), lister, "nva-stage", "nva-config")
	if got, want := consumerNames(fixed), []string{"job/migrate", "job/nightly-123", "pod/debug"}; !slices.Equal(got, want) {
		t.Fatalf("fixed = %v, want %v", got, want)
	}
}

// The strip marks them not restartable, and ctrl-r restarts only the
// restartable ones, its confirm saying how many it leaves alone.
func TestCtrlRLeavesNotRestartableConsumersAlone(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"LOG_LEVEL": "info"})
	mut := &fakeMutator{cm: cm}
	m := newModel(t, newSession(), cm, mut, fixedConsumerObjs())

	strip := plain(strings.Join(m.Strips(160), "\n"))
	for _, want := range []string{"deploy/nva-worker ↗ env · not restartable: cj/nightly ↗ volume · job/migrate ↗ volume · pod/debug ↗ volume"} {
		if !strings.Contains(strip, want) {
			t.Fatalf("strip missing %q:\n%s", want, strip)
		}
	}
	if strings.Contains(strip, "nva-worker-abc") || strings.Contains(strip, "migrate-xyz") {
		t.Fatalf("owned Pods must not be listed as consumers:\n%s", strip)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	for range "info" {
		m = step(t, m, tea.KeyPressMsg{Text: "backspace"})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "x"})
	m = step(t, m, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	if note := m.confirmNote("update"); !strings.Contains(note, "restart 1 consumer · 3 not restartable") {
		t.Fatalf("confirm note = %q, want it to say what ctrl-r leaves alone", note)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})

	if !slices.Equal(mut.rolloutRestarts, []string{"Deployment/nva-stage/nva-worker"}) {
		t.Fatalf("rolloutRestarts = %v, want only the Deployment", mut.rolloutRestarts)
	}
	if !strings.Contains(m.message, "restarted 1 consumer") {
		t.Fatalf("message = %q, want it to report the one restart", m.message)
	}
}
