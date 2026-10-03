//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestCordonAndUncordonDedicatedWorker(t *testing.T) {
	RequireCluster(t)
	client := mutationClient(t)
	name := NodeNamePrefix(t) + "-worker"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	before, err := client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	cancel()
	if err != nil {
		t.Fatalf("getting dedicated worker %s: %v", name, err)
	}
	if before.Spec.Unschedulable {
		t.Skipf("dedicated worker %s was already cordoned", name)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := client.CoreV1().Nodes().Patch(ctx, name, types.StrategicMergePatchType, []byte(`{"spec":{"unschedulable":false}}`), metav1.PatchOptions{})
		if err != nil {
			t.Errorf("restoring worker %s schedulability: %v", name, err)
		}
	})

	a := Launch(t)
	a.WaitFor("api-", Connect)
	a.gotoKind(t, "nodes", "Nodes")
	a.WaitFor(name, Settle)
	a.selectRow(t, name)
	a.Press("C")
	a.waitForRowText(t, name, "cordoned")
	var cordonedRV string
	waitForAPI(t, "worker cordon", func(ctx context.Context) (bool, error) {
		node, err := client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		cordonedRV = node.ResourceVersion
		return node.Spec.Unschedulable, nil
	})
	requireNewResourceVersion(t, before.ResourceVersion, cordonedRV)

	// Fence the second toggle on the refreshed row: its direction is derived
	// from the watch-fed Cordoned bit, not the API read above.
	a.Press("C")
	a.waitForRowText(t, name, "Ready")
	var uncordonedRV string
	waitForAPI(t, "worker uncordon", func(ctx context.Context) (bool, error) {
		node, err := client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		uncordonedRV = node.ResourceVersion
		return !node.Spec.Unschedulable, nil
	})
	requireNewResourceVersion(t, cordonedRV, uncordonedRV)
}

