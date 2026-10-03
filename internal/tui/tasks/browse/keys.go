package browse

import (
	"fmt"
	"strings"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/metapanel"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references only (never a
// key/label literal), per the registry invariant. The empty state (10c)
// spells its own n/a/g ways out inline in the body, so its keybar carries
// no groups — just the pill and the live watch note.
func (m Model) Keybar() tui.Keybar {
	if m.pendingEdit != nil {
		return tui.Keybar{
			Pill:      tui.ModeConfirm,
			PillText:  "CONFIRM",
			Groups:    [][]tui.KeyHint{{{Key: "y", Label: "confirm"}, {Key: "esc", Label: "cancel"}}},
			RightNote: m.editConfirmPrompt(),
		}
	}
	if m.pendingStopAllForwards {
		return tui.Keybar{
			Pill:      tui.ModeConfirm,
			PillText:  "CONFIRM",
			Groups:    [][]tui.KeyHint{{{Key: "y", Label: "confirm"}, {Key: "esc", Label: "cancel"}}},
			RightNote: m.stopAllForwardsPrompt(),
		}
	}
	if m.pendingCronJobRun != nil {
		return m.cronJobRunKeybar()
	}
	if m.pendingCronJobResume != nil {
		return m.cronJobResumeKeybar()
	}
	if m.pendingJobRerun != nil {
		return m.jobRerunKeybar()
	}
	if m.pendingScale != nil {
		kb := tui.Keybar{
			Pill:     tui.ModeBrowse,
			PillText: "SCALE",
			Groups: [][]tui.KeyHint{{
				{Key: m.pendingScale.input.Value() + tui.GlyphSelBar, Label: "replicas"},
				{Key: "+/−", Label: "nudge"},
				{Key: "↵", Label: "apply"},
				{Key: "esc", Label: "cancel"},
			}},
		}
		if hpa := m.pendingScale.hpaName; hpa != "" {
			// The will-run line and the HPA warning together rarely fit on
			// one keybar row (insetChromeLine drops the whole right side
			// rather than truncating either) — the warning is the more
			// decision-relevant fact at this moment, so it takes the slot
			// instead of the redundant kubectl-equivalent text.
			kb.RightWarnNote = fmt.Sprintf("managed by hpa/%s — scaling overridden on next sync", hpa)
		} else {
			kb.RightNote = m.scaleWillRunLine()
		}
		return kb
	}
	if m.pendingSetImage != nil && !m.actions.Active() {
		// While a PROD confirm is showing, the generic m.actions.Active()
		// branch further down takes over (its own "set-image" case renders
		// setImageWillRunLine) — same !m.actions.Active() guard pendingMeta
		// uses, for the same reason: the panel stays open underneath either
		// way (setimage_view.go's will-run strip shows the confirm-pending
		// state).
		t := m.pendingSetImage
		hints := []tui.KeyHint{{Key: "↵", Label: "apply"}, {Key: "↑↓", Label: "pick from history"}}
		if len(t.containers) > 1 {
			hints = append(hints, tui.KeyHint{Key: tui.GlyphTab, Label: "container"})
		}
		hints = append(hints, tui.KeyHint{Key: "ctrl-e", Label: "full ref"}, tui.KeyHint{Key: "esc", Label: "cancel"})
		rightNote := "watch the rollout in 9a"
		if t.kind == kube.KindCronJob {
			rightNote = "future jobs use the new image · running jobs unaffected"
		}
		return tui.Keybar{
			Pill:      tui.ModeBrowse,
			PillText:  "SET IMAGE",
			Groups:    [][]tui.KeyHint{hints},
			RightNote: rightNote,
		}
	}
	if m.pendingSetResources != nil && !m.actions.Active() {
		hints := []tui.KeyHint{{Key: "↵", Label: "apply changed fields"}, {Key: "↑↓", Label: "field"}, {Key: "+/−", Label: "nudge (64Mi / 50m)"}}
		if len(m.pendingSetResources.containers) > 1 {
			hints = append(hints, tui.KeyHint{Key: tui.GlyphTab, Label: "container"})
		}
		hints = append(hints, tui.KeyHint{Key: "u", Label: "unset field"}, tui.KeyHint{Key: "esc", Label: "cancel"})
		return tui.Keybar{
			Pill:     tui.ModeBrowse,
			PillText: "RESOURCES",
			Groups:   [][]tui.KeyHint{hints},
		}
	}
	if m.pendingMeta != nil && !m.actions.Active() {
		// While a TierInline confirm is showing (a joined-label edit or any
		// removal), m.actions.Active() wins instead — the panel-local META
		// keybar below only ever renders in navigation/editing/adding, never
		// stacked under the confirm's own y/N (see the actions.Active()
		// branch's "set-meta" case further down, and metapanel's package doc
		// comment: the panel itself stays open underneath either way).
		return tui.Keybar{Pill: tui.ModeBrowse, PillText: "META", Groups: m.pendingMeta.KeybarHints()}
	}
	if m.actions.Active() {
		if m.actions.Tier() == actions.TierInline {
			note, shortNote := m.actions.Prompt(), ""
			hints := []tui.KeyHint{{Key: "y", Label: "confirm"}, {Key: "esc", Label: "cancel"}}
			if pending := m.actions.Pending(); pending != nil {
				switch pending.Scope.Verb {
				case "rollback":
					// 18a: "shell out to helm with a will run line" — the exact
					// command, not just the generic verb/target prompt every
					// other TierInline confirm uses.
					note = rollbackPrompt(m.session.CommandTarget(), pending.Scope)
					shortNote = rollbackPromptShort(m.session.CommandTarget(), pending.Scope)
				case "delete":
					// 8b/20a: the same "will run: kubectl delete ..."
					// documentation idiom every other mutating verb's inline
					// confirm uses — replaces the generic verb/target prompt,
					// which only duplicated the y/n hints already in Groups.
					// A marked set names every target and offers no
					// force-delete escalation.
					if len(pending.Scope.BulkTargets) > 0 {
						note = m.bulkDeleteWillRunLine(pending.Scope)
						break
					}
					note = deleteWillRunLine(pending.Scope)
					if verbs.ForceDelete.AppliesTo(kube.ResourceKind(pending.Scope.ResourceKind)) {
						hints = append(hints, verbs.ForceDelete.Hint())
					}
				case "set-image":
					// 24a: the exact "will run: kubectl set image ..." line, same
					// idiom as rollback/delete above.
					note = setImageWillRunLine(pending.Scope)
				case "set-resources":
					// 25a: the exact "will run: kubectl set resources ..." line,
					// same idiom as set-image above.
					note = setResourcesWillRunLine(pending.Scope)
				case "rollout-restart":
					// 9a: the exact "will run: kubectl rollout restart ..." line,
					// same idiom as set-image/set-resources above.
					note = rolloutRestartWillRunLine(pending.Scope)
				case "cert-renew":
					// 35c: the exact "will run: kubectl patch certificate/...
					// --subresource=status ..." line, same idiom as
					// rollout-restart/argo-sync above.
					note = certRenewWillRunLine(pending.Scope)
				case "argo-sync":
					// 33a: the exact "will run: kubectl patch application/... ..."
					// line, same idiom as rollout-restart above. ArgoSync is
					// TierInline outright (a real, visible change to what's
					// deployed), not a nominal-TierNone verb that only reaches
					// here in PROD — argo-refresh never appears here, fixed
					// TierNone, same as flux-reconcile's own absence.
					note = argoSyncWillRunLine(pending.Scope)
				case "job-retry":
					// Only reached here for a job-retry action that skipped
					// browse's own staged 'R' preflight (never happens from
					// this screen — commitJobRerun always stages first — but
					// kept for the same defensive-completeness reason every
					// other verb's case exists). Same idiom as rollout-restart.
					note = jobRetryWillRunLine(pending.Scope)
				case "job-replace":
					// §37c's "replace" choice — genuinely destructive, so
					// (unlike job-retry) this really does reach the ordinary
					// Controller confirm every time: staging only offers the
					// choice, it doesn't confirm it (job_actions.go's own doc
					// comment).
					note = jobReplaceWillRunLine(pending.Scope)
				case "job-suspend", "job-resume":
					// the exact "will run: kubectl patch job/... --type merge
					// ..." line, same idiom as flux-suspend/flux-resume's own
					// (unconfirmed, TierNone) execFeedback line, just rendered
					// through the confirm note since this one is TierInline.
					note = jobSuspendWillRunLine(pending.Scope)
				case "cronjob-suspend":
					// only reached outside PROD — verbs.Resolve's CronJobSuspend row
					// escalates suspend to TierModal in PROD instead (routed to
					// delete.go's typeNameConfirmModal via
					// verbs.Resolve's typed-name policy for "cronjob-suspend", single-
					// target only — a marked-set confirm routes to
					// cronjob_bulk.go's own bulk count modal instead, Body()'s
					// dispatch). cronjob-run-now no longer reaches here at all
					// (0.8.0 plan §36b: staged in screen state, TierNone,
					// cronjob_actions.go's own Keybar() case further up).
					// cronjob-resume never appears here either, fixed TierNone
					// (staged, reversible, immediate) the same way. cronjob-set-
					// schedule is likewise absent — its apply is unconditionally
					// TierNone (0.8.0 plan §3 decision 10).
					if len(pending.Scope.BulkTargets) > 0 {
						note = bulkCronJobSuspendWillRunLine(pending.Scope)
						break
					}
					note = cronJobSuspendWillRunLine(pending.Scope)
					if summary, ok := m.cronJobSummaryFor(pending.Scope.Namespace, pending.Scope.ResourceName); ok {
						note += "   " + cronJobSuspendDangerNote(summary, m.now)
					}
				case "set-meta":
					// 26a: the panel itself stays open under this confirm
					// (meta.go's own doc comment) and already renders the full
					// "will run: kubectl label/annotate ..." line plus the
					// Service-selector join warning in its own will-run strip —
					// duplicating that whole line here regularly overruns the
					// keybar's width and gets silently dropped entirely
					// (insetChromeLine's "not enough room" behavior), so this
					// note stays to the short, keybar-safe join warning alone
					// (never shown for a plain removal, which carries no join
					// text of its own).
					note = metapanel.WillRunLine(pending.Scope)
					if pending.Scope.MetaJoinService != "" {
						note = fmt.Sprintf("detaches %d pods from svc/%s",
							pending.Scope.MetaJoinPodCount, pending.Scope.MetaJoinService)
					}
				}
			}
			return tui.Keybar{
				Pill:           tui.ModeConfirm,
				PillText:       "CONFIRM",
				Groups:         [][]tui.KeyHint{hints},
				RightNote:      note,
				RightNoteShort: shortNote,
			}
		}
		return tui.Keybar{Pill: tui.ModeConfirm, PillText: "CONFIRM"}
	}

	pillText := strings.ToUpper(m.desc.Display)
	if m.desc.Short != "" {
		// §14a: a discovered kind's pill is its short kind name (CERTS),
		// not the full uppercase plural.
		pillText = m.desc.Short
	}
	switch m.kind {
	case kube.KindHelmRelease:
		// docs/design README.md §18a: "Keybar pill HELM" — the short form,
		// unlike every other kind's pill (its full uppercase Display name).
		pillText = "HELM"
	case kube.KindDeployment:
		// docs/design README.md §9a: "Keybar pill DEPLOY", not the plural
		// "DEPLOYMENTS" every other kind's pill would produce.
		pillText = "DEPLOY"
	case kube.KindCustomResourceDefinition:
		// docs/design README.md §14b: "Keybar pill CRDS" — the built-in CRD
		// list's own short form, not the full "CUSTOMRESOURCEDEFINITIONS".
		pillText = "CRDS"
	}
	if m.desc.Argo {
		// docs/design README.md §33a: "Keybar pill ARGOAPP" — gated on the
		// descriptor flag rather than a kind-name case above, since
		// Application is discovered, not a compile-time kube.KindX constant.
		pillText = "ARGOAPP"
	}
	pill := tui.ModeBrowse
	if m.grouped() {
		pillText, pill = "ALL NS", tui.ModeAllNS
	}
	if m.filterActive && m.filterListFocused {
		// The filter strip stays up (view.go's Strips) once committed via
		// enter, so the pill keeps saying so too — takes over ALL NS (still
		// true regardless, just less urgent) but itself yields to a mark
		// count below, same "more urgent state wins" precedent.
		pillText, pill = "FILTER", tui.ModeFilter
	}
	if n := len(m.marks); n > 0 {
		// 20a: "mode pill shows the count" — takes over the pill entirely,
		// the more urgent state once anything is marked.
		pillText, pill = fmt.Sprintf("%d MARKED", n), tui.ModeBrowse
	}

	if m.state == tui.TaskStatePermissionDenied {
		return tui.Keybar{
			Pill:       pill,
			PillText:   pillText,
			Groups:     [][]tui.KeyHint{{verbs.Goto.Hint(), verbs.Context.Hint(), verbs.WhoCan.Hint(), {Key: "y", Label: "copy error"}, verbs.Retry.Hint()}},
			RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint()),
		}
	}
	if m.state == tui.TaskStateError {
		return tui.Keybar{
			Pill:       pill,
			PillText:   pillText,
			Groups:     [][]tui.KeyHint{{verbs.Retry.Hint(), verbs.Goto.Hint(), verbs.Context.Hint(), {Key: "y", Label: "copy error"}}},
			RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint()),
		}
	}

	if m.state == tui.TaskStateEmpty {
		return tui.Keybar{
			Pill:       pill,
			PillText:   pillText,
			RightNote:  fmt.Sprintf("0 %s · watching — new %s appear live", lowerDisplay(m.desc.Display), lowerDisplay(m.desc.Display)),
			RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint()),
		}
	}

	if m.state == tui.TaskStateLoading {
		// 15a: only the nav verbs (g/n/c) are live before the first list
		// lands — everything row-scoped stays dark until rows exist
		// (docs/design README.md §15a).
		return tui.Keybar{
			Pill:       pill,
			PillText:   pillText,
			Groups:     [][]tui.KeyHint{{verbs.Goto.Hint(), verbs.Namespace.Hint(), verbs.Context.Hint()}},
			RightNote:  "row actions enable when data lands",
			RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint()),
		}
	}

	if m.filterActive && !m.filterListFocused {
		return tui.Keybar{
			Pill:      tui.ModeFilter,
			PillText:  "FILTER",
			Groups:    [][]tui.KeyHint{{{Key: "esc", Label: "clear"}, verbs.MarkAll.Hint()}},
			RightNote: "type to narrow",
		}
	}

	if m.offline() {
		return tui.Keybar{
			Pill:     tui.ModeOffline,
			PillText: "OFFLINE",
			Groups: [][]tui.KeyHint{{
				verbs.Retry.Hint(),
				verbs.Context.Hint(),
				{Key: "↑↓", Label: "browse snapshot"},
			}},
			RightNote: "mutating actions disabled",
		}
	}

	// v.0.3.0.dc.html §29a: goto/filter/mark/yaml/edit/events/timeline/meta/
	// namespace/context/all-namespaces are identical on every screen, so they
	// never render here — they're taught once, in the ? overlay's SCOPE/
	// LIST/RESOURCE columns (app.helpScopeKeys/helpListKeys/
	// helpResourceKeys). Only what's actually different about this kind's
	// row goes in the bar below.
	groups := [][]tui.KeyHint{}
	if m.kind == kube.KindPod {
		podGroup := []tui.KeyHint{}
		if m.openLogs != nil {
			podGroup = append(podGroup, verbs.Logs.Hint())
		}
		podGroup = append(podGroup, verbs.Exec.Hint())
		if m.openForward != nil {
			podGroup = append(podGroup, verbs.Forward.Hint())
		}
		if len(podGroup) > 0 {
			groups = append(groups, podGroup)
		}
	}
	if m.kind == kube.KindNode {
		nodeGroup := []tui.KeyHint{}
		nodeGroup = append(nodeGroup, verbs.NodeDebug.Hint())
		groups = append(groups, nodeGroup)
		if m.mutator != nil {
			groups = append(groups, []tui.KeyHint{verbs.Cordon.Hint(), verbs.Drain.Hint()})
		}
	}
	// openPodsHint relabels ↵ to "pods" — Deployment/StatefulSet/DaemonSet
	// rows all open the kind's own pods list (update.go's openDeploymentPods/
	// openStatefulSetPods/openDaemonSetPods), the exact "pods of deployment"
	// → "pods" compression v.0.3.0.dc.html §29a calls out; the key still
	// comes from the registry, only the label is contextual (same pattern as
	// the bulk-delete count override below).
	openPodsHint := verbs.Open.Hint()
	openPodsHint.Label = "pods"
	if m.kind == kube.KindDeployment {
		deployGroup := []tui.KeyHint{openPodsHint}
		if m.mutator != nil {
			deployGroup = append(deployGroup, verbs.Scale.Hint(), verbs.SetImage.Hint(), verbs.SetResources.Hint())
		}
		if m.openForward != nil {
			deployGroup = append(deployGroup, verbs.Forward.Hint())
		}
		groups = append(groups, deployGroup)
	}
	if m.kind == kube.KindStatefulSet {
		stsGroup := []tui.KeyHint{openPodsHint}
		if m.mutator != nil {
			stsGroup = append(stsGroup, verbs.Scale.Hint(), verbs.SetImage.Hint(), verbs.SetResources.Hint())
		}
		groups = append(groups, stsGroup)
	}
	if m.kind == kube.KindDaemonSet {
		dsGroup := []tui.KeyHint{openPodsHint}
		if m.mutator != nil {
			dsGroup = append(dsGroup, verbs.SetImage.Hint(), verbs.SetResources.Hint())
		}
		groups = append(groups, dsGroup)
	}
	if m.kind == kube.KindService && m.openForward != nil {
		groups = append(groups, []tui.KeyHint{verbs.Forward.Hint()})
	}
	if m.kind == kube.KindHelmRelease {
		helmGroup := []tui.KeyHint{}
		if m.openHelmDetail != nil {
			detailHint := verbs.Open.Hint()
			detailHint.Label = "diagnose"
			helmGroup = append(helmGroup, detailHint)
		}
		helmGroup = append(helmGroup, verbs.HelmPods.Hint())
		if m.openHelmValues != nil {
			helmGroup = append(helmGroup, verbs.HelmValues.Hint())
		}
		if m.openHelmHistory != nil {
			helmGroup = append(helmGroup, verbs.HelmHistory.Hint())
		}
		if m.mutator != nil {
			helmGroup = append(helmGroup, verbs.Rollback.Hint())
		}
		if len(helmGroup) > 0 {
			groups = append(groups, helmGroup)
		}
	}
	if m.fluxVerbsApply() {
		groups = append(groups, m.fluxKeybarGroup())
	}
	if m.argoVerbsApply() {
		groups = append(groups, m.argoKeybarGroup())
	}
	if m.certVerbsApply() {
		groups = append(groups, m.certKeybarGroup())
	}
	if m.kind == kube.KindJob && m.state == tui.TaskStateReady && m.mutator != nil {
		groups = append(groups, m.jobKeybarGroup())
	}
	if m.kind == kube.KindCronJob && m.state == tui.TaskStateReady && m.mutator != nil {
		groups = append(groups, m.cronJobKeybarGroup())
	}
	if m.kind == kube.KindForward {
		fwdGroup := []tui.KeyHint{verbs.StopForward.Hint(), verbs.RestartForward.Hint(), verbs.CopyForwardURL.Hint()}
		if m.forwards != nil {
			fwdGroup = append(fwdGroup, verbs.StopAllForwards.Hint())
		}
		groups = append(groups, fwdGroup)
	}
	if m.mutator != nil && m.kind != kube.KindForward && m.kind != kube.KindHelmRelease {
		// 18a: delete/uninstall is deliberately out of scope for Helm
		// Releases, the same "no install/upgrade-from-repo" carve-out —
		// ctrl-d has no meaningful, safe implementation here (it would only
		// delete one revision Secret, not uninstall the release).
		deleteHint := verbs.Delete.Hint()
		if n := len(m.marks); n > 0 && verbs.Delete.Bulk {
			// 20a: "ctrl-d delete 3 · y/N" — the set-applicable verb's
			// keybar label names the marked count instead of the cursor row.
			deleteHint.Label = fmt.Sprintf("delete %d", n)
		}
		if m.kind == kube.KindDeployment {
			groups = append(groups, []tui.KeyHint{verbs.RolloutRestart.Hint()})
		}
		groups = append(groups, []tui.KeyHint{deleteHint})
	}
	if m.kind == kube.KindService {
		groups = append(groups,
			[]tui.KeyHint{verbs.CopyServiceClusterIP.Hint()},
			[]tui.KeyHint{verbs.CopyServiceExternalIP.Hint()},
		)
	}
	if m.grouped() {
		// JumpNamespace/ToggleGroup are only meaningful once already in ALL
		// NS mode, so — unlike Namespace/Context/AllNamespaces themselves —
		// they stay in the bar rather than moving to ?.
		groups = append(groups, []tui.KeyHint{verbs.JumpNamespace.Hint(), verbs.ToggleGroup.Hint()})
	}

	return tui.Keybar{
		Pill:       pill,
		PillText:   pillText,
		Groups:     groups,
		RightNote:  m.execFeedback,
		RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint()),
	}
}

func lowerDisplay(display string) string {
	return strings.ToLower(display)
}

// CapturingInput reports whether the filter box is open, so the root shell
// (tui.InputCapturer) lets every keystroke — including g/n/c/? — reach
// browse's own key handling instead of treating them as global shortcuts.
// A committed-but-list-focused filter (filterListFocused) doesn't count:
// its query is done being typed, so g/n/c/? go back to being the shell's
// global shortcuts, same as any other ready list.
func (m Model) CapturingInput() bool {
	return (m.filterActive && !m.filterListFocused) || m.actions.Active() || m.pendingEdit != nil || m.pendingStopAllForwards ||
		m.pendingScale != nil || m.pendingSetImage != nil || m.pendingSetResources != nil || m.pendingMeta != nil ||
		m.pendingJobRerun != nil || m.pendingCronJobRun != nil || m.pendingCronJobResume != nil
}

// ActionResult puts the actions controller's outcome line — a failed or
// successful delete, drain, cordon, restart, … — on the keybar
// (tui.ActionResulter, rendered by tui.Frame).
func (m Model) ActionResult() tui.ResultLine { return m.actions.Result() }
