package local

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
)

// toolResultsByCall collects tool-role messages keyed by tool call ID.
func toolResultsByCall(msgs []Message) map[string]string {
	out := map[string]string{}
	for _, m := range msgs {
		if m.Role == "tool" {
			out[m.ToolCallID] = m.Content
		}
	}
	return out
}

// B1 regression: a write_file and a bash read of the same file in ONE batch
// must run in order — the cat sees the new content instead of racing the write.
func TestBatch_WriteThenReadSeesContent(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")

	a := &Agent{skipPerms: true, toolTimeout: 5 * time.Second}
	tcs := []toolCall{
		{ID: "w", Function: toolCallFunction{Name: "write_file", Arguments: `{"path":"` + f + `","content":"hello"}`}},
		{ID: "b", Function: toolCallFunction{Name: "bash", Arguments: `{"command":"cat ` + f + `"}`}},
	}
	msgs, err := a.executeToolCalls(context.Background(), nil, tcs, "", "", io.Discard, nil, nil, "", nil)
	if err != nil {
		t.Fatalf("executeToolCalls: %v", err)
	}
	res := toolResultsByCall(msgs)
	if !strings.Contains(res["b"], "hello") {
		t.Errorf("bash cat must see the written content, got %q", res["b"])
	}
}

// B1 regression: two edit_file calls on the same file in ONE batch must both
// land — sequential barriers mean no lost read-modify-write race.
func TestBatch_TwoEditsSameFileBothLand(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.go")
	if err := os.WriteFile(f, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Agent{skipPerms: true, toolTimeout: 5 * time.Second}
	tcs := []toolCall{
		{ID: "e1", Function: toolCallFunction{Name: "edit_file", Arguments: `{"path":"` + f + `","old_string":"alpha","new_string":"ALPHA"}`}},
		{ID: "e2", Function: toolCallFunction{Name: "edit_file", Arguments: `{"path":"` + f + `","old_string":"gamma","new_string":"GAMMA"}`}},
	}
	msgs, err := a.executeToolCalls(context.Background(), nil, tcs, "", "", io.Discard, nil, nil, "", nil)
	if err != nil {
		t.Fatalf("executeToolCalls: %v", err)
	}
	got, _ := os.ReadFile(f)
	want := "ALPHA\nbeta\nGAMMA\n"
	if string(got) != want {
		t.Errorf("both edits must land:\n got %q\nwant %q", got, want)
	}
	res := toolResultsByCall(msgs)
	if isToolError(res["e1"]) || isToolError(res["e2"]) {
		t.Errorf("both edits must succeed: e1=%q e2=%q", res["e1"], res["e2"])
	}
}

// B1 decision (ADR-0062): a failed mutating barrier (bash/write/…) cancels the
// rest of the batch — later calls are skipped with an explanatory result.
func TestBatch_FailedMutatingBarrierCancelsRest(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "never.txt")

	a := &Agent{skipPerms: true, toolTimeout: 5 * time.Second}
	tcs := []toolCall{
		{ID: "boom", Function: toolCallFunction{Name: "bash", Arguments: `{"command":"false"}`}},
		{ID: "w", Function: toolCallFunction{Name: "write_file", Arguments: `{"path":"` + f + `","content":"x"}`}},
		{ID: "r", Function: toolCallFunction{Name: "read_file", Arguments: `{"path":"` + f + `"}`}},
	}
	msgs, err := a.executeToolCalls(context.Background(), nil, tcs, "", "", io.Discard, nil, nil, "", nil)
	if err != nil {
		t.Fatalf("executeToolCalls: %v", err)
	}
	res := toolResultsByCall(msgs)
	if !toolCallFailed("bash", res["boom"]) {
		t.Errorf("false must fail: %q", res["boom"])
	}
	for _, id := range []string{"w", "r"} {
		if !strings.Contains(res[id], "skipped") || !strings.Contains(res[id], "cancelled") {
			t.Errorf("%s must be skipped after the failed barrier, got %q", id, res[id])
		}
	}
	if _, statErr := os.Stat(f); statErr == nil {
		t.Error("write after a failed barrier must not run")
	}
}

