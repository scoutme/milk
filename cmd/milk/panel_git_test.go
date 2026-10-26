package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// --- buildGitPanelLines ---

func gitRowsFixture() []gitGraphLine {
	return []gitGraphLine{
		{Prefix: "●─╮", Commit: gitCommit{Short: "abc1234", Hash: "abc1234" + strings.Repeat("0", 33), Subject: "merge the feature", Decor: "HEAD -> main"}},
		{Prefix: "● │", Commit: gitCommit{Short: "def5678", Hash: "def5678" + strings.Repeat("0", 33), Subject: "add the feature"}},
		{Prefix: "●", Commit: gitCommit{Short: "aaa1111", Hash: "aaa1111" + strings.Repeat("0", 33), Subject: "initial commit"}},
	}
}

func TestBuildGitPanelLines_HeaderAndRows(t *testing.T) {
	lines, ids := buildGitPanelLines(gitRowsFixture(), "main", 2, "", gitPanelInner)
	plain := treePlain(lines)
	joined := strings.Join(plain, "\n")

	if !strings.Contains(joined, "main") || !strings.Contains(joined, "✎2") {
		t.Errorf("header should show branch and dirty count; got:\n%s", joined)
	}
	if !strings.Contains(plain[0], "git") {
		t.Errorf("title row = %q", plain[0])
	}
	// Row indexes: 0 title, 1 blank, 2 header, 3 blank, 4.. commits.
	if len(lines) != 7 {
		t.Fatalf("got %d lines, want 7: %q", len(lines), plain)
	}
	for i, row := range lines[4:] {
		if !strings.Contains(plain[4+i], gitRowsFixture()[i].Commit.Short) {
			t.Errorf("row %d = %q, want short hash %s", i, plain[4+i], gitRowsFixture()[i].Commit.Short)
		}
		_ = row
	}
	if ids[4] != gitRowsFixture()[0].Commit.Hash || ids[6] != gitRowsFixture()[2].Commit.Hash {
		t.Errorf("ids misaligned: %v", ids)
	}
}

func TestBuildGitPanelLines_CleanAndErrorStates(t *testing.T) {
	_, _ = buildGitPanelLines(nil, "main", 0, "", gitPanelInner)
	lines, _ := buildGitPanelLines(nil, "main", 0, "not a git repository", gitPanelInner)
	joined := strings.Join(treePlain(lines), "\n")
	if !strings.Contains(joined, "not a git repository") {
		t.Errorf("error state not shown; got:\n%s", joined)
	}
	lines, _ = buildGitPanelLines(nil, "main", 0, "", gitPanelInner)
	joined = strings.Join(treePlain(lines), "\n")
	if !strings.Contains(joined, "loading") {
		t.Errorf("nil rows should show (loading…); got:\n%s", joined)
	}
	lines, _ = buildGitPanelLines([]gitGraphLine{}, "main", 0, "", gitPanelInner)
	joined = strings.Join(treePlain(lines), "\n")
	if !strings.Contains(joined, "no commits") {
		t.Errorf("empty rows should say so; got:\n%s", joined)
	}
	lines, _ = buildGitPanelLines(gitRowsFixture(), "main", 0, "", gitPanelInner)
	joined = strings.Join(treePlain(lines), "\n")
	if strings.Contains(joined, "✎") {
		t.Errorf("clean tree must not show a dirty marker; got:\n%s", joined)
	}
}

func TestGitRowLine_LongSubjectTruncated(t *testing.T) {
	r := gitGraphLine{Prefix: "●", Commit: gitCommit{Short: "abc1234", Subject: strings.Repeat("x", 100)}}
	line := ansi.Strip(gitRowLine(r, gitPanelInner))
	if w := len([]rune(line)); w > gitPanelInner {
		t.Errorf("row overflows inner: %d > %d (%q)", w, gitPanelInner, line)
	}
	if !strings.HasSuffix(line, "…") {
		t.Errorf("long subject should end truncated, got %q", line)
	}
}

// --- git data fetch (integration against a scratch repo) ---

func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func scratchRepo(t *testing.T) string {
	t.Helper()
	gitAvailable(t)
	dir := t.TempDir()
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
	run("init")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-m", "first commit")
	return dir
}

func TestGitLog_ScratchRepo(t *testing.T) {
	dir := scratchRepo(t)
	commits, branch, dirty, err := gitLog(dir)
	if err != nil {
		t.Fatalf("gitLog: %v", err)
	}
	if len(commits) != 1 || commits[0].Subject != "first commit" {
		t.Errorf("commits = %+v", commits)
	}
	if branch == "" {
		t.Error("branch should be set")
	}
	if dirty != 0 {
		t.Errorf("dirty = %d, want 0", dirty)
	}
	// The layout must produce exactly one clickable row with the hash.
	rows := layoutGraph(commits)
	if len(rows) != 1 || rows[0].Commit.Hash != commits[0].Hash {
		t.Errorf("layout rows = %+v", rows)
	}
}

func TestGitLog_NotARepo(t *testing.T) {
	gitAvailable(t)
	_, _, _, err := gitLog(t.TempDir())
	if err == nil {
		t.Fatal("expected an error outside a repo")
	}
	if got := gitErrText(err); got != "not a git repository" {
		t.Errorf("gitErrText = %q", got)
	}
}

func TestGitShow_ScratchRepo(t *testing.T) {
	dir := scratchRepo(t)
	commits, _, _, err := gitLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := gitShow(dir, commits[0].Hash)
	if err != nil {
		t.Fatalf("gitShow: %v", err)
	}
	if !strings.Contains(out, "first commit") || !strings.Contains(out, "a.txt") {
		t.Errorf("gitShow output missing summary/diff:\n%s", out)
	}
}

// --- region mapping ---

func TestRegionAt_ProjectAndGit(t *testing.T) {
	m := dragTestModel()
	m.width = 240
	m.panelProject = true
	m.panelGit = true
	m.projectLines, m.projectIDs = buildProjectTreeLines([]string{"a.go"}, false, nil, projectPanelInner, nil)
	m.gitLines, m.gitIDs = buildGitPanelLines(gitRowsFixture(), "main", 0, "", gitPanelInner)

	main := m.mainWidth()
	if r, _ := m.regionAt(main + 1); r != regionProject {
		t.Errorf("first project column maps to %v, want regionProject", r)
	}
	if r, _ := m.regionAt(main + projectPanelWidth + 1); r != regionGit {
		t.Errorf("first git column maps to %v, want regionGit", r)
	}
	if r, _ := m.regionAt(main - 1); r != regionNone {
		t.Errorf("main area maps to %v, want regionNone", r)
	}
}
