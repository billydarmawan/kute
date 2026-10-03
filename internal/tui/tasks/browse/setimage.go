// 24a's 'i' inline set-image/set-tag editor (docs/design README.md §24a):
// reversible outside PROD, so — like scale.go's pendingScale — this is a
// bespoke gate (pendingSetImage) rather than actions.Controller's y/N/
// type-name flow, since there's a container/tag/history buffer to gather
// before there's anything to Begin. Once ↵ commits, execution itself does
// go through actions.Controller/kube.Mutator (verbs.Resolve(verbs.SetImage, …) decides
// TierNone outside PROD vs. TierInline in PROD — the ordinary inline y/N
// Controller already renders for rollback/delete). Kept in its own file,
// browse's per-concern split convention (like scale.go/edit.go).
package browse

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// imageHistoryEntry is one row of 24a's TAG · SEEN · FROM table, sourced
// entirely from the watch cache (never a registry call).
type imageHistoryEntry struct {
	tag       string
	seenAt    time.Time // sort key only, never rendered directly
	seenLabel string    // the SEEN column's rendered text
	from      string    // the FROM column's rendered text
}

// setImageContainer identifies both a container and the PodSpec list that
// owns it. Native sidecars are init containers at the API level even though
// the UI presents them alongside long-running containers.
type setImageContainer struct {
	kube.ContainerInfo
	initContainer bool
}

// setImageTarget is the state pendingSetImage gates on while 24a's panel is
// showing.
type setImageTarget struct {
	kind      kube.ResourceKind
	namespace string
	name      string
	// created is the workload's own CreationTimestamp — the fallback "seen"
	// clock for StatefulSet/DaemonSet, which have no ReplicaSet revision
	// layer to read a more precise one from.
	created      time.Time
	desiredCount int32 // "applying rolls out N pods" — apps workloads' own currentReplicas(row)
	containers   []setImageContainer
	containerIdx int
	// repo is the active container's image repo, the dim prefix in
	// non-fullRef mode.
	repo string
	// input is the type-ahead value: just the tag outside fullRef mode, the
	// whole repo:tag ref once ctrl-e unlocks it. Every wholesale buffer
	// replacement (container switch, ctrl-e toggle, a history pick) parks
	// the cursor at the end via setBuffer, same as scale.go's prompt always
	// leaving the cursor ready to append/backspace.
	input      textfield.Model
	fullRef    bool
	history    []imageHistoryEntry
	historyIdx int // -1 = nothing picked/matched

	// pendingCommit is set the instant a commit starts (TierNone's
	// synchronous apply, or a TierInline confirm) and cleared once
	// handleSetImageResult applies its outcome — mirrors metaTarget's own
	// field of the same name/purpose.
	pendingCommit *setImagePendingCommit
	// awaitingRefresh keeps the just-applied value authoritative in the
	// editor until the informer cache observes the write. The API response
	// can beat that watch update, so rebuilding immediately from the cache
	// would briefly restore the old tag.
	awaitingRefresh *setImagePendingCommit
	// message/lastError are the panel's own transient inline result line —
	// "set image: worker=..." on success, the raw server error on failure —
	// cleared the next time a commit starts or the user does anything else
	// (updateSetImageKey).
	message   string
	lastError string
}

// setImagePendingCommit remembers what a TierNone or confirmed-TierInline
// commit is currently trying to write, so handleSetImageResult can build the
// right inline success message and know which container to re-select after a
// refresh — mirrors metaPendingCommit.
type setImagePendingCommit struct {
	container     string
	image         string
	initContainer bool
}

// setBuffer replaces t.input's value wholesale and parks the cursor at its
// end — the shared tail of every place buffer changes as a whole rather than
// by a single keystroke (selectSetImageContainer, ctrl-e, a history pick).
func (t *setImageTarget) setBuffer(s string) {
	t.input.SetValue(s)
	t.input.CursorEnd()
}

// activeContainer is the container the panel is currently editing.
func (t setImageTarget) activeContainer() setImageContainer {
	return t.containers[t.containerIdx]
}

