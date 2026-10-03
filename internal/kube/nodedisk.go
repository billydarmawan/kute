package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ErrNodeStatsForbidden is NodeDiskUsage's sentinel for a caller without
// `get nodes/proxy` — the permission the kubelet summary API sits behind.
// It is a sentinel rather than a raw apierrors value so the UI can tell
// "you can't see this" from "the kubelet didn't answer" without importing
// apimachinery.
var ErrNodeStatsForbidden = errors.New("forbidden: get nodes/proxy")

// NodeDisk is a node's root filesystem usage as the kubelet itself reports
// it — the filesystem the kubelet's root dir, logs and emptyDirs live on,
// the one the DiskPressure (nodefs) eviction signal watches. metrics-server
// carries no disk figure at all, so this is the only live source.
//
// Both rows are filesystem fill (capacity minus available) — the figure
// the kubelet's nodefs.available/imagefs.available eviction signals watch.
// ImageUsedBytes/ImageCapacityBytes describe imagefs — the container
// runtime's image and writable-layer store. They are zero when the kubelet reports no imagefs.
type NodeDisk struct {
	UsedBytes     int64
	CapacityBytes int64

	ImageUsedBytes     int64
	ImageCapacityBytes int64

	// availableBytes/imageAvailableBytes are kept only for SeparateImageFs.
	availableBytes      int64
	imageAvailableBytes int64
}

// SeparateImageFs reports whether imagefs is a different filesystem from
// the root one. The summary API names no device, so this compares the two
// statfs readings: the same filesystem reports the same capacity and, taken
// moments apart, nearly the same free space. Capacity alone isn't enough —
// two identically sized disks are the ordinary case, not a coincidence.
// A kubelet on a single disk (kind, a default microk8s) reports imagefs
// anyway, which is exactly the duplicate row this exists to suppress.
func (d NodeDisk) SeparateImageFs() bool {
	if d.ImageCapacityBytes == 0 {
		return false
	}
	if d.ImageCapacityBytes != d.CapacityBytes {
		return true
	}
	delta := d.imageAvailableBytes - d.availableBytes
	if delta < 0 {
		delta = -delta
	}
	return delta > d.CapacityBytes/100
}

// NewNodeDisk builds a NodeDisk from both filesystems' raw statfs figures —
// for fakes and tests, which can't set the unexported available fields.
// Pass zero image figures for a kubelet that reports no imagefs.
func NewNodeDisk(used, capacity, available, imageCapacity, imageAvailable int64) NodeDisk {
	d := NodeDisk{UsedBytes: used, CapacityBytes: capacity, availableBytes: available}
	if imageCapacity > 0 {
		d.ImageCapacityBytes = imageCapacity
		d.ImageUsedBytes = imageCapacity - imageAvailable
		d.imageAvailableBytes = imageAvailable
	}
	return d
}

// NodeDiskUsage reads one node's root filesystem usage from the kubelet
// summary API, proxied through the API server
// (/api/v1/nodes/<name>/proxy/stats/summary). It is a per-node read, never a
// cluster-wide sweep: the summary carries every pod's stats alongside the
// node's own, so its size scales with the node's pod count, and 11b asks for
// it only for the one node on screen, on a slow cadence of its own.
func (c *Cluster) NodeDiskUsage(ctx context.Context, nodeName string) (NodeDisk, error) {
	raw, err := c.clientset.CoreV1().RESTClient().Get().
		AbsPath("/api/v1/nodes", nodeName, "proxy", "stats", "summary").
		DoRaw(ctx)
	if err != nil {
		if apierrors.IsForbidden(err) {
			return NodeDisk{}, fmt.Errorf("%w: %w", ErrNodeStatsForbidden, err)
		}
		return NodeDisk{}, err
	}
	return parseNodeDiskSummary(raw)
}

// summaryFs is the slice of a kubelet FsStats this package reads.
type summaryFs struct {
	UsedBytes      *uint64 `json:"usedBytes"`
	CapacityBytes  *uint64 `json:"capacityBytes"`
	AvailableBytes *uint64 `json:"availableBytes"`
}

// parseNodeDiskSummary pulls node.fs and node.runtime.imageFs out of a
// kubelet stats summary. Only the fields read are declared — the rest of the
// payload (every pod's CPU/memory/network/volume stats) is skipped by the
// decoder. A missing or partial imageFs is not an error: it just leaves the
// image figures zero.
func parseNodeDiskSummary(raw []byte) (NodeDisk, error) {
	var summary struct {
		Node struct {
			Fs      *summaryFs `json:"fs"`
			Runtime *struct {
				ImageFs *summaryFs `json:"imageFs"`
			} `json:"runtime"`
		} `json:"node"`
	}
	if err := json.Unmarshal(raw, &summary); err != nil {
		return NodeDisk{}, fmt.Errorf("decode kubelet stats summary: %w", err)
	}
	fs := summary.Node.Fs
	if fs == nil || fs.CapacityBytes == nil || (fs.UsedBytes == nil && fs.AvailableBytes == nil) {
		return NodeDisk{}, errors.New("kubelet stats summary carries no node filesystem figures")
	}
	// used is capacity minus available, as for imagefs, rather than the
	// summary's own usedBytes: that one is statfs total−free and so leaves
	// out the root-reserved blocks (5% on a default ext4), reading that many
	// points below the nodefs.available eviction signal this row tracks.
	// usedBytes is only the fallback for a summary without available.
	var used, available, imageCapacity, imageAvailable int64
	if fs.AvailableBytes != nil {
		available = int64(*fs.AvailableBytes)
		used = int64(*fs.CapacityBytes) - available
	} else {
		used = int64(*fs.UsedBytes)
	}
	if rt := summary.Node.Runtime; rt != nil && rt.ImageFs != nil && rt.ImageFs.CapacityBytes != nil && rt.ImageFs.AvailableBytes != nil {
		imageCapacity, imageAvailable = int64(*rt.ImageFs.CapacityBytes), int64(*rt.ImageFs.AvailableBytes)
	}
	return NewNodeDisk(used, int64(*fs.CapacityBytes), available, imageCapacity, imageAvailable), nil
}
