package browse

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/helmrepo"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/testutil/goldentest"
	"github.com/kute-dev/kute/internal/tui"
)

func helmRelease(namespace, name, chart, chartVersion, appVersion, status string, revision int) *kube.HelmReleaseObject {
	return kube.NewHelmReleaseObject(kube.HelmRelease{
		Namespace: namespace, Name: name, Chart: chart, ChartVersion: chartVersion,
		AppVersion: appVersion, Revision: revision, Status: status,
	})
}

func TestHelmUpdatedSortsByElapsedTime(t *testing.T) {
	now := time.Now()
	newRelease := func(name string, updated time.Time) *kube.HelmReleaseObject {
		release := helmRelease("default", name, name, "1.0.0", "1.0.0", "deployed", 1)
		release.Release.Updated = updated
		return release
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHelmRelease: {
			newRelease("ancient", now.Add(-1176*24*time.Hour)),
			newRelease("recent", now.Add(-time.Hour)),
			newRelease("middle", now.Add(-6*24*time.Hour)),
		},
	}}
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	// UPDATED is column seven and, as a recency column, defaults to newest
	// first. Its rendered values (for this fixture: 1h ago, 6d ago, and
	// 1176d ago) must be compared as durations rather than strings.
	m = step(t, m, tea.KeyPressMsg{Text: "7"})
	if m.sortColumn != 7 || m.sortAsc {
		t.Fatalf("sortColumn=%d sortAsc=%v, want 7/false (newest first)", m.sortColumn, m.sortAsc)
	}
	if want := []string{"recent", "middle", "ancient"}; !equalStrings(displayRowNames(m), want) {
		t.Fatalf("names = %v, want %v (newest first)", displayRowNames(m), want)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "7"})
	if want := []string{"ancient", "middle", "recent"}; !equalStrings(displayRowNames(m), want) {
		t.Fatalf("names = %v, want %v (oldest first)", displayRowNames(m), want)
	}
}

// TestHelmReleaseHealthStripCountsByStatus confirms 18a's health strip
// buckets deployed/pending-*/failed into OK/Warn/Fail per the design's own
// strip example ("3 deployed · 1 pending-upgrade · 1 failed").
func TestHelmReleaseHealthStripCountsByStatus(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHelmRelease: {
			helmRelease("default", "postgresql", "postgresql", "12.1.9", "15.4.0", "deployed", 3),
			helmRelease("default", "redis", "redis", "18.1.5", "7.2.4", "deployed", 2),
			helmRelease("default", "grafana", "grafana", "7.3.0", "10.4.2", "deployed", 1),
			helmRelease("default", "prometheus", "kube-prometheus-stack", "58.2.1", "0.73.0", "pending-upgrade", 2),
			helmRelease("default", "broken-app", "mychart", "1.0.0", "2.1.0", "failed", 2),
		},
	}}
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("expected ready state, got %s (feedback=%q)", m.state, m.feedback)
	}
	strip := plain(m.healthStripLine(m.Theme(), 120))
	for _, want := range []string{"3", "deployed", "1", "pending-upgrade", "failed", "helm.sh/release.v1 secrets"} {
		if !strings.Contains(strip, want) {
			t.Fatalf("health strip %q missing %q", strip, want)
		}
	}
}

// outdatedRelease is a release the local repo cache has a newer chart for.
func outdatedRelease(namespace, name, chart, deployed, available string) *kube.HelmReleaseObject {
	return kube.NewHelmReleaseObject(kube.HelmRelease{
		Namespace: namespace, Name: name, Chart: chart, ChartVersion: deployed,
		AppVersion: "1.0.0", Revision: 3, Status: "deployed",
	}.WithLatest(available, "testrepo", false))
}

