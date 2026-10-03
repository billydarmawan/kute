// Package helmrepo reads the user's *local* Helm repository cache — the
// `repositories.yaml` config plus the `<repo>-index.yaml` files `helm repo
// update` writes — and answers one question: what is the newest version of
// this chart available anywhere the user has configured?
//
// That is the whole of docs/design README.md §18a's outdated-release
// signal. It is deliberately a disk read and nothing else: no network on a
// browse path, and no helm binary on a read path, so 18a's "browsing needs
// no helm binary" rule holds (HelmRollback stays the only shell-out). The
// price is that freshness is whenever the user last ran `helm repo update`,
// which is why Status carries the cache's age for the UI to say out loud.
//
// Like internal/update and internal/kube it is a leaf package — nothing here
// imports the TUI or the cluster layer — so it is testable against a temp
// dir with no cluster and no UI.
package helmrepo

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kute-dev/kute/internal/semver"
)

// Chart is what a repo cache knows about one chart name.
type Chart struct {
	// Version is the newest non-prerelease version offered for this chart
	// (`helm search repo`'s own default answer). A chart that only ever
	// published prereleases falls back to the newest of those.
	Version string
	// Repo is the repository name Version came from ("bitnami").
	Repo string
	// Ambiguous marks a chart name served by more than one configured repo
	// with differing newest versions, where kute could not tell which one
	// the release actually came from. The UI says so rather than presenting
	// a guess as fact — a release Secret doesn't record its source repo.
	Ambiguous bool
}

// Index is a resolved view of every configured repo's cached index: chart
// name → each repo's newest offering of it. It is small (a few KB)
// regardless of how large the index files were, because parsing keeps only
// this.
//
// Candidates are kept per repo rather than collapsed to one winner because
// a shared chart name across repos usually means two *different* charts, not
// two copies of one: fluent/fluent-bit is at 0.57.x while grafana/fluent-bit
// is at 2.x. Collapsing to "the highest" would report a perfectly current
// fluent/fluent-bit as four majors behind — a false alarm, which is the one
// failure this feature cannot afford. LatestFor resolves against the
// deployed version instead.
type Index struct {
	charts map[string][]Chart
	status Status
}

// Status describes where the answers came from and how stale they are —
// everything the 18a strip needs to caveat itself.
type Status struct {
	// Configured is false when there is no repositories.yaml at all, or it
	// lists no repos. Every Latest then misses, and the UI must say "no helm
	// repo cache" rather than letting empty results read as "up to date".
	Configured bool
	// Repos is how many configured repos had a readable cached index.
	Repos int
	// Charts is how many distinct chart names those indexes offered.
	Charts int
	// Oldest is the modification time of the *stalest* cached index that was
	// read — the honest answer to "how old is this data", since one repo
	// left behind makes the whole answer partly stale. Zero when nothing was
	// read.
	Oldest time.Time
}

// Age reports how old the stalest cached index is as of now. Callers pass
// the clock rather than reading it, so this stays usable from a pure render
// path's caller (the model computes the note when it loads, not when it
// draws).
func (s Status) Age(now time.Time) time.Duration {
	if s.Oldest.IsZero() {
		return 0
	}
	return now.Sub(s.Oldest)
}

// Latest returns the cache's best single answer for chart with no deployed
// version to disambiguate against — the highest candidate, flagged Ambiguous
// when the repos disagree. Prefer LatestFor wherever the current version is
// known, which on kute's own paths is everywhere.
//
// A miss is an unknown — never evidence that a release is current.
func (i Index) Latest(chart string) (Chart, bool) {
	return i.LatestFor(chart, "")
}

