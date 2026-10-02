//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
)

// TestPodDebugPanelStagesCopyMode covers §41c, the branch 'x' takes when the
// pod cannot hold a shell open.
//
// The routing decision is the point. browse's beginExecOrDebug reads the
// pod's own phase and container states and skips the shell probe entirely
// for a pod that won't stay running — so this needs a pod that is durably
// not running, which a real kubelet supplies and a fake can only assert.
// An image reference nothing can pull gives exactly that: Pending forever,
// container Waiting forever, no controller to reconcile it away.
func TestPodDebugPanelStagesCopyMode(t *testing.T) {
	RequireCluster(t)
	const name = "phase3-debug-pending"
	client := mutationClient(t)
	createPodWithImage(t, client, name, "kute-e2e.invalid/nothing-here:v1")

	a := Launch(t)
	a.WaitFor(name, Connect)
	a.filterTo(t, name)
	a.selectRow(t, name)
	a.Press("x")

	// The panel, in copy mode, named for the pod — "copy mode" is the
	// header's own right-hand note and appears on no other target.
	a.WaitForAll(Settle, "debug", name, "copy mode")

	// §41c's mode selector: attach is hard-disabled, and it says why in the
	// row itself rather than merely dimming. That reason is the pod's real
	// state, read from the cluster.
	a.WaitForAll(Settle, "attach ephemeral", "copy pod", "needs a running container")

	// The fields are the copy set — entrypoint is the one that actually
	// breaks the loop, and no other target renders it.
	a.WaitForAll(Settle, "copy name", "entrypoint", "processes")

	// The cost, stated: a copy is a real pod the user now owns.
	a.WaitForWrapped("creates a real pod", Settle)

	// And the command, before it runs: --copy-to is what distinguishes a
	// copy launch from an attach in the one line the user can actually
	// check. The global flags come before the verb, pinning the subprocess
	// to the context kute is on rather than the kubeconfig's current one.
	a.WaitForWrapped("will run", Settle)
	a.WaitForWrapped("kubectl --kubeconfig", Settle)
	a.WaitForWrapped("--context "+ContextName(t)+" debug -it "+name, Settle)
	a.WaitForWrapped("--copy-to", Settle)

	// Nothing ran. A commit is a tea.ExecProcess handoff, which suspends the
	// program and stops it producing frames at all — so a panel that
	// launched on open fails this fence rather than merely looking wrong.
	if latency := a.InputFence(); latency > 2*time.Second {
		t.Fatalf("the debug panel stopped servicing input after %s — it appears to have handed off the terminal without a confirm", latency)
	}

	a.Esc()
	a.WaitFor("Pods", Settle)
}

