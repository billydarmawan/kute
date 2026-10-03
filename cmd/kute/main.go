package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/kute-dev/kute/internal/app"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	cfg := app.DefaultConfig()
	cfg.Version = version
	cfg.Commit = commit
	cfg.Date = date
	flag.StringVar(&cfg.Context, "context", "", "kubeconfig context to launch against (default: last used, else the kubeconfig's current-context)")
	flag.StringVar(&cfg.Namespace, "namespace", "", "namespace to launch in (default: last used, else the context's own)")
	flag.StringVar(&cfg.Namespace, "n", "", "shorthand for --namespace")
	flag.StringVar(&cfg.ScopeNamespace, "namespace-scoped", "", "namespace to scope every informer to — restricts kute to this namespace, for identities without cluster-wide list access")
	flag.StringVar(&cfg.Kubeconfig, "kubeconfig", "", "path to the kubeconfig file to use (default: $KUBECONFIG, else ~/.kube/config)")
	flag.BoolVar(&cfg.Demo, "demo", false, "run against an in-memory fake cluster instead of a real one")
	flag.BoolVar(&cfg.Keycast, "keycast", false, "show a recent-keypresses chip (bottom-right) — for demo recording")
	flag.StringVar(&cfg.Theme, "theme", "", "override theme selection: dark|light (default: auto-detect)")
	flag.StringVar(&cfg.LogFile, "log-file", "", "write the error and client-go log stream to this file (for bug reports)")
	flag.BoolVar(&cfg.NoUpdateCheck, "no-update-check", false, "disable the once-per-24h update check")
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("kute %s (%s, %s)\n", version, commit, date)
		return
	}

	var setNames []string
	flag.Visit(func(f *flag.Flag) { setNames = append(setNames, f.Name) })
	if err := conflictingScopeFlags(setNames); err != nil {
		fmt.Fprintf(os.Stderr, "kute: %v\n", err)
		os.Exit(1)
	}
	if err := validateFlags(cfg, flag.Args()); err != nil {
		fmt.Fprintf(os.Stderr, "kute: %v\n", err)
		flag.Usage()
		os.Exit(2)
	}
	for _, w := range demoIgnoredFlags(cfg, setNames) {
		fmt.Fprintf(os.Stderr, "kute: %s is ignored with --demo\n", w)
	}

	if err := app.RunWithConfig(cfg); err != nil {
		// A crash has already printed its own footer, naming the report
		// file — a second "kute: program was killed" line under it would
		// bury the one thing the user is meant to act on.
		if !errors.Is(err, app.ErrCrashed) {
			fmt.Fprintf(os.Stderr, "kute: %v\n", err)
		}
		os.Exit(1)
	}
}

// conflictingScopeFlags rejects -n/--namespace alongside --namespace-scoped:
// --namespace-scoped already selects the namespace (docs/lazy-informers.md
// §5.6), so the two together are ambiguous rather than a precedence
// question. setNames is exactly flag.Visit's own list —
// the flags actually set on the command line, not their (possibly
// zero-value) results — so an explicit `--namespace=""` still counts as
// set, and a bare, unflagged launch never trips this.
func conflictingScopeFlags(setNames []string) error {
	hasNamespace := slices.Contains(setNames, "namespace") || slices.Contains(setNames, "n")
	hasScoped := slices.Contains(setNames, "namespace-scoped")
	if hasNamespace && hasScoped {
		return errors.New("--namespace and --namespace-scoped are mutually exclusive; --namespace-scoped already selects the namespace")
	}
	return nil
}

// validateFlags rejects what flag.Parse lets through but kute would
// silently ignore or only fail on much later: a --theme other than
// dark/light, stray positional arguments (`kute pods` is not a command),
// and a namespace that can't exist (a typo there surfaced as Forbidden or
// an empty list rather than as the typo it is).
func validateFlags(cfg app.Config, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected argument %q: kute takes flags only", args[0])
	}
	switch cfg.Theme {
	case "", "dark", "light":
	default:
		return fmt.Errorf("invalid --theme %q: want dark or light", cfg.Theme)
	}
	for flagName, ns := range map[string]string{"--namespace": cfg.Namespace, "--namespace-scoped": cfg.ScopeNamespace} {
		if ns == "" {
			continue
		}
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return fmt.Errorf("invalid %s %q: %s", flagName, ns, strings.Join(errs, "; "))
		}
	}
	return nil
}

// demoIgnoredFlags names the cluster-selection flags --demo drops: the
// fake cluster has one fixed context and no kubeconfig. -n and
// --namespace-scoped are honoured in demo mode and aren't listed.
func demoIgnoredFlags(cfg app.Config, setNames []string) []string {
	if !cfg.Demo {
		return nil
	}
	var ignored []string
	for _, name := range []string{"context", "kubeconfig"} {
		if slices.Contains(setNames, name) {
			ignored = append(ignored, "--"+name)
		}
	}
	return ignored
}
