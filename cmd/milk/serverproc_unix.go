//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// detachedSysProcAttr returns a SysProcAttr that starts the process in a new
// process group so it is not killed when milk exits (SIGINT/SIGHUP propagation
// is limited to the controlling process group).
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// connRefusedErrno reports whether err wraps ECONNREFUSED.
func connRefusedErrno(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == syscall.ECONNREFUSED
}

// killProcess sends SIGTERM to the process group of pid (which equals pid
// because we started the shell with Setpgid: true). This ensures llama-server
// (child of the sh -c wrapper) is also terminated.
func killProcess(pid int) error {
	// Negative pid sends the signal to every process in the process group.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		// Fall back to killing only the process itself.
		proc, ferr := os.FindProcess(pid)
		if ferr != nil {
			return err
		}
		return proc.Signal(syscall.SIGTERM)
	}
	return nil
}
