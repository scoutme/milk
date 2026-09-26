package main

// Regression tests for how cliRunner.Execute delivers context on resumed Claude
// CLI turns. Claude Code records a conversation's system prompt on its first
// request and replays it on every resume (--system-prompt-snapshot, default on),
// so --append-system-prompt-file on resume is ignored until a compaction. Per-turn
// context must therefore travel in the user message; the full stable block still
// goes to the file so a compaction re-records it (ADR-0046, verified behaviour
// #5/#6).

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/agent/claude"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/session"
)

// fakeClaudeCall is what one fake claude invocation received.
type fakeClaudeCall struct {
	systemFile string // --append-system-prompt-file content ("" if not passed)
	prompt     string // final positional argument
	resumed    bool   // --resume was passed
}

// newFakeClaude writes a shell script standing in for the claude binary. Each
// invocation records the append-file content and the prompt into dir, then
// emits a minimal stream-json result.
func newFakeClaude(t *testing.T) (bin string, calls func() []fakeClaudeCall) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a POSIX shell script")
	}
	dir := t.TempDir()
	bin = filepath.Join(dir, "claude")
	script := `#!/bin/sh
n=$(ls "` + dir + `" | grep -c '^call-')
d="` + dir + `/call-$n"
mkdir "$d"
prev=""
for a in "$@"; do
  if [ "$prev" = "--append-system-prompt-file" ]; then cat "$a" > "$d/system"; fi
  if [ "$a" = "--resume" ]; then touch "$d/resumed"; fi
  prev="$a"
done
printf '%s' "$prev" > "$d/prompt"
echo '{"type":"result","subtype":"success","result":"ok","session_id":"sid-1"}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	calls = func() []fakeClaudeCall {
		var out []fakeClaudeCall
		for i := 0; ; i++ {
			d := filepath.Join(dir, "call-"+strconv.Itoa(i))
			if _, err := os.Stat(d); err != nil {
				return out
			}
			sys, _ := os.ReadFile(filepath.Join(d, "system"))
			prompt, _ := os.ReadFile(filepath.Join(d, "prompt"))
			_, resumedErr := os.Stat(filepath.Join(d, "resumed"))
			out = append(out, fakeClaudeCall{systemFile: string(sys), prompt: string(prompt), resumed: resumedErr == nil})
		}
	}
	return bin, calls
}

// noInput satisfies cliRunner.newInput; the fake claude never reports denials.
func noInput() inputReader { return nil }

func runFakeCLITurn(t *testing.T, r *cliRunner, sess *session.Session, mode escalation.ContextMode, sessionID string, inject bool, prompt string) {
	t.Helper()
	_, err := r.Execute(context.Background(), config.Config{}, sess, nil, RoleEscalation, mode,
		sessionID, "nonce1", nil, inject, prompt, TurnCallbacks{}, io.Discard)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestCLIResume_TurnContextGoesInPrompt(t *testing.T) {
	bin, calls := newFakeClaude(t)
	var hash string
	r := newCLIRunner(claude.NewWithOpts(bin, true, nil, nil), "claude", permContext{contextHash: &hash}, noInput)
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	sess.LastLocalSummary = "primary ran the tests: 3 failures in parser_test.go"

	// Continuation with a new primary summary, no instruction re-injection.
	runFakeCLITurn(t, r, sess, escalation.ContextModeContinuation, "sid-1", false, "fix them")
	// Next continuation: nothing new to deliver.
	runFakeCLITurn(t, r, sess, escalation.ContextModeContinuation, "sid-1", false, "thanks")

	got := calls()
	if len(got) != 2 {
		t.Fatalf("got %d claude calls, want 2", len(got))
	}
	first, second := got[0], got[1]
	if !first.resumed || !second.resumed {
		t.Fatalf("both turns must use --resume")
	}

	// The per-turn summary reaches Claude via the prompt, ahead of the user's text.
	if !strings.HasPrefix(first.prompt, "<milk-context>\n") || !strings.HasSuffix(first.prompt, "</milk-context>\n\nfix them") {
		t.Errorf("first prompt not wrapped in a milk-context block:\n%s", first.prompt)
	}
	if !strings.Contains(first.prompt, "3 failures in parser_test.go") {
		t.Errorf("first prompt missing the primary summary:\n%s", first.prompt)
	}
	// Identity-only static is already in the recorded system prompt: not repeated in the message.
	if strings.Contains(first.prompt, "[Milk agent context]") {
		t.Errorf("identity block should not be repeated in the prompt when not re-injecting:\n%s", first.prompt)
	}
	// Nothing new on the second turn → the prompt is exactly what the user typed.
	if second.prompt != "thanks" {
		t.Errorf("second prompt = %q, want %q", second.prompt, "thanks")
	}

	// The file always carries the full stable block, so a compaction re-records it.
	for i, c := range got {
		if !strings.Contains(c.systemFile, "[Milk agent context]") || !strings.Contains(c.systemFile, "nonce1") {
			t.Errorf("call %d: system file missing full static block (identity + nonce instructions):\n%s", i, c.systemFile)
		}
		if strings.Contains(c.systemFile, "3 failures") {
			t.Errorf("call %d: per-turn summary must not go to the (ignored) system file", i)
		}
	}
}

func TestCLIResume_ReturningDeliversInstructionsAndBrief(t *testing.T) {
	bin, calls := newFakeClaude(t)
	var hash string
	r := newCLIRunner(claude.NewWithOpts(bin, true, nil, nil), "claude", permContext{contextHash: &hash}, noInput)
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	sess.EscalationBrief = "refactor the router scorer"
	sess.LastLocalSummary = "primary renamed Score to Weight"

	runFakeCLITurn(t, r, sess, escalation.ContextModeReturning, "sid-1", true, "continue")

	got := calls()
	if len(got) != 1 || !got[0].resumed {
		t.Fatalf("want one resumed call, got %+v", got)
	}
	p := got[0].prompt
	for _, want := range []string{"refactor the router scorer", "primary renamed Score to Weight", "nonce1"} {
		if !strings.Contains(p, want) {
			t.Errorf("returning prompt missing %q:\n%s", want, p)
		}
	}
	if !strings.HasSuffix(p, "\n\ncontinue") {
		t.Errorf("user text must follow the context block:\n%s", p)
	}
}

func TestCLIFirst_ContextStaysInSystemFileEvenAfterIdenticalHash(t *testing.T) {
	bin, calls := newFakeClaude(t)
	var hash string
	r := newCLIRunner(claude.NewWithOpts(bin, true, nil, nil), "claude", permContext{contextHash: &hash}, noInput)
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	sess.EscalationBrief = "same brief"

	// Two fresh sessions with identical context: each must get it (a new session
	// has nothing recorded yet), and the prompt stays untouched.
	runFakeCLITurn(t, r, sess, escalation.ContextModeFirst, "", true, "go")
	runFakeCLITurn(t, r, sess, escalation.ContextModeFirst, "", true, "go")

	got := calls()
	if len(got) != 2 {
		t.Fatalf("got %d calls, want 2", len(got))
	}
	for i, c := range got {
		if c.resumed {
			t.Errorf("call %d: first-mode turn must not resume", i)
		}
		if c.prompt != "go" {
			t.Errorf("call %d: prompt = %q, want %q", i, c.prompt, "go")
		}
		if !strings.Contains(c.systemFile, "same brief") {
			t.Errorf("call %d: new session missing its context in the system file:\n%s", i, c.systemFile)
		}
	}
}

func TestWithTurnContext(t *testing.T) {
	if got := claude.WithTurnContext("  \n", "hi"); got != "hi" {
		t.Errorf("empty context: got %q", got)
	}
	got := claude.WithTurnContext("ctx line", "hi")
	want := "<milk-context>\n[Context from milk for this turn — not typed by the user. The user's message follows the closing tag.]\nctx line\n</milk-context>\n\nhi"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}
