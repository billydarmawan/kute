// Package kube's Helm release support (18a, docs/design/README.md):
// releases are decoded straight from `sh.helm.release.v1` Secrets already
// sitting in the watched Secret cache — "browsing needs no helm binary".
// Only the rollback verb shells out to the real `helm` binary (see
// HelmRollback below). Helm itself stores each release revision as its own
// Secret (labelled owner=helm, name=<release>, version=<revision>), so a
// release "row" is really the highest-revision Secret for a given
// name/namespace — DecodeHelmReleases/LatestHelmReleases do that
// aggregation; HelmReleaseHistory keeps every revision for 18a's `h` rail.
package kube

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/kute-dev/kute/internal/semver"
)

// HelmReleaseSecretType is the Secret.Type Helm 3 stores every release
// revision under (helm.sh/helm/v3/pkg/storage/driver's own constant,
// reproduced here rather than pulling in the full Helm SDK for one string).
const HelmReleaseSecretType corev1.SecretType = "helm.sh/release.v1"

// HelmHookAnnotation is the annotation Helm stamps on a hook resource (e.g.
// a post-install/post-upgrade Job) naming which hook(s) it runs as —
// comma-separated when a resource serves more than one. §37a's SOURCE
// column reads this to render "helm/<release> <hook>" for a Job Helm itself
// created, distinct from a genuinely standalone one.
const HelmHookAnnotation = "helm.sh/hook"

// HelmReleaseNameAnnotation is the ownership annotation Helm 3 stamps on
// every resource it renders as part of a release — including hook
// resources — naming the release. §37a's SOURCE column reads it alongside
// HelmHookAnnotation for the "helm/<release> <hook>" label.
const HelmReleaseNameAnnotation = "meta.helm.sh/release-name"

// HelmRelease is one decoded release revision — either the release's
// current (highest-revision) state for the 18a list, or one entry in its
// full history (h).
type HelmRelease struct {
	Name         string
	Namespace    string
	Chart        string
	ChartVersion string
	AppVersion   string
	Revision     int
	// Status is Helm's own lowercase-hyphenated status word ("deployed",
	// "pending-upgrade", "failed", "superseded", "uninstalled", …) — the 18a
	// STATUS cell's base text.
	Status string
	// StatusReason is Info.Description, non-empty mainly for a failed
	// release ("hook timeout", "pre-upgrade hooks failed: ..."). 18a: "failed
	// carries the reason verbatim".
	StatusReason string
	Updated      time.Time
	// Values is the release's supplied values (Config) rendered as YAML —
	// what 18a's 'v' pushes into the read-only YAML viewer.
	Values   string
	Manifest string
	Notes    string
	// Hooks is Helm's saved hook ledger for this revision. It is distinct
	// from Manifest: Helm stores hook manifests separately, which is why a
	// release blocked in pre-install can have no ordinary live objects yet.
	Hooks []HelmHook

	// RolloutPending is 18a's "helm is done, Kubernetes isn't" signal: a
	// workload this release declares is still rolling out (or hasn't
	// settled). Helm's own Status goes back to "deployed" the moment the
	// manifests are applied — without --wait it never reports the rollout at
	// all — so a release genuinely mid-upgrade reads `deployed` and, before
	// this, rendered green while its pods were still being replaced.
	//
	// Like LatestVersion it is not decoded from the release Secret: answering
	// it means reading the workload caches, so it is annotated a layer up
	// (app's lister decorator, via resources.UnsettledWorkloads) where one
	// read serves the whole list. Zero — no annotation — is "nothing known to
	// be pending", the same read-only-degrades-to-quiet rule the rest of the
	// screen follows.
	RolloutPending bool

	// LatestVersion/LatestRepo/LatestAmbiguous are 18a's outdated signal:
	// the newest version of this chart in the user's *local* Helm repo cache
	// and where it came from. They are not decoded from the release Secret —
	// nothing in a release records its source repo — they are annotated a
	// layer up (app's lister decorator) from internal/helmrepo, and stay
	// zero whenever there's no repo cache to consult. Empty LatestVersion is
	// an unknown, never "this release is current".
	LatestVersion   string
	LatestRepo      string
	LatestAmbiguous bool
}

