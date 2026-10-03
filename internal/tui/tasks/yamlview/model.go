// Package yamlview is 8a (docs/design/README.md §8a): a read-only,
// syntax-highlighted, foldable YAML view of any object, any kind — reached
// by 'y' from tasks/browse (any row), tasks/poddetail, and
// tasks/nodedetail. Live-updates its object's YAML on
// kube.ResourceChangedMsg (same kind-level granularity every other detail
// task uses), keeping the cursor's line position stable across a refetch.
package yamlview

import (
	"context"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
)

// YAMLReader is the seam for the yaml text itself — satisfied by
// *kube.Cluster and *fake.Cluster already (kube/yaml.go, kube/fake/fake.go).
type YAMLReader interface {
	GetYAML(ctx context.Context, kind kube.ResourceKind, namespace, name string) (text, resourceVersion string, err error)
}

// ManagedFieldsReader fetches an object's metadata.managedFields live.
// Optional: informer caches strip managedFields (it is bookkeeping this app
// reads nowhere else, and a large fraction of an object's bytes), so this
// screen — the only one that displays them — fetches them for the single
// object it is showing. A lister that doesn't implement it falls back to
// whatever the cached object carries, which is what --demo and the tests do.
type ManagedFieldsReader interface {
	GetManagedFields(ctx context.Context, kind kube.ResourceKind, namespace, name string) (string, error)
}

// Config are yamlview's dependencies, per repo convention (package-local
// Config struct, interface-typed fields, New fills zero values). Lister is
// the fallback source for managedFields when ManagedFields is unset
// (kube.GetYAML itself already strips managedFields before marshaling).
type Config struct {
	Session       *tui.Session
	Lister        resources.RawLister
	YAML          YAMLReader
	ManagedFields ManagedFieldsReader
	Kind          kube.ResourceKind
	Namespace     string
	Name          string
	LoadTimeout   time.Duration
}

type Model struct {
	width, height int

	session       *tui.Session
	lister        resources.RawLister
	yaml          YAMLReader
	managedFields ManagedFieldsReader
	timeout       time.Duration

	kind      kube.ResourceKind
	namespace string
	name      string

	resourceVersion string
	// lines is the displayed source text — for a Secret, already masked
	// (secret.go), so nothing reading it can surface a value.
	lines []string
	// copyText is what 'Y' copies: the loaded YAML for any other kind, the
	// plaintext-free base64 form for a Secret.
	copyText string
	// managedFieldsLines is metadata.managedFields' own marshaled content
	// (kube.ManagedFieldsYAML), spliced back in by applyManagedFields
	// (managedfields.go) once the user unfolds it — nil for objects with no
	// managed fields.
	managedFieldsLines []string
	folded             map[string]bool

	// Secret semantics (docs/design README.md §21a) — isSecret gates all of
	// it off for every other kind. revealed is per-data-key, in-memory only:
	// it lives on this Model instance and is never read back on a fresh
	// New() (yamlview is re-pushed fresh each time), so leaving the view
	// re-masks everything by construction, not by an explicit reset.
	isSecret         bool
	secretType       string
	secretData       []secretDataLine
	revealed         map[string]bool
	revealAllConfirm bool

	// cursor/offset index into the rendered (post-fold) line list, per
	// fold.go's renderLines.
	cursor int
	offset int

	searchActive bool
	searchInput  textfield.Model

	// conn is the last kube.ConnStateMsg forwarded by the root shell — the
	// header badge's real connection state (never a hardcoded "live").
	conn kube.ConnState

	// loadEpoch is bumped on every reload, so only the newest load()'s
	// result is applied.
	loadEpoch int

	state    tui.TaskState
	feedback string
	spinner  spinner.Model
}

// loadedMsg carries one load()'s result. epoch is the loadEpoch it was
// issued under — an older load landing after a newer one is dropped.
// keepManagedFields means the object's resourceVersion hadn't moved since
// the last load, so no live managedFields fetch was made and the ones on
// screen still hold.
type loadedMsg struct {
	epoch             int
	text              string
	resourceVersion   string
	managedFieldsYAML string
	keepManagedFields bool
	err               error
}

func New(cfg Config) Model {
	if cfg.LoadTimeout == 0 {
		cfg.LoadTimeout = 10 * time.Second
	}
	state := tui.TaskStateLoading
	feedback := "Loading " + cfg.Name + "..."
	if cfg.Lister == nil || cfg.YAML == nil {
		state = tui.TaskStateError
		feedback = "no cluster connection"
	}
	return Model{
		width:         tui.DefaultWidth,
		height:        tui.DefaultHeight,
		session:       cfg.Session,
		lister:        cfg.Lister,
		yaml:          cfg.YAML,
		managedFields: cfg.ManagedFields,
		timeout:       cfg.LoadTimeout,
		kind:          cfg.Kind,
		namespace:     cfg.Namespace,
		name:          cfg.Name,
		state:         state,
		feedback:      feedback,
		spinner:       components.NewSpinner(),
	}
}

func (m Model) Init() tea.Cmd {
	if m.lister == nil || m.yaml == nil {
		return nil
	}
	return tea.Batch(m.load(), m.spinner.Tick)
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width, m.height = size.Width, size.Height
}

// rendered is the current displayed line list — the single call site every
// cursor/render/fold operation goes through: fold state first, then
// managedFields' spliced-in content (managedfields.go), then Secret
// masking/reveal (secret.go) last.
func (m Model) rendered() []renderLine {
	lines := renderLines(m.lines, m.folded)
	lines = applyManagedFields(lines, m.managedFieldsLines, m.folded)
	if m.isSecret {
		lines = applySecretReveal(lines, m.secretData, m.revealed)
	}
	return lines
}
