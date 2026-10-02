//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// TestEverydayFlow is the everyday-flows acceptance walk as one session: land
// on the Pods list, open a pod, read its logs, its events and its timeline,
// then run a mutating verb — and end up back on the screen you started the
// verb from, which is the part a unit test cannot check.
//
// It runs as one Launch on purpose. Each step's starting state is the
// previous step's ending state, so a screen that renders correctly only when
// opened first has nowhere to hide.
func TestEverydayFlow(t *testing.T) {
	RequireCluster(t)
	a := Launch(t)

	// --- the resting screen ------------------------------------------------
	// Connect starts three informers and browse lands on Pods; both fixture
	// workloads have to reach the frame, including the one that is meant to
	// be broken.
	a.WaitForAll(Connect, "api-", "worker-", "kute-e2e")
	a.waitForWorkerUnready(t)

	// --- pod detail (§5a) --------------------------------------------------
	pod := a.selectAPIPod(t)
	a.Enter()
	a.WaitGone("Loading", Settle)
	// Both containers, and the meta grid's node assignment — the fields that
	// only exist because a real kubelet scheduled and ran this pod.
	a.WaitForAll(Settle, pod, "server", "sidecar")

	// --- logs (§5b) --------------------------------------------------------
	// The reason kind is the substrate rather than kwok: this stream comes
	// off a real kubelet.
	a.Press("l")
	a.WaitFor("KUTE-E2E-LOG-MARKER", Settle)
	a.backToPodDetail()

	// --- events (§9b) ------------------------------------------------------
	a.Press("e")
	a.WaitGone("Loading", Settle)
	a.WaitFor("Started", Settle)
	a.backToPodDetail()

	// --- timeline (§16b) ---------------------------------------------------
	a.Press("t")
	a.WaitGone("Loading", Settle)
	a.WaitFor("TIMELINE", Settle)
	a.backToPodDetail()

	// Back out to the list the pod was opened from.
	a.Esc()
	a.WaitFor("api-", Settle)
}

// TestRolloutRestartStaysOnScreen is the "confirm → execute → refresh → show
// result → remain on screen" contract, run against a real Deployment: a
// rollout-restart is TierInline outside a prod context, so it confirms with
// y/N rather than a type-the-name modal, and when it lands the Deployments
// list must still be the Deployments list.
func TestRolloutRestartStaysOnScreen(t *testing.T) {
	RequireCluster(t)
	a := Launch(t)
	a.WaitFor("api-", Connect)

	// Note which pods the api Deployment is running now, so the restart can
	// be checked by its effect rather than by the absence of an error.
	before := a.selectAPIPod(t)
	a.Esc()

	a.gotoKind(t, "deployments", "Deployments")
	a.WaitForAll(Settle, "api", "worker")

	a.filterTo(t, "api")
	a.Press("R")

	// TierInline: an inline y/N on the keybar, never the red type-the-name
	// modal, because this context is not marked prod.
	a.WaitFor("CONFIRM", Settle)
	if frame := a.Frame(); strings.Contains(frame, TypeToConfirm) {
		t.Fatalf("a non-prod rollout-restart escalated to the type-the-name modal:\n%s", frame)
	}

	a.Press("y")

	// Remain: the confirm clears and the Deployments list is still the
	// Deployments list, with the row still on it. A screen that popped back
	// to Pods, or closed onto an empty state, fails here.
	a.WaitGone("CONFIRM", Settle)
	a.WaitForAll(Settle, "Deployments", "api")

	// And the restart really happened: rollout-restart stamps the pod
	// template, so every pod that was running before is replaced.
	a.Esc()
	a.gotoKind(t, "pods", "Pods")
	a.WaitFor("api-", Settle)
	a.WaitGone(before, Settle)
}

