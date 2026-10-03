package resources

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// TestUnsettledWorkloadsNamesOnlyTheMovingOnes: the set is what 18a joins a
// release's manifest against, so a settled workload in it would leave a
// release marked "rolling out" forever, and a moving one missing from it is
// the green-mid-upgrade bug itself.
func TestUnsettledWorkloadsNamesOnlyTheMovingOnes(t *testing.T) {
	rolling := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "nva", Name: "api", Generation: 3},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr32(3)},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 3, Replicas: 4, ReadyReplicas: 2, UpdatedReplicas: 1, AvailableReplicas: 2,
		},
	}
	stable := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "nva", Name: "web", Generation: 1},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr32(2)},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1, Replicas: 2, ReadyReplicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2,
		},
	}
	// Spec changed, controller hasn't observed it yet: nothing in the status
	// looks wrong, which is exactly the window an upgrade is first visible in.
	unobserved := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "nva", Name: "queue", Generation: 5},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr32(1)},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 4, Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1,
		},
	}
	settledSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "nva", Name: "agent", Generation: 2},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration: 2, DesiredNumberScheduled: 3, UpdatedNumberScheduled: 3, NumberReady: 3,
		},
	}
	partialSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "nva", Name: "collector", Generation: 2},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration: 2, DesiredNumberScheduled: 3, UpdatedNumberScheduled: 1, NumberReady: 2,
		},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment:  {rolling, stable},
		kube.KindStatefulSet: {unobserved},
		kube.KindDaemonSet:   {settledSet, partialSet},
	}}

	got := UnsettledWorkloads(t.Context(), lister, "nva")
	want := []kube.WorkloadRef{
		{Kind: kube.KindDeployment, Namespace: "nva", Name: "api"},
		{Kind: kube.KindStatefulSet, Namespace: "nva", Name: "queue"},
		{Kind: kube.KindDaemonSet, Namespace: "nva", Name: "collector"},
	}
	if len(got) != len(want) {
		t.Fatalf("UnsettledWorkloads = %v, want exactly %v", got, want)
	}
	for _, ref := range want {
		if _, ok := got[ref]; !ok {
			t.Errorf("%+v missing from the unsettled set %v", ref, got)
		}
	}
}

// TestUnsettledWorkloadsReadsOnlyTheRollingKinds: this runs off the Helm
// list, and every kind it touches starts an informer. Three named kinds is
// the whole budget — a sweep of the registry here is the breadth-first read
// the lazy-informer rule forbids.
func TestUnsettledWorkloadsReadsOnlyTheRollingKinds(t *testing.T) {
	rec := &recordingLister{}
	UnsettledWorkloads(t.Context(), rec, "nva")

	want := map[kube.ResourceKind]bool{
		kube.KindDeployment: true, kube.KindStatefulSet: true, kube.KindDaemonSet: true,
	}
	if len(rec.kinds) != len(want) {
		t.Fatalf("listed %v, want exactly the three rolling kinds", rec.kinds)
	}
	for _, kind := range rec.kinds {
		if !want[kind] {
			t.Errorf("listed %s — reading it here starts its informer from the Helm list", kind)
		}
	}
}

// TestUnsettledWorkloadsDegradesQuietly: an unreadable cache (Forbidden, or
// one that hasn't filled) must contribute nothing. Reporting a rollout kute
// can't see would put a permanent ▸ on every release.
func TestUnsettledWorkloadsDegradesQuietly(t *testing.T) {
	lister := fakeLister{err: errNoCache}
	if got := UnsettledWorkloads(t.Context(), lister, "nva"); len(got) != 0 {
		t.Errorf("UnsettledWorkloads on a failing lister = %v, want empty", got)
	}
	if got := UnsettledWorkloads(t.Context(), nil, "nva"); len(got) != 0 {
		t.Errorf("UnsettledWorkloads(nil lister) = %v, want empty", got)
	}
}