// LatestFor returns the newest version of chart that plausibly continues
// the series current is on.
//
// With one candidate, that's simply it. With several — the same chart name
// served by unrelated repos — candidates sharing current's major version
// win, because that is the series the release is actually tracking. If none
// does, the highest candidate is returned but flagged Ambiguous, so the UI
// can mark the answer as needing a human rather than asserting it.
func (i Index) LatestFor(chart, current string) (Chart, bool) {
	candidates := i.charts[chart]
	switch len(candidates) {
	case 0:
		return Chart{}, false
	case 1:
		return candidates[0], true
	}

	if current != "" {
		major := semver.Major(current)
		var matched []Chart
		for _, c := range candidates {
			if semver.Major(c.Version) == major {
				matched = append(matched, c)
			}
		}
		if len(matched) > 0 {
			best := matched[0] // candidates are sorted newest-first
			best.Ambiguous = len(matched) > 1 && matched[0].Version != matched[1].Version
			return best, true
		}
	}

	best := candidates[0]
	best.Ambiguous = true
	return best, true
}

// Status describes the cache this Index was built from.
func (i Index) Status() Status { return i.status }

// Loader locates and parses the repo cache. The zero value discovers Helm's
// own default paths; tests set both fields to a temp dir.
type Loader struct {
	// ConfigPath is the repositories.yaml path. Empty = discovered.
	ConfigPath string
	// CachePath is the directory holding <repo>-index.yaml. Empty =
	// discovered.
	CachePath string
}

// repositoriesFile is the subset of repositories.yaml kute reads.
type repositoriesFile struct {
	Repositories []struct {
		Name string `yaml:"name"`
	} `yaml:"repositories"`
}

// indexFile is the subset of a repo's index.yaml kute reads. Naming only
// the version field does *not* make a full yaml.Unmarshal cheap: yaml.v3
// builds the whole node tree before decoding, so a 6 MB index still pays
// ~110 MB and millions of allocations for descriptions, digests, keywords
// and download URLs that are ~95% of the bytes and none of the answer.
// readIndex therefore decodes it through reduceIndex first.
type indexFile struct {
	Entries map[string][]struct {
		Version string `yaml:"version"`
	} `yaml:"entries"`
}

// Load reads the config and every cached index it names.
//
// A missing config or cache directory is not an error: it yields an empty
// Index whose Status reports Configured=false. Only a config that exists but
// can't be parsed is worth an error, and even then the caller is free to use
// the empty Index — no read path in kute should fail because the user has no
// Helm setup.
func (l Loader) Load() (Index, error) {
	idx := Index{charts: map[string][]Chart{}}

	configPath := l.ConfigPath
	if configPath == "" {
		configPath = defaultConfigPath()
	}
	cachePath := l.CachePath
	if cachePath == "" {
		cachePath = defaultCachePath()
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return idx, nil
		}
		return idx, fmt.Errorf("read %s: %w", configPath, err)
	}
	var cfg repositoriesFile
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return idx, fmt.Errorf("parse %s: %w", configPath, err)
	}
	if len(cfg.Repositories) == 0 {
		return idx, nil
	}
	idx.status.Configured = true

	for _, repo := range cfg.Repositories {
		if !safeRepoName(repo.Name) {
			continue
		}
		path := filepath.Join(cachePath, repo.Name+"-index.yaml")
		info, err := os.Stat(path)
		if err != nil {
			// A configured repo that has never been fetched, or was fetched
			// under a different cache dir. Every other repo still counts.
			continue
		}
		entries, err := readIndex(path)
		if err != nil {
			// A truncated or half-written index (helm repo update killed
			// mid-write) must not blank out the repos that did parse.
			continue
		}
		idx.status.Repos++
		if idx.status.Oldest.IsZero() || info.ModTime().Before(idx.status.Oldest) {
			idx.status.Oldest = info.ModTime()
		}
		idx.merge(repo.Name, entries)
	}
	idx.sortCandidates()
	idx.status.Charts = len(idx.charts)
	return idx, nil
}

// merge folds one repo's entries into the index as additional candidates.
func (i *Index) merge(repo string, entries map[string]string) {
	for name, version := range entries {
		i.charts[name] = append(i.charts[name], Chart{Version: version, Repo: repo})
	}
}

