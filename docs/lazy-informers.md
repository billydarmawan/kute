# kute — Lazy informers & startup cost

How kute went from pulling ~22 MB before drawing anything to pulling ~1.3 MB, and what
that cost in design changes.

Shipped on `main` (19 commits, originally the `perf/lazy-informers` branch; run against a
real cluster, see [Observed on the real cluster](#observed-on-the-real-cluster)). Full
suite green, `-race` clean, **zero golden fixtures changed** — this work alters *when*
data loads, never how it renders.

Everything identified along the way has been done. What remains in §5 is the reasoning
behind each change and the trade-offs deliberately accepted, not a backlog.

## Contents

1. [The problem](#1-the-problem)
2. [What shipped](#2-what-shipped)
3. [Measured effect](#3-measured-effect)
4. [Design decisions worth remembering](#4-design-decisions-worth-remembering)
5. [Follow-ups](#5-follow-ups)
6. [CronJobs: a second lazy dependency and a third one-shot read (v0.8.0)](#6-cronjobs-a-second-lazy-dependency-and-a-third-one-shot-read-v080)
7. [Pod debug: an authoritative fourth one-shot read](#7-pod-debug-an-authoritative-fourth-one-shot-read)
8. [Who am I: a fifth one-shot read](#8-who-am-i-a-fifth-one-shot-read)

---

## 1. The problem

`kube.Cluster.Start` registered and started **21 cluster-wide informers** at connect and
blocked on all of them. Before first paint the app pulled every Secret, ConfigMap, Event,
ReplicaSet and ControllerRevision in the cluster, then kept watching them for the rest of
the session whether or not those screens were ever opened. Startup cost scaled with
cluster size rather than with what was on screen.

This surfaced as a 1–2 minute startup over an SSH port-forward to a private AKS cluster,
with the connection banner flapping throughout. The tunnel only made it visible; the
underlying property was there on every cluster.

Two facts from the investigation shaped everything that followed:

- **Every typed read already funnelled through `Cluster.ListRaw`** — including the
  non-registry paths (`rbac.go`, `events.go`, `CountInstances`). No typed lister access
  existed anywhere else, which made an implicit auto-start hook in `ListRaw` a complete
  backstop: no screen has to declare what it needs, and no read path can forget to.
- **`browse` already had the loading machinery.** `applyRowsLoaded` handled "empty result
  + cache not synced → stay loading, retry in 250 ms". It only needed a per-kind truth
  source instead of the cluster-wide one.

### Why not namespace-scoped informers by default

Considered and ruled out **as the default**. `namespace == ""` is a first-class,
user-reachable state (the `a` key, §6b's namespace-grouped triage view), and ~15 consumers
genuinely need cross-namespace reads — the overview, the Nodes list, `nodedetail`, the
empty-state hints, cross-workload image history, the namespace palette's per-namespace
counts, and `routetable`'s cross-namespace Gateway resolution. Worse, a namespace switch
would have become a `SwitchContext`-shaped cache rebuild, turning today's instant filter
change (one field write) into a loading flash. It trades away features for the same win
laziness gets for free.

Still true as the default. [§5.5](#55-the-helm-release-cache-scopes-to-the-namespace--done)
is the one kind that had to break it unconditionally, and it pays exactly the price named
above (one cache per namespace *read*, not per namespace that exists).

It is now also available as an **explicit, opt-in mode** rather than a rejected idea —
[§5.6](#56-namespace-scoped-mode---namespace-scoped--done) — for the case this section's
reasoning doesn't cover: an identity with no cluster-wide `list` access at all, only a
namespaced `Role`. A plain `kute` under that identity can't even complete the eager
Namespace/Pod/Node reads this document assumes always succeed; `--namespace-scoped=<ns>`
is what makes it usable, at the cost this section describes, paid deliberately and only when
asked for.

---

## 2. What shipped

| Commit | What it does |
|---|---|
| `df863a8` `fix(kube)` | Stop a slow link reporting a false connection outage |
| `08b85bb` `refactor(kube)` | Drive typed informers and cache reads from one kind table |
| `132f229` `fix(tui)` | Keep loading states tied to the kind actually being viewed |
| `54d5d1e` `perf(kube)` | **Start each kind's informer on first use instead of all at launch** |
| `5428db3` `perf(tui)` | Count kinds in the jump palette without loading them |
| `43202a4` `fix(tui)` | Refresh screens when their secondary resources change |
| `668464d` `perf(kube)` | Drop managedFields from informer caches |
| `914db9e` `chore(scripts)` | Add `scripts/measure-cluster-payload.sh` |
| `8950b6b` `perf(kube)` | List Helm releases without reading every Secret |
| `238170c` `perf(kube)` | Discover custom kinds without downloading their schemas |
| `c62324d` `fix(tui)` | Stop the jump palette hanging the app on a real cluster |
| `9939ab7` `fix(tui)` | Don't report empty data while its cache is still loading |

The last two are written up in [§5.1](#51-crd-discovery--done-238170c) and
[§5.2](#52-helm-releases-read-every-secret--done-8950b6b), where they were first
identified, rather than repeated here.

### df863a8 — the false-outage fix

The `/livez` ping ran on a 2 s interval with a 3 s timeout, sharing one HTTP/2 connection
with the initial LIST storm. On a constrained link it queued behind megabytes of body and
blew its deadline while the link was healthy — the same cluster reported ~200 ms the
moment the sync finished. Every timeout flipped `ConnReconnecting`.

Timeout raised to 10 s, and a *timed-out* probe is forgiven while caches are doing an
initial LIST (bounded at 90 s). Only timeouts: a refused connection, DNS failure, TLS
error or 401 still flips on the first ping, so the 4c unreachable-context screen appears
as promptly as ever. The rule is deliberately blind to the current phase — keying off
"we're still Connected" would let one unlucky watch error early in the sync re-arm the
banner storm behind it.

`internal/kube/health.go`, `probe.go`

### 08b85bb — one kind table

`registerWatches` held a map of 21 informers; `ListRaw` held a parallel 21-arm switch;
a comment asked humans to keep them in sync. Both collapse into one `typedKinds` table
carrying each kind's informer accessor, cache reader, GVR and scope.

**This was a safety fix, not tidying.** Reaching for a factory's `Lister()` *registers*
that informer as a side effect, so `ListRaw` was already registering all 21 — they simply
never started, because `factory.Start` was called exactly once. The first incremental
`factory.Start` would have run every one of them **with no watch-error handler attached**.
Latent while startup was all-or-nothing; a correctness trap the moment it became
incremental.

Also closed two data races the table made obvious: `ListRaw` read `c.factory` unlocked
while `SwitchContext` replaced it under `c.mu`, and `ensureDynamicKind` did the same with
`c.dynFactory`/`c.stopCh`.

`internal/kube/watch.go`, `cluster.go`, `dynamic.go`

### 132f229 — per-kind sync state

Screens asked the cluster whether *every* informer had synced to decide if an empty list
was trustworthy. Informers fill independently, so that conflates two questions — and on a
cluster whose RBAC forbids listing some rarely-watched kind, the aggregate answer is "no"
forever.

`Cluster.KindSynced(kind, namespace)` answers for one kind's own cache (the namespace
argument arrived with §5.6; outside scoped mode it normalizes to `""`). `Synced()` keeps its name but
is now explicitly a connect latch. `KindSynced` reports "settled" for anything nothing will
ever deliver — a stopped cluster, a kind with no informer, and a kind whose watch came back
Forbidden — so a caller gating a spinner on it cannot hang.

`helmAwareLister` had to answer for whichever cache releases were actually decoded from —
at this point the shared Secret cache. Load-bearing: without the mapping the Helm list
flashed "no releases" while Secrets were still filling. (§5.2 later gave releases their own
cache, and this mapping now points there instead.)

`internal/kube/cluster.go`, `internal/app/app.go`, `browse/model.go`,
`nodedetail/model.go`, `internal/tui/namespace.go`

### 54d5d1e — the feature

Connect starts three informers (`Namespace`, `Pod`, `Node`) and waits only on those.
Everything else starts when something first reads it, via `ensureKind` called from
`ListRaw`. The first read of a lazy kind returns an empty cache — which is exactly what
`KindSynced` reports on, so callers hold their loading state and the informer's own change
events bring them back.

Discovered CRDs got the same treatment: discovery used to open an instance watch per
Established CRD whether or not it was ever browsed. (Discovery itself still read whole CRD
objects at this point — [§5.1](#51-crd-discovery--done-238170c) later replaced that too.)

The eager three earn their place by being needed before any navigation, or by having no
reload path if they arrive late (the Pods health strip, Nodes list, node detail and
overview all read Nodes without subscribing to Node changes).

`internal/kube/cluster.go`, `watch.go`, `dynamic.go`, `health.go`

### 5428db3 — counts without loading

The jump palette shows a count per kind and got them by measuring informer caches — so
pressing `g` would have started a watch for every registered kind. The launch stampede,
one keystroke away.

Counts now come from a one-object metadata LIST reading `remainingItemCount` (~2 KB per
kind, no watch, no cache). It works for kinds whose informers have never run, so the long
tail gets a *real* number where it previously showed whatever its unstarted cache held.

Fetched off the Update loop, memoized for 10 s because the palette rebuilds its item list
on every keystroke. A count that hasn't arrived renders as a dim en dash, never a
fabricated zero.

`internal/kube/count.go`, `internal/tui/counts.go`, `goto.go`, `browse/hints.go`

### 43202a4 — secondary-resource refresh

Most lists are built from more than the cache they're named after: Deployments' IMAGE
column compares against ReplicaSets, Ingresses' BACKENDS resolves through Services and
Pods, pod detail's EVENTS grid reads Events and hops through ReplicaSets for the owner.
Each screen subscribed only to the kind it displayed.

**Two pre-existing bugs fixed here**, independent of laziness:

- `routetable` had **no change subscription at all** — it loaded once on open and never
  again, so a backend that moved left BACKENDS wrong until you navigated away and back.
- The synchronous one-shot readers (rollout history, scale prompt) read caches from a
  keypress handler with nowhere to put a loading state.

One `auxKinds` table names each kind's secondary kinds, driving both reload and prefetch.

`browse/auxkinds.go`, `browse/update.go`, `poddetail/update.go`, `routetable/update.go`

### 668464d — managedFields transform

Server-side-apply bookkeeping, routinely a third of an object's stored bytes, read nowhere
in this app. Stripped on the way into every cache, typed and dynamic. Safe for writes
because nothing does read-modify-write through a cache — every mutation is a patch built
from scratch and sent via the clientset.

The YAML view still shows them, fetched live for the single object on screen. A failed
fetch costs the fold and nothing else.

**Caveat worth knowing:** this saves memory and *zero network*, because the transform runs
after the reflector decodes the wire object. At ~1 k pods it's single-digit MB. It's
insurance for larger clusters, not part of the startup win. This commit is the one that
could be dropped without disturbing anything else.

`internal/kube/transform.go`, `yaml.go`, `yamlview/`

---

## 3. Measured effect

Real numbers from `aks-nva-prod-eastus2-02` via `scripts/measure-cluster-payload.sh`
(48 CRDs, 91 pods, 2 nodes, 14 namespaces):

| | before | after |
|---|---|---|
| Typed kinds at connect | 19.0 MB | **1.3 MB** |
| CRD discovery | 2.76 MB | **~15 KB, estimated** (§5.1) |
| **Total** | **≈ 21.8 MB** | **≈ 1.3 MB** |

A ~16× cut.

**One caveat on that table.** The "before" column and the per-kind figures below are
measured. The CRD-discovery "after" is not: it's the metadata list (48 names) plus a
discovery round trip, sized from their payloads rather than observed on the wire. Nor can
`measure-cluster-payload.sh` confirm it — that script measures what `kubectl` pulls, not
what kute pulls. Confirming it properly means watching kute's own traffic (a proxy, or
apiserver audit logs); the practical check is whether first paint against a slow cluster
is now effectively instant.

### Observed on the real cluster

Run against `aks-nva-prod-eastus2-02` over an SSH port-forward — the setup that started
this, where startup used to take 1–2 minutes with the connection banner flapping
throughout. Qualitative, as reported by the user, not instrumented:

- **Pods list: under 2 s.**
- **Namespace palette: opens immediately**, without counts, as designed.
- **Jump palette: instant** — openable *before the pods list has even appeared*, which is
  the point: it no longer depends on any cache being warm. Counts fill in behind it within
  a second or two.

That last one only became true after `c62324d`; the first run of this branch hung the `g`
key for about a minute (see [What the tests missed](#what-the-tests-missed)).

All of the above were subsequently walked and work. One bug came out of it: the Helm
history screen announced "no revisions found — the release secrets may have been deleted"
for a couple of seconds before its data arrived. Three more screens shared the fault
(events, timeline, who-can) — see `9939ab7`, and the invariant it produced: an empty state
is a claim about the cluster, and may not be entered before the caches behind it have
filled.

What each lazy kind now costs only if opened:

```
secrets              310 objects   12.3 MB     ← largest single kind; no longer
                                                 read to list Helm releases
replicasets          284 objects    2.2 MB
controllerrevisions  111 objects    1.4 MB
jobs                  49 objects  358.9 KB
clusterroles         101 objects  370.0 KB
deployments           33 objects  324.4 KB
```

---

## 4. Design decisions worth remembering

- **Auto-start is implicit, inside `ListRaw`.** The alternative — every caller declaring
  its kinds — meant touching ~85 read sites (72 direct `ListRaw` calls plus 13 through
  `resources.List`/`Count`) where a *missed* one fails silently as an empty list.
  Implicit is correct by construction and needs suppression at every breadth-first
  consumer — today the goto palette's resource corpus, the namespace palette's
  per-namespace counts, browse's empty-state hints, and 24a's cross-workload image
  history. All gate on `tui.KindCached` — `KindStarted` ("would this read start nothing
  new?"), then `KindSynced` — never on `KindSynced` alone, which by design reports
  "settled" for a kind no informer backs yet. That gate is only as good as `KindStarted`'s
  answer: until `KindCustomResourceDefinition` with no informer yet was made to read as
  un-started, a goto keystroke still started the full-CRD informer through it. A new
  consumer goes on this list and through `KindCached`; nothing reads breadth-first
  without it.
- **`ensureKind` holds `c.mu` across `factory.Start`.** The two-phase alternative has a
  real interleave: goroutine B's `Start` runs A's just-registered informer before A wires
  its handler, and `SetWatchErrorHandler` then fails outright on a running informer.
- **The connect-grace window re-arms on every informer start**, not at connect. Lazy starts
  move LIST contention mid-session; anchoring at connect would leave "open Secrets on a
  slow link" unforgiven and paint an offline banner over a healthy cluster. It only ever
  applies once this context's link is proven (`Cluster.linkProvenLocked`: `Reached`, or any
  HTTP response below 500 through the transport) — a never-reached cluster's timeouts are an
  outage, so a blackholed endpoint reaches 4c on its first failed probe, and a re-armed
  window can't hold it on the spinner.
- **`KindSynced` reports true for things that will never arrive** (stopped cluster,
  no informer, Forbidden). A spinner gated on it cannot hang.
- **"Is anything still listing" is latched, not swept.** Every reflector's watch error
  funnels through `recordWatchError`, which holds the same `c.mu` that `ListRaw`/
  `ensureKind` take on the render loop's behalf — so the O(all-informers) `HasSynced()`
  sweep behind the connect-grace window used to run inside that hold, per error, with each
  call taking that informer's own lock. With thirty lazy informers started and a connection
  flapping, that is UI jank by construction. `allKindsSynced` caches the *true* answer, so
  the steady state is a bool read. Sound because `HasSynced` is monotonic — a `DeltaFIFO`
  counts an initial population once, so a watch drop and re-LIST never un-syncs a filled
  cache — which leaves a newly started informer as the only thing that can change the
  answer back. Every registration site clears the latch in the same critical section as its
  own write (`noteInformerStartedLocked`); a fourth site that forgets fails *silently*, the
  same shape as the missing decorator forward in
  [What the tests missed](#what-the-tests-missed) — a stale latch closes the grace window
  over exactly the LIST burst it exists to forgive.
- **Counts are never fabricated.** Unknown renders as `–`, not `0`.

### Verification notes

Each of these was checked by breaking the thing it claims to protect, rather than
asserted:

- The race test genuinely fails on the old unlocked `c.factory` read (confirmed by
  reverting the lock and observing `WARNING: DATA RACE`).
- The namespace-palette test genuinely hangs on "loading namespaces…" with the old
  aggregate gate (confirmed by reverting the gate).
- The Helm field-selector test genuinely fails without the filter (confirmed by removing
  `WithTweakListOptions`, giving `field selector "", want "type=helm.sh/release.v1"`).

### What the tests missed

`c62324d` fixed a minute-long freeze on the `g` key that the whole suite was green
through. The cause is worth recording: the two lister decorators in `internal/app` wrap
`resources.RawLister`, which promotes only `ListRaw`, so every optional seam
(`Synced`, `KindSynced`, `CountLive`, …) needs an explicit forward. A missing one is
invisible at runtime — the consumer type-asserts, misses, and takes its fallback path,
which here meant counting informer caches and starting an informer per kind on the update
loop.

Nothing caught it because the palette's own tests use doubles that implement the seams
directly, so the decorator stack was never exercised. Both decorators are now compile-time
asserted against all three seams. **When adding an optional seam, assert the decorators
against it in the same commit** — the fallback path is what makes the omission silent.

That rule was written and immediately under-applied. `c62324d` asserted the three seams
*it* had just fixed without auditing the one that already existed: `ListHelmReleaseSecrets`
predated the assertion block and was never added to it. So the Helm Releases list read
every Secret in the namespace through the shared informer — the exact 12.3 MB above that
the filtered release cache exists to avoid — and left that cache cold until the `h`
keypress started it, mid-session, cluster-wide, with the history screen polling on a
spinner until it synced. **The seam list is not fixed: audit it, don't append to it.**

The demo path is why it survived so long. `fake.Cluster` answers both routes identically
from one seeded map and reports every kind synced immediately, so `--demo` — the path the
goldens exercise — renders the same thing at the same speed down either branch. A fallback
that is invisible in the fake is the shape to watch for.

Three tests were wrong on the first attempt and are worth remembering as traps:

- The first end-to-end transform test **passed while proving nothing**, because the test
  helper built a factory without the transform. Both factories now come from one
  `newTypedFactory` constructor so production and tests can't drift again.
- The first partial-discovery test used a malformed GroupVersion, which makes the fake
  discovery client fail *wholesale* rather than partially — not what partial discovery
  looks like. The matching logic was split into a pure `customKindsFrom` and tested
  directly instead.
- The obvious home for a decorator-stack test is `internal/kube`, next to the recorded-
  actions tests — but `kube` can't import `app`, and a double built in `app` would be
  another stack that isn't the real one. `internal/app/lister_test.go` records reads
  through `newSessionLister`, the same constructor `NewModel` calls, so there is no
  second composition to drift.

---

## 5. Follow-ups

§5.1 and §5.2 were identified here and have since been done; they're kept with their
reasoning intact. §5.3 and §5.4 are standing decisions, not open work. Nothing is
outstanding.

### 5.1 CRD discovery — **done** (`238170c`)

Connect listed every CRD object to learn which custom kinds exist. A CRD carries the full
OpenAPI v3 validation schema for every version it serves, so on the measured cluster that
was 2.76 MB across 48 CRDs, of which **1.8% was anything kute reads** — the largest single
thing a connect pulled once informers went lazy.

```
CRDs:            48
transferred:     2.76 MB
actually used:   50.4 KB  (1.8%)
schema overhead: 2.71 MB  (98.2%)
```

A cache transform could not have fixed this: it runs after the reflector decodes, so the
bytes have already crossed the wire. The server had to stop sending them.

**What it does now.** Two cheap reads that together say the same thing as the CRD list:

- a **metadata-only list of CRDs** — names and nothing else. A CRD's name is exactly
  `<plural>.<group>`, so it identifies precisely which group/resource pairs are custom.
- the **discovery API**, for each one's Kind, served version and scope. A resource the
  server is serving is established by definition, so that condition check comes free.

The name list is what makes the filter exact. Matching on group names instead would need a
hardcoded list of built-in groups, and that list gets Gateway API wrong —
`gateway.networking.k8s.io` is CRD-backed despite the `k8s.io` suffix.

**Printer columns** are the one thing neither read supplies, and they live in the part of a
CRD that makes it huge. They arrive per-kind, on first read of that kind — typically none
per session, occasionally one. Until then a custom kind renders with the neutral NAME/AGE
columns §14a already provides for ("printer columns → columns, Ready-style condition →
status, **else neutral**"), then picks up its real ones: `ListRaw` announces the fetch as a
CRD change, the root shell rebuilds the registry, and `browse` re-reads its descriptor.

The CRD informer itself is no longer started at connect either. The CRDs list (14b) is the
one screen that wants whole CRD objects, so it opens when that screen does.

`internal/kube/dynamic.go`, `discovery.go`, `cluster.go`, `internal/tui/model.go`,
`internal/tui/tasks/browse/update.go`

### 5.2 Helm releases read every Secret — **done** (`8950b6b`)

Releases were decoded from the shared Secret cache, so opening the Helm Releases list or
one release's history pulled every Secret in scope — 12.3 MB on this cluster — to find a
few dozen release revisions.

Releases now have their own Secret informer, filtered server-side to
`type=helm.sh/release.v1`. `Secret.type` is a supported field selector, so the narrowing
happens at the API server and the rest never crosses the wire.

The two caches coexist deliberately: browsing Secrets is a screen about Secrets and still
populates the shared, unfiltered one. The release cache starts on first read like every
other lazy kind and emits its own change events, so the Helm screens no longer depend on an
unrelated Secret changing to notice a new revision. `KindSynced` answers for the release
cache rather than the shared one — the old mapping would have reported "settled" off a
cache the Helm screens never read, flashing "no releases".

`internal/kube/helm.go`, `cluster.go`, `internal/app/app.go`,
`internal/tui/tasks/helmhistory/model.go`, `internal/kube/fake/fake.go`

### 5.3 Known trade-offs accepted, not bugs

- **The fuzzy `g` corpus is smaller from a cold session.** `gotoResourceItems` lists
  *objects*, not counts, so `5428db3`'s count fix didn't cover it; `c62324d` added the skip.
  Typing a pod name into `g` searches only kinds you've opened this session. Mitigation if
  it bites: rank un-started kinds by name-match on the *kind*, so the jump to that list is
  still one keystroke.
- **Opening the CRDs list starts a watch per discovered kind**, to keep its COUNT column
  truthful. Strictly better than the old behaviour (which opened them at connect
  regardless), but the column really wants `CountLive` rather than a cache length.
- **Opening the Secrets list still pulls 12.3 MB.** Inherent — you asked for Secrets.
- **Reading the Helm kind starts the Deployment/StatefulSet/DaemonSet informers.** 18a's
  rollout glyph needs them: helm reports a release `deployed` the moment it has applied
  its manifests, so without reading the workloads a release renders green through the
  whole rollout the user just triggered. Three named kinds, read once per list (not once
  per release — `resources.UnsettledWorkloads` returns only the workloads that are
  moving), and they are three of the smallest kinds on any cluster. Namespaced like every
  other read, and `browse`'s `auxKinds` reloads on them so the arrow clears. The
  annotation lives in the lister decorator, so 19a's overview pays it too when it reads
  releases for its outdated tail — accepted rather than adding a "releases without rollout
  state" seam to let one caller opt out of three of the cheapest kinds there are. Note this is
  the *opposite* trade from §5.2's Secret entry — there the extra cache was pure cost for
  a screen that read nothing from it; here the screen genuinely can't answer without it.
- **Opening one Helm release's diagnostic detail starts Events and only the kinds named
  by that release.** The screen compares Helm's saved hook/manifest inventory with live
  cluster evidence, so those reads are the answer the user explicitly requested. It
  deduplicates `(kind, namespace)` before calling `ListRaw`, gates every scope on
  `KindSynced`/`KindError`, and reloads only on the resulting dynamic kind set. It must
  never loop over the registry: a chart with Service+Deployment+Job may start those three
  caches, not every kind kute knows.

### 5.4 Not done, deliberately

- **No payload instrumentation inside the app.** A TUI has no console to log to, and a
  debug channel for one measurement is a feature nobody asked for.
  `scripts/measure-cluster-payload.sh` answers the question from outside. (This was
  originally about measuring the CRD list before deciding whether to fix it; §5.1 has
  since removed that read entirely.)
- **The managedFields fetch happens on YAML-view open, not on fold-expand.** The collapsed
  line reads `▸ managedFields (212 lines folded)` — it needs the count to render, which
  can't be known without fetching. One small GET on an explicitly-opened screen beat
  changing spec'd copy.

---

### 5.5 The Helm release cache scopes to the namespace — **done**

§5.2 gave releases their own Secret informer, filtered server-side to
`type=helm.sh/release.v1`, and that filter is still the right one. What it doesn't narrow
is *scope*: the informer watched every namespace, so browsing one namespace's releases
pulled every namespace's. Release Secrets carry the release's whole gzipped manifest, so
that is not a rounding error — 8.19 MB cluster-wide against 4 MB for the namespace the
screen was actually showing, on the cluster below.

Over a VPN that difference is the difference between working and not. The cluster-wide
LIST takes 60–90 s at ~130 KB/s, which straddles the API server's 60 s request window: it
dies partway through with `stream error: … INTERNAL_ERROR; received from peer`, the
reflector retries the same doomed request, and the cache never syncs. Reported as a Helm
Releases screen that had been loading for **over 2000 seconds** while
`helm list -n nva-uat` answered in 4 s and the same build over SSH *on the node* opened
instantly — same 8 MB, no VPN in between.

Measured on that cluster, from the workstation:

| read | result |
|---|---|
| cluster-wide informer (what shipped in §5.2) | fails at 62.9 s, never syncs |
| same with `limit=50` | still 62.9 s — RV=0 lists ignore `limit`, served whole from the watch cache |
| informer scoped to `nva-uat` | 255 Secrets in 13.4 s, synced |

So `ensureHelmSecrets` now takes the namespace being read and keys one informer per
namespace ("" still meaning all). `KindSynced(KindHelmRelease, namespace)` answers for the
namespace asked about — the caller passes the one its own read used, since that is the
cache the rows on screen came from. (This section first shipped answering for whichever
scope was read most recently; §5.6 replaced that with the explicit argument every kind now
takes.)

This is the exception to [Why not namespace-scoped informers by default](#why-not-namespace-scoped-informers-by-default),
and it pays that section's price honestly: switching namespaces on the Helm list starts a
second cache and shows a loading state while it fills, where every other kind re-filters
an existing one instantly. It buys a screen that loads at all. The reasoning doesn't
generalise — it applies to a kind whose objects are individually large *and* whose screen
is namespace-scoped in practice. All-namespaces mode still reads cluster-wide: you asked
for every namespace, the same answer §5.3 gives for the Secrets list.

Two related reads went with it. `browse`'s `auxKinds` still listed `KindSecret` under
`KindHelmRelease` from before §5.2, so opening the Helm list *prefetched* the shared
unfiltered cluster-wide Secret cache — reinstating the exact 12.3 MB read §5.2 removed, on
the way into a screen that never touches it, competing for the same saturated link. And
`KindSynced` now reports settled for a cache whose initial LIST keeps failing, with
`KindError` alongside it saying why, because "not synced yet" was true forever here and
CLAUDE.md's promise that a spinner can never hang wasn't being kept. Every screen that
gates an empty state on `KindsSynced` asks `KindsError` next and shows the read failure
instead of claiming the cluster is empty (`browse`, `helmhistory`, `events`, `timeline`,
`whocan`). The stall is a status, not a verdict: the reflector keeps retrying, a success
clears it, and the informer's own change event reloads the screen.

Per-namespace caches accumulate. One is started per namespace actually read — the Helm
list, `h`/`↵` into a release — and none is stopped before the next context switch, so each
namespace visited keeps its watch and its full gzipped revisions in memory until then. The
one way that cost isn't paid twice: once the all-namespaces cache has synced it already holds
every namespace's releases, so a namespaced read that has no cache of its own is served from
it (`Cluster.helmScopeLocked`, which `scopeKeyForLocked` shares so `KindSynced`/`KindError`
answer for the same cache), rather than LISTing the same objects again. Stopping idle
namespaced caches is not done.

`internal/kube/helm.go`, `cluster.go`, `dynamic.go`, `health.go`, `internal/app/app.go`,
`internal/tui/kindsync.go`, `internal/tui/tasks/browse/{aux,update}.go`,
`internal/tui/tasks/{helmhistory,events,timeline,whocan}/update.go`

---

### 5.6 Namespace-scoped mode (`--namespace-scoped`) — **done** (`34a7df7`, e2e coverage `7dfc325`)

[Why not namespace-scoped informers by default](#why-not-namespace-scoped-informers-by-default)
still holds for the default launch. It doesn't hold for an identity that has *no*
cluster-wide `list` access at all — a `Role` bound in one namespace, no `ClusterRole`
anywhere. Under that identity a plain `kute` never gets past connect: the eager
Namespace/Pod/Node reads (§1) are cluster-wide LISTs, every one comes back `Forbidden`, and
`Cluster.Start` used to wait on their raw `HasSynced` forever — a denied cache never syncs,
so a missing capability read as a hung connection.

`--namespace-scoped=<ns>` fixes the actual problem (there is no cluster-wide cache this
identity can read, so stop trying to build one) rather than papering over the symptom. Two
changes, orthogonal to each other:

- **Startup tolerance** (`Cluster.Start`, both modes): "settled" now means *the initial LIST
  completed, or a watch error recorded a permanent Forbidden* — `eagerHasSynced`'s own
  `InformerSynced` funcs check `kindFailed` before `HasSynced`. A transient failure (an
  outage, a slow link) still waits for real; only a denial short-circuits. This half applies
  whether or not scoped mode is on — a cluster-wide launch against a partially-restricted
  identity (Namespace granted, Node denied) benefits identically.
- **Per-namespace caches, opt-in** (`Cluster.cacheScope`, the single place mode is
  consulted): with `SetNamespaceScope` never called, every cache key normalizes to `""` —
  byte-for-byte the behavior this whole document describes, asserted by tests that check the
  fake clientset's recorded actions don't change with the feature compiled in but unused.
  With it called, a namespaced kind gets one informer *per namespace actually read*
  (`kindInformers`/`kindFailed`/`kindStalled`/`dynKinds` are keyed by `(kind, namespace)`,
  not `kind` alone — the same "start on first read, not up front" laziness this document's
  §1 established, just with more distinct caches instead of one). `""` always still means
  the explicit cluster-wide/all-namespaces cache — never silently downgraded — so §5's
  cross-namespace consumers (the overview, `nodedetail`, `routetable`, who-can's
  ClusterRole half) keep working under scoped mode exactly as they do today, at the cost of
  starting that one cache on demand when something actually asks for `""`.
- **Cluster-scoped kinds are unaffected either way**: Node, Namespace, ClusterRole(Binding),
  and the built-in CRD list have exactly one cache regardless of mode — `cacheScope`
  recognizes them (via `typedKinds[kind].clusterScoped`, or a discovered kind's own
  `ClusterScoped` flag) and always answers `""`.
- **KindHelmRelease keeps its own §5.5 scoping**, independent of this mode: its one-cache-
  per-namespace-read shape already existed and is now what scoped mode's own per-kind
  caches copy, not something layered under it — `KindSynced(KindHelmRelease, ns)` reads
  `helmInformers[ns]` directly rather than going through `cacheScope` at all.

Two smaller correctness fixes ride along, both scoped/unscoped-agnostic: a context
generation counter (`Cluster.generation`, bumped on every `Start`) so a watch-error callback
from a context `SwitchContext` already replaced can't write into the new context's
`kindFailed`/`kindStalled` maps (a wider window than before, now that a context can have
several factories instead of one); and `KindSynced`/`KindError`/`KindForbidden` all take an
explicit `namespace` argument instead of answering for "the last read's own scope" — the
implicit-scope shape §5.5 introduced for Helm, generalized and made required everywhere,
since a caller in scoped mode genuinely can be asking about two different caches for the
same kind in the same frame.

`internal/kube/{cluster,watch,dynamic,helm,count,health}.go`, `internal/tui/kindsync.go`,
`internal/app/{app,config,session}.go`, `cmd/kute/main.go`,
`internal/tui/{namespace,goto,model}.go`, `internal/tui/components/palette/palette.go`,
`internal/tui/tasks/browse/{model,update,view,hints}.go`, and one call-site update apiece in
`internal/tui/tasks/{events,timeline,whocan,helmhistory,cronjobdetail,cronjobschedule,
jobattempts,fluxtree,nodedetail}`. End-to-end coverage against a real cluster (a scoped
`kute-team` identity, a lazily-loaded granted kind, an ungranted kind's 403 card, denied
all-namespaces, and returning to a granted namespace) lives in `test/e2e`.

---

### 5.7 TLS cert reads have their own Secret cache — **done**

A TLS Ingress's or Gateway's expiry cell (23a/23b, `routetable`'s `resolveCertExpiry`) and
the cert chain's `secret/<name>` ref (`certchain`) each need one Secret. Both read it with
`ListRaw(KindSecret, ns)`, which in the default mode normalizes to the shared cluster-wide
Secret informer — full payloads, nothing stripped, every Helm release manifest included
(the 8.19 MB of §5.5). Opening one TLS Ingress paid for every Secret in the cluster.

They now read `ListTLSSecrets(ns)`, the §5.5 shape applied to a second type: a Secret
informer filtered server-side to `type=kubernetes.io/tls`, one per namespace actually read
in both modes (keyed by the read's own namespace, never normalized by `cacheScope`). It is
registered under the synthetic key `KindTLSSecret` in `kindInformers`, so watch-error,
stall, Forbidden, health-sweep and Reached bookkeeping cover it with no parallel map, and
its change events carry that key. A screen gates a miss on
`KindSynced`/`KindError`/`KindForbidden(KindTLSSecret, ns)` with the namespace it read —
never `KindSecret`, which answers for the cache this path leaves unstarted. The seam is
optional (`routetable.TLSSecretLister`, `certchain.TLSSecretLister`): both lister
decorators forward it, falling back to the shared cache when nothing filtered sits
underneath, and then answer the `KindTLSSecret` gates from `KindSecret` to match.

The trade: a Secret of some other type that an ingress controller would still accept with a
`tls.crt` key (an Opaque one, say) is invisible to the filtered cache. cert-manager always
writes `kubernetes.io/tls`, and the API rejects a missing `tls.crt`/`tls.key` only on that
type, so it's the type a TLS reference is meant to name — but not the only one that works.
So a miss in the TLS cache falls back to the shared Secret cache **only when that cache is
already started** for the same namespace scope (`tui.KindStartedChecker`'s
`KindStarted(KindSecret, ns)` — the Secrets list was opened, say), and has settled without
an error (`KindsSynced`/`KindsError(KindSecret, ns)`). A lister that can't answer
`KindStarted` counts as not started. There, a Secret of that name with a `tls.crt` key is
used as if it were TLS-typed; one without it reads "not a tls secret"; no Secret at all is a
definitive "secret not found"/"missing", since the shared cache holds every type. The
fallback never starts the shared cache: when it isn't running, the miss keeps the TLS
cache's own gating and its Fail state, but names what was checked — "no kubernetes.io/tls
secret" (routetable), "missing as kubernetes.io/tls" (certchain) — instead of claiming no
Secret of that name exists. Both screens already reload on `KindSecret` events, which only
a started shared cache emits, so a fallback hit stays live without widening anything.

`internal/kube/{tlssecrets,cluster,kinds}.go`, `internal/kube/fake/fake.go`,
`internal/app/app.go`, `internal/tui/tasks/{routetable,certchain}/{load,update}.go`

## 6. CronJobs: a second lazy dependency and a third one-shot read (v0.8.0)

Shipped as part of the v0.8.0 CronJobs rework; recorded here so the lazy-loading rules that
already govern every other kind stay documented rather than only implicit in the code.
Nothing below changes the eager set — Namespace, Pod, Node stay the only informers
`Cluster.Start` waits on.

### CronJob's own informer is lazy like any other kind

`KindCronJob` starts the same way every non-eager kind does: the first `ListRaw(KindCronJob)`
call — triggered by opening the CronJobs list — runs `ensureKind`, which registers and starts
its informer via the shared `typedKinds` table (`internal/kube/watch.go`). Nothing before that
first read touches it.

### Job becomes a required lazy dependency the moment CronJobs opens

CronJobs answers "did the last run pass, when is the next one, is it even enabled" from data
that does not live on the CronJob object alone — the last terminal outcome, the active-run
count, and the retained-history table all come from that CronJob's own associated Jobs.
So `browse`'s CronJob branch also lists `KindJob` — once, cluster/namespace-scoped like any
other kind, not narrowed by cronjob name server-side — and aggregates it client-side into
per-CronJob summaries. **There is no `ListRaw(Job)` per CronJob row**: a screen showing seven
CronJobs issues one Job list, not seven, the same "list primary/related raw objects once, then
aggregate" shape §5.5's Helm release cache and 18a's rollout-glyph read already use for their
own secondary kinds.

`KindCronJob: {KindJob, KindPod}` belongs in `internal/tui/tasks/browse/auxkinds.go` so a Job
or Pod change reloads the list — `auxKinds` is what makes a screen's *secondary* reads reload
on their own watch events instead of going stale (§2's `43202a4`). Both `KindCronJob` and
`KindJob` are required for the list's own Ready/Empty gate (`tui.KindsSynced(lister,
KindCronJob, KindJob)`, checked even when CronJob rows are already present, `KindsError`
checked immediately after) — the anti-hang and no-fabricated-empty-state rules `132f229`/
`9939ab7` established apply here exactly as they do everywhere else: a kind with genuinely
zero associated Jobs must still wait for that cache to settle before the row renders "no
retained runs," and a Job-cache read that settles with an error must show an inline load-error
strip rather than silently rendering the no-history path.

Pod is different in kind from Job here: it is already one of the three eager kinds (§2's
`54d5d1e`), so CronJobs never has to start it, and it is read **best-effort**, not required —
it enriches a failed Job's exit-code/log target and nothing else blocks on it. A Pod that has
already been garbage-collected loses only that enrichment; the Job's own condition
message/reason stays authoritative.

**Opening CronJobs must never make Job or CronJob eager at connect**, and must never poll —
the live NEXT countdown and the strip's own UTC wall clock come from a one-second `tea.Tick`
local to the CronJob screens (list, detail, schedule editor), which only rebuilds
already-fetched summaries into new display strings. It never calls `ListRaw`, metrics, or
discovery, exactly the same boundary the metrics poll (`Conventions`, this file's §4) and
every other UI-only clock in Kute already respects.

### A third one-shot read: cached server version

CronJobs' schedule editor (36d) needs to know whether the connected API server is new enough
to honor `spec.timeZone` before it lets a user rely on setting one — an older server can prune
the field silently rather than reject the write, so guessing wrong is a write that looks
successful and does nothing. `CLAUDE.md`'s Conventions section names the two one-shot reads
that already exist — `CountLive` (palette counts) and 8a's live `managedFields` fetch — as the
only reads exempt from "watch-based updates, not polling." This adds a third of the same
shape: one cached `Discovery().ServerVersion()` call, taken during connect/context-switch
alongside the eager informer start, never repeated on a timer and never re-issued per screen
open.

The result is a tri-state `supported`/`unsupported`/`unknown` capability stored on the stable
cluster/fake context (mirroring how `ConnState` and other connection-scoped facts are held) and
injected into `tasks/cronjobschedule` as a package-local capability interface — the same
pattern every task dependency follows (`Architecture`'s "define the interface you need in the
task package" rule), not a new optional seam bolted onto `resources.RawLister`. Kubernetes
1.27+ reads `supported`; below 1.25 reads `unsupported`; 1.25–1.26 reads `unknown` unless an
already-populated `spec.timeZone` on the object being edited is itself direct evidence that
this particular object's field works, regardless of what the version alone would suggest. An
`unknown`/`unsupported` capability makes the timezone field read-only without fabricating a
cluster version; an already-populated value stays visible and editable-adjacent either way.
Node kubelet versions are deliberately never consulted for this — a mixed-version node pool
says nothing about the control plane's own API version.

## 7. Pod debug: an authoritative fourth one-shot read

Opening DEBUG for a Pod issues one live `SelfSubjectAccessReview`, scoped to the panel's
current mode: `create pods/ephemeralcontainers` for attach or `create pods` for copy. Changing
mode issues a replacement review and stale results are discarded. This is explicit user-driven
work, starts no informer, and is never polled.

The review deliberately does not reuse the cache-local who-can resolver. That resolver can
explain Kubernetes RBAC bindings, but it cannot reproduce every API-server authorizer or know
all groups embedded in an authentication token. Using its partial answer as a launch gate made
behavior depend on whether the four lazy RBAC caches had settled: the first `x` could open and
the second `x` after Escape could be rejected. The server's `SelfSubjectAccessReview` is the
launch gate now; who-can remains an optional explanation after an explicit denial. Review
errors and no-opinion answers fail open with a visible warning, leaving the actual debug request
as the final server-side check.

## 8. Who am I: a fifth one-shot read

Right after the server-version read, connect (and every context switch) issues one
`SelfSubjectReview` — `authentication.k8s.io/v1`, falling back to `v1beta1` only when v1
answers NotFound — and caches the returned username and groups on the cluster
(`Cluster.ReviewedIdentity`, `internal/kube/identity.go`). Who-can (22a) pins that identity as
"(you)", and §33a's Argo sync stamps its username as `operation.initiatedBy.username`. For token, OIDC and
exec-plugin auth the kubeconfig only knows its AuthInfo entry name, which is never an RBAC
subject. The review is a create with an empty body, touches no informer, is cleared on context
switch, and is never retried or re-issued per screen open; any failure leaves it unset and the
kubeconfig identity stands in.

## Running the measurement

```sh
scripts/measure-cluster-payload.sh              # current kubecontext
scripts/measure-cluster-payload.sh --context X  # a specific one
```

Read-only. Needs `kubectl` and `python3`.

## Keeping the numbers true

The figures above were measured once, against one cluster, and a document
cannot fail a build. `internal/kube/payload_budget_test.go` restates the three
that carry the design — connect's payload (§3), the per-namespace Helm scope
(§5.5), and the managedFields strip (§4) — as budgets over a fake clientset's
recorded actions, priced in bytes against fixtures built with the shapes above
(schema-heavy CRDs, manifest-heavy release Secrets, managedFields-heavy pods).

It complements rather than replaces `lazy_test.go`/`helm_secrets_test.go`,
which assert *which* resource was listed, with what selector and in what
scope. A read can keep all three and still get much more expensive; this is
the axis that catches that.