// TestPodDebugPanelStagesAttachOnAShelllessPod covers §41a's fork into
// §41b: a pod that is running perfectly well but has no shell to exec into.
//
// This is the one debug path that cannot be reached without a container
// runtime. kute decides it by running a real `kubectl exec` probe per
// container and reading the runtime's own "executable file not found"
// answer — a distinction no fake makes, since a fake has no failure mode
// that is a *successful* answer of "no shell here".
func TestPodDebugPanelStagesAttachOnAShelllessPod(t *testing.T) {
	RequireCluster(t)
	client := mutationClient(t)
	image := shelllessImage(t, client)
	const name = "phase3-debug-shellless"
	createPodWithImage(t, client, name, image)
	waitForAPI(t, "the shell-less pod to run", func(ctx context.Context) (bool, error) {
		pod, err := client.CoreV1().Pods(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return pod.Status.Phase == corev1.PodRunning, nil
	})

	a := Launch(t)
	a.WaitFor(name, Connect)
	a.filterTo(t, name)
	a.selectRow(t, name)
	a.Press("x")

	// Attach mode, not copy: the pod is running, so nothing is disabled and
	// the header says what an ephemeral container actually is.
	a.WaitForAll(Settle, "debug", name, "pod running · ephemeral container")
	a.WaitForAll(Settle, "attach ephemeral", "target", "profile")
	a.WaitForWrapped("cannot be removed, only outlived", Settle)
	a.WaitForWrapped("--target", Settle)

	// The launch gate is a live SelfSubjectAccessReview against the real API
	// server. The admin identity is allowed, so the panel must not be
	// sitting in its checking or denied state.
	a.WaitGone("checking access", Settle)
	a.Never("is denied", 2*time.Second)

	if latency := a.InputFence(); latency > 2*time.Second {
		t.Fatalf("the debug panel stopped servicing input after %s — it appears to have handed off the terminal without a confirm", latency)
	}

	a.Esc()
	a.WaitFor("Pods", Settle)
}

// TestPodDebugPanelDoesNotInventADenial pins the access gate's own rule:
// "no opinion is inconclusive, not a denial".
//
// kind authorizes with RBAC, which answers a SelfSubjectAccessReview for an
// ungranted verb with allowed=false and denied=false — it declines to allow
// rather than denying. That is precisely the shape the panel must not read
// as a refusal: doing so recreates the false-denial trap, where the app
// blocks a launch the API server would have accepted (another authorizer,
// another group). Only a real API server produces this answer, which is why
// the rule has no unit-level guard.
func TestPodDebugPanelDoesNotInventADenial(t *testing.T) {
	RequireCluster(t)
	const name = "phase3-debug-restricted"
	createPodWithImage(t, mutationClient(t), name, "kute-e2e.invalid/nothing-here:v1")

	// kute-partial, not kute-restricted: the panel can only be reached from
	// a Pods list that renders, and kute-restricted's grant is namespaced,
	// so a cluster-wide launch shows it §4b's 403 card instead of any rows.
	// kute-partial can list pods cluster-wide and still cannot create one,
	// which is exactly the pairing this test needs.
	a := Launch(t, WithKubeconfig(PartialKubeconfigPath()))
	a.WaitFor(name, Connect)
	a.filterTo(t, name)
	a.selectRow(t, name)
	a.Press("x")

	a.WaitForAll(Settle, "debug", name, "copy mode")
	// The review has to actually finish — a panel still checking says
	// nothing about how it reads the answer.
	a.WaitGone("checking access", Settle)
	a.Never("is denied", 3*time.Second)
	// The will-run line is still offered: the API server remains the
	// backstop, and the panel's job is to document the command, not to
	// pre-refuse it.
	a.WaitForWrapped("will run", Settle)

	a.Esc()
	a.WaitFor("Pods", Settle)
}

// TestPodDetailShowsEphemeralContainers covers §41e: the EPHEMERAL group,
// which appears only once a debug container has actually been attached.
//
// The attach is made through the pods/ephemeralcontainers subresource
// directly rather than through kute, for the same reason the PTY suite owns
// the handoff tests: committing the panel suspends the program. What is
// being checked here is the half that survives the debug session — a pod
// carrying an ephemeral container renders one, whoever attached it
// (`kubectl debug` run outside kute included).
func TestPodDetailShowsEphemeralContainers(t *testing.T) {
	RequireCluster(t)
	client := mutationClient(t)
	const name = "phase3-ephemeral-host"
	const debugContainer = "kute-e2e-debugger"
	createPodWithImage(t, client, name, busyboxImage, "/bin/sh", "-c", "sleep 900")
	waitForAPI(t, "the ephemeral host pod to run", func(ctx context.Context) (bool, error) {
		pod, err := client.CoreV1().Pods(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return pod.Status.Phase == corev1.PodRunning, nil
	})
	attachEphemeralContainer(t, client, name, debugContainer)

	a := Launch(t)
	a.WaitFor(name, Connect)
	a.filterTo(t, name)
	a.selectRow(t, name)
	a.Enter()
	a.WaitLoaded(Settle)

	// The pod's own containers are still the CONTAINERS grid; the debug
	// container is a group of its own, because it is not part of the pod's
	// spec.containers and never was.
	a.WaitForAll(Settle, "CONTAINERS", "EPHEMERAL", debugContainer)

	a.Esc()
	a.WaitFor("Pods", Settle)
}

// busyboxImage is the same digest-pinned image every workload fixture uses,
// so a disposable pod never waits on a pull the fixtures have not already
// done.
const busyboxImage = "busybox:1.37@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028"

// createPodWithImage creates a single-container pod on a caller-chosen
// image and force-deletes it in cleanup.
//
// Distinct from network_test.go's createDisposablePod, which pins busybox:
// the image is the whole variable here — one that never pulls, one with no
// shell in it, one that runs — and each selects a different branch of 'x'.
func createPodWithImage(t *testing.T, client kubernetes.Interface, name, image string, command ...string) *corev1.Pod {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pod, err := client.CoreV1().Pods(Namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace, Labels: map[string]string{"app": name}},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: ptr(int64(1)),
			Containers: []corev1.Container{{
				Name:  "pod",
				Image: image,
				// Optional, and deliberately so: a shell-less image cannot
				// run a shell command, and overriding its entrypoint with
				// one leaves the container Waiting forever — which routes
				// 'x' to copy mode and silently tests the wrong branch.
				Command: command,
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating disposable pod %s: %v", name, err)
	}
	cleanupObject(t, schema.GroupResource{Resource: "pods"}, name, func(ctx context.Context) error {
		return client.CoreV1().Pods(Namespace).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
	})
	return pod
}

// attachEphemeralContainer adds one ephemeral container through the
// subresource `kubectl debug` itself writes through.
func attachEphemeralContainer(t *testing.T, client kubernetes.Interface, podName, containerName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pod, err := client.CoreV1().Pods(Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading %s before attaching an ephemeral container: %v", podName, err)
	}
	pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:    containerName,
			Image:   busyboxImage,
			Command: []string{"/bin/sh", "-c", "sleep 900"},
		},
		TargetContainerName: "pod",
	})
	if _, err := client.CoreV1().Pods(Namespace).UpdateEphemeralContainers(ctx, podName, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("attaching an ephemeral container to %s: %v", podName, err)
	}
}

// shelllessImage returns an image the cluster's nodes already hold that has
// no shell in it — the pause image every node runs for its own sandboxes.
//
// Read off a Node's status.images rather than hard-coded: the pause tag
// tracks the Kubernetes version, and a hard-coded one would turn a version
// bump into a pull failure that reads as a kute bug.
func shelllessImage(t *testing.T, client kubernetes.Interface) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing nodes for a preloaded shell-less image: %v", err)
	}
	for _, node := range nodes.Items {
		for _, img := range node.Status.Images {
			for _, ref := range img.Names {
				if strings.Contains(ref, "pause:") && !strings.Contains(ref, "@") {
					return ref
				}
			}
		}
	}
	t.Skip("no preloaded pause image on any node — nothing shell-less to probe without a pull")
	return ""
}