// A failed READ does not cancel anything — reads are not cancelling barriers.
func TestBatch_FailedReadDoesNotCancel(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "later.txt")

	a := &Agent{skipPerms: true, toolTimeout: 5 * time.Second}
	tcs := []toolCall{
		{ID: "miss", Function: toolCallFunction{Name: "read_file", Arguments: `{"path":"` + filepath.Join(dir, "nope.txt") + `"}`}},
		{ID: "w", Function: toolCallFunction{Name: "write_file", Arguments: `{"path":"` + f + `","content":"x"}`}},
	}
	msgs, err := a.executeToolCalls(context.Background(), nil, tcs, "", "", io.Discard, nil, nil, "", nil)
	if err != nil {
		t.Fatalf("executeToolCalls: %v", err)
	}
	res := toolResultsByCall(msgs)
	if !isToolError(res["miss"]) {
		t.Errorf("missing file must fail: %q", res["miss"])
	}
	if isToolError(res["w"]) {
		t.Errorf("write must still run after a failed read, got %q", res["w"])
	}
	if _, statErr := os.Stat(f); statErr != nil {
		t.Errorf("write after a failed read must land: %v", statErr)
	}
}

// Issue #271 regression: a bash call that exits 0 while writing to stderr is
// a SUCCESS. stderr is informational (grep's "binary file matches", compiler
// and git notices) — it must not cancel the rest of the batch.
func TestBatch_BashExitZeroStderrDoesNotCancel(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "later.txt")

	a := &Agent{skipPerms: true, toolTimeout: 5 * time.Second}
	tcs := []toolCall{
		{ID: "warn", Function: toolCallFunction{Name: "bash", Arguments: `{"command":"echo oops >&2"}`}},
		{ID: "w", Function: toolCallFunction{Name: "write_file", Arguments: `{"path":"` + f + `","content":"x"}`}},
	}
	msgs, err := a.executeToolCalls(context.Background(), nil, tcs, "", "", io.Discard, nil, nil, "", nil)
	if err != nil {
		t.Fatalf("executeToolCalls: %v", err)
	}
	res := toolResultsByCall(msgs)
	if toolCallFailed("bash", res["warn"]) {
		t.Errorf("exit-0-with-stderr must not count as a bash failure: %q", res["warn"])
	}
	if strings.Contains(res["w"], "skipped") {
		t.Errorf("write must not be skipped after an exit-0 bash with stderr, got %q", res["w"])
	}
	if _, statErr := os.Stat(f); statErr != nil {
		t.Errorf("write after an exit-0 bash with stderr must land: %v", statErr)
	}
}

// A bash launch failure (the working directory does not exist) is a failure
// and cancels the rest of the batch — runBash reports it as exit code -1.
func TestBatch_BashStartFailureCancels(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "never.txt")

	a := &Agent{skipPerms: true, toolTimeout: 5 * time.Second}
	tcs := []toolCall{
		{ID: "boom", Function: toolCallFunction{Name: "bash", Arguments: `{"command":"echo hi","workdir":"` + filepath.Join(dir, "no-such-dir") + `"}`}},
		{ID: "w", Function: toolCallFunction{Name: "write_file", Arguments: `{"path":"` + f + `","content":"x"}`}},
	}
	msgs, err := a.executeToolCalls(context.Background(), nil, tcs, "", "", io.Discard, nil, nil, "", nil)
	if err != nil {
		t.Fatalf("executeToolCalls: %v", err)
	}
	res := toolResultsByCall(msgs)
	if !toolCallFailed("bash", res["boom"]) {
		t.Errorf("a launch failure must fail the bash call: %q", res["boom"])
	}
	if !strings.Contains(res["w"], "skipped") || !strings.Contains(res["w"], "cancelled") {
		t.Errorf("write must be skipped after a failed launch, got %q", res["w"])
	}
}

// A bash timeout is a failure and cancels the rest of the batch (exit code -1
// from the timeout path — no string matching on the error text).
func TestBatch_BashTimeoutCancels(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "never.txt")

	a := &Agent{skipPerms: true, toolTimeout: 5 * time.Second}
	tcs := []toolCall{
		{ID: "hang", Function: toolCallFunction{Name: "bash", Arguments: `{"command":"sleep 5","timeout_seconds":0.3}`}},
		{ID: "w", Function: toolCallFunction{Name: "write_file", Arguments: `{"path":"` + f + `","content":"x"}`}},
	}
	msgs, err := a.executeToolCalls(context.Background(), nil, tcs, "", "", io.Discard, nil, nil, "", nil)
	if err != nil {
		t.Fatalf("executeToolCalls: %v", err)
	}
	res := toolResultsByCall(msgs)
	if !toolCallFailed("bash", res["hang"]) {
		t.Errorf("a timeout must fail the bash call: %q", res["hang"])
	}
	if !strings.Contains(res["w"], "skipped") || !strings.Contains(res["w"], "cancelled") {
		t.Errorf("write must be skipped after a timed-out bash, got %q", res["w"])
	}
}