// composedImage is the full image ref the buffer currently represents.
func (t setImageTarget) composedImage() string {
	if t.fullRef {
		return t.input.Value()
	}
	// A tag+digest reference is still the current image while its tag field
	// is untouched. Once the tag changes, drop the old digest: retaining it
	// would keep selecting the old content (or produce an invalid digest).
	if t.input.Value() == tagOf(t.activeContainer().Image) {
		return t.activeContainer().Image
	}
	return t.repo + ":" + t.input.Value()
}

// unchanged reports whether composedImage equals the active container's
// current image — §24a: "re-entering the current tag flips the strip to
// 'same image — apply is a no-op'".
func (t setImageTarget) unchanged() bool {
	if t.awaitingRefresh != nil && t.activeContainer().Name == t.awaitingRefresh.container && t.composedImage() == t.awaitingRefresh.image {
		return true
	}
	return t.composedImage() == t.activeContainer().Image
}

// imageEditable reports whether kind takes 24a's set-image prompt —
// Deployment, StatefulSet, DaemonSet, and CronJob: the four browse kinds with
// a pod template `kubectl set image` can target.
func imageEditable(kind kube.ResourceKind) bool {
	return kind == kube.KindDeployment || kind == kube.KindStatefulSet || kind == kube.KindDaemonSet || kind == kube.KindCronJob
}

// beginSetImage opens 24a's panel for the selected row. ok is false when
// nothing applies (wrong kind, no mutator, not ready, no row selected, or
// the raw object/containers can't be resolved from the watch cache) —
// mirroring beginScale's ok-bool contract.
func (m *Model) beginSetImage() bool {
	if !imageEditable(m.kind) || m.mutator == nil || m.state != tui.TaskStateReady {
		return false
	}
	row, ok := m.selectedRow()
	if !ok {
		return false
	}
	t, ok := m.buildSetImageTarget(m.kind, row.Namespace, row.Name, currentReplicas(row))
	if !ok {
		return false
	}
	m.pendingSetImage = t
	m.selectSetImageContainer(0)
	return true
}

// buildSetImageTarget fetches kind/namespace/name fresh from the lister and
// builds a setImageTarget shell (no container selected yet — the caller must
// call selectSetImageContainer) — the fetch-and-construct half of
// beginSetImage, factored out so handleSetImageResult's own post-commit
// refresh always reflects the authoritative server state rather than an
// optimistic local patch, exactly like buildMetaTarget. desiredCount is
// passed in rather than re-derived from a row, since a set-image commit never
// changes replica count and the row backing it may not have reloaded yet.
func (m *Model) buildSetImageTarget(kind kube.ResourceKind, namespace, name string, desiredCount int32) (*setImageTarget, bool) {
	obj, ok := workloadObject(m.session.ClusterContext(), m.lister, kind, namespace, name)
	if !ok {
		return nil, false
	}
	containers := setImageContainerInfos(obj)
	if len(containers) == 0 {
		return nil, false
	}
	acc, err := apimeta.Accessor(obj)
	created := time.Time{}
	if err == nil {
		created = acc.GetCreationTimestamp().Time
	}
	input := textfield.New()
	input.Prompt = ""
	styles := tui.TextInputStyles(m.Theme())
	styles.Focused.Text = styles.Focused.Text.Bold(true)
	styles.Blurred.Text = styles.Blurred.Text.Bold(true)
	input.SetStyles(styles)
	input.Focus()
	return &setImageTarget{
		kind: kind, namespace: namespace, name: name,
		created: created, desiredCount: desiredCount,
		containers: containers, input: input,
	}, true
}

// selectSetImageContainer switches the panel's active container tab
// (beginSetImage's initial 0, or 'tab' cycling), recomputing repo/buffer/
// history for the newly active container.
func (m *Model) selectSetImageContainer(idx int) {
	t := m.pendingSetImage
	t.containerIdx = idx
	resetSetImageBuffer(t)
	c := t.activeContainer()
	t.history = imageHistory(m.session.ClusterContext(), m.lister, t.kind, t.namespace, t.name, c.Name, c.Image, c.initContainer, t.created)
	t.historyIdx = matchHistoryIndex(t)
}

