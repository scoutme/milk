//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"syscall"
)

// connRefusedErrno reports whether err wraps WSAECONNREFUSED on Windows.
func connRefusedErrno(err error) bool {
	var errno syscall.Errno
	if e, ok := err.(syscall.Errno); ok {
		errno = e
	}
	return errno == syscall.ECONNREFUSED
}

// detachedSysProcAttr on Windows uses CREATE_NEW_PROCESS_GROUP so the server
// process is detached from milk's console but still manageable.
func detachedSysProcAttr() *syscall.SysProcAttr {
	// CREATE_NEW_PROCESS_GROUP (0x00000200) detaches the process from the
	// current console group without hiding it completely (unlike DETACHED_PROCESS),
	// which is required for taskkill /T to traverse children correctly.
	return &syscall.SysProcAttr{CreationFlags: 0x00000200}
}

// killProcess terminates the process tree rooted at pid using taskkill /T /F,
// which kills the process and all its children — necessary because the PID
// points to the sh wrapper, not llama-server itself.
func killProcess(pid int) error {
	out, err := exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprintf("%d", pid)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("taskkill: %w (output: %s)", err, out)
	}
	return nil
}