// TestOutdatedReleasesCountSeparatelyFromStatus is 18a's central claim about
// the outdated signal: it cross-cuts helm status rather than replacing it. A
// deployed release that is behind its repo has to keep being counted
// "deployed" — the strip is about release health, and stapling outdated-ness
// into it would make a perfectly healthy install look broken.
func TestOutdatedReleasesCountSeparatelyFromStatus(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHelmRelease: {
			outdatedRelease("default", "certs", "cert-manager", "1.14.4", "1.16.2"),
			outdatedRelease("default", "redis", "redis", "18.1.5", "20.1.3"),
			helmRelease("default", "postgresql", "postgresql", "12.1.9", "15.4.0", "deployed", 3),
		},
	}}
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	strip := plain(m.healthStripLine(m.Theme(), 120))
	if !strings.Contains(strip, "3 deployed") {
		t.Errorf("health strip %q lost the deployed count — outdated must not reclassify a healthy release", strip)
	}
	if !strings.Contains(strip, "2 outdated") {
		t.Errorf("health strip %q missing the outdated count", strip)
	}

	// The row keeps its deployed status class and says "behind" in the glyph
	// column alone.
	row, ok := m.selectedRow()
	if !ok {
		t.Fatal("expected a selected row")
	}
	if row.Status != resources.StatusOK {
		t.Errorf("outdated row Status = %v, want %v", row.Status, resources.StatusOK)
	}
	if row.GlyphClass != resources.StatusWarn || row.Glyph != tui.GlyphWarning {
		t.Errorf("outdated row glyph = %q/%v, want %q/%v", row.Glyph, row.GlyphClass, tui.GlyphWarning, resources.StatusWarn)
	}
	if got, want := row.Cells[2], "1.16.2"; got != want {
		t.Errorf("LATEST cell = %q, want %q", got, want)
	}
}

// TestUnknownChartRendersAsUnknown: a chart in no configured repo (locally
// built, OCI, no repo cache at all) must read as an unknown, never as
// "you're current".
func TestUnknownChartRendersAsUnknown(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHelmRelease: {helmRelease("default", "inhouse", "inhouse-chart", "0.3.1", "1.0.0", "deployed", 1)},
	}}
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	row, ok := m.selectedRow()
	if !ok {
		t.Fatal("expected a selected row")
	}
	if got, want := row.Cells[2], "–"; got != want {
		t.Errorf("LATEST cell = %q, want %q", got, want)
	}
	if row.Outdated {
		t.Error("row marked outdated with no version to compare against")
	}
	strip := plain(m.healthStripLine(m.Theme(), 120))
	if strings.Contains(strip, "outdated") {
		t.Errorf("health strip %q claims outdated releases with nothing to compare against", strip)
	}
}

// TestChartCacheNoteCaveatsTheStrip: the LATEST column has a second data
// source with its own trustworthiness, so the strip names how fresh the local
// repo cache is in every state. A missing or stale one is a warning carrying
// the command that fixes it; a fresh one is a plain footnote, because silence
// left no way to tell "checked, and you're current" from "never checked".
func TestChartCacheNoteCaveatsTheStrip(t *testing.T) {
	tests := []struct {
		name       string
		status     helmrepo.Status
		want       string
		wantRemedy string
		wantWarn   bool
	}{
		{"no cache at all", helmrepo.Status{}, "no helm repo cache", "run helm repo add", true},
		{"configured but nothing fetched", helmrepo.Status{Configured: true}, "no helm repo cache", "run helm repo add", true},
		{"stale", helmrepo.Status{Configured: true, Repos: 2, Oldest: time.Now().Add(-6 * 24 * time.Hour)}, "repo cache 6d old", "run helm repo update", true},
		{"a day old is already stale", helmrepo.Status{Configured: true, Repos: 2, Oldest: time.Now().Add(-25 * time.Hour)}, "repo cache 1d old", "run helm repo update", true},
		{"fresh", helmrepo.Status{Configured: true, Repos: 2, Oldest: time.Now().Add(-2 * time.Hour)}, "repo cache 2h old", "", false},
		{"seconds old reads as just updated, not 0s", helmrepo.Status{Configured: true, Repos: 2, Oldest: time.Now()}, "repo cache just updated", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := chartStatusLister{
				fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
					kube.KindHelmRelease: {helmRelease("default", "postgresql", "postgresql", "12.1.9", "15.4.0", "deployed", 3)},
				}},
				status: tt.status,
			}
			session := newSession()
			session.Location.Kind = kube.KindHelmRelease
			m := New(Config{Session: session, Lister: lister})
			m.SetSize(120, 36)
			m = step(t, m, m.Init()())

			note := m.chartCacheNote
			if note.text != tt.want {
				t.Errorf("chartCacheNote.text = %q, want %q", note.text, tt.want)
			}
			if note.remedy != tt.wantRemedy {
				t.Errorf("chartCacheNote.remedy = %q, want %q", note.remedy, tt.wantRemedy)
			}
			if note.warn != tt.wantWarn {
				t.Errorf("chartCacheNote.warn = %v, want %v", note.warn, tt.wantWarn)
			}
			strip := plain(m.healthStripLine(m.Theme(), 120))
			if !strings.Contains(strip, tt.want) {
				t.Errorf("health strip %q missing the caveat %q", strip, tt.want)
			}
			if tt.wantRemedy != "" && !strings.Contains(strip, tt.wantRemedy) {
				t.Errorf("health strip %q names a problem without its fix %q", strip, tt.wantRemedy)
			}
		})
	}
}

