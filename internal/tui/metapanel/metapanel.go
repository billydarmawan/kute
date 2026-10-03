// Package metapanel is 26a's 'm' inline labels/annotations editor (docs/
// design README.md §26a), extracted from browse so every object detail
// screen (5a pod, 11b node, 14d CRD, 36e CronJob) can host the same panel —
// task packages can't import one another, so the shared surface lives here,
// a sibling of actions/verbs rather than under components (it reads the
// cluster through resources.RawLister and drives actions.Controller; the
// components tree stays pure UI).
//
// Like browse's setresources.go/setimage.go, a bespoke gate (the host's
// panel pointer) rather than actions.Controller's y/N/type-name flow up
// front, since there's a per-row value buffer (or, while adding, a
// key+value pair) to gather before there's an action to Begin. Once
// ↵/delete commits, execution does go through actions.Controller/
// kube.Mutator — TierNone for an ordinary edit (metadata changes are
// reversible), escalated to TierInline only for a Service-selector-joined
// label edit (or one whose Service joins aren't known yet) or any key
// removal. Unlike 24a/25a this escalation is never
// PROD-driven and never escalates further to the type-the-name modal
// (docs/design README.md §26a: "reversible, no type-the-name modal — per
// 8b's tiering").
//
// Keys are context-sensitive rather than globally reserved: navigation mode
// (the default) never accepts typed text, so single-letter shortcuts there —
// including 'y' copy, 'a'/insert add — can never shadow a value a user might
// want to type. Only pressing ↵ on a row enters editing mode, and only there
// does every printable character (including 'a'/'A'/'y') insert literally.
// This mirrors the add sub-mode's own key/value buffers, which have always
// worked this way. Removal is 'D' (not 'delete'), matching browse's own
// row-delete key instead of a second, inconsistent one — safe as a bare
// letter because navigation mode never takes typed text.
//
// A TierInline confirm (a Service-selector-joined label edit, or any
// removal) renders *inside* the still-open panel rather than closing it
// first and falling back to the generic y/N-over-the-table convention 8b's
// delete confirm uses: the host deliberately leaves its panel pointer set
// across actions.Controller's Begin/Confirm/Cancel cycle (a host's updateKey
// already routes every keypress to its confirm handler once ctrl.Active(),
// ahead of the panel check, so this needs no extra input plumbing — only
// Body/Keybar need to keep showing the panel underneath; a cancel must call
// CancelConfirm so the row's buffer reverts). The panel only closes once the
// action actually resolves (the host's actions.ResultMsg case calling
// HandleResult), matching a plain TierNone apply's own close-on-success
// behavior; cancelling reverts the row's buffer and leaves the panel open in
// navigation mode, the same "esc backs out without closing" contract
// editing-mode's own esc already has.
package metapanel

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Config carries what the panel needs from its host screen. The host keeps
// ownership of its own actions.Controller — every method that can start or
// render a commit takes it as a parameter.
type Config struct {
	Session *tui.Session
	Lister  resources.RawLister
}

// Model is the open panel. A host holds a *Model (nil while closed) and
// routes keys/paste/results into it — see the package doc comment for the
// hosting contract.
type Model struct {
	cfg Config
	t   *metaTarget
}

// metaAddKind is which grid (if any) a/A's insert row is targeting.
type metaAddKind int

const (
	metaAddNone metaAddKind = iota
	metaAddLabel
	metaAddAnnotation
)

// metaRow is one LABELS/ANNOTATIONS grid row.
type metaRow struct {
	isAnnotation bool
	key          string
	current      string
	// input is the editable value, pre-filled to current, cursor-anchored
	// throughout — no replace-on-first-keystroke gate, the same continuous
	// text-field model browse's resourceField buffer uses. Its Placeholder
	// is "—" (metaInputStyles), so an emptied buffer shows the same dash
	// displayOrDash renders for a non-editing empty value.
	input textfield.Model

	// readOnly rows (a controller-managed annotation, or a workload's own
	// immutable spec.selector.matchLabels key) can be navigated to but never
	// edited or removed — kute says so up front via readOnlyNote rather than
	// letting the apply bounce off the API server (docs/design README.md
	// §26a).
	readOnly     bool
	readOnlyNote string
	// helmOwnedNote marks the one row announcing Helm ownership
	// (app.kubernetes.io/managed-by=Helm) — editable, just carries a note
	// that the next `helm upgrade` may revert it.
	helmOwnedNote bool
	// joinService/joinPodCount are set (labels only) when a Service selector
	// in this namespace currently matches this key/value pair — computed
	// once at Open time ("joins render before you touch anything"), never
	// recomputed against the in-progress (unapplied) buffer.
	joinService  string
	joinPodCount int
	// joinUnknown is set (labels only) when the Service cache those joins
	// were read from hadn't filled yet, or can't be read at all — no join
	// found is then not evidence of no join, so the edit is treated as if
	// it were joined (TierInline) rather than applied blind. Its value is
	// the reason, rendered in the row note and will-run strip.
	joinUnknown string
	// secretMasked marks a row whose current is a mask placeholder, not the
	// real value (maskSecretManifestRow) — 'y' copies nothing from it.
	secretMasked bool
}

