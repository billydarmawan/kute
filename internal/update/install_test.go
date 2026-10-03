package update

import (
	"runtime"
	"testing"
)

func TestDetectInstall(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		goos    string
		manager string
		command string
	}{
		{"macos homebrew cellar", "/opt/homebrew/Cellar/kute/0.2.0/bin/kute", "darwin", "homebrew", homebrewCommand},
		{"linux homebrew cellar", "/home/linuxbrew/.linuxbrew/Cellar/kute/0.2.0/bin/kute", "linux", "homebrew", homebrewCommand},
		{"intel macos homebrew cellar", "/usr/local/Cellar/kute/0.2.0/bin/kute", "darwin", "homebrew", homebrewCommand},
		{"apple silicon homebrew cask", "/opt/homebrew/Caskroom/kute/0.9.0/kute", "darwin", "homebrew", homebrewCommand},
		{"intel macos homebrew cask", "/usr/local/Caskroom/kute/0.9.0/kute", "darwin", "homebrew", homebrewCommand},
		{"linux homebrew cask", "/home/linuxbrew/.linuxbrew/Caskroom/kute/0.9.0/kute", "linux", "homebrew", homebrewCommand},
		{"install script default location", "/usr/local/bin/kute", "linux", "curl", curlCommand},
		{"install script home-local", "/home/user/.local/bin/kute", "linux", "curl", curlCommand},
		{"go run dev binary", "/tmp/go-build12345/b001/kute", "linux", "curl", curlCommand},

		{"scoop per-user", `C:\Users\dev\scoop\apps\kute\current\kute.exe`, "windows", "scoop", scoopCommand},
		{"scoop global", `C:\ProgramData\scoop\apps\kute\0.2.0\kute.exe`, "windows", "scoop", scoopCommand},
		{"powershell installer default location", `C:\Users\dev\AppData\Local\Programs\kute\kute.exe`, "windows", "powershell", powershellCommand},
		{"windows unpacked zip", `D:\tools\kute.exe`, "windows", "powershell", powershellCommand},
		// A path can *look* like scoop's on a Unix host (a mounted share,
		// a directory someone named "scoop"); the shape is what's matched,
		// not the platform, and `scoop update` is still the right answer
		// only on Windows — but this stays deliberately goos-independent
		// so a WSL-visible scoop install isn't told to re-run curl.
		{"scoop layout under wsl", "/mnt/c/Users/dev/scoop/apps/kute/current/kute.exe", "linux", "scoop", scoopCommand},

		// go install puts the binary in GOBIN/GOPATH's bin; the install
		// script would add a second kute elsewhere on PATH (14 L14).
		{"go install default gopath", "/home/user/go/bin/kute", "linux", "go", goCommand},
		{"go install custom gobin", "/opt/gobin/kute", "linux", "go", goCommand},
		{"go install on windows", `C:\Users\dev\go\bin\kute.exe`, "windows", "go", goCommand},
		{"a subdirectory of go bin is not go install", "/home/user/go/bin/sub/kute", "linux", "curl", curlCommand},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectInstall(tt.path, tt.goos, []string{"/home/user/go/bin", "/opt/gobin/", `C:\Users\dev\go\bin`})
			if got.Manager != tt.manager || got.Command != tt.command {
				t.Errorf("detectInstall(%q, %q) = %+v, want {%q %q}", tt.path, tt.goos, got, tt.manager, tt.command)
			}
		})
	}
}

// DetectInstall is the exported wrapper; it must agree with detectInstall on
// the host's own GOOS, which is what app.buildUpdateFactory actually calls.
func TestDetectInstallUsesHostGOOS(t *testing.T) {
	const p = "/usr/local/bin/kute"
	if got, want := DetectInstall(p), detectInstall(p, runtime.GOOS, goBinDirs()); got != want {
		t.Errorf("DetectInstall(%q) = %+v, want %+v", p, got, want)
	}
}