// WithLatest returns a copy of r carrying what a chart index said about its
// chart. Kept as a setter here — rather than kube reading the index itself —
// so this package stays free of any dependency on the local-disk Helm repo
// cache: the two callers that have an index (app's lister decorator and the
// history screen) look the chart up and hand the answer over.
func (r HelmRelease) WithLatest(version, repo string, ambiguous bool) HelmRelease {
	r.LatestVersion, r.LatestRepo, r.LatestAmbiguous = version, repo, ambiguous
	return r
}

// WithRollout returns a copy of r carrying whether its workloads have
// settled. Kept as a setter for the same reason as WithLatest: kube decodes
// the release, and the one caller that can see the workload caches
// (app's lister decorator) hands the answer over.
func (r HelmRelease) WithRollout(pending bool) HelmRelease {
	r.RolloutPending = pending
	return r
}

// Outdated reports whether the repo cache offers a newer version of this
// release's chart than the one deployed.
func (r HelmRelease) Outdated() bool {
	return r.LatestVersion != "" && semver.IsNewer(r.ChartVersion, r.LatestVersion)
}

// LatestCell renders 18a's LATEST column: the available version, marked when
// two repos serve this chart name and kute can't tell which one the release
// came from. An unknown chart (locally built, OCI, no repo cache) renders the
// em-dash placeholder every other unknown cell in the app uses.
func (r HelmRelease) LatestCell() string {
	if r.LatestVersion == "" {
		return "–"
	}
	if r.LatestAmbiguous {
		return r.LatestVersion + " ?"
	}
	return r.LatestVersion
}

// StatusCell renders 18a's STATUS column text: the bare status word, with
// Helm's description appended for pending and failed transactions.
func (r HelmRelease) StatusCell() string {
	if (r.Status == "failed" || strings.HasPrefix(r.Status, "pending-")) && r.StatusReason != "" {
		return r.Status + " · " + r.StatusReason
	}
	return r.Status
}

// HelmHook is one hook record embedded in a Helm release Secret.
type HelmHook struct {
	Name           string
	Kind           string
	Path           string
	Manifest       string
	Events         []string
	Weight         int
	DeletePolicies []string
	LastRun        HelmHookRun
}

// HelmHookRun is Helm's persisted view of the most recent hook execution.
// It may lag Kubernetes when the client disappears before committing the
// next release state, so diagnostic UIs must label it as Helm state.
type HelmHookRun struct {
	StartedAt   time.Time
	CompletedAt time.Time
	Phase       string
}

// HelmReleaseObject adapts a HelmRelease to runtime.Object so it can flow
// through the same resources.List/Project pipeline as every other kind
// (the same shape ForwardObject already established for a non-typed,
// app-synthesized kind — see forward.go). Unlike ForwardObject it also
// carries a real ObjectMeta (Name/Namespace mirrored from Release) so
// apimeta.Accessor works on it — a Helm release genuinely lives in a
// namespace, unlike a Forward session, so callers that generically
// namespace-filter a RawLister's results (kube/fake's own ListRaw, and any
// test double built the same way) need it to satisfy metav1.Object.
type HelmReleaseObject struct {
	metav1.TypeMeta
	metav1.ObjectMeta
	Release HelmRelease
}

// NewHelmReleaseObject builds a HelmReleaseObject with ObjectMeta populated
// from r — the one constructor every caller (helmAwareLister, test
// fixtures) should use rather than setting ObjectMeta by hand.
func NewHelmReleaseObject(r HelmRelease) *HelmReleaseObject {
	return &HelmReleaseObject{
		ObjectMeta: metav1.ObjectMeta{Name: r.Name, Namespace: r.Namespace},
		Release:    r,
	}
}

