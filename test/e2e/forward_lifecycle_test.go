//go:build e2e

package e2e

import (
	"context"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var resolvedForwardPodRe = regexp.MustCompile(`pod/(api-[a-z0-9-]+)`)

func TestForwardListenerClosesOnApplicationQuit(t *testing.T) {
	RequireCluster(t)
	a := Launch(t)
	a.WaitFor("api-", Connect)
	local := startSelectedPodForward(t, a)
	if body := fetchThrough(t, local); !strings.Contains(body, "KUTE-E2E-FORWARD-OK") {
		t.Fatalf("forward returned %q", body)
	}

	a.Quit()
	WaitForTCPRefused(t, net.JoinHostPort("127.0.0.1", local), 5*time.Second)
}

func TestForwardScreenStopRemovesRowAndListener(t *testing.T) {
	RequireCluster(t)
	a := Launch(t)
	a.WaitFor("api-", Connect)
	local := startSelectedPodForward(t, a)
	_ = fetchThrough(t, local)

	a.gotoKind(t, "forwards", "Forwards")
	a.WaitFor("localhost:"+local, Settle)
	a.Press("x")
	a.WaitGone("localhost:"+local, Settle)
	WaitForTCPRefused(t, net.JoinHostPort("127.0.0.1", local), 5*time.Second)
}

func TestServiceForwardRebindsToReplacementAndStopCancelsRetry(t *testing.T) {
	RequireCluster(t)
	a := Launch(t)
	a.WaitFor("api-", Connect)
	a.gotoKind(t, "services", "Services")
	a.filterTo(t, "api")
	a.Press("f")
	frame := a.WaitForAll(Settle, "forward › api", "service · "+Namespace, "8080", "kubectl port-forward")
	match := localPortRe.FindStringSubmatch(frame)
	if match == nil {
		t.Fatalf("no local port in picker:\n%s", frame)
	}
	local := match[1]
	a.Enter()
	a.WaitFor("⇄", Settle)
	_ = fetchThrough(t, local)

	a.gotoKind(t, "forwards", "Forwards")
	frame = a.WaitFor("service/api", Settle)
	oldPod := resolvedPodFromFrame(t, frame)
	client := e2eClientset(t)
	ctx := context.Background()
	zero := int64(0)
	if err := client.CoreV1().Pods(Namespace).Delete(ctx, oldPod, metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil {
		t.Fatalf("deleting resolved pod %s: %v", oldPod, err)
	}

	a.WaitFor("retry", Settle)
	_ = fetchThrough(t, local)
	frame, ok := a.poll(func(f string) bool {
		return strings.Contains(f, "service/api") && strings.Contains(f, "→ pod/api-") && !strings.Contains(f, "pod/"+oldPod)
	}, Settle)
	if !ok {
		t.Fatalf("forward never resolved a replacement for %s:\n%s", oldPod, frame)
	}

	newPod := resolvedPodFromFrame(t, frame)
	if err := client.CoreV1().Pods(Namespace).Delete(ctx, newPod, metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil {
		t.Fatalf("deleting replacement pod %s: %v", newPod, err)
	}
	a.WaitFor("retry", Settle)
	a.Press("x")
	a.WaitGone("localhost:"+local, Settle)
	assertTCPRemainsRefused(t, net.JoinHostPort("127.0.0.1", local), 4*time.Second)
}

// TestUndeclaredPortForwardsThroughTheFreeRow is the fixture's second httpd
// (8081, deliberately absent from the Deployment's `ports`) and the row that
// exists for it: `f`, ↓ to `+ other port`, type 8081, ↵ — real bytes back
// through a port nothing on the object ever declared. Fetching, not the
// screen, is the assertion: a panel can claim a forward started and still
// carry nothing.
func TestUndeclaredPortForwardsThroughTheFreeRow(t *testing.T) {
	RequireCluster(t)
	a := Launch(t)
	a.WaitFor("api-", Connect)
	a.selectAPIPod(t)
	a.Press("f")
	// The free-port row, not the header: a pod with no containerPorts at all
	// would also render "no declared ports", and this api pod declares 8080.
	frame := a.WaitFor("+ other port", Settle)
	if !strings.Contains(frame, "8080/http") {
		t.Fatalf("expected the declared port still listed:\n%s", frame)
	}
	a.Press("down")
	a.Type("8081")
	// "other port  8081" is the row's own render, and only the whole buffer
	// matches it — an untyped row says "e.g. 8080:3000" instead. The last
	// localhost: on the panel is the free row's derived local port (the
	// declared row's own came first), which is what actually gets bound.
	frame = a.WaitFor("other port  8081", Settle)
	matches := localPortRe.FindAllStringSubmatch(frame, -1)
	if len(matches) == 0 {
		t.Fatalf("no local port in picker:\n%s", frame)
	}
	local := matches[len(matches)-1][1]
	a.Enter()
	a.WaitFor("⇄", Settle)
	if body := fetchThrough(t, local); !strings.Contains(body, "KUTE-E2E-FORWARD-OK") {
		t.Fatalf("forward to the undeclared 8081 returned %q", body)
	}

	a.gotoKind(t, "forwards", "Forwards")
	a.WaitFor("localhost:"+local, Settle)
}

func startSelectedPodForward(t *testing.T, a *App) string {
	t.Helper()
	pod := a.selectAPIPod(t)
	a.Press("f")
	frame := a.WaitForAll(Settle, "forward", pod, "8080", "kubectl port-forward")
	match := localPortRe.FindStringSubmatch(frame)
	if match == nil {
		t.Fatalf("no local port in picker:\n%s", frame)
	}
	a.Enter()
	a.WaitFor("⇄", Settle)
	return match[1]
}

func resolvedPodFromFrame(t *testing.T, frame string) string {
	t.Helper()
	match := resolvedForwardPodRe.FindStringSubmatch(frame)
	if match == nil {
		t.Fatalf("no resolved API pod in forwards frame:\n%s", frame)
	}
	return match[1]
}

func assertTCPRemainsRefused(t *testing.T, address string, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("stopped forward revived listener %s", address)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
