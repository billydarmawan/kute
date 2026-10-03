package main

import (
	"slices"
	"testing"

	"github.com/kute-dev/kute/internal/app"
)

// TestConflictingScopeFlags pins the usage-error rule
// (docs/lazy-informers.md §5.6): --namespace-scoped already
// selects the namespace, so pairing it with either spelling of -n/--namespace
// is ambiguous rather than a precedence question, and every other
// combination is a normal launch.
func TestConflictingScopeFlags(t *testing.T) {
	tests := []struct {
		name    string
		set     []string
		wantErr bool
	}{
		{"neither set", nil, false},
		{"namespace alone", []string{"namespace"}, false},
		{"n alone", []string{"n"}, false},
		{"namespace-scoped alone", []string{"namespace-scoped"}, false},
		{"other unrelated flags alone", []string{"context", "demo"}, false},
		{"namespace and namespace-scoped", []string{"namespace", "namespace-scoped"}, true},
		{"n and namespace-scoped", []string{"n", "namespace-scoped"}, true},
		{"namespace, n, and namespace-scoped", []string{"namespace", "n", "namespace-scoped"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := conflictingScopeFlags(tt.set)
			if tt.wantErr && err == nil {
				t.Fatalf("conflictingScopeFlags(%v) = nil, want an error", tt.set)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("conflictingScopeFlags(%v) = %v, want nil", tt.set, err)
			}
		})
	}
}

// TestValidateFlags: values flag.Parse accepts but kute would silently
// ignore, or only fail on much later, are usage errors (14 L15).
func TestValidateFlags(t *testing.T) {
	tests := []struct {
		name    string
		cfg     app.Config
		args    []string
		wantErr bool
	}{
		{"bare launch", app.Config{}, nil, false},
		{"dark theme", app.Config{Theme: "dark"}, nil, false},
		{"light theme", app.Config{Theme: "light"}, nil, false},
		{"theme typo", app.Config{Theme: "drak"}, nil, true},
		{"positional argument", app.Config{}, []string{"pods"}, true},
		{"valid namespace", app.Config{Namespace: "kube-system"}, nil, false},
		{"invalid namespace", app.Config{Namespace: "Kube_System"}, nil, true},
		{"valid scoped namespace", app.Config{ScopeNamespace: "team-a"}, nil, false},
		{"invalid scoped namespace", app.Config{ScopeNamespace: "team a"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFlags(tt.cfg, tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateFlags(%+v, %v) = %v, wantErr %t", tt.cfg, tt.args, err, tt.wantErr)
			}
		})
	}
}

// TestDemoIgnoredFlags: --demo drops --context and --kubeconfig, and says
// so rather than ignoring them in silence; -n is honoured in demo mode.
func TestDemoIgnoredFlags(t *testing.T) {
	set := []string{"demo", "context", "kubeconfig", "n"}
	if got := demoIgnoredFlags(app.Config{Demo: true}, set); !slices.Equal(got, []string{"--context", "--kubeconfig"}) {
		t.Fatalf("demoIgnoredFlags = %v, want [--context --kubeconfig]", got)
	}
	if got := demoIgnoredFlags(app.Config{}, set); got != nil {
		t.Fatalf("demoIgnoredFlags without --demo = %v, want nil", got)
	}
}
