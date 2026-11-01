package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

// --- transient vs definitive git failures ---
//
// The git panel used to render *every* git failure as "not a
// repository" — including the 3s timeout under load or mid-checkout (a
// killed process leaves an ExitError with empty stderr), which made the
// panel flash "not a git repository" at apparently random moments. The
// classification below is the line between "render/keep as fact" and
// "keep the last good snapshot, retry next poll".

func TestGitErrDefinitive_Classification(t *testing.T) {
	missingGit := &exec.Error{Name: "git", Err: exec.ErrNotFound}
	killed := &exec.ExitError{} // timeout kill: process gone, no stderr
	notRepo := &exec.ExitError{Stderr: []byte("fatal: not a git repository (or any of the parent directories): .git\n")}
	badObject := &exec.ExitError{Stderr: []byte("fatal: bad object HEAD\n")}

	for _, tc := range []struct {
		name       string
		err        error
		definitive bool
	}{
		{"nil", nil, false},
		{"git binary missing", missingGit, true},
		{"not a repo", notRepo, true},
		{"timeout kill", killed, false},
		{"other git fatal", badObject, false},
		{"context deadline", context.DeadlineExceeded, false},
	} {
		if got := gitErrDefinitive(tc.err); got != tc.definitive {
			t.Errorf("%s: gitErrDefinitive = %v, want %v", tc.name, got, tc.definitive)
		}
		wantTransient := tc.err != nil && !tc.definitive
		if got := transientGitErr(tc.err); got != wantTransient {
			t.Errorf("%s: transientGitErr = %v, want %v", tc.name, got, wantTransient)
		}
	}
}

func TestRefreshGitCmd_NotARepoDefinitive(t *testing.T) {
	gitAvailable(t)
	msg := refreshGitCmd(t.TempDir())().(gitDataMsg)
	if msg.transient {
		t.Error("a non-repo is definitive — must not classify as transient")
	}
	if msg.err != "not a git repository" {
		t.Errorf("err = %q, want %q", msg.err, "not a git repository")
	}
}

// TestRefreshGitCmd_EmptyRepo: right after `git init` (no commits yet)
// git log refuses with a fatal — that used to surface as "not a git
// repository". The repo is real: err must stay empty and rows must be an
// empty, non-nil set so the panel renders "(no commits)", not "(loading…)".
func TestRefreshGitCmd_EmptyRepo(t *testing.T) {
	gitAvailable(t)
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	msg := refreshGitCmd(dir)().(gitDataMsg)
	if msg.err != "" || msg.transient {
		t.Errorf("empty repo: err = %q transient = %v, want neither", msg.err, msg.transient)
	}
	if msg.rows == nil || len(msg.rows) != 0 {
		t.Errorf("rows = %v, want non-nil empty", msg.rows)
	}
	// The "## No commits yet on <branch>" header must parse to the real
	// branch, not to its first word "No".
	out, err := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		t.Fatalf("symbolic-ref: %v", err)
	}
	if want := strings.TrimSpace(string(out)); msg.branch != want {
		t.Errorf("branch = %q, want %q", msg.branch, want)
	}
}

// TestGitDataMsg_TransientKeepsLastSnapshot: a transient probe failure is
// never rendered — the panel keeps the graph it already had, and the
// loading row before the first success.
func TestGitDataMsg_TransientKeepsLastSnapshot(t *testing.T) {
	m := newTestModelForPanels(t)

	updated, _ := m.Update(gitDataMsg{transient: true, err: "not a git repository"})
	m2 := updated.(model)
	if m2.gitLines != nil {
		t.Fatalf("transient first load wrote lines: %q", m2.gitLines)
	}

	updated, _ = m2.Update(gitDataMsg{rows: gitRowsFixture(), branch: "main", dirty: 1})
	m3 := updated.(model)
	wantLines, wantErr := m3.gitLines, m3.gitErr

	updated, _ = m3.Update(gitDataMsg{transient: true, err: "not a git repository"})
	m4 := updated.(model)
	if !slices.Equal(m4.gitLines, wantLines) {
		t.Errorf("transient msg replaced lines:\n%q\nwant\n%q", m4.gitLines, wantLines)
	}
	if m4.gitErr != wantErr || m4.gitDirty != 1 || m4.gitBranch != "main" {
		t.Errorf("transient msg mutated the snapshot: err=%q dirty=%d branch=%q",
			m4.gitErr, m4.gitDirty, m4.gitBranch)
	}
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
	// The header must carry the real branch name. A plain "branch != \"\""
	// pin once let a broken status probe slip through rendering "HEAD"
	// instead of the branch (the --no-optional-locks placement bug).
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if want := strings.TrimSpace(string(out)); branch != want {
		t.Errorf("branch = %q, want %q", branch, want)
	}
	if dirty != 0 {
		t.Errorf("dirty = %d, want 0", dirty)
	}
	// Dirty count comes from the same probe as the branch.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, dirty, err := gitLog(dir); err != nil || dirty != 1 {
		t.Errorf("after modify: dirty = %d err = %v, want 1, nil", dirty, err)
	}
	// The layout must produce exactly one clickable row with the hash.
	rows := layoutGraph(commits)
	if len(rows) != 1 || rows[0].Commit.Hash != commits[0].Hash {
		t.Errorf("layout rows = %+v", rows)
	}
}

// TestGitLog_StatusProbeFailureIsTransient: when the status probe fails but
// the log probe worked, gitLog must surface the failure as a transient error
// — never swallow it into a fabricated "HEAD"/0 header (that is exactly how
// the git panel came to say "⎇ HEAD" instead of the branch).
func TestGitLog_StatusProbeFailureIsTransient(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake git binary is a POSIX shell script")
	}
	bin := t.TempDir()
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$a\" = status ]; then\n" +
		"    echo 'fatal: unable to write index.lock' >&2\n" +
		"    exit 1\n" +
		"  fi\n" +
		"done\n" +
		"printf 'abc1234\\037abc1234\\037\\037subj\\037\\0372 hours ago\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	commits, branch, dirty, err := gitLog(t.TempDir())
	if err == nil || !transientGitErr(err) {
		t.Fatalf("err = %v, want transient (the repo is real; only status failed)", err)
	}
	if commits != nil || branch != "" || dirty != 0 {
		t.Errorf("commits=%v branch=%q dirty=%d, want no fabricated data on failure", commits, branch, dirty)
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
