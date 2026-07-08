//go:build windows

package local

import "os/exec"

func setProcGroup(_ *exec.Cmd) {}

func killProcGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