// TestChartCacheWarningSurvivesANarrowStrip: padBetween drops a right side
// that doesn't fit rather than truncating it, so the widest thing on that
// side used to take the cache warning down with it — leaving an all-`–`
// LATEST column at 80 columns with nothing saying why. The data source is
// constant and the note varies, so the note outranks it at every width.
func TestChartCacheWarningSurvivesANarrowStrip(t *testing.T) {
	m := goldenHelmModel(t, 120, 36) // four releases, a 6-day-old repo cache
	for _, width := range []int{120, 110, 100, 90, 80} {
		strip := plain(m.healthStripLine(m.Theme(), width))
		if !strings.Contains(strip, "cache 6d old") {
			t.Errorf("width %d: health strip %q dropped the repo-cache warning", width, strip)
		}
	}
	if strip := plain(m.healthStripLine(m.Theme(), 160)); !strings.Contains(strip, "from helm.sh/release.v1 secrets · repo cache 6d old — run helm repo update") {
		t.Errorf("width 160 has room for source and warning both, got %q", strip)
	}
	if strip := plain(m.healthStripLine(m.Theme(), 120)); !strings.Contains(strip, "repo cache 6d old — run helm repo update") || strings.Contains(strip, "helm.sh/release.v1") {
		t.Errorf("width 120: the data source should be the first thing dropped, got %q", strip)
	}
	if strip := plain(m.healthStripLine(m.Theme(), 80)); !strings.Contains(strip, "cache 6d old") {
		t.Errorf("width 80: the short form should survive, got %q", strip)
	}
}

// TestChartCacheNoteWarnsInColor pins the missing/stale note to Theme.Warn
// and the fresh one to the strip's usual Theme.TextDim, in both themes — a
// warning rendered in the same dim as the label beside it isn't one.
func TestChartCacheNoteWarnsInColor(t *testing.T) {
	stale := helmrepo.Status{Configured: true, Repos: 2, Oldest: time.Now().Add(-6 * 24 * time.Hour)}
	fresh := helmrepo.Status{Configured: true, Repos: 2, Oldest: time.Now().Add(-2 * time.Hour)}
	themes := map[string]tui.Theme{"dark": tui.Dark(), "light": tui.Light()}
	for name, theme := range themes {
		t.Run(name, func(t *testing.T) {
			staleStrip := goldentest.Truecolor(helmStripModel(t, stale).healthStripLine(theme, 120))
			wantWarn := goldentest.Truecolor(lipgloss.NewStyle().Foreground(theme.Warn).Render("repo cache 6d old — run helm repo update"))
			if !strings.Contains(staleStrip, wantWarn) {
				t.Errorf("stale cache note not rendered in Theme.Warn\nstrip: %q\nwant substring: %q", staleStrip, wantWarn)
			}
			freshStrip := goldentest.Truecolor(helmStripModel(t, fresh).healthStripLine(theme, 120))
			wantDim := goldentest.Truecolor(lipgloss.NewStyle().Foreground(theme.TextDim).Render("repo cache 2h old"))
			if !strings.Contains(freshStrip, wantDim) {
				t.Errorf("fresh cache note not rendered in Theme.TextDim\nstrip: %q\nwant substring: %q", freshStrip, wantDim)
			}
		})
	}
}

