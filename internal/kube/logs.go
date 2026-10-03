package kube

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
)

type LogStreamRequest struct {
	Namespace  string
	PodName    string
	Container  string
	Follow     bool
	Timestamps bool
	TailLines  int64
	// SinceSeconds limits the stream to lines newer than this many seconds
	// ago (5b's "since 15m" toolbar cycle); 0 means no lower bound.
	SinceSeconds int64
}

type PodLogStreamer interface {
	StreamPodLogs(context.Context, LogStreamRequest) (io.ReadCloser, error)
}

type ClientPodLogStreamer struct {
	Client kubernetes.Interface
}

func (s ClientPodLogStreamer) StreamPodLogs(ctx context.Context, req LogStreamRequest) (io.ReadCloser, error) {
	if req.Namespace == "" {
		return nil, errors.New("pod namespace is required for log streaming")
	}
	if strings.TrimSpace(req.PodName) == "" {
		return nil, errors.New("pod name is required for log streaming")
	}

	options := &corev1.PodLogOptions{
		Container:  req.Container,
		Follow:     req.Follow,
		Timestamps: req.Timestamps,
	}
	if req.TailLines > 0 {
		options.TailLines = &req.TailLines
	}
	if req.SinceSeconds > 0 {
		options.SinceSeconds = &req.SinceSeconds
	}

	return s.Client.CoreV1().Pods(req.Namespace).GetLogs(req.PodName, options).Stream(ctx)
}

func ScanLogLines(ctx context.Context, reader io.Reader, emit func(string) bool) error {
	scanner := bufio.NewScanner(reader)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if !emit(scanner.Text()) {
			return nil
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

// IsPermissionError classifies err as an RBAC/permission denial (4b): the
// typed apierrors.IsForbidden check first (real API server responses), with
// a substring fallback for sources that don't return a typed *StatusError
// (e.g. kube/fake's seeded errors, or a wrapped message).
func IsPermissionError(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsForbidden(err) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "forbidden") || strings.Contains(text, "permission")
}

// IsContainerNotStartedError classifies err as the kubelet log handler's
// fixed error text for a container that hasn't started yet (e.g.
// `container "X" in pod "Y" is waiting to start: ContainerCreating`) — a
// substring check, same shape as IsPermissionError, since the API server
// returns this as a BadRequest with no distinct typed error to check
// instead. podlogs (5b) uses it only as a defense-in-depth fallback: the
// pod cache's own container state is checked before connecting, so this
// only fires in the narrow race where a container flips back to waiting
// between that check and the actual connect.
func IsContainerNotStartedError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "is waiting to start")
}