// changed reports whether r differs from its prefilled current value.
func (r metaRow) changed() bool { return r.input.Value() != r.current }

// setBuffer replaces r.input's value wholesale and parks the cursor at its
// end — the same convention browse's resourceField.setBuffer/
// setImageTarget.setBuffer use.
func (r *metaRow) setBuffer(s string) {
	r.input.SetValue(s)
	r.input.CursorEnd()
}

// metaInputStyles builds the row-edit buffer's textfield.Styles: SelBg
// background baked in since metaValueCell (the only caller of r.input.View)
// only ever renders the selected+editing row, and a "—" placeholder so an
// emptied buffer shows the same dash displayOrDash renders elsewhere.
func metaInputStyles(theme tui.Theme) textfield.Styles {
	styles := tui.TextInputStyles(theme)
	styles.Focused.Text = styles.Focused.Text.Background(theme.SelBg)
	styles.Blurred.Text = styles.Blurred.Text.Background(theme.SelBg)
	styles.Focused.Placeholder = styles.Focused.Placeholder.Background(theme.SelBg)
	styles.Blurred.Placeholder = styles.Blurred.Placeholder.Background(theme.SelBg)
	return styles
}

// metaAddInputStyles builds the add-row's two buffers' textfield.Styles —
// unlike metaInputStyles, no SelBg background (metaAddRowLine never draws
// one) and bold text (metaAddBufferCell's old textStyle for both focused and
// unfocused non-empty content).
func metaAddInputStyles(theme tui.Theme) textfield.Styles {
	styles := tui.TextInputStyles(theme)
	styles.Focused.Text = styles.Focused.Text.Bold(true)
	styles.Blurred.Text = styles.Blurred.Text.Bold(true)
	return styles
}

// metaSection is which grid — LABELS or ANNOTATIONS — navigation, 'a'/insert,
// and tab/shift+tab currently target. Only two sections exist, so tab and
// shift+tab both just toggle it.
type metaSection int

const (
	metaSectionLabels metaSection = iota
	metaSectionAnnotations
)

// metaPendingCommit remembers what a TierNone or confirmed-TierInline commit
// is currently trying to write, so HandleResult can either build the
// right inline success message + know which row to refocus after a refresh,
// or — on failure — restore the exact pre-commit interaction state (still
// editing, or still in the add sub-flow) with the attempted value intact
// (docs/design README.md §26a: "confirm → execute → refresh → show result →
// remain on screen").
type metaPendingCommit struct {
	isAdd        bool
	isRemove     bool
	section      metaSection
	key          string // the existing row's key (edit/remove); "" for add
	isAnnotation bool
	value        string // the value being applied; "" for a removal
}

// metaTarget is the state the panel gates on while showing.
type metaTarget struct {
	kind      kube.ResourceKind
	namespace string
	name      string

	labels      []metaRow
	annotations []metaRow
	// section is the currently focused grid; labelIdx/annotationIdx are that
	// grid's own cursor, kept independently so switching focus and back
	// doesn't lose your place in either list.
	section       metaSection
	labelIdx      int
	annotationIdx int
	// editing is true while the selected row's value is a free-typing
	// buffer (entered via ↵) — see the package doc comment on why
	// navigation-mode shortcuts and editing-mode text input never collide.
	editing bool

	// adding is metaAddNone unless 'a'/insert's insert row is showing.
	adding        metaAddKind
	addKeyInput   textfield.Model
	addValueInput textfield.Model
	addOnValue    bool // tab moves focus from key to value

	// pendingCommit is set the instant a commit starts (TierNone's
	// synchronous apply, or a TierInline confirm) and cleared once
	// HandleResult applies its outcome — see that type's own doc comment.
	pendingCommit *metaPendingCommit
	// message/lastError are the panel's own transient inline result line —
	// "updated env=staging" / "removed kute.dev/owner" on success, the raw
	// server error on failure — cleared the next time a commit starts.
	message   string
	lastError string
}

// selectedRow is the focused section's row at its own cursor, if any — nil
// when that section is empty (including both grids empty at once).
func (t *metaTarget) selectedRow() *metaRow {
	switch t.section {
	case metaSectionAnnotations:
		if t.annotationIdx >= 0 && t.annotationIdx < len(t.annotations) {
			return &t.annotations[t.annotationIdx]
		}
	default:
		if t.labelIdx >= 0 && t.labelIdx < len(t.labels) {
			return &t.labels[t.labelIdx]
		}
	}
	return nil
}

// rowsFor returns section's row slice and a pointer to its own cursor field
// — shared by moveSelection and HandleResult's failure-restore path,
// which both need to address either grid generically.
func (t *metaTarget) rowsFor(section metaSection) ([]metaRow, *int) {
	if section == metaSectionAnnotations {
		return t.annotations, &t.annotationIdx
	}
	return t.labels, &t.labelIdx
}

