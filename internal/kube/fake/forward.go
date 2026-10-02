// Port-forward seams for --demo/tests: a PodResolver that resolves
// Service/Deployment targets against the fake cluster's own seeded objects
// (same selector-matching logic the real kube.ClientsetPodResolver runs
// against a live clientset), and a ForwardDialer whose Tunnel binds a real
// local listener — so 13a's busy-port detection still probes a real OS
// port in demo mode — but never proxies real traffic, since there's no
// backing pod to actually reach.
package fake

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/kute-dev/kute/internal/kube"
)

type podResolver struct{ cluster *Cluster }

// NewPodResolver builds the fake kube.PodResolver bound to cluster.
func NewPodResolver(cluster *Cluster) kube.PodResolver { return podResolver{cluster: cluster} }

func (r podResolver) ResolveForwardPod(ctx context.Context, target kube.ForwardTarget) (string, error) {
	switch target.Kind {
	case kube.KindPod:
		return target.Name, nil
	case kube.KindService:
		objs, _ := r.cluster.ListRaw(ctx, kube.KindService, target.Namespace)
		for _, obj := range objs {
			if svc, ok := obj.(*corev1.Service); ok && svc.Name == target.Name {
				return r.firstMatchingPod(ctx, target.Namespace, svc.Spec.Selector)
			}
		}
		return "", fmt.Errorf("service %q not found", target.Name)
	case kube.KindDeployment:
		objs, _ := r.cluster.ListRaw(ctx, kube.KindDeployment, target.Namespace)
		for _, obj := range objs {
			dep, ok := obj.(*appsv1.Deployment)
			if !ok || dep.Name != target.Name {
				continue
			}
			sel := map[string]string{}
			if dep.Spec.Selector != nil {
				sel = dep.Spec.Selector.MatchLabels
			}
			return r.firstMatchingPod(ctx, target.Namespace, sel)
		}
		return "", fmt.Errorf("deployment %q not found", target.Name)
	default:
		return "", fmt.Errorf("port-forward is not supported for kind %s", target.Kind)
	}
}

// ResolveForwardPort maps a Service targetPort onto the seeded pod's
// container ports with the same kube.PodTargetPort lookup the real resolver
// runs against a live Pod.
func (r podResolver) ResolveForwardPort(ctx context.Context, namespace, pod string, target intstr.IntOrString, protocol string) (int32, error) {
	if target.Type == intstr.Int {
		return kube.PodTargetPort(nil, target, protocol)
	}
	objs, _ := r.cluster.ListRaw(ctx, kube.KindPod, namespace)
	for _, obj := range objs {
		if p, ok := obj.(*corev1.Pod); ok && p.Name == pod {
			return kube.PodTargetPort(p, target, protocol)
		}
	}
	return 0, fmt.Errorf("pod %q not found", pod)
}

func (r podResolver) firstMatchingPod(ctx context.Context, namespace string, selector map[string]string) (string, error) {
	if len(selector) == 0 {
		return "", fmt.Errorf("target has no selector to resolve a backing pod")
	}
	objs, _ := r.cluster.ListRaw(ctx, kube.KindPod, namespace)
	for _, obj := range objs {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}
		match := true
		for k, v := range selector {
			if pod.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return pod.Name, nil
		}
	}
	return "", fmt.Errorf("no pods match the target's selector")
}

type dialer struct{}

// NewForwardDialer builds the fake kube.ForwardDialer.
func NewForwardDialer() kube.ForwardDialer { return dialer{} }

// flakyForwardPod is the one demo pod whose forwards periodically drop —
// worker-0, the fixture's crashlooping pod: a forward into a container
// that keeps restarting realistically would lose its connection, unlike
// every other (stable) demo pod. Without this, 13c/13d's failing/retry/
// backoff state and the hard "never modal/banner" invariant it drives
// could only ever be checked by reading code, never by driving --demo mode
// (CLAUDE.md: "the fake provider must stay feature-complete for tests/demo
// mode").
const flakyForwardPod = "worker-0"

// flakyTunnelTTL is how long a forward into flakyForwardPod stays Active
// before ForwardManager.run sees Run() fail and flips it to Reconnecting.
const flakyTunnelTTL = 8 * time.Second

func (dialer) Dial(_ string, pod string, localPort int, _ int32) (kube.Tunnel, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		return nil, err
	}
	t := &tunnel{ln: ln}
	if pod == flakyForwardPod {
		return &flakyTunnel{tunnel: t, ttl: flakyTunnelTTL}, nil
	}
	return t, nil
}

// tunnel accepts and immediately closes local connections — enough to make
// the local port genuinely bound (so busy-port detection has something
// real to probe) without pretending to proxy data nothing backs.
type tunnel struct {
	ln        net.Listener
	closeOnce sync.Once
}

func (t *tunnel) Run(activity func()) error {
	for {
		conn, err := t.ln.Accept()
		if err != nil {
			return err
		}
		if activity != nil {
			activity()
		}
		_ = conn.Close()
	}
}

func (t *tunnel) Close() {
	t.closeOnce.Do(func() { _ = t.ln.Close() })
}

// flakyTunnel wraps a normal tunnel but abandons it after ttl, simulating
// the container-restart-driven disconnect flakyForwardPod's doc comment
// describes — ForwardManager.run reads the returned error the same way it
// would a real dropped connection, entering Reconnecting/backoff.
type flakyTunnel struct {
	*tunnel
	ttl time.Duration
}

func (t *flakyTunnel) Run(activity func()) error {
	done := make(chan error, 1)
	go func() { done <- t.tunnel.Run(activity) }()
	select {
	case err := <-done:
		return err
	case <-time.After(t.ttl):
		t.Close()
		<-done // Accept's own error return, discarded — ours is more specific
		return fmt.Errorf("connection lost: container restarting")
	}
}
