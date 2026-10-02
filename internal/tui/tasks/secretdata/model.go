// Package secretdata is 27b (docs/design README.md §27b, sourced from
// docs/design/v.0.2.0.dc.html's 27b mockup): a Secret's own Data view — a
// KEY/VALUE/SIZE grid where every existing value stays masked in
// navigation. 'a' opens a line-insert add row; '↵' on an existing row
// decodes its real value and opens it for editing (a blind rewrite is not
// what was chosen here — see editKeyState's own doc comment). Reached from
// tasks/browse's Secrets list on 'enter', the same "full pushed screen, its
// own breadcrumb segment" shape poddetail/nodedetail/helmhistory already
// use — not a bespoke panel embedded in browse's own table, since the
// mockup's breadcrumb explicitly adds a "› Data" segment past the object
// itself.
//
// Like meta.go (26a), a commit never closes this screen: adding a key,
// editing an existing one, or removing one all go through
// actions.Controller/kube.Mutator (non-PROD: add/edit apply immediately;
// PROD: inline y/N first; removal: always inline y/N, regardless of PROD),
// and the outcome re-fetches the Secret fresh rather than trusting the
// locally-typed value — "confirm → execute → refresh → show result →
// remain on screen," the same contract 26a established, with the one
// difference the design doc calls out: the result message names the
// touched key alone, never its value, success or not.
package secretdata

import (
	"cmp"
	"context"
	"slices"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"
	corev1 "k8s.io/api/core/v1"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
)

// secretInputStyles builds the add/edit buffers' textfield.Styles: bold text
// on a SelBg background, matching this screen's own fill := ...SelBg used
// throughout view.go for both the add row and the decode-then-edit row.
func secretInputStyles(theme tui.Theme) textfield.Styles {
	styles := tui.TextInputStyles(theme)
	styles.Focused.Text = styles.Focused.Text.Bold(true).Background(theme.SelBg)
	styles.Blurred.Text = styles.Blurred.Text.Bold(true).Background(theme.SelBg)
	return styles
}

// newSecretInput builds one of this screen's add/edit buffers — styled,
// prompt-less (this screen renders its own "+ "/key= prefixes), unfocused
// until the caller calls Focus() on whichever buffer has it.
func newSecretInput(theme tui.Theme) textfield.Model {
	ti := textfield.New()
	ti.Prompt = ""
	ti.SetStyles(secretInputStyles(theme))
	return ti
}

// Config are secretdata's dependencies, per repo convention (package-local
// Config struct, interface-typed fields, New fills zero values).
type Config struct {
	Session     *tui.Session
	Lister      resources.RawLister
	Mutator     kube.Mutator
	Namespace   string
	Name        string
	LoadTimeout time.Duration
}

// secretKeyRow is one existing Data-view row. value holds the real decoded
// plaintext (client-go already decodes .Data's base64 into these bytes when
// unmarshaling the API response, so this stores no more than what's already
// resident in process memory) — kept off-screen (never rendered) in
// navigation, and only surfaced when '↵' opens the row for editing.
type secretKeyRow struct {
	key   string
	value string
	size  int
}

// addKeyState is 27b's line-insert add row — nil on Model when not
// showing. masked toggles the value cell between plaintext-while-typing
// (the default, mockup's own "visible while typing · ctrl-x re-mask") and a
// fixed mask glyph once ctrl-x hides it.
type addKeyState struct {
	keyInput   textfield.Model
	valueInput textfield.Model
	onValue    bool
	masked     bool
}

// editKeyState is '↵' on an existing row: a decode-then-edit flow, not a
// blind rewrite — the value buffer starts pre-filled with the key's real
// decoded value (original, kept alongside for the "unchanged → no-op"
// check) rather than empty, and otherwise behaves like the add row's own
// value entry (visible while typing by default, ctrl-x masks it) since
// editing has to actually show what's being changed. The key itself isn't
// editable here — only 27b's own add flow types a key.
type editKeyState struct {
	key        string
	original   string
	valueInput textfield.Model
	masked     bool
}

func (e editKeyState) changed() bool { return e.valueInput.Value() != e.original }

// secretPendingCommit remembers what an in-flight add/edit/remove is trying
// to write, the same shape meta.go's metaPendingCommit uses — cleared once
// handleResult applies the outcome. isEdit distinguishes an existing key's
// rewrite from a brand-new add purely for the inline result wording
// ("updated" vs "added") and failure-restore target (editing vs adding) —
// it has no effect on tiering or the patch itself, both go through the same
// PatchSecretData call.
type secretPendingCommit struct {
	key    string
	value  string // "" for a removal
	remove bool
	isEdit bool
	// original is the pre-edit decoded value (isEdit only) — carried
	// through so a failed edit's restore (handleResult) can rebuild
	// editKeyState with the real original, not the just-attempted value,
	// keeping its own changed() check meaningful on a retry.
	original string
	// overwrite marks an add whose key already exists — escalated to a
	// confirm, and reported as "updated" rather than "added".
	overwrite bool
}

