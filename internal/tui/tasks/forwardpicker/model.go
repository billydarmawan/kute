// Package forwardpicker is 13a (docs/design/README.md §13a): the small
// centered panel 'f' pushes from tasks/browse for a Pod/Service/Deployment
// row (docs/design README.md §13a, mirroring 10a's execpicker shape) —
// candidate ports discovered from the target object itself, a per-row local
// port pre-filled (and edited in place), a "will run" documentation line,
// and Enter starting the forward via kube.ForwardManager before popping
// back to the caller. Unlike execpicker/nodeshell, this never suspends the
// program — the forward keeps running in the background after the picker
// closes.
package forwardpicker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"
	apimeta "k8s.io/apimachinery/pkg/api/meta"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

// Config are forwardpicker's dependencies, per repo convention
// (package-local Config struct, interface-typed fields, New fills zero
// values).
type Config struct {
	Session  *tui.Session
	Lister   resources.RawLister
	Resolver kube.PodResolver
	Dialer   kube.ForwardDialer
	Manager  *kube.ForwardManager
	Target   kube.ForwardTarget
}

// portRow is one candidate port plus its editable local-port state.
type portRow struct {
	kube.PortOption
	localPort int
	// busyFrom is the originally pre-filled port when it turned out to be
	// bound already and localPort got bumped past it (docs/design
	// README.md §13a: "8080 busy → 18080") — 0 when no bump was needed.
	busyFrom int
	// podPort is the backing pod's container port the forward will dial:
	// Port itself for a Pod/Deployment option, the Service targetPort
	// resolved against resolvedPod for a Service — 0 when that couldn't be
	// resolved (no backing pod, or a named port the pod doesn't declare).
	podPort int32
	// manual marks the trailing free-entry row: the target declares no ports
	// at all, or none of them is the one wanted. A declared port is only a
	// hint — plenty of pods never declare the port they listen on, and
	// kubectl will happily forward any port to any of them — so the panel
	// always ends in a row that accepts one.
	manual bool
	// localEdited records that the local port was chosen deliberately (typed
	// on a declared row, or spelled as the left half of a `local:remote`
	// pair), so re-deriving the pre-fill from a newly typed remote port
	// leaves it alone.
	localEdited bool
	// editing/editInput are the row's in-place buffer. editRemote says which
	// half of the pair it writes: the local port on a declared row, whose
	// remote half is already known, and the remote port on the manual row,
	// which is the half that isn't.
	editing    bool
	editRemote bool
	editInput  textfield.Model
	// beforeEdit is the free-port row as it stood when its buffer opened.
	// That buffer drives the row live, so esc restores this rather than
	// leaving a half-typed port behind as if it had been committed.
	beforeEdit *portRow
	// pairErr says why the free-port buffer can't name a forward (a half
	// past 65535) — empty when it parses, or when nothing is typed yet.
	pairErr string
}

// portPairLimit is the manual row's buffer width — kubectl's `local:remote`
// spelling, both halves up to five digits plus the colon.
const portPairLimit = 11

type Model struct {
	width, height int

	session  *tui.Session
	lister   resources.RawLister
	resolver kube.PodResolver
	dialer   kube.ForwardDialer
	manager  *kube.ForwardManager
	target   kube.ForwardTarget

	state    tui.TaskState
	feedback string

	resolvedPod string // best-effort backing pod for Service/Deployment targets
	rows        []portRow
	selected    int

	conn kube.ConnState
}

func New(cfg Config) Model {
	return Model{
		width:    tui.DefaultWidth,
		height:   tui.DefaultHeight,
		session:  cfg.Session,
		lister:   cfg.Lister,
		resolver: cfg.Resolver,
		dialer:   cfg.Dialer,
		manager:  cfg.Manager,
		target:   cfg.Target,
		state:    tui.TaskStateLoading,
		feedback: "Loading ports...",
	}
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width, m.height = size.Width, size.Height
}

// portsLoadedMsg carries the target object's discovered ports plus its
// best-effort resolved backing pod (Service/Deployment targets only — a Pod
// target's resolvedPod is always just its own name).
type portsLoadedMsg struct {
	ports       []kube.PortOption
	podPorts    []int32 // parallel to ports; see portRow.podPort
	resolvedPod string
	err         error
}

