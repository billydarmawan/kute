package kube

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
)

// ShellCandidates are the shells 10a probes for, in the preference order
// execArgs itself applies (bash first, sh as the floor). Detecting anything
// kute wouldn't actually run would make the picker's shells column disagree
// with its own "will run" line.
var ShellCandidates = []string{"bash", "sh"}

// ErrDemoUnavailable is what every tea.ExecProcess handoff (exec, kubectl
// debug) reports in --demo mode instead of actually shelling out. kube/fake
// stays feature-complete for what a screen *displays* (CLAUDE.md), but a
// tty handoff needs a real API server on the other end to attach to — there
// is nothing honest for kube/fake to fake here, so browse/execpicker/
// debugpanel short-circuit before ever building a kubectl command, rather
// than let a real kubectl process fail loudly against whatever (or no)
// kubeconfig context happens to be active.
//
// Kept deliberately short: browse renders this behind an "exec exited: "/
// "node shell exited: " prefix on the same keybar row as the update chip
// and the "? help" hint (RightNote + RightHints, chrome.go's
// renderKeybarV2), and insetChromeLine drops the *entire* right side —
// including "? help" — rather than truncating when the combined text
// overflows the row (same quirk browse/keys.go's pendingScale comment
// documents). A longer message here doesn't get cut off; it silently
// erases the help hint next to it.
var ErrDemoUnavailable = errors.New("no cluster (demo)")

// shellProbeScript lists whichever ShellCandidates exist on PATH, one per
// line. Built from ShellCandidates rather than spelled out, so the probe
// can't drift into detecting a shell execArgs would never actually run. It
// runs through a shell, so it needs one to work at all — hence DetectShells'
// two attempts.
var shellProbeScript = "for s in " + strings.Join(ShellCandidates, " ") +
	`; do command -v $s >/dev/null 2>&1 && echo $s; done`

// DetectShells reports which of ShellCandidates a container actually has,
// in preference order (docs/design README.md §10a: "detected shells").
//
// This is a real probe, not an inference from the image name: it runs
// `command -v` inside the container via kubectl exec. That's one short-lived
// exec per container, on a screen the user opened by pressing 'x' — already
// an explicit intent to exec into this pod, and strictly less invasive than
// the interactive shell they're about to get. It does leave a pods/exec
// audit entry per container, which is the honest cost of answering the
// question instead of guessing at it.
//
// An empty slice with a nil error is a real answer — the container has no
// shell at all (distroless), which is exactly what a user needs to know
// before pressing enter. A non-nil error means the probe couldn't run
// (no kubectl, no pods/exec permission, connection down); callers must
// render that as unknown, never as "no shell" and never as a fabricated
// list.
func DetectShells(ctx context.Context, target CommandTarget, namespace, pod, container string) ([]string, error) {
	// Two attempts: the probe script needs a shell to run, so an image
	// shipping bash but no sh (rare, but real) would otherwise report no
	// shell. bash is the fallback runner precisely because it's the other
	// candidate.
	var firstErr error
	// A genuinely shell-less (distroless) container fails *both* attempts
	// the same way a real probe failure would: kubectl exec exits non-zero.
	// allMissing stays true only while every failure so far is the OCI
	// runtime's own "the runner itself isn't in the container" error —
	// which the probe script can't tell apart from "no pods/exec
	// permission" any other way, since it never got a shell to run
	// `command -v` through. Once both candidates fail that way, that *is*
	// the answer: no shell, not an unknown probe result.
	allMissing := true
	for _, runner := range []string{"sh", "bash"} {
		out, err := exec.CommandContext(ctx, "kubectl", kubectlArgv(target, shellProbeArgs(namespace, pod, container, runner)...)...).Output()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if !missingExecutable(err) {
				allMissing = false
			}
			continue
		}
		return parseDetectedShells(string(out)), nil
	}
	if allMissing {
		return nil, nil
	}
	return nil, firstErr
}