// Editable reports whether kind takes 26a's editor — every real kind
// (Pod, Node, a CRD instance, …) except kute's own synthetic non-object rows,
// which have no metadata.labels/annotations to speak of.
func Editable(kind kube.ResourceKind) bool {
	switch kind {
	case kube.KindForward, kube.KindHelmRelease, kube.KindWhoCan, kube.KindOverview:
		return false
	default:
		return true
	}
}

// IsActionID reports whether id names a 26a set-meta/remove-meta action
// (commitMeta/commitMetaRemove's ID scheme) — hosts use it to route an
// actions.ResultMsg to HandleResult instead of their generic result path.
func IsActionID(id string) bool {
	return strings.HasPrefix(id, "set-meta-") || strings.HasPrefix(id, "remove-meta-")
}

// theme is the host session's theme (every host derives its own Theme() the
// same way).
func (p *Model) theme() tui.Theme {
	if p.cfg.Session != nil {
		return p.cfg.Session.Theme
	}
	return tui.Dark()
}

// Open fetches kind/namespace/name via the lister and builds the panel —
// ok is false when the object can't be read (the host shows nothing, the
// same ok-bool contract browse's beginSetImage/beginSetResources use).
func Open(cfg Config, kind kube.ResourceKind, namespace, name string) (*Model, bool) {
	p := &Model{cfg: cfg}
	t, ok := p.buildTarget(kind, namespace, name)
	if !ok {
		return nil, false
	}
	p.t = t
	return p, true
}

// buildTarget fetches kind/namespace/name fresh from the lister and builds a
// metaTarget from its real, current labels/annotations — the row-building
// half of Open, factored out so HandleResult's own post-commit refresh
// (docs/design README.md §26a: "re-fetch the object... rather than leaving
// the locally edited state on screen") always reflects the authoritative
// server state rather than an optimistic local patch, exactly like the
// panel's very first open.
func (p *Model) buildTarget(kind kube.ResourceKind, namespace, name string) (*metaTarget, bool) {
	obj, ok := findObject(p.cfg.Session.ClusterContext(), p.cfg.Lister, kind, namespace, name)
	if !ok {
		return nil, false
	}
	acc, err := apimeta.Accessor(obj)
	if err != nil {
		return nil, false
	}
	objLabels := acc.GetLabels()

	theme := p.theme()
	t := &metaTarget{kind: kind, namespace: namespace, name: name}
	t.labels = buildMetaRows(objLabels, false, theme)
	t.annotations = buildMetaRows(acc.GetAnnotations(), true, theme)

	joins := serviceLabelJoins(p.cfg.Session.ClusterContext(), p.cfg.Lister, namespace, objLabels)
	// Service is lazy: the read above may well have just started its
	// informer and come back empty. Asked after the read, about the same
	// namespace it used (CLAUDE.md, "never gate on Synced()").
	joinUnknown := ""
	if !tui.KindsSynced(p.cfg.Lister, namespace, kube.KindService) {
		joinUnknown = "services still loading"
	} else if tui.KindsError(p.cfg.Lister, namespace, kube.KindService) != nil {
		joinUnknown = "services unreadable"
	}
	immutable := immutableSelectorKeys(obj)
	helmOwned := objLabels["app.kubernetes.io/managed-by"] == "Helm"
	for i := range t.labels {
		l := &t.labels[i]
		if j, ok := joins[l.key]; ok {
			l.joinService, l.joinPodCount = j.service, j.podCount
		} else {
			l.joinUnknown = joinUnknown
		}
		if immutable[l.key] {
			l.readOnly, l.readOnlyNote = true, "immutable selector · server rejects this edit"
		}
		if helmOwned && l.key == "app.kubernetes.io/managed-by" {
			l.helmOwnedNote = true
		}
	}
	for i := range t.annotations {
		a := &t.annotations[i]
		if controllerManagedAnnotationKey(a.key) {
			a.readOnly, a.readOnlyNote = true, "controller-managed · read-only"
		}
		if kind == kube.KindSecret && a.key == corev1.LastAppliedConfigAnnotation {
			maskSecretManifestRow(a)
		}
	}
	return t, true
}

// buildMetaRows sorts values by key (for stable, deterministic display —
// metadata maps carry no meaningful iteration order of their own) and
// prefills each row's buffer to its current value.
func buildMetaRows(values map[string]string, isAnnotation bool, theme tui.Theme) []metaRow {
	keys := slices.Sorted(maps.Keys(values))
	styles := metaInputStyles(theme)
	rows := make([]metaRow, 0, len(keys))
	for _, k := range keys {
		r := metaRow{isAnnotation: isAnnotation, key: k, current: values[k]}
		r.input = textfield.New()
		r.input.SetStyles(styles)
		r.input.Prompt = ""
		r.input.Placeholder = "—"
		r.setBuffer(values[k])
		rows = append(rows, r)
	}
	return rows
}

// joinInfo is one label key's Service-selector join fact.
type joinInfo struct {
	service  string
	podCount int
}

