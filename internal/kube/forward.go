// Package kube's port-forward support (13a/13c/13d, docs/design/README.md):
// ForwardManager runs forwarding sessions in-process (no kubectl
// subprocess) via a pluggable ForwardDialer/PodResolver pair, so the real
// cluster dials through client-go's SPDY upgrade (spdyDialer below) while
// kube/fake substitutes an in-memory stand-in for tests/--demo. Sessions are
// keyed by an opaque ID, independent of any *Cluster — a ForwardManager is
// constructed once at the composition root and never rebuilt on context
// switch, which is what makes forwards survive one (docs/design README.md
// §13d: "global across context switches").
package kube

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	// client-go's portforward.New/spdy.NewDialer (the pair this file uses) still
	// take apimachinery's httpstream.Dialer, not the k8s.io/streaming replacement
	// — that's only wired through the parallel NewForStreaming/NewDialerForStreaming
	// pair, which isn't interface-compatible.
	//nolint:staticcheck // deprecated, but see the note above: the replacement
	// is not interface-compatible with the portforward/spdy pair this file uses.
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// ForwardState is one session's current tunnel state.
type ForwardState string

const (
	ForwardActive       ForwardState = "active"
	ForwardReconnecting ForwardState = "reconnecting"
)

// ForwardTarget names what a forward points at: a specific Pod, or a
// Service/Deployment resolved to one of its backing pods at dial time
// (docs/design README.md §13c: "svc/deploy targets show the resolved
// backing pod").
type ForwardTarget struct {
	Kind      ResourceKind
	Namespace string
	Name      string
}

// PortOption is one candidate port offered by 13a's picker, discovered from
// the target object itself.
type PortOption struct {
	Port      int32
	Name      string
	Protocol  string
	Container string // empty for Service ports, which aren't container-scoped
	// TargetPort is a Service port's spec.ports[].targetPort — the backing
	// pod's port the Service routes Port to, a number or a container port
	// name. A pod-subresource port-forward needs that container port, never
	// the Service's own Port (kubectl's `port-forward svc/x` does the same
	// translation). Zero for Pod/Deployment options, whose Port already is
	// the container port; see ForwardSession.TargetPort.
	TargetPort intstr.IntOrString
}

// ForwardablePorts lists obj's candidate ports for the 13a picker: a Pod or
// Deployment's container ports, or a Service's declared ports. Any other
// type (or a nil/unexpected object) yields no ports.
func ForwardablePorts(obj runtime.Object) []PortOption {
	switch o := obj.(type) {
	case *corev1.Pod:
		return containerPortOptions(o.Spec.Containers)
	case *corev1.Service:
		out := make([]PortOption, 0, len(o.Spec.Ports))
		for _, p := range o.Spec.Ports {
			target := p.TargetPort
			// The API server defaults an unset targetPort to port; an object
			// that never went through defaulting (a fixture, a dry-run
			// manifest) still means the same thing.
			if target.Type == intstr.Int && target.IntVal == 0 {
				target = intstr.FromInt32(p.Port)
			}
			out = append(out, PortOption{Port: p.Port, Name: p.Name, Protocol: string(p.Protocol), TargetPort: target})
		}
		return out
	case *appsv1.Deployment:
		return containerPortOptions(o.Spec.Template.Spec.Containers)
	default:
		return nil
	}
}

func containerPortOptions(containers []corev1.Container) []PortOption {
	var out []PortOption
	for _, c := range containers {
		for _, p := range c.Ports {
			out = append(out, PortOption{Port: p.ContainerPort, Name: p.Name, Protocol: string(p.Protocol), Container: c.Name})
		}
	}
	return out
}

