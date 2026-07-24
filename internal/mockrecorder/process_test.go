//go:build integration

package mockrecorder_test

import (
	"fmt"
	"os/exec"
	"testing"
)

type processHandle struct {
	Process *exec.Cmd
}

func (p *processHandle) Kill() error {
	if p.Process.Process != nil {
		return p.Process.Process.Kill()
	}
	return nil
}

type killable interface {
	Kill() error
}

type processWrapper struct {
	Process killable
}

func startProcess(t *testing.T, bin string, port int, name, recDir string) *processWrapper {
	t.Helper()
	cmd := exec.Command(bin, "server",
		"--port", fmt.Sprintf("%d", port),
		"--name", name,
		"--rec-dir", recDir,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mock server: %v", err)
	}
	return &processWrapper{Process: &processHandle{Process: cmd}}
}
