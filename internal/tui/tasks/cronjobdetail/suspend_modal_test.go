package cronjobdetail

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/kute-dev/kute/internal/config"
)

// TestProdSuspendModalFitsAndNamesWhatStops: the PROD type-the-name suspend
// modal states both the will-run command and §36c's "running jobs
// unaffected / what stops" note — on separate lines, since side by side
// they overran a 120-column terminal and cut off the modal's right border.
func TestProdSuspendModalFitsAndNamesWhatStops(t *testing.T) {
	t.Parallel()
	c, _ := newFakeCronJob("default", "nightly")
	m := newModel(t, c, "default", "nightly")
	m.session.Config = config.Config{ProdContexts: []string{"test-cluster"}}
	m = step(t, m, tea.KeyPressMsg{Text: "s"})

	out := ansi.Strip(m.View().Content)
	if !strings.Contains(out, "will run: kubectl patch cronjob/nightly") || !strings.Contains(out, "no active jobs right now") {
		t.Fatalf("modal is missing the will-run line or the danger note:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, "…") {
			t.Fatalf("a modal line was truncated at 120 columns:\n%s", out)
		}
	}
}
