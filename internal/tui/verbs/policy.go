package verbs

import (
	"slices"

	"github.com/kute-dev/kute/internal/kube"
)

// Tier classifies how a mutating verb is confirmed before it executes
// (mvp-plan.md §0.4, §8b). It lives here, beside the policy table that picks
// it, so the registry, the table and actions.Controller share one type
// without an import cycle: actions imports verbs, never the reverse.
type Tier int

const (
	// TierNone executes immediately, no confirmation (cordon, scale —
	// reversible verbs, or verbs whose own staged preview is the confirm).
	TierNone Tier = iota
	// TierInline shows a y/N prompt in the keybar.
	TierInline
	// TierModal shows a centered modal: the type-the-name modal (a marked
	// set: type-the-count) when Policy.TypedName, else a plain y/N card.
	TierModal
)

// Policy is the whole confirm decision for one action: how much friction
// (Tier) and, for TierModal, whether the user must type the target's name
// (or a marked set's count) rather than answer a y/N card.
type Policy struct {
	Tier      Tier
	TypedName bool
}

// Target is what Resolve needs to know about the object a verb acts on,
// beyond the verb itself and the context's PROD tag.
type Target struct {
	// Kind is the target's registry kind — the delete family escalates
	// cascadeDeleteKinds.
	Kind kube.ResourceKind
	// Reverse is the verb's undo direction (Scope.Verb == Verb.ReverseID):
	// resume, uncordon.
	Reverse bool
	// Remove is a key removal through an editor verb (26a's meta panel).
	Remove bool
	// Overwrite is an add that replaces an existing value the merge patch
	// can't tell from a new key (27a/27b's 'a' on a key that's already there).
	Overwrite bool
	// Joined is a label edit that changes a Service's selector join, or
	// whose join can't be known yet (26a).
	Joined bool
}

// rule is one verb's row in the policy table.
type rule struct {
	tier  Tier // outside PROD
	prod  Tier // in a PROD context (config.yaml's prodContexts)
	typed bool // a TierModal confirm types the name, not a y/N card
	// cascade: a cascadeDeleteKinds target is always TierModal.
	cascade bool
	// hasReverse: Target.Reverse resolves to reverse instead, PROD or not.
	hasReverse bool
	reverse    Tier
	// Floors raised to (never lowered) by a Target fact, PROD or not.
	onRemove, onOverwrite, onJoined Tier
}

// cascadeDeleteKinds are the kinds whose delete removes far more than the
// row: a Namespace takes every object inside it, a CustomResourceDefinition
// every instance of its kind, and a Node every pod bound to it (PodGC
// deletes them, DaemonSet pods included, and the node is gone until its
// kubelet re-registers — strictly more disruptive than Drain, which is
// already always modal). Deleting one always gets the modal — the
// type-the-name modal for one row, type-the-count for a marked set —
// PROD or not (docs/design README.md §8b/§11a/§14b/§20a).
var cascadeDeleteKinds = []kube.ResourceKind{kube.KindNamespace, kube.KindCustomResourceDefinition, kube.KindNode}

