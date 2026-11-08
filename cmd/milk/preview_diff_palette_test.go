package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The transcript's diff palette (internal/diff — see diffsurface_test.go for
// the same precedent): these are the exact sequences tool-result edit previews
// render with, and file/working-tree previews must match them.
const (
	transcriptDiffGreen = "\x1b[38;5;119;48;5;22m" // additions: bright green on 256-colour 22
	transcriptDiffRed   = "\x1b[38;5;203;48;5;52m" // deletions: bright red on 256-colour 52
	transcriptDim       = "\x1b[2m"
)

// fullContextRepo commits a 40-line file, then dirties only its first line —
// the fixture that separates full-context previews (whole file shown) from
// default-context ones (only the hunk around line 1).
func fullContextRepo(t *testing.T) string {
	t.Helper()
	dir := scratchRepo(t)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	var committed strings.Builder
	committed.WriteString("line one ORIGINAL\n")
	for i := 2; i < 40; i++ {
		fmt.Fprintf(&committed, "line %d\n", i)
	}
	committed.WriteString("line forty\n")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(committed.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-m", "grow")
	// Dirty line 1 only: -U3 would show just lines ~1-4, full context shows 40.
	dirtied := strings.Replace(committed.String(), "line one ORIGINAL", "line one CHANGED", 1)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(dirtied), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A modified file's preview must carry the WHOLE file (every unchanged line
// present, not only the hunks around the change) rendered in the transcript's
// diff palette — green additions, red deletions, dim context.
func TestFileDiffBuffer_FullFileAndTranscriptColors(t *testing.T) {
	dir := fullContextRepo(t)

	buf, label := fileDiffBuffer(dir, "a.txt")
	if buf == nil || label != "diff a.txt" {
		t.Fatalf("buf=%v label=%q, want a diff buffer labelled %q", buf != nil, label, "diff a.txt")
	}
	snap := buf.Snapshot()

	if !strings.Contains(snap, "line forty") {
		t.Errorf("full context missing: the file's last line must be in the preview (only hunks?):\n%s", snap)
	}
	if !strings.Contains(snap, transcriptDiffGreen+"+line one CHANGED") {
		t.Errorf("addition not in the transcript palette (want %q prefix):\n%s", transcriptDiffGreen, snap)
	}
	if !strings.Contains(snap, transcriptDiffRed+"-line one ORIGINAL") {
		t.Errorf("deletion not in the transcript palette (want %q prefix):\n%s", transcriptDiffRed, snap)
	}
	if !strings.Contains(snap, transcriptDim+" line forty\x1b[0m") {
		t.Errorf("context lines must be dim like renderHunk's:\n%s", snap)
	}
}

// The working-tree preview stays the project summary: status list plain,
// patch in the same palette, DEFAULT context (unlike single-file previews,
// it does not expand every file to full length).
func TestWorkingTreePreview_StatusPlainPatchColoredDefaultContext(t *testing.T) {
	dir := fullContextRepo(t)

	out, err := workingTreePreview(dir)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out, " M a.txt") {
		t.Errorf("status line missing:\n%s", out)
	}
	if strings.Contains(out, transcriptDim+" M a.txt") {
		t.Errorf("status list must stay plain (unstyled), got:\n%s", out)
	}
	if !strings.Contains(out, transcriptDiffGreen+"+line one CHANGED") {
		t.Errorf("patch not in the transcript palette:\n%s", out)
	}
	if strings.Contains(out, "line forty") {
		t.Errorf("working-tree preview must keep default context — the file's tail leaked in:\n%s", out)
	}
}
