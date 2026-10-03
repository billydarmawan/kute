package kube_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/tasks/configmapdata"
)

func cmVolumeSpec(cm string) corev1.PodSpec {
	return corev1.PodSpec{Volumes: []corev1.Volume{{
		Name:         "config",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cm}}},
	}}}
}

// loadConfigMapData opens 27a on cm/app-config and runs its load to
// completion against c, returning the rendered strips.
func loadConfigMapData(t *testing.T, c *kube.Cluster) string {
	t.Helper()
	m := configmapdata.New(configmapdata.Config{
		Session: &tui.Session{Theme: tui.Dark()},
		Lister:  c, Namespace: "app", Name: "app-config",
		LoadTimeout: 2 * time.Second,
	})
	m.SetSize(160, 36)
	deadline := time.Now().Add(3 * time.Second)
	msg := m.Reload()()
	for {
		updated, _ := m.Update(msg)
		m = *updated.(*configmapdata.Model)
		strips := ansi.Strip(strings.Join(m.Strips(160), "\n"))
		if strings.Contains(strips, "deploy/web") {
			return strips
		}
		if time.Now().After(deadline) {
			t.Fatalf("27a never loaded its consumers; strips:\n%s", strips)
		}
		time.Sleep(5 * time.Millisecond)
		msg = m.Reload()()
	}
}

// Opening a ConfigMap's Data view lists Job/CronJob consumers only from a
// cache something else already started — it must never start the Job or
// CronJob informer itself (CLAUDE.md, "the app pays for the screen you're
// on").
func TestConfigMapDataNeverStartsJobOrCronJobInformers(t *testing.T) {
	c, cs := kube.NewLazyTestCluster(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "app-config", Namespace: "app"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "app"},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: cmVolumeSpec("app-config")}}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "app"},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: cmVolumeSpec("app-config")}}},
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "app"},
			Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: cmVolumeSpec("app-config")}}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "debug", Namespace: "app"}, Spec: cmVolumeSpec("app-config")},
	)
	defer c.Stop()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	strips := loadConfigMapData(t, c)

	for _, kind := range []kube.ResourceKind{kube.KindJob, kube.KindCronJob} {
		if c.KindStarted(kind, "app") {
			t.Errorf("opening a ConfigMap started the %s informer", kind)
		}
	}
	listed := kube.ListedResources(cs)
	if slices.Contains(listed, "jobs") || slices.Contains(listed, "cronjobs") {
		t.Errorf("opening a ConfigMap listed %v; jobs and cronjobs must stay unread", listed)
	}
	if strings.Contains(strips, "job/migrate") || strings.Contains(strips, "cj/nightly") {
		t.Errorf("un-started Job/CronJob caches must be omitted, got:\n%s", strips)
	}
	// Pod is eager, so the bare Pod is listed straight away.
	if !strings.Contains(strips, "not restartable: pod/debug ↗ volume") {
		t.Errorf("the bare Pod consumer should be listed as not restartable, got:\n%s", strips)
	}

	// Once something else has started them (the user opened CronJobs, which
	// reads both), the next load lists them.
	for _, kind := range []kube.ResourceKind{kube.KindCronJob, kube.KindJob} {
		if _, err := c.ListRaw(t.Context(), kind, "app"); err != nil {
			t.Fatalf("ListRaw(%s): %v", kind, err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for !c.KindSynced(kube.KindJob, "app") || !c.KindSynced(kube.KindCronJob, "app") {
		if time.Now().After(deadline) {
			t.Fatal("Job/CronJob caches never synced")
		}
		time.Sleep(5 * time.Millisecond)
	}
	strips = loadConfigMapData(t, c)
	for _, want := range []string{"not restartable: cj/nightly ↗ volume · job/migrate ↗ volume"} {
		if !strings.Contains(strips, want) {
			t.Errorf("want %q once its cache runs, got:\n%s", want, strips)
		}
	}
}