// helmStripModel is a loaded single-release Helm list reporting status as its
// repo-cache state.
func helmStripModel(t *testing.T, status helmrepo.Status) Model {
	t.Helper()
	lister := chartStatusLister{
		fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
			kube.KindHelmRelease: {helmRelease("default", "postgresql", "postgresql", "12.1.9", "15.4.0", "deployed", 3)},
		}},
		status: status,
	}
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	return step(t, m, m.Init()())
}

// chartStatusLister is a fakeLister that also reports repo-cache state — the
// optional ChartIndexReporter seam app's decorator satisfies for real.
type chartStatusLister struct {
	fakeLister
	status helmrepo.Status
}

func (l chartStatusLister) ChartIndexStatus() helmrepo.Status { return l.status }

// TestHelmReleaseFailedStatusCellCarriesReason confirms a failed release's
// STATUS cell renders "failed · <reason>" verbatim (docs/design README.md
// §18a).
func TestHelmReleaseFailedStatusCellCarriesReason(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHelmRelease: {kube.NewHelmReleaseObject(kube.HelmRelease{
			Namespace: "default", Name: "broken-app", Chart: "mychart", ChartVersion: "1.0.0",
			Revision: 2, Status: "failed", StatusReason: "hook timeout",
		})},
	}}
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	row, ok := m.selectedRow()
	if !ok {
		t.Fatal("expected a selected row")
	}
	if got, want := row.Cells[5], "failed · hook timeout"; got != want {
		t.Fatalf("STATUS cell = %q, want %q", got, want)
	}
}

// TestHelmPodsTracksItsOrigin confirms p preserves the release-to-Pods
// breadcrumb/back-navigation relationship after Enter became diagnostics.
func TestHelmPodsOpensFilteredPods(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHelmRelease: {helmRelease("default", "postgresql", "postgresql", "12.1.9", "15.4.0", "deployed", 3)},
		kube.KindPod:         {pod("default", "postgresql-0")},
	}}
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "p"})
	if m.kind != kube.KindPod || m.filterInput.Value() != "" {
		t.Fatalf("expected associated Pods with no user filter, got kind=%s filter=%q", m.kind, m.filterInput.Value())
	}
	if m.originName != "postgresql" || m.originKind != kube.KindHelmRelease {
		t.Fatalf("expected origin set to the release, got kind=%s name=%q", m.originKind, m.originName)
	}
	if got := displayRowNames(m); len(got) != 0 {
		t.Fatalf("release with no Pod-producing manifest objects showed Pods: %v", got)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "esc"})
	if m.kind != kube.KindHelmRelease {
		t.Fatalf("expected esc to switch back to Helm Releases, got %s", m.kind)
	}
}

