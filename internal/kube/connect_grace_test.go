package kube

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// connectGrace exists for "a link we know works is busy". These tests pin
// the "we know works" half: a cluster that has never answered — down, or
// behind a VPN that dropped — times out exactly like a congested one, and
// must reach 4c on the first failed probe instead of after the 90 s window.

func TestRecordPingReportsTimeoutWhenNeverReached(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.recordPing(pingTimeout, timeoutErr(), false, false, h.startedAt.Add(pingInterval+pingTimeout))

	if got := h.get(); got.Phase != ConnReconnecting {
		t.Fatalf("Phase = %v, want Reconnecting — a never-reached cluster's timeout is an outage, not congestion", got.Phase)
	}
}

func TestOnWatchErrorReportsTimeoutWhenNeverReached(t *testing.T) {
	t.Parallel()
	h := newHealth()
	// What a reflector LIST against a blackholed IP reports.
	h.onWatchError(timeoutErr(), false, false, h.startedAt.Add(10*time.Second))

	if got := h.get(); got.Phase != ConnReconnecting {
		t.Fatalf("Phase = %v, want Reconnecting on a LIST timeout against a never-reached cluster", got.Phase)
	}
}

// TestListBurstDoesNotExtendGraceWhenNeverReached: every kind the user opens
// while staring at the spinner starts an informer and re-arms the window. For
// a never-reached cluster that used to push 4c out by another 90 s each time.
func TestListBurstDoesNotExtendGraceWhenNeverReached(t *testing.T) {
	t.Parallel()
	h := newHealth()
	h.noteListBurst()
	h.recordPing(pingTimeout, timeoutErr(), false, false, time.Now())

	if got := h.get(); got.Phase != ConnReconnecting {
		t.Fatalf("Phase = %v, want Reconnecting — a fresh informer start is no evidence the link works", got.Phase)
	}
}

// graceCluster is a Cluster whose every request goes through the real
// observingRoundTripper to rt, with one started-but-unsynced informer so the
// connect-grace window is open on the sync side.
func graceCluster(t *testing.T, rt roundTripFunc) (*Cluster, healthProbeSnapshot) {
	t.Helper()
	gate := &authenticationGate{staticCredentials: true}
	cs, err := kubernetes.NewForConfig(&rest.Config{
		Host:      "https://cluster.example.test",
		Transport: &observingRoundTripper{next: rt, gate: gate},
	})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	unsynced := newTypedFactory(fake.NewClientset()).Core().V1().Pods().Informer()
	c := &Cluster{
		clientset:     cs,
		authGate:      gate,
		generation:    1,
		health:        newHealth(),
		kindInformers: map[scopeKey]cache.SharedIndexInformer{{KindPod, ""}: unsynced},
	}
	return c, healthProbeSnapshot{clientset: cs, gate: gate, generation: 1, ctx: t.Context()}
}

// livezTimesOut answers every request but /livez with 200 and times /livez
// out — a link that works but is saturated by an initial LIST.
func livezTimesOut(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/livez" {
		return nil, timeoutErr()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader("{}")),
		Request:    req,
	}, nil
}

func TestPingTimeoutAgainstNeverReachedClusterGoesOffline(t *testing.T) {
	t.Parallel()
	c, probe := graceCluster(t, func(*http.Request) (*http.Response, error) {
		return nil, timeoutErr() // blackholed: nothing ever answers
	})
	c.ping(probe)

	if got := c.ConnState(); !got.Offline() {
		t.Fatalf("ConnState = %+v, want offline on the first timed-out ping of a never-reached cluster", got)
	}
}

func TestPingTimeoutAfterReachedKeepsGrace(t *testing.T) {
	t.Parallel()
	c, probe := graceCluster(t, livezTimesOut)
	c.noteReached() // e.g. a /livez success or a cache event earlier this session
	c.ping(probe)

	if got := c.ConnState(); got.Phase != ConnConnected {
		t.Fatalf("Phase = %v, want Connected — a reached cluster's slow initial LIST is congestion", got.Phase)
	}
}

// TestPingTimeoutAfterAnyResponseKeepsGrace is the cold-launch slow-link
// case: the initial LIST's response headers are back, its body is still
// crawling down the tunnel, no cache has filled and no ping has succeeded.
// The response alone proves the link works.
func TestPingTimeoutAfterAnyResponseKeepsGrace(t *testing.T) {
	t.Parallel()
	c, probe := graceCluster(t, livezTimesOut)
	if err := c.clientset.Discovery().RESTClient().Get().AbsPath("/api/v1/pods").Do(t.Context()).Error(); err != nil {
		t.Fatalf("list: %v", err)
	}
	c.ping(probe)

	if got := c.ConnState(); got.Phase != ConnConnected {
		t.Fatalf("Phase = %v, want Connected — a server that has answered once is reachable", got.Phase)
	}
}

// TestSwitchContextResetsReachedForConnectGrace: proof that the context being
// left was reachable says nothing about the one being switched into. A switch
// into a blackholed context must go offline on its first timed-out ping.
func TestSwitchContextResetsReachedForConnectGrace(t *testing.T) {
	release := make(chan struct{})
	blackhole := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(blackhole.Close)
	t.Cleanup(func() { close(release) })

	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["a"] = &clientcmdapi.Cluster{Server: blackhole.URL}
	cfg.Clusters["b"] = &clientcmdapi.Cluster{Server: blackhole.URL}
	cfg.AuthInfos["u"] = &clientcmdapi.AuthInfo{}
	cfg.Contexts["a"] = &clientcmdapi.Context{Cluster: "a", AuthInfo: "u", Namespace: "default"}
	cfg.Contexts["b"] = &clientcmdapi.Context{Cluster: "b", AuthInfo: "u", Namespace: "default"}
	cfg.CurrentContext = "a"
	path := filepath.Join(t.TempDir(), "config")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	SetKubeconfigPath(path)
	t.Cleanup(func() { SetKubeconfigPath("") })

	c, err := NewClusterForContext("a")
	if err != nil {
		t.Fatalf("NewClusterForContext: %v", err)
	}
	t.Cleanup(c.Stop)
	// Context a was reachable: both kinds of evidence are latched.
	c.noteReached()
	c.authGate.responded.Store(true)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_ = c.SwitchContext(ctx, "b", "") // b never answers, so its caches never sync

	c.mu.Lock()
	probeCtx, cancelProbe := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancelProbe()
	probe := healthProbeSnapshot{clientset: c.clientset, gate: c.authGate, generation: c.generation, ctx: probeCtx}
	c.mu.Unlock()
	c.ping(probe)

	if got := c.ConnState(); !got.Offline() {
		t.Fatalf("ConnState = %+v, want offline — the previous context's reachability must not buy the new one grace", got)
	}
}
