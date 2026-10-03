// Package state persists cross-session UI state — palette recents and the
// per-context namespace/kind/filter the user was last looking at
// (mvp-plan.md §0.6) — to $XDG_STATE_HOME/kute/state.json, falling back to
// ~/.local/state/kute/state.json. It is best-effort: a missing, corrupt,
// or unrecognized-future-version file never blocks startup, it just yields
// a fresh zero value.
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// CurrentVersion is the schema version stamped on every save. Bump it and
// extend migrate whenever the schema changes.
const CurrentVersion = 3

// MaxRecent caps every Recent* list, most-recent-first. 11, not some
// rounder number: 6a/7a's numbered recent-pick (tui.recentNumbers) excludes
// the list's own first two entries — index 0 is always current, index 1 is
// always the "previous" alt-tab target (tui.mostRecentOther) — before
// assigning digits 1-9 to what's left, so the list needs current + previous
// + 9 addressable slots = 11 for the full '1'-'9' range to ever be reachable.
const MaxRecent = 11

// PerContext is the namespace/kind/filter remembered for one kube-context,
// keyed by context name in State.PerContext, plus that context's own
// namespace-recents list. Namespace is the singular restore value (what
// context-switch restores); RecentNamespaces is the full most-recent-first
// list the namespace palette's RECENT row/digit-pick read — kept as a
// separate field rather than derived from RecentNamespaces[0], mirroring how
// Kind and the global RecentKinds list already coexist independently.
// Namespaces don't get a global recents list like kinds/contexts do: a
// namespace only exists inside its own cluster, so a global list would show
// dead rows from other contexts (see State's doc comment).
type PerContext struct {
	Namespace        string   `json:"namespace,omitempty"`
	Kind             string   `json:"kind,omitempty"`
	Filter           string   `json:"filter,omitempty"`
	RecentNamespaces []string `json:"recentNamespaces,omitempty"`
	// RecentDebugImages is the debug panel's (§41b/§41c) most-recent-first
	// image list — schema v3. Per-context, not global, the same reasoning
	// RecentNamespaces already gives: a registry that makes sense in one
	// cluster's context may be unreachable from another. Only the image
	// field gets recents; target/profile/entrypoint don't per the design
	// mockup.
	RecentDebugImages []string `json:"recentDebugImages,omitempty"`
}

// State is the persisted document. RecentKinds/RecentContexts are
// most-recent-first — index 0 is always the current entry (only a completed
// switch pushes to the front), index 1 the one before it. Each palette
// scope's alt-tab grammar (opening the palette pre-selects index 1, so a
// bare open+enter toggles back to it — see tui.mostRecentOther) reads off
// that ordering. Kinds and contexts exist across every cluster, so their
// recents are one global list, filtered to what the current context actually
// has at display time. Namespaces don't share that property (a namespace is
// scoped to one cluster), so their recents live per-context instead — see
// PerContext.RecentNamespaces.
type State struct {
	Version        int                   `json:"version"`
	RecentKinds    []string              `json:"recentKinds,omitempty"`
	RecentContexts []string              `json:"recentContexts,omitempty"`
	PerContext     map[string]PerContext `json:"perContext,omitempty"`
	// UpdateCheck is 28a/28b's cached release-feed check (docs/design
	// README.md's State Management section: "cached in the state dir,
	// drives the 28a chip and 28b's per-version dismissal") — schema v2.
	UpdateCheck UpdateCheckState `json:"updateCheck,omitzero"`

	// base is the document as Load read it, deep-copied and never mutated,
	// so Save can tell this session's own changes from a concurrent
	// instance's (see merge). Nil for a State Load didn't produce, which
	// Save writes as-is.
	base *State
}