type Model struct {
	width, height int

	session *tui.Session
	lister  resources.RawLister
	mutator kube.Mutator
	actions actions.Controller
	timeout time.Duration

	namespace string
	name      string

	secretType string
	keys       []secretKeyRow
	selected   int

	// adding is non-nil while 'a'/insert's line-insert add row is showing.
	adding *addKeyState
	// editing is non-nil while '↵'s decode-then-edit row is showing —
	// mutually exclusive with adding (opening one closes the other, though
	// in practice each key handler only ever reaches one at a time).
	editing *editKeyState

	// pendingCommit is set the instant a commit starts (add's TierNone
	// synchronous apply, or either verb's TierInline confirm) and cleared
	// once handleResult applies its outcome.
	pendingCommit *secretPendingCommit
	// focusKey carries the just-committed key across the async refresh
	// load() triggers, so applyLoaded can refocus it (an add) or — once it
	// no longer exists (a removal) — clamp to the nearest remaining row.
	focusKey string
	// message/lastError are the screen's own transient inline result line
	// — "added SMTP_PASSWORD" / "removed SMTP_PASSWORD" (the key alone,
	// never "=value", success or not — docs/design README.md §27b's own
	// no-leak rule) on success, the raw server error on failure. Cleared
	// the next time a key is pressed in navigation mode.
	message   string
	lastError string
	// notice explains why ↵ refused to open a value that can't round-trip
	// through the single-line buffer (multi-line or binary) — the key
	// alone, never the value. Cleared on the next navigation key.
	notice string

	conn        kube.ConnState
	reloadEpoch int

	state    tui.TaskState
	feedback string
	spinner  spinner.Model
}

// loadedMsg carries one load()'s result.
type loadedMsg struct {
	epoch      int
	found      bool
	secretType string
	keys       []secretKeyRow
	err        error
}

func New(cfg Config) Model {
	if cfg.LoadTimeout == 0 {
		cfg.LoadTimeout = 10 * time.Second
	}
	state := tui.TaskStateLoading
	feedback := "Loading " + cfg.Name + "..."
	if cfg.Lister == nil {
		state = tui.TaskStateError
		feedback = "no cluster connection"
	}
	return Model{
		width:     tui.DefaultWidth,
		height:    tui.DefaultHeight,
		session:   cfg.Session,
		lister:    cfg.Lister,
		mutator:   cfg.Mutator,
		actions:   actions.New(cfg.Mutator),
		timeout:   cfg.LoadTimeout,
		namespace: cfg.Namespace,
		name:      cfg.Name,
		state:     state,
		feedback:  feedback,
		spinner:   components.NewSpinner(),
	}
}

func (m Model) Init() tea.Cmd {
	if m.lister == nil {
		return nil
	}
	return tea.Batch(m.load(), m.spinner.Tick)
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width, m.height = size.Width, size.Height
}

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
		objs, err := lister.ListRaw(ctx, kube.KindSecret, namespace)
		if err != nil {
			return loadedMsg{epoch: epoch, err: err}
		}
		for _, obj := range objs {
			s, ok := obj.(*corev1.Secret)
			if !ok || s.Name != name {
				continue
			}
			secretType := string(s.Type)
			if secretType == "" {
				secretType = "Opaque"
			}
			return loadedMsg{epoch: epoch, found: true, secretType: secretType, keys: secretRowsFrom(s.Data)}
		}
		return loadedMsg{epoch: epoch, found: false}
	}
}

// secretRowsFrom sorts a Secret's decoded .Data by key (for stable,
// deterministic display — a map carries no meaningful iteration order of
// its own), one row per key with its decoded value and byte length.
func secretRowsFrom(data map[string][]byte) []secretKeyRow {
	rows := make([]secretKeyRow, 0, len(data))
	for k, v := range data {
		rows = append(rows, secretKeyRow{key: k, value: string(v), size: len(v)})
	}
	slices.SortFunc(rows, func(a, b secretKeyRow) int { return cmp.Compare(a.key, b.key) })
	return rows
}

// indexOfSecretKey finds key's row index in rows, if present.
func indexOfSecretKey(rows []secretKeyRow, key string) (int, bool) {
	for i, r := range rows {
		if r.key == key {
			return i, true
		}
	}
	return 0, false
}

func (m Model) selectedKeyRow() (secretKeyRow, bool) {
	if m.selected < 0 || m.selected >= len(m.keys) {
		return secretKeyRow{}, false
	}
	return m.keys[m.selected], true
}

// CapturingInput reports whether the add/edit row's buffers want every
// keystroke — the root shell's own InputCapturer check (tui.taskCapturingInput),
// so 'g'/'n'/'c'/'?' don't get hijacked as shell shortcuts while typing a
// key/value, the same reasoning meta.go's browse-embedded panel doesn't need
// (browse itself isn't gated this way, but a full pushed screen is).
func (m Model) CapturingInput() bool {
	return m.adding != nil || m.editing != nil
}

// isProd mirrors browse's own isProd (delete.go) — the same PROD-tag source
// (Session.Config.IsProd), duplicated per the repo's package-local-seam
// convention (poddetail/nodedetail/helmhistory each keep their own copy).
func (m Model) isProd() bool {
	if m.session == nil {
		return false
	}
	return m.session.Config.IsProd(m.session.Location.Context)
}

// ScreenObject implements tui.ObjectScreen: a namespace switch from here
// follows this object into the new namespace.
func (m *Model) ScreenObject() (kube.ResourceKind, string, string) {
	return kube.KindSecret, m.namespace, m.name
}

var _ tui.ObjectScreen = (*Model)(nil)