// PodTargetPort resolves a Service targetPort against pod: a number is used
// as-is, and a name maps to the container port carrying that name (and
// protocol, when one is given) — the same lookup the endpoints controller
// does, so a named port can legitimately resolve to different numbers on
// different backing pods.
func PodTargetPort(pod *corev1.Pod, target intstr.IntOrString, protocol string) (int32, error) {
	if target.Type == intstr.Int {
		if target.IntVal <= 0 {
			return 0, fmt.Errorf("invalid target port %d", target.IntVal)
		}
		return target.IntVal, nil
	}
	if pod == nil {
		return 0, fmt.Errorf("no pod to resolve named port %q against", target.StrVal)
	}
	if protocol == "" {
		protocol = string(corev1.ProtocolTCP)
	}
	match := func(containers []corev1.Container, sidecarsOnly bool) (int32, bool) {
		for _, c := range containers {
			if sidecarsOnly && (c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways) {
				continue
			}
			for _, p := range c.Ports {
				proto := string(p.Protocol)
				if proto == "" {
					proto = string(corev1.ProtocolTCP)
				}
				if p.Name == target.StrVal && proto == protocol {
					return p.ContainerPort, true
				}
			}
		}
		return 0, false
	}
	if port, ok := match(pod.Spec.Containers, false); ok {
		return port, nil
	}
	if port, ok := match(pod.Spec.InitContainers, true); ok {
		return port, nil
	}
	return 0, fmt.Errorf("pod %q has no container port named %q", pod.Name, target.StrVal)
}

// forwardDialPort is the pod port a session dials: its own RemotePort for a
// Pod/Deployment option (zero TargetPort), else the Service targetPort
// resolved against the session's current backing pod.
func forwardDialPort(ctx context.Context, resolver PodResolver, namespace, pod string, remotePort int32, target intstr.IntOrString, protocol string) (int32, error) {
	if target.Type == intstr.Int && target.IntVal == 0 {
		return remotePort, nil
	}
	if target.Type == intstr.Int {
		return PodTargetPort(nil, target, protocol)
	}
	if resolver == nil {
		return 0, fmt.Errorf("no pod resolver to resolve named port %q", target.StrVal)
	}
	return resolver.ResolveForwardPort(ctx, namespace, pod, target, protocol)
}

// ForwardSession is one live/attempted port-forward, the row 13c's Forwards
// list renders.
type ForwardSession struct {
	ID             string
	Target         ForwardTarget
	ResolvedPod    string
	LocalPort      int
	RemotePort     int32
	RemotePortName string
	// TargetPort/Protocol carry a Service option's targetPort through to
	// every (re)dial; PodPort is the container port the current tunnel
	// actually dialed (RemotePort itself for Pod/Deployment targets, the
	// resolved targetPort for a Service — 0 until a dial resolves it).
	TargetPort     intstr.IntOrString
	Protocol       string
	PodPort        int32
	State          ForwardState
	Err            string
	Attempt        int
	NextRetryAt    time.Time
	StartedAt      time.Time
	LastActivityAt time.Time
}

// Tunnel is one dialed port-forward connection, returned by ForwardDialer.
type Tunnel interface {
	// Run blocks until the tunnel ends (lost connection or Close), calling
	// activity() for every new local connection it proxies — the only
	// per-connection signal client-go's portforward package exposes without
	// forking it, so ForwardManager treats it as a recency-of-use signal
	// (13c's TRAFFIC column) rather than a byte rate.
	Run(activity func()) error
	Close()
}

// ForwardDialer opens tunnels against namespace/pod. Bound to a fixed
// clientset/rest.Config snapshot at construction time (see
// NewSpdyForwardDialer) — never a live *Cluster reference — so an existing
// forward's reconnect attempts keep hitting the cluster it was started
// against even if the browsed context switches later.
type ForwardDialer interface {
	Dial(namespace, pod string, localPort int, remotePort int32) (Tunnel, error)
}

