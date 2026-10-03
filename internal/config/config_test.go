package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kute-dev/kute/internal/testutil/testenv"
)

func TestLoadMissingFileYieldsNothingProd(t *testing.T) {
	t.Parallel()
	got := loadFrom(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if got.IsProd("anything") {
		t.Fatalf("expected no prod contexts from a missing file")
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadBrokenFileFailsClosed: a config file kute can't read must never
// quietly drop every context back to non-PROD — that would turn every
// delete into an inline y/N. Whatever prodContexts said is unknowable, so
// every context reads as PROD and LoadErr says why.
func TestLoadBrokenFileFailsClosed(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"syntax error":           "prodContexts: [unterminated",
		"prodContexts a mapping": "prodContexts:\n  prod-eu: true\n",
		"one bad list element":   "prodContexts:\n  - prod-eu\n  - [nested]\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := loadFrom(writeConfig(t, body))
			if got.LoadErr == nil {
				t.Fatalf("LoadErr = nil for %q", body)
			}
			if !got.ProdUnknown() || !got.IsProd("anything") {
				t.Fatalf("a broken prodContexts must fail closed (every context PROD), got %+v", got)
			}
		})
	}
}

// TestLoadSalvagesProdContextsPastABadField: one mistyped key elsewhere in
// the file (nodeShellImage as a list) must not take prodContexts down with
// it — and the mistake is still reported, not swallowed.
func TestLoadSalvagesProdContextsPastABadField(t *testing.T) {
	t.Parallel()
	got := loadFrom(writeConfig(t, "prodContexts: [prod-eu]\ntheme: dark\nnodeShellImage: [x]\n"))
	if got.LoadErr == nil {
		t.Fatalf("LoadErr = nil; the bad nodeShellImage must be reported")
	}
	if got.ProdUnknown() {
		t.Fatalf("prodContexts parsed fine; must not fall back to all-PROD")
	}
	if !got.IsProd("prod-eu") || got.IsProd("dev-kind") {
		t.Fatalf("salvaged prodContexts wrong: %+v", got.ProdContexts)
	}
	if got.Theme != "dark" {
		t.Fatalf("Theme = %q, want the salvaged dark", got.Theme)
	}
}

// TestLoadAcceptsScalarProdContexts: `prodContexts: prod-eu` means exactly
// what it looks like.
func TestLoadAcceptsScalarProdContexts(t *testing.T) {
	t.Parallel()
	got := loadFrom(writeConfig(t, "prodContexts: prod-eu\n"))
	if got.LoadErr != nil {
		t.Fatalf("LoadErr = %v, want a scalar accepted as a one-element list", got.LoadErr)
	}
	if !got.IsProd("prod-eu") || got.IsProd("dev-kind") {
		t.Fatalf("scalar prodContexts = %+v", got.ProdContexts)
	}
}

func TestLoadUnreadableFileFailsClosed(t *testing.T) {
	t.Parallel()
	// A directory where the file should be: ReadFile fails with something
	// other than not-exist.
	got := loadFrom(t.TempDir())
	if got.LoadErr == nil || !got.IsProd("anything") {
		t.Fatalf("an unreadable config must fail closed, got %+v", got)
	}
}

func TestLoadParsesProdContexts(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "prodContexts:\n  - prod-eks\n  - prod-gke\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadFrom(path)
	if !got.IsProd("prod-eks") || !got.IsProd("prod-gke") {
		t.Fatalf("expected both prod contexts recognized, got %+v", got)
	}
	if got.IsProd("dev-kind") {
		t.Fatalf("dev-kind should not be prod")
	}
}