// TestHelmPodsEscRestoresNamespaceAfterWidening: p on a release whose
// manifest names another namespace widens the Pods view to all namespaces,
// and esc must put the user's own namespace back — not leave the session
// (and the Helm list's release informer) cluster-wide — and reselect the
// release in its own namespace, not a same-named one elsewhere.
func TestHelmPodsEscRestoresNamespaceAfterWidening(t *testing.T) {
	release := helmRelease("default", "shop", "shop", "1.0.0", "1.0.0", "deployed", 1)
	release.Release.Manifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: shop-worker
  namespace: other
`
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHelmRelease: {release},
	}}
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "p"})
	if m.kind != kube.KindPod || m.namespace != "" {
		t.Fatalf("expected the release Pods widened to all namespaces, got kind=%s ns=%q", m.kind, m.namespace)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "esc"})
	if m.kind != kube.KindHelmRelease || m.namespace != "default" || session.Location.Namespace != "default" {
		t.Fatalf("after esc: kind=%s ns=%q session ns=%q, want HelmRelease in default", m.kind, m.namespace, session.Location.Namespace)
	}
	if m.pendingSelect.name != "" {
		t.Fatalf("expected the release reselected once rows landed, pendingSelect still %+v", m.pendingSelect)
	}
	if row, ok := m.selectedRow(); !ok || row.Name != "shop" || row.Namespace != "default" {
		t.Fatalf("selected row = %+v (ok=%v), want default/shop", row, ok)
	}
}

func TestEnterOnHelmReleaseShowsOnlyManifestOwnedPods(t *testing.T) {
	controller := true
	release := helmRelease("default", "aim-bp-app", "app", "1.0.0", "1.0.0", "deployed", 1)
	release.Release.Manifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: aim-bp-app
`
	ownedRS := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "aim-bp-app-6bdbbbd5fd", Namespace: "default", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "aim-bp-app", Controller: &controller}}}}
	unrelatedRS := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "aim-bp-preference-survey-app-5947cfcf64", Namespace: "default", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "aim-bp-preference-survey-app", Controller: &controller}}}}
	ownedPod := pod("default", "aim-bp-app-6bdbbbd5fd-sgrh5")
	ownedPod.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: ownedRS.Name, Controller: &controller}}
	unrelatedPod := pod("default", "aim-bp-preference-survey-app-5947cfcf64-f72h9")
	unrelatedPod.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: unrelatedRS.Name, Controller: &controller}}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHelmRelease: {release},
		kube.KindReplicaSet:  {ownedRS, unrelatedRS},
		kube.KindPod:         {ownedPod, unrelatedPod},
	}}
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m = step(t, m, tea.KeyPressMsg{Text: "p"})

	if m.filterInput.Value() != "" {
		t.Fatalf("release association leaked into user filter: %q", m.filterInput.Value())
	}
	if got := displayRowNames(m); !equalStrings(got, []string{ownedPod.Name}) {
		t.Fatalf("release Pods = %v, want [%s]", got, ownedPod.Name)
	}
}

func TestEnterOnHelmReleaseOpensDiagnostics(t *testing.T) {
	release := helmRelease("default", "postgresql", "postgresql", "12.1.9", "15.4.0", "pending-install", 1)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindHelmRelease: {release}}}
	var got kube.HelmRelease
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	m := New(Config{Session: session, Lister: lister, OpenHelmDetail: func(release kube.HelmRelease, _, _ int) (tea.Model, tea.Cmd) {
		got = release
		return stubTask{}, nil
	}})
	m = step(t, m, m.Init()())
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("Enter returned %T, want diagnostic task", updated)
	}
	if got.Name != "postgresql" || got.Status != "pending-install" {
		t.Fatalf("detail received %+v", got)
	}
}

// TestHelmReleaseValuesAndHistoryPushTasks confirms 'v'/'h' push the wired
// Open funcs with the loaded release's full data.
func TestHelmReleaseValuesAndHistoryPushTasks(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHelmRelease: {helmRelease("default", "postgresql", "postgresql", "12.1.9", "15.4.0", "deployed", 3)},
	}}
	var gotValuesRelease kube.HelmRelease
	var gotHistoryNS, gotHistoryName string
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	m := New(Config{
		Session: session, Lister: lister,
		OpenHelmValues: func(release kube.HelmRelease, w, h int) (tea.Model, tea.Cmd) {
			gotValuesRelease = release
			return stubTask{}, nil
		},
		OpenHelmHistory: func(namespace, name string, w, h int) (tea.Model, tea.Cmd) {
			gotHistoryNS, gotHistoryName = namespace, name
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "v"})
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected 'v' to push the values stub task, got %T", updated)
	}
	if gotValuesRelease.Name != "postgresql" || gotValuesRelease.Revision != 3 {
		t.Fatalf("OpenHelmValues got %+v", gotValuesRelease)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Text: "h"})
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected 'h' to push the history stub task, got %T", updated)
	}
	if gotHistoryNS != "default" || gotHistoryName != "postgresql" {
		t.Fatalf("OpenHelmHistory got ns=%q name=%q", gotHistoryNS, gotHistoryName)
	}
}

