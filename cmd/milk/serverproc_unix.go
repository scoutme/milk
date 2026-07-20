//go:build !windows

package main

import (
	"os"
	"syscall"
)

// detachedSysProcAttr returns a SysProcAttr that starts the process in a new
// process group so it is not killed when milk exits (SIGINT/SIGHUP propagation
// is limited to the controlling process group).
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killProcess sends SIGTERM to the given PID.
func killProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}
