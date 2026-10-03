package debugpanel

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/state"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// launchResultMsg carries a kubectl debug tea.ExecProcess's exit outcome.
// tgt/mode distinguish which of the three launches produced it, since a
// clean exit means something different for each: an attach launch pops
// back (same contract as tasks/execpicker's execResultMsg), while a copy
// launch and a node-debug launch both stay on the panel — copy already
// knows the pod's name and shows §41c's CLEAN UP prompt immediately; node
// debug doesn't (kubectl auto-generates it) and hands off to
// findNodeDebugPodCmd (node.go) to find it in the Pod cache first.
type launchResultMsg struct {
	err      error
	tgt      target
	mode     podMode
	copyName string
	// launchedAt is the node target's own correlation floor for
	// findNodeDebugPodCmd — unused for a Pod target.
	launchedAt time.Time
}

// isProd mirrors every other screen's package-local seam (e.g.
// nodedetail/edit.go's isProd): whether the active context is tagged prod
// in ~/.config/kute/config.yaml.
func (m Model) isProd() bool {
	if m.session == nil {
		return false
	}
	return m.session.Config.IsProd(m.session.Location.Context)
}

// pasteTarget is the open free-text field, if any — mirrors forwardpicker's
// own resolver.
func (m *Model) pasteTarget() tui.PasteTarget {
	if m.editingField == fieldNone {
		return nil
	}
	return tui.PasteInto(&m.editInput)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cmd, ok := tui.RoutePaste(msg, m.pasteTarget()); ok {
		return m, cmd
	}
	// CLEAN UP's age renders against this stamp, never the clock (pure
	// render); every message moves it on.
	m.now = time.Now()
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
		m.actions.SetOffline(m.conn.Offline())
	case launchResultMsg:
		return m.handleLaunchResult(msg)
	case nodeDebugPodLookupMsg:
		return m.handleNodeDebugPodLookup(msg)
	case copyPodLookupMsg:
		// The exit error stays on the feedback line alongside the prompt.
		if msg.found && m.cleanup == nil {
			m.stageCopyCleanup(msg.name, msg.created)
		}
	case nodeDebugRetryDueMsg:
		return m, m.findNodeDebugPodCmd(msg.after, msg.attempt)
	case actions.RunningMsg:
		// A slow write's keybar line; the returned command is the only
		// path its ResultMsg arrives by.
		return m, m.actions.HandleRunning(msg)
	case actions.ResultMsg:
		return m.handleActionResult(msg)
	case accessReviewedMsg:
		m.handleAccessReviewed(msg)
	case tea.KeyPressMsg:
		// Any key acknowledges the last action's outcome line; the key
		// still does what it would have.
		m.actions.DismissResult()
		return m.updateKey(msg)
	}
	return m, nil
}

func (m *Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.editingField != fieldNone {
		return m.updateEditKey(msg)
	}
	if m.actions.Active() {
		return m.updateActionsConfirmKey(msg)
	}
	if m.launchPending {
		return m.updateLaunchConfirmKey(msg)
	}
	switch msg.String() {
	case "ctrl+q", "ctrl+c":
		return m, tea.Quit
	case "esc":
		return m, func() tea.Msg { return tui.BackMsg{} }
	case "enter":
		if m.accessState == accessChecking || m.accessState == accessDenied {
			return m, nil
		}
		// The launch is Mutating: refused while offline, like every other
		// mutating verb (docs/design README.md §52) — the keybar has
		// already dropped the hint and says "mutating actions disabled".
		if m.launchVerb().HiddenWhileOffline(m.conn.Offline()) {
			return m, nil
		}
		return m, m.beginLaunch()
	case "m":
		if m.tgt == targetPod {
			before := m.mode
			m.cycleMode()
			if m.mode != before {
				return m, m.startAccessReview()
			}
		}
	case "i":
		m.beginImageOrCopyNameEdit()
	case "t":
		m.cycleTargetOrContainer()
	case "p":
		m.cycleProfile()
	case "e":
		if m.tgt == targetPod && m.mode == modeCopy {
			m.beginEntrypointEdit()
		}
	case "s":
		if m.tgt == targetPod && m.mode == modeCopy {
			m.copyShareProcesses = !m.copyShareProcesses
		}
	case verbs.DebugCleanup.Key:
		if m.cleanup != nil {
			return m, m.beginCleanupDelete()
		}
	case "w":
		if m.accessState == accessDenied && m.openWhoCan != nil {
			q := m.accessQuery()
			task, cmd := m.openWhoCan(q.Verb, q.Resource, q.Namespace, m.width, m.height)
			if task != nil {
				return task, cmd
			}
		}
	}
	return m, nil
}

