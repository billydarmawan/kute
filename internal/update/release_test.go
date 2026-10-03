package update

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGitHubCheckerLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/kute-dev/kute/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"tag_name": "v0.2.1",
			"published_at": "2026-07-18T00:00:00Z",
			"html_url": "https://github.com/kute-dev/kute/releases/tag/v0.2.1",
			"assets": [
				{"name": "kute_darwin_arm64.tar.gz", "browser_download_url": "https://example.com/kute.tar.gz"},
				{"name": "changelog.json", "browser_download_url": "https://example.com/changelog.json"}
			]
		}`))
	}))
	defer srv.Close()

	c := GitHubChecker{Repo: "kute-dev/kute", BaseURL: srv.URL}
	rel, err := c.Latest(t.Context())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.Version != "0.2.1" {
		t.Errorf("Version = %q, want 0.2.1 (leading v stripped)", rel.Version)
	}
	if rel.HTMLURL != "https://github.com/kute-dev/kute/releases/tag/v0.2.1" {
		t.Errorf("HTMLURL = %q", rel.HTMLURL)
	}
	if rel.ChangelogURL != "https://example.com/changelog.json" {
		t.Errorf("ChangelogURL = %q", rel.ChangelogURL)
	}
	wantPublished := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	if !rel.PublishedAt.Equal(wantPublished) {
		t.Errorf("PublishedAt = %v, want %v", rel.PublishedAt, wantPublished)
	}
}

func TestGitHubCheckerLatestNoChangelogAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name": "v0.1.0", "assets": []}`))
	}))
	defer srv.Close()

	c := GitHubChecker{Repo: "kute-dev/kute", BaseURL: srv.URL}
	rel, err := c.Latest(t.Context())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.ChangelogURL != "" {
		t.Errorf("ChangelogURL = %q, want empty", rel.ChangelogURL)
	}
}

func TestGitHubCheckerLatestNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := GitHubChecker{Repo: "kute-dev/kute", BaseURL: srv.URL}
	if _, err := c.Latest(t.Context()); err == nil {
		t.Fatal("Latest: expected an error for a 404 response")
	}
}

func TestGitHubCheckerLatestMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c := GitHubChecker{Repo: "kute-dev/kute", BaseURL: srv.URL}
	if _, err := c.Latest(t.Context()); err == nil {
		t.Fatal("Latest: expected an error for malformed JSON")
	}
}

func TestGitHubCheckerChangelog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/changelog.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"type":"fix","text":"rollout watch could miss the final ready event"},{"type":"new","text":"resources editor accepts binary suffixes"}]`))
	}))
	defer srv.Close()

	c := GitHubChecker{Repo: "kute-dev/kute"}
	entries, err := c.Changelog(t.Context(), Release{ChangelogURL: srv.URL + "/changelog.json"})
	if err != nil {
		t.Fatalf("Changelog: %v", err)
	}
	if len(entries) != 2 || entries[0].Type != "fix" || entries[1].Type != "new" {
		t.Fatalf("Changelog entries = %+v", entries)
	}
}

func TestGitHubCheckerChangelogEmptyURL(t *testing.T) {
	c := GitHubChecker{Repo: "kute-dev/kute"}
	entries, err := c.Changelog(t.Context(), Release{})
	if err != nil || entries != nil {
		t.Fatalf("Changelog with no asset = (%v, %v), want (nil, nil)", entries, err)
	}
}

// TestGitHubCheckerLatestBoundsTheBody: a body larger than any real release
// document is refused rather than decoded into memory without bound
// (14 L11c).
func TestGitHubCheckerLatestBoundsTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name": "v0.2.1", "body": "`))
		_, _ = w.Write([]byte(strings.Repeat("x", 2*maxResponseBytes)))
		_, _ = w.Write([]byte(`"}`))
	}))
	defer srv.Close()

	c := GitHubChecker{Repo: "kute-dev/kute", BaseURL: srv.URL}
	if _, err := c.Latest(t.Context()); err == nil {
		t.Fatal("Latest decoded a body past maxResponseBytes, want an error")
	}
}
