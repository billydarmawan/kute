package tui

import "github.com/kute-dev/kute/internal/kube"

// MetricsUnavailableNote is the dim one-liner a cpu/mem usage block shows in
// place of its bars when a metrics-server read failed — 11b's USED /
// CAPACITY and 19a's CAPACITY share it so the two screens describe the same
// failure the same way. Only a missing metrics.k8s.io API is "not
// installed": a Forbidden read or a transient failure (timeout, a 503 from
// an unhealthy APIService backend) says so instead, since telling an RBAC-
// restricted user to install something that's already there is wrong.
func MetricsUnavailableNote(err error) string {
	switch {
	case kube.IsMetricsServerMissing(err):
		return "cpu / mem — no metrics-server installed"
	case kube.IsPermissionError(err):
		return "cpu / mem — not permitted to read metrics-server"
	default:
		return "cpu / mem — metrics-server unavailable"
	}
}
