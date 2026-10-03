package helmrepo

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fullDecode is readIndex without the reduceIndex fast path — the answer the
// fast path must reproduce exactly.
func fullDecode(t testing.TB, raw string) map[string]string {
	t.Helper()
	var f indexFile
	if err := yaml.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("full decode: %v", err)
	}
	out := map[string]string{}
	for name, versions := range f.Entries {
		if best := newestVersion(versions); best != "" {
			out[name] = best
		}
	}
	return out
}

func readIndexString(t testing.TB, raw string) map[string]string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x-index.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readIndex(path)
	if err != nil {
		t.Fatalf("readIndex: %v", err)
	}
	return got
}

// TestReduceIndexMatchesFullDecode: whatever layout the file is in, the
// fast path and the full decode agree — either because the reduced document
// carries the same versions, or because reduceIndex declined and readIndex
// fell back.
func TestReduceIndexMatchesFullDecode(t *testing.T) {
	cases := map[string]struct {
		body    string
		reduces bool
	}{
		// helm's own (sigs.k8s.io/yaml) layout: dashes at the key's indent.
		"helm layout": {`apiVersion: v1
entries:
  cert-manager:
  - annotations:
      artifacthub.io/version: 9.9.9
    apiVersion: v2
    dependencies:
    - name: common
      version: 99.0.0
    description: |
      version: 42.0.0 is mentioned in prose
    name: cert-manager
    version: v1.16.2
  - name: cert-manager
    version: "v1.15.0"
  "quoted-chart":
  - version: 1.0.0 # inline comment
    name: quoted-chart
  empty-chart: []
generated: "2026-07-01T00:00:00Z"
`, false}, // "empty-chart: []" isn't a bare key, so this one falls back
		"helm layout without flow": {`apiVersion: v1
entries:
  cert-manager:
  - dependencies:
    - name: common
      version: 99.0.0
    name: cert-manager
    version: v1.16.2
  - version: 'v1.17.0-rc.1'
  redis:
  - name: redis
    version: 18.1.5
  - name: redis
    version: 18.2.0
generated: "2026-07-01T00:00:00Z"
`, true},
		// yaml.v3's layout: dashes indented past the key.
		"indented sequences": {`entries:
  redis:
    - name: redis
      version: 18.1.5
    - version: 18.2.0
`, true},
		"json": {`{"apiVersion":"v1","entries":{"redis":[{"version":"18.1.5"}]}}`, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := reduceIndex([]byte(tc.body)); ok != tc.reduces {
				t.Errorf("reduceIndex ok = %v, want %v", ok, tc.reduces)
			}
			want := fullDecode(t, tc.body)
			if got := readIndexString(t, tc.body); !maps.Equal(got, want) {
				t.Fatalf("readIndex = %v, full decode = %v", got, want)
			}
		})
	}
}

// bigIndex builds a realistic-shape index: many charts, many versions, each
// carrying the descriptions/digests/URLs that make up most of a real one.
func bigIndex(charts, versions int) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nentries:\n")
	for c := range charts {
		fmt.Fprintf(&b, "  chart-%d:\n", c)
		for v := range versions {
			fmt.Fprintf(&b, "  - annotations:\n      artifacthub.io/changes: |\n        - kind: fixed\n          description: something %d\n"+
				"    apiVersion: v2\n    appVersion: 1.%d.0\n    created: \"2026-01-01T00:00:00Z\"\n"+
				"    dependencies:\n    - name: common\n      repository: oci://example\n      version: 2.x.x\n"+
				"    description: %s\n    digest: %064d\n    keywords:\n    - one\n    - two\n"+
				"    maintainers:\n    - email: a@example.com\n      name: a\n    name: chart-%d\n"+
				"    urls:\n    - https://example.com/charts/chart-%d-1.%d.0.tgz\n    version: 1.%d.0\n",
				v, v, strings.Repeat("a long description ", 8), v, c, c, v, v)
		}
	}
	b.WriteString("generated: \"2026-07-01T00:00:00Z\"\n")
	return b.String()
}

func TestReduceIndexOnABigIndex(t *testing.T) {
	body := bigIndex(20, 30)
	if _, ok := reduceIndex([]byte(body)); !ok {
		t.Fatal("reduceIndex declined helm's own layout")
	}
	if got, want := readIndexString(t, body), fullDecode(t, body); !maps.Equal(got, want) {
		t.Fatalf("readIndex disagrees with the full decode")
	}
}

func BenchmarkReadIndex(b *testing.B) {
	path := filepath.Join(b.TempDir(), "big-index.yaml")
	body := bigIndex(300, 30)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := readIndex(path); err != nil {
			b.Fatal(err)
		}
	}
}
