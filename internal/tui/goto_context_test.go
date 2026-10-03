package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/state"
	"github.com/kute-dev/kute/internal/testutil/testenv"
)

// §2b: typing in the jump palette searches kinds, resources, namespaces
// *and contexts*. A context result switches like 7a's palette; the current
// context isn't offered. Not parallel-safe: t.Setenv.
func TestGotoFuzzyCorpusIncludesContexts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(reprobeTestKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	testenv.SetHome(t, t.TempDir())

	sess := &Session{
		Theme:    Dark(),
		Registry: resources.DefaultRegistry(),
		Cluster:  &kube.Cluster{},
		Location: Location{Context: "dev", Kind: kube.KindPod},
		State:    state.State{PerContext: map[string]state.PerContext{}},
	}
	corpus := buildGotoResourceCorpus(sess)

	var found bool
	for _, it := range gotoFuzzyItems(sess, "prod-eks", corpus) {
		if it.Label == "dev" && gotoSwitchesContext(it) {
			t.Fatalf("the current context must not be offered as a jump")
		}
		if it.Label == "prod-eks" && gotoSwitchesContext(it) {
			found = true
			if gotoDispatch(sess, it) == nil {
				t.Fatal("expected a context result to dispatch a context switch")
			}
		}
	}
	if !found {
		t.Fatal("expected the prod-eks context in the fuzzy results")
	}
	for _, it := range gotoFuzzyItems(sess, "dev", corpus) {
		if it.Label == "dev" && gotoSwitchesContext(it) {
			t.Fatalf("the current context must not be offered as a jump")
		}
	}
}