// serviceLabelJoins finds, for each key in objLabels, the (alphabetically
// first, for determinism when more than one matches) Service in namespace
// whose selector matches the whole objLabels set — docs/design README.md
// §26a: "joins render before you touch anything." podCount is how many Pods
// in the namespace that Service currently selects (kind-independent of
// whatever object is being edited), the exact "detaches N pods" figure the
// confirm's warning line names.
func serviceLabelJoins(ctx context.Context, lister resources.RawLister, namespace string, objLabels map[string]string) map[string]joinInfo {
	if lister == nil || len(objLabels) == 0 {
		return nil
	}
	svcObjs, err := lister.ListRaw(ctx, kube.KindService, namespace)
	if err != nil {
		return nil
	}
	set := labels.Set(objLabels)

	type svcMatch struct {
		name     string
		selector map[string]string
	}
	var matches []svcMatch
	for _, obj := range svcObjs {
		svc, ok := obj.(*corev1.Service)
		if !ok || len(svc.Spec.Selector) == 0 {
			continue
		}
		if labels.SelectorFromSet(svc.Spec.Selector).Matches(set) {
			matches = append(matches, svcMatch{name: svc.Name, selector: svc.Spec.Selector})
		}
	}
	if len(matches) == 0 {
		return nil
	}
	slices.SortFunc(matches, func(a, b svcMatch) int { return cmp.Compare(a.name, b.name) })

	podObjs, _ := lister.ListRaw(ctx, kube.KindPod, namespace)
	out := map[string]joinInfo{}
	for key := range objLabels {
		for _, sm := range matches {
			if _, ok := sm.selector[key]; !ok {
				continue
			}
			out[key] = joinInfo{
				service:  sm.name,
				podCount: countMatchingPods(podObjs, labels.SelectorFromSet(sm.selector)),
			}
			break
		}
	}
	return out
}

// countMatchingPods counts how many Pod objects among podObjs satisfy
// selector — the real, current "how many pods does this Service serve"
// figure, independent of the object whose labels are being edited.
func countMatchingPods(podObjs []runtime.Object, selector labels.Selector) int {
	n := 0
	for _, obj := range podObjs {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}
		if selector.Matches(labels.Set(pod.Labels)) {
			n++
		}
	}
	return n
}

// immutableSelectorKeys reads a Deployment/StatefulSet/DaemonSet's own
// spec.selector.matchLabels — immutable server-side after creation (docs/
// design README.md §26a: "Deployment selector labels are immutable
// server-side — kute says so up front instead of letting the apply bounce
// off the API server"). Nil for every other kind.
func immutableSelectorKeys(obj runtime.Object) map[string]bool {
	var sel *metav1.LabelSelector
	switch o := obj.(type) {
	case *appsv1.Deployment:
		sel = o.Spec.Selector
	case *appsv1.StatefulSet:
		sel = o.Spec.Selector
	case *appsv1.DaemonSet:
		sel = o.Spec.Selector
	default:
		return nil
	}
	if sel == nil || len(sel.MatchLabels) == 0 {
		return nil
	}
	out := make(map[string]bool, len(sel.MatchLabels))
	for k := range sel.MatchLabels {
		out[k] = true
	}
	return out
}

// maskSecretManifestRow replaces a Secret's last-applied-configuration
// value — the whole manifest, data and plaintext stringData included — with
// a size-only placeholder before anything renders or copies it (§21a: a
// Secret value is never on screen unless revealed, and 26a has no reveal).
// The real value is dropped from the row entirely; the row is read-only, so
// nothing ever needs it.
func maskSecretManifestRow(r *metaRow) {
	r.current = fmt.Sprintf("•••••••• · contains secret data · %d B", len(r.current))
	r.setBuffer(r.current)
	r.readOnly, r.readOnlyNote, r.secretMasked = true, "contains secret data · read-only", true
}

// controllerManagedAnnotationKey reports whether key is one of the
// controller-written annotations §26a calls out as read-only:
// deployment.kubernetes.io/revision and the kubectl.kubernetes.io/* family.
func controllerManagedAnnotationKey(key string) bool {
	return key == "deployment.kubernetes.io/revision" || strings.HasPrefix(key, "kubectl.kubernetes.io/")
}

// metaKeyExists reports whether key already exists in t's label/annotation
// section — used to decide the ADD flow's --overwrite flag.
func metaKeyExists(t *metaTarget, isAnnotation bool, key string) bool {
	rows := t.labels
	if isAnnotation {
		rows = t.annotations
	}
	return slices.ContainsFunc(rows, func(r metaRow) bool { return r.key == key })
}

// PasteTarget mirrors Update's own three-way split: the add row's focused
// buffer while adding, the selected row's value buffer while editing, and
// nothing in navigation mode — which never accepts typed text either. The
// host's pasteTarget resolver returns this in the same position it routes
// keys to Update (CLAUDE.md's paste invariant: pointer receiver, focused
// buffer).
func (p *Model) PasteTarget() tui.PasteTarget {
	t := p.t
	switch {
	case t.adding != metaAddNone:
		if t.addOnValue {
			return tui.PasteInto(&t.addValueInput)
		}
		return tui.PasteInto(&t.addKeyInput)
	case t.editing:
		r := t.selectedRow()
		if r == nil {
			return nil
		}
		return tui.PasteInto(&r.input)
	}
	return nil
}

