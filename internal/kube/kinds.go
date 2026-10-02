package kube

import "strings"

// ResourceKind identifies a Kubernetes resource type the UI can list. The string
// value is the API Kind, used for display and as the registry key.
//
// For almost every kind the two are the same string. The exceptions are in
// substitutedKinds below: a discovered kind whose API Kind is already taken
// in resources.Registry gets a different registry key, because the registry
// is keyed on ResourceKind alone and Register is last-write-wins. Anywhere
// the kind has to travel back out to the API server or to kubectl, use
// APIKind or ResourceArg rather than string(k) — see their doc comments for
// what breaks otherwise.
type ResourceKind string

// substituted describes a registry kind whose key is deliberately not its
// API Kind.
type substituted struct {
	apiKind  string // metadata.kind on the wire, and Event.involvedObject.kind
	group    string // the API group that disambiguates it from the collision
	resource string // "<plural>.<group>", the unambiguous kubectl resource arg
}

// substitutedKinds is the hand-kept substitution table. Deliberately tiny: a
// substitution costs the invariant that a ResourceKind is its API Kind, so
// it is only worth paying where a curated kind needs a stable const of its
// own. Every other collision — a CRD sharing a built-in's Kind, or two
// groups sharing one — is resolved at discovery with a self-describing
// QualifiedKind key (DiscoveredKind.RegistryKind, AssignRegistryKinds), no
// table entry needed.
var substitutedKinds = map[ResourceKind]substituted{
	KindFluxHelmRelease: {
		apiKind:  "HelmRelease",
		group:    FluxGroupHelm,
		resource: "helmreleases." + FluxGroupHelm,
	},
}

// Flux API groups (docs/design README.md §30a). Enumerated rather than
// matched on a ".toolkit.fluxcd.io" suffix: "kute never guesses", and a
// future Flux group with different status semantics must not silently
// inherit §30a's descriptor.
const (
	FluxGroupSource       = "source.toolkit.fluxcd.io"
	FluxGroupKustomize    = "kustomize.toolkit.fluxcd.io"
	FluxGroupHelm         = "helm.toolkit.fluxcd.io"
	FluxGroupNotification = "notification.toolkit.fluxcd.io"
	FluxGroupImage        = "image.toolkit.fluxcd.io"
)

// FluxReconcileAnnotation is the annotation every Flux controller watches
// for an out-of-band "reconcile now" request — §30a's 'r' stamps it with a
// fresh RFC3339 timestamp, the same thing `flux reconcile` does and the
// same annotate-to-trigger mechanism RolloutRestart already uses.
const FluxReconcileAnnotation = "reconcile.fluxcd.io/requestedAt"

// IsFluxGroup reports whether group is one of Flux's own API groups, and so
// whether a kind discovered in it gets §30a's curated descriptor.
//
// Recognition is by group, never by bare Kind name: Flux's HelmRelease
// shares its Kind with §18a's Helm-3 release kind, and matching on the name
// is precisely the bug §30a exists to prevent.
func IsFluxGroup(group string) bool {
	switch group {
	case FluxGroupSource, FluxGroupKustomize, FluxGroupHelm, FluxGroupNotification, FluxGroupImage:
		return true
	}
	return false
}

// ArgoGroup is Argo CD's one API group (docs/design README.md §33a). A
// single constant rather than a switch of one: unlike Flux's five groups,
// Argo CD ships as one CRD family, so there's nothing else to enumerate.
const ArgoGroup = "argoproj.io"

// ArgoRefreshAnnotation is the annotation argocd-application-controller
// watches for an out-of-band "refresh now" request — §33a's 'r' stamps it
// "normal", the same value `argocd app get --refresh` sends and the same
// annotate-to-trigger mechanism FluxReconcileAnnotation uses on a Flux
// object.
const ArgoRefreshAnnotation = "argocd.argoproj.io/refresh"

// IsArgoGroup reports whether group is Argo CD's own API group, and so
// whether a kind discovered in it is a candidate for §33a's curated
// descriptor. Candidate, not automatic: BuildDiscoveredRegistry also checks
// the Kind, since argoproj.io serves AppProject alongside Application and
// only Application carries sync/health status — the same group-plus-Kind
// gate §23b's httpRouteDescriptor uses within Gateway API's shared group.
func IsArgoGroup(group string) bool {
	return group == ArgoGroup
}

