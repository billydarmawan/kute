// Package helmdetail renders the saved Helm transaction beside live
// Kubernetes evidence. It deliberately reads only kinds declared by this
// release, preserving the app's lazy-informer contract.
package helmdetail

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
)

type EventsReader interface {
	NamespaceEvents(context.Context, string) ([]kube.Event, error)
}

// HelmSecretLister is the optional narrow seam onto the release-Secret cache
// (helmhistory's own HelmSecretLister, duplicated per the package-local-seam
// convention). Exported so app can assert its lister decorators against it.
type HelmSecretLister interface {
	ListHelmReleaseSecrets(ctx context.Context, namespace string) ([]runtime.Object, error)
}

// reloadDebounce coalesces bursts of watch events — Events above all, which
// this screen always listens to — into one reload, the same value as
// browse's own reloadDebounce, duplicated per the package-local-seam
// convention.
const reloadDebounce = 250 * time.Millisecond

// reloadDueMsg fires scheduleReload's debounced reload; epoch drops one
// superseded by a later event.
type reloadDueMsg struct{ epoch int }

// scheduleReload arranges one reloadDueMsg reloadDebounce from now.
func scheduleReload(epoch int) tea.Cmd {
	return tea.Tick(reloadDebounce, func(time.Time) tea.Msg {
		return reloadDueMsg{epoch: epoch}
	})
}

type OpenEventsFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)
type OpenSavedYAMLFunc func(ref kube.HelmObjectRef, width, height int) (tea.Model, tea.Cmd)

type Config struct {
	Session       *tui.Session
	Lister        resources.RawLister
	Events        EventsReader
	Release       kube.HelmRelease
	OpenEvents    OpenEventsFunc
	OpenSavedYAML OpenSavedYAMLFunc
	LoadTimeout   time.Duration
}

type rowKind int

const (
	rowHook rowKind = iota
	rowObject
)

type diagnosticRow struct {
	typeLabel string
	ref       kube.HelmObjectRef
	kind      kube.ResourceKind
	helmState string
	liveState string
	note      string
	live      bool
	kindType  rowKind
}

type Model struct {
	width, height int
	session       *tui.Session
	lister        resources.RawLister
	events        EventsReader
	openEvents    OpenEventsFunc
	openYAML      OpenSavedYAMLFunc
	timeout       time.Duration
	release       kube.HelmRelease
	rows          []diagnosticRow
	selected      int
	relevant      map[kube.ResourceKind]bool
	diagnosis     string
	state         tui.TaskState
	feedback      string
	conn          kube.ConnState
	spinner       spinner.Model
	reloadEpoch   int
	now           time.Time
}

type loadedMsg struct {
	epoch     int
	release   kube.HelmRelease
	rows      []diagnosticRow
	relevant  map[kube.ResourceKind]bool
	diagnosis string
	pending   bool
	err       error
	now       time.Time
}

func New(cfg Config) Model {
	if cfg.LoadTimeout == 0 {
		cfg.LoadTimeout = 10 * time.Second
	}
	state := tui.TaskStateLoading
	feedback := "Loading Helm release evidence..."
	if cfg.Lister == nil {
		state, feedback = tui.TaskStateError, "no cluster connection"
	}
	return Model{
		width: tui.DefaultWidth, height: tui.DefaultHeight, session: cfg.Session,
		lister: cfg.Lister, events: cfg.Events, openEvents: cfg.OpenEvents,
		openYAML: cfg.OpenSavedYAML, timeout: cfg.LoadTimeout, release: cfg.Release,
		relevant: map[kube.ResourceKind]bool{kube.KindHelmRelease: true},
		state:    state, feedback: feedback, spinner: components.NewSpinner(), now: time.Now(),
	}
}

func (m Model) Init() tea.Cmd {
	if m.lister == nil {
		return nil
	}
	return tea.Batch(m.load(), m.spinner.Tick)
}

