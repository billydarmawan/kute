package verbs

import (
	"slices"
	"testing"

	"github.com/kute-dev/kute/internal/kube"
)

// Confirm surfaces, written the way a user meets them.
const (
	immediate = "immediate" // TierNone
	yN        = "y/N"       // TierInline: one keybar line
	card      = "card"      // TierModal, y/N card
	typeName  = "type-name" // TierModal, type the name (a marked set: the count)
)

func surface(p Policy) string {
	switch {
	case p.Tier == TierNone:
		return immediate
	case p.Tier == TierInline:
		return yN
	case p.TypedName:
		return typeName
	default:
		return card
	}
}

// wantPolicy is the whole destructive-action policy, spelled out by hand
// (CLAUDE.md's destructive-action invariant): {outside PROD, inside PROD}
// for each verb's forward direction on an ordinary kind. Exceptions —
// cascade kinds, reverse directions, target facts — are pinned separately
// below.
var wantPolicy = map[string][2]string{
	Delete.ID:                    {yN, typeName},
	ForceDelete.ID:               {typeName, typeName},
	RolloutRestart.ID:            {yN, typeName},
	RolloutUndo.ID:               {yN, typeName},
	Rollback.ID:                  {yN, typeName},
	JobReplace.ID:                {yN, typeName},
	JobSuspend.ID:                {yN, typeName},
	CronJobSuspend.ID:            {yN, typeName},
	Drain.ID:                     {card, card},
	ArgoSync.ID:                  {yN, card},
	CertRenew.ID:                 {yN, yN},
	RemoveSecretKey.ID:           {yN, yN},
	RemoveConfigMapKey.ID:        {yN, yN},
	StopAllForwards.ID:           {yN, yN},
	Cordon.ID:                    {immediate, immediate},
	FluxSuspend.ID:               {immediate, immediate},
	FluxReconcile.ID:             {immediate, immediate},
	ArgoRefresh.ID:               {immediate, immediate},
	Scale.ID:                     {immediate, immediate},
	CronJobRunNow.ID:             {immediate, immediate},
	JobRetry.ID:                  {immediate, immediate},
	CronJobSetSchedule.ID:        {immediate, immediate},
	CronJobScheduleUndo.ID:       {immediate, immediate},
	Meta.ID:                      {immediate, immediate},
	SetImage.ID:                  {immediate, yN},
	SetResources.ID:              {immediate, yN},
	AddSecretKey.ID:              {immediate, yN},
	AddConfigMapKey.ID:           {immediate, yN},
	RestartConfigMapConsumers.ID: {yN, typeName},
	Edit.ID:                      {immediate, yN},
	NodeDebug.ID:                 {immediate, yN},
	NodeDebugDetail.ID:           {immediate, yN},
	PodDebug.ID:                  {immediate, yN},
}

// kindsFor is every kind a verb is checked against: its own Kinds (or a
// spread of ordinary kinds for a kind-agnostic verb), plus every
// cascade-delete kind.
func kindsFor(v Verb) []kube.ResourceKind {
	kinds := slices.Clone(v.Kinds)
	if len(kinds) == 0 {
		kinds = []kube.ResourceKind{kube.KindPod, kube.KindDeployment, kube.KindConfigMap, kube.KindHelmRelease}
	}
	return append(kinds, cascadeDeleteKinds...)
}

// TestResolvePolicyTable pins every (verb, kind, PROD) combination the
// registry defines — tier and confirm style together — in one table.
func TestResolvePolicyTable(t *testing.T) {
	t.Parallel()

	for _, v := range All {
		want, ok := wantPolicy[v.ID]
		if !ok {
			continue
		}
		for _, kind := range kindsFor(v) {
			for i, isProd := range []bool{false, true} {
				exp := want[i]
				if v.ID == Delete.ID && slices.Contains(cascadeDeleteKinds, kind) {
					exp = typeName // a Namespace/CRD/Node delete is always the modal
				}
				got := surface(Resolve(v, Target{Kind: kind}, isProd))
				if got != exp {
					t.Errorf("Resolve(%s, %s, prod=%v) = %s, want %s", v.ID, kind, isProd, got, exp)
				}
			}
		}
	}
}

