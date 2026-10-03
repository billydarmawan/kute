// debug.go wires §41a/§41b/§41c/§41d (docs/design v.0.11.0.dc.html): the
// shell pre-check that decides whether 'x' on a Pod row execs, opens
// tasks/execpicker, or opens tasks/debugpanel, plus 'x' on a Node row
// (openSelectedNodeDebug, update.go).
package browse

import (
	"context"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
)

// ShellDetector answers which shells a container actually has — declared
// here per the repo's consuming-interface convention (execpicker's own
// ShellDetector is the model this mirrors); kube.DetectShells is the real
// implementation, kube/fake supplies the demo one.
type ShellDetector interface {
	DetectShells(ctx context.Context, namespace, pod, container string) ([]string, error)
}

// shellProbeTimeout bounds one container's probe — same bound execpicker's
// own detectShellsCmd uses.
const shellProbeTimeout = 5 * time.Second

// podShellDetection is one container's probe result.
type podShellDetection struct {
	container string
	shells    []string
	err       error
}

// podShellsProbedMsg carries every container's shell-detection outcome for
// one 'x' press (D5's async pre-check) — unlike execpicker's own per-row
// message, browse needs every result at once to decide whether to exec
// directly, push execpicker, or push the debug panel.
type podShellsProbedMsg struct {
	namespace, podName string
	containers         []kube.ContainerInfo
	results            []podShellDetection
}

// allShellless reports whether every container's probe came back with a
// real (non-nil-err) empty result — the distroless case §41a/§41b route to
// the debug panel for. A probe that couldn't run at all (non-nil err) never
// counts as "no shell", matching execpicker's own shellsText contract.
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

// detectPodShellsCmd probes every container in parallel, returning one
// aggregated message once all probes finish — one short-lived kubectl exec
// per container, the same cost tasks/execpicker's own Init already pays,
// now paid on every 'x' press (not just the multi-container ones) so a
// distroless single-container pod routes to the debug panel instead of a
// dead exec attempt.
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

// beginExecOrDebug resolves 'x' for the selected Pod row. Pod state wins
// before shell detection: kubectl exec cannot probe a Pending or terminal
// pod, so those states open copy-mode debug directly. Running pods retain
// the async shell pre-check and its exec/execpicker/attach-debug fork.
func (m *Model) beginExecOrDebug() (tea.Model, tea.Cmd, bool) {
	if m.kind != kube.KindPod {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	pod, ok := m.pods[kube.PodKey(row.Namespace, row.Name)]
	if !ok || len(pod.ContainerInfos) == 0 {
		return nil, nil, false
	}
	podPhase, waiting := podStatusAndWaiting(pod)
	if kube.PodWontStayRunning(podPhase, waiting) {
		task, cmd := m.openPodDebug(pod.Namespace, pod.Name, pod.ContainerInfos, podPhase, waiting)
		return task, cmd, true
	}
	if m.shells == nil {
		return m.openSelectedExec()
	}
	if m.shellProbing {
		// A probe is already in flight: a second one would fire a second
		// kubectl exec once the first shell exits.
		return nil, nil, true
	}
	m.shellProbing = true
	m.execFeedback = shellProbingNote(pod.Name)
	return nil, detectPodShellsCmd(m.session.ClusterContext(), pod.Namespace, pod.Name, pod.ContainerInfos, m.shells), true
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

// routePodShellsProbed decides, now that every container's probe result is
// in, which of exec/execpicker/debugpanel 'x' actually meant (docs/design
// v.0.11.0.dc.html §41a): every container shell-less skips the picker
// entirely and opens the debug panel directly (modeCopy if the pod won't
// stay running, else modeAttach — decided inside debugpanel.New from
// podPhase/waiting); a single container with a shell (or an unknown result)
// execs immediately, matching today's fast path; anything else pushes
// execpicker as before.
func (m *Model) routePodShellsProbed(msg podShellsProbedMsg) (tea.Model, tea.Cmd) {
	m.shellProbing = false
	if m.execFeedback == shellProbingNote(msg.podName) {
		m.execFeedback = ""
	}
	if msg.allShellless() {
		podPhase, waiting := m.selectedPodStatusAndWaiting(msg.namespace, msg.podName)
		return m.openPodDebug(msg.namespace, msg.podName, msg.containers, podPhase, waiting)
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

// decorateDebugCopies appends §41c's "⚑ debug copy" NameSuffix tag to any
// Pod row this session recognizes (Session.DebugCopies) as a copy-mode pod
// kute itself created. This is a client-side fact that never lands on the
// object, so — unlike §41e's ephemeral-container tag — it can't be applied
// at projection time (projectPod's own doc comment) and instead rides in
// here as a post-projection decoration, once per fresh Pod list load
// (applyRowsLoaded).
func (m *Model) decorateDebugCopies() {
	if m.kind != kube.KindPod || m.session == nil || m.session.DebugCopies == nil {
		return
	}
	for i := range m.rows {
		tag := " ⚑ debug copy"
		if m.rows[i].NameSuffix != "" {
			// Already carrying §41e's ephemeral-container ⚑ — don't repeat
			// the glyph, just add the second fact.
			tag = " · debug copy"
		}
		if m.session.DebugCopies.Contains(m.rows[i].Namespace, m.rows[i].Name) {
			m.rows[i].NameSuffix += tag
		}
	}
}

// selectedPodStatusAndWaiting reads the loaded pod's actual API phase and
// whether any container currently sits Waiting. The phase, not the display
// Reason, is load-bearing for terminal Job pods: kubectl exec rejects both
// Succeeded and Failed.
func (m Model) selectedPodStatusAndWaiting(namespace, podName string) (string, bool) {
	pod, ok := m.pods[kube.PodKey(namespace, podName)]
	if !ok {
		return "", false
	}
	return podStatusAndWaiting(pod)
}

func podStatusAndWaiting(pod kube.Pod) (string, bool) {
	for _, c := range pod.ContainerInfos {
		if c.State == "Waiting" {
			return pod.Status, true
		}
	}
	return pod.Status, false
}

// shellProbingNote is the keybar line shown while the 'x' shell probe runs.
func shellProbingNote(podName string) string {
	return podName + ": checking for a shell…"
}
