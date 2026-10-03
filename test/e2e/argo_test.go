//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// TestArgoApplications drives §33a against real Argo CD CRDs served by a
// real API server, with no Argo controller running — the same load-bearing
// absence as TestFluxScreens: the hand-written sync/health/operationState
// status in 55-argocd-objects.yaml is permanent, so a Degraded app stays
// Degraded for the whole test instead of being reconciled away.
func TestArgoApplications(t *testing.T) {
	RequireCluster(t)
	applications := argoDynamic(t).Resource(applicationGVR).Namespace(Namespace)
	seedDynamicObject(t, applications, "kute-billing", func(obj *unstructured.Unstructured) {
		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["argocd.argoproj.io/refresh"] = "e2e-prior-state"
		obj.SetAnnotations(annotations)
		unstructured.RemoveNestedField(obj.Object, "operation")
	}, func(current, original *unstructured.Unstructured) {
		restoreAnnotation(current, original, "argocd.argoproj.io/refresh")
		restoreNestedField(t, current, original, "operation")
	})
	a := Launch(t)
	a.WaitFor("api-", Connect)

	gotoApps := func() {
		a.gotoKind(t, "applic", "Applications")
		// "kute-billing" alone isn't a Ready fence: a revisited kind seeds
		// its loading state from 15a's cached-rows-dimmed view
		// (resetAndLoad), which can render the very same row text while
		// m.state is still Loading. argoVerbsApply() — and so every §33a
		// verb, refresh included — is gated on state == Ready, so a 'r'
		// pressed inside that window is a silent no-op: nothing in the
		// switch's case list matches, and the model has no complaint to
		// show. "refresh" only ever renders in the keybar once
		// argoVerbsApply() is true, so waiting for it here closes the
		// exact window "list renders the sync×health matrix" already
		// fences on for its own first visit.
		a.WaitForAll(Settle, "kute-billing", "refresh")
	}

	t.Run("list renders the sync×health matrix", func(t *testing.T) {
		gotoApps()

		// Both independent axes visible at once, pinned above the fold:
		// Synced+Degraded and OutOfSync+Healthy — different incidents, on
		// their own columns rather than collapsed into one glyph. SYNC is wide
		// enough to keep Argo's full status vocabulary visible.
		a.WaitForAll(Settle, "kute-billing", "kute-frontend", "Degraded", "OutOfSync")

		// §33a's sub-line: the diagnosis Argo itself wrote, verbatim on the
		// row — naming the worker Deployment, which genuinely CrashLoopBackOffs
		// on this cluster.
		a.WaitForWrapped(`Deployment/worker: container "worker" is in CrashLoopBackOff`, Settle)

		// The §33a verb set is advertised.
		a.WaitForAll(Settle, "refresh", "sync", "dashboard url")

		// kute-search is Syncing/Progressing — normal activity, not trouble —
		// so §33a folds it into the healthy tail rather than pinning it above
		// the fold with the two unhealthy rows. The health strip's own tally
		// must count it as "progressing", never as "degraded"/"out of sync",
		// the same "a reconciling object is not a failure" rule TestFluxScreens
		// checks for Kustomizations.
		a.WaitForAll(Settle, "1 degraded", "1 out of sync", "1 progressing")
		if frame := a.Frame(); strings.Contains(frame, "2 degraded") || strings.Contains(frame, "2 out of sync") {
			t.Errorf("kute-search (Syncing/Progressing) was folded into the degraded/out-of-sync tally:\n%s", frame)
		}

		// Progressing and Healthy both fold into the healthy tail — expand
		// it (tab) to see the rest of the matrix on screen at once. Not
		// asserting the SYNC/HEALTH cell text here too: the health strip
		// above already pinned "1 progressing" as the untruncated word.
		a.Press("tab")
		a.WaitForAll(Settle, "kute-search", "kute-api", "kute-web")
	})

	// argoproj.io also serves AppProject, which carries no sync/health
	// status — internal/resources/crd.go recognises Application by group
	// *and* Kind, so AppProject has to stay on the generic 14a
	// custom-resource path untouched. This is the exact Flux/HelmRelease
	// collision class CLAUDE.md calls out, on the other kind in the pair.
	t.Run("appproject stays on the generic custom-resource path", func(t *testing.T) {
		a.gotoKind(t, "appproj", "Appprojects")
		a.WaitFor("default", Settle)

		frame := a.Frame()
		if strings.Contains(frame, "Sync Status") || strings.Contains(frame, "Health Status") {
			t.Errorf("AppProject rendered Argo's Sync/Health columns — it must stay on the generic CRD path:\n%s", frame)
		}
	})

	t.Run("refresh stamps the annotation", func(t *testing.T) {
		gotoApps()
		a.selectListRow(t, "kute-billing")

		// 'r' is TierNone — it executes immediately, no confirm. The fence is
		// the API, not a frame: the annotation is invisible to the row.
		before := waitForApplication(t, "kute-billing", "was readable before refresh", func(*unstructured.Unstructured) bool { return true })
		a.Press("r")
		refreshed := waitForApplication(t, "kute-billing", "gained a fresh refresh annotation",
			func(u *unstructured.Unstructured) bool {
				return u.GetAnnotations()["argocd.argoproj.io/refresh"] == "normal" &&
					u.GetResourceVersion() != before.GetResourceVersion()
			})
		requireNewResourceVersion(t, before.GetResourceVersion(), refreshed.GetResourceVersion())
		a.WaitForAll(Settle, "Applications", "refresh")
	})

	t.Run("sync shows the confirm then patches the target revision", func(t *testing.T) {
		gotoApps()
		a.selectListRow(t, "kute-billing")

		a.Press("S")
		// TierInline: the pill switches to CONFIRM rather than syncing
		// immediately. The will-run note (§27a's "names the command that
		// executes" kubectl patch line) is set but doesn't fit next to the
		// keybar's own y/n hints on a 120-col frame — insetChromeLine drops
		// a RightNote outright rather than wrapping it — so this only
		// asserts the tier, the same restraint flux_test.go's own
		// suspend/reconcile subtest shows (it verifies the write through the
		// API, never a will-run string on screen).
		a.WaitFor("CONFIRM", Settle)
		before := waitForApplication(t, "kute-billing", "was readable before sync", func(*unstructured.Unstructured) bool { return true })
		a.Press("y")

		// The app's own spec.source.targetRevision ("main") pins the sync,
		// read from the API rather than a frame (the list does not project
		// .operation).
		synced := waitForApplication(t, "kute-billing", "gained a fresh sync request",
			func(u *unstructured.Unstructured) bool {
				rev, _, _ := unstructured.NestedString(u.Object, "operation", "sync", "revision")
				return rev == "main" && u.GetResourceVersion() != before.GetResourceVersion()
			})
		requireNewResourceVersion(t, before.GetResourceVersion(), synced.GetResourceVersion())
		a.WaitForAll(Settle, "Applications", "refresh")
	})

	t.Run("dashboard url resolves", func(t *testing.T) {
		gotoApps()
		a.selectListRow(t, "kute-billing")

		// The deep link itself lands on the clipboard, which no frame can
		// show; what this asserts is that the argocd-cm seam resolved — a
		// failure would surface as this exact execFeedback string.
		a.WaitFor("dashboard url", Settle)
		a.Press("u")
		a.Never("dashboard url not configured", 2*time.Second)
	})

	t.Run("enter opens the generic detail, no bespoke screen", func(t *testing.T) {
		gotoApps()

		// Warm the Event informer before ↵: 14d's load() reads Events with
		// one one-shot call, and objectdetail (docs/design README.md §14d)
		// redirects straight to the raw YAML view rather than an empty
		// screen when an object appears to have neither conditions nor
		// events — Argo apps never carry a condition, so that read is the
		// only thing standing between kute-billing and the redirect. If the
		// Event kind's cache hasn't synced even once yet (it's lazy, same as
		// every other kind), the one-shot read can't tell "genuinely none"
		// from "not synced yet" and takes the redirect regardless.
		// Visiting 9b first — the ordinary way a user would already have
		// looked — makes sure the cache is warm by the time ↵ reads it. Not
		// waiting for kute-billing's own event text here: 9b applies its own
		// "last hour" display window, which the fixture's fixed timestamp
		// falls outside of — ObjectEvents itself carries no such filter, so
		// the cache this warms is what 14d actually reads.
		a.Press("e")
		a.WaitLoaded(Settle)
		a.back(t, "Applications")

		a.selectListRow(t, "kute-billing")

		// §33a deliberately keeps Custom: true — there is no bespoke Argo
		// detail screen the way Flux has 31a, so ↵ routes to 14d's generic
		// detail: a meta grid off the kind's own descriptor columns (SYNC/
		// HEALTH/REVISION/SYNCED — the same short headers the list uses,
		// not the CRD's raw printer-column names), plus CONDITIONS (empty —
		// Argo apps never carry one) and EVENTS.
		a.Enter()
		a.WaitLoaded(Settle)
		a.WaitForAll(Settle, "kute-billing", "SYNC", "HEALTH", "REVISION", "Degraded")
		// The REASON cell is 16 wide, so "OperationCompleted" truncates with
		// the app-wide … marker rather than being hard-cut.
		a.WaitForAll(Settle, "CONDITIONS", "EVENTS", "OperationComple…", "application synced successfully")

		// No Flux-style bespoke banner — proof this is 14d, not a
		// copy-pasted Flux detail screen.
		a.Never("RECONCILE FAILED", 2*time.Second)

		a.back(t, "Applications")
	})
}

