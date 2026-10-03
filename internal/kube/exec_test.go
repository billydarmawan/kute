package kube

import (
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestExecSpecWithContainerAndShell(t *testing.T) {
	t.Parallel()
	cmd := ExecSpec(CommandTarget{}, "default", "api-1", "worker", "bash")
	got := strings.Join(cmd.Args, " ")
	want := "kubectl exec -it api-1 -n default -c worker -- bash"
	if got != want {
		t.Fatalf("ExecSpec args = %q, want %q", got, want)
	}
}

func TestExecSpecWithoutContainer(t *testing.T) {
	t.Parallel()
	cmd := ExecSpec(CommandTarget{}, "default", "api-1", "", "sh")
	got := strings.Join(cmd.Args, " ")
	want := "kubectl exec -it api-1 -n default -- sh"
	if got != want {
		t.Fatalf("ExecSpec args = %q, want %q", got, want)
	}
}

func TestExecSpecWithoutShellFallsBackToDetection(t *testing.T) {
	t.Parallel()
	cmd := ExecSpec(CommandTarget{}, "default", "api-1", "worker", "")
	got := strings.Join(cmd.Args, " ")
	if !strings.Contains(got, "command -v bash") {
		t.Fatalf("expected the bash-then-sh fallback probe, got %q", got)
	}
	if !strings.HasPrefix(got, "kubectl exec -it api-1 -n default -c worker -- sh -c") {
		t.Fatalf("unexpected command prefix: %q", got)
	}
}

func TestExecCommandStringMatchesExecSpec(t *testing.T) {
	t.Parallel()
	got := ExecCommandString(CommandTarget{}, "default", "api-1", "worker", "bash")
	want := "kubectl exec -it api-1 -n default -c worker -- bash"
	if got != want {
		t.Fatalf("ExecCommandString = %q, want %q", got, want)
	}
}

func TestExecCommandStringQuotesFallbackProbe(t *testing.T) {
	t.Parallel()
	got := ExecCommandString(CommandTarget{}, "default", "api-1", "worker", "")
	want := "kubectl exec -it api-1 -n default -c worker -- sh -c 'command -v bash >/dev/null && exec bash || exec sh'"
	if got != want {
		t.Fatalf("ExecCommandString = %q, want %q", got, want)
	}
}

// runFailing runs a shell one-liner that writes msg to stderr and exits
// non-zero, returning the *exec.ExitError .Output() produces — the same
// shape a failed kubectl exec returns, with Stderr populated since Output()
// captures it whenever Cmd.Stderr wasn't already set.
func runFailing(t *testing.T, msg string) error {
	t.Helper()
	quoted := "'" + strings.ReplaceAll(msg, "'", `'\''`) + "'"
	_, err := exec.Command("sh", "-c", "echo "+quoted+" >&2; exit 1").Output()
	if err == nil {
		t.Fatal("expected the command to fail")
	}
	return err
}

// TestMissingExecutableDetectsOCIRuntimeENOENT confirms the fork that
// separates a genuinely shell-less container from a real probe failure: the
// runtime's own "executable file not found" wording says the probed runner
// itself isn't in the image, anything else (RBAC, connectivity) doesn't.
func TestMissingExecutableDetectsOCIRuntimeENOENT(t *testing.T) {
	t.Parallel()

	ociErr := runFailing(t, `OCI runtime exec failed: exec failed: unable to start container process: exec: "sh": executable file not found in $PATH: unknown`)
	if !missingExecutable(ociErr) {
		t.Errorf("expected an OCI 'executable file not found' error to be detected as a missing runner")
	}

	deniedErr := runFailing(t, `Error from server (Forbidden): pods "api-1" is forbidden: User cannot create resource "pods/exec"`)
	if missingExecutable(deniedErr) {
		t.Errorf("expected an RBAC denial not to be mistaken for a missing runner")
	}

	if missingExecutable(errors.New("no such host")) {
		t.Errorf("expected a non-ExitError to never count as a missing runner")
	}
}

func TestParseDetectedShellsPreferenceOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, out string
		want      []string
	}{
		{
			// The probe echoes in its own loop order; the result is
			// normalized to ShellCandidates order so bash leads.
			name: "both, echoed sh first",
			out:  "/bin/sh\n/bin/bash\n",
			want: []string{"bash", "sh"},
		},
		{name: "sh only", out: "/bin/sh\n", want: []string{"sh"}},
		{name: "bash in usr/bin", out: "/usr/bin/bash\n", want: []string{"bash"}},
		// A container with no shell produces no output — an empty result,
		// which callers must render as "no shell" rather than unknown.
		{name: "none", out: "", want: nil},
		{name: "blank lines only", out: "\n\n  \n", want: nil},
		// Output from inside a container isn't trusted: anything that isn't
		// a candidate is ignored rather than shown to the user.
		{name: "unrecognized entries ignored", out: "fish\n/bin/zsh\n/bin/sh\n", want: []string{"sh"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseDetectedShells(tt.out); !slices.Equal(got, tt.want) {
				t.Errorf("parseDetectedShells(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

func TestShellProbeArgsIsNonInteractive(t *testing.T) {
	t.Parallel()

	got := shellProbeArgs("default", "api-1", "worker", "sh")
	if slices.Contains(got, "-it") {
		t.Errorf("probe argv must not allocate a TTY: %v", got)
	}
	want := []string{"exec", "api-1", "-n", "default", "-c", "worker", "--", "sh", "-c", shellProbeScript}
	if !slices.Equal(got, want) {
		t.Errorf("shellProbeArgs = %v, want %v", got, want)
	}
}

// TestShellProbeScriptCoversEveryCandidate pins the probe script to the
// candidate list it's generated from: detecting a shell kute would never
// actually exec into would make 10a's shells column disagree with its own
// will-run line.
func TestShellProbeScriptCoversEveryCandidate(t *testing.T) {
	t.Parallel()

	want := "for s in " + strings.Join(ShellCandidates, " ") + "; do"
	if !strings.HasPrefix(shellProbeScript, want) {
		t.Errorf("probe script = %q, want it to iterate exactly %v", shellProbeScript, ShellCandidates)
	}
}

func TestAttachSpecReattachesToTheContainer(t *testing.T) {
	t.Parallel()
	cmd := AttachSpec(CommandTarget{}, "default", "api-1", "debugger-x7k2p")
	got := strings.Join(cmd.Args, " ")
	want := "kubectl attach -it api-1 -n default -c debugger-x7k2p"
	if got != want {
		t.Fatalf("AttachSpec args = %q, want %q", got, want)
	}
}