// UpdateCheckState is the persisted trio 28a's ambient chip needs even in a
// session that never re-checks (28a's own 24h-cadence check runs at most
// once per launch) — everything richer (release notes, changelog entries)
// is refetched on demand and never written here, per that section's
// "cached in the state dir" being scoped to exactly these three fields.
type UpdateCheckState struct {
	LastChecked   time.Time `json:"lastChecked,omitzero"`
	LatestVersion string    `json:"latestVersion,omitempty"`
	SeenVersions  []string  `json:"seenVersions,omitempty"`
}

// Path returns the state file location: $XDG_STATE_HOME/kute/state.json,
// or ~/.local/state/kute/state.json when XDG_STATE_HOME is unset.
func Path() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "kute", "state.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "state", "kute", "state.json")
}

// Load reads and returns the state file at Path(). Missing/corrupt files
// yield a fresh zero value; an unrecognized newer Version is discarded
// (never partially interpreted) rather than risking a misread. Neither case
// is an error — callers get a usable State unconditionally.
func Load() State {
	return loadFrom(Path())
}

func loadFrom(path string) State {
	data, err := os.ReadFile(path)
	if err != nil {
		return zero()
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return zero()
	}
	switch {
	case s.Version == CurrentVersion:
		return withBase(normalize(s))
	case s.Version < CurrentVersion:
		return withBase(normalize(migrate(s)))
	default:
		return zero()
	}
}

func zero() State {
	return withBase(State{Version: CurrentVersion, PerContext: map[string]PerContext{}})
}

func withBase(s State) State {
	b := s.clone()
	s.base = &b
	return s
}

func (s State) clone() State {
	out := s
	out.base = nil
	out.RecentKinds = slices.Clone(s.RecentKinds)
	out.RecentContexts = slices.Clone(s.RecentContexts)
	out.UpdateCheck.SeenVersions = slices.Clone(s.UpdateCheck.SeenVersions)
	out.PerContext = make(map[string]PerContext, len(s.PerContext))
	for k, pc := range s.PerContext {
		pc.RecentNamespaces = slices.Clone(pc.RecentNamespaces)
		pc.RecentDebugImages = slices.Clone(pc.RecentDebugImages)
		out.PerContext[k] = pc
	}
	return out
}

func normalize(s State) State {
	if s.PerContext == nil {
		s.PerContext = map[string]PerContext{}
	}
	s.RecentKinds = capRecent(s.RecentKinds)
	s.RecentContexts = capRecent(s.RecentContexts)
	for name, pc := range s.PerContext {
		pc.RecentNamespaces = capRecent(pc.RecentNamespaces)
		pc.RecentDebugImages = capRecent(pc.RecentDebugImages)
		s.PerContext[name] = pc
	}
	return s
}

func capRecent(items []string) []string {
	if len(items) > MaxRecent {
		return items[:MaxRecent]
	}
	return items
}

// migrate upgrades s to CurrentVersion. v1 -> v2 (UpdateCheck, 28a/28b) and
// v2 -> v3 (PerContext.RecentDebugImages, §41b/§41c) both add no field that
// needs a data transform — JSON decoding an older file already leaves the
// new field at its zero value — so, like every bump so far, this is just
// the version stamp; the hook is here for the next bump that actually needs
// to transform something.
func migrate(s State) State {
	s.Version = CurrentVersion
	return s
}

// MarkUpdateSeen records version as seen (opening 28b for it, or 'x'
// skipping it there) — idempotent, so the ambient chip (28a) never re-nags
// for a version already seen, per docs/design README.md §28a.
func (s *State) MarkUpdateSeen(version string) {
	if version == "" || slices.Contains(s.UpdateCheck.SeenVersions, version) {
		return
	}
	s.UpdateCheck.SeenVersions = append(s.UpdateCheck.SeenVersions, version)
}

// UpdateSeen reports whether version has already been marked seen.
func (s State) UpdateSeen(version string) bool {
	return slices.Contains(s.UpdateCheck.SeenVersions, version)
}

// ErrNewerVersion is Save's refusal to overwrite a state file stamped with a
// Version this build doesn't know: Load discarded it rather than misread it,
// so writing back would replace a newer binary's recents, perContext and
// update cache with this session's near-empty snapshot (a dev build run
// beside a release, or a downgrade).
var ErrNewerVersion = errors.New("state file was written by a newer kute; not overwriting it")

