// Package debugpanel is §41b/§41c/§41d (docs/design v.0.11.0.dc.html §41):
// the centered panel `x` pushes for a Pod with no shell in any container, a
// Pod that won't stay running, or a Node — three sub-screens sharing one
// panel shell (in-place fields, a "will run" line, a PROD-only inline y/N
// gate, tea.ExecProcess launch) with a target (pod/node) and, for a pod, a
// mode (attach/copy) discriminant. §41a's fork on tasks/execpicker's own
// shell-less row, and browse/poddetail's own shell-precheck, decide when
// this package opens instead of execpicker/execCmd. Once open, the panel
// performs the authoritative access review for its current pod mode before
// it enables launch.
package debugpanel

import (
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
)

// target discriminates which of §41's objects this panel is debugging.
type target int

const (
	targetPod target = iota
	targetNode
)

// podMode discriminates §41b (attach) from §41c (copy) — pod target only.
type podMode int

const (
	modeAttach podMode = iota
	modeCopy
)

// fieldID names the one free-text field a given target/mode combination
// ever has open at once — every other field (target/container/profile/
// mode/share-processes) is a single-key cycle, never a text buffer.
type fieldID int

const (
	fieldNone fieldID = iota
	fieldImage
	fieldCopyName
	fieldEntrypoint
)

// Config are debugpanel's dependencies and the target object's own data,
// per repo convention (package-local Config struct, New fills zero
// values).
type Config struct {
	Session    *tui.Session
	Mutator    kube.Mutator // §41c/§41d's cleanup delete only — the launch itself never calls it
	Access     AccessReviewer
	OpenWhoCan OpenWhoCanFunc

	// Lister backs §41d's post-exit node-debug pod discovery only (node.go):
	// kubectl auto-generates the node-debugger pod's name and never reports
	// it back to the caller (unlike copy mode's caller-chosen --copy-to
	// name), so the panel finds it by reading the already-warm cluster-wide
	// Pod cache — one of the app's three eager informers — rather than a
	// live API call. Unused for a Pod target.
	Lister resources.RawLister

	// Pod target
	Namespace  string
	Name       string
	Containers []kube.ContainerInfo
	// PodPhase/Waiting pick the default mode (a non-Running API phase, or a
	// Waiting container, default to modeCopy and hard-disable attach — the
	// confirmed decision, docs/design v.0.11.0.dc.html §41c) and are also
	// used to re-evaluate that gate live if the row's state changes while
	// the panel is open.
	PodPhase string
	Waiting  bool
	// InitialTargetContainer pre-selects the attach mode's target-container
	// field — set by tasks/execpicker's own §41a fork, which already knows
	// which specific container has no shell (the one the cursor was on).
	// Empty falls back to the first non-sidecar container, same as opening
	// the panel any other way.
	InitialTargetContainer string

	// Node target
	NodePodCount int
	// NodeShellImage is the caller's configured override for the
	// node-debug default image (config.Config.NodeShellImage, the retired
	// NodeShell verb's own knob). Empty falls back to
	// kube.DefaultNodeShellImage, same as before this field existed.
	NodeShellImage string

	// Target selects which of the two shapes above applies. Namespace is
	// ignored for a Node target (kubectl debug node/X takes no -n).
	IsNode bool

	// Demo is true for --demo — see browse.Config.Demo's doc comment;
	// launchCmd checks it before building a real kubectl debug command.
	Demo bool
}

// cleanupPrompt is §41c/§41d's post-exit "CLEAN UP" band — set once a
// copy-mode pod launch exits cleanly, or once a node-debug launch's
// kubectl-generated pod is found in the Pod cache (node.go); cleared by
// either a completed ctrl-d delete or esc ("keep"). namespace is carried
// explicitly rather than falling back to the model's own m.namespace,
// since a node-debug pod's namespace is only known once it's found —
// m.namespace is unset for a Node target.
type cleanupPrompt struct {
	// context is the kubeconfig context a copy was created in — its
	// Session.DebugCopies key (unset for a node-debugger pod, never added).
	context   string
	namespace string
	name      string
	startedAt time.Time
}