// beginImageOrCopyNameEdit opens 'i's one free-text field for the current
// target/mode — image for an attach or node launch, copy name for a copy
// launch (the mode's only freeform field; entrypoint has its own key).
func (m *Model) beginImageOrCopyNameEdit() {
	switch {
	case m.tgt == targetNode:
		m.beginFieldEdit(fieldImage, m.nodeImage)
	case m.mode == modeAttach:
		m.beginFieldEdit(fieldImage, m.attachImage)
	default:
		m.beginFieldEdit(fieldCopyName, m.copyName)
	}
}

func (m *Model) beginEntrypointEdit() {
	m.beginFieldEdit(fieldEntrypoint, m.copyEntrypoint)
}

func (m *Model) beginFieldEdit(field fieldID, value string) {
	m.editingField = field
	m.editInput = tui.NewTextInput(m.Theme())
	m.editInput.SetValue(value)
	m.editInput.CursorEnd()
	m.editInput.Focus()
}

func (m *Model) updateEditKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.editingField = fieldNone
		m.editInput.Blur()
		return m, nil
	case "enter":
		m.commitFieldEdit()
		return m, nil
	case "tab":
		// Recents only ever apply to the image field (state schema v3,
		// PerContext.RecentDebugImages) — copy name and entrypoint don't
		// get them, per that field's own doc comment. A non-image field
		// falls through to the default branch below, same as any other
		// key it doesn't special-case.
		if m.editingField == fieldImage {
			m.cycleRecentImage()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.editInput, cmd = m.editInput.Update(msg)
	return m, cmd
}

// recentImages is this context's persisted image recents (state schema v3,
// PerContext.RecentDebugImages) — most-recent-first, image only.
func (m Model) recentImages() []string {
	if m.session == nil {
		return nil
	}
	return m.session.State.PerContext[m.session.Location.Context].RecentDebugImages
}

// cycleRecentImage is 'tab' inside the open image field — advances the
// buffer to the recent image after whichever one is currently showing,
// wrapping back to the most-recent entry once the list is exhausted. A
// buffer that doesn't match any recent (the field's default, or a value
// the user is mid-typing) jumps straight to the most-recent entry.
func (m *Model) cycleRecentImage() {
	recents := m.recentImages()
	if len(recents) == 0 {
		return
	}
	current := m.editInput.Value()
	next := recents[0]
	for i, r := range recents {
		if r == current && i+1 < len(recents) {
			next = recents[i+1]
			break
		}
	}
	m.editInput.SetValue(next)
	m.editInput.CursorEnd()
}

// pushRecentImage records image as this context's most-recent debug image
// (state schema v3, PerContext.RecentDebugImages) — called from launchCmd
// for the two target/mode combinations that have an image field (node
// debug, pod attach). Copy mode reuses the pod's own container image, so
// it never has one to record.
func (m Model) pushRecentImage(image string) {
	if m.session == nil || image == "" {
		return
	}
	ctx := m.session.Location.Context
	if m.session.State.PerContext == nil {
		m.session.State.PerContext = map[string]state.PerContext{}
	}
	pc := m.session.State.PerContext[ctx]
	pc.RecentDebugImages = state.PushRecent(pc.RecentDebugImages, image)
	m.session.State.PerContext[ctx] = pc
}

