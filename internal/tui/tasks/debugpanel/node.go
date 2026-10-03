package debugpanel

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// cyclePodProfile steps the shared §41b/§41c profile field ('p' in either
// pod mode), preserving the selection when the user toggles modes.
func (m *Model) cyclePodProfile() { m.podProfile = m.podProfile.Next() }

// cycleNodeProfile steps §41d's profile field ('p' on a node debug panel) —
// the same field the retired 's' NodeShell verb hardcoded to "sysadmin".
func (m *Model) cycleNodeProfile() { m.nodeProfile = m.nodeProfile.Next() }

// nodeDebugPodPrefix is kubectl's own naming convention for the pod
// `kubectl debug node/X` creates (node-debugger-<node>-<random suffix>) —
// no flag controls it, unlike copy mode's caller-chosen --copy-to name
// (kube/debug.go's NodeDebugSpec doc comment).
func nodeDebugPodPrefix(node string) string { return "node-debugger-" + node + "-" }

// nodeDebugLookupMaxAttempts bounds findNodeDebugPodCmd's retry. The Pod
// cache is one of the app's three eager informers (docs/lazy-informers.md),
// so its watch event for a pod kubectl created when the debug session
// started — minutes before the shell typically exits — has almost always
// already landed by the time handleLaunchResult runs; this only covers the
// rare case it hasn't.
const nodeDebugLookupMaxAttempts = 6

// nodeDebugCreationSkew widens findNodeDebugPodCmd's "created no earlier
// than launch" cut-off. The API server stores CreationTimestamp at second
// precision, and kubectl usually creates the pod within the same second as
// launchedAt (a local time.Now() with nanoseconds), so comparing the two
// directly missed the pod most of the time; the allowance also absorbs clock
// skew between this machine and the API server. The node-debugger-<node>-
// prefix plus "newest wins" still keep an older orphan out.
const nodeDebugCreationSkew = 5 * time.Second

// nodeDebugPodLookupMsg carries one findNodeDebugPodCmd attempt's result.
type nodeDebugPodLookupMsg struct {
	after     time.Time
	attempt   int
	namespace string
	name      string
	found     bool
	created   time.Time
}

// nodeDebugRetryDueMsg fires after a failed lookup's backoff tick
// (tui.CacheSyncRetryInterval) — the same tick-then-reload split
// nodedetail's own scheduleReload/reloadDueMsg uses, so the actual cache
// read only ever happens from a message handler, never inside a Tick
// callback.
type nodeDebugRetryDueMsg struct {
	after   time.Time
	attempt int
}

// findNodeDebugPodCmd searches the already-warm cluster-wide Pod cache
// (spec.nodeName == m.nodeName, name prefixed node-debugger-<node>-,
// created no earlier than after, less nodeDebugCreationSkew) for the pod
// kubectl just created — the only way the panel can learn the name, since
// kubectl prints it to the terminal the user just saw but never reports it
// back to the caller.
// after excludes an older orphaned node-debugger pod already sitting on
// the same node from an earlier session (kube/debug.go's NodeDebugSpec doc
// comment: kubectl leaves these behind — the exact bug report §41d's
// CLEAN UP prompt exists to stop repeating). A nil lister (Config.Lister
// unset) reports not-found on the first attempt like any other lookup
// failure, so the panel falls back to popping back rather than ever
// showing a broken CLEAN UP prompt.
func (m Model) findNodeDebugPodCmd(after time.Time, attempt int) tea.Cmd {
	lister := m.lister
	node := m.nodeName
	if lister == nil {
		return func() tea.Msg { return nodeDebugPodLookupMsg{after: after, attempt: attempt} }
	}
	parent := m.session.ClusterContext()
	cutoff := after.Truncate(time.Second).Add(-nodeDebugCreationSkew)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		objs, err := lister.ListRaw(ctx, kube.KindPod, "")
		if err != nil {
			return nodeDebugPodLookupMsg{after: after, attempt: attempt}
		}
		prefix := nodeDebugPodPrefix(node)
		var best *corev1.Pod
		for _, obj := range objs {
			p, ok := obj.(*corev1.Pod)
			if !ok || p.Spec.NodeName != node || !strings.HasPrefix(p.Name, prefix) {
				continue
			}
			if p.CreationTimestamp.Time.Before(cutoff) {
				continue
			}
			if best == nil || p.CreationTimestamp.After(best.CreationTimestamp.Time) {
				best = p
			}
		}
		if best == nil {
			return nodeDebugPodLookupMsg{after: after, attempt: attempt}
		}
		return nodeDebugPodLookupMsg{namespace: best.Namespace, name: best.Name, found: true, created: best.CreationTimestamp.Time}
	}
}

// handleNodeDebugPodLookup applies one findNodeDebugPodCmd attempt: found
// stages §41d's own CLEAN UP prompt (cleanupPrompt carrying the pod's own
// discovered namespace, mirroring §41c's); not-found retries on
// tui.CacheSyncRetryInterval up to nodeDebugLookupMaxAttempts, then gives
// up and pops back — kubectl already told the user the pod's name on their
// own terminal, kute just couldn't confirm it from here.
func (m *Model) handleNodeDebugPodLookup(msg nodeDebugPodLookupMsg) (tea.Model, tea.Cmd) {
	if msg.found {
		m.cleanup = &cleanupPrompt{namespace: msg.namespace, name: msg.name, startedAt: msg.created}
		return m, nil
	}
	if msg.attempt+1 >= nodeDebugLookupMaxAttempts {
		return m, func() tea.Msg { return tui.BackMsg{} }
	}
	after, attempt := msg.after, msg.attempt+1
	return m, tea.Tick(tui.CacheSyncRetryInterval, func(time.Time) tea.Msg {
		return nodeDebugRetryDueMsg{after: after, attempt: attempt}
	})
}