// missingExecutable reports whether a kubectl exec attempt failed because
// the probed runner itself isn't present in the container — the container
// runtime's own ENOENT wording ("executable file not found in $PATH"),
// stable across docker/containerd/CRI-O since they all shell out through
// runc. Distinct from a real probe failure (no kubectl on PATH, no
// pods/exec permission, connection down): none of those mention a missing
// executable.
func missingExecutable(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	return strings.Contains(string(exitErr.Stderr), "executable file not found")
}

// shellProbeArgs builds the probe's kubectl argv. Deliberately not
// execArgs: the probe is non-interactive, so it must not pass -it (kubectl
// warns "Unable to use a TTY" onto stderr when stdin isn't a terminal, and
// a TTY would merge stderr into the output being parsed).
func shellProbeArgs(namespace, pod, container, runner string) []string {
	args := []string{"exec", pod, "-n", namespace}
	if container != "" {
		args = append(args, "-c", container)
	}
	return append(args, "--", runner, "-c", shellProbeScript)
}

// parseDetectedShells reduces the probe's stdout to ShellCandidates in
// preference order. It matches on the basename so /bin/bash and /usr/bin/bash
// both count, and ignores anything unrecognized rather than trusting
// arbitrary output from inside a container.
func parseDetectedShells(out string) []string {
	found := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i := strings.LastIndex(line, "/"); i >= 0 {
			line = line[i+1:]
		}
		if slices.Contains(ShellCandidates, line) {
			found[line] = true
		}
	}
	var shells []string
	for _, c := range ShellCandidates {
		if found[c] {
			shells = append(shells, c)
		}
	}
	return shells
}

// execArgs builds the kubectl exec argv shared by ExecSpec and
// ExecCommandString, so the 10a picker's "will run" documentation line can
// never drift from the command that actually executes.
func execArgs(namespace, pod, container, shell string) []string {
	args := []string{"exec", "-it", pod, "-n", namespace}
	if container != "" {
		args = append(args, "-c", container)
	}
	args = append(args, "--")
	if shell != "" {
		args = append(args, shell)
	} else {
		args = append(args, "sh", "-c", "command -v bash >/dev/null && exec bash || exec sh")
	}
	return args
}

// ExecSpec builds the kubectl exec command for the 10a exec screen. Bubble
// Tea suspends and hands the tty to this process (tea.ExecProcess); exit
// returns control to the pod that launched it. An empty shell keeps the
// in-container bash-then-sh fallback below, for the paths with no detection
// result to go on (a single-container pod, which skips the picker entirely,
// or a probe that couldn't run); 10a passes DetectShells' answer once it has
// one, so the picker's shells column, its "will run" line and this command
// all name the same shell.
func ExecSpec(target CommandTarget, namespace, pod, container, shell string) *exec.Cmd {
	return kubectlCommand(target, execArgs(namespace, pod, container, shell)...)
}

// AttachSpec builds `kubectl attach -it` for §41e's ↵ on an ephemeral
// container: it reconnects to the shell kubectl debug started there — a
// dropped connection costs one key, not a second shell or a second
// injected container. Only a Running container can be attached to.
func AttachSpec(target CommandTarget, namespace, pod, container string) *exec.Cmd {
	return kubectlCommand(target, attachArgs(namespace, pod, container)...)
}

func attachArgs(namespace, pod, container string) []string {
	return []string{"attach", "-it", pod, "-n", namespace, "-c", container}
}

// ExecCommandString renders the exact kubectl invocation ExecSpec builds,
// for 10a's "will run" line (docs/design README.md §10a: "no magic,
// copyable documentation") — quoting any argument containing whitespace
// (the shell-fallback probe's `-c` payload).
func ExecCommandString(target CommandTarget, namespace, pod, container, shell string) string {
	return kubectlCommandString(target, execArgs(namespace, pod, container, shell)...)
}
