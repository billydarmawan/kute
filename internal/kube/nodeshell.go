package kube

import (
	"strings"
)

// DefaultNodeShellImage is the node-debug panel's default image (§41d) —
// the image only needs a `chroot` binary, since the shell itself resolves
// against the host root, not the image. Formerly the fixed image of the
// standalone 's' NodeShell verb, retired in favor of 'x' opening the debug
// panel (tasks/debugpanel) prefilled with this same default — see
// NodeDebugSpec (debug.go).
const DefaultNodeShellImage = "busybox:1.37"

// Node labels the two managed platforms that can't host a node shell set on
// their own nodes.
const (
	// fargateComputeTypeLabel is set to "fargate" on the Node object EKS
	// synthesises per Fargate pod. Documented and definitive — it is what
	// `kubectl get nodes -l eks.amazonaws.com/compute-type=fargate` selects.
	fargateComputeTypeLabel = "eks.amazonaws.com/compute-type"
	// gkeNodePoolLabel is on every GKE node, Autopilot or Standard. It is
	// only used to confirm a node is GKE's at all before the name check
	// below is allowed to mean anything.
	gkeNodePoolLabel = "cloud.google.com/gke-nodepool"
	// autopilotNodePrefix is how GKE names Autopilot-managed nodes —
	// "gk3-<cluster>-…" against Standard's "gke-<cluster>-…". Google
	// generates both prefixes; neither is user-settable, so a Standard node
	// cannot be mistaken for an Autopilot one.
	autopilotNodePrefix = "gk3-"
)

// NodeShellUnavailable explains why a node shell cannot work on this node, or
// returns "" when it can. A node shell is `kubectl debug --profile=sysadmin`:
// a privileged container in the node's host namespaces, chrooted into the
// node's root filesystem. Two managed platforms make that impossible, and
// they fail in ways worth telling apart from "the command didn't work":
//
//   - EKS Fargate. Each pod gets its own synthetic Node object with no
//     machine behind it to attach to.
//   - GKE Autopilot. The nodes are real, but Autopilot's admission webhooks
//     reject privileged host-namespace containers outright.
//
// Neither is detectable from a cluster-level API — Autopilot in particular is
// a property of the GKE cluster resource, not of anything Kubernetes serves —
// so this reads the node's own labels, falling back to GKE's Autopilot node
// naming for the case where no label distinguishes it. Deliberately
// conservative: a false negative just means the user sees kubectl's own
// rejection instead of ours, while a false positive would hide a working
// verb, so the name check only applies to nodes already known to be GKE's.
//
// This is a message, not a gate: the decided handling is a clear error
// naming the reason, not a hidden key, so 'x' stays on a
// Node's keybar and explains itself when pressed (the debug panel it opens
// — tasks/debugpanel, §41d — never launches).
func NodeShellUnavailable(name string, labels map[string]string) string {
	if labels[fargateComputeTypeLabel] == "fargate" {
		return "node shell is unavailable on EKS Fargate: the pod has no node to attach to"
	}
	if _, gke := labels[gkeNodePoolLabel]; gke && strings.HasPrefix(name, autopilotNodePrefix) {
		return "node shell is unavailable on GKE Autopilot: it rejects the privileged host-namespace container kubectl debug needs"
	}
	return ""
}