// Update routes one key press while the panel is open (and no confirm is
// active — the host's ctrl.Active() branch runs first, exactly like browse).
// closed is true when esc closes the panel; the host nils its pointer then.
// Add-mode and editing-mode each get first refusal (both are text-entry
// contexts, so every printable character must reach the buffer, never a
// shortcut); everything else here is navigation mode, which never accepts
// typed text at all, so its single-letter shortcuts (y, a) can never shadow
// a value.
func (p *Model) Update(msg tea.KeyPressMsg, ctrl *actions.Controller) (closed bool, cmd tea.Cmd) {
	t := p.t
	if t.adding != metaAddNone {
		return false, p.updateAddKey(msg, ctrl)
	}
	if t.editing {
		return false, p.updateEditKey(msg, ctrl)
	}
	if msg.String() != "esc" {
		// A leftover "updated env=staging"/error banner from the last
		// commit is only meant to answer "what just happened" — the moment
		// the user does anything else in navigation mode, it's stale.
		// (Editing/add-mode's own failure-restore path never routes through
		// here, so a retry-in-progress error stays visible while retyping.)
		t.message, t.lastError = "", ""
	}
	switch msg.String() {
	case "esc":
		return true, nil
	case "up", "k":
		t.moveSelection(-1)
	case "down", "j":
		t.moveSelection(1)
	case "tab", "shift+tab":
		if t.section == metaSectionLabels {
			t.section = metaSectionAnnotations
		} else {
			t.section = metaSectionLabels
		}
	case "enter":
		r := t.selectedRow()
		if r == nil || r.readOnly || !textfield.RoundTrips(r.current) {
			// A multi-line (or non-UTF-8) value would open already
			// flattened in the single-line buffer, so ↵ on the untouched
			// buffer would write the corruption back — the will-run strip
			// explains the refusal instead (view.go).
			return false, nil
		}
		r.setBuffer(r.current)
		r.input.Focus()
		t.editing = true
	case "D":
		r := t.selectedRow()
		if r == nil || r.readOnly {
			return false, nil
		}
		row := *r
		target := *t
		return false, p.commitMetaRemove(target, row, ctrl)
	case "a", "insert":
		t.adding = metaAddLabel
		if t.section == metaSectionAnnotations {
			t.adding = metaAddAnnotation
		}
		addStyles := metaAddInputStyles(p.theme())
		t.addKeyInput = textfield.New()
		t.addKeyInput.SetStyles(addStyles)
		t.addKeyInput.Prompt = ""
		t.addKeyInput.Focus()
		t.addValueInput = textfield.New()
		t.addValueInput.SetStyles(addStyles)
		t.addValueInput.Prompt = ""
		t.addOnValue = false
	case "y":
		if r := t.selectedRow(); r != nil && !r.secretMasked {
			return false, tea.SetClipboard(r.key + "=" + r.current)
		}
	}
	return false, nil
}

// moveSelection moves the focused section's own cursor by delta, clamped —
// the other section's cursor is untouched, so switching focus (tab) and back
// always returns to the same row.
func (t *metaTarget) moveSelection(delta int) {
	switch t.section {
	case metaSectionAnnotations:
		if n := len(t.annotations); n > 0 {
			t.annotationIdx = min(max(t.annotationIdx+delta, 0), n-1)
		}
	default:
		if n := len(t.labels); n > 0 {
			t.labelIdx = min(max(t.labelIdx+delta, 0), n-1)
		}
	}
}

// updateEditKey routes keys while the selected row's value is being edited
// (entered via ↵ in navigation mode) — every printable character, including
// 'a'/'A'/'y', inserts literally here; ↵ saves, esc cancels back to
// navigation without closing the panel.
func (p *Model) updateEditKey(msg tea.KeyPressMsg, ctrl *actions.Controller) tea.Cmd {
	t := p.t
	r := t.selectedRow()
	if r == nil {
		t.editing = false
		return nil
	}
	switch msg.String() {
	case "esc":
		r.setBuffer(r.current)
		r.input.Blur()
		t.editing = false
	case "enter":
		if !r.changed() {
			t.editing = false
			r.input.Blur()
			return nil
		}
		row := *r
		target := *t
		t.editing = false
		r.input.Blur()
		return p.commitMeta(target, row, true, false, ctrl)
	default:
		var cmd tea.Cmd
		r.input, cmd = r.input.Update(msg)
		return cmd
	}
	return nil
}

