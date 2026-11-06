package local

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
	"github.com/scoutme/milk/internal/workflow/board"
)

func TestGlobToRegex_MatchGlob(t *testing.T) {
	for _, tc := range []struct {
		pattern, rel string
		want         bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "src/main.go", true}, // basename match
		{"*.go", "main.py", false},
		{"src/*.ts", "src/a.ts", true},
		{"src/*.ts", "src/deep/a.ts", false}, // * does not cross /
		{"src/**/*.ts", "src/a.ts", true},    // **/ matches zero dirs
		{"src/**/*.ts", "src/deep/a.ts", true},
		{"**/index.ts", "a/b/c/index.ts", true},
		{"**/index.ts", "index.ts", true},
		{"file?.txt", "file1.txt", true},
		{"file?.txt", "file12.txt", false},
		{"a.b", "axb", false}, // '.' is literal
	} {
		if got := matchGlob(tc.pattern, tc.rel); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.rel, got, tc.want)
		}
	}
}

func TestFindFiles_GlobAndCap(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.txt", filepath.Join("sub", "d.go")} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, _ := dispatchTool(context.Background(), "find_files", `{"path":"`+dir+`","pattern":"*.go"}`, nil, nil, memory.ConsumerAll, "", nil)
	for _, want := range []string{"a.go", "b.go", "d.go"} {
		if !strings.Contains(res, want) {
			t.Errorf("missing %q in %s", want, res)
		}
	}
	if strings.Contains(res, "c.txt") {
		t.Errorf("c.txt must not match *.go: %s", res)
	}

	// ** glob matches nested paths.
	res, _ = dispatchTool(context.Background(), "find_files", `{"path":"`+dir+`","pattern":"**/d.go"}`, nil, nil, memory.ConsumerAll, "", nil)
	if !strings.Contains(res, "d.go") || strings.Contains(res, "a.go") {
		t.Errorf("** glob wrong: %s", res)
	}
}