// DeepCopyObject satisfies runtime.Object, including the hook ledger's
// slice-backed fields.
func (o *HelmReleaseObject) DeepCopyObject() runtime.Object {
	cp := *o
	cp.Release.Hooks = make([]HelmHook, len(o.Release.Hooks))
	for i, hook := range o.Release.Hooks {
		cp.Release.Hooks[i] = hook
		cp.Release.Hooks[i].Events = append([]string(nil), hook.Events...)
		cp.Release.Hooks[i].DeletePolicies = append([]string(nil), hook.DeletePolicies...)
	}
	return &cp
}

// helmReleaseData mirrors just the fields kute reads off Helm 3's own
// release.Release JSON schema (helm.sh/helm/v3/pkg/release) — reproduced
// locally rather than importing the Helm SDK for a handful of fields.
type helmReleaseData struct {
	Name string `json:"name"`
	Info struct {
		Status        string    `json:"status"`
		Description   string    `json:"description"`
		LastDeployed  time.Time `json:"last_deployed"`
		FirstDeployed time.Time `json:"first_deployed"`
		Notes         string    `json:"notes"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Name       string `json:"name"`
			Version    string `json:"version"`
			AppVersion string `json:"appVersion"`
		} `json:"metadata"`
	} `json:"chart"`
	Config    map[string]any `json:"config"`
	Manifest  string         `json:"manifest"`
	Version   int            `json:"version"`
	Namespace string         `json:"namespace"`
	Hooks     []struct {
		Name           string   `json:"name"`
		Kind           string   `json:"kind"`
		Path           string   `json:"path"`
		Manifest       string   `json:"manifest"`
		Events         []string `json:"events"`
		Weight         int      `json:"weight"`
		DeletePolicies []string `json:"delete_policies"`
		LastRun        struct {
			StartedAt   time.Time `json:"started_at"`
			CompletedAt time.Time `json:"completed_at"`
			Phase       string    `json:"phase"`
		} `json:"last_run"`
	} `json:"hooks"`
}

// maxHelmReleasePayload caps how much a single release Secret is allowed to
// gunzip to.
//
// The apiserver caps a Secret's data at 1 MiB, but that bounds the
// *compressed* bytes: gzip's ratio runs to roughly 1000:1 on repetitive
// input, so a Secret well inside the API's limit can decompress to hundreds
// of megabytes. Nothing asks whether helm wrote a `helm.sh/release.v1`
// Secret before kute reads it — browsing a namespace is enough — and a TUI
// has nowhere to report an out-of-memory kill to. So the read is bounded and
// an over-large payload becomes an ordinary decode error, which
// DecodeHelmReleases already knows how to skip.
//
// 32 MiB is ~40× the largest compressed payload the API will store, so it is
// not a limit a real release can reach; a chart big enough to would have hit
// helm's own storage limit first.
const maxHelmReleasePayload = 32 << 20

// DecodeHelmReleaseSecret decodes one release revision Secret. Helm stores
// the release payload base64-encoded a *second* time (on top of the Secret
// API's own wire-format encoding, which client-go already reverses into
// secret.Data), then gzip-compressed, then JSON — this reverses exactly
// that: base64 decode, gunzip, unmarshal.
func DecodeHelmReleaseSecret(secret *corev1.Secret) (HelmRelease, error) {
	return decodeHelmReleaseSecret(secret, maxHelmReleasePayload)
}

// decodeHelmReleaseSecret is DecodeHelmReleaseSecret with the payload cap as
// a parameter, so a test can exercise the limit without building — or
// decompressing — 32 MiB to do it.
func decodeHelmReleaseSecret(secret *corev1.Secret, maxPayload int64) (HelmRelease, error) {
	if secret.Type != HelmReleaseSecretType {
		return HelmRelease{}, fmt.Errorf("secret %s/%s is not a %s release", secret.Namespace, secret.Name, HelmReleaseSecretType)
	}
	raw, ok := secret.Data["release"]
	if !ok {
		return HelmRelease{}, fmt.Errorf("secret %s/%s has no release data", secret.Namespace, secret.Name)
	}
	b64Decoded, err := base64.StdEncoding.DecodeString(string(raw))
	if err != nil {
		return HelmRelease{}, fmt.Errorf("base64-decode release: %w", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(b64Decoded))
	if err != nil {
		return HelmRelease{}, fmt.Errorf("gunzip release: %w", err)
	}
	defer func() { _ = gz.Close() }()
	// One byte past the cap, so hitting it is distinguishable from a payload
	// that merely ends there — silently truncating would hand json.Unmarshal
	// a cut-off document and report it as malformed JSON.
	jsonBytes, err := io.ReadAll(io.LimitReader(gz, maxPayload+1))
	if err != nil {
		return HelmRelease{}, fmt.Errorf("read release: %w", err)
	}
	if int64(len(jsonBytes)) > maxPayload {
		return HelmRelease{}, fmt.Errorf("release %s/%s decompresses past the %d-byte limit", secret.Namespace, secret.Name, maxPayload)
	}
	var data helmReleaseData
	if err := json.Unmarshal(jsonBytes, &data); err != nil {
		return HelmRelease{}, fmt.Errorf("unmarshal release: %w", err)
	}

	namespace := data.Namespace
	if namespace == "" {
		namespace = secret.Namespace
	}
	valuesYAML := ""
	if len(data.Config) > 0 {
		if y, err := sigsyaml.Marshal(data.Config); err == nil {
			valuesYAML = string(y)
		}
	}
	release := HelmRelease{
		Name:         data.Name,
		Namespace:    namespace,
		Chart:        data.Chart.Metadata.Name,
		ChartVersion: data.Chart.Metadata.Version,
		AppVersion:   data.Chart.Metadata.AppVersion,
		Revision:     data.Version,
		Status:       data.Info.Status,
		StatusReason: data.Info.Description,
		Updated:      data.Info.LastDeployed,
		Values:       valuesYAML,
		Manifest:     data.Manifest,
		Notes:        data.Info.Notes,
	}
	for _, hook := range data.Hooks {
		release.Hooks = append(release.Hooks, HelmHook{
			Name: hook.Name, Kind: hook.Kind, Path: hook.Path, Manifest: hook.Manifest,
			Events: append([]string(nil), hook.Events...), Weight: hook.Weight,
			DeletePolicies: append([]string(nil), hook.DeletePolicies...),
			LastRun:        HelmHookRun{StartedAt: hook.LastRun.StartedAt, CompletedAt: hook.LastRun.CompletedAt, Phase: hook.LastRun.Phase},
		})
	}
	return release, nil
}

// DecodeHelmReleases decodes every helm.sh/release.v1 Secret in objs,
// silently skipping anything that isn't one or fails to decode (the same
// best-effort tolerance projectForward/podsByName already apply to a
// partially-unexpected object list).
func DecodeHelmReleases(objs []runtime.Object) []HelmRelease {
	return decodeHelmReleases(objs, maxHelmReleasePayload)
}

// decodeHelmReleases is DecodeHelmReleases with the per-Secret payload cap
// as a parameter — see decodeHelmReleaseSecret.
func decodeHelmReleases(objs []runtime.Object, maxPayload int64) []HelmRelease {
	out := make([]HelmRelease, 0, len(objs))
	for _, obj := range objs {
		secret, ok := obj.(*corev1.Secret)
		if !ok || secret.Type != HelmReleaseSecretType {
			continue
		}
		r, err := decodeHelmReleaseSecret(secret, maxPayload)
		if err != nil {
			continue
		}
		out = append(out, r)
	}
	return out
}

// LatestHelmReleases collapses all to one row per namespace/name — the
// highest-revision entry, matching `helm list`'s own "current state of the
// release" semantics (18a's list shows one row per release, not one per
// revision).
func LatestHelmReleases(all []HelmRelease) []HelmRelease {
	latest := make(map[string]HelmRelease, len(all))
	for _, r := range all {
		key := r.Namespace + "/" + r.Name
		if cur, ok := latest[key]; !ok || r.Revision > cur.Revision {
			latest[key] = r
		}
	}
	out := slices.Collect(maps.Values(latest))
	slices.SortFunc(out, func(a, b HelmRelease) int {
		return cmp.Or(
			cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(a.Name, b.Name),
		)
	})
	return out
}

// DecodeLatestHelmReleases is LatestHelmReleases(DecodeHelmReleases(objs))
// without decoding the revisions it would throw away — the list's one row per
// release, at the cost of one decode per release rather than one per
// revision.
//
// Decoding is the expensive part (base64, gunzip of up to 32 MiB, JSON, then a
// YAML re-marshal of the values map), and helm keeps ten revisions per release
// by default, so decoding everything and then keeping the newest discards
// roughly nine tenths of the work. Helm labels every revision Secret with its
// release `name` and `version` (EncodeHelmReleaseSecret writes the same
// labels), so the newest revision per release is chosen from metadata first
// and only that one is decoded.
//
// The output matches the decode-everything path: a newest revision that fails
// to decode falls back to the next-highest one (exactly as
// DecodeHelmReleases' skip-on-error would have left it), and a Secret missing
// either label — or carrying a version that isn't a number — is decoded
// unconditionally and competes on its decoded revision as before.
func DecodeLatestHelmReleases(objs []runtime.Object) []HelmRelease {
	return decodeLatestHelmReleases(objs, DecodeHelmReleaseSecret)
}

// decodeLatestHelmReleases is DecodeLatestHelmReleases with the per-Secret
// decoder as a parameter, so a test can count how many Secrets it decodes.
func decodeLatestHelmReleases(objs []runtime.Object, decode func(*corev1.Secret) (HelmRelease, error)) []HelmRelease {
	type candidate struct {
		index   int
		version int
		secret  *corev1.Secret
	}
	groups := map[string][]candidate{}
	var order []string // group keys in first-seen order, for determinism
	var unlabelled []candidate
	for i, obj := range objs {
		secret, ok := obj.(*corev1.Secret)
		if !ok || secret.Type != HelmReleaseSecretType {
			continue
		}
		name := secret.Labels["name"]
		version, err := strconv.Atoi(secret.Labels["version"])
		if name == "" || err != nil {
			unlabelled = append(unlabelled, candidate{index: i, secret: secret})
			continue
		}
		key := secret.Namespace + "\x00" + name
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], candidate{index: i, version: version, secret: secret})
	}

	type decoded struct {
		index   int
		release HelmRelease
	}
	out := make([]decoded, 0, len(order)+len(unlabelled))
	for _, key := range order {
		cands := groups[key]
		// Newest first; a stable sort keeps input order among equal versions,
		// which is the tie-break LatestHelmReleases' strict `>` applies.
		slices.SortStableFunc(cands, func(a, b candidate) int { return cmp.Compare(b.version, a.version) })
		for _, c := range cands {
			if r, err := decode(c.secret); err == nil {
				out = append(out, decoded{index: c.index, release: r})
				break
			}
		}
	}
	for _, c := range unlabelled {
		if r, err := decode(c.secret); err == nil {
			out = append(out, decoded{index: c.index, release: r})
		}
	}
	// Back into input order before the final collapse, so any tie it has to
	// break (an unlabelled Secret matching a labelled one's revision) breaks
	// the same way the decode-everything path did.
	slices.SortFunc(out, func(a, b decoded) int { return cmp.Compare(a.index, b.index) })
	releases := make([]HelmRelease, len(out))
	for i, d := range out {
		releases[i] = d.release
	}
	return LatestHelmReleases(releases)
}

// HelmReleaseSecretsFor narrows release Secrets to one release's own
// revisions, before anything decodes them.
//
// The release cache is cluster-wide, and decoding is not cheap — base64,
// gunzip, JSON, then a YAML re-marshal of the values map, per revision of
// every release in every namespace. 18a's history screen wants one release,
// and it re-reads on every Secret change event and on every cache-sync
// retry tick, so doing that filtering after the decode (HelmReleaseHistory
// alone) throws away nearly all of the work it just paid for.
//
// Helm labels each revision Secret with the release name; the object-name
// fallback covers a Secret written without them.
func HelmReleaseSecretsFor(objs []runtime.Object, namespace, name string) []runtime.Object {
	prefix := fmt.Sprintf("sh.helm.release.v1.%s.v", name)
	out := make([]runtime.Object, 0, len(objs))
	for _, obj := range objs {
		secret, ok := obj.(*corev1.Secret)
		if !ok || secret.Type != HelmReleaseSecretType {
			continue
		}
		if namespace != "" && secret.Namespace != namespace {
			continue
		}
		if release, labelled := secret.Labels["name"]; labelled {
			if release != name {
				continue
			}
		} else if !strings.HasPrefix(secret.Name, prefix) {
			continue
		}
		out = append(out, obj)
	}
	return out
}

// HelmReleaseHistory filters all to one release's revisions, newest first —
// 18a's `h` revision rail.
func HelmReleaseHistory(all []HelmRelease, namespace, name string) []HelmRelease {
	var out []HelmRelease
	for _, r := range all {
		if r.Namespace == namespace && r.Name == name {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b HelmRelease) int { return cmp.Compare(b.Revision, a.Revision) })
	return out
}

// helmRollbackArgs builds the `helm rollback` argv shared by HelmRollback
// and HelmRollbackCommandString, so 18a's "will run" line can't drift from
// what runs. toRevision 0 means "the previous revision" (plain
// `helm rollback <name>`, Helm's own default).
func helmRollbackArgs(target CommandTarget, namespace, name string, toRevision int) []string {
	args := []string{"rollback", name}
	if toRevision > 0 {
		args = append(args, strconv.Itoa(toRevision))
	}
	return helmArgv(target, append(args, "-n", namespace)...)
}

// HelmRollbackCommandString renders the exact `helm` invocation HelmRollback
// runs — 18a's "will run" documentation line (the same copyable-command
// idiom as 10a/13a/17b).
func HelmRollbackCommandString(target CommandTarget, namespace, name string, toRevision int) string {
	return commandString(append([]string{"helm"}, helmRollbackArgs(target, namespace, name, toRevision)...))
}

// HelmRollbackCommandSummary is HelmRollbackCommandString for a line too
// narrow to hold it: the --kubeconfig path — the one flag whose length the
// user doesn't control and that can run to a hundred columns — is elided
// behind a trailing "…", while the release, revision, namespace and the
// --kube-context that pins which cluster it lands on all stay readable. With
// no --kubeconfig there is nothing to elide and it is the full command.
func HelmRollbackCommandSummary(target CommandTarget, namespace, name string, toRevision int) string {
	if target.Kubeconfig == "" {
		return HelmRollbackCommandString(target, namespace, name, toRevision)
	}
	return HelmRollbackCommandString(CommandTarget{Context: target.Context}, namespace, name, toRevision) + " …"
}

// HelmRollback shells out to the real `helm` binary — the one Helm verb
// that isn't decoded from the watch cache (18a: "browsing needs no helm
// binary" but rollback does). target pins helm to the cluster kute is
// connected to rather than the kubeconfig's current-context (see
// CommandTarget). Returns a clear, inline-explainable error when helm isn't
// on PATH rather than a raw exec.ErrNotFound.
func HelmRollback(ctx context.Context, target CommandTarget, namespace, name string, toRevision int) error {
	if _, err := exec.LookPath("helm"); err != nil {
		return fmt.Errorf("helm not found in PATH — install helm to roll back releases")
	}
	cmd := exec.CommandContext(ctx, "helm", helmRollbackArgs(target, namespace, name, toRevision)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("helm rollback failed: %s", msg)
	}
	return nil
}

// EncodeHelmReleaseSecret builds the Secret Helm itself would store for r —
// the inverse of DecodeHelmReleaseSecret. Used by kube/fake's demo fixtures
// (fixtures.go) and by fake.Cluster.HelmRollback to synthesize the new
// revision a real `helm rollback` would create, without needing a real Helm
// SDK dependency.
func EncodeHelmReleaseSecret(r HelmRelease) *corev1.Secret {
	data := helmReleaseData{
		Name:      r.Name,
		Namespace: r.Namespace,
		Version:   r.Revision,
		Manifest:  r.Manifest,
	}
	data.Info.Status = r.Status
	data.Info.Description = r.StatusReason
	data.Info.LastDeployed = r.Updated
	data.Info.FirstDeployed = r.Updated
	data.Info.Notes = r.Notes
	data.Chart.Metadata.Name = r.Chart
	data.Chart.Metadata.Version = r.ChartVersion
	data.Chart.Metadata.AppVersion = r.AppVersion
	for _, hook := range r.Hooks {
		var encoded struct {
			Name           string   `json:"name"`
			Kind           string   `json:"kind"`
			Path           string   `json:"path"`
			Manifest       string   `json:"manifest"`
			Events         []string `json:"events"`
			Weight         int      `json:"weight"`
			DeletePolicies []string `json:"delete_policies"`
			LastRun        struct {
				StartedAt   time.Time `json:"started_at"`
				CompletedAt time.Time `json:"completed_at"`
				Phase       string    `json:"phase"`
			} `json:"last_run"`
		}
		encoded.Name, encoded.Kind, encoded.Path, encoded.Manifest = hook.Name, hook.Kind, hook.Path, hook.Manifest
		encoded.Events, encoded.Weight, encoded.DeletePolicies = append([]string(nil), hook.Events...), hook.Weight, append([]string(nil), hook.DeletePolicies...)
		encoded.LastRun.StartedAt, encoded.LastRun.CompletedAt, encoded.LastRun.Phase = hook.LastRun.StartedAt, hook.LastRun.CompletedAt, hook.LastRun.Phase
		data.Hooks = append(data.Hooks, encoded)
	}
	if r.Values != "" {
		cfg := map[string]any{}
		if err := sigsyaml.Unmarshal([]byte(r.Values), &cfg); err == nil {
			data.Config = cfg
		}
	}

	jsonBytes, _ := json.Marshal(data)
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	_, _ = gz.Write(jsonBytes)
	_ = gz.Close()
	encoded := base64.StdEncoding.EncodeToString(gzBuf.Bytes())

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("sh.helm.release.v1.%s.v%d", r.Name, r.Revision),
			Namespace: r.Namespace,
			Labels: map[string]string{
				"owner":   "helm",
				"name":    r.Name,
				"status":  r.Status,
				"version": strconv.Itoa(r.Revision),
			},
			CreationTimestamp: metav1.NewTime(r.Updated),
		},
		Type: HelmReleaseSecretType,
		Data: map[string][]byte{"release": []byte(encoded)},
	}
}

// helmReleaseFieldSelector narrows a Secret list/watch to Helm's own release
// storage. Secret.type is a server-supported field selector, so this filters
// at the API server rather than after the fact.
var helmReleaseFieldSelector = fields.OneTermEqualSelector("type", string(HelmReleaseSecretType)).String()

// ensureHelmSecrets idempotently starts the informer behind
// ListHelmReleaseSecrets for one namespace ("" for all): a Secret informer of
// its own, filtered server-side to type=helm.sh/release.v1.
//
// It is deliberately not the shared Secret cache. Releases are a small,
// distinctively-typed slice of a namespace's Secrets, but the shared cache
// holds all of them — image-pull credentials, TLS material, every
// ServiceAccount token — which on a real cluster is the single largest kind
// there is. Listing Helm releases used to pull all of it (12 MB on the
// cluster this was measured against) to find a few dozen release Secrets.
//
// The two caches coexist: opening the Secrets list still populates the shared
// one, and that is the right cost for a screen that is actually about
// Secrets. This one only ever holds releases.
//
// It is also scoped to the namespace being read, one informer per namespace,
// rather than one cluster-wide cache serving every read. The type filter
// narrows the *kind* of Secret but not the *scope*, and release Secrets carry
// the release's whole gzipped manifest, so a cluster-wide list is far larger
// than the namespaced one that answers the screen actually open: 8.19 MB
// against 4 MB on the cluster this was measured against. On a link where the
// bigger read can't finish inside the API server's 60s request window it
// never completes at all — the reflector retries the same doomed LIST
// forever, KindSynced never flips, and the Helm list spins indefinitely while
// `helm list -n <ns>` answers in seconds. All-namespaces mode (namespace "")
// still reads cluster-wide: you asked for every namespace, same as the
// Secrets list itself.
func (c *Cluster) ensureHelmSecrets(namespace string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopCh == nil {
		return
	}
	if c.helmInformers[namespace] != nil {
		return
	}
	factory := informers.NewSharedInformerFactoryWithOptions(
		c.clientset, defaultResync,
		informers.WithNamespace(namespace),
		informers.WithTransform(stripManagedFields),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = helmReleaseFieldSelector
		}),
	)
	informer := factory.Core().V1().Secrets().Informer()
	gen := c.generation
	if c.watcher != nil {
		c.watcher.register(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, namespace, helmReleaseFieldSelector, func() {
			c.recordWatchEstablished(gen, KindHelmRelease, namespace)
		}, func(err error) {
			c.recordWatchEnded(gen, KindHelmRelease, namespace, err)
		})
	}
	// Best-effort: a failed registration just means no health signal here.
	// Runs later, on the reflector's goroutine — so it takes the lock
	// itself rather than assuming the one held here. recordWatchError
	// re-checks gen atomically with both the state write and the
	// health.onWatchError call — see its doc comment.
	_ = informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		c.recordWatchError(gen, KindHelmRelease, namespace, err)
	})
	// Handler registration errors are non-fatal for a read-only UI.
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.notifyScope(gen, KindHelmRelease, namespace) },
		UpdateFunc: func(any, any) { c.notifyScope(gen, KindHelmRelease, namespace) },
		DeleteFunc: func(any) { c.notifyScope(gen, KindHelmRelease, namespace) },
	})
	if c.helmFactories == nil {
		c.helmFactories = map[string]informers.SharedInformerFactory{}
		c.helmInformers = map[string]cache.SharedIndexInformer{}
	}
	c.helmFactories[namespace] = factory
	c.helmInformers[namespace] = informer
	c.noteInformerStartedLocked()
	factory.Start(c.stopCh)
	c.health.noteListBurst()
}

// ListHelmReleaseSecrets returns the helm.sh/release.v1 Secrets in namespace
// ("" for all), from a cache that holds only those. Callers decode them with
// DecodeHelmReleases; this deliberately returns Secrets rather than releases
// so the decode stays where it already lives.
func (c *Cluster) ListHelmReleaseSecrets(ctx context.Context, namespace string) ([]runtime.Object, error) {
	c.ensureHelmSecrets(namespace)
	c.mu.Lock()
	f := c.helmFactories[namespace]
	c.mu.Unlock()
	if f == nil {
		// No cluster to read from (a stopped Cluster); an empty result plus
		// KindSynced reporting settled is how every other read degrades.
		return nil, nil
	}
	// The factory is already scoped to namespace, so its cluster-wide List is
	// this namespace's releases — but the namespaced path stays for the
	// all-namespaces cache, which really does hold every namespace's.
	lister := f.Core().V1().Secrets().Lister()
	return listNamespaced(lister.List, lister.Secrets, namespace, labels.Everything())
}
