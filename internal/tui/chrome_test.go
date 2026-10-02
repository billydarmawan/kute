package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/kute-dev/kute/internal/kube"
)

// TestLiveConnBadge pins the shared badge's phase mapping: connected (and
// no-cluster/zero states) render the screen's own connected text plus a
// latency suffix when known; Reconnecting/Failed always render the red
// disconnected badge regardless of a stale latency value.
func TestLiveConnBadge(t *testing.T) {
	theme := Dark()
	cases := []struct {
		name string
		conn kube.ConnState
		want string
	}{
		{"zero state", kube.ConnState{}, GlyphRunning + " connected"},
		{"connected with latency", kube.ConnState{Phase: kube.ConnConnected, Latency: 12 * time.Millisecond}, GlyphRunning + " connected · 12ms"},
		{"reconnecting", kube.ConnState{Phase: kube.ConnReconnecting, Latency: 12 * time.Millisecond}, GlyphProbing + " disconnected"},
		{"failed", kube.ConnState{Phase: kube.ConnFailed}, GlyphProbing + " disconnected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LiveConnBadge(theme, tc.conn, GlyphRunning+" connected")
			if got.Text != tc.want {
				t.Fatalf("badge text = %q, want %q", got.Text, tc.want)
			}
		})
	}
}

func TestLiveConnBadgeKeepsConnectedText(t *testing.T) {
	got := LiveConnBadge(Dark(), kube.ConnState{Phase: kube.ConnConnected}, "● live · updates as object changes")
	if !strings.HasPrefix(got.Text, "● live · updates as object changes") {
		t.Fatalf("badge text = %q, want the caller's connected text", got.Text)
	}
}

// TestBuildForwardChip pins 13d's three ambient-chip states: nothing when
// no forwards exist, a quiet purple count while every session is healthy,
// and a yellow chip the moment any session is reconnecting (docs/design
// README.md §13d).
func TestBuildForwardChip(t *testing.T) {
	theme := Dark()
	cases := []struct {
		name    string
		summary ForwardSummary
		want    string
	}{
		{"none", ForwardSummary{}, ""},
		{"all active", ForwardSummary{Active: 3}, GlyphForward + " 3"},
		{"some reconnecting", ForwardSummary{Active: 2, Reconnecting: 1}, GlyphForward + " 2 · " + GlyphProbing + " 1"},
		{"all reconnecting", ForwardSummary{Reconnecting: 1}, GlyphProbing + " 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildForwardChip(theme, tc.summary)
			if got.Text != tc.want {
				t.Fatalf("BuildForwardChip(%+v).Text = %q, want %q", tc.summary, got.Text, tc.want)
			}
		})
	}
}

func TestBuildForwardChipColorsByState(t *testing.T) {
	theme := Dark()
	active := BuildForwardChip(theme, ForwardSummary{Active: 1})
	if active.Style.GetForeground() != theme.Accent {
		t.Fatalf("healthy chip color = %v, want theme.Accent", active.Style.GetForeground())
	}
	reconnecting := BuildForwardChip(theme, ForwardSummary{Active: 1, Reconnecting: 1})
	if reconnecting.Style.GetForeground() != theme.Warn {
		t.Fatalf("reconnecting chip color = %v, want theme.Warn", reconnecting.Style.GetForeground())
	}
}

// TestHeaderKeepsConnBadgeWithLongContext pins M3: an EKS ARN context used
// to push the whole right side — the connection badge included — off the
// header. The badge must survive at every width, with the context
// middle-ellipsized and the kind kept whole.
func TestHeaderKeepsConnBadgeWithLongContext(t *testing.T) {
	theme := Dark()
	h := HeaderState{
		Crumbs: append(BrandCrumbs(theme),
			Crumb{Text: " │ "},
			Crumb{Text: "c "},
			Crumb{Text: "arn:aws:eks:us-east-1:123456789012:cluster/payments-prod"},
			Crumb{Text: " › "},
			Crumb{Text: "n "},
			Crumb{Text: "default"},
			Crumb{Text: " › "},
			Crumb{Text: "g "},
			Crumb{Text: "CustomResourceDefinitions", Fixed: true},
		),
		SyncNote:    "sync 2s",
		UpdateChip:  ConnBadge{Text: "↑ 0.2.1"},
		ForwardChip: ConnBadge{Text: "⇄ 2"},
		Conn:        LiveConnBadge(theme, kube.ConnState{Phase: kube.ConnReconnecting}, "● connected"),
	}
	for _, width := range []int{80, 120} {
		line := renderHeaderV2(h, theme, width)[0]
		if got := ansi.StringWidth(line); got != width {
			t.Fatalf("width %d: header is %d cells wide:\n%s", width, got, line)
		}
		for _, want := range []string{"◌ disconnected", "CustomResourceDefinitions", "…"} {
			if !strings.Contains(line, want) {
				t.Fatalf("width %d: header missing %q:\n%s", width, want, line)
			}
		}
	}
	if line := renderHeaderV2(h, theme, 120)[0]; !strings.Contains(line, "↑ 0.2.1") || !strings.Contains(line, "⇄ 2") || !strings.Contains(line, "payments-prod") {
		t.Fatalf("at 120 columns the chips should still fit beside a shrunk context:\n%s", line)
	}
}