func TestFindFiles_TruncatesWithMarker(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < findFilesMaxResults+5; i++ {
		name := filepath.Join(dir, "f"+strconv.Itoa(i)+".txt")
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, _ := dispatchTool(context.Background(), "find_files", `{"path":"`+dir+`","pattern":"*.txt"}`, nil, nil, memory.ConsumerAll, "", nil)
	if !strings.Contains(res, "truncated") || !strings.Contains(res, "5 more matches") {
		t.Errorf("expected truncation marker naming the withheld count, got %q", tail(res, 300))
	}
}

func TestGrep_FiltersAndCaps(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nfunc Hello() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("hello text\nhello again\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// include filter
	res, _ := dispatchTool(context.Background(), "grep", `{"pattern":"Hello","path":"`+dir+`","include":"*.go","recursive":true}`, nil, nil, memory.ConsumerAll, "", nil)
	if !strings.Contains(res, "Hello()") || strings.Contains(res, "hello text") {
		t.Errorf("include filter wrong: %s", res)
	}

	// ignore_case
	res, _ = dispatchTool(context.Background(), "grep", `{"pattern":"hello","path":"`+dir+`","ignore_case":true,"recursive":true,"include":"*.go"}`, nil, nil, memory.ConsumerAll, "", nil)
	if !strings.Contains(res, "Hello()") {
		t.Errorf("ignore_case not applied: %s", res)
	}
	res, _ = dispatchTool(context.Background(), "grep", `{"pattern":"hello","path":"`+dir+`","recursive":true,"include":"*.go"}`, nil, nil, memory.ConsumerAll, "", nil)
	if strings.Contains(res, "Hello()") {
		t.Errorf("case-sensitive search matched uppercase: %s", res)
	}

	// max_results truncation marker
	res, _ = dispatchTool(context.Background(), "grep", `{"pattern":"hello","path":"`+dir+`","recursive":true,"include":"*.txt","max_results":1}`, nil, nil, memory.ConsumerAll, "", nil)
	if !strings.Contains(res, "truncated") || !strings.Contains(res, "1 more") {
		t.Errorf("expected truncation marker, got %q", res)
	}
}

func TestGrep_ContextLines(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("one\ntwo\nthree\nfour\nfive\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := dispatchTool(context.Background(), "grep", `{"pattern":"three","path":"`+dir+`","context":1}`, nil, nil, memory.ConsumerAll, "", nil)
	for _, want := range []string{"two", "three", "four"} {
		if !strings.Contains(res, want) {
			t.Errorf("context=1 must include %q: %s", want, res)
		}
	}
	if strings.Contains(res, "five") {
		t.Errorf("context=1 must not include five: %s", res)
	}
}

// bash workdir overrides the session cwd for one command.
func TestBash_Workdir(t *testing.T) {
	dir := t.TempDir()
	sessDir := t.TempDir()
	sess := &session.Session{CWD: sessDir}
	res, _ := dispatchTool(context.Background(), "bash", `{"command":"pwd","workdir":"`+dir+`"}`, sess, nil, memory.ConsumerAll, "", nil)
	if !strings.Contains(res, filepath.Base(dir)) {
		t.Errorf("workdir not applied: %s", res)
	}
}

// Background shell lifecycle: start, read output, kill. Exit criteria for
// Phase 3 ("dev server up, hit it, kill it") in unit form.
func TestBackgroundShell_Lifecycle(t *testing.T) {
	s, err := bgShells.start("echo boot; sleep 30; echo done", t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	id := s.id
	t.Cleanup(func() { s.kill("test cleanup", "") }) //nolint:errcheck

	// Output arrives asynchronously.
	deadline := time.Now().Add(3 * time.Second)
	for {
		res, _ := runBashOutput(map[string]any{"shell_id": id})
		if strings.Contains(res, "boot") {
			if !strings.Contains(res, "running") {
				t.Errorf("expected running status, got %q", res)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background output never arrived: %s", res)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// tail_lines
	res, _ := runBashOutput(map[string]any{"shell_id": id, "tail_lines": 1})
	if !strings.Contains(res, "boot") {
		t.Errorf("tail_lines=1 must keep the only line: %q", res)
	}

	// kill
	res, _ = runKillBash(context.Background(), map[string]any{"shell_id": id})
	if isToolError(res) {
		t.Fatalf("kill_bash: %s", res)
	}
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		t.Fatal("shell did not stop after kill_bash")
	}
	res, _ = runBashOutput(map[string]any{"shell_id": id})
	if !strings.Contains(res, "killed") {
		t.Errorf("expected killed status, got %q", res)
	}

	// Unknown IDs fail cleanly.
	if res, _ := runBashOutput(map[string]any{"shell_id": "nope"}); !isToolError(res) {
		t.Errorf("unknown shell_id must error: %s", res)
	}
	if res, _ := runKillBash(context.Background(), map[string]any{"shell_id": "nope"}); !isToolError(res) {
		t.Errorf("unknown shell_id must error: %s", res)
	}
}

// run_in_background goes through the bash tool end to end.
func TestBash_RunInBackgroundTool(t *testing.T) {
	res, _ := dispatchTool(context.Background(), "bash", `{"command":"echo hi-from-bg; sleep 30","run_in_background":true}`, nil, nil, memory.ConsumerAll, "", nil)
	if isToolError(res) {
		t.Fatalf("run_in_background: %s", res)
	}
	var r toolResult
	if err := json.Unmarshal([]byte(res), &r); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(r.Output)
	id = strings.TrimPrefix(id, "started background shell ")
	if idx := strings.Index(id, " "); idx > 0 {
		id = id[:idx]
	}
	t.Cleanup(func() {
		runKillBash(context.Background(), map[string]any{"shell_id": id}) //nolint:errcheck
	})

	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := runBashOutput(map[string]any{"shell_id": id})
		if strings.Contains(out, "hi-from-bg") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background output missing: %s", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// tail() returns the last n bytes of s for readable failure messages.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// A shell started under a board identity remembers its owner (the tool path
// reads it off the ctx), and a kill from a different requester — logged as
// cross_owner — still stops it: ownership is attribution, not access control.
func TestBackgroundShell_OwnerFromBoardIdentity(t *testing.T) {
	ctx := board.WithIdentity(context.Background(), "worker\u00b7Item-2")
	if got := shellActor(ctx); got != "worker\u00b7Item-2" {
		t.Fatalf("shellActor = %q", got)
	}
	s, err := bgShells.start("sleep 30", t.TempDir(), shellActor(ctx))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.kill("test cleanup", "") })
	if s.owner != "worker\u00b7Item-2" {
		t.Errorf("owner = %q, want the board identity", s.owner)
	}
	other := board.WithIdentity(context.Background(), "evaluator\u00b7Item-2")
	if out, _ := runKillBash(other, map[string]any{"shell_id": s.id}); isToolError(out) {
		t.Fatalf("cross-owner kill_bash failed: %s", out)
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("shell was not stopped")
	}
}

func TestLogSnippet(t *testing.T) {
	if got := logSnippet("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
	// "é" is two bytes: cutting at 3 would split the second one.
	if got := logSnippet("abéé", 4); got != "abé…" {
		t.Errorf("got %q, want rune-safe cut", got)
	}
}

func TestUsageRole_WorkflowRoleSuffix(t *testing.T) {
	a := (&Agent{escalationName: ""}).AsWorkflowExecutor()
	base := a.logRole()
	if got := a.usageRole(context.Background()); got != base {
		t.Errorf("no turn role: got %q, want %q", got, base)
	}
	ctx := workflow.WithTurnRole(context.Background(), "worker")
	if got := a.usageRole(ctx); got != base+":worker" {
		t.Errorf("got %q, want %q", got, base+":worker")
	}
	plain := &Agent{}
	if got := plain.usageRole(ctx); got != plain.logRole() {
		t.Errorf("a non-workflow agent must ignore the turn role, got %q", got)
	}
}