// TestRollbackInlineConfirmNonProd confirms 'R' on a Helm release shows the
// inline y/N confirm (non-prod) and executes through kube.Mutator.
// HelmRollback on 'y' — "Rollback inherits 8b friction" (docs/design
// README.md §18a).
func TestRollbackInlineConfirmNonProd(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHelmRelease: {helmRelease("default", "postgresql", "postgresql", "12.1.9", "15.4.0", "deployed", 3)},
	}}
	mut := &fakeHelmMutator{}
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	if !m.actions.Active() {
		t.Fatal("expected a pending confirm after 'R'")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if mut.namespace != "default" || mut.name != "postgresql" || mut.revision != 2 {
		t.Fatalf("HelmRollback called with ns=%q name=%q rev=%d, want default/postgresql/2", mut.namespace, mut.name, mut.revision)
	}
}

// TestRollbackInProdRequiresTypedName: the Releases list's PROD rollback is
// the type-the-release-name modal, not a one-key y (§18a: "Rollback
// inherits 8b friction").
func TestRollbackInProdRequiresTypedName(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHelmRelease: {helmRelease("default", "postgresql", "postgresql", "12.1.9", "15.4.0", "deployed", 3)},
	}}
	mut := &fakeHelmMutator{}
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	session.Config = config.Config{ProdContexts: []string{session.Location.Context}}
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	view := plain(m.Render())
	if !strings.Contains(view, `type "postgresql" to confirm`) || !strings.Contains(view, "helm rollback postgresql 2") {
		t.Fatalf("expected the type-the-name modal with its will-run line in PROD:\n%s", view)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if mut.name != "" {
		t.Fatal("expected 'y' + enter not to roll back in PROD")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "esc"})
	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	for _, r := range "postgresql" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if mut.namespace != "default" || mut.name != "postgresql" || mut.revision != 2 {
		t.Fatalf("HelmRollback called with ns=%q name=%q rev=%d, want default/postgresql/2", mut.namespace, mut.name, mut.revision)
	}
}

