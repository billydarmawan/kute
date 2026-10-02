package kube

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestTimelineFromEvents(t *testing.T) {
	now := time.Now()
	groups := []EventGroup{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Namespace: "default", Message: "m", LastSeen: now},
	}
	entries := TimelineFromEvents(groups)
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Kind != TimelineEvent || e.Severity != "Warning" || e.Object != "Pod/worker-0" || !e.Time.Equal(now) {
		t.Fatalf("entry = %+v, unexpected projection", e)
	}
}

func TestTimelineFromRestartsSkipsPodsWithoutTermination(t *testing.T) {
	pods := []Pod{
		{Name: "a", Namespace: "default"}, // no LastTermination
		{Name: "b", Namespace: "default", LastTermination: &LastTermination{
			Container: "app", Reason: "OOMKilled", ExitCode: 137, FinishedAt: time.Now(),
		}},
	}
	entries := TimelineFromRestarts(pods)
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	if entries[0].Object != "Pod/b" || entries[0].Kind != TimelineRestart {
		t.Fatalf("entry = %+v, want restart for Pod/b", entries[0])
	}
}

// TestTimelineFromRestartsSkipsCleanJobCompletion pins that a completed Job
// pod's clean exit 0 (findLastTermination in pods.go leaves LastTermination
// nil for these) never surfaces as a mislabeled "Restarted ... exit 0"
// timeline entry.
func TestTimelineFromRestartsSkipsCleanJobCompletion(t *testing.T) {
	pods := []Pod{
		{Name: "batch-1-x7f2k", Namespace: "default"}, // LastTermination nil, mirrors a completed Job pod
	}
	entries := TimelineFromRestarts(pods)
	if len(entries) != 0 {
		t.Fatalf("entries = %+v, want none for a cleanly completed pod", entries)
	}
}

func newReplicaSet(name, deployment string, revision string, image string, created time.Time) *appsv1.ReplicaSet {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(created),
		},
	}
	if revision != "" {
		rs.Annotations = map[string]string{"deployment.kubernetes.io/revision": revision}
	}
	if deployment != "" {
		rs.OwnerReferences = []metav1.OwnerReference{{Kind: "Deployment", Name: deployment}}
	}
	if image != "" {
		rs.Spec.Template.Spec.Containers = []corev1.Container{{Image: image}}
	}
	return rs
}

func TestTimelineFromRolloutsFiltersToDeploymentOwnedReplicaSets(t *testing.T) {
	objs := []runtime.Object{
		newReplicaSet("nva-worker-1", "nva-worker", "3", "nginx:1.25", time.Now().Add(-time.Hour)),
		newReplicaSet("nva-worker-2", "nva-worker", "4", "nginx:1.26", time.Now()),
		newReplicaSet("orphan", "", "", "", time.Now()),                          // no Deployment owner
		newReplicaSet("no-revision", "nva-worker", "", "nginx:1.24", time.Now()), // no revision annotation
	}
	entries := TimelineFromRollouts(objs)
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2, got %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.Kind != TimelineRollout || e.Object != "Deployment/nva-worker" {
			t.Fatalf("entry = %+v, unexpected projection", e)
		}
	}
}

func TestMergeTimelineSortsNewestFirst(t *testing.T) {
	now := time.Now()
	a := []TimelineEntry{{Time: now.Add(-time.Hour), Reason: "old"}}
	b := []TimelineEntry{{Time: now, Reason: "new"}}
	c := []TimelineEntry{{Time: now.Add(-30 * time.Minute), Reason: "mid"}}

	merged := MergeTimeline(a, b, c)
	if len(merged) != 3 {
		t.Fatalf("len = %d, want 3", len(merged))
	}
	if merged[0].Reason != "new" || merged[1].Reason != "mid" || merged[2].Reason != "old" {
		t.Fatalf("merged not newest-first: %+v", merged)
	}
}

// TestRolledBackRevisionIsLabelledAndRedated pins a rollback's reused
// ReplicaSet: its creationTimestamp is the original rollout's, weeks back,
// so the current revision takes the Deployment's Progressing lastUpdateTime
// and is labelled with the revision it used to be.
func TestRolledBackRevisionIsLabelledAndRedated(t *testing.T) {
	now := time.Now()
	reused := newReplicaSet("web-old", "web", "6", "nginx:1.25", now.Add(-21*24*time.Hour))
	reused.Annotations["deployment.kubernetes.io/revision-history"] = "2,4"
	older := newReplicaSet("web-mid", "web", "5", "nginx:1.26", now.Add(-2*time.Hour))
	older.Annotations["deployment.kubernetes.io/revision-history"] = "3"
	entries := TimelineFromRollouts([]runtime.Object{reused, older})

	progressed := now.Add(-5 * time.Minute)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", Annotations: map[string]string{"deployment.kubernetes.io/revision": "6"}},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, LastUpdateTime: metav1.NewTime(progressed),
		}}},
	}
	RedateRolledBackRevisions(entries, []runtime.Object{dep})

	byRev := map[int]TimelineEntry{}
	for _, e := range entries {
		byRev[e.Revision] = e
	}
	if e := byRev[6]; !e.Time.Equal(progressed) || e.Message != "revision 6 · rolled back (was rev 4) · nginx:1.25" {
		t.Fatalf("current reused revision = %+v, want redated to the Progressing update and labelled", e)
	}
	// Not the current revision, so no signal dates its re-adoption.
	if e := byRev[5]; !e.Time.Equal(older.CreationTimestamp.Time) {
		t.Fatalf("older reused revision moved to %v, want its creation time", e.Time)
	}
}