// commitFieldEdit closes the open field, keeping the previous value on an
// empty commit rather than accepting a blank image/copy-name/entrypoint.
func (m *Model) commitFieldEdit() {
	value := strings.TrimSpace(m.editInput.Value())
	if value != "" {
		switch m.editingField {
		case fieldImage:
			if m.tgt == targetNode {
				m.nodeImage = value
			} else {
				m.attachImage = value
			}
		case fieldCopyName:
			m.copyName = value
		case fieldEntrypoint:
			m.copyEntrypoint = value
		}
	}
	m.editingField = fieldNone
	m.editInput.Blur()
}

// cycleTargetOrContainer is 't' — the attach mode's target-container field
// and the copy mode's container field are mutually exclusive (one panel
// state at a time), so one key serves both.
func (m *Model) cycleTargetOrContainer() {
	if m.tgt != targetPod {
		return
	}
	if m.mode == modeAttach {
		m.cycleAttachTarget()
	} else {
		m.cycleCopyContainer()
	}
}

// cycleProfile is 'p'. Pod attach and copy modes share one profile; node
// debug keeps its independent sysadmin-default selection.
func (m *Model) cycleProfile() {
	switch m.tgt {
	case targetNode:
		m.cycleNodeProfile()
	case targetPod:
		m.cyclePodProfile()
	}
}

// beginLaunch resolves 'enter' through the target's own policy row —
// verbs.NodeDebug for a node, verbs.PodDebug for a pod — which decides
// whether to launch immediately (TierNone, outside PROD — for a node,
// reproduces the retired 's' NodeShell verb's zero-friction launch) or
// stage launchPending for one inline y/N line first (TierInline, PROD
// only) — the same bespoke, non-actions.Controller gate
// nodedetail/edit.go's beginEdit uses, since the actual launch is a
// tea.ExecProcess handoff, never a Mutator call.
func (m *Model) beginLaunch() tea.Cmd {
	if verbs.Resolve(m.launchVerb(), verbs.Target{}, m.isProd()).Tier == verbs.TierNone {
		return m.launchCmd()
	}
	m.launchPending = true
	return nil
}

// launchVerb is the target's own launch verb — verbs.NodeDebug for a node,
// verbs.PodDebug for a pod.
func (m Model) launchVerb() verbs.Verb {
	if m.tgt == targetNode {
		return verbs.NodeDebug
	}
	return verbs.PodDebug
}

func (m *Model) updateLaunchConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case verbs.ConfirmAccept.Key:
		m.launchPending = false
		if m.launchVerb().HiddenWhileOffline(m.conn.Offline()) {
			// The connection dropped while the PROD y/N was showing.
			return m, nil
		}
		return m, m.launchCmd()
	case verbs.ConfirmDecline.Key, verbs.ConfirmCancel.Key:
		m.launchPending = false
	}
	return m, nil
}

