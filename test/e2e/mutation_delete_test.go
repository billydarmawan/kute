//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestNonProdDeleteCommitsAndClampsSelection(t *testing.T) {
	RequireCluster(t)
	const deleted, neighbor = "phase3-delete-a", "phase3-delete-b"
	before := createConfigMap(t, deleted, map[string]string{"value": "before"})
	createConfigMap(t, neighbor, map[string]string{"value": "keep"})
	client := mutationClient(t)

	a := Launch(t)
	a.WaitFor("api-", Connect)
	a.gotoKind(t, "configmaps", "ConfigMaps")
	a.WaitForAll(Settle, deleted, neighbor)
	a.selectRow(t, deleted)
	a.Press("D")
	a.WaitFor("CONFIRM", Settle)
	a.Press("y")

	waitForDeleted(t, "non-prod configmap", func(ctx context.Context) error {
		_, err := client.CoreV1().ConfigMaps(Namespace).Get(ctx, deleted, metav1.GetOptions{})
		return err
	})
	// The keybar's outcome line names the deleted object until the next key,
	// so the row's absence is read from the table itself, not the frame.
	a.WaitFor("✓ Delete ConfigMap "+deleted, Settle)
	a.waitForRowGone(t, deleted)
	a.waitForSelectedRow(t, neighbor)
	if before.ResourceVersion == "" {
		t.Fatal("created configmap had no resourceVersion")
	}
	a.WaitFor("ConfigMaps", Settle)
}

func TestProdDeleteCommitsAfterTypedName(t *testing.T) {
	RequireCluster(t)
	const name = "phase3-prod-delete"
	createConfigMap(t, name, map[string]string{"value": "before"})
	client := mutationClient(t)

	a := Launch(t, WithProdContexts(ContextName(t)))
	a.WaitFor("api-", Connect)
	a.gotoKind(t, "configmaps", "ConfigMaps")
	a.WaitFor(name, Settle)
	a.selectRow(t, name)
	a.Press("D")
	a.WaitForAll(Settle, "PROD CONTEXT", TypeToConfirm, name)
	a.Type(name)
	a.Enter()

	waitForDeleted(t, "PROD configmap", func(ctx context.Context) error {
		_, err := client.CoreV1().ConfigMaps(Namespace).Get(ctx, name, metav1.GetOptions{})
		return err
	})
	a.WaitGone(TypeToConfirm, Settle)
	a.WaitFor("✓ Delete ConfigMap "+name, Settle)
	a.waitForRowGone(t, name)
	a.WaitFor("ConfigMaps", Settle)
}

func TestForceDeleteEscalationUsesDisposablePod(t *testing.T) {
	RequireCluster(t)
	const name = "phase3-force-delete"
	client := mutationClient(t)
	grace := int64(60)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	pod, err := client.CoreV1().Pods(Namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace, Labels: map[string]string{"app": name}},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: &grace,
			Containers:                    []corev1.Container{{Name: "pod", Image: "busybox:1.37@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028", Command: []string{"/bin/sh", "-c", "trap '' TERM; sleep 600"}}},
		},
	}, metav1.CreateOptions{})
	cancel()
	if err != nil {
		t.Fatalf("creating disposable pod: %v", err)
	}
	cleanupObject(t, schema.GroupResource{Resource: "pods"}, name, func(ctx context.Context) error {
		zero := int64(0)
		return client.CoreV1().Pods(Namespace).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: &zero})
	})

	a := Launch(t)
	a.WaitFor(name, Connect)
	a.filterTo(t, name)
	a.Press("D")
	a.WaitFor("CONFIRM", Settle)
	// Force delete is always the type-the-name modal, PROD or not.
	a.Press("ctrl+k")
	a.WaitForAll(Settle, "Force delete "+name+"?", TypeToConfirm)
	a.Type(name)
	a.Enter()

	waitForDeleted(t, "force-deleted pod", func(ctx context.Context) error {
		_, err := client.CoreV1().Pods(Namespace).Get(ctx, name, metav1.GetOptions{})
		return err
	})
	a.WaitGone("Force delete "+name+"?", Settle)
	a.WaitFor("Pods", Settle)
	if pod.ResourceVersion == "" {
		t.Fatal("created pod had no resourceVersion")
	}
}
