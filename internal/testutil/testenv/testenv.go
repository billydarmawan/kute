// Package testenv provides environment fixups shared by tests that need to
// redirect a lookup the standard library resolves differently per platform.
package testenv

import (
	"runtime"
	"testing"
)

// SetHome points os.UserHomeDir at dir for the duration of the test.
//
// t.Setenv("HOME", …) alone is not enough: os.UserHomeDir reads $HOME only on
// unix, and %USERPROFILE% on Windows. A test that sets just HOME therefore
// passes on Linux and silently keeps resolving the *real* home on Windows,
// which fails twice over — the assertion sees a path it never chose, and any
// code under test that writes lands in the developer's own home directory,
// leaking state into every later test in the package.
//
// It also clears XDG_CONFIG_HOME, which config.Path prefers over the home
// directory: a developer who sets it would otherwise have tests read and
// write their real config.yaml. A test that wants it sets it afterwards.
func SetHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "")
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", dir)
	}
}