// resetSetImageBuffer parks t's buffer back on the active container's real
// current image — the tag-only, non-fullRef prefill selectSetImageContainer
// uses for a fresh container tab, and also what cancelling a PROD confirm
// (update.go's revertConfirmedEditor) reverts to, discarding whatever was typed
// — the same "esc backs out without keeping the typed change" contract
// meta.go's own cancel path has for a joined-label edit.
func resetSetImageBuffer(t *setImageTarget) {
	c := t.activeContainer()
	t.repo = imageRepo(c.Image)
	t.setBuffer(tagOf(c.Image))
	t.fullRef = false
}

// setImagePasteTarget is the image/tag buffer — the field most likely to be
// filled from the clipboard, since a digest or a CI-produced tag is not
// something anyone retypes. Re-matches the history rail after insertion,
// exactly as the typed path does.
func (m *Model) setImagePasteTarget() tui.PasteTarget {
	t := m.pendingSetImage
	insert := tui.PasteInto(&t.input)
	return func(s string) {
		before := t.input.Value()
		insert(s)
		if t.input.Value() != before {
			t.historyIdx = matchHistoryIndex(t)
		}
	}
}

// updateSetImageKey routes keys while pendingSetImage's panel is showing.
func (m *Model) updateSetImageKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	t := m.pendingSetImage
	if msg.String() != "esc" {
		// A leftover "set image: worker=..."/error banner from the last
		// commit only ever answers "what just happened" — the moment the
		// user does anything else, it's stale (mirrors updateMetaKey's own
		// navigation-mode clearing).
		t.message, t.lastError = "", ""
	}
	switch msg.String() {
	case "esc":
		m.pendingSetImage = nil
	case "enter":
		if t.unchanged() {
			return m, nil
		}
		return m, m.commitSetImage(*t)
	case verbs.NextPanel.Press():
		if len(t.containers) > 1 {
			m.selectSetImageContainer((t.containerIdx + 1) % len(t.containers))
		}
	case verbs.SetImageFullRef.Key:
		if t.fullRef {
			repo := imageRepo(t.input.Value())
			tag := tagOf(t.input.Value())
			t.repo = repo
			t.setBuffer(tag)
			t.fullRef = false
		} else {
			t.setBuffer(t.composedImage())
			t.fullRef = true
		}
		t.historyIdx = matchHistoryIndex(t)
	case "up":
		m.stepSetImageHistory(-1)
	case "down":
		m.stepSetImageHistory(1)
	default:
		var cmd tea.Cmd
		before := t.input.Value()
		t.input, cmd = t.input.Update(msg)
		if t.input.Value() != before {
			t.historyIdx = matchHistoryIndex(t)
		}
		return m, cmd
	}
	return m, nil
}

// stepSetImageHistory moves historyIdx by delta (clamped) and overwrites
// buffer from the picked entry's tag — §24a's "↑↓ pick from history".
func (m *Model) stepSetImageHistory(delta int) {
	t := m.pendingSetImage
	if len(t.history) == 0 {
		return
	}
	idx := max(min(t.historyIdx+delta, len(t.history)-1), 0)
	t.historyIdx = idx
	tag := t.history[idx].tag
	if t.fullRef {
		t.setBuffer(t.repo + ":" + tag)
	} else {
		t.setBuffer(tag)
	}
}

// matchHistoryIndex finds the history entry (if any) whose tag equals t's
// current buffer — so a typed-exact-match highlights the same as an
// arrow-picked one (docs/design README.md §24a's mockup shows the prefilled
// tag already highlighting its own history row). In fullRef mode, a buffer
// that no longer names the same repo can't match any entry (history was
// built against the original repo).
func matchHistoryIndex(t *setImageTarget) int {
	tag := t.input.Value()
	if t.fullRef {
		if imageRepo(t.input.Value()) != t.repo {
			return -1
		}
		tag = tagOf(t.input.Value())
	}
	return slices.IndexFunc(t.history, func(e imageHistoryEntry) bool { return e.tag == tag })
}