// policies is the destructive-action policy, in one place (CLAUDE.md's
// destructive-action invariant; review 10 M1). Every confirm in the app —
// actions.Controller.Begin, and the three bespoke gates outside it (the
// kubectl edit/debug tty handoffs and 20a's marked-set delete) — resolves
// through Resolve, never through a tier picked at the call site.
var policies = map[string]rule{
	// Delete: inline y/N, the type-the-name modal in PROD — and always the
	// modal for a Namespace/CRD/Node (cascadeDeleteKinds).
	Delete.ID: {tier: TierInline, prod: TierModal, typed: true, cascade: true},
	// Force delete (grace period 0) is always the type-the-name modal.
	ForceDelete.ID: {tier: TierModal, prod: TierModal, typed: true},
	// The rest of the "destroys or replaces something" family: inline y/N,
	// type-the-name modal in PROD (§9a, §16b, §18a "Rollback inherits 8b
	// friction", §37c).
	RolloutRestart.ID: {tier: TierInline, prod: TierModal, typed: true},
	RolloutUndo.ID:    {tier: TierInline, prod: TierModal, typed: true},
	Rollback.ID:       {tier: TierInline, prod: TierModal, typed: true},
	JobReplace.ID:     {tier: TierInline, prod: TierModal, typed: true},
	// Suspend tears down running work (§36c/§37a); resume restores the prior
	// state exactly, so it stays reversible-and-immediate.
	JobSuspend.ID:     {tier: TierInline, prod: TierModal, typed: true, hasReverse: true, reverse: TierNone},
	CronJobSuspend.ID: {tier: TierInline, prod: TierModal, typed: true, hasReverse: true, reverse: TierNone},
	// Drain is always modal, but a y/N card: there's no one name the risk
	// is about (CLAUDE.md: "drain/force-delete always modal").
	Drain.ID: {tier: TierModal, prod: TierModal},
	// Argo CD sync escalates to a modal in PROD but never to the delete-grade
	// type-the-name treatment (§33a).
	ArgoSync.ID: {tier: TierInline, prod: TierModal},
	// Always one y/N line, never PROD-escalated (§35c renew; §27a/§27b "removing
	// a key keeps the y/N too"; 13a's stop-all, a browse-owned gate).
	CertRenew.ID:          {tier: TierInline, prod: TierInline},
	RemoveSecretKey.ID:    {tier: TierInline, prod: TierInline},
	RemoveConfigMapKey.ID: {tier: TierInline, prod: TierInline},
	StopAllForwards.ID:    {tier: TierInline, prod: TierInline},
	// Reversible-and-immediate, PROD or not (cordon/uncordon, flux
	// suspend/resume — both directions restore the prior state exactly).
	Cordon.ID:      {hasReverse: true},
	FluxSuspend.ID: {hasReverse: true},
	// Requests what the controller would do on its own interval anyway
	// (§30a, §33a), or commits a staged preview that was itself the confirm
	// (§17b scale, §36b run-now, §37c rerun's create path, §36d's editor and
	// its one-step undo).
	FluxReconcile.ID:       {},
	ArgoRefresh.ID:         {},
	Scale.ID:               {},
	CronJobRunNow.ID:       {},
	JobRetry.ID:            {},
	CronJobSetSchedule.ID:  {},
	CronJobScheduleUndo.ID: {},
	// 26a labels/annotations: immediate, but a removal or a Service-selector
	// join (known or not yet known) asks one y/N, PROD or not.
	Meta.ID: {onRemove: TierInline, onJoined: TierInline},
	// Immediate outside PROD, one y/N line in PROD — never the type-the-name
	// modal (§24a, §25a, §27a, §27b, §41, and kubectl edit's tty handoff).
	// An add over an existing Secret/ConfigMap key always asks first.
	SetImage.ID:                  {prod: TierInline},
	SetResources.ID:              {prod: TierInline},
	AddSecretKey.ID:              {prod: TierInline, onOverwrite: TierInline},
	AddConfigMapKey.ID:           {prod: TierInline, onOverwrite: TierInline},
	RestartConfigMapConsumers.ID: {prod: TierInline, onOverwrite: TierInline},
	Edit.ID:                      {prod: TierInline},
	NodeDebug.ID:                 {prod: TierInline},
	NodeDebugDetail.ID:           {prod: TierInline},
}

// untiered are the Mutating verbs with no confirm policy at all: tty
// handoffs whose session kubectl owns once kute suspends, gated only by the
// OFFLINE check at their own dispatch sites.
var untiered = map[string]bool{
	Exec.ID:                    true,
	CronJobScheduleFullEdit.ID: true,
}

// Resolve is the destructive-action policy: the confirm v gets on t in a
// context whose PROD tag is isProd. A verb with no row in the table fails
// closed — the type-the-name modal — so a new mutating verb can't execute
// unconfirmed by omission (TestEveryMutatingVerbHasAPolicy catches it first).
func Resolve(v Verb, t Target, isProd bool) Policy {
	r, ok := policies[v.ID]
	if !ok {
		return Policy{Tier: TierModal, TypedName: true}
	}
	tier := r.tier
	if isProd {
		tier = r.prod
	}
	if r.hasReverse && t.Reverse {
		tier = r.reverse
	}
	if r.cascade && slices.Contains(cascadeDeleteKinds, t.Kind) {
		tier = TierModal
	}
	if t.Remove {
		tier = max(tier, r.onRemove)
	}
	if t.Overwrite {
		tier = max(tier, r.onOverwrite)
	}
	if t.Joined {
		tier = max(tier, r.onJoined)
	}
	return Policy{Tier: tier, TypedName: tier == TierModal && r.typed}
}
