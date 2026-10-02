// Helm-release-specific browse machinery for 18a (docs/design README.md):
// the p "open this release's pods" shortcut, ↵ diagnostic detail, 'v' values
// (pushes the read-only YAML viewer) and 'h' history (pushes
// tasks/helmhistory's revision rail), and the 'R' rollback verb (shells out
// to the real helm binary, 8b-style friction). Kept in its own file,
// browse's per-concern split convention (like nodes.go/deployments.go/
// forwards.go).
package browse

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/helmrepo"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// ChartIndexReporter is implemented by a lister that also knows the state of
// the local Helm repo cache the LATEST column is answered from. Declared
// here, in the package that consumes it, per the repo's seam convention.
//
// Exported so app can assert its lister decorators against it at compile
// time: like every optional seam, a decorator that forgets to forward it
// still builds — the type assertion below just misses, and the strip
// silently stops caveating a stale answer.
type ChartIndexReporter interface {
	ChartIndexStatus() helmrepo.Status
}

// chartCacheStale is how old the repo cache gets before its age stops being
// a footnote and becomes a warning. Under a day, "run helm repo update" isn't
// news.
const chartCacheStale = 24 * time.Hour

// chartCacheNote is 18a's caveat on the LATEST column's data source: how old
// the local repo cache the column is answered from actually is.
//
// The strip names it in every state — an all-`–` LATEST column must never be
// readable as "everything is up to date", and silence when the cache is fresh
// left no way to tell "checked, and you're current" from "never checked".
// Fresh renders dim; missing or stale turns Theme.Warn and carries the
// command that fixes it.
//
// Resolved as fields rather than a finished string because the view picks a
// form by available width and colors the pieces differently — while the clock
// stays out of the render path.
type chartCacheNote struct {
	// text is the full form ("no helm repo cache", "repo cache 6d old").
	text string
	// short is the narrow-terminal form ("no repo cache", "cache 6d old"),
	// which on an 80-column strip is the only one that fits beside the
	// health counts.
	short string
	// remedy names the command that fixes it ("run helm repo add"), empty
	// while the cache is fresh and there's nothing to fix.
	remedy string
	// warn marks the missing and stale states, which render in Theme.Warn
	// rather than the strip's usual Theme.TextDim.
	warn bool
}

func (n chartCacheNote) empty() bool { return n.text == "" }

// chartCacheNoteFor resolves the note from the lister's repo-cache status.
// The zero value — no note at all — is what a lister that doesn't report one
// gets, which is every kind but Helm releases.
//
// now is passed in because this runs in the load command, not the render
// path (which stays pure: no clock reads).
func chartCacheNoteFor(lister resources.RawLister, now time.Time) chartCacheNote {
	reporter, ok := lister.(ChartIndexReporter)
	if !ok {
		return chartCacheNote{}
	}
	status := reporter.ChartIndexStatus()
	if !status.Configured || status.Repos == 0 {
		return chartCacheNote{
			text:   "no helm repo cache",
			short:  "no repo cache",
			remedy: "run helm repo add",
			warn:   true,
		}
	}
	note := chartCacheNote{
		text:  "repo cache " + chartCacheAge(status.Age(now)) + " old",
		short: "cache " + chartCacheAge(status.Age(now)) + " old",
	}
	if status.Age(now) < time.Minute {
		// --demo's static cache is minted at startup, and a real one right
		// after `helm repo update` is seconds old: "repo cache 0s old" reads
		// like a defect rather than the best case.
		note.text, note.short = "repo cache just updated", "cache fresh"
	}
	if status.Age(now) >= chartCacheStale {
		note.remedy, note.warn = "run helm repo update", true
	}
	return note
}

// chartCacheAge is shortAge with a floor of one minute, so the sub-minute
// bucket ("0s") never reaches a user-facing sentence.
func chartCacheAge(age time.Duration) string {
	if age < time.Minute {
		age = time.Minute
	}
	return shortAge(age)
}

// openReleaseObjects switches to the Pods created by the selected release's
// rendered-manifest workloads. A release name is not a Pod identity: fuzzy
// matching it admits unrelated charts with overlapping names.
func (m *Model) openReleaseObjects(row resources.Row) tea.Cmd {
	release, ok := m.helmReleases[helmReleaseKey(row.Namespace, row.Name)]
	if !ok {
		return nil
	}
	sources := kube.HelmReleasePodSources(release)
	// Helm normally applies into its release namespace, but a rendered
	// manifest may name another one explicitly. Retain the familiar scoped
	// view for the common case; widen only when that is needed to avoid
	// silently omitting an owned Pod.
	for _, source := range sources {
		if source.Namespace != row.Namespace {
			m.namespace = ""
			m.session.Location.Namespace = ""
			break
		}
	}
	_ = m.switchKind(kube.KindPod)
	m.releasePodSources = sources
	m.releasePodView = true
	// resetAndLoad may have restored an ordinary Pods cached view before the
	// release association was installed. It is not a valid preview of this
	// narrower semantic view.
	m.rows, m.visible, m.display = nil, nil, nil
	m.cachedView = false
	m.originKind, m.originName = kube.KindHelmRelease, row.Name
	// switchKind created its load command before the semantic association was
	// installed. Supersede it so the first Pods read cannot briefly use the
	// ordinary unfiltered path.
	m.reloadEpoch++
	m.metricsEpoch++
	cmds := []tea.Cmd{m.load(), m.spinner.Tick}
	if m.pollsMetrics() {
		cmds = append(cmds, m.loadMetricsCmd(m.metricsEpoch), m.scheduleMetricsTick(m.metricsEpoch))
	}
	return tea.Batch(cmds...)
}

