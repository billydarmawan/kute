package kube

import (
	"os/exec"
	"strings"
)

// CommandTarget names the cluster an external kubectl/helm subprocess must
// act on. kute picks its context in-process (configOverrides.CurrentContext,
// restored from recents or --context, switched by 7a) and never writes it
// back to the kubeconfig, so a child process left to resolve the file's own
// current-context can land on a different cluster than the one on screen —
// behind a confirm that was gated on kute's context, not the child's. Every
// kubectl/helm argv is therefore built through kubectlArgv/helmArgv, which
// pin both fields explicitly whenever they're known.
//
// The zero value adds no flags: that is the in-cluster case (no kubeconfig
// file, no context name — kubectl falls back to the same in-cluster config
// kute's own client used) and --demo, which never shells out at all.
type CommandTarget struct {
	// Kubeconfig is --kubeconfig's value, the one source a child process
	// can't see on its own. $KUBECONFIG is deliberately not copied here: the
	// child inherits the environment variable already, and it may be a
	// colon-separated list that kubectl's single-file --kubeconfig flag
	// rejects.
	Kubeconfig string
	// Context is the kubeconfig context kute itself is connected to.
	Context string
}

// NewCommandTarget pins contextName alongside the process's --kubeconfig
// override (SetKubeconfigPath), if any.
func NewCommandTarget(contextName string) CommandTarget {
	kubeconfigFlagPathMu.RLock()
	defer kubeconfigFlagPathMu.RUnlock()
	return CommandTarget{Kubeconfig: kubeconfigFlagPath, Context: contextName}
}

// CommandTarget is the target for a subprocess acting on this Cluster — the
// context its own client is built against. Read under c.mu: SwitchContext
// replaces c.kctx from a tea.Cmd goroutine.
func (c *Cluster) CommandTarget() CommandTarget {
	c.mu.Lock()
	name := c.kctx.ContextName
	c.mu.Unlock()
	return NewCommandTarget(name)
}

// kubectlArgv prefixes args with t's global flags. They go before the verb so
// they can never land after an exec/debug `--` separator, where kubectl
// would hand them to the container command instead.
func kubectlArgv(t CommandTarget, args ...string) []string {
	var out []string
	if t.Kubeconfig != "" {
		out = append(out, "--kubeconfig", t.Kubeconfig)
	}
	if t.Context != "" {
		out = append(out, "--context", t.Context)
	}
	return append(out, args...)
}

// helmArgv appends t's flags in helm's own spelling (--kube-context, not
// --context, which helm doesn't have).
func helmArgv(t CommandTarget, args ...string) []string {
	out := append([]string(nil), args...)
	if t.Kubeconfig != "" {
		out = append(out, "--kubeconfig", t.Kubeconfig)
	}
	if t.Context != "" {
		out = append(out, "--kube-context", t.Context)
	}
	return out
}

// kubectlCommand is the one place a kubectl subprocess is built.
func kubectlCommand(t CommandTarget, args ...string) *exec.Cmd {
	return exec.Command("kubectl", kubectlArgv(t, args...)...)
}

// kubectlCommandString renders the exact invocation kubectlCommand(t,
// args...) runs.
func kubectlCommandString(t CommandTarget, args ...string) string {
	return commandString(append([]string{"kubectl"}, kubectlArgv(t, args...)...))
}

// commandString renders argv as a copy-pasteable invocation, quoting any
// argument containing whitespace — the "will run" line for every
// *CommandString function, built from the same argv the subprocess gets so
// the two can't drift apart.
func commandString(argv []string) string {
	out := append([]string(nil), argv...)
	for i, a := range out {
		if strings.ContainsAny(a, " \t") {
			out[i] = "'" + a + "'"
		}
	}
	return strings.Join(out, " ")
}
