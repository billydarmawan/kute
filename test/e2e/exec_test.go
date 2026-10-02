//go:build e2e

package e2e

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// This file is the reason the suite runs on kind rather than kwok. A fake
// kubelet can serve a pod list; only a real one can hand back a shell probe's
// exit status or carry bytes through a port-forward. Logs (§5b) are covered in
// flow_test.go; these are the other two.

// TestExecPickerDetectsShellsOnARealContainer covers §10a as far as a headless
// harness honestly can. The picker itself is fully exercisable: it opens
// because the api pod has two containers (a single-container pod execs
// straight through without it), and its shells column comes from a real
// exec round-trip against the running container — not from anything the
// object says about itself.
//
// It deliberately stops short of ↵. That hands the tty to kubectl via
// tea.ExecProcess, and there is no tty here to hand over; the fixture's job
// is to prove the probe and the picker work against a real kubelet.
func TestExecPickerDetectsShellsOnARealContainer(t *testing.T) {
	RequireCluster(t)
	a := Launch(t)
	a.WaitFor("api-", Connect)

	pod := a.selectAPIPod(t)
	a.Press("x")

	// Both containers, named from the pod spec, with the will-run line §10a
	// calls copyable documentation. That line is hard-wrapped to the panel,
	// and its global flags come before the verb — pinning kubectl to the
	// context kute is on, not the kubeconfig's current one — so it's read
	// ignoring wrapping rather than as one "kubectl exec" substring.
	a.WaitForAll(Settle, "exec", pod, "2 containers", "server", "sidecar", "will run")
	a.WaitForWrapped("kubectl --kubeconfig", Settle)
	a.WaitForWrapped("--context "+ContextName(t)+" exec -it "+pod, Settle)

	// The shells column resolves to a real answer for both containers.
	// busybox has sh, and the only way to know that is to have run something
	// inside the container.
	//
	// Read as the row's last column, and waited for as that value: a frame
	// search for "sh" would pass with the probe entirely broken, because the
	// image is pinned by digest and every one of these rows contains
	// "sha256". Waiting for the in-flight marker to disappear is no good
	// either — the panel truncates "checking…" to "checkin...", so the whole
	// word is never on screen to wait on.
	frame, ok := a.poll(func(f string) bool {
		return lastColumn(f, "server") == "sh" && lastColumn(f, "sidecar") == "sh"
	}, Settle)
	if !ok {
		t.Fatalf("shells never resolved to sh for both containers (server=%q sidecar=%q):\n%s",
			lastColumn(frame, "server"), lastColumn(frame, "sidecar"), frame)
	}

	a.Esc()
	a.WaitFor("api-", Settle)
}

// lastColumn returns the final whitespace-delimited token on the row naming
// row, with the panel's right border discarded — the way to read a
// right-aligned column without pinning the x positions a resize would move.
func lastColumn(frame, row string) string {
	for _, line := range strings.Split(frame, "\n") {
		if !strings.Contains(line, row) {
			continue
		}
		fields := strings.Fields(strings.Trim(strings.TrimSpace(line), "│ "))
		if len(fields) == 0 {
			continue
		}
		return fields[len(fields)-1]
	}
	return ""
}

// localPortRe reads the picker's chosen local endpoint off the screen rather
// than assuming a port: kute picks a free one, and a test that hard-coded
// 8080 would pass or fail on what else happens to be listening.
var localPortRe = regexp.MustCompile(`localhost:(\d+)`)

// TestPortForwardCarriesRealTraffic is §13a end to end. The api container
// runs busybox httpd on 8080 specifically so this test can prove the tunnel
// by fetching through it — a forward that reports itself started and moves no
// bytes is exactly the failure a screen-only assertion cannot see.
func TestPortForwardCarriesRealTraffic(t *testing.T) {
	RequireCluster(t)
	a := Launch(t)
	a.WaitFor("api-", Connect)

	pod := a.selectAPIPod(t)
	a.Press("f")

	// The candidate ports are discovered from the object: the api container
	// declares 8080 as "http".
	frame := a.WaitForAll(Settle, "forward", pod, "8080", "container server", "kubectl port-forward")

	match := localPortRe.FindStringSubmatch(frame)
	if match == nil {
		t.Fatalf("no localhost:<port> on the forward picker:\n%s", frame)
	}
	local := match[1]

	a.Enter()

	// The header chip is the only ambient signal a forward is running — a
	// failing forward may change its colour and nothing else, never a modal
	// over an unrelated screen.
	a.WaitFor("⇄", Settle)

	// The actual point: bytes move. The forward runs in this process, so the
	// tunnel is reachable on localhost here.
	body := fetchThrough(t, local)
	if !strings.Contains(body, "KUTE-E2E-FORWARD-OK") {
		t.Fatalf("fetched %q through the forward, want the fixture's index.html", body)
	}
}

// fetchThrough GETs the forwarded local port, retrying briefly: the tunnel is
// established asynchronously after ↵, so the first connection can beat it.
func fetchThrough(t *testing.T, port string) string {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(Settle)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://localhost:" + port + "/")
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		return string(body)
	}
	t.Fatalf("nothing served through the forward on localhost:%s within %s: %v", port, Settle, lastErr)
	return ""
}