// Gateway API's and cert-manager's API groups — the groups that own the
// bare Kind names kute routes to bespoke screens (KindGateway/KindHTTPRoute
// → tasks/routetable, KindCertificate → tasks/certchain). See ownedKinds.
const (
	GatewayAPIGroup  = "gateway.networking.k8s.io"
	CertManagerGroup = "cert-manager.io"
	CertManagerACME  = "acme.cert-manager.io"
)

// ownedKinds names, for each bare Kind kute itself routes on, the one API
// group allowed to hold that bare registry key. Istio's networking.istio.io
// Gateway and ACK ACM's Certificate are real, common CRDs; under their bare
// Kind they would either hide the kind kute actually means or be routed into
// a screen that misreads them. Any other group serving one of these Kinds is
// registered under a qualified key instead (QualifiedKind).
var ownedKinds = map[string]string{
	string(KindGateway):            GatewayAPIGroup,
	string(KindHTTPRoute):          GatewayAPIGroup,
	string(KindGRPCRoute):          GatewayAPIGroup,
	string(KindTCPRoute):           GatewayAPIGroup,
	string(KindCertificate):        CertManagerGroup,
	string(KindCertificateRequest): CertManagerGroup,
	string(KindIssuer):             CertManagerGroup,
	string(KindClusterIssuer):      CertManagerGroup,
	string(KindOrder):              CertManagerACME,
	string(KindChallenge):          CertManagerACME,
	"Application":                  ArgoGroup,
}

// IsBuiltinKind reports whether k is a registry key kute defines itself —
// every typed-informer kind plus the synthetic ones (Forward, Helm releases,
// the CRDs list, the goto carve-outs). A discovered CRD whose Kind is one of
// these never gets the bare key: resources.Registry is keyed on ResourceKind
// alone and Register is last-write-wins, so a Knative Service registered as
// "Service" replaced the core Services list outright.
func IsBuiltinKind(k ResourceKind) bool {
	if _, ok := typedKinds[k]; ok {
		return true
	}
	switch k {
	case KindForward, KindCustomResourceDefinition, KindHelmRelease, KindFluxHelmRelease,
		KindWhoCan, KindOverview, KindFluxTree:
		return true
	}
	return false
}

// QualifiedKind is the registry key a discovered kind gets when its bare
// Kind is already taken: "<Kind>.<group>", e.g. "Service.serving.knative.dev".
// The key is self-describing, so APIKind and ResourceArg recover the wire
// Kind and a group-qualified kubectl argument from it with no table lookup —
// the substitution is data computed at discovery, not a hand-kept list.
func QualifiedKind(kind, group string) ResourceKind {
	return ResourceKind(kind + "." + group)
}

// APIKind is the Kubernetes Kind behind k. Identical to string(k) for every
// kind but a substituted one.
//
// Anything comparing against a field the API server populated —
// Event.involvedObject.kind, an ownerReference's Kind — must go through
// this. Comparing string(k) instead silently matches nothing, which reads
// as "this object has no events" rather than as a bug.
func (k ResourceKind) APIKind() string {
	if s, ok := substitutedKinds[k]; ok {
		return s.apiKind
	}
	// A qualified key (QualifiedKind): a Kind never contains a dot, so the
	// first one ends it.
	if i := strings.IndexByte(string(k), '.'); i > 0 {
		return string(k)[:i]
	}
	return string(k)
}

// ResourceArg renders k as a kubectl resource argument: the lowercased Kind
// for everything normal, the fully-qualified "<plural>.<group>" for a
// substituted kind, where the bare name would either fail to resolve or
// resolve to the wrong resource. A qualified key (QualifiedKind) lowercases
// to "<kind>.<group>", which kubectl resolves through the discovery mapper's
// singular name for exactly that group. Used by every command string kute prints
// as copyable documentation, and by the kubectl edit handoff.
func (k ResourceKind) ResourceArg() string {
	if s, ok := substitutedKinds[k]; ok {
		return s.resource
	}
	return strings.ToLower(string(k))
}

