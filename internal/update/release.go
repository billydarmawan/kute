// Package update is kute's own version-check logic (docs/design/README.md
// §28a/28b): fetching the latest GitHub release and its changelog.json
// asset, detecting how kute itself was installed, and comparing semver
// strings. It has no dependency on internal/tui — the same "leaf package"
// shape as internal/kube — so it's usable and testable with no UI or live
// network involved.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// Release is the subset of a GitHub release the update-check flow needs.
// Version has any leading "v" stripped (git tags are "v0.2.1"; kute's own
// build version and every comparison elsewhere in this package are
// "0.2.1").
type Release struct {
	Version      string
	PublishedAt  time.Time
	HTMLURL      string
	ChangelogURL string // browser_download_url of the changelog.json asset; "" if the release has none
}

// ChangelogEntry is one row of changelog.json (produced by
// scripts/release-notes.sh via git-cliff, see cliff.toml) — Type is one of
// "new"/"fix"/"perf", Text is the verbatim commit description.
type ChangelogEntry struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Checker is the seam 28a/28b's ambient check and manual re-check ('r')
// both call through — satisfied by GitHubChecker for real use, faked in
// tests.
type Checker interface {
	Latest(ctx context.Context) (Release, error)
	Changelog(ctx context.Context, rel Release) ([]ChangelogEntry, error)
}

// GitHubChecker is the real Checker, against the public GitHub REST API.
// BaseURL defaults to the real API when empty — overridable so tests point
// it at an httptest.Server instead of the network.
type GitHubChecker struct {
	// Repo is "owner/name", e.g. "kute-dev/kute".
	Repo    string
	BaseURL string
	Client  *http.Client
}

const defaultGitHubAPI = "https://api.github.com"

func (c GitHubChecker) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return defaultGitHubAPI
}

func (c GitHubChecker) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type githubRelease struct {
	TagName     string        `json:"tag_name"`
	PublishedAt time.Time     `json:"published_at"`
	HTMLURL     string        `json:"html_url"`
	Assets      []githubAsset `json:"assets"`
}

// Latest fetches GET /repos/{Repo}/releases/latest — GitHub's own semantics
// already exclude drafts and prereleases, which is exactly 28a's "pre-
// releases never surface unless you run one" default behavior for the
// common (stable) case.
func (c GitHubChecker) Latest(ctx context.Context) (Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", c.baseURL(), c.Repo)
	var gr githubRelease
	if err := getJSON(ctx, c.client(), url, &gr); err != nil {
		return Release{}, err
	}
	rel := Release{
		Version:     strings.TrimPrefix(gr.TagName, "v"),
		PublishedAt: gr.PublishedAt,
		HTMLURL:     gr.HTMLURL,
	}
	for _, a := range gr.Assets {
		if a.Name == "changelog.json" {
			rel.ChangelogURL = a.BrowserDownloadURL
			break
		}
	}
	return rel, nil
}

// Changelog fetches rel.ChangelogURL (a changelog.json release asset —
// scripts/release-notes.sh's [{type, text}] shape) — an empty
// ChangelogURL (a release with no such asset) returns nil, nil rather than
// an error.
func (c GitHubChecker) Changelog(ctx context.Context, rel Release) ([]ChangelogEntry, error) {
	if rel.ChangelogURL == "" {
		return nil, nil
	}
	var entries []ChangelogEntry
	if err := getJSON(ctx, c.client(), rel.ChangelogURL, &entries); err != nil {
		return nil, err
	}
	for i := range entries {
		entries[i].Type = stripControls(entries[i].Type)
		entries[i].Text = stripControls(entries[i].Text)
	}
	return entries, nil
}

// stripControls drops C0/C1 control characters (ESC among them) from text
// fetched off a release asset before 28b renders it: changelog.json isn't
// signed or verified, and a tampered one must not be able to write
// terminal escape sequences. Whitespace controls become a space, since an
// entry renders as one line.
func stripControls(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
}

func getJSON(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update: GET %s: unexpected status %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out)
}

// maxResponseBytes caps one release-feed or changelog.json body. Both are a
// few KiB; a body that never ends (a misbehaving proxy, a hostile mirror)
// must not be decoded into memory without bound.
const maxResponseBytes = 1 << 20