// Save atomically writes s to Path() (temp file + fsync + rename, so a crash mid
// write never corrupts the previous state), stamping Version. It refuses
// with ErrNewerVersion, leaving the file untouched, when the file on disk
// carries a newer Version. A State from Load is merged onto whatever is on
// disk now rather than replacing it, so two kute windows (one per context
// is common) each keep the other's changes — see merge.
func (s State) Save() error {
	return s.saveTo(Path())
}

func (s State) saveTo(path string) error {
	// Checked at write time, not remembered from Load: a newer binary may
	// have written the file since this session started.
	if data, err := os.ReadFile(path); err == nil {
		var onDisk struct {
			Version int `json:"version"`
		}
		if json.Unmarshal(data, &onDisk) == nil && onDisk.Version > CurrentVersion {
			return ErrNewerVersion
		}
	}
	if s.base != nil {
		s = merge(loadFrom(path), *s.base, s)
	}
	s.Version = CurrentVersion
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	// Flush before the rename: otherwise a power loss can leave the rename
	// durable but the data not, and an empty state.json loads as zero().
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

// merge applies mine's changes since base onto disk, the file as it is now
// (another instance may have saved since this one loaded): a PerContext
// entry this session changed replaces disk's, every other entry is disk's;
// a recents list this session changed leads, followed by disk's remaining
// entries; seen versions are unioned; the later update check wins. Without
// it the last window to exit discarded every other window's per-context
// location, recents and dismissed versions (so the 28a chip re-nagged).
// The read-merge-rename runs at exit in well under a millisecond, so two
// instances racing inside that window is the one case it doesn't cover.
func merge(disk, base, mine State) State {
	out := disk.clone()
	for k := range base.PerContext {
		if _, ok := mine.PerContext[k]; !ok {
			delete(out.PerContext, k)
		}
	}
	for k, pc := range mine.PerContext {
		if b, ok := base.PerContext[k]; !ok || !samePerContext(b, pc) {
			out.PerContext[k] = pc
		}
	}
	out.RecentKinds = mergeRecent(disk.RecentKinds, base.RecentKinds, mine.RecentKinds)
	out.RecentContexts = mergeRecent(disk.RecentContexts, base.RecentContexts, mine.RecentContexts)
	for _, v := range mine.UpdateCheck.SeenVersions {
		out.MarkUpdateSeen(v)
	}
	if mine.UpdateCheck.LastChecked.After(disk.UpdateCheck.LastChecked) {
		out.UpdateCheck.LastChecked = mine.UpdateCheck.LastChecked
		out.UpdateCheck.LatestVersion = mine.UpdateCheck.LatestVersion
	}
	return out
}

func samePerContext(a, b PerContext) bool {
	return a.Namespace == b.Namespace && a.Kind == b.Kind && a.Filter == b.Filter &&
		slices.Equal(a.RecentNamespaces, b.RecentNamespaces) &&
		slices.Equal(a.RecentDebugImages, b.RecentDebugImages)
}

// mergeRecent keeps disk's list when this session never touched its own,
// and otherwise puts this session's order first — its index 0 is what it
// was last looking at — with disk's other entries after it.
func mergeRecent(disk, base, mine []string) []string {
	if slices.Equal(base, mine) {
		return disk
	}
	out := slices.Clone(mine)
	for _, v := range disk {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return capRecent(out)
}

// PushRecent prepends value to items (most-recent-first), removing any
// existing occurrence and capping the result at MaxRecent. Empty values are
// ignored.
func PushRecent(items []string, value string) []string {
	if value == "" {
		return items
	}
	out := make([]string, 0, MaxRecent)
	out = append(out, value)
	for _, it := range items {
		if it == value {
			continue
		}
		out = append(out, it)
		if len(out) >= MaxRecent {
			break
		}
	}
	return out
}