// commitSetImage executes t through actions.Controller — verbs.Resolve(verbs.SetImage, …)
// resolves TierNone outside PROD (Begin runs it immediately, mirroring
// commitScale) or TierInline in PROD (Controller's ordinary inline y/N, the
// same path rollback/delete already take). Arms pendingCommit first so
// handleSetImageResult knows what was attempted once the result comes back —
// the panel itself stays open the whole time (docs/design README.md §26a's
// "confirm → execute → refresh → show result → remain on screen" contract,
// 24a's own retrofit onto it).
func (m *Model) commitSetImage(t setImageTarget) tea.Cmd {
	c := t.activeContainer()
	image := t.composedImage()
	m.armSetImageCommit(&setImagePendingCommit{container: c.Name, image: image, initContainer: c.initContainer})
	return m.actions.Begin(actions.Request{Verb: verbs.SetImage, Prod: m.isProd()}, tui.TaskAction{
		ID:    "set-image-" + t.namespace + "/" + t.name,
		Label: fmt.Sprintf("Set image for %s?", t.name),
		Scope: tui.TaskScope{
			ResourceKind:  string(t.kind),
			ResourceName:  t.name,
			Namespace:     t.namespace,
			Verb:          "set-image",
			IsMutating:    true,
			Container:     c.Name,
			Image:         image,
			InitContainer: c.initContainer,
		},
	})
}

// armSetImageCommit records what a commit about to start (via actions.Begin)
// is attempting, on the live panel — a no-op if the panel somehow isn't open
// (defensive only; commitSetImage is itself pendingSetImage-gated). Clears
// any stale message/error from a previous commit — mirrors armMetaCommit.
func (m *Model) armSetImageCommit(pc *setImagePendingCommit) {
	if m.pendingSetImage == nil {
		return
	}
	m.pendingSetImage.pendingCommit = pc
	m.pendingSetImage.awaitingRefresh = nil
	m.pendingSetImage.message = ""
	m.pendingSetImage.lastError = ""
}

// handleSetImageResult applies a set-image action's outcome to the still-open
// panel — update.go's actions.ResultMsg case calls this instead of ever
// nulling m.pendingSetImage itself, mirroring handleMetaResult's own doc
// comment and docs/design README.md §26a's contract: "confirm → execute →
// refresh → show result → remain on screen."
//
// On success, the object is re-fetched and the panel rebuilt only once the
// informer has observed the applied image. The API response can arrive before
// that watch update; until then awaitingRefresh keeps the submitted buffer in
// place and the matching ResourceChangedMsg calls refreshSetImageTarget.
//
// On failure, nothing is refetched: the buffer still holds the attempted
// value (updateSetImageKey never cleared it), and the server's error is
// surfaced via t.lastError (setimage_view.go's will-run strip).
//
// Only esc ever closes the panel from here — a failed or successful commit
// never does (except the object having vanished entirely, matching
// handleMetaResult's own fallback).
func (m *Model) handleSetImageResult(msg actions.ResultMsg) tea.Cmd {
	t := m.pendingSetImage
	pc := t.pendingCommit
	t.pendingCommit = nil

	if msg.Err != nil {
		t.lastError = msg.Err.Error()
		t.message = ""
		return nil
	}

	message := "applied"
	if pc != nil {
		message = fmt.Sprintf("set image: %s=%s", pc.container, pc.image)
	}
	fresh, ok := m.buildSetImageTarget(t.kind, t.namespace, t.name, t.desiredCount)
	if !ok {
		// The object vanished from the lister (deleted concurrently, most
		// likely) — nothing left to refresh into, so the panel closes rather
		// than sit open on a stale/empty shell.
		m.pendingSetImage = nil
		return nil
	}
	if pc != nil && !setImageTargetHasImage(fresh, pc.container, pc.image, pc.initContainer) {
		// The API write won the race with the informer. Keep the submitted
		// value visible; the matching watch event will rebuild from cache.
		t.awaitingRefresh = pc
		t.message = message
		t.lastError = ""
		return nil
	}
	m.pendingSetImage = fresh
	idx := 0
	if pc != nil {
		for i, c := range fresh.containers {
			if c.Name == pc.container && c.initContainer == pc.initContainer {
				idx = i
				break
			}
		}
	}
	m.selectSetImageContainer(idx)
	m.pendingSetImage.message = message
	return nil
}

