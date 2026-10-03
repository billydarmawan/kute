package update

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// InstallInfo is 28b's "installed via" box: which of kute's distribution
// channels (README.md's Install section) produced the running binary, and the
// exact command to re-run to upgrade.
type InstallInfo struct {
	Manager string // "homebrew" | "scoop" | "go" | "curl" | "powershell"
	Command string
}

const (
	// upgrade, not install: `brew install` on an installed cask stops at
	// "already installed". --cask because releases ship as a cask.
	homebrewCommand   = "brew upgrade --cask kute-dev/tap/kute"
	scoopCommand      = "scoop update kute"
	curlCommand       = "curl -fsSL https://kute.dev/install.sh | sh"
	powershellCommand = "irm https://kute.dev/install.ps1 | iex"
	goCommand         = "go install github.com/kute-dev/kute/cmd/kute@latest"
)

// DetectInstall classifies execPath (typically os.Executable(), already
// resolved through filepath.EvalSymlinks so a Homebrew-managed symlink into
// /Cellar/ or /opt/homebrew/ is recognized even when the resolved path
// itself is what's inspected here, not a symlink) into one of kute's real
// install channels.
func DetectInstall(execPath string) InstallInfo {
	return detectInstall(execPath, runtime.GOOS, goBinDirs())
}

// goBinDirs is where `go install` puts binaries: $GOBIN, else every
// $GOPATH entry's bin, else ~/go/bin (GOPATH's default).
func goBinDirs() []string {
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		return []string{gobin}
	}
	var dirs []string
	for _, p := range filepath.SplitList(os.Getenv("GOPATH")) {
		if p != "" {
			dirs = append(dirs, filepath.Join(p, "bin"))
		}
	}
	if len(dirs) == 0 {
		if home, err := os.UserHomeDir(); err == nil {
			dirs = append(dirs, filepath.Join(home, "go", "bin"))
		}
	}
	return dirs
}

// detectInstall takes goos and goBins rather than reading the environment
// so every branch stays table-testable from any host. A binary in a `go
// install` directory gets `go install …@latest` — the install script would
// put a second kute in another directory on PATH, shadowing this one or
// shadowed by it. Anything else no package manager claims is attributed to
// the install script for the platform — re-running it is also the correct
// upgrade path there, so there's no separate "plain binary, no command"
// case to fall back to (docs/design README.md §28b).
func detectInstall(execPath, goos string, goBins []string) InstallInfo {
	switch {
	case isHomebrewPath(execPath):
		return InstallInfo{Manager: "homebrew", Command: homebrewCommand}
	case isScoopPath(execPath):
		return InstallInfo{Manager: "scoop", Command: scoopCommand}
	case isGoInstallPath(execPath, goBins):
		return InstallInfo{Manager: "go", Command: goCommand}
	case goos == "windows":
		return InstallInfo{Manager: "powershell", Command: powershellCommand}
	default:
		return InstallInfo{Manager: "curl", Command: curlCommand}
	}
}

// isHomebrewPath matches the cask kute ships as (/Caskroom/ — on Intel macOS
// /usr/local/Caskroom/, which contains no "/homebrew/" and was being told to
// run the install script over Homebrew's own symlink) as well as a formula's
// /Cellar/ and the Homebrew prefixes themselves.
func isHomebrewPath(execPath string) bool {
	p := slashPath(execPath)
	return strings.Contains(p, "/Caskroom/") ||
		strings.Contains(p, "/Cellar/") ||
		strings.Contains(p, "/homebrew/") ||
		strings.Contains(p, "linuxbrew")
}

// isScoopPath matches both scoop layouts — the per-user default under
// %USERPROFILE%\scoop and a `scoop install -g` under %ProgramData%\scoop —
// since both put the shim's target at <root>/apps/<name>/<version>/.
func isScoopPath(execPath string) bool {
	return strings.Contains(slashPath(execPath), "/scoop/apps/")
}

// isGoInstallPath reports whether execPath sits directly in one of goBins.
func isGoInstallPath(execPath string, goBins []string) bool {
	dir := strings.TrimSuffix(slashPath(execPath), "/"+baseName(execPath))
	for _, bin := range goBins {
		if bin != "" && strings.TrimSuffix(slashPath(filepath.Clean(bin)), "/") == dir {
			return true
		}
	}
	return false
}

// baseName is the last element of execPath under either separator.
func baseName(execPath string) string {
	p := slashPath(execPath)
	return p[strings.LastIndex(p, "/")+1:]
}

// slashPath normalizes separators without filepath.ToSlash, which is a no-op
// on Unix — so a Windows path handed to it from a Linux test (or from a
// WSL-visible install) keeps its backslashes and matches nothing.
func slashPath(execPath string) string {
	return strings.ReplaceAll(execPath, `\`, "/")
}
