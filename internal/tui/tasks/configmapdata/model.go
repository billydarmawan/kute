// Package configmapdata is 27a (docs/design README.md §27a, sourced from
// docs/design/v.0.2.0.dc.html's 27a mockup): a ConfigMap's own Data view — a
// KEY/VALUE/SIZE grid, reached from tasks/browse's ConfigMaps list on
// 'enter', the same "full pushed screen, its own breadcrumb segment" shape
// tasks/secretdata (27b) already established for Secret. Unlike Secret,
// ConfigMap values aren't sensitive, so there's no masking anywhere on this
// screen — every row shows its real value straight off, and there's no
// ctrl-x mask toggle on the add/edit buffers either.
//
// Two things this screen has that 27b doesn't:
//   - A consumer strip under the header (which Deployment/StatefulSet/
//     DaemonSet pod templates reference this ConfigMap, and how — env or
//     volume), computed fresh off the watch on every load().
//   - ctrl-r: apply the edited value *and* chain a rollout-restart of every
//     consumer (kute never restarts consumers on a plain ↵ apply — 27a's own
//     "pods don't reload configmaps on their own").
//
// §27a's own spec leans on §17a (the not-yet-built YAML edit-mode screen)
// for two things this package deliberately does *not* build: a shared
// "buffer editor" component for multi-line values, and resourceVersion-based
// conflict detection on apply. 17a doesn't exist yet, so per the plan this
// package uses its own minimal multi-line textarea (multilineEditState
// below) instead of waiting on 17a, and skips optimistic-concurrency
// conflict handling entirely — the same gap every other implemented editor
// in this repo (26a meta.go, 27b secretdata, 25a setresources.go) already
// has, for the identical reason.
//
// Like meta.go (26a) and secretdata (27b), a commit never closes this
// screen: adding a key, editing one (single- or multi-line), or removing one
// all go through actions.Controller/kube.Mutator (non-PROD: add/edit apply
// immediately; PROD: inline y/N first; removal: always inline y/N,
// regardless of PROD), and the outcome re-fetches the ConfigMap (and its
// consumer list) fresh rather than trusting the locally-typed value —
// "confirm → execute → refresh → show result → remain on screen."
package configmapdata

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
)

// Config are configmapdata's dependencies, per repo convention (package-local
// Config struct, interface-typed fields, New fills zero values).
type Config struct {
	Session     *tui.Session
	Lister      resources.RawLister
	Mutator     kube.Mutator
	Namespace   string
	Name        string
	LoadTimeout time.Duration
}

// configMapKeyRow is one existing Data-view row — value is the real,
// plaintext value straight from .Data (ConfigMap carries no base64 decode
// step the way Secret's .Data []byte does), so unlike secretKeyRow there's
// nothing to keep off-screen: every row shows it directly.
type configMapKeyRow struct {
	key   string
	value string
	size  int
}

// multiline reports whether the row's value needs the buffer editor rather
// than the single-line in-place edit (docs/design README.md §27a: "Multi-
// line keys show a folded summary ... e opens the buffer editor").
func (r configMapKeyRow) multiline() bool { return strings.Contains(r.value, "\n") }

// configMapInputStyles builds the add/edit buffers' textfield.Styles: bold
// text on a SelBg background, matching this screen's own fill :=
// ...SelBg used throughout view.go for both the add row and the in-place
// edit row.
func configMapInputStyles(theme tui.Theme) textfield.Styles {
	styles := tui.TextInputStyles(theme)
	styles.Focused.Text = styles.Focused.Text.Bold(true).Background(theme.SelBg)
	styles.Blurred.Text = styles.Blurred.Text.Bold(true).Background(theme.SelBg)
	return styles
}

// addKeyState is 27a's line-insert add row — nil on Model when not showing.
// No masked field: unlike 27b's Secret add row, a ConfigMap value is never
// sensitive, so there's nothing to hide while typing.
type addKeyState struct {
	keyInput   textfield.Model
	valueInput textfield.Model
	onValue    bool
}

// editKeyState is '↵' on an existing single-line row: an in-place rewrite,
// value buffer pre-filled with the row's real (already-visible) value.
// original is kept alongside for the "unchanged → no-op" check.
type editKeyState struct {
	key        string
	original   string
	valueInput textfield.Model
}

func (e editKeyState) changed() bool { return e.valueInput.Value() != e.original }

// multilineEditState is the "simpler solution" 27a's own plan substitutes
// for 17a's shared buffer editor (which doesn't exist yet): a bubbles/v2
// textarea.Model scoped to one key's value, opened by 'e' (or '↵') on a row
// whose value contains a newline — its own soft-wrap/scrolling/line-number
// gutter replace what used to be this package's hand-rolled lines/row/col +
// scrollWindow. alt+a applies (moved off ctrl+s, which is the terminal's
// own XOFF flow-control key in some environments and can read as a frozen
// app), R applies and restarts every consumer, esc cancels back to
// navigation without applying.
type multilineEditState struct {
	key      string
	original string
	textarea textarea.Model
}

