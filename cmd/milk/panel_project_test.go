package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/session"
)

func treePlain(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = ansi.Strip(l)
	}
	return out
}

// --- buildProjectTreeLines ---

func TestBuildProjectTreeLines_Structure(t *testing.T) {
	files := []string{"cmd/milk/main.go", "cmd/milk/repl.go", "README.md", "go.mod"}
	lines, ids := buildProjectTreeLines(files, false, nil, projectPanelInner, nil)
	plain := treePlain(lines)

	want := []string{
		"", // title (checked separately)
		"", // blank
		"▾ cmd/",
		"  ▸ milk/",
		"  go.mod",
		"  README.md",
	}
	// The title line is padded/styled; check the rows below it.
	body := plain[2:]
	if len(body) != 4 {
		t.Fatalf("got %d body rows %q, want 4", len(body), body)
	}
	for i := range want[2:] {
		if !strings.Contains(body[i], strings.TrimSpace(want[2:][i])) {
			t.Errorf("row %d = %q, want contains %q", i, body[i], want[2:][i])
		}
	}
	if !strings.Contains(plain[0], "project") {
		t.Errorf("title row = %q, want 'project'", plain[0])
	}

	wantIDs := []string{"", "", "d:cmd/", "d:cmd/milk/", "go.mod", "README.md"}
	for i := range wantIDs {
		if ids[i] != wantIDs[i] {
			t.Errorf("id[%d] = %q, want %q", i, ids[i], wantIDs[i])
		}
	}
}

func TestBuildProjectTreeLines_ExpandOverride(t *testing.T) {
	files := []string{"cmd/milk/main.go", "README.md"}
	// cmd/ collapsed explicitly — its children vanish.
	lines, ids := buildProjectTreeLines(files, false, map[string]bool{"cmd/": false}, projectPanelInner, nil)
	joined := strings.Join(treePlain(lines), "\n")
	if strings.Contains(joined, "milk/") {
		t.Errorf("collapsed dir should hide children; got:\n%s", joined)
	}
	_ = ids

	// Deeper dir expanded explicitly — its children appear even though
	// depth-1 dirs start collapsed.
	lines, _ = buildProjectTreeLines(files, false, map[string]bool{"cmd/milk/": true}, projectPanelInner, nil)
	joined = strings.Join(treePlain(lines), "\n")
	if !strings.Contains(joined, "main.go") {
		t.Errorf("expanded dir should show children; got:\n%s", joined)
	}
}

func TestBuildProjectTreeLines_LoadingEmptyTruncated(t *testing.T) {
	joined := strings.Join(treePlain(buildProjectTreeLinesFirst(nil, false)), "\n")
	if !strings.Contains(joined, "loading") {
		t.Errorf("nil files should show (loading…); got:\n%s", joined)
	}
	joined = strings.Join(treePlain(buildProjectTreeLinesFirst([]string{}, false)), "\n")
	if !strings.Contains(joined, "empty") {
		t.Errorf("no files should show (empty); got:\n%s", joined)
	}
	joined = strings.Join(treePlain(buildProjectTreeLinesFirst([]string{"a.go"}, true)), "\n")
	if !strings.Contains(joined, "truncated") {
		t.Errorf("truncated snapshot should say so; got:\n%s", joined)
	}
}

func buildProjectTreeLinesFirst(files []string, truncated bool) []string {
	lines, _ := buildProjectTreeLines(files, truncated, nil, projectPanelInner, nil)
	return lines
}

// --- handleProjectPanelClick ---

func projectClickModel(t *testing.T) *model {
	t.Helper()
	m := dragTestModel()
	m.st.cwd = t.TempDir()
	m.st.sess = &session.Session{ID: "test-session"}
	m.projectFiles = []string{"cmd/a.go", "README.md"}
	m.rebuildProjectTree()
	return m
}

func TestHandleProjectPanelClick_DirToggle(t *testing.T) {
	m := projectClickModel(t)
	// Row 2 is the "▾ cmd/" dir.
	m.handleProjectPanelClick(2)
	if m.projectExpanded["cmd/"] != false {
		t.Errorf("click should collapse the open dir, got %v", m.projectExpanded["cmd/"])
	}
	// After collapse the dir row is still row 2; clicking re-expands.
	m.handleProjectPanelClick(2)
	if m.projectExpanded["cmd/"] != true {
		t.Errorf("second click should re-expand, got %v", m.projectExpanded["cmd/"])
	}
}

func TestHandleProjectPanelClick_FilePreviewThenInsert(t *testing.T) {
	m := projectClickModel(t)
	if err := os.WriteFile(filepath.Join(m.st.cwd, "README.md"), []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Rows: "", "", "▾ cmd/", "  ▸ a.go"?? no — cmd is expanded (depth 0),
	// so row 3 is the nested file and row 4 is README.md.
	idx := -1
	for i, id := range m.projectIDs {
		if id == "README.md" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("README.md row not found in ids %v", m.projectIDs)
	}

	cmd := m.handleProjectPanelClick(idx)
	if cmd == nil || m.attached == nil {
		t.Fatalf("single click should preview the file (attach), got attached=%v", m.attached)
	}
	if m.attached.label != "file README.md" {
		t.Errorf("attach label = %q", m.attached.label)
	}
	if snap := m.attached.buf.Snapshot(); !strings.Contains(snap, "# hi") {
		t.Errorf("preview content = %q", snap)
	}

	// Double-click within 400ms inserts the @-mention.
	m.handleProjectPanelClick(idx)
	if !strings.Contains(m.ta.Value(), "@README.md") {
		t.Errorf("double click should insert @-mention, textarea = %q", m.ta.Value())
	}
	// And the third click previews again (the arm state was consumed).
	m.handleProjectPanelClick(idx)
	if m.lastTreeClickPath != "README.md" {
		t.Errorf("third click should re-arm preview, got %q", m.lastTreeClickPath)
	}
}

func TestFilePreviewBuffer(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	buf, label := filePreviewBuffer(dir, "a.txt")
	if buf == nil || label != "file a.txt" {
		t.Fatalf("preview = %v %q", buf, label)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{0x00, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}
	if buf, _ := filePreviewBuffer(dir, "bin.dat"); buf != nil {
		t.Error("binary content should be refused")
	}
	if buf, _ := filePreviewBuffer(dir, "missing.txt"); buf != nil {
		t.Error("missing file should be refused")
	}
}

// TestProjectPanelStaleArmExpires: the double-click window is 400ms — a slow
// second click is a fresh single click (preview), not an insert.
func TestProjectPanelStaleArmExpires(t *testing.T) {
	m := projectClickModel(t)
	idx := -1
	for i, id := range m.projectIDs {
		if id == "README.md" {
			idx = i
		}
	}
	m.handleProjectPanelClick(idx)
	m.lastTreeClickTime = time.Now().Add(-time.Second)
	m.handleProjectPanelClick(idx)
	if strings.Contains(m.ta.Value(), "@README.md") {
		t.Error("stale arm must not insert an @-mention")
	}
}