// PodResolver resolves a ForwardTarget to the pod name a dial should use:
// itself for a Pod target, or a matching backing pod for Service/Deployment.
type PodResolver interface {
	ResolveForwardPod(ctx context.Context, target ForwardTarget) (string, error)
	// ResolveForwardPort maps a Service targetPort onto pod's container
	// port (see PodTargetPort). Re-run against every newly resolved pod,
	// since a named port may map to different numbers on different pods.
	ResolveForwardPort(ctx context.Context, namespace, pod string, target intstr.IntOrString, protocol string) (int32, error)
}

// ForwardPodChecker is the optional watch-reconciliation seam implemented by
// the real resolver. A port-forward's upgraded SPDY session can remain open
// after its Pod is deleted, so Tunnel.Run alone is not a reliable liveness
// signal. ForwardManager checks the resolved Pod whenever the Cluster's Pod
// informer emits a change and closes the tunnel when it has gone away; the
// normal reconnect loop then re-resolves Service/Deployment targets.
type ForwardPodChecker interface {
	ForwardPodAvailable(ctx context.Context, namespace, pod string) (bool, error)
}

// --- real (SPDY) dialer ---

type spdyDialer struct {
	clientset kubernetes.Interface
	restCfg   *rest.Config
	gate      *authenticationGate
	watcher   *watchObserver
}

// NewSpdyForwardDialer builds the real ForwardDialer, snapshotting clientset
// and restCfg at call time (the caller must build this fresh from the
// active *Cluster right before Start, not cache it across context
// switches).
func NewSpdyForwardDialer(clientset kubernetes.Interface, restCfg *rest.Config) ForwardDialer {
	return spdyDialer{clientset: clientset, restCfg: restCfg}
}

// NewForwardDialer snapshots the active context's authenticated SPDY config
// while retaining the same outer request gate as ordinary REST traffic.
func (c *Cluster) NewForwardDialer() ForwardDialer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return spdyDialer{clientset: c.clientset, restCfg: c.credentialCfg, gate: c.authGate, watcher: c.watcher}
}

func (d spdyDialer) Dial(namespace, pod string, localPort int, remotePort int32) (Tunnel, error) {
	req := d.clientset.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward")
	transport, upgrader, err := spdy.RoundTripperFor(d.restCfg)
	if err != nil {
		return nil, err
	}
	if d.gate != nil {
		transport = &observingRoundTripper{next: transport, gate: d.gate, observer: d.watcher}
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", req.URL())
	return &spdyTunnel{dialer: dialer, localPort: localPort, remotePort: remotePort}, nil
}

type spdyTunnel struct {
	dialer     httpstreamDialer
	localPort  int
	remotePort int32

	mu     sync.Mutex
	stopCh chan struct{}
}

// httpstreamDialer is the subset of httpstream.Dialer portforward.New needs
// — declared locally so this file only imports client-go/transport/spdy for
// the concrete constructor, not apimachinery's httpstream package too.
type httpstreamDialer = httpstream.Dialer

func (t *spdyTunnel) Run(activity func()) error {
	t.mu.Lock()
	if t.stopCh != nil {
		t.mu.Unlock()
		return fmt.Errorf("tunnel already running")
	}
	stopCh := make(chan struct{})
	t.stopCh = stopCh
	t.mu.Unlock()

	readyCh := make(chan struct{})
	out := &activityWriter{activity: activity}
	pf, err := portforward.New(t.dialer, []string{fmt.Sprintf("%d:%d", t.localPort, t.remotePort)}, stopCh, readyCh, out, out)
	if err != nil {
		return err
	}
	return pf.ForwardPorts()
}

func (t *spdyTunnel) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopCh == nil {
		return
	}
	select {
	case <-t.stopCh:
	default:
		close(t.stopCh)
	}
}

// activityWriter scans portforward's out/errOut stream for "Handling
// connection" lines (see spdyTunnel.Run) and reports each as one activity
// tick.
type activityWriter struct{ activity func() }