func (m Model) Init() tea.Cmd {
	if m.lister == nil {
		return nil
	}
	target := m.target
	lister := m.lister
	resolver := m.resolver
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 10*time.Second)
		defer cancel()
		objs, err := lister.ListRaw(ctx, target.Kind, target.Namespace)
		if err != nil {
			return portsLoadedMsg{err: err}
		}
		for _, o := range objs {
			accessor, err := apimeta.Accessor(o)
			if err != nil || accessor.GetName() != target.Name {
				continue
			}
			ports := kube.ForwardablePorts(o)
			resolvedPod := target.Name
			if target.Kind != kube.KindPod {
				resolvedPod = ""
				if resolver != nil {
					if pod, rerr := resolver.ResolveForwardPod(ctx, target); rerr == nil {
						resolvedPod = pod
					}
				}
			}
			podPorts := make([]int32, len(ports))
			for i, p := range ports {
				podPorts[i] = p.Port
				if target.Kind != kube.KindService {
					continue
				}
				podPorts[i] = 0
				if resolvedPod != "" && resolver != nil {
					if n, rerr := resolver.ResolveForwardPort(ctx, target.Namespace, resolvedPod, p.TargetPort, p.Protocol); rerr == nil {
						podPorts[i] = n
					}
				}
			}
			return portsLoadedMsg{ports: ports, podPorts: podPorts, resolvedPod: resolvedPod}
		}
		return portsLoadedMsg{err: fmt.Errorf("%s %q not found", target.Kind, target.Name)}
	}
}

// preferredLocalPort is 13a's pre-fill rule: privileged remote ports (<1024)
// pre-fill 8000 above (80 → 8080); everything else pre-fills at the same
// number (9090 → 9090).
func preferredLocalPort(remote int32) int {
	if remote < 1024 {
		return int(remote) + 8000
	}
	return int(remote)
}

// pickLocalPort probes preferred for an available local bind, walking
// upward on a busy port (docs/design README.md §13a: "if the pre-fill is
// busy, the row says so inline and pre-fills the next free one"). A port in
// held — one an existing forward owns — counts as busy even when nothing is
// bound to it, since a Reconnecting forward holds no listener during its
// backoff. Returns the chosen port and, if it differs from preferred, the
// busy port that was skipped.
func pickLocalPort(preferred int, held map[int]bool) (chosen, busyFrom int) {
	for port := preferred; port < preferred+64 && port <= 65535; port++ {
		if !held[port] && portFree(port) {
			if port != preferred {
				return port, preferred
			}
			return port, 0
		}
	}
	return preferred, 0
}

// portFree reports whether port can be bound on both loopbacks a forward
// listens on — client-go binds 127.0.0.1 and ::1, and the copied
// "localhost:<port>" may resolve to either. A host with no IPv6 loopback
// can't bind ::1 at all, which isn't a collision, so only "address in use"
// counts as busy there.
func portFree(port int) bool {
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	ln6, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
	if err != nil {
		return !errors.Is(err, syscall.EADDRINUSE)
	}
	_ = ln6.Close()
	return true
}

// heldLocalPorts is the set of local ports every existing forward owns,
// whatever its state.
func (m Model) heldLocalPorts() map[int]bool {
	held := map[int]bool{}
	if m.manager == nil {
		return held
	}
	for _, s := range m.manager.List() {
		held[s.LocalPort] = true
	}
	return held
}

// declaredRows is how many ports came off the object itself. The trailing
// free-entry row never counts as one: "no declared ports" is a claim about
// the object, not about how many things the panel can forward.
func (m Model) declaredRows() int {
	n := 0
	for _, row := range m.rows {
		if !row.manual {
			n++
		}
	}
	return n
}

// row is the selected row, or the zero value when there's no selection.
func (m Model) row() portRow {
	if m.selected < 0 || m.selected >= len(m.rows) {
		return portRow{}
	}
	return m.rows[m.selected]
}

// deriveLocal keeps the manual row's local port pre-fill tracking the remote
// port typed so far (80 → 8080, 9090 → 9090), unless the local port was
// chosen deliberately. With no remote port typed there is nothing to pre-fill
// for, and the row shows none. Only probe — on commit — walks past a busy
// port: a bind probe per keystroke would run up to 128 net.Listen calls on
// the update loop for intermediate values nobody asked for.
func (m *Model) deriveLocal(row *portRow, probe bool) {
	if row.localEdited {
		return
	}
	if row.Port == 0 {
		row.localPort, row.busyFrom = 0, 0
		return
	}
	if !probe {
		row.localPort, row.busyFrom = preferredLocalPort(row.Port), 0
		return
	}
	row.localPort, row.busyFrom = pickLocalPort(preferredLocalPort(row.Port), m.heldLocalPorts())
}