func newMultilineEditState(key, value string, theme tui.Theme) *multilineEditState {
	ta := textarea.New()
	ta.Prompt = ""
	ta.SetStyles(tui.TextAreaStyles(theme))
	ta.SetValue(value)
	ta.Focus()
	return &multilineEditState{key: key, original: value, textarea: ta}
}

func (m multilineEditState) value() string { return m.textarea.Value() }

// textareaRoundTrips reports whether value survives the buffer editor's
// textarea.SetValue unchanged. It doesn't for a tab (expanded to spaces),
// a CRLF line ending (split into two breaks) or binary content — and an
// edit to one line would then write the whole value back normalized.
func textareaRoundTrips(value string) bool {
	ta := textarea.New()
	ta.SetValue(value)
	return ta.Value() == value
}

func (m multilineEditState) changed() bool {
	return m.value() != m.original
}

// configMapConsumer is one workload referencing the ConfigMap from its pod
// template — the embedded kube.ConfigMapConsumerRef is exactly what
// TaskScope.ConfigMapConsumers/ctrl-r's restart loop needs; refKind ("env" or
// "volume") is display-only, for the header consumer strip and the keybar
// note.
type configMapConsumer struct {
	kube.ConfigMapConsumerRef
	refKind string
}

// configMapPendingCommit remembers what an in-flight add/edit/remove is
// trying to write, the same shape secretdata's secretPendingCommit uses —
// cleared once handleResult applies the outcome. restartConsumers/consumers
// carry a ctrl-r commit's target set through to the will-run strip and the
// success message ("updated KEY · restarted N consumers").
type configMapPendingCommit struct {
	key    string
	value  string // "" for a removal
	remove bool
	isEdit bool
	// original is the pre-edit value (isEdit only) — carried through so a
	// failed edit's restore (handleResult) can rebuild the right edit state
	// with the real original, not the just-attempted value.
	original string
	// overwrite marks an add whose key already exists — escalated to a
	// confirm, and reported as "updated" rather than "added".
	overwrite        bool
	restartConsumers bool
	consumers        []configMapConsumer
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

	keys      []configMapKeyRow
	selected  int
	consumers []configMapConsumer

	// adding is non-nil while 'a'/insert's line-insert add row is showing.
	adding *addKeyState
	// editing is non-nil while '↵'s single-line in-place edit is showing.
	editing *editKeyState
	// multiline is non-nil while the buffer editor ('e', or '↵' on a
	// multi-line row) is showing — mutually exclusive with adding/editing.
	multiline *multilineEditState

	// pendingCommit is set the instant a commit starts (add's TierNone
	// synchronous apply, or either verb's TierInline confirm) and cleared
	// once handleResult applies its outcome.
	pendingCommit *configMapPendingCommit
	// focusKey carries the just-committed key across the async refresh
	// load() triggers, so applyLoaded can refocus it (an add/edit) or —
	// once it no longer exists (a removal) — clamp to the nearest
	// remaining row.
	focusKey string
	// message/lastError are the screen's own transient inline result line —
	// cleared the next time a key is pressed in navigation mode.
	message   string
	lastError string
	// notice explains why ↵/e refused to open a value no buffer here can
	// hold unchanged (tabs, CRLF, binary). Cleared on the next navigation key.
	notice string

	conn        kube.ConnState
	reloadEpoch int

	state    tui.TaskState
	feedback string
	spinner  spinner.Model
}

// loadedMsg carries one load()'s result.
type loadedMsg struct {
	epoch     int
	found     bool
	keys      []configMapKeyRow
	consumers []configMapConsumer
	err       error
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
		objs, err := lister.ListRaw(ctx, kube.KindConfigMap, namespace)
		if err != nil {
			return loadedMsg{epoch: epoch, err: err}
		}
		var found bool
		var keys []configMapKeyRow
		for _, obj := range objs {
			cm, ok := obj.(*corev1.ConfigMap)
			if !ok || cm.Name != name {
				continue
			}
			found = true
			keys = configMapRowsFrom(cm.Data)
			break
		}
		if !found {
			return loadedMsg{epoch: epoch, found: false}
		}
		consumers := findConfigMapConsumers(ctx, lister, namespace, name)
		return loadedMsg{epoch: epoch, found: true, keys: keys, consumers: consumers}
	}
}

// configMapRowsFrom sorts a ConfigMap's .Data by key (for stable,
// deterministic display — a map carries no meaningful iteration order of its
// own), one row per key with its value and byte length.
func configMapRowsFrom(data map[string]string) []configMapKeyRow {
	rows := make([]configMapKeyRow, 0, len(data))
	for k, v := range data {
		rows = append(rows, configMapKeyRow{key: k, value: v, size: len(v)})
	}
	slices.SortFunc(rows, func(a, b configMapKeyRow) int { return cmp.Compare(a.key, b.key) })
	return rows
}

