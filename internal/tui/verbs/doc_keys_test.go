package verbs

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestDocsNameTheRegisteredMutatingKeys pins CLAUDE.md and the design spec
// to the registry for the core mutating verbs: every "`<key>` <verb>"
// mention must name the key verbs.go binds, so the docs can't drift back to
// a chord the code no longer handles (review 10 L8: delete, force-delete and
// rollout-restart each had three different keys across code, spec and
// CLAUDE.md).
func TestDocsNameTheRegisteredMutatingKeys(t *testing.T) {
	t.Parallel()
	byWord := map[string]Verb{
		"delete":          Delete,
		"drain":           Drain,
		"rollout-restart": RolloutRestart,
		"rollout restart": RolloutRestart,
		"cordon":          Cordon,
	}
	mention := regexp.MustCompile("`([^` ]+)` (delete|drain|rollout[- ]restart|cordon)\\b")
	force := regexp.MustCompile("`([^` ]+)` \\(`verbs\\.ForceDelete\\.Key`\\)|force delete \\(`([^` ]+)`")
	for _, path := range []string{"../../../CLAUDE.md", "../../../docs/design/README.md"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		doc := string(raw)
		seen := 0
		for _, m := range mention.FindAllStringSubmatch(doc, -1) {
			seen++
			if want := docKey(byWord[m[2]].Key); m[1] != want {
				t.Errorf("%s: %q names key %q, registry binds %q", path, m[0], m[1], want)
			}
		}
		if seen == 0 {
			t.Errorf("%s: no verb key mentions found — has the phrasing changed?", path)
		}
		for _, m := range force.FindAllStringSubmatch(doc, -1) {
			if got, want := m[1]+m[2], docKey(ForceDelete.Key); got != want {
				t.Errorf("%s: %q names force-delete key %q, registry binds %q", path, m[0], got, want)
			}
		}
	}
}

// docKey is how the docs spell a registry key: "ctrl+d" is written ctrl-d.
func docKey(key string) string { return strings.ReplaceAll(key, "+", "-") }