// openSelectedHelmValues pushes 18a's 'v' — the selected release's decoded
// values in the read-only YAML viewer. ok is false when values aren't
// wired, nothing's selected, or the release's full data hasn't loaded yet,
// so 'v' stays a no-op rather than pushing a broken screen.
func (m Model) openSelectedHelmValues() (tea.Model, tea.Cmd, bool) {
	if m.openHelmValues == nil || m.kind != kube.KindHelmRelease {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	release, ok := m.helmReleases[helmReleaseKey(row.Namespace, row.Name)]
	if !ok {
		return nil, nil, false
	}
	task, cmd := m.openHelmValues(release, m.width, m.height)
	return task, cmd, task != nil
}

// openSelectedHelmHistory pushes 18a's 'h' — the selected release's full
// revision rail.
func (m Model) openSelectedHelmHistory() (tea.Model, tea.Cmd, bool) {
	if m.openHelmHistory == nil || m.kind != kube.KindHelmRelease {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	task, cmd := m.openHelmHistory(row.Namespace, row.Name, m.width, m.height)
	return task, cmd, task != nil
}

func (m Model) openSelectedHelmDetail() (tea.Model, tea.Cmd, bool) {
	if m.openHelmDetail == nil || m.kind != kube.KindHelmRelease {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil, nil, false
	}
	release, ok := m.helmReleases[helmReleaseKey(row.Namespace, row.Name)]
	if !ok {
		return nil, nil, false
	}
	task, cmd := m.openHelmDetail(release, m.width, m.height)
	return task, cmd, task != nil
}

// beginRollback confirms rolling row back to its previous revision —
// verbs.Rollback's Tier (TierInline, escalated to TierModal in PROD by
// verbs.TierFor — "Rollback inherits 8b friction"). Executes through
// kube.Mutator.HelmRollback like every other write verb.
func (m *Model) beginRollback(row resources.Row) tea.Cmd {
	release := m.helmReleases[helmReleaseKey(row.Namespace, row.Name)]
	toRevision := release.Revision - 1 // 0 once Revision is 1, which HelmRollback/HelmRollbackCommandString both read as "Helm's own default: the previous revision"
	tier := verbs.TierFor(verbs.Rollback, m.isProd())
	return m.actions.Begin(tier, tui.TaskAction{
		ID:    "rollback-" + row.Namespace + "/" + row.Name,
		Label: fmt.Sprintf("Rollback %s to revision %d?", row.Name, toRevision),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindHelmRelease),
			ResourceName: row.Name,
			Namespace:    row.Namespace,
			Verb:         "rollback",
			IsMutating:   true,
			Revision:     toRevision,
		},
	})
}

// rollbackPrompt renders 18a's inline (non-PROD) confirm line — the
// generic actions.Controller.Prompt() plus the exact `helm rollback` command
// that will run (18a: "shell out to helm with a will run line").
func rollbackPrompt(target kube.CommandTarget, scope tui.TaskScope) string {
	// Kept short deliberately: insetChromeLine (tui/chrome.go) drops the
	// whole RightNote rather than truncating it when the keybar line
	// overflows width, so this stays terser than Controller.Prompt()'s own
	// default "Rollback HelmRelease ns/name? (y) confirm (n) cancel" despite
	// carrying more information (the actual command). The --kubeconfig path
	// alone can still overflow it, which is what rollbackPromptShort is for.
	return kube.HelmRollbackCommandString(target, scope.Namespace, scope.ResourceName, scope.Revision)
}

// rollbackPromptShort is the keybar's fallback when rollbackPrompt doesn't
// fit (tui.Keybar.RightNoteShort): the same command with the --kubeconfig
// path elided, so the confirm never loses its will-run line altogether.
func rollbackPromptShort(target kube.CommandTarget, scope tui.TaskScope) string {
	return kube.HelmRollbackCommandSummary(target, scope.Namespace, scope.ResourceName, scope.Revision)
}

// rollbackDetail renders the PROD modal's detail line (delete.go's
// typeNameConfirmModal) — the same "will run" command string, shown in the
// type-the-name modal instead of the keybar's single line.
func rollbackDetail(target kube.CommandTarget, scope tui.TaskScope) string {
	return "will run: " + kube.HelmRollbackCommandString(target, scope.Namespace, scope.ResourceName, scope.Revision)
}
