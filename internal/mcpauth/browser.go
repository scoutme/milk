package mcpauth

import (
	"os/exec"
	"runtime"
)

// OpenURL best-effort opens url in the system's default browser. It never
// blocks the caller — the launched process is not waited on — and errors
// (e.g. no display, missing opener binary) are returned but expected to be
// non-fatal: the caller should also print the URL for manual use.
func OpenURL(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
