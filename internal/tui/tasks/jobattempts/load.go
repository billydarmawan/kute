package jobattempts

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
)

// reloadDebounce coalesces bursts of watch events into one reload — same
// value as cronjobdetail's own reloadDebounce.
const reloadDebounce = 250 * time.Millisecond

// reloadDueMsg fires scheduleReload's retry — mirrors cronjobdetail's own.
type reloadDueMsg struct{ epoch int }

// scheduleReload arranges one reloadDueMsg reloadDebounce from now.
func (m Model) scheduleReload(epoch int) tea.Cmd {
	return tea.Tick(reloadDebounce, func(time.Time) tea.Msg {
		return reloadDueMsg{epoch: epoch}
	})
}

// load lists Job (namespace-scoped, single object) and best-effort Pod
// once each, then aggregates client-side via resources.BuildJobAttempts —
// the same recipe browse's own loadJobRows follows for the list. A Job list
// failure fails the whole load; a Pod list failure does not (the attempt
// table simply reflects fewer live pods — see the package doc comment).
func (m Model) load() tea.Cmd {
	epoch := m.reloadEpoch
	lister := m.lister
	namespace := m.namespace
	name := m.name
	timeout := m.timeout
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()

		jobObjs, err := lister.ListRaw(ctx, kube.KindJob, namespace)
		if err != nil {
			return loadedMsg{epoch: epoch, err: err}
		}
		job, found := findJob(jobObjs, name)
		if !found {
			return loadedMsg{epoch: epoch, found: false}
		}
		// Pods are best-effort — a failed Pod read must never erase a valid
		// Job-level failure (BuildJobAttempts already tolerates a nil/partial
		// pods slice).
		podObjs, _ := lister.ListRaw(ctx, kube.KindPod, namespace)

		summary, ok := resources.BuildJobAttempts(job, podObjs)
		if !ok {
			return loadedMsg{epoch: epoch, found: false}
		}
		return loadedMsg{
			epoch: epoch, found: true, summary: summary,
			pods: podsByName(podObjs), jobNames: jobNamesOf(jobObjs),
		}
	}
}

// jobNamesOf is the set of Job names in jobObjs — beginRerun's taken set.
func jobNamesOf(jobObjs []runtime.Object) map[string]bool {
	out := make(map[string]bool, len(jobObjs))
	for _, obj := range jobObjs {
		if j, ok := obj.(*batchv1.Job); ok {
			out[j.Name] = true
		}
	}
	return out
}

// findJob resolves name against jobObjs (a ListRaw reply already scoped to
// this screen's namespace).
func findJob(jobObjs []runtime.Object, name string) (*batchv1.Job, bool) {
	for _, obj := range jobObjs {
		if j, ok := obj.(*batchv1.Job); ok && j.Name == name {
			return j, true
		}
	}
	return nil, false
}

// podsByName indexes podObjs by name for the attempt table's own 'l' logs
// target lookup — mirrors cronjobdetail's own podsByName.
func podsByName(podObjs []runtime.Object) map[string]kube.Pod {
	out := make(map[string]kube.Pod, len(podObjs))
	for _, obj := range podObjs {
		if p, ok := obj.(*corev1.Pod); ok {
			out[p.Name] = kube.PodFromObject(p)
		}
	}
	return out
}
