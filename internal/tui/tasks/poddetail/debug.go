// debug.go wires §41a/§41b/§41c (docs/design v.0.11.0.dc.html): the shell
// pre-check that decides whether 'x' on the loaded pod execs, opens
// tasks/execpicker, or opens tasks/debugpanel — mirrors browse's own
// debug.go, duplicated per the repo's package-local-seam convention.
package poddetail

import (
	"context"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
)

// shellProbeTimeout bounds one container's probe — same bound execpicker's
// own detectShellsCmd uses.
const shellProbeTimeout = 5 * time.Second

type podShellDetection struct {
	container string
	shells    []string
	err       error
}

// podShellsProbedMsg carries every container's shell-detection outcome for
// one 'x' press — see browse's own podShellsProbedMsg doc comment.
type podShellsProbedMsg struct {
	namespace, podName string
	containers         []kube.ContainerInfo
	results            []podShellDetection
}

func (msg podShellsProbedMsg) allShellless() bool {
	if len(msg.results) == 0 {
		return false
	}
	for _, r := range msg.results {
		if r.err != nil || len(r.shells) != 0 {
			return false
		}
	}
	return true
}

// detectPodShellsCmd probes every container in parallel — see browse's own
// detectPodShellsCmd doc comment.
func detectPodShellsCmd(parent context.Context, namespace, podName string, containers []kube.ContainerInfo, detector ShellDetector) tea.Cmd {
	return func() tea.Msg {
		results := make([]podShellDetection, len(containers))
		var wg sync.WaitGroup
		for i, c := range containers {
			wg.Go(func() {
				ctx, cancel := context.WithTimeout(parent, shellProbeTimeout)
				defer cancel()
				shells, err := detector.DetectShells(ctx, namespace, podName, c.Name)
				results[i] = podShellDetection{container: c.Name, shells: shells, err: err}
			})
		}
		wg.Wait()
		return podShellsProbedMsg{namespace: namespace, podName: podName, containers: containers, results: results}
	}
}

// beginExecOrDebug resolves 'x' for the loaded pod — see browse's own
// beginExecOrDebug doc comment for the full contract.
func (m *Model) beginExecOrDebug() (tea.Model, tea.Cmd, bool) {
	if !m.found || len(m.pod.ContainerInfos) == 0 {
		return nil, nil, false
	}
	waiting := podHasWaitingContainer(m.pod.ContainerInfos)
	if kube.PodWontStayRunning(m.pod.Status, waiting) {
		task, cmd := m.openPodDebug(m.namespace, m.name, m.pod.ContainerInfos, m.pod.Status, waiting)
		return task, cmd, true
	}
	if m.shells == nil {
		return m.openSelectedExec()
	}
	return nil, detectPodShellsCmd(m.session.ClusterContext(), m.namespace, m.name, m.pod.ContainerInfos, m.shells), true
}

func podHasWaitingContainer(containers []kube.ContainerInfo) bool {
	for _, c := range containers {
		if c.State == "Waiting" {
			return true
		}
	}
	return false
}

// openPodDebug pushes the shared debug panel. The panel performs its own
// authoritative, mode-matched access review; callers must not gate opening
// on the partial cache-local RBAC graph. Terminal pod routing calls this
// before any shell probe; the running distroless path calls it after probing.
func (m *Model) openPodDebug(namespace, podName string, containers []kube.ContainerInfo, podPhase string, waiting bool) (tea.Model, tea.Cmd) {
	if m.openDebug == nil {
		m.execFeedback = podName + ": debug is unavailable"
		return m, nil
	}
	task, cmd := m.openDebug(namespace, podName, containers, podPhase, waiting, m.width, m.height)
	if task != nil {
		return task, cmd
	}
	return m, cmd
}

// routePodShellsProbed decides which of exec/execpicker/debugpanel 'x'
// meant, now that every container's probe result is in — see browse's own
// routePodShellsProbed doc comment.
func (m *Model) routePodShellsProbed(msg podShellsProbedMsg) (tea.Model, tea.Cmd) {
	if msg.allShellless() {
		waiting := podHasWaitingContainer(msg.containers)
		return m.openPodDebug(msg.namespace, msg.podName, msg.containers, m.pod.Status, waiting)
	}
	if len(msg.containers) == 1 {
		return m, execCmd(m.session.CommandTarget(), msg.namespace, msg.podName, msg.containers[0].Name, m.demo)
	}
	if m.openExec == nil {
		return m, nil
	}
	task, cmd := m.openExec(msg.namespace, msg.podName, msg.containers, m.width, m.height)
	if task != nil {
		return task, cmd
	}
	return m, cmd
}
