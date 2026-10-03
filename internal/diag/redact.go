package diag

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// secretPatterns match credential-shaped text that can reach the stream
// verbatim — an exec credential plugin's own stderr is carried in
// ConnState.Err, and kute can't know what a given plugin prints. The
// stream ends up in a crash report the bug form asks users to attach
// publicly, so anything that looks like a token is dropped on the way in.
var secretPatterns = []*regexp.Regexp{
	// A bearer token in a header or error message.
	regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]+`),
	// key=value / key: value for token-ish keys (access_token=…,
	// "id-token": "…", client_secret=…, password: …).
	regexp.MustCompile(`(?i)\b([a-z_-]*(?:token|secret|password))("?\s*[=:]\s*"?)[^\s"'&,;]+`),
	// A JWT anywhere (header.payload.signature, header always "eyJ").
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`),
}

// Redact scrubs s for a log line or crash report: credential-shaped
// substrings become [redacted], and the user's home directory becomes ~ so
// a path doesn't carry their username into a public bug report.
func Redact(s string) string {
	for i, re := range secretPatterns {
		switch i {
		case 0:
			s = re.ReplaceAllString(s, "$1 [redacted]")
		case 1:
			s = re.ReplaceAllString(s, "$1$2[redacted]")
		default:
			s = re.ReplaceAllString(s, "[redacted]")
		}
	}
	return tildeHome(s)
}

// tildeHome replaces the home directory, as a path prefix, with ~.
func tildeHome(s string) string {
	home, err := os.UserHomeDir()
	if err != nil || len(home) <= 1 {
		return s
	}
	home = filepath.Clean(home)
	if s == home {
		return "~"
	}
	return strings.ReplaceAll(s, home+string(filepath.Separator), "~"+string(filepath.Separator))
}
