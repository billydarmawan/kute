// Package execpicker is 10a (docs/design/README.md §10a): the small
// centered panel 'x' pushes from tasks/browse or tasks/poddetail when the
// selected pod has more than one container — single-container pods exec
// immediately without this screen (mvp-plan.md §Phase 8, "skipped entirely
// for single-container pods"). Enter suspends the program and hands the tty
// to `kubectl exec` (kube.ExecSpec via tea.ExecProcess); a clean exit pops
// back to the pod that opened the picker, a non-zero exit shows a feedback
// line in place so the user can pick another container or back out.
package execpicker

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// ShellDetector answers which shells a container actually has, for 10a's
// right-aligned shells column. Declared here rather than in kube, per the
// repo's consuming-interface convention; kube.DetectShells is the real
// implementation and kube/fake supplies the demo one.
type ShellDetector interface {
	DetectShells(ctx context.Context, namespace, pod, container string) ([]string, error)
}

// shellProbeTimeout bounds one container's probe. Generous enough for a
// laggy link, short enough that a wedged exec can't leave the column saying
// "checking…" for the life of the screen.
const shellProbeTimeout = 5 * time.Second

// shellResult is one container's detection outcome. Absent from Model.shells
// entirely means still in flight; err non-nil means the probe couldn't run
// (rendered as unknown, never as "no shell"); a nil err with no shells is a
// real "this container has none".
type shellResult struct {
	shells []string
	err    error
}

// OpenDebugFunc pushes tasks/debugpanel (§41b) in attach mode, targeting
// the highlighted shell-less container — §41a's fork: pressing enter on a
// container this picker already knows has no shell routes here instead of
// a dead exec attempt (docs/design v.0.11.0.dc.html §41a: "a shell-less row
// is a route, not a dead ↵"). Always attach mode, never copy: the picker is
// only ever reached when at least one *other* container in the pod does
// have a shell (browse/poddetail's own pre-check skips straight to the
// panel when every container is shell-less), so the pod plainly has a
// running container to attach alongside.
type OpenDebugFunc func(namespace, podName string, containers []kube.ContainerInfo, target string, width, height int) (tea.Model, tea.Cmd)

// Config are execpicker's dependencies, per repo convention (package-local
// Config struct, New fills zero values). Shells may be nil — the picker then
// renders its shells column as unknown rather than probing.
type Config struct {
	Session    *tui.Session
	Namespace  string
	PodName    string
	Containers []kube.ContainerInfo
	Shells     ShellDetector
	// Probed is the shells already detected per container by the opener's
	// own 'x' pre-check (successful probes only, possibly empty). Those
	// rows render their result at once; Init probes only the rest.
	Probed    map[string][]string
	OpenDebug OpenDebugFunc
	// Demo is true for --demo — see browse.Config.Demo's doc comment;
	// execSelected checks it before building a real kubectl command.
	Demo bool
}

type Model struct {
	width, height int

	session    *tui.Session
	namespace  string
	podName    string
	containers []kube.ContainerInfo

	shells    ShellDetector
	openDebug OpenDebugFunc
	detected  map[string]shellResult
	demo      bool

	selected int
	// feedback is set after a non-zero kubectl exec exit (docs/design
	// README.md §10a's callback contract), or when a key is refused while
	// offline — empty otherwise.
	feedback string

	// conn is the last kube.ConnStateMsg forwarded by the root shell — the
	// header badge's real connection state (never a hardcoded "live").
	conn kube.ConnState
}

func New(cfg Config) Model {
	detected := make(map[string]shellResult, len(cfg.Probed))
	for container, shells := range cfg.Probed {
		detected[container] = shellResult{shells: shells}
	}
	return Model{
		width:      tui.DefaultWidth,
		height:     tui.DefaultHeight,
		session:    cfg.Session,
		namespace:  cfg.Namespace,
		podName:    cfg.PodName,
		containers: cfg.Containers,
		shells:     cfg.Shells,
		openDebug:  cfg.OpenDebug,
		detected:   detected,
		demo:       cfg.Demo,
	}
}

// Init probes, all at once, every container's shells not already in
// Config.Probed. The picker only ever renders
// for a multi-container pod, so this is a handful of one-shot reads on an
// explicitly-opened screen, not a poll — and every row renders immediately
// with "checking…" rather than blocking on them.
func (m Model) Init() tea.Cmd {
	if m.shells == nil {
		return nil
	}
	cmds := make([]tea.Cmd, 0, len(m.containers))
	for _, c := range m.containers {
		if _, known := m.detected[c.Name]; known {
			continue
		}
		cmds = append(cmds, m.detectShellsCmd(c.Name))
	}
	return tea.Batch(cmds...)
}

// detectShellsCmd probes one container off the update loop.
func (m Model) detectShellsCmd(container string) tea.Cmd {
	detector, namespace, pod := m.shells, m.namespace, m.podName
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, shellProbeTimeout)
		defer cancel()
		shells, err := detector.DetectShells(ctx, namespace, pod, container)
		return shellsMsg{container: container, shells: shells, err: err}
	}
}

// preferredShell is the shell enter will actually run for the container at
// index i: the first detected candidate (kube.ShellCandidates order, bash
// first). Empty when detection is pending, failed, or found none — which
// keeps kube.ExecSpec on its in-container bash-then-sh fallback.
func (m Model) preferredShell(i int) string {
	if i < 0 || i >= len(m.containers) {
		return ""
	}
	res, ok := m.detected[m.containers[i].Name]
	if !ok || res.err != nil || len(res.shells) == 0 {
		return ""
	}
	return res.shells[0]
}

// selectedContainerName is the highlighted row's container name, or "" when
// nothing's selected.
func (m Model) selectedContainerName() string {
	if m.selected < 0 || m.selected >= len(m.containers) {
		return ""
	}
	return m.containers[m.selected].Name
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width, m.height = size.Width, size.Height
}