func (w *activityWriter) Write(p []byte) (int, error) {
	if w.activity != nil && bytes.Contains(p, []byte("Handling connection")) {
		w.activity()
	}
	return len(p), nil
}

// --- real pod resolver ---

type clientsetPodResolver struct{ clientset kubernetes.Interface }

// NewClientsetPodResolver builds the real PodResolver, snapshotting
// clientset the same way NewSpdyForwardDialer does.
func NewClientsetPodResolver(clientset kubernetes.Interface) PodResolver {
	return clientsetPodResolver{clientset: clientset}
}

func (r clientsetPodResolver) ForwardPodAvailable(ctx context.Context, namespace, pod string) (bool, error) {
	got, err := r.clientset.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return got.DeletionTimestamp == nil && got.Status.Phase == corev1.PodRunning, nil
}

func (r clientsetPodResolver) ResolveForwardPod(ctx context.Context, target ForwardTarget) (string, error) {
	switch target.Kind {
	case KindPod:
		pod, err := r.clientset.CoreV1().Pods(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		return pod.Name, nil
	case KindService:
		svc, err := r.clientset.CoreV1().Services(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		if len(svc.Spec.Selector) == 0 {
			return "", fmt.Errorf("service %q has no selector to resolve a backing pod", target.Name)
		}
		return r.firstRunningPod(ctx, target.Namespace, labels.SelectorFromSet(svc.Spec.Selector))
	case KindDeployment:
		dep, err := r.clientset.AppsV1().Deployments(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
		if err != nil {
			return "", err
		}
		return r.firstRunningPod(ctx, target.Namespace, sel)
	default:
		return "", fmt.Errorf("port-forward is not supported for kind %s", target.Kind)
	}
}

func (r clientsetPodResolver) ResolveForwardPort(ctx context.Context, namespace, pod string, target intstr.IntOrString, protocol string) (int32, error) {
	if target.Type == intstr.Int {
		return PodTargetPort(nil, target, protocol)
	}
	got, err := r.clientset.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	return PodTargetPort(got, target, protocol)
}

func (r clientsetPodResolver) firstRunningPod(ctx context.Context, namespace string, sel labels.Selector) (string, error) {
	pods, err := r.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return "", err
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			return p.Name, nil
		}
	}
	if len(pods.Items) > 0 {
		return pods.Items[0].Name, nil
	}
	return "", fmt.Errorf("no pods match the target's selector")
}

// PortForwardCommandString renders the kubectl invocation dialing pod
// directly would run — 13a's "will run" documentation line. The real dial
// path goes through client-go, not this subprocess, but the equivalent
// command is exact (same pod, same ports).
func PortForwardCommandString(namespace, pod string, localPort int, remotePort int32) string {
	return fmt.Sprintf("kubectl port-forward pod/%s %d:%d -n %s", pod, localPort, remotePort, namespace)
}

// ServicePortForwardCommandString is PortForwardCommandString's Service
// form: kubectl itself translates servicePort to the backing pod's
// targetPort, so it stays exact when kute couldn't resolve that port.
func ServicePortForwardCommandString(namespace, service string, localPort int, servicePort int32) string {
	return fmt.Sprintf("kubectl port-forward svc/%s %d:%d -n %s", service, localPort, servicePort, namespace)
}

// --- ForwardManager ---

// ForwardManager owns every live forwarding session for the app's lifetime —
// constructed once at the composition root, shared by every screen that
// needs it, and never rebuilt on context switch (see the package doc).
type ForwardManager struct {
	mu       sync.Mutex
	sessions map[string]*forwardEntry
	events   chan struct{}
	nextID   int
}

type forwardEntry struct {
	session  ForwardSession
	dialer   ForwardDialer
	resolver PodResolver
	ctx      context.Context
	cancel   context.CancelFunc
	tunnel   Tunnel
	checking bool
}

// NewForwardManager builds an empty manager.
func NewForwardManager() *ForwardManager {
	return &ForwardManager{sessions: map[string]*forwardEntry{}, events: make(chan struct{}, 1)}
}

// Events fires (best-effort, coalesced) whenever any session's state
// changes — the composition root forwards this into the Bubble Tea program
// as a kube.ResourceChangedMsg{Kind: KindForward} (see internal/app), so
// Forwards reloads through the exact same path every other kind's watch
// events already use.
func (m *ForwardManager) Events() <-chan struct{} { return m.events }

func (m *ForwardManager) notify() {
	select {
	case m.events <- struct{}{}:
	default:
	}
}

// List returns every session, sorted by ID for a stable render order.
func (m *ForwardManager) List() []ForwardSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ForwardSession, 0, len(m.sessions))
	for _, e := range m.sessions {
		out = append(out, e.session)
	}
	slices.SortFunc(out, func(a, b ForwardSession) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// ForwardObject adapts a ForwardSession to runtime.Object so Forwards can
// flow through the same resources.List/Project pipeline as real API
// objects (docs/design README.md §13c: "a registry kind, not a bespoke
// screen").
type ForwardObject struct {
	metav1.TypeMeta
	Session ForwardSession
}

// DeepCopyObject satisfies runtime.Object. ForwardSession has no pointer
// fields, so a shallow copy is a full deep copy.
func (o *ForwardObject) DeepCopyObject() runtime.Object {
	cp := *o
	return &cp
}

// ListRaw wraps List for resources.RawLister's shape (namespace is ignored
// — forwards are never namespace-filtered, docs/design README.md §13c).
func (m *ForwardManager) ListRaw() []runtime.Object {
	sessions := m.List()
	out := make([]runtime.Object, len(sessions))
	for i, s := range sessions {
		out[i] = &ForwardObject{Session: s}
	}
	return out
}

// Start opens a new forwarding session and returns its initial snapshot
// immediately; the dial itself (and any reconnect loop) runs in the
// background. dialer/resolver are the caller's already-snapshotted pair
// (NewSpdyForwardDialer/NewClientsetPodResolver for a real cluster, or
// kube/fake's equivalents in --demo).
func (m *ForwardManager) Start(dialer ForwardDialer, resolver PodResolver, target ForwardTarget, resolvedPod string, localPort int, remotePort int32, remotePortName string) ForwardSession {
	return m.StartPort(dialer, resolver, target, resolvedPod, localPort, PortOption{Port: remotePort, Name: remotePortName})
}

// StartPort is Start for a picked PortOption: a Service option's TargetPort
// rides along so every (re)dial reaches the backing pod's container port
// rather than the Service port the user picked.
func (m *ForwardManager) StartPort(dialer ForwardDialer, resolver PodResolver, target ForwardTarget, resolvedPod string, localPort int, port PortOption) ForwardSession {
	m.mu.Lock()
	m.nextID++
	id := fmt.Sprintf("fwd-%d", m.nextID)
	now := time.Now()
	session := ForwardSession{
		ID: id, Target: target, ResolvedPod: resolvedPod,
		LocalPort: localPort, RemotePort: port.Port, RemotePortName: port.Name,
		TargetPort: port.TargetPort, Protocol: port.Protocol,
		State: ForwardActive, StartedAt: now, LastActivityAt: now,
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.sessions[id] = &forwardEntry{session: session, dialer: dialer, resolver: resolver, ctx: ctx, cancel: cancel}
	m.mu.Unlock()

	m.notify()
	go m.run(ctx, id, resolvedPod)
	return session
}

// run is one session's dial/reconnect loop: dial, block in Run() until the
// tunnel ends, mark Reconnecting with a backoff, re-resolve the target pod
// (Service/Deployment only — a Pod target that's gone stays gone), and
// retry. Returns as soon as ctx is cancelled (Stop/Restart).
func (m *ForwardManager) run(ctx context.Context, id, initialPod string) {
	pod := initialPod
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		entry, ok := m.sessions[id]
		if !ok {
			m.mu.Unlock()
			return
		}
		dialer, resolver, target, localPort := entry.dialer, entry.resolver, entry.session.Target, entry.session.LocalPort
		remotePort, targetPort, protocol := entry.session.RemotePort, entry.session.TargetPort, entry.session.Protocol
		m.mu.Unlock()

		var tunnel Tunnel
		var err error
		var podPort int32
		if dialer == nil {
			err = fmt.Errorf("no forward dialer configured")
		} else {
			// Resolved per dial, against this attempt's pod: after a
			// reconnect re-resolution a named targetPort can map to a
			// different number on the replacement pod.
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			podPort, err = forwardDialPort(pctx, resolver, target.Namespace, pod, remotePort, targetPort, protocol)
			cancel()
			if err == nil {
				tunnel, err = dialer.Dial(target.Namespace, pod, localPort, podPort)
			}
		}
		if err == nil {
			m.mu.Lock()
			// A Stop or Restart can land while Dial is in flight, and this
			// goroutine has been the doomed one ever since. Publishing the
			// handle now would stomp the *replacement* goroutine's tunnel
			// with this stale one: the live tunnel becomes unreachable and is
			// never closed, leaking an SPDY connection and a local listening
			// port for every restart that hits the window. The other two
			// ctx.Err() checks in this loop bracket Run(), not Dial().
			if ctx.Err() != nil {
				m.mu.Unlock()
				tunnel.Close()
				return
			}
			if e, ok := m.sessions[id]; ok {
				e.tunnel = tunnel
				e.session.State = ForwardActive
				e.session.ResolvedPod = pod
				e.session.PodPort = podPort
				e.session.Err = ""
				e.session.Attempt = 0
				e.session.LastActivityAt = time.Now()
			}
			m.mu.Unlock()
			m.notify()
			attempt = 0
			err = tunnel.Run(func() { m.touchActivity(id) })
		}
		if ctx.Err() != nil {
			return
		}

		attempt++
		nextRetry := time.Now().Add(forwardBackoff(attempt))
		m.mu.Lock()
		if e, ok := m.sessions[id]; ok {
			e.session.State = ForwardReconnecting
			e.session.Attempt = attempt
			e.session.NextRetryAt = nextRetry
			if err != nil {
				e.session.Err = err.Error()
			}
		}
		m.mu.Unlock()
		m.notify()

		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(nextRetry)):
		}

		m.mu.Lock()
		entry, ok = m.sessions[id]
		if !ok {
			m.mu.Unlock()
			return
		}
		resolver, target = entry.resolver, entry.session.Target
		m.mu.Unlock()
		if target.Kind != KindPod && resolver != nil {
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if newPod, rerr := resolver.ResolveForwardPod(rctx, target); rerr == nil {
				pod = newPod
			}
			cancel()
		}
	}
}