// Reload implements tui.Reloader so returning from Events or saved YAML
// catches evidence changes that arrived while this task was on the stack.
func (m *Model) Reload() tea.Cmd {
	m.reloadEpoch++
	return m.load()
}

func (m *Model) SetSize(width, height int) {
	s := tui.NormalizeSize(width, height)
	m.width, m.height = s.Width, s.Height
}

func (m Model) Theme() tui.Theme {
	if m.session != nil {
		return m.session.Theme
	}
	return tui.Dark()
}

func (m Model) load() tea.Cmd {
	epoch, lister, events, fallback, timeout := m.reloadEpoch, m.lister, m.events, m.release, m.timeout
	parent := m.session.ClusterContext()
	registry := m.session.Registry
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		release := currentRelease(ctx, lister, fallback)

		type scope struct {
			kind      kube.ResourceKind
			namespace string
		}
		refs := kube.HelmReleaseObjects(release)
		hookRefs := make([]kube.HelmObjectRef, len(release.Hooks))
		scopes := map[scope]bool{}
		relevant := map[kube.ResourceKind]bool{kube.KindHelmRelease: true, kube.KindEvent: true}
		for i, hook := range release.Hooks {
			if ref, ok := hook.ObjectRef(release.Namespace); ok {
				ref = normalizeRef(registry, ref)
				hookRefs[i] = ref
				if kind := registryKind(registry, ref); kind != "" {
					scopes[scope{kind, ref.Namespace}], relevant[kind] = true, true
				}
			}
		}
		for i, ref := range refs {
			ref = normalizeRef(registry, ref)
			refs[i] = ref
			if kind := registryKind(registry, ref); kind != "" {
				scopes[scope{kind, ref.Namespace}], relevant[kind] = true, true
			}
		}

		objects := map[scope][]runtime.Object{}
		errs := map[scope]error{}
		pending := false
		for s := range scopes {
			objects[s], errs[s] = lister.ListRaw(ctx, s.kind, s.namespace)
			if !tui.KindsSynced(lister, s.namespace, s.kind) {
				pending = true
			}
			if errs[s] == nil {
				errs[s] = tui.KindsError(lister, s.namespace, s.kind)
			}
		}
		var eventList []kube.Event
		var eventErr error
		if events != nil {
			eventList, eventErr = events.NamespaceEvents(ctx, release.Namespace)
			if !tui.KindsSynced(lister, release.Namespace, kube.KindEvent) {
				pending = true
			}
			if eventErr == nil {
				eventErr = tui.KindsError(lister, release.Namespace, kube.KindEvent)
			}
		}

		rows := make([]diagnosticRow, 0, len(release.Hooks)+len(refs))
		diagnosis := ""
		for i, hook := range release.Hooks {
			ref := hookRefs[i]
			kind := registryKind(registry, ref)
			s := scope{kind, ref.Namespace}
			live := kind != "" && containsObject(objects[s], ref.Name)
			liveState := evidenceFor(ref, live, errs[s], eventList, eventErr)
			event := strings.Join(hook.Events, ", ")
			if event == "" {
				event = "hook"
			}
			phase := hookState(hook)
			note := ""
			if len(hook.DeletePolicies) > 0 {
				note = "cleanup: " + strings.Join(hook.DeletePolicies, ", ")
			}
			rows = append(rows, diagnosticRow{typeLabel: event, ref: ref, kind: kind, helmState: phase, liveState: liveState, note: note, live: live, kindType: rowHook})
			if diagnosis == "" && strings.HasPrefix(release.Status, "pending-") && strings.EqualFold(hook.LastRun.Phase, "running") && !live && completedEvent(ref, eventList, hook.LastRun.StartedAt) {
				diagnosis = fmt.Sprintf("Helm still records %s/%s as Running, but Kubernetes reports it Completed and the object is gone · the Helm transaction was not finalized", ref.Kind, ref.Name)
			}
		}
		for _, ref := range refs {
			kind := registryKind(registry, ref)
			s := scope{kind, ref.Namespace}
			live := kind != "" && containsObject(objects[s], ref.Name)
			state := "not created"
			if live {
				state = "present"
			} else if kind == "" {
				state = "kind unavailable"
			} else if errs[s] != nil {
				state = "unknown · " + shortError(errs[s])
			}
			rows = append(rows, diagnosticRow{typeLabel: "object", ref: ref, kind: kind, helmState: "declared", liveState: state, live: live, kindType: rowObject})
		}
		return loadedMsg{epoch: epoch, release: release, rows: rows, relevant: relevant, diagnosis: diagnosis, pending: pending, now: time.Now()}
	}
}