// updateAddKey routes keys while 'a'/insert's insert row is showing — a
// two-buffer (key, value) sub-mode distinct from normal row editing, since
// adding needs both typed rather than just a prefilled value. tab/shift+tab
// move focus forward/back between the two buffers, mirroring the rest of the
// panel's own tab/shift+tab convention rather than only going one way.
func (p *Model) updateAddKey(msg tea.KeyPressMsg, ctrl *actions.Controller) tea.Cmd {
	t := p.t
	switch msg.String() {
	case "esc":
		t.adding = metaAddNone
		t.addKeyInput.Blur()
		t.addValueInput.Blur()
	case "tab":
		t.addOnValue = true
		t.addKeyInput.Blur()
		t.addValueInput.Focus()
	case "shift+tab":
		t.addOnValue = false
		t.addValueInput.Blur()
		t.addKeyInput.Focus()
	case "enter":
		key := strings.TrimSpace(t.addKeyInput.Value())
		if key == "" {
			return nil
		}
		isAnnotation := t.adding == metaAddAnnotation
		row := metaRow{isAnnotation: isAnnotation, key: key, input: t.addValueInput}
		overwrite := metaKeyExists(t, isAnnotation, key)
		target := *t
		t.adding = metaAddNone
		t.addKeyInput.Blur()
		t.addValueInput.Blur()
		return p.commitMeta(target, row, overwrite, true, ctrl)
	default:
		var cmd tea.Cmd
		if t.addOnValue {
			t.addValueInput, cmd = t.addValueInput.Update(msg)
		} else {
			t.addKeyInput, cmd = t.addKeyInput.Update(msg)
		}
		return cmd
	}
	return nil
}

// commitMeta executes a label/annotation set through actions.Controller —
// TierNone (applies immediately, mirroring browse commitSetImage's non-PROD
// path) unless row is a Service-selector-joined label — or one whose join
// can't be known yet (joinUnknown) — which escalates to
// TierInline regardless of PROD (docs/design README.md §26a: "requires the
// inline y/N even though metadata edits are otherwise reversible" — no PROD
// dependency, unlike 24a/25a's own tiering, and no further escalation to a
// type-the-name modal). isAdd distinguishes the add sub-flow from an
// existing row's edit purely for HandleResult's own bookkeeping (which
// interaction state to restore on failure, "added"/"updated" wording on
// success) — it has no effect on tiering or the patch itself.
func (p *Model) commitMeta(t metaTarget, row metaRow, overwrite, isAdd bool, ctrl *actions.Controller) tea.Cmd {
	joined := false
	scope := tui.TaskScope{
		ResourceKind: string(t.kind), ResourceName: t.name, Namespace: t.namespace,
		Verb: "set-meta", IsMutating: true,
		MetaKey: row.key, MetaValue: row.input.Value(), MetaIsAnnotation: row.isAnnotation, MetaOverwrite: overwrite,
	}
	if row.joinService != "" {
		joined = true
		scope.MetaJoinService, scope.MetaJoinPodCount = row.joinService, row.joinPodCount
	} else if row.joinUnknown != "" {
		joined = true
	}
	kind := "label"
	if row.isAnnotation {
		kind = "annotation"
	}
	p.armCommit(&metaPendingCommit{
		isAdd: isAdd, section: sectionOf(row.isAnnotation),
		key: row.key, isAnnotation: row.isAnnotation, value: row.input.Value(),
	})
	return ctrl.Begin(actions.Request{Verb: verbs.Meta, Joined: joined}, tui.TaskAction{
		ID:    "set-meta-" + t.namespace + "/" + t.name + "/" + row.key,
		Label: fmt.Sprintf("Set %s %s on %s?", kind, row.key, t.name),
		Scope: scope,
	})
}

// commitMetaRemove executes a key removal through actions.Controller —
// always TierInline (docs/design README.md §26a: "reversible, no
// type-the-name modal — per 8b's tiering"), never PROD-escalated further.
func (p *Model) commitMetaRemove(t metaTarget, row metaRow, ctrl *actions.Controller) tea.Cmd {
	kind := "label"
	if row.isAnnotation {
		kind = "annotation"
	}
	p.armCommit(&metaPendingCommit{
		isRemove: true, section: sectionOf(row.isAnnotation),
		key: row.key, isAnnotation: row.isAnnotation,
	})
	return ctrl.Begin(actions.Request{Verb: verbs.Meta}, tui.TaskAction{
		ID:    "remove-meta-" + t.namespace + "/" + t.name + "/" + row.key,
		Label: fmt.Sprintf("Remove %s %s from %s?", kind, row.key, t.name),
		Scope: tui.TaskScope{
			ResourceKind: string(t.kind), ResourceName: t.name, Namespace: t.namespace,
			Verb: "set-meta", IsMutating: true,
			MetaKey: row.key, MetaIsAnnotation: row.isAnnotation, MetaRemove: true,
		},
	})
}

// armCommit records what a commit about to start (via actions.Begin) is
// attempting, on the live panel. Clears any stale message/error from a
// previous commit, the same "clear before the next attempt" point 24a/25a's
// own dry-run error handling uses.
func (p *Model) armCommit(pc *metaPendingCommit) {
	p.t.pendingCommit = pc
	p.t.message = ""
	p.t.lastError = ""
}

