package obs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// RotatingWriter is an io.WriteCloser that appends to a file and rotates it
// when a size threshold is reached, keeping a bounded number of rotated files.
type RotatingWriter struct {
	mu       sync.Mutex
	basePath string // primary log file path, e.g. "~/.milk/claude_debug.ndjson"
	ext      string // file extension including the dot, e.g. ".ndjson"
	dir      string // parent directory of basePath
	maxBytes int64
	maxFiles int

	file    *os.File
	written int64 // bytes written to the current file since last rotation
	closed  bool
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

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("rotatingwriter: stat %s: %w", basePath, err)
	}

	return &RotatingWriter{
		basePath: basePath,
		ext:      ext,
		dir:      dir,
		maxBytes: maxBytes,
		maxFiles: maxFiles,
		file:     f,
		written:  info.Size(),
	}, nil
}

// Write appends p to the current log file. If the cumulative bytes written
// since the last rotation reach maxBytes, the files are rotated.
func (rw *RotatingWriter) Write(p []byte) (n int, err error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.closed {
		return 0, fmt.Errorf("rotatingwriter: write to closed writer")
	}

	n, err = rw.file.Write(p)
	rw.written += int64(n)

	if rw.written >= rw.maxBytes {
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

// rotate performs log rotation. Caller must hold rw.mu.
func (rw *RotatingWriter) rotate() error {
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
	f, err := os.OpenFile(rw.basePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("rotatingwriter: open fresh: %w", err)
	}

	rw.file = f
	rw.written = 0
	return nil
}

// rotatedPath returns the path for the nth rotated file, preserving the
// original extension. For example, with basePath "debug.ndjson" and n=1,
// it returns "debug.1.ndjson".
func (rw *RotatingWriter) rotatedPath(n int) string {
	base := strings.TrimSuffix(filepath.Base(rw.basePath), rw.ext)
	return filepath.Join(rw.dir, fmt.Sprintf("%s.%d%s", base, n, rw.ext))
}
