package main

import (
	"strings"
	"testing"
)

func TestParseGitLog(t *testing.T) {
	out := "abc1234\x1f" + strings.Repeat("a", 40) + "\x1f" + strings.Repeat("b", 40) + " " + strings.Repeat("c", 40) + "\x1frefactor the dispatch layer\x1fHEAD -> main, origin/main\x1f2 hours ago\n" +
		"def5678\x1f" + strings.Repeat("d", 40) + "\x1f\x1finitial commit\x1f\x1f3 days ago\n"
	commits := parseGitLog(out)
	if len(commits) != 2 {
		t.Fatalf("got %d commits, want 2", len(commits))
	}
	c := commits[0]
	if c.Short != "abc1234" || c.Subject != "refactor the dispatch layer" || c.Age != "2 hours ago" {
		t.Errorf("bad commit fields: %+v", c)
	}
	if len(c.Parents) != 2 {
		t.Errorf("got %d parents, want 2", len(c.Parents))
	}
	if c.Decor != "HEAD -> main, origin/main" {
		t.Errorf("bad decor %q", c.Decor)
	}
	if len(commits[1].Parents) != 0 {
		t.Errorf("root commit should have no parents, got %v", commits[1].Parents)
	}
}

func prefixes(lines []gitGraphLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Prefix
	}
	return out
}

func TestLayoutGraph_Linear(t *testing.T) {
	commits := []gitCommit{
		{Hash: "c", Parents: []string{"b"}},
		{Hash: "b", Parents: []string{"a"}},
		{Hash: "a", Parents: nil},
	}
	got := prefixes(layoutGraph(commits))
	want := []string{"●", "●", "●"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("linear layout = %q, want %q", got, want)
	}
}

func TestLayoutGraph_Merge(t *testing.T) {
	// c merges a and b; both a and b lead to root r.
	commits := []gitCommit{
		{Hash: "c", Parents: []string{"a", "b"}},
		{Hash: "a", Parents: []string{"r"}},
		{Hash: "b", Parents: []string{"r"}},
		{Hash: "r", Parents: nil},
	}
	got := prefixes(layoutGraph(commits))
	want := []string{"●─╮", "● │", "│ ●", "●─╯"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("merge layout = %q, want %q", got, want)
	}
}

func TestLayoutGraph_BranchMergeBack(t *testing.T) {
	// m merges a and feature f; f branches off a.
	commits := []gitCommit{
		{Hash: "m", Parents: []string{"a", "f"}},
		{Hash: "f", Parents: []string{"a"}},
		{Hash: "a", Parents: []string{"r"}},
		{Hash: "r", Parents: nil},
	}
	got := prefixes(layoutGraph(commits))
	want := []string{"●─╮", "│ ●", "●─╯", "●"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("branch-merge layout = %q, want %q", got, want)
	}
}

func TestLayoutGraph_RowHashesSurvive(t *testing.T) {
	commits := []gitCommit{
		{Hash: "c", Parents: []string{"a", "b"}},
		{Hash: "a", Parents: nil},
		{Hash: "b", Parents: nil},
	}
	lines := layoutGraph(commits)
	for i, l := range lines {
		if l.Commit.Hash != commits[i].Hash {
			t.Errorf("row %d lost its commit: %+v", i, l)
		}
	}
}

func TestParseStatusBranch(t *testing.T) {
	for _, tc := range []struct {
		out    string
		branch string
		dirty  int
	}{
		{"## main...origin/main [ahead 1]\n M file.go\n?? new.go\n", "main", 2},
		{"## main\n", "main", 0},
		{"## HEAD (no branch)\n", "HEAD", 0},
		{"## No commits yet on master\n", "master", 0},
		{"", "HEAD", 0},
	} {
		branch, dirty := parseStatusBranch(tc.out)
		if branch != tc.branch || dirty != tc.dirty {
			t.Errorf("parseStatusBranch(%q) = %q,%d want %q,%d", tc.out, branch, dirty, tc.branch, tc.dirty)
		}
	}
}

func TestGitShow_RejectsBadHash(t *testing.T) {
	if _, err := gitShow(".", "abc123; rm -rf /"); err != errBadHash {
		t.Errorf("want errBadHash, got %v", err)
	}
}