// sectionOf maps a row's isAnnotation flag to its grid — shared by
// commitMeta/commitMetaRemove's metaPendingCommit construction above.
func sectionOf(isAnnotation bool) metaSection {
	if isAnnotation {
		return metaSectionAnnotations
	}
	return metaSectionLabels
}

// CancelConfirm reverts the selected row's buffer when the host cancels a
// TierInline confirm (n/esc): the panel stayed open under the confirm with
// the row's edit already applied to its buffer, and cancelling must revert
// it — the same "esc backs out without keeping the typed change" contract
// editing mode's own esc already has. A no-op for a pending removal, whose
// buffer never diverged from current in the first place. The host still
// calls ctrl.Cancel() itself (browse's cancelInlineConfirm shape).
func (p *Model) CancelConfirm() {
	if r := p.t.selectedRow(); r != nil {
		r.setBuffer(r.current)
	}
}

// HandleResult applies a set-meta/remove-meta action's outcome to the
// still-open panel — the host's actions.ResultMsg case calls this instead
// of ever nilling its panel pointer itself, per docs/design README.md §26a's
// own contract: "confirm → execute → refresh → show result → remain on
// screen." closed is true only when the object vanished mid-edit (deleted
// concurrently, most likely) — nothing left to refresh into, so the panel
// closes rather than sit open on a stale/empty shell.
//
// On success, the object is re-fetched and the grid rebuilt from the real,
// current cluster state (buildTarget — never an optimistic local patch),
// recomputing joins/immutable-selector/controller-managed/Helm-owned flags
// fresh; focus lands back on the row that was just touched (by key), or —
// after a removal — the nearest remaining row in the same grid.
//
// On failure, nothing is refetched: the row/add-buffers are restored to
// exactly their pre-commit interaction state (still editing, or still in the
// add sub-flow) with the attempted value intact, and the server's error is
// surfaced via t.lastError (the will-run strip).
//
// Only esc/back ever closes the panel from here — a failed or successful
// commit never does.
func (p *Model) HandleResult(msg actions.ResultMsg) (closed bool) {
	t := p.t
	pc := t.pendingCommit
	t.pendingCommit = nil

	if msg.Err != nil {
		t.lastError = msg.Err.Error()
		t.message = ""
		if pc == nil {
			return false
		}
		t.section = pc.section
		if pc.isAdd {
			t.adding = metaAddLabel
			if pc.isAnnotation {
				t.adding = metaAddAnnotation
			}
			t.addKeyInput.SetValue(pc.key)
			t.addKeyInput.CursorEnd()
			t.addValueInput.SetValue(pc.value)
			t.addValueInput.CursorEnd()
			t.addOnValue = true
			t.addKeyInput.Blur()
			t.addValueInput.Focus()
			return false
		}
		rows, idx := t.rowsFor(pc.section)
		for i := range rows {
			if rows[i].key != pc.key {
				continue
			}
			*idx = i
			if !pc.isRemove {
				rows[i].setBuffer(pc.value)
				rows[i].input.Focus()
				t.editing = true
			}
			break
		}
		return false
	}

	// docs/design README.md §26a: "Show an inline success message such as
	// updated env=staging or removed kute.dev/owner" — key=value for a
	// set, bare key for a removal, verbatim.
	message := "updated"
	switch {
	case pc == nil:
	case pc.isRemove:
		message = "removed " + pc.key
	case pc.isAdd:
		message = fmt.Sprintf("added %s=%s", pc.key, pc.value)
	default:
		message = fmt.Sprintf("updated %s=%s", pc.key, pc.value)
	}

	fresh, ok := p.buildTarget(t.kind, t.namespace, t.name)
	if !ok {
		return true
	}
	fresh.message = message
	fresh.section = t.section
	targetKey := ""
	if pc != nil {
		targetKey = pc.key
	}
	fresh.labelIdx = metaFocusIndex(fresh.labels, t.labelIdx, targetKey, t.section == metaSectionLabels)
	fresh.annotationIdx = metaFocusIndex(fresh.annotations, t.annotationIdx, targetKey, t.section == metaSectionAnnotations)
	p.t = fresh
	return false
}

