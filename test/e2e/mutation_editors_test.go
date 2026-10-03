//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConfigMapMutationBreadthAndConsumerRestart(t *testing.T) {
	RequireCluster(t)
	const cmName, deploymentName = "phase3-config", "phase3-config-consumer"
	created := createConfigMap(t, cmName, map[string]string{
		"mode":   "before",
		"script": "first line\nsecond line\n",
	})
	deployment := createDeployment(t, deploymentName, 0, cmName)
	client := mutationClient(t)

	a := Launch(t)
	a.WaitFor("api-", Connect)
	a.openFrom(t, "configmaps", "ConfigMaps", cmName)
	a.WaitForAll(Settle, "cm/"+cmName, deploymentName, "script")

	// Multiline edit: paste preserves the newline and alt+a applies without
	// closing the pushed Data screen.
	a.selectRow(t, "script")
	a.Enter()
	a.WaitFor("alt+a apply", Settle)
	a.Paste("phase3 marker\n")
	a.Press("alt+a")
	a.WaitFor("updated script", Settle)
	var multilineRV string
	waitForAPI(t, "multiline configmap edit", func(ctx context.Context) (bool, error) {
		cm, err := client.CoreV1().ConfigMaps(Namespace).Get(ctx, cmName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		multilineRV = cm.ResourceVersion
		return strings.Contains(cm.Data["script"], "phase3 marker\n"), nil
	})
	requireNewResourceVersion(t, created.ResourceVersion, multilineRV)
	a.WaitFor("cm/"+cmName, Settle)

	// Add and remove a key, requiring both API versions and the refreshed
	// key count/result line while remaining on the same screen.
	a.Press("a")
	a.Type("phase3-added")
	a.Press("tab")
	a.Type("temporary")
	a.Enter()
	a.WaitFor("added phase3-added", Settle)
	var addRV string
	waitForAPI(t, "configmap key add", func(ctx context.Context) (bool, error) {
		cm, err := client.CoreV1().ConfigMaps(Namespace).Get(ctx, cmName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		addRV = cm.ResourceVersion
		return cm.Data["phase3-added"] == "temporary", nil
	})
	requireNewResourceVersion(t, multilineRV, addRV)
	a.Press("D")
	a.WaitFor("CONFIRM", Settle)
	a.Press("y")
	a.WaitFor("removed phase3-added", Settle)
	var removeRV string
	waitForAPI(t, "configmap key removal", func(ctx context.Context) (bool, error) {
		cm, err := client.CoreV1().ConfigMaps(Namespace).Get(ctx, cmName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		removeRV = cm.ResourceVersion
		_, exists := cm.Data["phase3-added"]
		return !exists, nil
	})
	requireNewResourceVersion(t, addRV, removeRV)

	// Reopen to reset selection, then apply a value and restart the live
	// consumer in one action.
	a.Esc()
	a.WaitFor("ConfigMaps", Settle)
	a.Enter()
	a.WaitFor("cm/"+cmName, Settle)
	a.selectRow(t, "mode")
	a.Enter()
	replaceTypedValue(a, "before", "after-restart")
	a.Press("ctrl+r")
	a.WaitFor("CONFIRM", Settle)
	a.Press("y")
	a.WaitFor("updated mode · restarted 1 consumer", Settle)
	var finalRV, restartedAt string
	waitForAPI(t, "configmap apply plus restart", func(ctx context.Context) (bool, error) {
		cm, err := client.CoreV1().ConfigMaps(Namespace).Get(ctx, cmName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		dep, err := client.AppsV1().Deployments(Namespace).Get(ctx, deploymentName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		finalRV = cm.ResourceVersion
		restartedAt = dep.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"]
		return cm.Data["mode"] == "after-restart" && restartedAt != "", nil
	})
	requireNewResourceVersion(t, removeRV, finalRV)
	requireNewResourceVersion(t, deployment.ResourceVersion, mustDeploymentRV(t, deploymentName))
	a.WaitForAll(Settle, "cm/"+cmName, "after-restart")
}

func TestSecretRewriteRecoversFromConflict(t *testing.T) {
	RequireCluster(t)
	const name, key = "phase3-secret", "token"
	const before, after = "before-secret", "after-secret"
	created := createSecret(t, name, map[string][]byte{key: []byte(before)})
	client := mutationClient(t)

	a := Launch(t)
	a.WaitFor("api-", Connect)
	a.openFrom(t, "secrets", "Secrets", name)
	a.WaitForAll(Settle, "secret/"+name, key)
	a.selectRow(t, key)
	a.Enter()
	a.WaitFor(before, Settle)
	replaceTypedValue(a, before, after)

	// A deterministic wire-level 409 exercises the editor's failed
	// optimistic-concurrency shape: the attempted buffer and screen survive,
	// the Secret remains unchanged, and Enter can retry the same value.
	a.Proxy().FailNext(RequestMatcher{Method: http.MethodPatch, Resource: "secrets", Verb: "PATCH"}, http.StatusConflict, 1)
	fence := a.Proxy().Fence()
	a.Enter()
	rec := a.Proxy().WaitForRequest(fence, RequestMatcher{Resource: "secrets", Verb: "PATCH"}, Settle)
	if got := a.Proxy().WaitForCompletion(rec.ID, Settle).StatusCode; got != http.StatusConflict {
		t.Fatalf("secret conflict response = %d, want 409", got)
	}
	a.WaitForWrapped("changed on the server since you opened it", Settle)
	a.WaitFor(after, Settle)
	if got := secretValue(t, name, key); got != before {
		t.Fatalf("secret changed despite conflict: %q", got)
	}

	a.Enter()
	a.WaitFor("updated "+key, Settle)
	var finalRV string
	waitForAPI(t, "secret conflict retry", func(ctx context.Context) (bool, error) {
		secret, err := client.CoreV1().Secrets(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		finalRV = secret.ResourceVersion
		return string(secret.Data[key]) == after, nil
	})
	requireNewResourceVersion(t, created.ResourceVersion, finalRV)
	a.WaitFor("secret/"+name, Settle)
	if frame := a.Frame(); strings.Contains(frame, after) {
		t.Fatalf("committed secret value leaked into the resting frame:\n%s", frame)
	}
}

func mustDeploymentRV(t *testing.T, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), Settle)
	defer cancel()
	dep, err := mutationClient(t).AppsV1().Deployments(Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting deployment %s: %v", name, err)
	}
	return dep.ResourceVersion
}