const (
	KindPod                   ResourceKind = "Pod"
	KindDeployment            ResourceKind = "Deployment"
	KindDaemonSet             ResourceKind = "DaemonSet"
	KindStatefulSet           ResourceKind = "StatefulSet"
	KindReplicaSet            ResourceKind = "ReplicaSet"
	KindJob                   ResourceKind = "Job"
	KindCronJob               ResourceKind = "CronJob"
	KindService               ResourceKind = "Service"
	KindIngress               ResourceKind = "Ingress"
	KindConfigMap             ResourceKind = "ConfigMap"
	KindSecret                ResourceKind = "Secret"
	KindPersistentVolumeClaim ResourceKind = "PersistentVolumeClaim"
	KindNode                  ResourceKind = "Node"
	KindNamespace             ResourceKind = "Namespace"
	KindEvent                 ResourceKind = "Event"
	// KindForward is a synthetic kind: not a Kubernetes API type, but a
	// registry entry for the app's own in-process port-forward sessions
	// (docs/design README.md §13c) — ForwardManager.ListRaw feeds it
	// through the same resources.List/Project pipeline as every real kind.
	KindForward ResourceKind = "Forward"
	// KindCustomResourceDefinition is the 14b CRDs list — a built-in kind
	// (like Forward) backed by a dynamic informer over
	// apiextensions.k8s.io/v1 rather than a typed one (discovery.go/
	// dynamic.go). Discovered custom kinds (14a) are registered under their
	// own ResourceKind(dk.Kind), not this one.
	KindCustomResourceDefinition ResourceKind = "CustomResourceDefinition"

	// KindHelmRelease is a synthetic kind, like Forward: not its own
	// Kubernetes API type, but a registry entry decoded from
	// helm.sh/release.v1 Secrets already sitting in the watched Secret cache
	// (docs/design README.md §18a, kube/helm.go). One row per release
	// (namespace+name), aggregated to its highest revision.
	KindHelmRelease ResourceKind = "HelmRelease"

	// KindFluxHelmRelease is the discovered helm.toolkit.fluxcd.io
	// HelmRelease (docs/design README.md §30a) under a registry key that is
	// deliberately NOT its API Kind.
	//
	// resources.Registry is keyed on ResourceKind alone — not on (group,
	// kind) — and §18a's synthetic KindHelmRelease (Helm-3 release Secrets)
	// got there first, so registering Flux's under its bare Kind silently
	// replaced it: the Helm list rendered NAME+AGE only, and Flux's own
	// HelmReleases never appeared at all. Substituting the key rather than
	// re-keying the whole registry keeps the change to one table; the cost
	// is that this key is no longer the API Kind, which ResourceKind's
	// APIKind/ResourceArg exist to pay. See substitutedKinds.
	//
	// It is the only Flux kind that needs a const. Every other one is
	// addressed by its own Kind and reached through Descriptor.Flux — browse
	// never names an individual Flux kind.
	KindFluxHelmRelease ResourceKind = "FluxHelmRelease"

	// Gateway API kinds (gateway.networking.k8s.io) are never DefaultRegistry
	// entries — like every CRD they only exist in the registry once
	// discovered (docs/design README.md §23b, CLAUDE.md's "CRD support is
	// data, not code"). These consts exist purely so browse can name them
	// when routing ↵ to the bespoke routing table (tasks/routetable) instead
	// of the generic 14d object detail — the same kind-name carve-out
	// Pod/Node/Deployment's own bespoke ↵ routing already takes.
	KindHTTPRoute ResourceKind = "HTTPRoute"
	KindGRPCRoute ResourceKind = "GRPCRoute"
	KindTCPRoute  ResourceKind = "TCPRoute"
	KindGateway   ResourceKind = "Gateway"

	// RBAC kinds (rbac.authorization.k8s.io/v1) back tasks/whocan's (22a)
	// cache-only binding resolution and the "↵ opens the binding's YAML"
	// verb — they're real informer-backed kinds like Pod/Node, just never
	// registered in resources.Registry (they have no browse list; whocan
	// is a query, not a browser).
	KindRole               ResourceKind = "Role"
	KindRoleBinding        ResourceKind = "RoleBinding"
	KindClusterRole        ResourceKind = "ClusterRole"
	KindClusterRoleBinding ResourceKind = "ClusterRoleBinding"

	// KindHorizontalPodAutoscaler (autoscaling/v2) backs 17b's scale
	// prompt's HPA-managed-workload detection (docs/design README.md
	// §17b: "HPA-managed workloads show managed by hpa/<name> —
	// scaling overridden on next sync") — a real informer-backed kind
	// like Pod/Node, just never registered in resources.Registry (no
	// browse list needed for this feature), same shape as the RBAC kinds
	// above.
	KindHorizontalPodAutoscaler ResourceKind = "HorizontalPodAutoscaler"

	// KindControllerRevision (apps/v1) backs 24a's set-image history table
	// for StatefulSet/DaemonSet — the same rollout-history mechanism
	// `kubectl rollout history statefulset|daemonset` reads, since neither
	// controller owns ReplicaSets the way Deployment does. A real
	// informer-backed kind, never registered in resources.Registry (no
	// browse list needed), same shape as the RBAC/HPA kinds above.
	KindControllerRevision ResourceKind = "ControllerRevision"

	// KindWhoCan is a synthetic kind, like KindForward: not a Kubernetes API
	// type and never registered in resources.Registry (there is nothing to
	// list — 22a is "a query, not a browser"). It exists purely so the goto
	// corpus (tui/goto.go) can surface a "who-can" jump result and browse's
	// switchKind (tui/tasks/browse/update.go) can recognize it as a
	// carve-out that pushes tasks/whocan directly, the same kind-name
	// special-case Ingress/Gateway routing already takes for
	// tasks/routetable.
	KindWhoCan ResourceKind = "WhoCan"

	// KindOverview is a synthetic kind, like KindWhoCan: not a Kubernetes API
	// type and never registered in resources.Registry — 19a is "a routing
	// layer, not a dashboard", so there's nothing to list. It exists purely
	// so the goto corpus can surface a `g "ov"` jump result and browse's
	// switchKind can recognize it as a carve-out that pushes
	// tasks/overview directly, the same kind-name special-case KindWhoCan
	// already takes.
	KindOverview ResourceKind = "Overview"

	// KindFluxTree is a synthetic kind, like KindOverview: not a Kubernetes
	// API type and never registered in resources.Registry. §30b is a
	// *computed join* over the Flux kinds that are already registered, so it
	// has nothing of its own to list. It exists so the goto corpus can
	// surface a `g "flux"` result — only on a cluster that actually serves
	// Flux CRDs, per §30a's zero-chrome-otherwise rule — and browse's
	// switchKind can recognize it as a carve-out that pushes tasks/fluxtree.
	KindFluxTree ResourceKind = "FluxTree"

	// cert-manager kinds (docs/design README.md §35a) are, like Gateway
	// API's, never DefaultRegistry entries — ordinary discovered/generic CRD
	// kinds (§14a/§14d) with no curated Descriptor. These consts exist for
	// two narrow reasons, neither of which is registry curation: browse
	// needs to name Certificate for its bespoke ↵ carve-out into
	// tasks/certchain (the same kind-name carve-out Gateway API's own
	// KindHTTPRoute takes), and tasks/certchain needs to name every kind in
	// the Certificate → CertificateRequest → Order → Challenge ownerRef
	// chain, plus Issuer/ClusterIssuer for the refs strip, without scattering
	// string literals through its walk. Order and Challenge actually live in
	// the sibling acme.cert-manager.io API group, not cert-manager.io — an
	// accident of the real CRDs' packaging that doesn't matter here, since
	// nothing below gates on API group.
	KindCertificate        ResourceKind = "Certificate"
	KindCertificateRequest ResourceKind = "CertificateRequest"
	KindOrder              ResourceKind = "Order"
	KindChallenge          ResourceKind = "Challenge"
	KindIssuer             ResourceKind = "Issuer"
	KindClusterIssuer      ResourceKind = "ClusterIssuer"
)

// ResourceChangedMsg is emitted (as a tea.Msg) when a watched informer observes
// an add/update/delete for a kind, so screens can re-read from the cache. It is
// a plain struct so the kube package needs no dependency on Bubble Tea.
type ResourceChangedMsg struct {
	Kind ResourceKind
}

// CRDsDiscoveredMsg fires once after the initial (detached, out-of-band)
// cluster.Start goroutine in app.RunWithConfig completes — the one connect
// path where CRD discovery finishes outside any tea.Cmd the root shell can
// await synchronously (context switch and reconnect both already run
// discovery inline before their own result message is returned). The root
// shell handles it by rebuilding Session.Registry/Groups from
// Cluster.DiscoveredKinds().
type CRDsDiscoveredMsg struct{}
