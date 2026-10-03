package kube

import (
	"errors"
	"math"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	resource "k8s.io/apimachinery/pkg/api/resource"
)

// ErrMetricsNotConfigured is what the metrics readers return when no
// metrics client was built — wrapped, so test with errors.Is or
// IsMetricsServerMissing.
var ErrMetricsNotConfigured = errors.New("metrics client is not configured")

// IsMetricsServerMissing reports whether err means there is no
// metrics-server to ask — no client, or the metrics.k8s.io API not served
// (a 404) — as opposed to one that exists but failed this read (Forbidden,
// a timeout, a 503 from an unhealthy APIService backend). Only the former
// justifies saying "no metrics-server installed".
func IsMetricsServerMissing(err error) bool {
	return errors.Is(err, ErrMetricsNotConfigured) || apierrors.IsNotFound(err)
}

type PodMetrics struct {
	CPU string
	MEM string
	// Exact aggregated usage, so callers don't have to re-parse the formatted
	// strings (which lose precision on decimal quantities like "5.8Gi").
	CPUMilli int64
	MemBytes int64
}

// NodeMetric is a node's live CPU/MEM usage — the 11a nodes-list bars'
// numerator (kube.Cluster.NodeMetrics), same shape as PodMetrics.
type NodeMetric struct {
	CPU      string
	MEM      string
	CPUMilli int64
	MemBytes int64
}

// FormatCPU renders q as the compact "45m"/"1.2"/"2" strings PodMetrics/
// NodeMetric.CPU carry — exported so kube/fake can synthesize believable
// demo-mode usage in the same format the real cluster (cluster.go) reports.
func FormatCPU(q resource.Quantity) string {
	milli := q.MilliValue()
	if milli == 0 {
		return "0m"
	}
	if milli < 1000 {
		return strconv.FormatInt(milli, 10) + "m"
	}
	if milli%1000 == 0 {
		return strconv.FormatInt(milli/1000, 10)
	}
	return strconv.FormatFloat(float64(milli)/1000, 'f', 1, 64)
}

// FormatMemory renders q as the compact "128Mi"/"1.5Gi"/"175Mi" strings
// PodMetrics/NodeMetric.MEM carry — exported for the same reason as FormatCPU.
//
// The output is capped at three significant digits, and so at five cells,
// because that is exactly what browse's metric cell has room for: the CPU/MEM
// columns are resources.MetricColumnWidth wide, of which the mini bar takes six
// cells and the separator one. A fourth digit doesn't render narrower, it
// renders as "174.…".
func FormatMemory(q resource.Quantity) string {
	bytes := q.Value()
	if bytes < 1024 {
		return strconv.FormatInt(bytes, 10) + "B"
	}
	units := []struct {
		suffix string
		value  int64
	}{
		{"Ei", 1 << 60},
		{"Pi", 1 << 50},
		{"Ti", 1 << 40},
		{"Gi", 1 << 30},
		{"Mi", 1 << 20},
		{"Ki", 1 << 10},
	}
	for i, unit := range units {
		if bytes < unit.value {
			continue
		}
		// Anything from 1000 of its own unit up would need a fourth digit, so
		// hand it to the next unit up and let it render as "1.0Gi" instead of
		// "1023Mi". Ei is the top of the table only because int64 tops out at
		// 8 EiB, so this can never run off the end.
		if i > 0 && math.Round(float64(bytes)/float64(unit.value)) >= 1000 {
			unit = units[i-1]
		}
		if bytes%unit.value == 0 {
			return strconv.FormatInt(bytes/unit.value, 10) + unit.suffix
		}
		scaled := float64(bytes) / float64(unit.value)
		// 9.95 and up rounds to "10.0", a digit too wide — take whole numbers
		// from there on.
		if scaled < 9.95 {
			return strconv.FormatFloat(scaled, 'f', 1, 64) + unit.suffix
		}
		return strconv.FormatInt(int64(math.Round(scaled)), 10) + unit.suffix
	}
	return strconv.FormatInt(bytes, 10) + "B"
}

// PodKey is the map key every pod-metrics lookup uses. Pod names are only
// unique within a namespace, and the metrics maps are built from
// cluster-wide Lists whenever the caller passes "" — so the namespace has
// to be in the key or same-named pods silently collide.
func PodKey(namespace, name string) string {
	return namespace + "/" + name
}