func TestLoadParsesTheme(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("theme: light\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadFrom(path)
	if got.Theme != "light" {
		t.Fatalf("Theme = %q, want light", got.Theme)
	}
}

func TestUpdateCheckEnabledDefaultsTrue(t *testing.T) {
	t.Parallel()
	if !(Config{}).UpdateCheckEnabled() {
		t.Fatalf("UpdateCheckEnabled must default to true when update.check is absent")
	}
}

func TestUpdateCheckEnabledParsesFalse(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("update:\n  check: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadFrom(path)
	if got.UpdateCheckEnabled() {
		t.Fatalf("expected update.check: false to disable the check")
	}
}

func TestUpdateCheckEnabledParsesExplicitTrue(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("update:\n  check: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadFrom(path)
	if !got.UpdateCheckEnabled() {
		t.Fatalf("expected update.check: true to keep the check enabled")
	}
}

func TestIsProdNameHeuristicNotApplied(t *testing.T) {
	t.Parallel()
	// A context literally named "prod-looking-but-not-listed" must not be
	// treated as prod — PROD comes only from the explicit list, never a
	// name heuristic (mvp-plan.md decision #2).
	c := Config{ProdContexts: []string{"prod-eks"}}
	if c.IsProd("prod-looking-but-not-listed") {
		t.Fatalf("IsProd must not use a name heuristic")
	}
}

// TestSetProdPersistsAndRoundTrips drives SetProd end to end through Path()
// (t.Setenv("HOME", …) rather than a loadFrom(path)-style injected path,
// since SetProd/save have no path parameter — not t.Parallel-safe per
// testing.T.Setenv's restriction, matching context_test.go's
// writeContextTestKubeconfig).
func TestSetProdPersistsAndRoundTrips(t *testing.T) {
	testenv.SetHome(t, t.TempDir())

	c := Load()
	if c.IsProd("prod-eks") {
		t.Fatalf("fresh config must start with nothing prod")
	}
	if err := c.SetProd("prod-eks", true); err != nil {
		t.Fatalf("SetProd(true): %v", err)
	}
	if !c.IsProd("prod-eks") {
		t.Fatalf("in-memory Config not updated by SetProd(true)")
	}

	reloaded := Load()
	if !reloaded.IsProd("prod-eks") {
		t.Fatalf("prod-eks not persisted across reload: %+v", reloaded)
	}

	if err := reloaded.SetProd("prod-eks", false); err != nil {
		t.Fatalf("SetProd(false): %v", err)
	}
	if reloaded.IsProd("prod-eks") {
		t.Fatalf("in-memory Config not updated by SetProd(false)")
	}
	if got := Load(); got.IsProd("prod-eks") {
		t.Fatalf("prod-eks still persisted after unmark: %+v", got)
	}
}

// TestSetProdNoopWhenStatusUnchanged asserts SetProd doesn't write the file
// (and doesn't error) when the requested status already holds — toggling
// off a context that's already off, or on one already on.
func TestSetProdNoopWhenStatusUnchanged(t *testing.T) {
	testenv.SetHome(t, t.TempDir())

	c := Config{ProdContexts: []string{"prod-eks"}}
	if err := c.SetProd("dev-kind", false); err != nil {
		t.Fatalf("SetProd(false) on already-non-prod: %v", err)
	}
	if _, err := os.Stat(Path()); err == nil {
		t.Fatalf("SetProd must not write the file when status is unchanged")
	}

	if err := c.SetProd("prod-eks", true); err != nil {
		t.Fatalf("SetProd(true) on already-prod: %v", err)
	}
	if _, err := os.Stat(Path()); err == nil {
		t.Fatalf("SetProd must not write the file when status is unchanged")
	}
}

// TestSetProdRefusesToOverwriteUnparsableFile: the in-memory Config after a
// broken load is only what survived the parse, so marking a context PROD
// must refuse rather than replace the user's file with that remnant.
func TestSetProdRefusesToOverwriteUnparsableFile(t *testing.T) {
	testenv.SetHome(t, t.TempDir())
	const body = "# my prod list\nprodContexts: [prod-eu\n"
	if err := os.MkdirAll(filepath.Dir(Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	c := Load()
	if err := c.SetProd("dev-kind", true); !errors.Is(err, ErrUnparsable) {
		t.Fatalf("SetProd err = %v, want ErrUnparsable", err)
	}
	got, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("config.yaml was rewritten:\n%s", got)
	}
}

// writeHomeConfig writes body to Path() under a fresh HOME with mode.
func writeHomeConfig(t *testing.T, body string, mode os.FileMode) {
	t.Helper()
	testenv.SetHome(t, t.TempDir())
	if err := os.MkdirAll(filepath.Dir(Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func readHomeConfig(t *testing.T) string {
	t.Helper()
	got, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

// TestSetProdKeepsCommentsAndUnknownKeys: ctrl+p rewrites prodContexts and
// nothing else — a comment, a key this build doesn't know, and key order all
// survive (14 M1c).
func TestSetProdKeepsCommentsAndUnknownKeys(t *testing.T) {
	writeHomeConfig(t, "# kute config\ntheme: dark # always dark\nfutureKey:\n  nested: 1\nprodContexts: [prod-eu] # the scary ones\n", 0o644)

	c := Load()
	if err := c.SetProd("prod-us", true); err != nil {
		t.Fatalf("SetProd: %v", err)
	}
	const want = "# kute config\ntheme: dark # always dark\nfutureKey:\n  nested: 1\nprodContexts: [prod-eu, prod-us] # the scary ones\n"
	if got := readHomeConfig(t); got != want {
		t.Fatalf("config.yaml =\n%s\nwant\n%s", got, want)
	}
	if !c.IsProd("prod-us") || !c.IsProd("prod-eu") {
		t.Fatalf("in-memory Config not updated: %+v", c.ProdContexts)
	}
}

// TestSetProdKeepsEditsMadeAfterLoad: the write starts from the file as it
// is now, not the snapshot this session loaded, so a hand edit or another
// kute's ctrl+p made in between survives (14 M1b).
func TestSetProdKeepsEditsMadeAfterLoad(t *testing.T) {
	writeHomeConfig(t, "prodContexts: [prod-eu]\n", 0o600)
	c := Load()

	if err := os.WriteFile(Path(), []byte("nodeShellImage: mirror/busybox\nprodContexts: [prod-eu, prod-ap]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.SetProd("prod-us", true); err != nil {
		t.Fatalf("SetProd: %v", err)
	}
	got := Load()
	if got.NodeShellImage != "mirror/busybox" {
		t.Fatalf("external nodeShellImage edit lost:\n%s", readHomeConfig(t))
	}
	if want := (StringList{"prod-eu", "prod-ap", "prod-us"}); !slices.Equal(got.ProdContexts, want) {
		t.Fatalf("prodContexts = %v, want %v", got.ProdContexts, want)
	}
	if !slices.Equal(c.ProdContexts, got.ProdContexts) {
		t.Fatalf("in-memory %v doesn't mirror disk %v", c.ProdContexts, got.ProdContexts)
	}
}

// TestSetProdNeverPersistsRuntimeOverrides: --no-update-check (and any other
// value held only in memory) must never reach the file (14 M1a).
func TestSetProdNeverPersistsRuntimeOverrides(t *testing.T) {
	testenv.SetHome(t, t.TempDir())
	c := Load()
	c.DisableUpdateCheck()
	if c.UpdateCheckEnabled() {
		t.Fatalf("DisableUpdateCheck must disable the check for this process")
	}
	// The pre-fix BuildSession expressed the flag as this field; even an
	// in-memory value there must stay in memory.
	off := false
	c.Update.Check = &off
	c.Theme = "light"

	if err := c.SetProd("prod-eu", true); err != nil {
		t.Fatalf("SetProd: %v", err)
	}
	if got := readHomeConfig(t); got != "prodContexts:\n  - prod-eu\n" {
		t.Fatalf("config.yaml carries in-memory values:\n%s", got)
	}
	if !Load().UpdateCheckEnabled() {
		t.Fatalf("--no-update-check leaked into config.yaml")
	}
}

// TestSetProdPreservesFileMode: the atomic temp+rename write keeps the
// existing file's permissions rather than the temp file's.
func TestSetProdPreservesFileMode(t *testing.T) {
	writeHomeConfig(t, "theme: dark\n", 0o640)
	if err := os.Chmod(Path(), 0o640); err != nil { // umask-proof
		t.Fatal(err)
	}
	c := Load()
	if err := c.SetProd("prod-eu", true); err != nil {
		t.Fatalf("SetProd: %v", err)
	}
	fi, err := os.Stat(Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(Path()))
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

// TestSetProdCreatesMissingFile: a user who never wrote config.yaml gets one
// (and its directory), private by default.
func TestSetProdCreatesMissingFile(t *testing.T) {
	testenv.SetHome(t, t.TempDir())
	c := Load()
	if err := c.SetProd("prod-eu", true); err != nil {
		t.Fatalf("SetProd: %v", err)
	}
	fi, err := os.Stat(Path())
	if err != nil {
		t.Fatalf("config.yaml not created: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode = %v, want 0600", fi.Mode().Perm())
	}
	if !Load().IsProd("prod-eu") {
		t.Fatalf("prod-eu not persisted")
	}
}

// TestSetProdOffRemovesEntry: unmarking drops just that context, and the
// last one takes the key with it rather than leaving `prodContexts: []`.
func TestSetProdOffRemovesEntry(t *testing.T) {
	writeHomeConfig(t, "theme: dark\nprodContexts: [prod-eu, prod-us]\n", 0o600)
	c := Load()
	if err := c.SetProd("prod-eu", false); err != nil {
		t.Fatalf("SetProd: %v", err)
	}
	if got := readHomeConfig(t); got != "theme: dark\nprodContexts: [prod-us]\n" {
		t.Fatalf("after first unmark:\n%s", got)
	}
	if err := c.SetProd("prod-us", false); err != nil {
		t.Fatalf("SetProd: %v", err)
	}
	if got := readHomeConfig(t); got != "theme: dark\n" {
		t.Fatalf("after last unmark:\n%s", got)
	}
	if c.IsProd("prod-eu") || c.IsProd("prod-us") {
		t.Fatalf("in-memory Config still prod: %v", c.ProdContexts)
	}
}

// TestSetProdKeepsCommentOnlyFile: a file holding only comments has no YAML
// node to carry them, so they're kept verbatim ahead of the new key.
func TestSetProdKeepsCommentOnlyFile(t *testing.T) {
	writeHomeConfig(t, "# fill me in later\n", 0o600)
	c := Load()
	if err := c.SetProd("prod-eu", true); err != nil {
		t.Fatalf("SetProd: %v", err)
	}
	if got := readHomeConfig(t); got != "# fill me in later\nprodContexts:\n  - prod-eu\n" {
		t.Fatalf("config.yaml =\n%s", got)
	}
}

// TestSetProdWritesThroughSymlink: a dotfile-managed config.yaml symlink is
// written through, not replaced by a regular file.
func TestSetProdWritesThroughSymlink(t *testing.T) {
	testenv.SetHome(t, t.TempDir())
	target := filepath.Join(t.TempDir(), "dotfiles-config.yaml")
	if err := os.WriteFile(target, []byte("theme: dark\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, Path()); err != nil {
		t.Fatal(err)
	}
	c := Load()
	if err := c.SetProd("prod-eu", true); err != nil {
		t.Fatalf("SetProd: %v", err)
	}
	if fi, err := os.Lstat(Path()); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v %v", fi, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "theme: dark\nprodContexts:\n  - prod-eu\n" {
		t.Fatalf("target =\n%s", got)
	}
}

// TestSetProdRefusesFileBrokenSinceLoad: the file parsed at startup but was
// broken by a hand edit since — the write-time re-read refuses too.
func TestSetProdRefusesFileBrokenSinceLoad(t *testing.T) {
	writeHomeConfig(t, "prodContexts: [prod-eu]\n", 0o600)
	c := Load()
	const broken = "prodContexts: [prod-eu\n"
	if err := os.WriteFile(Path(), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.SetProd("prod-us", true); !errors.Is(err, ErrUnparsable) {
		t.Fatalf("SetProd err = %v, want ErrUnparsable", err)
	}
	if got := readHomeConfig(t); got != broken {
		t.Fatalf("broken file rewritten:\n%s", got)
	}
}

// TestSetProdKeepsListShape: a block list keeps its entries' own comments,
// and a lone-scalar prodContexts becomes a list with its comment kept.
func TestSetProdKeepsListShape(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"block", "prodContexts:\n  # EU first\n  - prod-eu # frankfurt\n  - prod-ap\n", "prodContexts:\n  # EU first\n  - prod-eu # frankfurt\n  - prod-us\n"},
		{"scalar", "prodContexts: prod-eu # frankfurt\n", "prodContexts: # frankfurt\n  - prod-eu\n  - prod-us\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeHomeConfig(t, tc.body, 0o600)
			c := Load()
			if err := c.SetProd("prod-ap", false); err != nil {
				t.Fatalf("SetProd: %v", err)
			}
			if err := c.SetProd("prod-us", true); err != nil {
				t.Fatalf("SetProd: %v", err)
			}
			if got := readHomeConfig(t); got != tc.want {
				t.Fatalf("config.yaml =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

func TestLoadParsesArgoCDNamespace(t *testing.T) {
	t.Parallel()
	got := loadFrom(writeConfig(t, "argoCDNamespace: gitops\n"))
	if got.LoadErr != nil || got.ArgoCDNamespace != "gitops" {
		t.Fatalf("got %+v, want argoCDNamespace gitops with no error", got)
	}
}

// TestBadArgoCDNamespaceLeavesProdContextsAlone: a mistyped argoCDNamespace
// is reported, but neither drops prodContexts nor makes every context PROD.
func TestBadArgoCDNamespaceLeavesProdContextsAlone(t *testing.T) {
	t.Parallel()
	got := loadFrom(writeConfig(t, "prodContexts: [prod-eu]\nargoCDNamespace: [gitops]\n"))
	if got.LoadErr == nil {
		t.Fatal("LoadErr = nil; the bad argoCDNamespace must be reported")
	}
	if got.ProdUnknown() || !got.IsProd("prod-eu") || got.IsProd("dev-kind") {
		t.Fatalf("prodContexts must survive a bad argoCDNamespace intact: %+v unknown=%v", got.ProdContexts, got.ProdUnknown())
	}
	if got.ArgoCDNamespace != "" {
		t.Fatalf("ArgoCDNamespace = %q, want empty for an undecodable value", got.ArgoCDNamespace)
	}
}

// TestSetProdLeavesArgoCDNamespaceUntouched: argoCDNamespace is read-only —
// ctrl+p rewrites prodContexts alone, so the key and its exact text survive,
// and an in-memory change to it is never written.
func TestSetProdLeavesArgoCDNamespaceUntouched(t *testing.T) {
	writeHomeConfig(t, "argoCDNamespace: gitops # argo lives here\nprodContexts: [prod-eu]\n", 0o644)

	c := Load()
	if c.ArgoCDNamespace != "gitops" {
		t.Fatalf("ArgoCDNamespace = %q, want gitops", c.ArgoCDNamespace)
	}
	c.ArgoCDNamespace = "elsewhere"
	if err := c.SetProd("prod-us", true); err != nil {
		t.Fatalf("SetProd: %v", err)
	}
	const want = "argoCDNamespace: gitops # argo lives here\nprodContexts: [prod-eu, prod-us]\n"
	if got := readHomeConfig(t); got != want {
		t.Fatalf("config.yaml =\n%s\nwant\n%s", got, want)
	}
}