// TestRollbackInlineConfirmKeepsWillRunLineWithLongKubeconfig: the
// Releases list's own inline confirm falls back to the kubeconfig-elided
// command rather than losing its will-run line when the full one overflows
// the keybar (helmhistory has the same regression test).
func TestRollbackInlineConfirmKeepsWillRunLineWithLongKubeconfig(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHelmRelease: {helmRelease("default", "postgresql", "postgresql", "12.1.9", "15.4.0", "deployed", 3)},
	}}
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	session.Cluster = &kube.Cluster{}
	session.Kubeconfig = "/tmp/TestHelmRollbackCreatesANewVisibleRevision1234567890/001/proxy.kubeconfig"
	m := New(Config{Session: session, Lister: lister, Mutator: &fakeHelmMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m = step(t, m, tea.KeyPressMsg{Text: "R"})

	frame := plain(m.Render())
	if want := "helm rollback postgresql 2 -n default --kube-context microk8s-cluster …"; !strings.Contains(frame, want) {
		t.Fatalf("confirm frame lacks the will-run line %q:\n%s", want, frame)
	}
}

// fakeHelmMutator is a minimal kube.Mutator stub recording HelmRollback
// calls — every other method is a no-op success.
type fakeHelmMutator struct {
	namespace, name string
	revision        int
}

func (f *fakeHelmMutator) DeleteResource(_ context.Context, _ kube.ResourceKind, _, _ string) error {
	return nil
}
func (f *fakeHelmMutator) DeleteResourceForced(_ context.Context, _ kube.ResourceKind, _, _ string) error {
	return nil
}
func (f *fakeHelmMutator) RolloutRestart(_ context.Context, _ kube.ResourceKind, _, _ string) error {
	return nil
}
func (f *fakeHelmMutator) Cordon(_ context.Context, _ string, _ bool) error { return nil }
func (f *fakeHelmMutator) Drain(_ context.Context, _ string) (int, error)   { return 0, nil }
func (f *fakeHelmMutator) Scale(context.Context, kube.ResourceKind, string, string, int32) error {
	return nil
}
func (f *fakeHelmMutator) SetImage(context.Context, kube.ResourceKind, string, string, string, string, bool) error {
	return nil
}
func (f *fakeHelmMutator) SetResources(context.Context, kube.ResourceKind, string, string, string, kube.ResourceEdits, bool) error {
	return nil
}
func (f *fakeHelmMutator) PatchMeta(context.Context, kube.ResourceKind, string, string, bool, string, string, bool) error {
	return nil
}
func (f *fakeHelmMutator) PatchSecretData(context.Context, string, string, string, string, bool) error {
	return nil
}
func (f *fakeHelmMutator) PatchConfigMapData(context.Context, string, string, string, string, bool) error {
	return nil
}

func (f *fakeHelmMutator) SetFluxSuspend(_ context.Context, kind kube.ResourceKind, namespace, name string, suspend bool) error {
	return nil
}

func (f *fakeHelmMutator) RequestFluxReconcile(_ context.Context, kind kube.ResourceKind, namespace, name string) error {
	return nil
}
func (f *fakeHelmMutator) RequestArgoRefresh(_ context.Context, kind kube.ResourceKind, namespace, name string) error {
	return nil
}
func (f *fakeHelmMutator) RequestArgoSync(_ context.Context, kind kube.ResourceKind, namespace, name, revision string) error {
	return nil
}
func (f *fakeHelmMutator) RenewCertificate(_ context.Context, namespace, name string) error {
	return nil
}
func (f *fakeHelmMutator) RetryJob(_ context.Context, namespace, name, newName, creator string, at time.Time) error {
	return nil
}
func (f *fakeHelmMutator) ReplaceJob(_ context.Context, namespace, name string) error {
	return nil
}
func (f *fakeHelmMutator) SetJobSuspend(_ context.Context, namespace, name string, suspend bool) error {
	return nil
}
func (f *fakeHelmMutator) TriggerCronJob(_ context.Context, namespace, name, newJobName, creator string, at time.Time) error {
	return nil
}
func (f *fakeHelmMutator) SetCronJobSuspend(_ context.Context, namespace, name string, suspend bool, resourceVersion string, currentGeneration int64, at time.Time) error {
	return nil
}
func (f *fakeHelmMutator) SetCronJobSchedule(_ context.Context, namespace, name string, edit kube.CronJobScheduleEdit) (kube.CronJobScheduleResult, error) {
	return kube.CronJobScheduleResult{}, nil
}
func (f *fakeHelmMutator) HelmRollback(_ context.Context, namespace, name string, revision int) error {
	f.namespace, f.name, f.revision = namespace, name, revision
	return nil
}
func (f *fakeHelmMutator) RolloutUndo(context.Context, string, string, int) error { return nil }

// helmCountingLister counts ListRaw(HelmRelease) calls only: the other kinds
// a load may read (workload caches for hints, Nodes) are not what this pins.
type helmCountingLister struct {
	fakeLister
	helmReads int
}

func (l *helmCountingLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	if kind == kube.KindHelmRelease {
		l.helmReads++
	}
	return l.fakeLister.ListRaw(ctx, kind, namespace)
}

// TestHelmReleaseLoadReadsReleasesOnce pins review 08 H2: one load reads the
// release list once and builds both the rows and the v/h/R/↵/p map from it.
// Every ListRaw(HelmRelease) decodes release Secrets and re-reads the three
// workload caches for the rollout glyph, and the list reloads on every
// workload change in scope, so a second read for the map doubled the cost
// of every rollout.
func TestHelmReleaseLoadReadsReleasesOnce(t *testing.T) {
	lister := &helmCountingLister{fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindHelmRelease: {
			helmRelease("default", "postgresql", "postgresql", "12.1.9", "15.4.0", "deployed", 3),
			helmRelease("default", "redis", "redis", "18.1.5", "7.2.4", "deployed", 2),
		},
	}}}
	session := newSession()
	session.Location.Kind = kube.KindHelmRelease
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	if lister.helmReads != 1 {
		t.Fatalf("one load issued %d ListRaw(HelmRelease) calls, want 1", lister.helmReads)
	}
	if len(m.rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(m.rows))
	}
	if got := m.helmReleases[helmReleaseKey("default", "postgresql")]; got.Revision != 3 || got.Chart != "postgresql" {
		t.Fatalf("helmReleases[postgresql] = %+v, want revision 3 of chart postgresql", got)
	}
}