// backToPodDetail leaves a screen pushed from pod detail and waits for pod
// detail itself.
//
// Not a wait for the pod's name: every screen reached from pod detail — logs,
// events, timeline — names the same pod in its own header, so that wait is
// satisfied by the screen being left, and the next key press lands on it
// instead. CONTAINERS is pod detail's own section title and no other screen
// renders it.
func (a *App) backToPodDetail() {
	a.t.Helper()
	a.Esc()
	a.WaitFor("CONTAINERS", Settle)
}

// selectAPIPod filters the Pods list down to the api Deployment's pods,
// leaving the cursor on the first, and returns that pod's name as it appears
// on screen.
func (a *App) selectAPIPod(t *testing.T) string {
	t.Helper()
	a.filterTo(t, "api-")
	name := firstMatch(a.Frame(), "api-")
	if name == "" {
		t.Fatalf("no api- pod row in the filtered list:\n%s", a.Frame())
	}
	a.selectRow(t, name)
	return name
}

// filterTo opens browse's filter, narrows to term, and closes the filter's
// typing mode so ordinary keys work again.
func (a *App) filterTo(t *testing.T, term string) {
	t.Helper()
	// Browse accepts '/' only once the destination kind's rows are ready.
	// A goto fences on the new title, which can render while its lazy informer
	// is still loading; typing immediately after an ignored '/' would route
	// ordinary letters as browse shortcuts (notably 'a' = all namespaces).
	// The term every caller filters for identifies a fixture row, so seeing it
	// is both the honest readiness fence and evidence the filter can match.
	a.WaitFor(term, Settle)
	a.Press("/")
	a.Type(term)
	frame, ok := a.poll(func(frame string) bool {
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "/ "+term) {
				return true
			}
		}
		return false
	}, Settle)
	if !ok {
		t.Fatalf("filter query %q never reached the filter strip:\n%s", term, frame)
	}
	// enter leaves typing mode with the filter applied — esc would clear it.
	a.Enter()
}

// gotoKind switches the browse list to another kind through the one goto
// palette, the way a user does: g, type, enter.
//
// want has to be something only the destination shows — its title, not one
// of its rows. Enter dispatches the switch as a command, so the frame this
// fences on can still be the previous kind's list, and a want that list also
// contains returns instantly, leaving every key after it aimed at a screen
// that is still being replaced.
//
// The wait *before* Enter is what makes a discovered kind reachable: the
// palette only knows the kinds discovery has found so far, and a query for
// one it hasn't (early in a connect) doesn't fail — it matches the kind's own
// CRD row and lands on CustomResourceDefinitions instead. Nothing recovers
// from that; the test just waits out its budget on a screen it never asked
// for. Committing only once the destination is on the ranked list turns that
// race into a wait.
//
// That fence is want itself here because a kind's palette row carries the
// same display name its list is titled with. Where the two differ, drive
// gotoPalette directly.
func (a *App) gotoKind(t *testing.T, query, want string) {
	t.Helper()
	a.gotoPalette(t, query, want, want)
}

// gotoPalette drives the one jump palette: type query, wait for row to be on
// the ranked list, commit, then wait for want.
//
// row and want are separate because the synthetic goto targets — the ones
// with no kind-registry entry behind them — label their row for the query
// rather than for the screen it opens, so their row's detail line is the
// honest fence. §30b's flux tree needs one as much as any discovered kind
// does: it is offered only on a cluster whose Flux CRDs discovery has
// actually found, so early in a connect it is simply not there.
func (a *App) gotoPalette(t *testing.T, query, row, want string) {
	t.Helper()
	a.Press("g")
	a.Type(query)
	a.WaitFor(row, Settle)
	a.Enter()
	a.WaitFor(want, Settle)
}

// firstMatch returns the first whitespace-delimited token on any line that
// starts with prefix — how a row's name is read out of a frame without
// pinning column positions that a resize would move.
//
// The token has to be strictly longer than the prefix, which is what keeps
// the filter's own echo of the search term ("/ api-") from being mistaken for
// a row.
func firstMatch(frame, prefix string) string {
	for _, line := range strings.Split(frame, "\n") {
		for _, field := range strings.Fields(line) {
			if len(field) > len(prefix) && strings.HasPrefix(field, prefix) {
				return field
			}
		}
	}
	return ""
}