// TestResolveTargetFacts pins the per-target exceptions: reverse directions,
// overwrites, Service-selector joins and key removals.
func TestResolveTargetFacts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		verb Verb
		t    Target
		want [2]string // {outside PROD, inside PROD}
	}{
		{"uncordon", Cordon, Target{Reverse: true}, [2]string{immediate, immediate}},
		{"flux resume", FluxSuspend, Target{Reverse: true}, [2]string{immediate, immediate}},
		{"job resume", JobSuspend, Target{Kind: kube.KindJob, Reverse: true}, [2]string{immediate, immediate}},
		{"cronjob resume", CronJobSuspend, Target{Kind: kube.KindCronJob, Reverse: true}, [2]string{immediate, immediate}},
		{"reverse ignored on a one-way verb", Delete, Target{Kind: kube.KindPod, Reverse: true}, [2]string{yN, typeName}},
		{"secret add over an existing key", AddSecretKey, Target{Kind: kube.KindSecret, Overwrite: true}, [2]string{yN, yN}},
		{"configmap add over an existing key", AddConfigMapKey, Target{Kind: kube.KindConfigMap, Overwrite: true}, [2]string{yN, yN}},
		{"ctrl-r over an existing key", RestartConfigMapConsumers, Target{Kind: kube.KindConfigMap, Overwrite: true}, [2]string{yN, typeName}},
		{"meta selector-joined label", Meta, Target{Kind: kube.KindPod, Joined: true}, [2]string{yN, yN}},
		{"meta removal", Meta, Target{Kind: kube.KindPod, Remove: true}, [2]string{yN, yN}},
		{"secret key removal", RemoveSecretKey, Target{Kind: kube.KindSecret, Remove: true}, [2]string{yN, yN}},
		{"configmap key removal", RemoveConfigMapKey, Target{Kind: kube.KindConfigMap, Remove: true}, [2]string{yN, yN}},
		{"facts never lower a tier", Delete, Target{Kind: kube.KindNamespace, Overwrite: true, Joined: true, Remove: true}, [2]string{typeName, typeName}},
	}
	for _, tt := range tests {
		for i, isProd := range []bool{false, true} {
			if got := surface(Resolve(tt.verb, tt.t, isProd)); got != tt.want[i] {
				t.Errorf("%s: Resolve(%s, %+v, prod=%v) = %s, want %s", tt.name, tt.verb.ID, tt.t, isProd, got, tt.want[i])
			}
		}
	}
}

// TestEveryMutatingVerbHasAPolicy: a new mutating verb must get a row in
// the policy table (or be a deliberate untiered tty handoff), and the table
// may only name registered verbs.
func TestEveryMutatingVerbHasAPolicy(t *testing.T) {
	t.Parallel()

	for _, v := range All {
		_, hasRule := policies[v.ID]
		_, pinned := wantPolicy[v.ID]
		if hasRule != pinned {
			t.Errorf("%s: policy table row=%v but TestResolvePolicyTable pins it=%v", v.ID, hasRule, pinned)
		}
		if v.Mutating && !hasRule && !untiered[v.ID] {
			t.Errorf("%s is Mutating with no confirm policy — add it to verbs.policies", v.ID)
		}
		if hasRule && untiered[v.ID] {
			t.Errorf("%s is both untiered and in the policy table", v.ID)
		}
	}
	for id := range policies {
		if _, ok := ByID(id); !ok {
			t.Errorf("policy table names %q, which isn't in All", id)
		}
	}
	for id := range untiered {
		if v, ok := ByID(id); !ok || !v.Mutating {
			t.Errorf("untiered names %q, which isn't a registered Mutating verb", id)
		}
	}
}

// TestResolveFailsClosed: a verb with no row never executes unconfirmed.
func TestResolveFailsClosed(t *testing.T) {
	t.Parallel()

	got := Resolve(Verb{ID: "not-registered"}, Target{}, false)
	if got.Tier != TierModal || !got.TypedName {
		t.Fatalf("Resolve(unknown) = %+v, want the type-the-name modal", got)
	}
}

// TestReverseIDsAreTheExecutedVerbs: a two-direction verb's ReverseID is
// the Scope.Verb actions.Controller executes for its undo direction.
func TestReverseIDsAreTheExecutedVerbs(t *testing.T) {
	t.Parallel()

	want := map[string]string{
		Cordon.ID: "uncordon", FluxSuspend.ID: "flux-resume",
		JobSuspend.ID: "job-resume", CronJobSuspend.ID: "cronjob-resume",
	}
	for _, v := range All {
		if v.ReverseID != want[v.ID] {
			t.Errorf("%s.ReverseID = %q, want %q", v.ID, v.ReverseID, want[v.ID])
		}
	}
}