// refreshSetImageTarget confirms a successful apply from the informer cache.
// A same-kind event may belong to another object, so the panel is replaced
// only when the expected container image is actually present.
func (m *Model) refreshSetImageTarget() {
	t := m.pendingSetImage
	pc := t.awaitingRefresh
	if pc == nil {
		return
	}
	selected := t.activeContainer()
	fresh, ok := m.buildSetImageTarget(t.kind, t.namespace, t.name, t.desiredCount)
	if !ok || !setImageTargetHasImage(fresh, pc.container, pc.image, pc.initContainer) {
		return
	}
	message, lastError := t.message, t.lastError
	m.pendingSetImage = fresh
	idx := 0
	for i, c := range fresh.containers {
		if c.Name == selected.Name && c.initContainer == selected.initContainer {
			idx = i
			break
		}
	}
	m.selectSetImageContainer(idx)
	m.pendingSetImage.message = message
	m.pendingSetImage.lastError = lastError
}

func setImageTargetHasImage(t *setImageTarget, container, image string, initContainer bool) bool {
	for _, c := range t.containers {
		if c.Name == container && c.initContainer == initContainer {
			return c.Image == image
		}
	}
	return false
}

// setImageContainerInfos extracts every image target from obj's pod template:
// regular containers first, then conventional init containers and native
// sidecars in declaration order. Set Resources deliberately keeps using
// workloadContainerInfos, whose narrower target set is unchanged.
func setImageContainerInfos(obj runtime.Object) []setImageContainer {
	var spec corev1.PodSpec
	switch o := obj.(type) {
	case *appsv1.Deployment:
		spec = o.Spec.Template.Spec
	case *appsv1.StatefulSet:
		spec = o.Spec.Template.Spec
	case *appsv1.DaemonSet:
		spec = o.Spec.Template.Spec
	case *batchv1.CronJob:
		spec = o.Spec.JobTemplate.Spec.Template.Spec
	default:
		return nil
	}
	infos := make([]setImageContainer, 0, len(spec.Containers)+len(spec.InitContainers))
	for _, c := range spec.Containers {
		infos = append(infos, setImageContainer{ContainerInfo: kube.ContainerInfo{Name: c.Name, Image: c.Image}})
	}
	for _, c := range spec.InitContainers {
		sidecar := c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
		infos = append(infos, setImageContainer{
			ContainerInfo: kube.ContainerInfo{Name: c.Name, Image: c.Image, IsSidecar: sidecar},
			initContainer: true,
		})
	}
	return infos
}

// setImageWillRunLine renders the exact "will run: kubectl set image ..."
// line for a pending TierInline (PROD) confirmation's keybar RightNote —
// same idiom as deleteWillRunLine/rollbackPrompt, reading straight off the
// already-resolved actions.Controller Scope rather than pendingSetImage
// (which is nil by the time a PROD confirm is showing — commitSetImage
// clears it before Begin).
func setImageWillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.SetImageCommandString(kube.ResourceKind(scope.ResourceKind), scope.Namespace, scope.ResourceName, scope.Container, scope.Image)
}