// sortCandidates orders every chart's candidates newest-first (repo name
// breaking ties) so LatestFor can read the best match off the front and two
// runs over the same cache always answer identically.
func (i *Index) sortCandidates() {
	for name, candidates := range i.charts {
		slices.SortStableFunc(candidates, func(a, b Chart) int {
			return cmp.Or(
				-semver.Compare(a.Version, b.Version),
				cmp.Compare(a.Repo, b.Repo),
			)
		})
		i.charts[name] = candidates
	}
}

// safeRepoName reports whether name can be joined onto the cache dir as a
// single path element.
//
// The name comes out of the user's own repositories.yaml, and filepath.Join
// *cleans* its result rather than confining it — a repo named "../../etc"
// would resolve to a file outside the Helm cache entirely. Helm itself never
// writes such a name, so anything that looks like a path is a config this code
// should decline to follow rather than guess at.
func safeRepoName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`) && !strings.ContainsRune(name, os.PathSeparator)
}

// readIndex parses one index.yaml down to chart name → newest version.
func readIndex(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f indexFile
	reduced, ok := reduceIndex(raw)
	if !ok || yaml.Unmarshal(reduced, &f) != nil {
		// Not the block layout helm writes (JSON, flow style, anchors, a
		// half-written file): the full decode is the authority, including
		// on whether the file parses at all.
		f = indexFile{}
		if err := yaml.Unmarshal(raw, &f); err != nil {
			return nil, err
		}
	}
	out := make(map[string]string, len(f.Entries))
	for name, versions := range f.Entries {
		if best := newestVersion(versions); best != "" {
			out[name] = best
		}
	}
	return out, nil
}

// reduceIndex cuts an index.yaml in helm's own block layout down to the one
// field kute reads, as a much smaller YAML document of the same shape:
//
//	entries:
//	  <chart>:
//	  - version: <raw value>
//
// It is a line walk, not a parser, so it is strict about the layout it
// accepts: a chart key at one indent, its list items' dashes at another,
// each item's fields two columns past its dash. Any line it can't place —
// a top-level flow style, a tab, an indent between those columns — reports
// !ok and the caller decodes the original in full. Values are copied
// verbatim, so quoting and escapes are still the YAML decoder's to read.
func reduceIndex(raw []byte) ([]byte, bool) {
	var out strings.Builder
	out.Grow(len(raw) / 16)
	inEntries, seenEntries := false, false
	keyIndent, dashIndent := -1, -1
	chartOpen, chartItems := false, 0
	// The current list item's version, written once the item ends — so a
	// second version key on one item is caught rather than silently kept.
	itemOpen, itemVersion := false, ""
	closeItem := func() {
		if itemOpen {
			if itemVersion == "" {
				out.WriteString("  - {}\n")
			} else {
				out.WriteString("  - version:" + itemVersion + "\n")
			}
		}
		itemOpen, itemVersion = false, ""
	}
	closeChart := func() {
		closeItem()
		if chartOpen && chartItems == 0 {
			out.WriteString(" []\n")
		}
		chartOpen, chartItems = false, 0
	}
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimRight(line, "\r\n")
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.ContainsRune(line, '\t') {
			return nil, false
		}
		indent := len(line) - len(trimmed)
		if indent == 0 {
			closeChart()
			if trimmed == "---" || trimmed == "..." || strings.HasPrefix(trimmed, "{") {
				return nil, false
			}
			inEntries = strings.HasPrefix(trimmed, "entries:")
			if inEntries {
				if seenEntries || strings.TrimSpace(strings.TrimPrefix(trimmed, "entries:")) != "" {
					return nil, false
				}
				seenEntries = true
				out.WriteString("entries:\n")
			}
			continue
		}
		if !inEntries {
			continue
		}
		if keyIndent < 0 {
			keyIndent = indent
		}
		isDash := strings.HasPrefix(trimmed, "- ")
		switch {
		case indent == keyIndent && !isDash:
			// A chart name: "<name>:" with nothing after it.
			if !strings.HasSuffix(trimmed, ":") {
				return nil, false
			}
			closeChart()
			out.WriteString("  " + trimmed)
			chartOpen = true
		case isDash && indent >= keyIndent && (dashIndent < 0 || indent == dashIndent):
			if !chartOpen {
				return nil, false
			}
			closeItem()
			dashIndent = indent
			if chartItems == 0 {
				out.WriteString("\n")
			}
			chartItems++
			itemOpen = true
			if v, ok := strings.CutPrefix(trimmed[2:], "version:"); ok {
				itemVersion = v
			}
		case dashIndent >= 0 && indent == dashIndent+2:
			if !itemOpen {
				return nil, false
			}
			if v, ok := strings.CutPrefix(trimmed, "version:"); ok {
				if itemVersion != "" {
					return nil, false
				}
				itemVersion = v
			}
		case dashIndent >= 0 && indent > dashIndent+2:
			// A nested field (dependencies, maintainers, a block scalar's
			// body): never an item's own version.
		default:
			return nil, false
		}
	}
	closeChart()
	if !seenEntries {
		return nil, false
	}
	return []byte(out.String()), true
}

// newestVersion picks a chart's newest release, preferring stable versions
// the way `helm search repo` does: a prerelease only wins if the chart has
// published nothing else. The file's own ordering is never trusted — an
// index assembled from several `helm repo index --merge` runs isn't sorted.
func newestVersion(versions []struct {
	Version string `yaml:"version"`
}) string {
	best, bestPre := "", ""
	for _, v := range versions {
		version := strings.TrimSpace(v.Version)
		if version == "" {
			continue
		}
		if semver.IsPrerelease(version) {
			if bestPre == "" || semver.IsNewer(bestPre, version) {
				bestPre = version
			}
			continue
		}
		if best == "" || semver.IsNewer(best, version) {
			best = version
		}
	}
	if best != "" {
		return best
	}
	return bestPre
}

// defaultConfigPath is Helm's own repositories.yaml location: the
// HELM_REPOSITORY_CONFIG override, else the platform config dir.
func defaultConfigPath() string {
	if p := os.Getenv("HELM_REPOSITORY_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(helmConfigDir(), "repositories.yaml")
}

// defaultCachePath is Helm's own repository cache directory: the
// HELM_REPOSITORY_CACHE override, else the platform cache dir.
func defaultCachePath() string {
	if p := os.Getenv("HELM_REPOSITORY_CACHE"); p != "" {
		return p
	}
	return filepath.Join(helmCacheDir(), "repository")
}

// helmConfigDir/helmCacheDir mirror helm's own helmpath resolution
// (pkg/helmpath/lazypath.go: HELM_*_HOME, then XDG, then the platform
// default) rather than Go's os.UserConfigDir/os.UserCacheDir, which disagree
// with helm on both macOS — helm puts its config under ~/Library/Preferences,
// not ~/Library/Application Support — and Windows, where helm uses %APPDATA%
// and %TEMP% rather than %LOCALAPPDATA%. helm is not a dependency here, so
// these are reimplemented; keep them in step with lazypath_windows.go and
// lazypath_darwin.go upstream.
//
// The one asymmetry is helm's, not ours: a HELM_*_HOME override is used
// verbatim, while an XDG or default base gets "helm" appended.
func helmConfigDir() string {
	if h := os.Getenv("HELM_CONFIG_HOME"); h != "" {
		return h
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "helm")
	}
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "helm")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "helm"
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Preferences", "helm")
	}
	return filepath.Join(home, ".config", "helm")
}

func helmCacheDir() string {
	if h := os.Getenv("HELM_CACHE_HOME"); h != "" {
		return h
	}
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "helm")
	}
	if runtime.GOOS == "windows" {
		if tmp := os.Getenv("TEMP"); tmp != "" {
			return filepath.Join(tmp, "helm")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "helm"
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Caches", "helm")
	}
	return filepath.Join(home, ".cache", "helm")
}