// Refresh rebuilds the open panel from the cache after the host saw a
// kube.ResourceChangedMsg for kind — the panel's own kind, or Service (the
// selector joins). HandleResult's own refetch usually runs before the watch
// event for the patch it just made has reached the cache, so without this the
// grid would keep showing the pre-commit value under its "updated k=v" line
// (and a following ↵ would pre-fill it) until the panel was reopened.
//
// Everything the user is in the middle of survives: section, each grid's
// cursor (by key), the result/error line, an open row buffer (as typed — only
// its current, and so changed(), moves), and the add row. A no-op while a
// commit is in flight or its confirm is showing, since that confirm describes
// the rows as they stand, and when the object can't be read (closing is left
// to esc and HandleResult).
func (p *Model) Refresh(kind kube.ResourceKind, ctrl *actions.Controller) {
	t := p.t
	if kind != t.kind && kind != kube.KindService {
		return
	}
	if t.pendingCommit != nil || (ctrl != nil && ctrl.Active()) {
		return
	}
	fresh, ok := p.buildTarget(t.kind, t.namespace, t.name)
	if !ok {
		return
	}
	fresh.section = t.section
	fresh.message, fresh.lastError = t.message, t.lastError
	fresh.labelIdx = metaKeepIndex(t.labels, fresh.labels, t.labelIdx)
	fresh.annotationIdx = metaKeepIndex(t.annotations, fresh.annotations, t.annotationIdx)
	if t.editing {
		if old, cur := t.selectedRow(), fresh.selectedRow(); old != nil && cur != nil && old.key == cur.key {
			cur.input = old.input
			fresh.editing = true
		}
	}
	fresh.adding, fresh.addOnValue = t.adding, t.addOnValue
	fresh.addKeyInput, fresh.addValueInput = t.addKeyInput, t.addValueInput
	p.t = fresh
}

// metaKeepIndex is Refresh's cursor carry-over: the row with the same key as
// oldRows[oldIdx] if it's still there, else the same position, clamped.
func metaKeepIndex(oldRows, newRows []metaRow, oldIdx int) int {
	if len(newRows) == 0 {
		return 0
	}
	if oldIdx >= 0 && oldIdx < len(oldRows) {
		for i, r := range newRows {
			if r.key == oldRows[oldIdx].key {
				return i
			}
		}
	}
	return max(min(oldIdx, len(newRows)-1), 0)
}

// metaFocusIndex picks the refreshed grid's cursor after a commit: the same
// key's new row when it still exists (an edit or an add), or — once it's
// gone (a removal) — the nearest remaining row at about the same position.
// applies is false for the grid that wasn't focused at commit time, which
// always just keeps index 0 (its cursor is meaningless until focused again).
func metaFocusIndex(newRows []metaRow, oldIdx int, targetKey string, applies bool) int {
	if !applies || len(newRows) == 0 {
		return 0
	}
	if targetKey != "" {
		for i, r := range newRows {
			if r.key == targetKey {
				return i
			}
		}
	}
	return min(oldIdx, len(newRows)-1)
}

// WillRunLine renders the exact "will run: kubectl label/annotate ..."
// line for a pending TierInline confirmation's keybar RightNote — same
// "read straight off the resolved Scope" idiom as browse's
// setImageWillRunLine/setResourcesWillRunLine.
func WillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.MetaCommandString(
		kube.ResourceKind(scope.ResourceKind), scope.Namespace, scope.ResourceName,
		scope.MetaIsAnnotation, scope.MetaKey, scope.MetaValue, scope.MetaRemove, scope.MetaOverwrite,
	)
}

// KeybarHints is the META-pill hint groups for the panel's current sub-mode
// — lifted from browse's own three-way switch so every host renders the
// identical keybar. The host wraps it in its own tui.Keybar with PillText
// "META", and only while !ctrl.Active() (a TierInline confirm's own y/N
// keybar wins instead).
func (p *Model) KeybarHints() [][]tui.KeyHint {
	t := p.t
	var hints []tui.KeyHint
	switch {
	case t.adding != metaAddNone:
		// Editing mode (add sub-flow): every printable character inserts
		// literally, so these are the only reserved keys.
		hints = []tui.KeyHint{
			{Key: "↵", Label: "apply"}, {Key: tui.GlyphTab, Label: "key ↔ value"}, {Key: "esc", Label: "cancel"},
		}
	case t.editing:
		// Editing mode: same reasoning — typing a value must never be
		// shadowed by a shortcut.
		hints = []tui.KeyHint{{Key: "↵", Label: "save"}, {Key: "esc", Label: "cancel"}}
	default:
		// Navigation mode never accepts typed text, so single-letter
		// shortcuts here (a, y) can't shadow a value the way they could
		// if typing edited the row directly.
		hints = []tui.KeyHint{
			{Key: "↑↓", Label: "row"}, {Key: tui.GlyphTab, Label: "switch grid"},
			{Key: "↵", Label: "edit"}, {Key: "a/insert", Label: "add"},
			{Key: "D", Label: "remove key · y/N"}, {Key: "y", Label: "copy key=value"},
		}
	}
	return [][]tui.KeyHint{hints}
}

// findObject finds the named raw object of kind in namespace via
// lister.ListRaw — a private copy of browse's workloadObject (task packages
// can't import one another; the five-line duplication is the accepted cost,
// same as jobattempts' rerun copy).
func findObject(ctx context.Context, lister resources.RawLister, kind kube.ResourceKind, namespace, name string) (runtime.Object, bool) {
	if lister == nil {
		return nil, false
	}
	objs, err := lister.ListRaw(ctx, kind, namespace)
	if err != nil {
		return nil, false
	}
	for _, obj := range objs {
		acc, err := apimeta.Accessor(obj)
		if err != nil {
			continue
		}
		if acc.GetName() == name && acc.GetNamespace() == namespace {
			return obj, true
		}
	}
	return nil, false
}
