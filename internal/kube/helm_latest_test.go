package kube

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// helmRevisionSecrets builds releases × revisions labelled release Secrets,
// shuffled out of revision order the way an informer cache hands them back.
func helmRevisionSecrets(namespace string, releases, revisions int, manifest string) []runtime.Object {
	var out []runtime.Object
	for rev := revisions; rev >= 1; rev-- {
		for i := 0; i < releases; i++ {
			status := "superseded"
			if rev == revisions {
				status = "deployed"
			}
			out = append(out, EncodeHelmReleaseSecret(HelmRelease{
				Name: fmt.Sprintf("release-%02d", i), Namespace: namespace,
				Chart: "chart", ChartVersion: fmt.Sprintf("1.0.%d", rev),
				Revision: rev, Status: status,
				Values:   "replicaCount: 2\nimage:\n  tag: v1\n",
				Manifest: manifest,
			}))
		}
	}
	// Interleave so the newest revision isn't always first.
	for i := 0; i+1 < len(out); i += 3 {
		out[i], out[i+1] = out[i+1], out[i]
	}
	return out
}

// TestDecodeLatestHelmReleasesDecodesOnlyTheNewestRevision pins review 08 H2:
// the list needs one row per release, so it must decode one Secret per
// release — not every revision helm keeps (ten by default) only to throw
// nine in ten away.
func TestDecodeLatestHelmReleasesDecodesOnlyTheNewestRevision(t *testing.T) {
	t.Parallel()
	objs := helmRevisionSecrets("production", 4, 10, "")
	decoded := map[string]int{}
	got := decodeLatestHelmReleases(objs, func(s *corev1.Secret) (HelmRelease, error) {
		decoded[s.Name]++
		return DecodeHelmReleaseSecret(s)
	})
	if len(decoded) != 4 {
		t.Fatalf("decoded %d Secrets (%v), want 4 — one per release", len(decoded), decoded)
	}
	for name := range decoded {
		if !strings.HasSuffix(name, ".v10") {
			t.Errorf("decoded %s, want only the .v10 revisions", name)
		}
	}
	if len(got) != 4 {
		t.Fatalf("got %d releases, want 4", len(got))
	}
	for _, r := range got {
		if r.Revision != 10 || r.Status != "deployed" {
			t.Errorf("%s: revision %d status %q, want 10 deployed", r.Name, r.Revision, r.Status)
		}
	}
}

// TestDecodeLatestHelmReleasesMatchesDecodingEverything holds the output to
// the decode-everything path it replaced, across the cases where choosing by
// label could diverge from choosing by decoded revision.
func TestDecodeLatestHelmReleasesMatchesDecodingEverything(t *testing.T) {
	t.Parallel()
	objs := helmRevisionSecrets("production", 3, 4, "kind: ConfigMap\n")
	objs = append(objs, helmRevisionSecrets("staging", 2, 2, "")...)

	// The newest revision of release-00 is corrupt: it must fall back to v3,
	// exactly as skip-on-error did.
	for _, obj := range objs {
		s := obj.(*corev1.Secret)
		if s.Namespace == "production" && s.Name == "sh.helm.release.v1.release-00.v4" {
			s.Data["release"] = []byte("not base64!")
		}
	}
	// An unlabelled Secret for release-01 carrying a newer revision than any
	// labelled one must still win on its decoded revision.
	unlabelled := EncodeHelmReleaseSecret(HelmRelease{Name: "release-01", Namespace: "production", Revision: 7, Status: "pending-upgrade"})
	unlabelled.Labels = nil
	// A non-numeric version label is treated as unlabelled, not as revision 0.
	badVersion := EncodeHelmReleaseSecret(HelmRelease{Name: "release-02", Namespace: "production", Revision: 9, Status: "failed"})
	badVersion.Labels["version"] = "nine"
	// Not a release at all: ignored by both.
	opaque := &corev1.Secret{Type: corev1.SecretTypeOpaque}
	opaque.Name, opaque.Namespace = "token", "production"
	objs = append(objs, unlabelled, badVersion, opaque)

	want := LatestHelmReleases(DecodeHelmReleases(objs))
	got := DecodeLatestHelmReleases(objs)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DecodeLatestHelmReleases diverged from decode-everything:\n got  %+v\n want %+v", got, want)
	}
	revisions := map[string]int{}
	for _, r := range got {
		revisions[r.Namespace+"/"+r.Name] = r.Revision
	}
	for key, rev := range map[string]int{
		"production/release-00": 3, "production/release-01": 7, "production/release-02": 9,
		"staging/release-00": 2, "staging/release-01": 2,
	} {
		if revisions[key] != rev {
			t.Errorf("%s = revision %d, want %d", key, revisions[key], rev)
		}
	}
}

// BenchmarkHelmReleaseList measures 18a's list decode at review 08 H2's
// shape: 30 releases × helm's default 10 revisions, ~100 KB manifests.
// "decode-all" is the path the list used to take; "latest-only" is
// DecodeLatestHelmReleases.
func BenchmarkHelmReleaseList(b *testing.B) {
	manifest := strings.Repeat("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: filler\ndata:\n  key: value-0123456789abcdef\n", 1100)
	objs := helmRevisionSecrets("production", 30, 10, manifest)
	b.Run("decode-all", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if got := LatestHelmReleases(DecodeHelmReleases(objs)); len(got) != 30 {
				b.Fatalf("got %d releases", len(got))
			}
		}
	})
	b.Run("latest-only", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if got := DecodeLatestHelmReleases(objs); len(got) != 30 {
				b.Fatalf("got %d releases", len(got))
			}
		}
	})
}
