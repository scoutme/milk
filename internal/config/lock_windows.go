//go:build windows

package config

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockSidecar takes an exclusive lock on a stable sidecar file next to the
// config file (LockFileEx byte-range lock on the first byte; the config file
// itself is rename-replaced, so it cannot be the lock target). Windows
// releases the lock when the process dies, matching the flock semantics of
// lock_unix.go.
func lockSidecar(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil && st.Size() == 0 {
		_, _ = f.Write([]byte{0})
	}
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
		_ = f.Close()
	}, nil
}
