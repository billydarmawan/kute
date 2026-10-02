package tui

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
)

type corpusPodLister struct{ pods []runtime.Object }

func (l corpusPodLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	if kind == kube.KindPod {
		return l.pods, nil
	}
	return nil, nil
}

func corpusTestSession(n int) *Session {
	pods := make([]runtime.Object, 0, n)
	for i := range n {
		pods = append(pods, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("app-%05d", i), Namespace: "default"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Ready: true}}},
		})
	}
	return &Session{
		Registry: resources.DefaultRegistry(),
		Groups:   resources.DefaultGroups(),
		Location: Location{Namespace: "default", Kind: kube.KindPod},
		Lister:   corpusPodLister{pods: pods},
	}
}

// TestGotoCapsResourceResultsNotTheCorpus: the whole corpus is searched, the
// cap bounds only what's shown, and only the shown rows get a status glyph.
func TestGotoCapsResourceResultsNotTheCorpus(t *testing.T) {
	sess := corpusTestSession(200)
	corpus := buildGotoResourceCorpus(sess)
	if len(corpus.items) != 200 {
		t.Fatalf("corpus holds %d objects; want all 200", len(corpus.items))
	}

	resourcesShown := 0
	for _, it := range gotoFuzzyItems(sess, "app", corpus) {
		if target, ok := it.Data.(gotoTarget); ok && target.action == gotoOpenResource {
			resourcesShown++
			if it.Right == "" {
				t.Fatalf("result %q has no status glyph", it.Label)
			}
		}
	}
	if resourcesShown != maxGotoResults {
		t.Fatalf("showed %d resource results; want the cap, %d", resourcesShown, maxGotoResults)
	}

	items := gotoFuzzyItems(sess, "app-00199", corpus)
	if len(items) == 0 || items[0].Label != "app-00199" {
		t.Fatalf("expected the last object in cache order to rank first for its exact name, got %v", items)
	}
}

// BenchmarkGotoFuzzyKeystroke is one keystroke's work against a 5,000-pod
// namespace once the palette's corpus snapshot exists — what typing costs on
// the Update loop. "broad" matches every object (the first keystroke or
// two), "narrow" a handful; both search all 5,000 and show at most 50.
func BenchmarkGotoFuzzyKeystroke(b *testing.B) {
	sess := corpusTestSession(5000)
	corpus := buildGotoResourceCorpus(sess)
	for _, q := range []struct{ name, query string }{{"broad", "ap"}, {"narrow", "app-0421"}} {
		b.Run(q.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = gotoFuzzyItems(sess, q.query, corpus)
			}
		})
	}
}

// BenchmarkGotoCorpusBuild is the once-per-palette (and once per watch event
// that lands while it's open, on the next keystroke) snapshot of 5,000 pods.
func BenchmarkGotoCorpusBuild(b *testing.B) {
	sess := corpusTestSession(5000)
	b.ReportAllocs()
	for b.Loop() {
		_ = buildGotoResourceCorpus(sess)
	}
}
