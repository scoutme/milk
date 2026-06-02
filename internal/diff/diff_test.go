package diff

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForEdit_Basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	os.WriteFile(path, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644)

	got := ForEdit(path, "line2\nline3", "lineA\nlineB\nlineC", 1)
	if got == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(got, "-line2") {
		t.Error("expected deleted line2")
	}
	if !strings.Contains(got, "+lineA") {
		t.Error("expected added lineA")
	}
	// context lines
	if !strings.Contains(got, " line1") {
		t.Error("expected context line1")
	}
	if !strings.Contains(got, " line4") {
		t.Error("expected context line4")
	}
}

func TestForEdit_NoChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	os.WriteFile(path, []byte("abc\n"), 0o644)

	got := ForEdit(path, "abc", "abc", 3)
	if got != "" {
		t.Errorf("expected empty diff for no-op, got %q", got)
	}
}

func TestForEdit_MissingFile(t *testing.T) {
	got := ForEdit("/nonexistent/path.go", "old", "new", 3)
	if got != "" {
		t.Errorf("expected empty diff for missing file, got %q", got)
	}
}

func TestForWrite_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	os.WriteFile(path, []byte("old content\n"), 0o644)

	got := ForWrite(path, "new content\n", 3)
	if !strings.Contains(got, "-old content") {
		t.Error("expected deleted old content")
	}
	if !strings.Contains(got, "+new content") {
		t.Error("expected added new content")
	}
}

func TestForWrite_NewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.go")
	got := ForWrite(path, "hello\nworld\n", 3)
	if !strings.Contains(got, "(new file)") {
		t.Error("expected new file marker")
	}
	if !strings.Contains(got, "+hello") {
		t.Error("expected added lines")
	}
}

func TestForEdit_ContextLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	// 10 lines, edit line 5
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, "line"+string(rune('0'+i)))
	}
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)

	got := ForEdit(path, "line5", "lineX", 3)
	// Should contain lines 2-4 as context before and 6-8 after
	if !strings.Contains(got, " line2") || !strings.Contains(got, " line4") {
		t.Error("expected 3 lines of context before")
	}
	if !strings.Contains(got, " line6") || !strings.Contains(got, " line8") {
		t.Error("expected 3 lines of context after")
	}
	// Should NOT contain lines far from the edit
	if strings.Contains(got, " line1") {
		t.Error("should not include line1 (too far from edit)")
	}
	if strings.Contains(got, " line9") {
		t.Error("should not include line9 (too far from edit)")
	}
}