func (m *ForwardManager) touchActivity(id string) {
	m.mu.Lock()
	if e, ok := m.sessions[id]; ok {
		e.session.LastActivityAt = time.Now()
	}
	m.mu.Unlock()
}

// ReconcilePodTargets reacts to a Pod informer event without polling. Real
// Service/Deployment forwards validate their currently resolved Pod in the
// background; a missing/terminating Pod closes the active tunnel, which feeds
// the existing reconnect/backoff/re-resolution loop. API errors are ignored:
// an outage is not evidence that the Pod disappeared, and closing a working
// tunnel would only make the outage worse.
func (m *ForwardManager) ReconcilePodTargets() {
	type check struct {
		id        string
		entry     *forwardEntry
		checker   ForwardPodChecker
		ctx       context.Context
		namespace string
		pod       string
	}

	m.mu.Lock()
	checks := make([]check, 0, len(m.sessions))
	for id, entry := range m.sessions {
		if entry.checking || entry.session.Target.Kind == KindPod || entry.session.ResolvedPod == "" {
			continue
		}
		checker, ok := entry.resolver.(ForwardPodChecker)
		if !ok {
			continue
		}
		entry.checking = true
		checks = append(checks, check{
			id: id, entry: entry, checker: checker, ctx: entry.ctx,
			namespace: entry.session.Target.Namespace, pod: entry.session.ResolvedPod,
		})
	}
	m.mu.Unlock()

	for _, candidate := range checks {
		go m.reconcilePodTarget(candidate.ctx, candidate.id, candidate.entry, candidate.checker, candidate.namespace, candidate.pod)
	}
}

