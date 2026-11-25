package obs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/scoutme/milk/internal/config"
)

// RotatingWriter is an io.WriteCloser that appends to a file and rotates it
// when the *shared* file's size reaches a threshold, keeping a bounded
// number of rotated files. Rotation is sized off the file's actual stat size
// (not a per-process byte counter) and serialized across processes on a
// "<file>.lock" sidecar, so several milk sessions writing the same log
// rotate it once at the cap instead of each racing its own count (#457).
type RotatingWriter struct {
	mu       sync.Mutex
	basePath string // primary log file path, e.g. "~/.milk/claude_debug.ndjson"
	ext      string // file extension including the dot, e.g. ".ndjson"
	dir      string // parent directory of basePath
	maxBytes int64
	maxFiles int

	file   *os.File
	closed bool
}

// NewRotatingWriter creates a RotatingWriter. It ensures the parent directory
// of basePath exists and opens basePath for append. maxBytes is the per-file
// size limit; maxFiles is the total number of files (primary + rotated).
func NewRotatingWriter(basePath string, maxBytes int64, maxFiles int) (*RotatingWriter, error) {
	dir := filepath.Dir(basePath)
	ext := filepath.Ext(basePath)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("rotatingwriter: mkdir %s: %w", dir, err)
	}

	f, err := os.OpenFile(basePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("rotatingwriter: open %s: %w", basePath, err)
	}

	return &RotatingWriter{
		basePath: basePath,
		ext:      ext,
		dir:      dir,
		maxBytes: maxBytes,
		maxFiles: maxFiles,
		file:     f,
	}, nil
}

// Write appends p to the current log file. The rotation decision uses the
// file's actual size (including bytes other processes appended), not this
// process's own byte count.
func (rw *RotatingWriter) Write(p []byte) (n int, err error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.closed {
		return 0, fmt.Errorf("rotatingwriter: write to closed writer")
	}
	size, err := rw.reopenIfStaleLocked()
	if err != nil {
		return 0, err
	}

	n, err = rw.file.Write(p)

	if size+int64(n) >= rw.maxBytes {
		if rerr := rw.rotate(); rerr != nil && err == nil {
			err = rerr
		}
	}

	return n, err
}

// Close closes the current file handle. Subsequent Write calls return an error.
func (rw *RotatingWriter) Close() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.closed {
		return nil
	}
	rw.closed = true
	return rw.file.Close()
}

// rotate performs log rotation. Caller must hold rw.mu. The rename chain runs
// under the "<basePath>.lock" sidecar so two processes (or a rotation racing
// a /otel trim) serialize instead of interleaving their shifts, and the size
// is re-checked under the lock: if another process already rotated or a trim
// emptied the file, there is nothing to shift.
func (rw *RotatingWriter) rotate() error {
	if unlock, lerr := config.LockSidecar(rw.basePath + ".lock"); lerr == nil {
		defer unlock()
	} else {
		Debug("rotatingwriter: lock unavailable", "file", rw.basePath, "err", lerr)
	}

	if _, err := rw.reopenIfStaleLocked(); err != nil {
		return err
	}
	if info, serr := rw.file.Stat(); serr == nil && info.Size() < rw.maxBytes {
		return nil // another process already rotated (or the file was trimmed)
	}

	// Close current file.
	if err := rw.file.Close(); err != nil {
		return fmt.Errorf("rotatingwriter: close before rotate: %w", err)
	}

	// Delete the file at the highest index to make room.
	highest := rw.rotatedPath(rw.maxFiles - 1)
	os.Remove(highest) // ignore error (may not exist)

	// Shift files downward: i → i+1 for i = maxFiles-2 down to 2.
	for i := rw.maxFiles - 2; i >= 2; i-- {
		src := rw.rotatedPath(i)
		dst := rw.rotatedPath(i + 1)
		if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rotatingwriter: rename %s → %s: %w", src, dst, err)
		}
	}

	// Shift file 1 → 2.
	src1 := rw.rotatedPath(1)
	dst2 := rw.rotatedPath(2)
	if err := os.Rename(src1, dst2); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rotatingwriter: rename %s → %s: %w", src1, dst2, err)
	}

	// Move primary file to position 1.
	if err := os.Rename(rw.basePath, rw.rotatedPath(1)); err != nil {
		return fmt.Errorf("rotatingwriter: rename primary: %w", err)
	}

	// Open a fresh primary file.
	f, err := os.OpenFile(rw.basePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("rotatingwriter: open fresh: %w", err)
	}

	rw.file = f
	return nil
}

// reopenIfStaleLocked reopens the active path when another process or writer
// has renamed/recreated it while this writer still holds the old descriptor,
// and returns the current file's size (after any reopen) — the value the
// rotation decision is based on, so it reflects bytes every session wrote.
// Caller must hold rw.mu.
func (rw *RotatingWriter) reopenIfStaleLocked() (int64, error) {
	current, err := rw.file.Stat()
	if err != nil {
		return 0, fmt.Errorf("rotatingwriter: stat current file: %w", err)
	}
	active, err := os.Stat(rw.basePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return 0, fmt.Errorf("rotatingwriter: stat active file: %w", err)
		}
		return rw.reopenLocked()
	}
	if os.SameFile(current, active) {
		return current.Size(), nil
	}
	return rw.reopenLocked()
}

// reopenLocked closes the current descriptor and appends to the active path.
// Caller must hold rw.mu. Returns the reopened file's size.
func (rw *RotatingWriter) reopenLocked() (int64, error) {
	if err := rw.file.Close(); err != nil {
		return 0, fmt.Errorf("rotatingwriter: close stale file: %w", err)
	}
	f, err := os.OpenFile(rw.basePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("rotatingwriter: reopen active file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return 0, fmt.Errorf("rotatingwriter: stat reopened file: %w", err)
	}
	rw.file = f
	return info.Size(), nil
}

// rotatedPath returns the path for the nth rotated file, preserving the
// original extension. For example, with basePath "debug.ndjson" and n=1, it
// returns "debug.1.ndjson".
func (rw *RotatingWriter) rotatedPath(n int) string {
	base := strings.TrimSuffix(filepath.Base(rw.basePath), rw.ext)
	return filepath.Join(rw.dir, fmt.Sprintf("%s.%d%s", base, n, rw.ext))
}
