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
	if !strings.Contains(plain[0], "files") {
		t.Errorf("title row = %q, want 'files'", plain[0])
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

// The click cadence (panelClickAction): single defers focus with NO
// preview, double previews while focus never moves, triple inserts the
// @-mention the double click used to own.
func TestPanelClickCadence_File_PreviewDouble_MentionTriple(t *testing.T) {
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

	// click1: nothing previews; focus is deferred behind the timer.
	if cmd := m.panelClickAction(regionProject, idx); cmd == nil {
		t.Fatal("click1 must arm the focus timer")
	}
	if m.attached != nil {
		t.Error("single click must not preview")
	}
	if !m.panelClickPending.valid || m.panelFocus != regionNone {
		t.Fatalf("click1: pending=%v focus=%v, want armed + regionNone", m.panelClickPending.valid, m.panelFocus)
	}
	gen1 := m.panelClickGen

	// click2: the preview — and the deferred focus is abandoned.
	if cmd := m.panelClickAction(regionProject, idx); cmd == nil || m.attached == nil {
		t.Fatalf("double click should preview, got attached=%v", m.attached)
	}
	if m.attached.label != "file README.md" {
		t.Errorf("attach label = %q", m.attached.label)
	}
	if snap := m.attached.buf.Snapshot(); !strings.Contains(snap, "# hi") {
		t.Errorf("preview content = %q", snap)
	}
	if m.panelClickPending.valid {
		t.Error("double click must consume the deferred focus")
	}
	if m.panelFocus != regionNone {
		t.Errorf("double click must never move focus, got %v", m.panelFocus)
	}
	// The orphaned timer must not land focus later either.
	nm, _ := m.Update(panelClickFocusMsg{gen: gen1})
	if nm.(model).panelFocus != regionNone {
		t.Error("stale focus timer must be ignored after a double click")
	}

	// click3: the @-mention.
	m.panelClickAction(regionProject, idx)
	if !strings.Contains(m.ta.Value(), "@README.md") {
		t.Errorf("triple click should insert @-mention, textarea = %q", m.ta.Value())
	}
	if m.lastTreeClickPath != "" {
		t.Errorf("triple click should consume the arm, got %q", m.lastTreeClickPath)
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

// TestPanelClickCadence_SlowSecondClickIsFreshSingle: a second click after
// the 400ms window is a fresh single click — no preview, no @-mention, a
// newly armed (deferred) focus instead of the expired window's.
func TestPanelClickCadence_SlowSecondClickIsFreshSingle(t *testing.T) {
	m := projectClickModel(t)
	idx := -1
	for i, id := range m.projectIDs {
		if id == "README.md" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("README.md row not found in ids %v", m.projectIDs)
	}
	m.panelClickAction(regionProject, idx) // click1: armed
	m.panelClickPending.at = time.Now().Add(-time.Second)

	m.panelClickAction(regionProject, idx) // slow second click
	if m.attached != nil {
		t.Error("slow second click must not preview")
	}
	if strings.Contains(m.ta.Value(), "@README") {
		t.Error("slow second click must not insert an @-mention")
	}
	if !m.panelClickPending.valid || m.panelClickPending.line != idx {
		t.Errorf("slow second click should re-arm as a fresh single, pending=%+v", m.panelClickPending)
	}
}

// --- fileDiffBuffer: modified files preview as a diff ---

func TestFileDiffBuffer_CleanModifiedUntrackedNonRepo(t *testing.T) {
	dir := scratchRepo(t)

	// Clean committed file → no diff: caller falls back to contents.
	if buf, label := fileDiffBuffer(dir, "a.txt"); buf != nil || label != "" {
		t.Errorf("clean file: buf=%v label=%q, want nil/empty for the contents fallback", buf, label)
	}

	// Modified → complete diff vs HEAD (staged + unstaged), label says diff.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	buf, label := fileDiffBuffer(dir, "a.txt")
	if buf == nil || label != "diff a.txt" {
		t.Fatalf("modified file: buf=%v label=%q, want a diff buffer labelled %q", buf != nil, label, "diff a.txt")
	}
	if snap := buf.Snapshot(); !strings.Contains(snap, "+two") || !strings.Contains(snap, "a.txt") {
		t.Errorf("modified diff = %q, want the +two line and the path", snap)
	}

	// Untracked → whole-file added diff (git diff alone would show nothing).
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	buf, label = fileDiffBuffer(dir, "b.txt")
	if buf == nil || label != "diff b.txt" {
		t.Fatalf("untracked file: buf=%v label=%q", buf != nil, label)
	}
	if snap := buf.Snapshot(); !strings.Contains(snap, "/dev/null") || !strings.Contains(snap, "+fresh") {
		t.Errorf("untracked diff = %q, want the /dev/null added form", snap)
	}

	// Not a repo → nil: the caller keeps the raw content preview.
	if buf, _ := fileDiffBuffer(t.TempDir(), "a.txt"); buf != nil {
		t.Error("non-repo cwd should fall back to contents, got a diff buffer")
	}
}

func TestHandleProjectPanelClick_ModifiedFilePreviewsDiff(t *testing.T) {
	dir := scratchRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := dragTestModel()
	m.st.cwd = dir
	m.projectFiles = []string{"a.txt"}
	m.rebuildProjectTree()

	idx := -1
	for i, id := range m.projectIDs {
		if id == "a.txt" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("a.txt row not found in ids %v", m.projectIDs)
	}
	if cmd := m.handleProjectPanelClick(idx); cmd == nil || m.attached == nil {
		t.Fatalf("modified file click should attach a diff, got attached=%v", m.attached)
	}
	if m.attached.label != "diff a.txt" {
		t.Errorf("attach label = %q, want %q", m.attached.label, "diff a.txt")
	}
	if snap := m.attached.buf.Snapshot(); !strings.Contains(snap, "+two") {
		t.Errorf("preview = %q, want the diff content", snap)
	}
}
