package diff

import (
	"strings"
	"testing"
)

// ColorizeUnified must speak exactly the palette the transcript's edit
// previews use (renderHunk): additions green, deletions red, context dim
// with its leading space, headers/metadata dim — so a git-produced patch and
// a computed one read as one family.
func TestColorizeUnified_UsesTranscriptPalette(t *testing.T) {
	in := "diff --git a/x b/x\n" +
		"index 1..2 100644\n" +
		"--- a/x\n" +
		"+++ b/x\n" +
		"@@ -1,3 +1,3 @@\n" +
		" ctx\n" +
		"-old\n" +
		"+new\n" +
		"\\ No newline at end of file\n"
	got := ColorizeUnified(in)

	checks := []struct{ name, want string }{
		{"git header dim", ansiDim + "diff --git a/x b/x" + ansiReset},
		{"index line dim", ansiDim + "index 1..2 100644" + ansiReset},
		{"--- header dim, not red", ansiDim + "--- a/x" + ansiReset},
		{"+++ header dim, not green", ansiDim + "+++ b/x" + ansiReset},
		{"hunk header dim", ansiDim + "@@ -1,3 +1,3 @@" + ansiReset},
		{"context dim with its leading space", ansiDim + " ctx" + ansiReset},
		{"deletion red", ansiRed + "-old" + ansiReset},
		{"addition green", ansiGreen + "+new" + ansiReset},
		{"no-newline marker dim", ansiDim + "\\ No newline at end of file" + ansiReset},
	}
	for _, c := range checks {
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want it to contain %q", c.name, got, c.want)
		}
	}

	// Byte parity with the transcript's computed hunks: the same rows,
	// produced by renderHunk, must be identical to the colorized git rows.
	var h strings.Builder
	renderHunk(&h, hunk{lines: []line{{text: "ctx", kind: 0}, {text: "old", kind: -1}, {text: "new", kind: 1}}})
	for _, row := range strings.Split(strings.TrimRight(h.String(), "\n"), "\n") {
		if !strings.Contains(got, row) {
			t.Errorf("row parity: colorized output lacks renderHunk's %q", row)
		}
	}
}

// Stat blocks are metadata: space-prefixed lines (and the "..." separator)
// go dim, never green/red.
func TestColorizeUnified_StatBlockDim(t *testing.T) {
	in := "diff --git a/x b/x\n x.txt | 2 +-\n 1 file changed, 1 insertion(+), 1 deletion(-)\n"
	got := ColorizeUnified(in)
	if !strings.Contains(got, ansiDim+" x.txt | 2 +-"+ansiReset) {
		t.Errorf("stat line should be dim, got %q", got)
	}
	if !strings.Contains(got, ansiDim+" 1 file changed, 1 insertion(+), 1 deletion(-)"+ansiReset) {
		t.Errorf("summary line should be dim, got %q", got)
	}
}

// Text without a diff marker is returned untouched (defensive: the parser
// assumes diff shape), as is an already-colored line (a user's
// color.diff=always) and empty input.
func TestColorizeUnified_Passthrough(t *testing.T) {
	prose := "hello\n world\n-unrelated"
	if got := ColorizeUnified(prose); got != prose {
		t.Errorf("non-diff text must pass through, got %q", got)
	}
	colored := "diff --git a/x b/x\n\x1b[32m+already\x1b[m\n"
	if got := ColorizeUnified(colored); !strings.Contains(got, "\x1b[32m+already\x1b[m") {
		t.Errorf("already-escaped line must pass through, got %q", got)
	}
	if got := ColorizeUnified(""); got != "" {
		t.Errorf("empty input = empty output, got %q", got)
	}
}