// indexOfConfigMapKey finds key's row index in rows, if present.
func indexOfConfigMapKey(rows []configMapKeyRow, key string) (int, bool) {
	for i, r := range rows {
		if r.key == key {
			return i, true
		}
	}
	return 0, false
}

func (m Model) selectedKeyRow() (configMapKeyRow, bool) {
	if m.selected < 0 || m.selected >= len(m.keys) {
		return configMapKeyRow{}, false
	}
	return m.keys[m.selected], true
}

// configMapConsumerKinds is the fixed set of pod-template kinds 27a's
// consumer strip/ctrl-r restart scan over — the same three "workload" kinds
// 24a/25a already treat uniformly elsewhere in this app (Deployment/
// StatefulSet/DaemonSet all carry a corev1.PodSpec-shaped template; bare
// Pods/Jobs/CronJobs are out of scope, matching that existing precedent).
var configMapConsumerKinds = []kube.ResourceKind{kube.KindDeployment, kube.KindStatefulSet, kube.KindDaemonSet}

// findConfigMapConsumers scans every Deployment/StatefulSet/DaemonSet in
// namespace for a pod-template reference to the named ConfigMap — envFrom or
// env[].valueFrom.configMapKeyRef ("env"), or a configMap volume ("volume").
// A workload matching both is reported as "env" (display priority, since an
// env reference is the more common/impactful reload case). Sorted by kind
// then name for deterministic rendering/tests.
func findConfigMapConsumers(ctx context.Context, lister resources.RawLister, namespace, name string) []configMapConsumer {
	if lister == nil {
		return nil
	}
	var out []configMapConsumer
	for _, kind := range configMapConsumerKinds {
		objs, err := lister.ListRaw(ctx, kind, namespace)
		if err != nil {
			continue
		}
		for _, obj := range objs {
			podSpec, objName, ok := podTemplateSpec(kind, obj)
			if !ok {
				continue
			}
			refKind, matched := configMapRefKind(podSpec, name)
			if !matched {
				continue
			}
			out = append(out, configMapConsumer{
				ConfigMapConsumerRef: kube.ConfigMapConsumerRef{Kind: kind, Name: objName},
				refKind:              refKind,
			})
		}
	}
	slices.SortFunc(out, func(a, b configMapConsumer) int {
		return cmp.Or(
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Name, b.Name),
		)
	})
	return out
}

// podTemplateSpec extracts obj's pod template PodSpec + name for one of the
// three configMapConsumerKinds — ok is false for any other kind/type.
func podTemplateSpec(kind kube.ResourceKind, obj any) (corev1.PodSpec, string, bool) {
	switch kind {
	case kube.KindDeployment:
		if d, ok := obj.(*appsv1.Deployment); ok {
			return d.Spec.Template.Spec, d.Name, true
		}
	case kube.KindStatefulSet:
		if s, ok := obj.(*appsv1.StatefulSet); ok {
			return s.Spec.Template.Spec, s.Name, true
		}
	case kube.KindDaemonSet:
		if d, ok := obj.(*appsv1.DaemonSet); ok {
			return d.Spec.Template.Spec, d.Name, true
		}
	}
	return corev1.PodSpec{}, "", false
}

// configMapRefKind reports whether spec references the named ConfigMap, and
// how: "env" (envFrom.configMapRef, or an env var's
// valueFrom.configMapKeyRef, checked on both containers and initContainers)
// wins over "volume" (a configMap volume source) when both are present.
func configMapRefKind(spec corev1.PodSpec, name string) (string, bool) {
	hasEnv := false
	for _, containers := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
		for _, c := range containers {
			for _, ef := range c.EnvFrom {
				if ef.ConfigMapRef != nil && ef.ConfigMapRef.Name == name {
					hasEnv = true
				}
			}
			for _, e := range c.Env {
				if e.ValueFrom != nil && e.ValueFrom.ConfigMapKeyRef != nil && e.ValueFrom.ConfigMapKeyRef.Name == name {
					hasEnv = true
				}
			}
		}
	}
	if hasEnv {
		return "env", true
	}
	for _, v := range spec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name == name {
			return "volume", true
		}
	}
	return "", false
}

// CapturingInput reports whether the add/edit/buffer-editor buffers want
// every keystroke — the root shell's own InputCapturer check, mirroring
// secretdata's own doc comment.
func (m Model) CapturingInput() bool {
	return m.adding != nil || m.editing != nil || m.multiline != nil
}

// isProd mirrors secretdata's own isProd (Session.Config.IsProd), duplicated
// per the repo's package-local-seam convention.
func (m Model) isProd() bool {
	if m.session == nil {
		return false
	}
	return m.session.Config.IsProd(m.session.Location.Context)
}

// ScreenObject implements tui.ObjectScreen: a namespace switch from here
// follows this object into the new namespace.
func (m *Model) ScreenObject() (kube.ResourceKind, string, string) {
	return kube.KindConfigMap, m.namespace, m.name
}

var _ tui.ObjectScreen = (*Model)(nil)