type Model struct {
	width, height int

	session *tui.Session
	mutator kube.Mutator
	lister  resources.RawLister

	tgt target

	// Pod target
	namespace, podName string
	containers         []kube.ContainerInfo
	podPhase           string
	waiting            bool

	mode podMode

	attachImage     string
	attachTargetIdx int
	// podProfile is shared by attach and copy mode, so changing mode does
	// not silently discard the user's selected debugging privileges.
	podProfile kube.DebugProfile

	copyName           string
	copyContainerIdx   int
	copyEntrypoint     string
	copyShareProcesses bool

	// Node target
	nodeName     string
	nodeImage    string
	nodeProfile  kube.DebugProfile
	nodePodCount int

	editingField fieldID
	editInput    textfield.Model

	launchPending bool
	cleanup       *cleanupPrompt
	// now is the clock as of the last message — CLEAN UP's age renders
	// from it so Keybar stays a pure function of the model.
	now          time.Time
	actions      actions.Controller // cleanup delete only (verbs.Delete/kube.Mutator) — the debug launch itself is a bespoke gate (launchPending), never routed through this
	access       AccessReviewer
	openWhoCan   OpenWhoCanFunc
	accessState  accessState
	accessResult kube.AccessReviewResult
	accessErr    error
	accessGen    int

	feedback string
	conn     kube.ConnState
	demo     bool
}

func New(cfg Config) Model {
	m := Model{
		width:      tui.DefaultWidth,
		height:     tui.DefaultHeight,
		session:    cfg.Session,
		mutator:    cfg.Mutator,
		lister:     cfg.Lister,
		actions:    actions.New(cfg.Mutator),
		access:     cfg.Access,
		openWhoCan: cfg.OpenWhoCan,
		demo:       cfg.Demo,
	}
	if cfg.IsNode {
		m.tgt = targetNode
		m.nodeName = cfg.Name
		m.nodeImage = cfg.NodeShellImage
		if m.nodeImage == "" {
			m.nodeImage = kube.DefaultNodeShellImage
		}
		m.nodeProfile = kube.ProfileSysadmin
		m.nodePodCount = cfg.NodePodCount
		return m
	}
	m.tgt = targetPod
	m.namespace = cfg.Namespace
	m.podName = cfg.Name
	m.containers = cfg.Containers
	m.podPhase = cfg.PodPhase
	m.waiting = cfg.Waiting

	m.attachImage = kube.DefaultDebugImage
	m.attachTargetIdx = containerIndexOrDefault(cfg.Containers, cfg.InitialTargetContainer)
	m.podProfile = kube.ProfileGeneral

	m.copyName = kube.DefaultDebugCopyName(cfg.Name)
	m.copyContainerIdx = firstNonSidecarIndex(cfg.Containers)
	m.copyEntrypoint = kube.DefaultDebugCopyEntrypoint
	m.copyShareProcesses = true

	if m.podWontStayRunning() {
		m.mode = modeCopy
	} else {
		m.mode = modeAttach
	}
	if m.access != nil {
		m.accessState = accessChecking
		m.accessGen = 1
	} else {
		m.accessState = accessAllowed
	}
	return m
}

// firstNonSidecarIndex is the panel's default target/container selection —
// the pod's first ordinary container, matching how browse's own
// openSelectedExec picks a default before a picker/panel ever opens.
func firstNonSidecarIndex(containers []kube.ContainerInfo) int {
	return max(slices.IndexFunc(containers, func(c kube.ContainerInfo) bool { return !c.IsSidecar }), 0)
}

// containerIndexOrDefault resolves name to its index in containers, falling
// back to firstNonSidecarIndex when name is empty or not found — the
// tasks/execpicker fork (§41a) pre-selects the specific shell-less
// container the cursor was on; every other entry point leaves name empty.
func containerIndexOrDefault(containers []kube.ContainerInfo, name string) int {
	if name != "" {
		if i := slices.IndexFunc(containers, func(c kube.ContainerInfo) bool { return c.Name == name }); i >= 0 {
			return i
		}
	}
	return firstNonSidecarIndex(containers)
}

// podWontStayRunning reports whether §41c's copy-only gate applies — a
// terminal/not-yet-running phase or a Waiting container means an ephemeral
// attach has nothing running to attach to.
func (m Model) podWontStayRunning() bool {
	return kube.PodWontStayRunning(m.podPhase, m.waiting)
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width, m.height = size.Width, size.Height
}

func (m Model) Init() tea.Cmd {
	if m.tgt != targetPod || m.access == nil {
		return nil
	}
	return m.accessReviewCmd(m.accessGen, m.accessQuery())
}