// grep's "no matches" (exit 1) is a read, never a cancelling barrier.
func TestBatch_GrepNoMatchDoesNotCancel(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(f, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Agent{skipPerms: true, toolTimeout: 5 * time.Second}
	tcs := []toolCall{
		{ID: "miss", Function: toolCallFunction{Name: "grep", Arguments: `{"pattern":"zzz-no-match","path":"` + f + `"}`}},
		{ID: "w", Function: toolCallFunction{Name: "write_file", Arguments: `{"path":"` + filepath.Join(dir, "out.txt") + `","content":"x"}`}},
	}
	msgs, err := a.executeToolCalls(context.Background(), nil, tcs, "", "", io.Discard, nil, nil, "", nil)
	if err != nil {
		t.Fatalf("executeToolCalls: %v", err)
	}
	res := toolResultsByCall(msgs)
	if toolCallFailed("grep", res["miss"]) {
		t.Errorf("grep no-match must not count as a failure: %q", res["miss"])
	}
	if strings.Contains(res["w"], "skipped") {
		t.Errorf("write must run after a no-match grep, got %q", res["w"])
	}
}

// B2 regression: relative paths and shell commands resolve against the
// SESSION cwd (which over ACP can differ from the process cwd), not the
// process cwd. Every path-taking tool is covered.
func TestTools_RelativePathsUseSessionCWD(t *testing.T) {
	procDir := t.TempDir()
	sessDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sessDir, "existing.txt"), []byte("in-session\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(procDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(oldWD) }) //nolint:errcheck

	sess := &session.Session{CWD: sessDir}
	ctx := context.Background()

	// write_file: relative path lands under sessDir.
	if res, _ := dispatchTool(ctx, "write_file", `{"path":"made.txt","content":"x"}`, sess, nil, memory.ConsumerAll, "", nil); isToolError(res) {
		t.Fatalf("write_file: %s", res)
	}
	if _, err := os.Stat(filepath.Join(sessDir, "made.txt")); err != nil {
		t.Errorf("write_file must land in the session cwd: %v", err)
	}

	// read_file: relative path reads from sessDir.
	res, _ := dispatchTool(ctx, "read_file", `{"path":"existing.txt"}`, sess, nil, memory.ConsumerAll, "", nil)
	if !strings.Contains(res, "in-session") {
		t.Errorf("read_file must read from the session cwd, got %q", res)
	}

	// bash: command runs in sessDir.
	res, _ = dispatchTool(ctx, "bash", `{"command":"pwd"}`, sess, nil, memory.ConsumerAll, "", nil)
	if !strings.Contains(res, filepath.Base(sessDir)) {
		t.Errorf("bash must run in the session cwd %q, got %q", sessDir, res)
	}

	// list_dir: relative path lists sessDir.
	res, _ = dispatchTool(ctx, "list_dir", `{"path":"."}`, sess, nil, memory.ConsumerAll, "", nil)
	if !strings.Contains(res, "existing.txt") {
		t.Errorf("list_dir must list the session cwd, got %q", res)
	}

	// edit_file + move_file + delete_file on relative paths.
	if err := os.WriteFile(filepath.Join(sessDir, "editme.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, _ := dispatchTool(ctx, "edit_file", `{"path":"editme.txt","old_string":"one","new_string":"two"}`, sess, nil, memory.ConsumerAll, "", nil); isToolError(res) {
		t.Errorf("edit_file: %s", res)
	}
	if got, _ := os.ReadFile(filepath.Join(sessDir, "editme.txt")); string(got) != "two\n" {
		t.Errorf("edit_file must edit in the session cwd, got %q", got)
	}
	if res, _ := dispatchTool(ctx, "move_file", `{"source":"editme.txt","destination":"moved.txt"}`, sess, nil, memory.ConsumerAll, "", nil); isToolError(res) {
		t.Errorf("move_file: %s", res)
	}
	if _, err := os.Stat(filepath.Join(sessDir, "moved.txt")); err != nil {
		t.Errorf("move_file must move within the session cwd: %v", err)
	}
	if res, _ := dispatchTool(ctx, "delete_file", `{"path":"moved.txt"}`, sess, nil, memory.ConsumerAll, "", nil); isToolError(res) {
		t.Errorf("delete_file: %s", res)
	}
	if _, err := os.Stat(filepath.Join(sessDir, "moved.txt")); err == nil {
		t.Error("delete_file must delete in the session cwd")
	}
}

// B3 regression: grep uses extended regex — foo|bar and a+ match.
func TestGrep_ExtendedRegex(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("foo\nbar\ncaat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"foo|bar", "a+", "f.o"} {
		res, _ := dispatchTool(context.Background(), "grep", `{"pattern":"`+pattern+`","path":"`+dir+`","recursive":true}`, nil, nil, memory.ConsumerAll, "", nil)
		if isToolError(res) {
			t.Fatalf("grep %q: %s", pattern, res)
		}
		if strings.Contains(res, `"output":""`) {
			t.Errorf("grep pattern %q matched nothing — expected extended-regex semantics", pattern)
		}
	}
}

// B5 contract: a tool that is dispatched but not defined (the old glob /
// apply_patch pattern) must fail, and every schema-defined tool must be
// dispatchable — both directions fail CI otherwise.
func TestToolSetContract_NoDeadDispatchCases(t *testing.T) {
	for _, name := range []string{"glob", "apply_patch"} {
		res, _ := dispatchTool(context.Background(), name, "{}", nil, nil, memory.ConsumerAll, "", nil)
		if !isUnknownToolResult(res) {
			t.Errorf("%q must not be dispatched (no such tool): %s", name, res)
		}
	}
}

// dispatchedOutsideDispatchTool are handled directly in dispatchOneTool
// (signals to the host layer), so dispatchTool must not know them.
var dispatchedOutsideDispatchTool = map[string]bool{
	"ask_user":                true,
	"open_file":               true,
	"start_workflow":          true,
	"status_background_agent": true,
	"wait_background_agent":   true,
	"send_background_agent":   true,
}

// isUnknownToolResult reports whether a dispatchTool result is the
// "unknown tool" error — matched on the error prefix, never on a substring of
// the output (a grep over the source tree can legitimately contain the words).
func isUnknownToolResult(result string) bool {
	var r toolResult
	if json.Unmarshal([]byte(result), &r) != nil {
		return false
	}
	return strings.HasPrefix(r.Error, "unknown tool:")
}

func TestToolSetContract_EverySchemaToolDispatches(t *testing.T) {
	projectMapTestIsolate(t) // project_map maps the package dir — keep its index out of ~/.milk
	sess := &session.Session{}
	otelDir := t.TempDir()
	tools := schemas(&memory.Store{}, otelDir, sess, nil, &mockTaskStore{}, nil)
	for _, s := range tools {
		name := toolName(s)
		if name == "" {
			t.Fatalf("schema without a name: %v", s)
		}
		if dispatchedOutsideDispatchTool[name] {
			continue
		}
		res, _ := dispatchTool(context.Background(), name, "{}", sess, nil, memory.ConsumerAll, otelDir, &mockTaskStore{})
		if isUnknownToolResult(res) {
			t.Errorf("tool %q is in schemas() but dispatchTool does not know it", name)
		}
	}
}

// Issue #480 follow-up (found by the tool_search Phase 2 A/B): a corrective
// tool_search guidance result — here a guessed deferred-tool refusal — must
// NOT cancel the rest of the batch (ADR-0062's cancel-on-failure is about
// the world changing, and a refusal never touches the world). Before the
// fix, the delete_file refusal cancelled the sibling tool_search call that
// would have resolved it, sending the model into a retry loop that the
// runaway guard had to stop.
func TestBatch_ToolSearchRefusalDoesNotCancelSiblings(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	entry := tsFullEntry("delete_file", "Delete a file from disk.", tsStringParam("path"))
	a := tsAgent()
	a.skipPerms = true // delete_file is safety-gated; the batch tests run unattended
	a.toolTimeout = 5 * time.Second
	_, rt := a.applyToolSearch([]map[string]any{entry}, []map[string]any{entry})
	ctx := withToolSearch(context.Background(), rt)

	tcs := []toolCall{
		{ID: "d1", Function: toolCallFunction{Name: "delete_file", Arguments: `{"path":"` + f + `"}`}},
		{ID: "ts", Function: toolCallFunction{Name: "tool_search", Arguments: `{"query":"delete file"}`}},
		{ID: "d2", Function: toolCallFunction{Name: "delete_file", Arguments: `{"path":"` + f + `"}`}},
	}
	msgs, err := a.executeToolCalls(ctx, nil, tcs, "", "", io.Discard, nil, nil, "", nil)
	if err != nil {
		t.Fatalf("executeToolCalls: %v", err)
	}
	res := toolResultsByCall(msgs)

	if !strings.Contains(res["d1"], "deferred and state-changing") {
		t.Errorf("d1 must get the deferral refusal guidance, got %q", res["d1"])
	}
	for _, id := range []string{"ts", "d2"} {
		if strings.Contains(res[id], "skipped") || strings.Contains(res[id], "cancelled") {
			t.Errorf("%s must not be cancelled by the guidance result, got %q", id, res[id])
		}
	}
	if !strings.Contains(res["ts"], "Loaded") {
		t.Errorf("tool_search must load the schema, got %q", res["ts"])
	}
	if _, statErr := os.Stat(f); !os.IsNotExist(statErr) {
		t.Errorf("d2 must execute after the load (victim.txt should be deleted), stat err = %v, result = %q", statErr, res["d2"])
	}
}
