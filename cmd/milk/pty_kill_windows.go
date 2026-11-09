//go:build windows

package main

import "os/exec"

// killPTYProcessGroup mirrors the unix group kill for the force-kill
// escape. Windows has no embedded-PTY pane in practice (creack/pty ships a
// stub, so launchPTYPane always falls back to the piped path that never
// sets ptyPane), so killing the direct child is all that can apply if this
// is ever reached.
func killPTYProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
