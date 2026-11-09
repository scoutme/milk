//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// killPTYProcessGroup force-kills the pane child's whole process group —
// the last step of forceKillPTYPane's escape hatch. creack/pty starts the
// child with setsid, so its pid doubles as its process-group id: ^C alone
// only reaches the foreground group, and a task runner may have moved the
// real work into children of its own, so SIGKILL goes to the group *and*
// the direct child. Errors are deliberately ignored: ESRCH just means the
// process already exited on its own.
func killPTYProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