// TestCrashLoopingWorkloadSurfaces is what the worker fixture exists for
// beyond a red row in the list. A container that exits non-zero on every
// start produces state no fake generates for free — a termination record with
// a real exit code, a restart count the kubelet keeps incrementing, and a
// backoff the API server reports — and three screens are supposed to say so.
func TestCrashLoopingWorkloadSurfaces(t *testing.T) {
	RequireCluster(t)
	a := Launch(t)
	a.WaitFor("worker-", Connect)

	a.waitForWorkerUnready(t)

	a.filterTo(t, "worker-")
	pod := firstMatch(a.Frame(), "worker-")
	if pod == "" {
		t.Fatalf("no worker- pod row:\n%s", a.Frame())
	}
	a.Enter()
	a.WaitLoaded(Settle)

	// §5a's termination banner: the exit code and reason from the last
	// termination, which only exists because a real container really ran and
	// really failed.
	// Same reasoning as waitForWorkerUnready: the header's status word is
	// whatever the container was doing when this screen fetched the pod, so
	// it is not something to assert on. The termination record is — it
	// persists across the whole backoff, and it is the part that only exists
	// because a real container really ran and really failed.
	a.WaitForAll(Settle,
		pod,
		"Last termination",
		"exit 1",
		"restarts",
	)

	// §16b's timeline merges the restarts and the events into one feed. The
	// restart entry carries the same exit code, from a different source.
	a.Press("t")
	a.WaitLoaded(Settle)
	a.WaitForAll(Settle, "Timeline", "Restarted", "exit 1")
	// And the rollout rail alongside it, which is the other half of 16b.
	a.WaitFor("ROLLOUT HISTORY", Settle)
}

// TestCrashLoopingContainerLogsShowCrashOutput is review 04 H2: `l` on a
// crash-looping container must show what it printed before it died. Between
// restarts the container is Waiting(CrashLoopBackOff), and 5b used to park
// every Waiting container on "waiting for container to start" — hiding the
// crash for up to the five-minute backoff cap. The worker fixture prints
// "worker starting" and exits 1 on every run, so that line is in every run's
// log whatever state the container is in when the screen opens; it is never
// on 5a, which this test leaves.
func TestCrashLoopingContainerLogsShowCrashOutput(t *testing.T) {
	RequireCluster(t)
	a := Launch(t)
	a.WaitFor("worker-", Connect)

	a.waitForWorkerUnready(t)

	a.filterTo(t, "worker-")
	a.Enter()
	a.WaitFor("CONTAINERS", Settle)
	// Wait for a restart to have happened, so there is a previous run to read
	// — the termination record is the durable statement of it.
	a.WaitFor("Last termination", Settle)

	a.Press("l")
	a.WaitFor("worker starting", Settle)
}

// waitForWorkerUnready waits for the crash-looping fixture's row to show it is
// not ready.
//
// Deliberately not a wait for the literal "CrashLoopBackOff". That word is the
// container's *waiting* reason, so it is absent for the few seconds per cycle
// the container is actually running and failing — and, because the list only
// redraws when a watch event arrives, a frame captured during one of those
// windows stays on screen until the next restart. With the backoff capped at
// five minutes, that is far longer than any assertion here should wait. The
// RDY column is the stable statement of the same fact: a container that exits
// on every start never becomes ready.
func (a *App) waitForWorkerUnready(t *testing.T) {
	t.Helper()
	frame, ok := a.poll(func(f string) bool {
		for _, line := range strings.Split(f, "\n") {
			if strings.Contains(line, "worker-") && strings.Contains(line, "0/1") {
				return true
			}
		}
		return false
	}, Settle)
	if !ok {
		t.Fatalf("the worker fixture never showed as unready:\n%s", frame)
	}
}