// currentRelease re-reads this one release's newest revision. Through the
// release-Secret seam it narrows to this release's Secrets before decoding
// only the newest of them (helmhistory's recipe), rather than the decorated
// ListRaw(KindHelmRelease), which decodes every release in the namespace and
// reads the workload caches for a rollout flag this screen never shows.
// fallback stands whenever the read fails or no longer finds the release.
func currentRelease(ctx context.Context, lister resources.RawLister, fallback kube.HelmRelease) kube.HelmRelease {
	if hs, ok := lister.(HelmSecretLister); ok {
		secrets, err := hs.ListHelmReleaseSecrets(ctx, fallback.Namespace)
		if err != nil {
			return fallback
		}
		mine := kube.HelmReleaseSecretsFor(secrets, fallback.Namespace, fallback.Name)
		for _, r := range kube.DecodeLatestHelmReleases(mine) {
			if r.Name == fallback.Name && r.Namespace == fallback.Namespace {
				return r
			}
		}
		return fallback
	}
	objs, err := lister.ListRaw(ctx, kube.KindHelmRelease, fallback.Namespace)
	if err != nil {
		return fallback
	}
	for _, obj := range objs {
		if hr, ok := obj.(*kube.HelmReleaseObject); ok && hr.Release.Name == fallback.Name && hr.Release.Namespace == fallback.Namespace {
			return hr.Release
		}
	}
	return fallback
}

func registryKind(reg resources.Registry, ref kube.HelmObjectRef) kube.ResourceKind {
	group := ref.APIVersion
	if i := strings.IndexByte(group, '/'); i >= 0 {
		group = group[:i]
	} else {
		group = ""
	}
	// Prefer an exact discovered API-group match. This is load-bearing for
	// Flux HelmRelease, whose API Kind collides with the synthetic Helm-3
	// release registry entry.
	if group != "" {
		for _, kind := range reg.Kinds() {
			d, _ := reg.Descriptor(kind)
			if d.Custom && d.APIGroup == group && kind.APIKind() == ref.Kind {
				return kind
			}
		}
	}
	for _, kind := range reg.Kinds() {
		d, _ := reg.Descriptor(kind)
		if kind.APIKind() == ref.Kind && !d.Custom && kind != kube.KindHelmRelease {
			return kind
		}
	}
	return ""
}

func normalizeRef(reg resources.Registry, ref kube.HelmObjectRef) kube.HelmObjectRef {
	if kind := registryKind(reg, ref); kind != "" {
		if d, ok := reg.Descriptor(kind); ok && d.ClusterScoped {
			ref.Namespace = ""
		}
	}
	return ref
}

func containsObject(objs []runtime.Object, name string) bool {
	for _, obj := range objs {
		if a, err := metav1.Accessor(obj); err == nil && a.GetName() == name {
			return true
		}
	}
	return false
}

func evidenceFor(ref kube.HelmObjectRef, live bool, listErr error, events []kube.Event, eventErr error) string {
	if live {
		return "present"
	}
	if listErr != nil {
		return "unknown · " + shortError(listErr)
	}
	var latest *kube.Event
	for i := range events {
		if events[i].Object == ref.Kind+"/"+ref.Name && (latest == nil || events[i].LastSeen.After(latest.LastSeen)) {
			latest = &events[i]
		}
	}
	if latest != nil {
		if latest.Reason == "Completed" {
			return "Completed event · object no longer present"
		}
		return latest.Reason + " · " + latest.Message
	}
	if eventErr != nil {
		return "not present · events unavailable"
	}
	return "not present"
}