// launchCmd suspends the program and hands the tty to kubectl (tea.ExecProcess),
// dispatching to whichever of the three debug builders (internal/kube/debug.go)
// applies to the current target/mode. demo short-circuits before building a
// real kubectl command (kube.ErrDemoUnavailable's own doc comment): there's
// no cluster behind kube/fake for a real tty to attach to.
func (m Model) launchCmd() tea.Cmd {
	if m.tgt == targetNode {
		m.pushRecentImage(m.nodeImage)
		if m.demo {
			return demoLaunchResultCmd(launchResultMsg{tgt: targetNode})
		}
		launchedAt := time.Now()
		spec := kube.NodeDebugSpec(m.session.CommandTarget(), m.nodeName, m.nodeImage, m.nodeProfile)
		return tea.ExecProcess(spec, func(err error) tea.Msg {
			return launchResultMsg{err: err, tgt: targetNode, launchedAt: launchedAt}
		})
	}
	if m.mode == modeAttach {
		m.pushRecentImage(m.attachImage)
		target := m.attachTargetContainer().Name
		if m.demo {
			return demoLaunchResultCmd(launchResultMsg{tgt: targetPod, mode: modeAttach})
		}
		spec := kube.PodDebugAttachSpec(m.session.CommandTarget(), m.namespace, m.podName, m.attachImage, target, m.podProfile)
		return tea.ExecProcess(spec, func(err error) tea.Msg {
			return launchResultMsg{err: err, tgt: targetPod, mode: modeAttach}
		})
	}
	container := m.copyContainer().Name
	copyName := m.copyName
	if m.demo {
		return demoLaunchResultCmd(launchResultMsg{tgt: targetPod, mode: modeCopy, copyName: copyName})
	}
	launchedAt := time.Now()
	spec := kube.PodDebugCopySpec(m.session.CommandTarget(), m.namespace, m.podName, copyName, container, m.copyEntrypoint, m.copyShareProcesses, m.podProfile)
	return tea.ExecProcess(spec, func(err error) tea.Msg {
		return launchResultMsg{err: err, tgt: targetPod, mode: modeCopy, copyName: copyName, launchedAt: launchedAt}
	})
}

// demoLaunchResultCmd stamps kube.ErrDemoUnavailable onto an otherwise
// fully-populated launchResultMsg, so handleLaunchResult's per-target/mode
// routing (attach/node pop back, copy shows CLEAN UP) never has to special-
// case demo mode — it only ever sees the same message shape a real launch
// would produce, just with a non-nil err.
func demoLaunchResultCmd(msg launchResultMsg) tea.Cmd {
	msg.err = kube.ErrDemoUnavailable
	return func() tea.Msg { return msg }
}

// handleLaunchResult routes a completed launch per target/mode (docs/design
// v.0.11.0.dc.html §41b/§41c/§41d): an attach-mode exit pops back, matching
// execpicker's own execResultMsg contract; copy mode and node debug both
// stay on the panel toward showing the CLEAN UP prompt instead — copy mode
// already knows the pod's name and shows it right away, registering the
// new pod with Session.DebugCopies so the pods table can tag it for the
// rest of the session; node debug doesn't know the name yet (kubectl
// auto-generates it) and hands off to findNodeDebugPodCmd (node.go) to
// find it in the Pod cache first. The node-target error string is the
// retired 's' NodeShell verb's exact wording, reused verbatim rather than
// reworded.
func (m *Model) handleLaunchResult(msg launchResultMsg) (tea.Model, tea.Cmd) {
	if msg.tgt == targetNode {
		if msg.err != nil {
			m.feedback = "node shell exited: " + msg.err.Error()
			return m, nil
		}
		m.feedback = ""
		return m, m.findNodeDebugPodCmd(msg.launchedAt, 0)
	}
	if msg.mode == modeCopy {
		if msg.err != nil {
			// kubectl debug --copy-to creates the copy before attaching, so a
			// failed attach, an interrupted image pull or a non-zero shell
			// exit can still leave it running — look before giving up on it.
			m.feedback = "debug exited: " + msg.err.Error()
			return m, m.findCopyPodCmd(msg.copyName)
		}
		m.feedback = ""
		m.stageCopyCleanup(msg.copyName, msg.launchedAt)
		return m, nil
	}
	if msg.err != nil {
		m.feedback = "debug exited: " + msg.err.Error()
		return m, nil
	}
	return m, func() tea.Msg { return tui.BackMsg{} }
}