func TestWorkloadEditorsMutateAndRemain(t *testing.T) {
	RequireCluster(t)
	const name = "phase3-workload"
	created := createDeployment(t, name, 0, "")
	client := mutationClient(t)

	a := Launch(t)
	a.WaitFor("api-", Connect)
	a.gotoKind(t, "deployments", "Deployments")
	a.WaitFor(name, Settle)
	a.filterTo(t, name)

	// Scale navigates back to the list after its one-line prompt.
	a.Press("+")
	a.WaitFor("SCALE", Settle)
	a.Enter()
	var scaleRV string
	waitForAPI(t, "deployment scale", func(ctx context.Context) (bool, error) {
		dep, err := client.AppsV1().Deployments(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		scaleRV = dep.ResourceVersion
		return dep.Spec.Replicas != nil && *dep.Spec.Replicas == 1, nil
	})
	requireNewResourceVersion(t, created.ResourceVersion, scaleRV)
	a.waitForRowText(t, name, "/1")

	// Set image refreshes and remains in its inspector panel.
	a.Press("i")
	a.WaitFor("SET IMAGE", Settle)
	replaceTypedValue(a, "1.37", "1.36")
	a.Enter()
	a.WaitFor("set image: worker=busybox:1.36", Settle)
	var imageRV string
	waitForAPI(t, "set image", func(ctx context.Context) (bool, error) {
		dep, err := client.AppsV1().Deployments(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		imageRV = dep.ResourceVersion
		return dep.Spec.Template.Spec.Containers[0].Image == "busybox:1.36", nil
	})
	requireNewResourceVersion(t, scaleRV, imageRV)
	a.WaitForAll(Settle, "SET IMAGE", "busybox", "1.36")
	a.Esc()
	a.WaitFor("Deployments", Settle)

	// Resources now follows the same refresh-and-remain contract.
	a.Press("r")
	a.WaitFor("RESOURCES", Settle)
	replaceTypedValue(a, "10m", "20m")
	a.Enter()
	a.WaitFor("applied cpu.request=20m", Settle)
	var resourcesRV string
	waitForAPI(t, "set resources", func(ctx context.Context) (bool, error) {
		dep, err := client.AppsV1().Deployments(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		resourcesRV = dep.ResourceVersion
		return dep.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().String() == "20m", nil
	})
	requireNewResourceVersion(t, imageRV, resourcesRV)
	a.WaitForAll(Settle, "RESOURCES", "container   worker", "applied cpu.request=20m")
	a.Esc()
	a.WaitFor("Deployments", Settle)

	// Metadata uses its context-preserving grid and refreshes from the API.
	a.Press("m")
	a.WaitFor("META", Settle)
	a.Press("a")
	a.Type("phase3-e2e")
	a.Press("tab")
	a.Type("yes")
	a.Enter()
	a.WaitFor("added phase3-e2e=yes", Settle)
	var metaRV string
	waitForAPI(t, "metadata label edit", func(ctx context.Context) (bool, error) {
		dep, err := client.AppsV1().Deployments(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		metaRV = dep.ResourceVersion
		return dep.Labels["phase3-e2e"] == "yes", nil
	})
	requireNewResourceVersion(t, resourcesRV, metaRV)
	a.WaitForAll(Settle, "META", "phase3-e2e", "yes")
}

func TestSetImageUpdatesInitContainers(t *testing.T) {
	RequireCluster(t)
	const name = "set-image-init"
	client := mutationClient(t)
	dep := createDeployment(t, name, 0, "")
	restartAlways := corev1.ContainerRestartPolicyAlways
	dep.Spec.Template.Spec.InitContainers = []corev1.Container{
		{Name: "prepare", Image: "busybox:1.35", Command: []string{"true"}},
		{Name: "mesh", Image: "busybox:1.35", Command: []string{"sleep", "infinity"}, RestartPolicy: &restartAlways},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	created, err := client.AppsV1().Deployments(Namespace).Update(ctx, dep, metav1.UpdateOptions{})
	cancel()
	if err != nil {
		t.Fatalf("adding init containers to deployment %s: %v", name, err)
	}

	a := Launch(t)
	a.WaitFor("api-", Connect)
	a.gotoKind(t, "deployments", "Deployments")
	a.WaitFor(name, Settle)
	a.filterTo(t, name)
	a.Press("i")
	a.WaitForAll(Settle, "SET IMAGE", "prepare init", "mesh sidecar")

	// The regular worker is first; tab once reaches the conventional init
	// container and the apply must patch spec.initContainers.
	a.Press("tab")
	replaceTypedValue(a, "1.35", "1.36")
	a.Enter()
	a.WaitFor("set image: prepare=busybox:1.36", Settle)
	var prepareRV string
	waitForAPI(t, "conventional init-container image", func(ctx context.Context) (bool, error) {
		current, err := client.AppsV1().Deployments(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		prepareRV = current.ResourceVersion
		return current.Spec.Template.Spec.InitContainers[0].Image == "busybox:1.36" &&
			current.Spec.Template.Spec.Containers[0].Image == "busybox:1.37", nil
	})
	requireNewResourceVersion(t, created.ResourceVersion, prepareRV)

	// The next tab is a native sidecar. It is presented as a sidecar but is
	// stored and patched in the same initContainers API list.
	a.Press("tab")
	replaceTypedValue(a, "1.35", "1.36")
	a.Enter()
	a.WaitFor("set image: mesh=busybox:1.36", Settle)
	var meshRV string
	waitForAPI(t, "native-sidecar image", func(ctx context.Context) (bool, error) {
		current, err := client.AppsV1().Deployments(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		meshRV = current.ResourceVersion
		return current.Spec.Template.Spec.InitContainers[0].Image == "busybox:1.36" &&
			current.Spec.Template.Spec.InitContainers[1].Image == "busybox:1.36" &&
			current.Spec.Template.Spec.Containers[0].Image == "busybox:1.37", nil
	})
	requireNewResourceVersion(t, prepareRV, meshRV)
}
