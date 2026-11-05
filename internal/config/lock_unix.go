//go:build unix

package config

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockSidecar takes an exclusive advisory lock on a stable sidecar file next
// to the config file (the config file itself is rename-replaced, so it
// cannot be the lock target). flock is per-open-file-description: opening a
// second time in the same process would block against the first, which is
// exactly the write-write serialization we want (and why callers must never
// double-lock). The OS releases the lock when the process dies, so a crash
// mid-write cannot leave a stale lock behind.
func lockSidecar(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// Ensure the file has at least one byte so byte-range locks on Windows
	// (see lock_windows.go) behave identically across platforms.
	if st, err := f.Stat(); err == nil && st.Size() == 0 {
		_, _ = f.Write([]byte{0})
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}
