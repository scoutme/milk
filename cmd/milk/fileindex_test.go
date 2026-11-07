package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/testutil"
)

// gitRepo makes an isolated git work tree (no user or global git config).
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	testutil.SandboxHome(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	return dir
}

func writeFiles(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, r := range rels {
		p := filepath.Join(root, filepath.FromSlash(r))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ready waits for the first build, as a Tab press does.
func ready(t *testing.T, cwd string) *fileIndex {
	t.Helper()
	ix := indexFor(cwd)
	if ix.get(5*time.Second) == nil {
		t.Fatal("index never became ready")
	}
	return ix
}

// Issue #198: the target sat behind thousands of ignored entries that spent the
// whole walk budget; the git-backed list never visits them.
func TestAtPath_Issue198_TargetBehindIgnoredBallast(t *testing.T) {
	repo := gitRepo(t)
	writeFiles(t, repo, ".gitignore")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".mimocode/node_modules/\nwebsite/.venv/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4000; i++ {
		writeFiles(t, repo, fmt.Sprintf(".mimocode/node_modules/pkg%d/EventLogSessionAuth.js", i/10))
	}
	writeFiles(t, repo, "cmd/milk/main.go", "docs/acp-session-resume-plan.md", "docs/session-2026-10-01-primary-overload-analysis.md")
	ready(t, repo)

	got := atPathMatches("sess", repo, 6, 0)
	if len(got) == 0 || got[0] != "docs/acp-session-resume-plan.md" && got[0] != "docs/session-2026-10-01-primary-overload-analysis.md" {
		t.Fatalf("@sess = %v, want the docs files first", got)
	}
	for _, p := range got {
		if strings.Contains(p, "node_modules") {
			t.Errorf("ignored path suggested: %s", p)
		}
	}
	if !contains(got, "docs/acp-session-resume-plan.md") {
		t.Errorf("@sess missed docs/acp-session-resume-plan.md: %v", got)
	}
	if got := atPathMatches("docs/ses", repo, 6, 0); !contains(got, "docs/session-2026-10-01-primary-overload-analysis.md") {
		t.Errorf("@docs/ses = %v", got)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestAtPath_NestedGitignoreAndUntrackedFiles(t *testing.T) {
	repo := gitRepo(t)
	writeFiles(t, repo, "sub/keep.go", "sub/gen/out.go", "sub/new_untracked.go")
	if err := os.WriteFile(filepath.Join(repo, "sub", ".gitignore"), []byte("gen/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ready(t, repo)
	got := atPathMatches("sub/", repo, 20, 0)
	if !contains(got, "sub/keep.go") || !contains(got, "sub/new_untracked.go") {
		t.Errorf("untracked, not-ignored files must be offered: %v", got)
	}
	if contains(got, "sub/gen/") {
		t.Errorf("a nested .gitignore was not honoured: %v", got)
	}
	if got := atPathMatches("out", repo, 20, 0); len(got) != 0 {
		t.Errorf("file inside a nested-ignored dir found: %v", got)
	}
}

func TestAtPath_RankingTiers(t *testing.T) {
	repo := gitRepo(t)
	writeFiles(t, repo,
		"session.go",             // base-name prefix, top level
		"pkg/session_store.go",   // base-name prefix, deeper
		"pkg/mysession.go",       // base-name contains
		"sessions/readme.md",     // only the folder matches (path contains)
		"zz/deep/er/session.txt", // base-name prefix, much deeper
	)
	ready(t, repo)
	got := atPathMatches("sess", repo, 10, 0)
	// Base-name prefix first (direct children, then deeper, shorter first), then
	// base-name contains, then a match only in a folder name.
	want := []string{"sessions/", "session.go", "pkg/session_store.go", "zz/deep/er/session.txt", "pkg/mysession.go", "sessions/readme.md"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ranking = %v\nwant      %v", got, want)
	}
}

func TestAtPath_EmptyFragmentListsDirectChildrenAndOneCharStaysShallow(t *testing.T) {
	repo := gitRepo(t)
	writeFiles(t, repo, "a.txt", "b/inner/c.txt", "b/d.txt", "z.txt")
	ready(t, repo)

	got := atPathMatches("", repo, 20, 0)
	if strings.Join(got, ",") != "a.txt,b/,z.txt" {
		t.Errorf("bare @ = %v, want only the top-level entries", got)
	}
	got = atPathMatches("b/", repo, 20, 0)
	if strings.Join(got, ",") != "b/d.txt,b/inner/" {
		t.Errorf("@b/ = %v", got)
	}
	// One character: direct children only, never the whole tree.
	if got := atPathMatches("c", repo, 20, 0); contains(got, "b/inner/c.txt") {
		t.Errorf("a one-character fragment searched below the top level: %v", got)
	}
	if got := atPathMatches("c.", repo, 20, 0); !contains(got, "b/inner/c.txt") {
		t.Errorf("two characters should search the tree: %v", got)
	}
}

func TestAtPath_TrackedFileDeletedFromDiskIsNotOffered(t *testing.T) {
	repo := gitRepo(t)
	writeFiles(t, repo, "gone.go", "kept.go")
	if out, err := exec.Command("git", "-C", repo, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := os.Remove(filepath.Join(repo, "gone.go")); err != nil {
		t.Fatal(err)
	}
	ready(t, repo)
	got := atPathMatches("go", repo, 10, 0)
	if contains(got, "gone.go") || !contains(got, "kept.go") {
		t.Errorf("got %v", got)
	}
}

// Outside a git work tree the walk prunes ballast directories while descending.
func TestAtPath_NonGitFallbackPrunesBallast(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--git-dir").CombinedOutput(); err == nil {
		t.Skipf("temp dir is inside a git work tree: %s", out)
	}
	for i := 0; i < 300; i++ {
		writeFiles(t, dir, fmt.Sprintf("node_modules/p%d/session.js", i), fmt.Sprintf(".venv/lib/s%d/session.py", i))
	}
	writeFiles(t, dir, "src/session.go")
	ix := ready(t, dir)
	if ix.snap.Load().fromGit {
		t.Fatal("expected the walk fallback")
	}
	got := atPathMatches("sess", dir, 10, 0)
	if len(got) != 1 || got[0] != "src/session.go" {
		t.Errorf("got %v, want only src/session.go", got)
	}
}

func TestFileIndex_EntryCapBoundsMemory(t *testing.T) {
	old := fileIndexMaxEntries
	fileIndexMaxEntries = 50
	defer func() { fileIndexMaxEntries = old }()

	dir := t.TempDir()
	for i := 0; i < 200; i++ {
		writeFiles(t, dir, fmt.Sprintf("f%03d.txt", i))
	}
	ix := ready(t, dir)
	snap := ix.snap.Load()
	if len(snap.entries) > 50 || !snap.truncated {
		t.Errorf("entries=%d truncated=%v, want <=50 and truncated", len(snap.entries), snap.truncated)
	}
}

func TestFileIndex_RefreshesAfterInvalidateAndIsSingleFlight(t *testing.T) {
	repo := gitRepo(t)
	writeFiles(t, repo, "first.go")
	ix := ready(t, repo)
	before := ix.builds.Load()

	writeFiles(t, repo, "created_midsession.go")
	ix.invalidate()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ix.get(0) }()
	}
	wg.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for !contains(atPathMatches("midsession", repo, 5, 0), "created_midsession.go") {
		if time.Now().After(deadline) {
			t.Fatal("a file created mid-session never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := ix.builds.Load() - before; n != 1 {
		t.Errorf("50 concurrent uses started %d builds, want 1", n)
	}
	// A stale snapshot keeps serving while the refresh runs.
	if ix.snap.Load() == nil {
		t.Error("snapshot lost during refresh")
	}
}

func TestAtPath_OutsideCwdListsOneDirectoryAtATime(t *testing.T) {
	outside := t.TempDir()
	writeFiles(t, outside, "alpha.txt", "beta/inner.txt")
	cwd := t.TempDir()

	got := atPathMatches(outside+"/al", cwd, 10, 0)
	if len(got) != 1 || got[0] != filepath.ToSlash(filepath.Join(outside, "alpha.txt")) {
		t.Errorf("absolute path = %v", got)
	}
	rel, _ := filepath.Rel(cwd, outside)
	got = atPathMatches(rel+"/be", cwd, 10, 0)
	if len(got) != 1 || !strings.HasSuffix(got[0], "/beta/") {
		t.Errorf("relative path leaving cwd = %v", got)
	}
}

// Tab completion goes through the same index and may wait for the first build.
func TestBuildTabMatches_AtPathUsesTheIndex(t *testing.T) {
	repo := gitRepo(t)
	writeFiles(t, repo, "docs/acp-session-resume-plan.md", "main.go")
	res := buildTabMatches("look at @sess", repo, nil)
	if len(res.matches) != 1 || res.matches[0] != "@docs/acp-session-resume-plan.md" {
		t.Errorf("Tab matches = %v", res.matches)
	}
}

// The keystroke path is an in-memory filter: a worst-case full candidate list
// must answer in well under a frame.
func TestFileSnapshot_MatchIsCheapAtTheCap(t *testing.T) {
	files := make([]string, 0, fileIndexMaxEntriesDefault)
	for i := 0; len(files) < fileIndexMaxEntriesDefault*4/5; i++ {
		files = append(files, fmt.Sprintf("pkg%03d/sub%02d/file_%05d_session.go", i%400, i%25, i))
	}
	snap := newFileSnapshot(files, false, true)
	if len(snap.entries) > fileIndexMaxEntriesDefault+fileIndexMaxEntriesDefault/4 {
		t.Fatalf("unexpected snapshot size %d", len(snap.entries))
	}
	const runs = 100
	start := time.Now()
	for i := 0; i < runs; i++ {
		if got := snap.match("", "sess", 10); len(got) != 10 {
			t.Fatalf("got %d results", len(got))
		}
	}
	if per := time.Since(start) / runs; per > 25*time.Millisecond {
		t.Errorf("one keystroke filter over %d entries took %v (budget: far below a 120ms debounce)", len(snap.entries), per)
	} else {
		t.Logf("%d entries: %v per query", len(snap.entries), per)
	}
}
