package browse

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// The panel's own behavior (grids, joins, tiers, failure-restore) is tested
// in internal/tui/metapanel, where the editor now lives — what stays here is
// browse's hosting glue: 'm' opens it on the selected row, keys route into
// it, results refresh it, esc closes it.

// metaDeployment builds a Deployment carrying labels/annotations for 26a's
// editor to open on.
func metaDeployment(ns, name string, labels, annotations, selector map[string]string) *appsv1.Deployment {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, Annotations: annotations},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Image: "nva-worker:1.0"}}},
		}},
	}
	if selector != nil {
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
	}
	return dep
}

func newMetaModel(t *testing.T, mut *fakeMutator, objs map[kube.ResourceKind][]runtime.Object) Model {
	t.Helper()
	mut.metaObjs = objs
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	session.Location.Namespace = "default"
	m := New(Config{Session: session, Lister: fakeLister{objs: objs}, Mutator: mut})
	m.SetSize(120, 36)
	return step(t, m, m.Init()())
}

// TestMetaKeyOpensPanelAndEscCloses drives browse's own routing end to end:
// 'm' opens the shared panel on the selected row, the body renders its
// grids, an edit commits through browse's ResultMsg dispatch and refreshes
// in place, and esc closes back to the table.
func TestMetaKeyOpensPanelAndEscCloses(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"env": "stage"}, nil, nil)
	mut := &fakeMutator{}
	m := newMetaModel(t, mut, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	m = step(t, m, tea.KeyPressMsg{Text: "m"})
	if m.pendingMeta == nil {
		t.Fatal("'m' should open the labels/annotations panel")
	}
	body := m.Body(120, 36)
	if !strings.Contains(body, "LABELS · 1") || !strings.Contains(body, "env=") {
		t.Fatalf("panel body missing the labels grid:\n%s", body)
	}

	// An edit commits through browse's ResultMsg dispatch (TierNone applies
	// immediately) and the panel refreshes in place rather than closing.
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = step(t, m, tea.KeyPressMsg{Text: "g"})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(mut.metaPatches) != 1 || mut.metaPatches[0] != "default/nva-worker labels env=stageg" {
		t.Fatalf("metaPatches = %v, want one env=stageg label patch", mut.metaPatches)
	}
	if m.pendingMeta == nil {
		t.Fatal("the panel should stay open after a successful apply")
	}
	if body := m.Body(120, 36); !strings.Contains(body, "updated env=stageg") {
		t.Fatalf("panel body missing the inline success message:\n%s", body)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "esc"})
	if m.pendingMeta != nil {
		t.Error("esc should close the panel")
	}
}

// The PATCH result usually beats the watch event for it, so the panel's own
// refetch reads the pre-commit value; browse must hand the event to the panel
// so it shows what actually landed.
func TestMetaPanelRefreshesOnItsKindsChangeEvent(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"env": "stage"}, nil, nil)
	mut := &fakeMutator{}
	m := newMetaModel(t, mut, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})
	mut.metaObjs = nil // the patch lands server-side, not yet in the cache

	m = step(t, m, tea.KeyPressMsg{Text: "m"})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = step(t, m, tea.KeyPressMsg{Text: "g"})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if n := strings.Count(m.Body(120, 36), "stageg"); n != 1 {
		t.Fatalf("precondition: only the result line should name stageg before the watch event, got %d", n)
	}

	dep.Labels["env"] = "stageg"
	m = step(t, m, kube.ResourceChangedMsg{Kind: kube.KindDeployment})
	if n := strings.Count(m.Body(120, 36), "stageg"); n != 2 {
		t.Errorf("after the change event the row should read stageg too (want 2 occurrences, got %d):\n%s", n, m.Body(120, 36))
	}
}
