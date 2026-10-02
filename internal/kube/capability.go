// This file is docs/lazy-informers.md's "third one-shot read" (0.8.0 plan
// §3.8): a cached Discovery().ServerVersion() call, taken once alongside
// the eager informer start/discovery pass in Start (which SwitchContext
// also calls), never repeated on a timer and never re-issued per screen
// open — the same exemption CLAUDE.md's Conventions section already
// documents for CountLive and 8a's live managedFields fetch.
package kube

import (
	"context"
	"strconv"
	"strings"

	"k8s.io/client-go/kubernetes"
)

// TimeZoneCapability is the tri-state answer to "does the connected API
// server honor CronJob spec.timeZone" — tasks/cronjobschedule (36d) is the
// one consumer, reached through a package-local capability interface it
// declares itself (Architecture's "define the interface you need in the
// task package" rule), never a new optional seam on resources.RawLister.
// Node kubelet versions are never consulted for this: a mixed-version node
// pool says nothing about the control plane's own API version.
type TimeZoneCapability int

const (
	// TimeZoneCapabilityUnknown is the zero value: no successful probe yet,
	// or the probed version falls in the 1.25-1.26 band where the beta
	// feature gate could be disabled. A consumer must treat this the same
	// as Unsupported for write purposes — never a guess, never inferred
	// from anything else — unless the object being edited already carries
	// a populated spec.timeZone, which is direct evidence for that object
	// regardless of what the version alone would suggest.
	TimeZoneCapabilityUnknown TimeZoneCapability = iota
	// TimeZoneCapabilitySupported is Kubernetes 1.27+, where spec.timeZone
	// is GA.
	TimeZoneCapabilitySupported
	// TimeZoneCapabilityUnsupported is below Kubernetes 1.25, where an
	// older server may silently prune an unknown spec.timeZone field
	// rather than reject the write — guessing wrong here is a write that
	// looks successful and does nothing.
	TimeZoneCapabilityUnsupported
)

// TimeZoneCapability returns the cached tri-state read by the last
// Start/SwitchContext's one-shot probe. Unknown before the very first probe
// completes (e.g. read from a Cluster that hasn't Start-ed yet) — the same
// safe default a failed/offline probe also leaves behind.
func (c *Cluster) TimeZoneCapability() TimeZoneCapability {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tzCapability
}

// probeTimeZoneCapability runs the one-shot Discovery().ServerVersion()
// read and stores the classified result. Best-effort: a failed read
// (offline, RBAC restricting discovery) leaves the capability at whatever
// it already was (Unknown on a first connect) rather than failing the
// whole Start/SwitchContext — a consumer already treats Unknown as
// read-only, which is the safe outcome here too. gen and cs are Start's
// snapshot, as for refreshDiscovery: the answer lands only if gen is still
// current, so a superseded Start can't stamp the old server's version onto
// the new context.
func (c *Cluster) probeTimeZoneCapability(_ context.Context, gen int, cs kubernetes.Interface) {
	info, err := cs.Discovery().ServerVersion()
	if err != nil {
		return
	}
	tz := classifyTimeZoneCapability(info.Major, info.Minor)
	c.mu.Lock()
	if c.generation == gen {
		c.tzCapability = tz
	}
	c.mu.Unlock()
}

// classifyTimeZoneCapability implements §3.8's version bands. Major/Minor
// come from version.Info's own string fields, which can carry a trailing
// "+" (GKE/EKS's own convention for a patched minor version) — stripped
// before parsing rather than treated as a parse failure, so a managed
// cluster's version string doesn't fall back to Unknown just because of
// that suffix. Any other unparseable form (a dev/kwok build's unusual
// string) also lands on Unknown, never a guess.
func classifyTimeZoneCapability(majorStr, minorStr string) TimeZoneCapability {
	major, err1 := strconv.Atoi(strings.TrimRight(majorStr, "+"))
	minor, err2 := strconv.Atoi(strings.TrimRight(minorStr, "+"))
	if err1 != nil || err2 != nil {
		return TimeZoneCapabilityUnknown
	}
	switch {
	case major > 1 || (major == 1 && minor >= 27):
		return TimeZoneCapabilitySupported
	case major == 1 && minor < 25:
		return TimeZoneCapabilityUnsupported
	default:
		return TimeZoneCapabilityUnknown
	}
}
