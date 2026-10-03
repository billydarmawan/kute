package update

import (
	"fmt"
	"os/exec"
	"runtime"
)

// OpenBrowser fires off the OS's "open a URL" command and does not wait for
// it to finish (it is reaped in the background) — 28b's 'o' key ("release notes ↗"). Never invoked with
// anything but a Release.HTMLURL this package itself fetched from the
// GitHub API, so there's no untrusted input reaching exec.Command here.
func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		// Not `cmd /c start`: cmd re-parses the URL, so an & or ^ in it
		// would split or mangle the command line.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("update: open browser: %w", err)
	}
	// Reap it in the background, or every 'o' leaves a zombie behind until
	// kute exits.
	go func() { _ = cmd.Wait() }()
	return nil
}