func completedEvent(ref kube.HelmObjectRef, events []kube.Event, started time.Time) bool {
	for _, event := range events {
		if event.Object == ref.Kind+"/"+ref.Name && event.Reason == "Completed" && (started.IsZero() || !event.LastSeen.Before(started)) {
			return true
		}
	}
	return false
}

func hookState(hook kube.HelmHook) string {
	phase := hook.LastRun.Phase
	if phase == "" {
		return "not run"
	}
	if !hook.LastRun.CompletedAt.IsZero() {
		return phase + " · " + hook.LastRun.CompletedAt.Format("15:04")
	}
	if !hook.LastRun.StartedAt.IsZero() {
		return phase + " · since " + hook.LastRun.StartedAt.Format("15:04")
	}
	return phase
}

func shortError(err error) string {
	if apierrors.IsForbidden(err) {
		return "Forbidden"
	}
	return err.Error()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case kube.ConnStateMsg:
		m.conn = kube.ConnState(msg)
	case kube.ResourceChangedMsg:
		if m.relevant[msg.Kind] {
			// Debounced: Events are always relevant and arrive in bursts, and
			// each load re-reads the release plus every declared kind.
			m.reloadEpoch++
			return m, scheduleReload(m.reloadEpoch)
		}
	case reloadDueMsg:
		if msg.epoch == m.reloadEpoch {
			return m, m.load()
		}
	case tui.CacheSyncRetryMsg:
		if msg.Gen == m.reloadEpoch {
			return m, m.load()
		}
	case loadedMsg:
		if msg.epoch != m.reloadEpoch {
			return m, nil
		}
		if msg.err != nil {
			m.state, m.feedback = tui.TaskStateError, msg.err.Error()
			return m, nil
		}
		m.release, m.rows, m.relevant, m.diagnosis, m.now = msg.release, msg.rows, msg.relevant, msg.diagnosis, msg.now
		if msg.pending {
			m.state = tui.TaskStateLoading
			return m, tui.ScheduleCacheSyncRetry(m.reloadEpoch)
		}
		m.state, m.feedback = tui.TaskStateReady, ""
		if m.selected >= len(m.rows) {
			m.selected = max(len(m.rows)-1, 0)
		}
	case spinner.TickMsg:
		if m.state != tui.TaskStateLoading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+q", "ctrl+c":
			return m, tea.Quit
		case "esc", "backspace":
			return m, func() tea.Msg { return tui.BackMsg{} }
		case "up", "k":
			m.selected = max(m.selected-1, 0)
		case "down", "j":
			m.selected = min(m.selected+1, max(len(m.rows)-1, 0))
		case "enter":
			if row, ok := m.selectedRow(); ok && row.live && row.kind != "" {
				return m, tui.GotoResource(m.session, row.kind, row.ref.Namespace, row.ref.Name)
			}
		case "e":
			if row, ok := m.selectedRow(); ok && m.openEvents != nil && row.kind != "" {
				task, cmd := m.openEvents(row.kind, row.ref.Namespace, row.ref.Name, m.width, m.height)
				if task != nil {
					return task, cmd
				}
			}
		case "y":
			if row, ok := m.selectedRow(); ok && m.openYAML != nil {
				task, cmd := m.openYAML(row.ref, m.width, m.height)
				if task != nil {
					return task, cmd
				}
			}
		}
	}
	return m, nil
}

func (m Model) selectedRow() (diagnosticRow, bool) {
	if m.selected < 0 || m.selected >= len(m.rows) {
		return diagnosticRow{}, false
	}
	return m.rows[m.selected], true
}
