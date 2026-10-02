//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

func mutationClient(t *testing.T) kubernetes.Interface {
	t.Helper()
	RequireCluster(t)
	return e2eClientset(t)
}

func cleanupObject(t *testing.T, resource schema.GroupResource, name string, delete func(context.Context) error) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := delete(ctx); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("cleaning up %s %s: %v", resource.Resource, name, err)
		}
	})
}

func waitForAPI(t *testing.T, description string, check func(context.Context) (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(Settle)
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ok, err := check(ctx)
		cancel()
		if ok {
			return
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %v", description, lastErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitForDeleted(t *testing.T, description string, get func(context.Context) error) {
	t.Helper()
	waitForAPI(t, description+" to be deleted", func(ctx context.Context) (bool, error) {
		err := get(ctx)
		return apierrors.IsNotFound(err), err
	})
}

func requireNewResourceVersion(t *testing.T, before, after string) {
	t.Helper()
	if before == "" || after == "" || before == after {
		t.Fatalf("resourceVersion did not advance: before=%q after=%q", before, after)
	}
}

// seedDynamicObject captures a shared CRD fixture, writes a distinguishable
// pre-action state, and restores only the caller-owned fields in cleanup.
// Field-scoped restoration avoids overwriting unrelated server changes while
// still making reruns against a reused cluster honest.
func seedDynamicObject(
	t *testing.T,
	resource dynamic.ResourceInterface,
	name string,
	seed func(*unstructured.Unstructured),
	restore func(current, original *unstructured.Unstructured),
) *unstructured.Unstructured {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	original, err := resource.Get(ctx, name, metav1.GetOptions{})
	cancel()
	if err != nil {
		t.Fatalf("capturing %s before mutation: %v", name, err)
	}

	seeded := original.DeepCopy()
	seed(seeded)
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	seeded, err = resource.Update(ctx, seeded, metav1.UpdateOptions{})
	cancel()
	if err != nil {
		t.Fatalf("seeding %s before mutation: %v", name, err)
	}

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		current, getErr := resource.Get(cleanupCtx, name, metav1.GetOptions{})
		if getErr != nil {
			t.Errorf("getting %s for restore: %v", name, getErr)
			return
		}
		restore(current, original)
		if _, updateErr := resource.Update(cleanupCtx, current, metav1.UpdateOptions{}); updateErr != nil {
			t.Errorf("restoring %s: %v", name, updateErr)
		}
	})
	return seeded
}

func restoreAnnotation(current, original *unstructured.Unstructured, key string) {
	annotations := current.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if value, ok := original.GetAnnotations()[key]; ok {
		annotations[key] = value
	} else {
		delete(annotations, key)
	}
	current.SetAnnotations(annotations)
}

// restoreNestedField puts one field back the way seedDynamicObject found it.
//
// It reports through t rather than returning early on error, because the
// failure mode is invisible otherwise and outlives the run: a restore that
// gives up leaves the shared fixture carrying this test's seeded value, and
// under the documented KUTE_E2E_REUSE=1 the *next* run starts from that
// mutated state. A cleanup that cannot do its job has to say so.
func restoreNestedField(t *testing.T, current, original *unstructured.Unstructured, fields ...string) {
	t.Helper()
	path := strings.Join(fields, ".")
	value, found, err := unstructured.NestedFieldCopy(original.Object, fields...)
	if err != nil {
		t.Errorf("restoring %s on %s: reading the captured original: %v", path, current.GetName(), err)
		return
	}
	if !found {
		unstructured.RemoveNestedField(current.Object, fields...)
		return
	}
	if err := unstructured.SetNestedField(current.Object, value, fields...); err != nil {
		t.Errorf("restoring %s on %s: %v", path, current.GetName(), err)
	}
}

func replaceTypedValue(a *App, old, value string) {
	a.t.Helper()
	for range len(old) {
		a.Press("backspace")
	}
	a.Type(value)
}

func (a *App) waitForSelectedRow(t *testing.T, name string) {
	t.Helper()
	frame, ok := a.poll(func(frame string) bool {
		for _, line := range strings.Split(frame, "\n") {
			if selectedTableRow(line, name) {
				return true
			}
		}
		return false
	}, Settle)
	if !ok {
		t.Fatalf("row %q never became selected:\n%s", name, frame)
	}
}

func (a *App) waitForRowText(t *testing.T, name, text string) {
	t.Helper()
	frame, ok := a.poll(func(frame string) bool {
		for _, line := range strings.Split(frame, "\n") {
			if strings.Contains(line, name) && strings.Contains(line, text) {
				return true
			}
		}
		return false
	}, Settle)
	if !ok {
		t.Fatalf("row %q never contained %q:\n%s", name, text, frame)
	}
}

