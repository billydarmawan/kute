// Package whocan is 22a (docs/design/README.md §22a): RBAC "who can" —
// a query, not a browser. It answers "who can <verb> <resource> [in
// <namespace>]" by walking (Cluster)RoleBindings → (Cluster)Roles entirely
// from the informer cache (kube.ResolveWhoCan) — no server round trip, so
// it works even for a user with no list/get RBAC access of their own.
//
// Reached via the goto corpus (`g "who"`, tui/goto.go's gotoWhoCanItem) or
// `w` from browse's 4b 403 card, pre-filled with the denied verb/resource/
// namespace. The query's v/k/n slots are edited through the one shared
// palette shell (tui/whocan.go's ScopeVerb/ScopeResource wiring, plus the
// ordinary namespace palette for 'n') rather than any bespoke input widget.
package whocan

import (
	"context"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
)

// WhoCanReader is the seam whocan needs — satisfied by *kube.Cluster
// (kube/rbac.go) and *fake.Cluster (kube/fake/fake.go) already.
type WhoCanReader interface {
	WhoCan(ctx context.Context, query kube.WhoCanQuery) (kube.WhoCanResult, error)
}

// OpenYAMLFunc pushes tasks/yamlview (8a) for a resolved row's backing
// RoleBinding/ClusterRoleBinding — 22a's "↵ opens the binding's YAML".
type OpenYAMLFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// Config are whocan's dependencies, per repo convention (package-local
// Config struct, interface-typed fields, New fills zero values). Verb/
// Resource/Namespace seed the initial query — Resource in kubectl's
// group-qualified spelling ("deployments.apps"; bare for the core group),
// which is also how the strip and the `auth can-i` line show it — browse's 'w' (4b) pre-fills
// the denied verb/resource/namespace; the goto corpus's plain `g "who"`
// entry leaves them at New's defaults.
type Config struct {
	Session     *tui.Session
	RBAC        WhoCanReader
	OpenYAML    OpenYAMLFunc
	Verb        string
	Resource    string
	Namespace   string
	LoadTimeout time.Duration
}

// whoCanRow is one displayed SUBJECT/KIND/VIA/SCOPE row — either a real
// resolved kube.WhoCanSubject or the synthetic pinned current-user verdict
// row (docs/design README.md §22a: "the current user pinned as a red ✕ row
// whose VIA explains the closest miss"). granted/via/scope are only
// meaningful when pinned — a non-pinned row reads them off subject
// directly.
type whoCanRow struct {
	subject kube.WhoCanSubject
	pinned  bool
	granted bool
	// unknown marks a pinned row whose identity is only a kubeconfig entry
	// name (kube.WhoCanResult.CurrentUserUnknown) — neutral, never a ✕.
	unknown bool
}

type Model struct {
	width, height int

	session  *tui.Session
	rbac     WhoCanReader
	openYAML OpenYAMLFunc
	timeout  time.Duration

	verb      string
	resource  string
	namespace string

	result   kube.WhoCanResult
	rows     []whoCanRow
	selected int
	// offset keeps the selected row within the table's rendered viewport —
	// mirrors nodedetail/routetable's own clampOffset/tableDataRows pattern
	// (update.go's clampOffset).
	offset int

	// conn is the last kube.ConnStateMsg forwarded by the root shell — the
	// header badge's real connection state, same as every other screen.
	conn kube.ConnState

	// reloadEpoch guards a debounced/in-flight load against a query edit
	// (v/k/n) that lands mid-flight — mirrors browse's own reloadEpoch.
	reloadEpoch int

	state    tui.TaskState
	feedback string
	spinner  spinner.Model
}

// loadedMsg carries one load()'s result.
type loadedMsg struct {
	epoch  int
	result kube.WhoCanResult
	err    error
}

func New(cfg Config) Model {
	if cfg.LoadTimeout == 0 {
		cfg.LoadTimeout = 10 * time.Second
	}
	verb := cfg.Verb
	if verb == "" {
		verb = "list"
	}
	state := tui.TaskStateLoading
	feedback := "Resolving who can " + verb + " " + cfg.Resource + "..."
	if cfg.RBAC == nil {
		state = tui.TaskStateError
		feedback = "no cluster connection"
	}
	return Model{
		width:     tui.DefaultWidth,
		height:    tui.DefaultHeight,
		session:   cfg.Session,
		rbac:      cfg.RBAC,
		openYAML:  cfg.OpenYAML,
		timeout:   cfg.LoadTimeout,
		verb:      verb,
		resource:  cfg.Resource,
		namespace: cfg.Namespace,
		state:     state,
		feedback:  feedback,
		spinner:   components.NewSpinner(),
	}
}

func (m Model) Init() tea.Cmd {
	if m.rbac == nil {
		return nil
	}
	return tea.Batch(m.load(), m.spinner.Tick)
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width, m.height = size.Width, size.Height
}

// WhoCanQuery satisfies tui.WhoCanScoped — lets the root shell seed the
// 'v'/'K' palette opens from the query currently showing.
func (m Model) WhoCanQuery() (verb, resource string) {
	return m.verb, m.resource
}

func (m Model) selectedRow() (whoCanRow, bool) {
	if m.selected < 0 || m.selected >= len(m.rows) {
		return whoCanRow{}, false
	}
	return m.rows[m.selected], true
}

// SwitchesNamespaceInPlace reports that this screen handles
// tui.SwitchNamespaceMsg itself (see tui.NamespaceSwitcher).
func (m Model) SwitchesNamespaceInPlace() bool { return true }

var _ tui.NamespaceSwitcher = (*Model)(nil)