func (m *ForwardManager) reconcilePodTarget(sessionCtx context.Context, id string, expected *forwardEntry, checker ForwardPodChecker, namespace, pod string) {
	ctx, cancel := context.WithTimeout(sessionCtx, 10*time.Second)
	available, err := checker.ForwardPodAvailable(ctx, namespace, pod)
	cancel()

	m.mu.Lock()
	entry, ok := m.sessions[id]
	if !ok || entry != expected {
		m.mu.Unlock()
		return
	}
	entry.checking = false
	if err != nil || available || entry.session.ResolvedPod != pod {
		m.mu.Unlock()
		return
	}
	tunnel := entry.tunnel
	m.mu.Unlock()

	if tunnel != nil {
		tunnel.Close()
	}
}

// Stop ends and forgets id — a stopped forward is removed outright (13c's
// 'x', "executes immediately (reversible)": reversible via starting a new
// one, not by reviving the old row).
func (m *ForwardManager) Stop(id string) {
	m.mu.Lock()
	entry, ok := m.sessions[id]
	if ok {
		// Cancel while the entry is still discoverable. A Dial or resolver
		// released concurrently can now observe cancellation before it can
		// publish a handle against a session which has disappeared.
		entry.cancel()
		delete(m.sessions, id)
	}
	var tunnel Tunnel
	if ok {
		tunnel = entry.tunnel
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	if tunnel != nil {
		tunnel.Close()
	}
	m.notify()
}

// StopAll ends every session (13c's 'X', the one forward verb with an
// inline y/N — see verbs.StopAllForwards).
func (m *ForwardManager) StopAll() {
	m.mu.Lock()
	entries := make([]*forwardEntry, 0, len(m.sessions))
	for _, e := range m.sessions {
		e.cancel()
		entries = append(entries, e)
	}
	m.sessions = map[string]*forwardEntry{}
	m.mu.Unlock()
	for _, e := range entries {
		if e.tunnel != nil {
			e.tunnel.Close()
		}
	}
	if len(entries) > 0 {
		m.notify()
	}
}

// Restart force-reconnects id immediately, bypassing any pending backoff.
func (m *ForwardManager) Restart(id string) {
	m.mu.Lock()
	entry, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	pod := entry.session.ResolvedPod
	entry.cancel()
	// Closed after the unlock below, not here: Tunnel.Close takes the
	// tunnel's own lock, and Stop/StopAll both already release m.mu before
	// cancelling and closing. Restart was the one that did not.
	old := entry.tunnel
	ctx, cancel := context.WithCancel(context.Background())
	entry.ctx = ctx
	entry.cancel = cancel
	entry.tunnel = nil
	entry.session.State = ForwardActive
	entry.session.Attempt = 0
	entry.session.Err = ""
	m.mu.Unlock()
	if old != nil {
		old.Close()
	}
	m.notify()
	go m.run(ctx, id, pod)
}

// forwardBackoff is the reconnect delay schedule: 2s per attempt, capped at
// 30s — the same order of magnitude as the 4a connection-loss backoff.
func forwardBackoff(attempt int) time.Duration {
	d := time.Duration(attempt) * 2 * time.Second
	if d > 30*time.Second {
		return 30 * time.Second
	}
	if d <= 0 {
		return 2 * time.Second
	}
	return d
}