// workloadObject finds the named raw object of kind in namespace via
// lister.ListRaw — the same cache-read lookup shape scale.go's hpaManaging
// already uses for the HPA-managed-workload lookup.
func workloadObject(ctx context.Context, lister resources.RawLister, kind kube.ResourceKind, namespace, name string) (runtime.Object, bool) {
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

// workloadContainerInfos extracts obj's pod-template containers (name/image)
// plus native sidecars (initContainers with restartPolicy: Always, appended
// after the regular containers) — the same merge kube.buildContainerInfos
// does for a live Pod, minus the live-status merge this doesn't need.
func workloadContainerInfos(obj runtime.Object) []kube.ContainerInfo {
	var spec corev1.PodSpec
	switch o := obj.(type) {
	case *appsv1.Deployment:
		spec = o.Spec.Template.Spec
	case *appsv1.StatefulSet:
		spec = o.Spec.Template.Spec
	case *appsv1.DaemonSet:
		spec = o.Spec.Template.Spec
	case *batchv1.CronJob:
		spec = o.Spec.JobTemplate.Spec.Template.Spec
	default:
		return nil
	}
	infos := make([]kube.ContainerInfo, 0, len(spec.Containers)+len(spec.InitContainers))
	for _, c := range spec.Containers {
		infos = append(infos, kube.ContainerInfo{Name: c.Name, Image: c.Image})
	}
	for _, c := range spec.InitContainers {
		if c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways {
			continue // a regular (non-sidecar) init container
		}
		infos = append(infos, kube.ContainerInfo{Name: c.Name, Image: c.Image, IsSidecar: true})
	}
	return infos
}

// imageHistory combines this workload's own revision history with
// cross-workload sightings of the same image repo, newest-seen-first,
// deduped by tag (the most recent sighting of a tag wins regardless of
// source), capped to a reasonable panel-scrollable count.
func imageHistory(ctx context.Context, lister resources.RawLister, kind kube.ResourceKind, namespace, name, container, currentImage string, initContainer bool, created time.Time) []imageHistoryEntry {
	const maxEntries = 8
	repo, currentTag := imageRepo(currentImage), tagOf(currentImage)

	entries := ownRevisionHistory(ctx, lister, kind, namespace, name, container, currentImage, initContainer, created)
	entries = append(entries, crossWorkloadHistory(ctx, lister, kind, namespace, name, repo, currentTag)...)
	slices.SortStableFunc(entries, func(a, b imageHistoryEntry) int { return b.seenAt.Compare(a.seenAt) })

	seen := make(map[string]bool, len(entries))
	out := make([]imageHistoryEntry, 0, min(len(entries), maxEntries))
	for _, e := range entries {
		// A digest-only sighting has no tag to pick: the rail sets
		// repo:<tag>, so it can't express that image at all.
		if e.tag == "" || seen[e.tag] {
			continue
		}
		seen[e.tag] = true
		out = append(out, e)
		if len(out) >= maxEntries {
			break
		}
	}
	return out
}

// revisionCandidate is one raw (revision number, image, created-at) sample
// gathered from either a Deployment's owned ReplicaSets or a StatefulSet/
// DaemonSet's owned ControllerRevisions, before labelRevisions turns them
// into imageHistoryEntry rows.
type revisionCandidate struct {
	n       int64
	created time.Time
	image   string
}

// ownRevisionHistory reads this workload's own rollout history: a
// Deployment's owned ReplicaSets' "deployment.kubernetes.io/revision"
// annotation (the same signal kube/timeline.go's TimelineFromRollouts reads
// for 16b's rail), or — since StatefulSet/DaemonSet own no ReplicaSets —
// its owned ControllerRevisions (apps/v1), the same mechanism `kubectl
// rollout history statefulset|daemonset` itself reads. Falls back to a
// single "current" row (built from the workload's own creation time) when
// no revision object has been seen yet — a fresh object, or the informer
// cache still catching up.
func ownRevisionHistory(ctx context.Context, lister resources.RawLister, kind kube.ResourceKind, namespace, name, container, currentImage string, initContainer bool, created time.Time) []imageHistoryEntry {
	fallback := []imageHistoryEntry{{
		tag:       tagOf(currentImage),
		seenAt:    created,
		seenLabel: shortAge(time.Since(created)) + " · current",
		from:      "this " + strings.ToLower(string(kind)),
	}}
	if lister == nil {
		return fallback
	}
	var revs []revisionCandidate
	switch kind {
	case kube.KindDeployment:
		revs = deploymentRevisions(ctx, lister, namespace, name, container, initContainer)
	case kube.KindStatefulSet, kube.KindDaemonSet:
		revs = controllerRevisions(ctx, lister, kind, namespace, name, container, initContainer)
	default:
		return fallback
	}
	if len(revs) == 0 {
		return fallback
	}
	return labelRevisions(revs, kind)
}

// deploymentRevisions gathers revisionCandidates from a Deployment's owned
// ReplicaSets — the source ownRevisionHistory's doc comment describes.
func deploymentRevisions(ctx context.Context, lister resources.RawLister, namespace, name, container string, initContainer bool) []revisionCandidate {
	objs, err := lister.ListRaw(ctx, kube.KindReplicaSet, namespace)
	if err != nil {
		return nil
	}
	var revs []revisionCandidate
	for _, obj := range objs {
		rs, ok := obj.(*appsv1.ReplicaSet)
		if !ok {
			continue
		}
		if owner := kube.ControllerRef(rs.OwnerReferences); owner == nil || owner.Kind != "Deployment" || owner.Name != name {
			continue
		}
		revText, ok := rs.Annotations["deployment.kubernetes.io/revision"]
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(revText, 10, 64)
		if err != nil {
			continue
		}
		containers := rs.Spec.Template.Spec.Containers
		if initContainer {
			containers = rs.Spec.Template.Spec.InitContainers
		}
		image := containerImageByName(containers, container)
		if image == "" {
			continue
		}
		revs = append(revs, revisionCandidate{n: n, created: rs.CreationTimestamp.Time, image: image})
	}
	return revs
}

// controllerRevisions gathers revisionCandidates from a StatefulSet/
// DaemonSet's owned ControllerRevisions. Data.Raw is a JSON encoding of
// {"spec":{"template":{"spec":{"containers":[...]}}}} — the patch shape the
// StatefulSet/DaemonSet controllers themselves generate for each revision
// and `kubectl rollout history` decodes the same way.
func controllerRevisions(ctx context.Context, lister resources.RawLister, kind kube.ResourceKind, namespace, name, container string, initContainer bool) []revisionCandidate {
	objs, err := lister.ListRaw(ctx, kube.KindControllerRevision, namespace)
	if err != nil {
		return nil
	}
	var revs []revisionCandidate
	for _, obj := range objs {
		cr, ok := obj.(*appsv1.ControllerRevision)
		if !ok {
			continue
		}
		if owner := kube.ControllerRef(cr.OwnerReferences); owner == nil || owner.Kind != string(kind) || owner.Name != name {
			continue
		}
		image := controllerRevisionContainerImage(cr, container, initContainer)
		if image == "" {
			continue
		}
		revs = append(revs, revisionCandidate{n: cr.Revision, created: cr.CreationTimestamp.Time, image: image})
	}
	return revs
}

// controllerRevisionContainerImage decodes cr.Data.Raw's patch just enough
// to pull out container's image — the fields ownRevisionHistory's doc
// comment names, ignoring everything else the patch carries.
func controllerRevisionContainerImage(cr *appsv1.ControllerRevision, container string, initContainer bool) string {
	var patch struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers     []corev1.Container `json:"containers"`
					InitContainers []corev1.Container `json:"initContainers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(cr.Data.Raw, &patch); err != nil {
		return ""
	}
	containers := patch.Spec.Template.Spec.Containers
	if initContainer {
		containers = patch.Spec.Template.Spec.InitContainers
	}
	return containerImageByName(containers, container)
}

// labelRevisions sorts revs newest-revision-first and labels the top one
// "current" (this workload's live revision) and the rest "rollback
// target" — shared by Deployment's ReplicaSet-sourced revisions and
// StatefulSet/DaemonSet's ControllerRevision-sourced ones so both label
// identically.
func labelRevisions(revs []revisionCandidate, kind kube.ResourceKind) []imageHistoryEntry {
	slices.SortFunc(revs, func(a, b revisionCandidate) int { return cmp.Compare(b.n, a.n) })
	out := make([]imageHistoryEntry, 0, len(revs))
	for i, r := range revs {
		e := imageHistoryEntry{tag: tagOf(r.image), seenAt: r.created}
		if i == 0 {
			e.seenLabel = shortAge(time.Since(r.created)) + " · current"
			e.from = fmt.Sprintf("rev %d · this %s", r.n, strings.ToLower(string(kind)))
		} else {
			e.seenLabel = fmt.Sprintf("%s · rev %d", shortAge(time.Since(r.created)), r.n)
			e.from = "rollout history · rollback target"
		}
		out = append(out, e)
	}
	return out
}

// crossWorkloadHistory scans every Deployment/StatefulSet/DaemonSet cluster-
// wide (docs/design README.md §24a: "the same image tag seen on other
// workloads/namespaces ... the 'promote what prod runs' case") for a
// container whose image shares repo but carries a different tag than
// currentTag. seenAt is the sighted workload's own CreationTimestamp — a
// best-effort "seen" clock (the precise per-revision timestamp
// ownRevisionHistory resolves for the workload actually being edited would
// need one extra ReplicaSet lookup per sighted Deployment; this stays a
// single pass per kind).
//
// It reads only caches this session has already started (tui.KindCached):
// this is a loop over kinds the panel doesn't display, so reading the rest
// would start the StatefulSet and DaemonSet informers to edit a Deployment
// (all three from the CronJob list), and under --namespace-scoped start the
// cluster-wide caches that mode exists to avoid. When the cluster-wide cache
// isn't running, the edited workload's own namespace is read instead if
// that one is — so the edited kind, at least, always contributes.
func crossWorkloadHistory(ctx context.Context, lister resources.RawLister, kind kube.ResourceKind, namespace, name, repo, currentTag string) []imageHistoryEntry {
	if lister == nil {
		return nil
	}
	var out []imageHistoryEntry
	kinds := []kube.ResourceKind{kube.KindDeployment, kube.KindStatefulSet, kube.KindDaemonSet}
	if kind == kube.KindCronJob {
		// CronJob's own informer is already warm on this screen, so include
		// other schedules without making CronJob an extra read for apps views.
		kinds = append(kinds, kube.KindCronJob)
	}
	for _, k := range kinds {
		scope := ""
		if !tui.KindCached(lister, k, "") {
			if namespace == "" || !tui.KindCached(lister, k, namespace) {
				continue
			}
			scope = namespace
		}
		objs, err := lister.ListRaw(ctx, k, scope)
		if err != nil {
			continue
		}
		for _, obj := range objs {
			acc, err := apimeta.Accessor(obj)
			if err != nil {
				continue
			}
			if k == kind && acc.GetNamespace() == namespace && acc.GetName() == name {
				continue // the workload being edited itself
			}
			seenAt := acc.GetCreationTimestamp().Time
			for _, c := range setImageContainerInfos(obj) {
				if imageRepo(c.Image) != repo {
					continue
				}
				tag := tagOf(c.Image)
				if tag == currentTag {
					continue
				}
				out = append(out, imageHistoryEntry{
					tag:       tag,
					seenAt:    seenAt,
					seenLabel: shortAge(time.Since(seenAt)) + " ago",
					from:      fmt.Sprintf("%s/%s · %s", workloadArg(k), acc.GetName(), acc.GetNamespace()),
				})
			}
		}
	}
	return out
}

func containerImageByName(containers []corev1.Container, name string) string {
	for _, c := range containers {
		if c.Name == name {
			return c.Image
		}
	}
	return ""
}

// workloadArg renders kind as kubectl's short resource arg, the same
// deploy/sts/ds vocabulary kube.SetImageCommandString's "will run" line
// uses — duplicated here (browse can't import kube's unexported
// workloadResourceArg) per the repo's package-local-seam convention
// (execCmd/editCmd already duplicate across task packages the same way).
func workloadArg(kind kube.ResourceKind) string {
	switch kind {
	case kube.KindStatefulSet:
		return "sts"
	case kube.KindDaemonSet:
		return "ds"
	case kube.KindCronJob:
		return "cronjob"
	default:
		return "deploy"
	}
}

// imageRepo splits image into its repository name, excluding any tag or
// digest. A colon before the last slash is a registry port, not a tag.
func imageRepo(image string) string {
	name, _, _ := strings.Cut(image, "@")
	colon := strings.LastIndex(name, ":")
	if colon <= strings.LastIndex(name, "/") {
		return name
	}
	return name[:colon]
}

// tagOf splits image into its tag, defaulting to "latest" — Docker's own
// implicit default — when the ref carries neither a tag nor a digest. A
// digest-only ref ("repo@sha256:…") has no tag at all and returns "": it
// pins content, not "latest", and treating it as latest would offer
// "repo:latest" — a different image — as history.
func tagOf(image string) string {
	name, digest, hasDigest := strings.Cut(image, "@")
	repo := imageRepo(name)
	if len(repo) == len(name) {
		if hasDigest && digest != "" {
			return ""
		}
		return "latest"
	}
	return name[len(repo)+1:]
}

// shortAge renders a duration as a compact "12m"/"3h"/"5d" string — the same
// bucketing resources.shortAge uses (unexported there), duplicated here per
// the repo's package-local-seam convention.
func shortAge(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