var applicationGVR = schema.GroupVersionResource{
	Group: "argoproj.io", Version: "v1alpha1", Resource: "applications",
}

// argoDynamic builds a dynamic client against the e2e kubeconfig — the way to
// assert a write landed without reading it back through the UI that issued
// it. Same shape as flux_test.go's fluxDynamic.
func argoDynamic(t *testing.T) dynamic.Interface {
	t.Helper()
	cfg, err := clientcmd.BuildConfigFromFlags("", KubeconfigPath())
	if err != nil {
		t.Fatalf("building a REST config from %s: %v", KubeconfigPath(), err)
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("building a dynamic client: %v", err)
	}
	return client
}

// waitForApplication polls until pred is satisfied — the same shape as
// flux_test.go's waitForKustomization. The write goes out through a
// tea.Cmd, so a one-shot read here would be a race by construction; and the
// assertion is on the API server, never on a frame (the list does not
// project .operation, and the refresh annotation is invisible to the row).
func waitForApplication(t *testing.T, name, what string, pred func(*unstructured.Unstructured) bool) *unstructured.Unstructured {
	t.Helper()
	client := argoDynamic(t).Resource(applicationGVR).Namespace(Namespace)
	deadline := time.Now().Add(Settle)
	var last *unstructured.Unstructured
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		obj, err := client.Get(ctx, name, metav1.GetOptions{})
		cancel()
		if err == nil {
			last = obj
			if pred(obj) {
				return obj
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("application/%s never %s; last object: %+v", name, what, last)
	return nil
}