func createConfigMap(t *testing.T, name string, data map[string]string) *corev1.ConfigMap {
	t.Helper()
	client := mutationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cm, err := client.CoreV1().ConfigMaps(Namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace}, Data: data,
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating configmap %s: %v", name, err)
	}
	cleanupObject(t, schema.GroupResource{Resource: "configmaps"}, name, func(ctx context.Context) error {
		return client.CoreV1().ConfigMaps(Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	})
	return cm
}

func createSecret(t *testing.T, name string, data map[string][]byte) *corev1.Secret {
	t.Helper()
	client := mutationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	secret, err := client.CoreV1().Secrets(Namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace}, Type: corev1.SecretTypeOpaque, Data: data,
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating secret %s: %v", name, err)
	}
	cleanupObject(t, schema.GroupResource{Resource: "secrets"}, name, func(ctx context.Context) error {
		return client.CoreV1().Secrets(Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	})
	return secret
}

func createDeployment(t *testing.T, name string, replicas int32, configMap string) *appsv1.Deployment {
	t.Helper()
	client := mutationClient(t)
	labels := map[string]string{"app": name, "phase3": "before"}
	container := corev1.Container{
		Name: "worker", Image: "busybox:1.37",
		Command: []string{"/bin/sh", "-c", "while true; do sleep 30; done"},
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU: resourceQuantity("10m"), corev1.ResourceMemory: resourceQuantity("16Mi"),
		}},
	}
	if configMap != "" {
		container.Env = []corev1.EnvVar{{Name: "MODE", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: configMap}, Key: "mode",
		}}}}
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}}, Spec: corev1.PodSpec{Containers: []corev1.Container{container}}},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	created, err := client.AppsV1().Deployments(Namespace).Create(ctx, dep, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating deployment %s: %v", name, err)
	}
	cleanupObject(t, schema.GroupResource{Resource: "deployments"}, name, func(ctx context.Context) error {
		return client.AppsV1().Deployments(Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	})
	return created
}

func resourceQuantity(value string) resource.Quantity {
	return resource.MustParse(value)
}

func getJobByAnnotation(t *testing.T, key, value string) *batchv1.Job {
	t.Helper()
	client := mutationClient(t)
	var found *batchv1.Job
	waitForAPI(t, fmt.Sprintf("job with annotation %s=%s", key, value), func(ctx context.Context) (bool, error) {
		list, err := client.BatchV1().Jobs(Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		for i := range list.Items {
			if list.Items[i].Annotations[key] == value {
				found = list.Items[i].DeepCopy()
				return true, nil
			}
		}
		return false, nil
	})
	return found
}

func deleteJobNow(t *testing.T, name string) {
	t.Helper()
	zero := int64(0)
	client := mutationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := client.BatchV1().Jobs(Namespace).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: &zero, PropagationPolicy: ptr(metav1.DeletePropagationBackground)})
	cancel()
	if err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("deleting job %s: %v", name, err)
		return
	}
	waitForDeleted(t, "job "+name, func(ctx context.Context) error {
		_, err := client.BatchV1().Jobs(Namespace).Get(ctx, name, metav1.GetOptions{})
		return err
	})
}

func ptr[T any](v T) *T { return &v }

// waitForRowGone waits until name is no longer a row of the list on screen.
//
// A frame-wide WaitGone can't say this: a committed mutation names its object
// in the keybar's outcome line ("✓ Delete ConfigMap <name>"), which stays up
// until the next key press — so the name is on screen for as long as the test
// keeps looking, row or no row. The predicate also requires the table header,
// so a frame with no list on it at all (a modal still covering it, a loading
// state) can't pass as "the row is gone".
func (a *App) waitForRowGone(t *testing.T, name string) {
	t.Helper()
	frame, ok := a.poll(func(frame string) bool {
		rows, found := tableRows(frame)
		if !found {
			return false
		}
		for _, row := range rows {
			if lineHasExactField(row, name) {
				return false
			}
		}
		return true
	}, Settle)
	if !ok {
		t.Fatalf("row %q was still in the table after %s:\n%s", name, Settle, frame)
	}
}

// tableRows returns the body rows of the list on screen: the lines after the
// NAME header and its rule, up to the next rule, blank line or range footer
// ("1–4 of 4"). found reports whether a header was on screen at all.
func tableRows(frame string) (rows []string, found bool) {
	lines := strings.Split(frame, "\n")
	start := -1
	for i, line := range lines {
		if lineHasExactField(line, "NAME") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil, false
	}
	for start < len(lines) && isRuleLine(lines[start]) {
		start++
	}
	for _, line := range lines[start:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || isRuleLine(line) || rangeFooter.MatchString(trimmed) {
			break
		}
		rows = append(rows, line)
	}
	return rows, true
}

var rangeFooter = regexp.MustCompile(`^\d+–\d+ of \d+`)

func isRuleLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed != "" && strings.Trim(trimmed, "─") == ""
}
