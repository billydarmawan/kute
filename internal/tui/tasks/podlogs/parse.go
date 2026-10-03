package podlogs

import (
	"regexp"
	"strings"
	"time"
)

// Severity levels a log line can carry (docs/design README.md §5b: "level
// token colored INF green, WRN yellow, ERR red").
const (
	SeverityInfo = "INF"
	SeverityWarn = "WRN"
	SeverityErr  = "ERR"
)

var (
	reJSONLevel = regexp.MustCompile(`"level"\s*:\s*"([A-Za-z]+)"`)
	reLevelEq   = regexp.MustCompile(`(?i)\blevel=([A-Za-z]+)`)
	reToken     = regexp.MustCompile(`\b(ERROR|ERR|FATAL|PANIC|WARN|WARNING|WRN|INFO|INF)\b`)
)

// parseSeverity classifies a log line's severity by, in order: a JSON
// "level" field, a "level=" token (common structured-log convention), or a
// bare INF/WRN/ERR-family word anywhere in the line. Returns "" when none
// match.
func parseSeverity(line string) string {
	if m := reJSONLevel.FindStringSubmatch(line); m != nil {
		if sev := normalizeSeverity(m[1]); sev != "" {
			return sev
		}
	}
	if m := reLevelEq.FindStringSubmatch(line); m != nil {
		if sev := normalizeSeverity(m[1]); sev != "" {
			return sev
		}
	}
	if m := reToken.FindStringSubmatch(line); m != nil {
		return normalizeSeverity(m[1])
	}
	return ""
}

func normalizeSeverity(raw string) string {
	switch strings.ToUpper(raw) {
	case "ERROR", "ERR", "FATAL", "PANIC":
		return SeverityErr
	case "WARN", "WARNING", "WRN":
		return SeverityWarn
	case "INFO", "INF":
		return SeverityInfo
	default:
		return ""
	}
}

// rfc3339Prefix matches the RFC3339Nano timestamp GetLogs prepends to each
// line when PodLogOptions.Timestamps is set.
var rfc3339Prefix = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}))\s(.*)$`)

// splitTimestamp pulls a server-prepended RFC3339 timestamp off line,
// returning it reformatted as HH:MM:SS plus the remaining message. Returns
// ("", line) when line has no such prefix (kube/fake's seeded lines don't
// carry one, so demo mode shows no timestamp column — real clusters do).
// lineTime is the kubelet timestamp prefix of a raw line at full
// precision, for the history/follow seam's dedup (streamContainer).
func lineTime(line string) (time.Time, bool) {
	m := rfc3339Prefix.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, m[1])
	return t, err == nil
}

func splitTimestamp(line string) (timestamp, message string) {
	m := rfc3339Prefix.FindStringSubmatch(line)
	if m == nil {
		return "", line
	}
	t, err := time.Parse(time.RFC3339Nano, m[1])
	if err != nil {
		return "", line
	}
	return t.Format("15:04:05"), m[2]
}
