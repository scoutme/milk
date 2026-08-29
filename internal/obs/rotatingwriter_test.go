package obs

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRotatingWriter_SmallWrite(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "test.ndjson")

	rw, err := NewRotatingWriter(base, 1024, 3)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer rw.Close()

	data := []byte("hello world\n")
	if _, err := rw.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Main file should exist with the data.
	content, err := os.ReadFile(base)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(content) != "hello world\n" {
		t.Errorf("unexpected content: %q", content)
	}

	// No rotated files should exist.
	if _, err := os.Stat(filepath.Join(dir, "test.1.ndjson")); !os.IsNotExist(err) {
		t.Errorf("expected no rotated file, got err=%v", err)
	}
}

func TestRotatingWriter_RotatesOnMaxBytes(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "test.ndjson")

	rw, err := NewRotatingWriter(base, 50, 3)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer rw.Close()

	// Write enough to exceed maxBytes.
	data := []byte(strings.Repeat("A", 51))
	if _, err := rw.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Rotated file .1 should exist and contain the original data.
	r1, err := os.ReadFile(filepath.Join(dir, "test.1.ndjson"))
	if err != nil {
		t.Fatalf("ReadFile rotated.1: %v", err)
	}
	if string(r1) != string(data) {
		t.Errorf("rotated file content mismatch: got %d bytes, want %d", len(r1), len(data))
	}

	// Main file should exist and be empty (freshly opened).
	info, err := os.Stat(base)
	if err != nil {
		t.Fatalf("Stat base: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("expected fresh main file (0 bytes), got %d", info.Size())
	}
}

func TestRotatingWriter_DropsOldest(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "test.ndjson")

	// maxBytes=10, maxFiles=3: allows primary + 2 rotated files (indices 1, 2).
	rw, err := NewRotatingWriter(base, 10, 3)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer rw.Close()

	// Each write of 11 bytes exceeds maxBytes and triggers rotation.
	// Use distinct content so we can track which data survives.
	for i := 0; i < 5; i++ {
		data := []byte(strings.Repeat(string(rune('A'+i)), 11))
		if _, err := rw.Write(data); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	// After 5 rotations with maxFiles=3:
	// - test.2.ndjson holds the 4th write's data (DDDDD...)
	// - test.1.ndjson holds the 5th write's data (EEEEE...)
	// - test.ndjson is empty (fresh)
	// - The 1st-3rd writes' data should be completely gone.

	// Verify .1 and .2 exist.
	r1, err := os.ReadFile(filepath.Join(dir, "test.1.ndjson"))
	if err != nil {
		t.Fatalf("ReadFile .1: %v", err)
	}
	r2, err := os.ReadFile(filepath.Join(dir, "test.2.ndjson"))
	if err != nil {
		t.Fatalf("ReadFile .2: %v", err)
	}

	// The oldest data (first write: "AAAAAAAAAAA") should be gone from all files.
	oldest := strings.Repeat("A", 11)
	if strings.Contains(string(r1), oldest) {
		t.Errorf("oldest data leaked into .1 file")
	}
	if strings.Contains(string(r2), oldest) {
		t.Errorf("oldest data leaked into .2 file")
	}

	// File beyond the bound (.3) must not exist.
	if _, err := os.Stat(filepath.Join(dir, "test.3.ndjson")); !os.IsNotExist(err) {
		t.Errorf("expected test.3.ndjson to not exist, got err=%v", err)
	}
}

func TestRotatingWriter_ConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "test.ndjson")

	// Large maxBytes so no rotation happens during this test.
	rw, err := NewRotatingWriter(base, 10*1024*1024, 3)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer rw.Close()

	const numGoroutines = 8
	const writesPerGoroutine = 50
	const payload = "concurrent test data line\n"

	var wg sync.WaitGroup
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < writesPerGoroutine; i++ {
				if _, err := rw.Write([]byte(payload)); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Verify total bytes written matches expectations.
	totalExpected := int64(numGoroutines * writesPerGoroutine * len(payload))
	content, err := os.ReadFile(base)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if int64(len(content)) != totalExpected {
		t.Errorf("content size mismatch: got %d, want %d", len(content), totalExpected)
	}
}

func TestRotatingWriter_Close(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "test.ndjson")

	rw, err := NewRotatingWriter(base, 1024, 3)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}

	// Write something before close.
	if _, err := rw.Write([]byte("before close\n")); err != nil {
		t.Fatalf("Write before close: %v", err)
	}

	// Close should succeed.
	if err := rw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Double close should be a no-op.
	if err := rw.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}

	// Write after close should fail.
	if _, err := rw.Write([]byte("after close")); err == nil {
		t.Errorf("expected error writing to closed writer, got nil")
	}
}
