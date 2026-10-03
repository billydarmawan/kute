package app

import (
	"os"
	"slices"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/helmrepo"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/state"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// BuildSession loads persisted state/config, selects the Theme (decision
// #3: --theme flag, then the config file's theme: key, then terminal
// background detection), and builds the resource catalog. Cluster is a
// real *kube.Cluster unless cfg.Demo is set or the real cluster can't be
// reached, in which case it's nil (err carries the reason, nil in demo
// mode) — callers wire screens against kube/fake instead, behind the same
// seams (§0.10).
func BuildSession(cfg Config) (sess *tui.Session, cluster *kube.Cluster, err error) {
	// Before anything reads a kubeconfig: --kubeconfig outranks $KUBECONFIG,
	// and every reader in kube resolves through this one override.
	kube.SetKubeconfigPath(cfg.Kubeconfig)

	userConfig := config.Load()
	if cfg.NoUpdateCheck {
		// Per-invocation override only — held outside the persisted fields,
		// and SetProd only ever rewrites prodContexts, so the config file
		// (and every other kute session reading it) is untouched.
		userConfig.DisableUpdateCheck()
	}
	sessionState := state.Load()
	theme := selectTheme(cfg.Theme, userConfig.Theme)

	sess = &tui.Session{
		Charts:       helmrepo.NewCache(helmrepo.Loader{}),
		Registry:     resources.DefaultRegistry(),
		Groups:       resources.DefaultGroups(),
		State:        sessionState,
		Config:       userConfig,
		Theme:        theme,
		Styles:       tui.NewStyles(theme),
		Version:      sessionVersion(cfg.Version),
		Kubeconfig:   cfg.Kubeconfig,
		HelpScope:    helpScopeKeys(),
		HelpList:     helpListKeys(),
		HelpResource: helpResourceKeys(),
		HelpMisc:     helpMiscKeys(),
	}

	if cfg.Demo {
		// --namespace-scoped selects the namespace the same way -n does in
		// demo mode — the fake has no informers to scope, so there is no
		// mode to turn on, only a namespace to select
		// (docs/lazy-informers.md §5.6).
		if cfg.ScopeNamespace != "" {
			sess.Location.Namespace = cfg.ScopeNamespace
		} else if cfg.Namespace != "" {
			sess.Location.Namespace = cfg.Namespace
		}
		return sess, nil, nil
	}
	cluster, err = kube.NewClusterForContext(startupContext(cfg, sessionState))
	if err != nil {
		return sess, nil, err
	}
	sess.Cluster = cluster
	sess.Location = restoredLocation(sessionState, cluster.ContextSnapshot())
	// -n/--namespace is the highest-precedence namespace source, so it's
	// applied last: an explicit flag has to beat both the context's own
	// default namespace and the per-context restore above.
	if cfg.Namespace != "" {
		sess.Location.Namespace = cfg.Namespace
	}
	// --namespace-scoped takes the same highest-precedence slot cfg.Namespace
	// does — cmd/kute's conflictingScopeFlags already rejects the two
	// together, so this and the block above never both fire — and then turns
	// scoped mode on for the whole session before a single informer starts.
	// SetNamespaceScope also pins the Cluster's context namespace, so
	// Session.Location, Cluster.Context, and the eager Pod cache all agree
	// from the first frame (see its own doc comment).
	if cfg.ScopeNamespace != "" {
		sess.Location.Namespace = cfg.ScopeNamespace
		cluster.SetNamespaceScope(cfg.ScopeNamespace)
	}
	return sess, cluster, nil
}

// restoredLocation is where a freshly built cluster for ctx opens: the
// context's own namespace, overridden by that context's persisted
// namespace/kind/filter (state.PerContext) — the same restore 7a's switch
// does. Shared by launch and 4c/10b's rebuilds so the two never drift.
func restoredLocation(st state.State, ctx kube.Context) tui.Location {
	loc := tui.Location{Context: ctx.ContextName, Namespace: ctx.Namespace}
	if pc, ok := st.PerContext[ctx.ContextName]; ok {
		if pc.Namespace != "" {
			loc.Namespace = pc.Namespace
		}
		if pc.Kind != "" {
			loc.Kind = kube.ResourceKind(pc.Kind)
		}
		loc.Filter = pc.Filter
	}
	return loc
}

// startupContext picks the kubeconfig context to launch against: --context
// when given (passed through verbatim, so a name the kubeconfig doesn't have
// fails the build with an error naming it rather than silently landing
// somewhere else), else the most recently used one
// (sessionState.RecentContexts[0], mirroring 7a's own per-context
// namespace/kind/filter restore) if the kubeconfig still has it, otherwise
// "" — the kubeconfig's own current-context — for a fresh install or a recent
// context that's since been removed.
func startupContext(cfg Config, sessionState state.State) string {
	if cfg.Context != "" {
		return cfg.Context
	}
	if len(sessionState.RecentContexts) == 0 {
		return ""
	}
	recent := sessionState.RecentContexts[0]
	names, _, err := kube.AvailableContexts()
	if err != nil || !slices.Contains(names, recent) {
		return ""
	}
	return recent
}

// helpScopeKeys is the 7b help overlay's fixed SCOPE column (docs/design
// README.md §7b): the registered navigation verbs, in the order the panel
// shows them — all-namespaces before context, matching the reorganized
// SCOPE/LIST/RESOURCE/MISC layout. The alt-tab bare-Enter toggle (model.go's
// mostRecentOther) isn't listed here — it's a modifier on the palette's own
// pre-selection, not a distinct action, and the palette's own footer
// (recentPickHint) already surfaces it in context.
func helpScopeKeys() []tui.KeyHint {
	return []tui.KeyHint{
		verbs.Goto.Hint(),
		verbs.Namespace.Hint(),
		verbs.AllNamespaces.Hint(),
		verbs.Context.Hint(),
	}
}

// helpListKeys is 7b's fixed LIST column: generic list-row actions,
// identical on every screen (v.0.3.0.dc.html §29a). ↵ open lands here rather
// than in its own column or RESOURCE — it doesn't belong to any other fixed
// group, and per-screen Keybar() Groups must never carry it (every screen's
// own keybar omits it by convention, so this column is its one remaining
// home).
func helpListKeys() []tui.KeyHint {
	return []tui.KeyHint{
		verbs.Filter.Hint(),
		{Key: "1-9", Label: "sort column"},
		{Key: "↑↓ jk", Label: "move"},
		{Key: "ctrl+d", Label: "half-page down"},
		{Key: "ctrl+u", Label: "half-page up"},
		verbs.Mark.Hint(),
		verbs.Open.Hint(),
	}
}

// helpResourceKeys is 7b's fixed RESOURCE column: per-object actions,
// identical on every screen (v.0.3.0.dc.html §29a).
func helpResourceKeys() []tui.KeyHint {
	return []tui.KeyHint{
		verbs.YAML.Hint(),
		verbs.Edit.Hint(),
		verbs.Events.Hint(),
		verbs.Timeline.Hint(),
		verbs.Meta.Hint(),
	}
}

// helpMiscKeys is 7b's horizontal MISC footer row: shell-level actions with
// no other home, trimmed to bindings that actually exist today — the
// mockup's "p pause sync"/"r reconnect" aren't implemented yet (Phase 4), so
// listing them would document a lie.
func helpMiscKeys() []tui.KeyHint {
	return []tui.KeyHint{
		{Key: "U", Label: "what's new"},
		verbs.Help.Hint(),
		{Key: "esc", Label: "back"},
		{Key: "ctrl+q", Label: "quit"},
	}
}

// selectTheme resolves decision #3's precedence: flagTheme (--theme), then
// configTheme (config.yaml's theme: key), then terminal background
// detection. Any value other than "dark"/"light" falls through (so "auto"
// and typos both defer to detection rather than erroring).
//
// The detection branch only queries the terminal when stdin AND stdout are
// both real TTYs. lipgloss.HasDarkBackground already skips the query and
// falls back to its dark-on-error default when that's not the case on
// Unix — but on Windows, BackgroundColor reopens CONIN$/CONOUT$ whenever
// stdin/stdout are redirected instead of bailing out, so a non-interactive
// invocation (a piped/scripted launch, or `go test` itself) still issues a
// live OSC-11 query. With no terminal emulator on the other end to answer
// it, that query can block in ReadConsole indefinitely: Windows cancels the
// read through muesli/cancelreader's fallback implementation, which cannot
// interrupt an already-blocked ReadConsole syscall, so the query's own 2s
// timeout never actually fires. That's what turned a single `go test
// ./...` invocation into a 10-minute hang on windows-latest CI runners —
// this guard keeps the query from ever being attempted outside a real
// interactive terminal, matching the fallback HasDarkBackground documents.
func selectTheme(flagTheme, configTheme string) tui.Theme {
	for _, v := range []string{flagTheme, configTheme} {
		switch v {
		case "dark":
			return tui.Dark()
		case "light":
			return tui.Light()
		}
	}
	if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
		return tui.Dark()
	}
	if lipgloss.HasDarkBackground(os.Stdin, os.Stdout) {
		return tui.Dark()
	}
	return tui.Light()
}
