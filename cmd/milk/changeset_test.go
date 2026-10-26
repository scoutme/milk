package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/session"
)

// --- porcelain parsing (issue #331) ---

func TestParseStatusFiles(t *testing.T) {
	const root = "/repo"
	tests := []struct {
		name string
		cwd  string
		out  string
		want map[string]session.ChangeKind
	}{
		{
			name: "untracked and modified",
			cwd:  root,
			out:  "?? new.txt\n M tracked.txt\nM  staged.txt\n",
			want: map[string]session.ChangeKind{
				"new.txt":     session.ChangeCreated,
				"tracked.txt": session.ChangeModified,
				"staged.txt":  session.ChangeModified,
			},
		},
		{
			name: "added and deleted on both sides",
			cwd:  root,
			out:  "A  added.txt\n D wtgone.txt\nD  igone.txt\n",
			want: map[string]session.ChangeKind{
				"added.txt":  session.ChangeCreated,
				"wtgone.txt": session.ChangeDeleted,
				"igone.txt":  session.ChangeDeleted,
			},
		},
		{
			name: "staged add then worktree delete nets to deleted",
			cwd:  root,
			out:  "AD nearly.txt\nDA back.txt\n",
			want: map[string]session.ChangeKind{
				"nearly.txt": session.ChangeDeleted,
				"back.txt":   session.ChangeCreated,
			},
		},
		{
			name: "rename records both sides",
			cwd:  root,
			out:  "R  old.txt -> new.txt\n",
			want: map[string]session.ChangeKind{
				"old.txt": session.ChangeDeleted,
				"new.txt": session.ChangeRenamed,
			},
		},
		{
			name: "quoted rename",
			cwd:  root,
			out:  "R  \"old name.txt\" -> \"new name.txt\"\n",
			want: map[string]session.ChangeKind{
				"old name.txt": session.ChangeDeleted,
				"new name.txt": session.ChangeRenamed,
			},
		},
		{
			name: "subdir rebases onto cwd and drops outside paths",
			cwd:  "/repo/sub",
			out:  " M sub/inside.txt\n M other.txt\n?? sub/fresh.txt\n",
			want: map[string]session.ChangeKind{
				"inside.txt": session.ChangeModified,
				"fresh.txt":  session.ChangeCreated,
			},
		},
		{
			name: "rename whose sides sit outside cwd drops both",
			cwd:  "/repo/sub",
			out:  "R  a.txt -> b.txt\n",
			want: map[string]session.ChangeKind{},
		},
		{
			name: "branch header lines and junk are skipped",
			cwd:  root,
			out:  "# Not a repo? weird\n \n?? ok.txt\n",
			want: map[string]session.ChangeKind{"ok.txt": session.ChangeCreated},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseStatusFiles(tt.out, root, tt.cwd)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for p, k := range tt.want {
				if got[p] != k {
					t.Errorf("%s = %v, want %v", p, got[p], k)
				}
			}
		})
	}
}

func TestParseStatusFiles_CapsLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < changesetMaxStatusLines+500; i++ {
		b.WriteString(" M f\n")
	}
	// Unique paths so the cap (not map dedupe) is what bounds the size.
	b.Reset()
	for i := 0; i < changesetMaxStatusLines+500; i++ {
		b.WriteString(" M f" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26)) + "\n")
	}
	got := parseStatusFiles(b.String(), "/repo", "/repo")
	if len(got) > changesetMaxStatusLines {
		t.Fatalf("parsed %d entries, cap is %d", len(got), changesetMaxStatusLines)
	}
}

// --- merge + relativize ---

func TestMergeChangeset_GitWinsTraceOnlyWithoutGit(t *testing.T) {
	trace := map[string]session.ChangeKind{"a.txt": session.ChangeCreated}
	gitClean := map[string]session.ChangeKind{}

	// With a repo snapshot (even a clean one), porcelain is the truth and
	// stale traced entries drop out (issue #331's staleness rule).
	if got := mergeChangeset(trace, gitClean, true); len(got) != 0 {
		t.Fatalf("git snapshot must win over the trace, got %v", got)
	}
	// Without a repo, the trace is the only source.
	if got := mergeChangeset(trace, nil, false); got["a.txt"] != session.ChangeCreated {
		t.Fatalf("trace must survive without git, got %v", got)
	}
}

func TestChangesetRelativize(t *testing.T) {
	trace := map[string]session.ChangeKind{
		"/w/in.go":      session.ChangeModified,
		"/other/out.go": session.ChangeCreated,
		"/w/sub/x.go":   session.ChangeModified,
	}
	got := changesetRelativize(trace, "/w")
	if len(got) != 2 || got["in.go"] != session.ChangeModified || got["sub/x.go"] != session.ChangeModified {
		t.Fatalf("got %v, want in.go and sub/x.go", got)
	}
	if _, ok := got["../other/out.go"]; ok {
		t.Fatal("outside path must be dropped, not re-keyed with ..")
	}
	if len(changesetRelativize(nil, "/w")) != 0 {
		t.Fatal("nil trace must stay nil")
	}
}

// --- panel rendering ---