// stageCopyCleanup registers a debug copy with Session.DebugCopies (so the
// pods table tags it for the rest of the session) and shows §41c's CLEAN UP
// prompt for it. startedAt is when the copy was created (zero when unknown),
// for the prompt's "still running · 4m".
func (m *Model) stageCopyCleanup(name string, startedAt time.Time) {
	ctxName := ""
	if m.session != nil {
		ctxName = m.session.Location.Context
		if m.session.DebugCopies != nil {
			m.session.DebugCopies.Add(ctxName, m.namespace, name)
		}
	}
	m.cleanup = &cleanupPrompt{context: ctxName, namespace: m.namespace, name: name, startedAt: startedAt}
}

// copyPodLookupMsg is findCopyPodCmd's answer: whether the copy a failed
// copy-mode launch was creating exists in the Pod cache.
type copyPodLookupMsg struct {
	name    string
	found   bool
	created time.Time
}

// findCopyPodCmd checks the eager Pod cache for name in the panel's
// namespace. A nil lister or a failed read reports not-found, leaving just
// the exit error — the same fallback findNodeDebugPodCmd takes.
func (m Model) findCopyPodCmd(name string) tea.Cmd {
	lister := m.lister
	namespace := m.namespace
	if lister == nil {
		return func() tea.Msg { return copyPodLookupMsg{name: name} }
	}
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		objs, err := lister.ListRaw(ctx, kube.KindPod, namespace)
		if err != nil {
			return copyPodLookupMsg{name: name}
		}
		for _, obj := range objs {
			if acc, err := meta.Accessor(obj); err == nil && acc.GetNamespace() == namespace && acc.GetName() == name {
				return copyPodLookupMsg{name: name, found: true, created: acc.GetCreationTimestamp().Time}
			}
		}
		return copyPodLookupMsg{name: name}
	}
}

// beginCleanupDelete resolves ctrl-d on the CLEAN UP prompt — an ordinary
// kube.Mutator.DeleteResource call, so unlike the launch itself this routes
// through actions.Controller (verbs.DebugCleanup, Delete's own policy row)
// exactly as
// browse's own pod delete does (PROD escalates to the type-the-name modal
// the same way).
func (m *Model) beginCleanupDelete() tea.Cmd {
	if m.cleanup == nil {
		return nil
	}
	return m.actions.Begin(actions.Request{Verb: verbs.DebugCleanup, Prod: m.isProd()}, tui.TaskAction{
		ID:    "delete-" + string(kube.KindPod) + "-" + m.cleanup.namespace + "/" + m.cleanup.name,
		Label: fmt.Sprintf("Delete Pod %s?", m.cleanup.name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindPod),
			ResourceName: m.cleanup.name,
			Namespace:    m.cleanup.namespace,
			Verb:         "delete",
			IsMutating:   true,
		},
	})
}

// updateActionsConfirmKey routes keys while the cleanup delete's confirm is
// showing — the ordinary y/N outside PROD, or the type-the-name modal in
// PROD (verbs.Resolve's typed-name policy for verbs.DebugCleanup) — through
// the controller's one router (actions.HandleConfirmKey). Begun as
// DebugCleanup rather than Delete, it is never offered a force delete.
func (m *Model) updateActionsConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	cmd, _ := m.actions.HandleConfirmKey(msg)
	return m, cmd
}

// handleActionResult applies the cleanup delete's outcome — success clears
// cleanup, removes the DebugCopies tag (pod-copy mode only — a node-debug
// pod was never added to it, since it isn't a copy of anything), and pops
// back; failure leaves the CLEAN UP prompt showing with the server's error
// (actions.Controller's own HandleResult already surfaces it).
func (m *Model) handleActionResult(msg actions.ResultMsg) (tea.Model, tea.Cmd) {
	m.actions.HandleResult(msg)
	if msg.Err == nil && m.cleanup != nil {
		if m.tgt == targetPod && m.session != nil && m.session.DebugCopies != nil {
			m.session.DebugCopies.Remove(m.cleanup.context, m.cleanup.namespace, m.cleanup.name)
		}
		m.cleanup = nil
		return m, func() tea.Msg { return tui.BackMsg{} }
	}
	return m, nil
}
