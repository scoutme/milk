package local

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
)

// The five mutating tools record their deltas at the dispatch boundary —
// only on success (issue #331): a failed tool changed no files.

func traceTestSession(t *testing.T) (*session.Session, string) {
	t.Helper()
	dir := t.TempDir()
	sess := &session.Session{ID: "trace-" + t.Name(), CWD: dir}
	t.Cleanup(func() { session.ClearFileChanges(sess.ID) })
	return sess, dir
}

func TestDispatch_TracesSuccessfulWritesAndEdits(t *testing.T) {
	sess, dir := traceTestSession(t)
	ctx := t.Context()

	_, _ = dispatchTool(ctx, "write_file", `{"path":"new.go","content":"x"}`, sess, nil, memory.ConsumerAll, "", nil)
	_, _ = dispatchTool(ctx, "edit_file", `{"path":"new.go","old_string":"x","new_string":"y"}`, sess, nil, memory.ConsumerAll, "", nil)

	got := session.FileChanges(sess.ID)
	want := filepath.Join(dir, "new.go")
	// write created it; the edit must not demote a net-created path to
	// "modified" (net-aware kinds).
	if k, ok := got[want]; !ok || k != session.ChangeCreated {
		t.Fatalf("new.go = %v (ok=%v), want created", k, ok)
	}
	if len(got) != 1 {
		t.Fatalf("changeset = %v, want exactly one path", got)
	}
}

func TestDispatch_DoesNotTraceFailedTools(t *testing.T) {
	sess, _ := traceTestSession(t)
	ctx := t.Context()

	// edit_file on a missing file errors → nothing recorded.
	res, _ := dispatchTool(ctx, "edit_file", `{"path":"missing.txt","old_string":"a","new_string":"b"}`, sess, nil, memory.ConsumerAll, "", nil)
	if !isToolError(res) {
		t.Fatalf("expected an error result, got %s", res)
	}
	// delete_file of a missing file errors → nothing recorded.
	res, _ = dispatchTool(ctx, "delete_file", `{"path":"missing.txt"}`, sess, nil, memory.ConsumerAll, "", nil)
	if !isToolError(res) {
		t.Fatalf("expected an error result, got %s", res)
	}
	if got := session.FileChanges(sess.ID); len(got) != 0 {
		t.Fatalf("failed tools leaked into the trace: %v", got)
	}
}

func TestDispatch_TracesDeleteNetZero(t *testing.T) {
	sess, _ := traceTestSession(t)
	ctx := t.Context()

	_, _ = dispatchTool(ctx, "write_file", `{"path":"tmp.txt","content":"x"}`, sess, nil, memory.ConsumerAll, "", nil)
	_, _ = dispatchTool(ctx, "delete_file", `{"path":"tmp.txt"}`, sess, nil, memory.ConsumerAll, "", nil)

	if got := session.FileChanges(sess.ID); len(got) != 0 {
		t.Fatalf("created-then-deleted must net to nothing, got %v", got)
	}
}

func TestDispatch_TracesMoveBothSides(t *testing.T) {
	sess, dir := traceTestSession(t)
	ctx := t.Context()

	// Case 1: a file this session created, then moved — net created at the
	// new path, source forgotten.
	_, _ = dispatchTool(ctx, "write_file", `{"path":"from.txt","content":"x"}`, sess, nil, memory.ConsumerAll, "", nil)
	_, _ = dispatchTool(ctx, "move_file", `{"source":"from.txt","destination":"to.txt"}`, sess, nil, memory.ConsumerAll, "", nil)

	got := session.FileChanges(sess.ID)
	if k, ok := got[filepath.Join(dir, "to.txt")]; !ok || k != session.ChangeCreated {
		t.Fatalf("dst after created-move = %v (ok=%v), want created", k, ok)
	}
	if _, ok := got[filepath.Join(dir, "from.txt")]; ok {
		t.Fatalf("source side must be dropped for a net-created move, got %v", got)
	}
	session.ClearFileChanges(sess.ID)

	// Case 2: a pre-session file, then moved — both sides recorded.
	pre := filepath.Join(dir, "pre.txt")
	if err := os.WriteFile(pre, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = dispatchTool(ctx, "move_file", `{"source":"pre.txt","destination":"moved.txt"}`, sess, nil, memory.ConsumerAll, "", nil)

	got = session.FileChanges(sess.ID)
	if k, ok := got[pre]; !ok || k != session.ChangeDeleted {
		t.Fatalf("src = %v (ok=%v), want deleted", k, ok)
	}
	if k, ok := got[filepath.Join(dir, "moved.txt")]; !ok || k != session.ChangeRenamed {
		t.Fatalf("dst = %v (ok=%v), want renamed", k, ok)
	}
}

func TestDispatch_TraceGuards(t *testing.T) {
	ctx := t.Context()
	// Absolute temp paths: with a nil/ID-less session the cwd is "" and a
	// relative path would resolve into the package directory — the guards
	// are about recording, not about where the write lands.
	absA := filepath.ToSlash(filepath.Join(t.TempDir(), "a.txt"))
	absB := filepath.ToSlash(filepath.Join(t.TempDir(), "b.txt"))
	// Nil session: no panic, no global bucket.
	_, _ = dispatchTool(ctx, "write_file", `{"path":"`+absA+`","content":"x"}`, nil, nil, memory.ConsumerAll, "", nil)
	// Session with no ID: nothing recorded anywhere.
	sess := &session.Session{CWD: t.TempDir()}
	_, _ = dispatchTool(ctx, "write_file", `{"path":"`+absB+`","content":"x"}`, sess, nil, memory.ConsumerAll, "", nil)
	if got := session.FileChanges(""); len(got) != 0 {
		t.Fatalf("empty-id session collected: %v", got)
	}
}
