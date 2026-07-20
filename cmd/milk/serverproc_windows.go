//go:build windows

package main

import (
	"os"
	"syscall"
)

// connRefusedErrno reports whether err wraps WSAECONNREFUSED on Windows.
func connRefusedErrno(err error) bool {
	// syscall.ECONNREFUSED maps to WSAECONNREFUSED on Windows via the Go runtime.
	var errno syscall.Errno
	if e, ok := err.(syscall.Errno); ok {
		errno = e
	}
	return errno == syscall.ECONNREFUSED
}

func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000008} // DETACHED_PROCESS
}

// killProcess terminates the given PID on Windows.
func killProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}