// TestHelmReleaseGlyphOutranksOutdated: 18a keeps helm's own STATUS word and
// carries orthogonal signals in the glyph alone. A release that is both
// behind its repo and mid-rollout shows the rollout — §19a's own ranking says
// a chart behind its repo is the least urgent thing on screen.
func TestHelmReleaseGlyphOutranksOutdated(t *testing.T) {
	base := kube.HelmRelease{
		Namespace: "nva", Name: "api", Chart: "nva", ChartVersion: "1.4.2",
		Revision: 7, Status: "deployed",
	}.WithLatest("2.0.0", "testrepo", false)

	outdated := projectHelmRelease(kube.NewHelmReleaseObject(base))
	if outdated.Glyph != "▲" || outdated.GlyphClass != StatusWarn {
		t.Errorf("outdated glyph = %q/%v, want ▲/warn", outdated.Glyph, outdated.GlyphClass)
	}

	rolling := projectHelmRelease(kube.NewHelmReleaseObject(base.WithRollout(true)))
	if rolling.Glyph != rolloutArrow || rolling.GlyphClass != StatusWarn {
		t.Errorf("mid-rollout glyph = %q/%v, want %s/warn", rolling.Glyph, rolling.GlyphClass, rolloutArrow)
	}
	// Helm's own word, and the strip's count of it, are untouched: the
	// release really is deployed as far as helm is concerned.
	if rolling.Status != StatusOK {
		t.Errorf("mid-rollout Status = %v, want StatusOK — the strip still counts it deployed", rolling.Status)
	}
	if got := rolling.Cells[5]; got != "deployed" {
		t.Errorf("STATUS cell = %q, want helm's own word verbatim", got)
	}
}

// recordingLister records which kinds were read, for the informer-cost test.
type recordingLister struct {
	kinds []kube.ResourceKind
}

func (r *recordingLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	r.kinds = append(r.kinds, kind)
	return nil, nil
}

// errNoCache stands in for a cache that can't answer — Forbidden, or one
// that never filled.
var errNoCache = errors.New("forbidden")

// TestWorkloadSettledHonoursOnDeleteAndPartition: an OnDelete workload or a
// partitioned StatefulSet never reaches Updated == replicas by design, so
// comparing against that would keep 18a's rolling glyph on forever.
func TestWorkloadSettledHonoursOnDeleteAndPartition(t *testing.T) {
	sts := func(strategy appsv1.StatefulSetUpdateStrategy, updated int32) *appsv1.StatefulSet {
		return &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "nva", Name: "db", Generation: 2},
			Spec:       appsv1.StatefulSetSpec{Replicas: ptr32(5), UpdateStrategy: strategy},
			Status: appsv1.StatefulSetStatus{
				ObservedGeneration: 2, Replicas: 5, ReadyReplicas: 5, UpdatedReplicas: updated,
			},
		}
	}
	partition := func(p int32) appsv1.StatefulSetUpdateStrategy {
		return appsv1.StatefulSetUpdateStrategy{
			Type:          appsv1.RollingUpdateStatefulSetStrategyType,
			RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr32(p)},
		}
	}
	onDeleteDS := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "nva", Name: "agent", Generation: 2},
		Spec:       appsv1.DaemonSetSpec{UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.OnDeleteDaemonSetStrategyType}},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration: 2, DesiredNumberScheduled: 3, UpdatedNumberScheduled: 0, NumberReady: 3,
		},
	}
	tests := []struct {
		name string
		obj  runtime.Object
		want bool
	}{
		{"sts OnDelete with no updated pods", sts(appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}, 0), true},
		{"sts partition 3 with ordinals 3-4 updated", sts(partition(3), 2), true},
		{"sts partition 3 with only one updated", sts(partition(3), 1), false},
		{"sts default strategy partially updated", sts(appsv1.StatefulSetUpdateStrategy{}, 4), false},
		{"ds OnDelete with no updated pods", onDeleteDS, true},
	}
	for _, tt := range tests {
		if _, settled, _ := workloadSettled(tt.obj); settled != tt.want {
			t.Errorf("%s: settled = %v, want %v", tt.name, settled, tt.want)
		}
	}
}