func TestBuildProjectTreeLines_ChangesetStyling(t *testing.T) {
	files := []string{"cmd/milk/main.go", "README.md", "go.mod"}
	changes := map[string]session.ChangeKind{
		"cmd/milk/main.go": session.ChangeModified,
		"cmd/milk/gone.go": session.ChangeDeleted,
		"go.mod":           session.ChangeCreated,
		"README.md":        session.ChangeRenamed,
	}
	lines, ids := buildProjectTreeLines(files, false, nil, projectPanelInner, changes)
	plain := func() []string {
		var out []string
		for _, l := range lines {
			out = append(out, ansi.Strip(l))
		}
		return out
	}()

	// The summary counts every kind — deletions never render as rows.
	if !strings.Contains(plain[2], "changes 1A 1M 1D 1R") {
		t.Fatalf("summary line = %q", plain[2])
	}
	// Ancestor directories of changed files carry the muted marker.
	joined := strings.Join(plain, "\n")
	if !strings.Contains(joined, "▾ cmd/·") {
		t.Errorf("changed ancestor dir missing marker:\n%s", joined)
	}
	if !strings.Contains(joined, "  ▸ milk/·") {
		t.Errorf("nested changed ancestor dir missing marker:\n%s", joined)
	}
	// Changed files show their status glyph; unchanged ones don't.
	if !strings.Contains(joined, "A go.mod") || !strings.Contains(joined, "R README.md") {
		t.Errorf("changed file glyph missing:\n%s", joined)
	}
	// Deleted files exist only in the summary — no row, but their
	// ancestors still mark.
	if strings.Contains(joined, "gone.go") {
		t.Errorf("deleted file must not render as a row:\n%s", joined)
	}
	// IDs stay stable (summary lines are non-interactive).
	for _, id := range ids {
		if id == "gone.go" {
			t.Fatal("deleted file must not take a click target")
		}
	}
}

func TestBuildProjectTreeLines_NoChangesRenderingUnchanged(t *testing.T) {
	files := []string{"cmd/milk/main.go", "go.mod"}
	withNil, _ := buildProjectTreeLines(files, false, nil, projectPanelInner, nil)
	withEmpty, _ := buildProjectTreeLines(files, false, nil, projectPanelInner, map[string]session.ChangeKind{})
	if len(withNil) != len(withEmpty) {
		t.Fatalf("nil vs empty changeset render differently: %d vs %d lines", len(withNil), len(withEmpty))
	}
	for i := range withNil {
		if ansi.Strip(withNil[i]) != ansi.Strip(withEmpty[i]) {
			t.Fatalf("line %d: %q vs %q", i, ansi.Strip(withNil[i]), ansi.Strip(withEmpty[i]))
		}
	}
	// And the historical structure is intact (no summary/glyph rows).
	joined := ""
	for _, l := range withNil {
		joined += ansi.Strip(l) + "\n"
	}
	if strings.Contains(joined, "changes ") {
		t.Errorf("empty changeset must not add a summary line:\n%s", joined)
	}
}

// --- refresh + model wiring ---

func TestRefreshChangesetCmd_NonRepoReportsNoRepo(t *testing.T) {
	msg := refreshChangesetCmd(t.TempDir())().(changesetMsg)
	if !msg.noRepo || msg.gitOK || msg.files != nil {
		t.Fatalf("non-repo msg = %+v, want noRepo", msg)
	}
}

func TestSyncChangeset_TraceRendersAndGitSnapshotWins(t *testing.T) {
	dir := t.TempDir()
	sess := &session.Session{ID: "cmd-changeset-sync", CWD: dir}
	t.Cleanup(func() { session.ClearFileChanges(sess.ID) })

	m := &model{
		st:           &interactiveState{sess: sess, cwd: dir},
		projectFiles: []string{"a.txt"},
	}
	// Trace only: the modified file renders with its glyph.
	session.RecordFileChange(sess.ID, filepath.Join(dir, "a.txt"), session.ChangeModified)
	m.syncChangeset()
	if len(m.changesetFiles) != 1 {
		t.Fatalf("changeset = %v", m.changesetFiles)
	}
	joined := ""
	for _, l := range m.projectLines {
		joined += ansi.Strip(l) + "\n"
	}
	if !strings.Contains(joined, "M a.txt") || !strings.Contains(joined, "changes 1M") {
		t.Fatalf("tree not rebuilt from the trace:\n%s", joined)
	}
	// A clean repo snapshot drops the stale trace entry (staleness rule).
	m.changesetGitOK = true
	m.changesetGit = map[string]session.ChangeKind{}
	m.syncChangeset()
	if len(m.changesetFiles) != 0 {
		t.Fatalf("git snapshot ignored: %v", m.changesetFiles)
	}
	joined = ""
	for _, l := range m.projectLines {
		joined += ansi.Strip(l) + "\n"
	}
	if strings.Contains(joined, "M a.txt") || strings.Contains(joined, "changes ") {
		t.Fatalf("stale trace still rendered:\n%s", joined)
	}
}

func TestChangesetRefreshCmd_SkipsKnownNonRepoAndResetsOnCwdChange(t *testing.T) {
	m := &model{st: &interactiveState{cwd: "/definitely/not/a/repo/x"}}
	if m.changesetRefreshCmd() == nil {
		t.Fatal("first probe must schedule")
	}
	m.changesetNoGit = true // what changesetMsg{noRepo} sets
	if m.changesetRefreshCmd() != nil {
		t.Fatal("known non-repo must be skipped (the issue #331 skip)")
	}
	// A new cwd re-probes: every snapshot is cwd-scoped and resets.
	m.st.cwd = "/elsewhere"
	cmd := m.changesetRefreshCmd()
	if cmd == nil {
		t.Fatal("cwd change must re-probe")
	}
	if m.changesetCwd != "/elsewhere" || m.changesetNoGit {
		t.Fatalf("cwd change must reset the skip bookkeeping, got cwd=%q noGit=%v", m.changesetCwd, m.changesetNoGit)
	}
}
