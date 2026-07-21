# Issue: Silent Turn Interruptions

Session turns end silently — no error message, no `[interrupted]` label — without any user interaction. Affects both the local HTTP agent and the Claude CLI agent.

---

## Root causes confirmed

### RC-1: handleAgentDone gap (repl.go:589–619)

**File:** `cmd/milk/repl.go`, function `handleAgentDone`

**Code path:**

```go
// repl.go:589
if m.interrupted {
    m.interrupted = false
    m.appendTranscript(dim("[interrupted]") + "\n")
} else if msg.err != nil {
    // ... error handling ...
}
// repl.go:619
m.appendTranscript("\n")   // ← only a bare newline when err=nil AND !interrupted
```

When `m.interrupted == false` AND `msg.err == nil`, the block appends exactly one bare `"\n"` and nothing else. There is no branch that handles the case where the agent produced zero visible output. `currentTurnChars` (repl.go:378) counts every byte from `chunkMsg` and `thinkChunkMsg` handlers (lines 965, 970) and is reset to 0 at the start of every turn (line 1459); `prefixChunkMsg` (the "claude: " prefix line) deliberately does **not** increment it. So after a turn where only the prefix was emitted and the agent produced nothing, `m.currentTurnChars == 0` upon reaching `handleAgentDone`. The TUI shows a blank line; the user sees nothing.

This is the shared, final-mile gap for **all** agent types. Both of the lower-level root causes below ultimately surface here.

---

### RC-2: runPipe swallows non-zero exit codes (claude.go:444–462)

**File:** `internal/agent/claude/claude.go`, function `runPipe`

**Code path:**

```go
// claude.go:444
if err := cmd.Wait(); err != nil {
    stderr := filterKnownWarnings(strings.TrimSpace(stderrBuf.String()))
    if stderr != "" {
        return res, fmt.Errorf("claude exited with error: %s", stderr)
    }
    if parseErr != nil {
        return res, parseErr
    }
    if res.IsError {
        return res, fmt.Errorf("claude returned an error response")
    }
    if ctxErr := ctx.Err(); ctxErr != nil {
        return res, ctxErr
    }
    return res, nil   // ← error SWALLOWED
}
```

When:
1. `cmd.Wait()` returns non-nil (process exited non-zero — crash, OOM, transient failure)
2. `filterKnownWarnings` reduces stderr to empty (the "no stdin data received" filter may eat entire error messages if the real error is on that line, or real stderr is absent)
3. `parseErr == nil` (NDJSON stream ended cleanly before the crash)
4. `res.IsError == false` (no `is_error` result event was emitted)
5. `ctx.Err() == nil` (the crash was not caused by user Ctrl+C or turn timeout)

…then `runPipe` returns `(res, nil)`. `res.Text` may be empty or partial. The error is silently discarded. `agentDoneMsg{err: nil}` reaches the TUI. RC-1 then produces a bare newline.

The missing branch is the final `return res, nil` (line 462). There is no code path that turns "non-zero exit with no other diagnostic information" into an error.

---

### RC-3: Local agent treats empty SSE stream as success (local.go:681–690)

**File:** `internal/agent/local/local.go`, function `Run` (tool loop)

**Code path:**

```go
// local.go:681
resp, fallbackRaw, toolCalls, err := a.streamCompletion(ctx, msgs, tools, out)
if err != nil {
    return msgs, err
}

if len(toolCalls) == 0 {
    // "either a final text response, or the model emitting EOS … Both are terminal."
    msgs = append(msgs, Message{Role: "assistant", Content: resp})
    return msgs, nil   // resp == "" when the SSE stream was empty
}
```

`streamCompletion` (local.go:1022) routes to `scanSSE` (line 1085). If the inference server sends `[DONE]` with no content chunks, or closes the connection cleanly (io.EOF) without sending `[DONE]`, the scanner exits with `scanner.Err() == nil` and an empty `textBuf`. `classifyStreamResult` (line 1108) returns `("", "", nil, nil)` — no error. Back in `Run`, `resp == ""`, `toolCalls == nil`, so the function returns `(msgs, nil)` with an empty assistant message appended.

`localRunner.Execute` (runner.go:212–219) then builds `TurnResult{Text: ""}` with no error. `runPrimary` (dispatch.go:113) adds the empty-content assistant turn to session history (condition `res.Text != "" || res.EscalationReason == ""` evaluates true when there is no escalation). `agentDoneMsg{err: nil}` reaches the TUI. RC-1 produces a bare newline.

---

## Root causes refuted

### H4: Stream scan race on context cancellation

**Refuted.** For the **local HTTP agent**: when the HTTP transport cancels the request (context cancelled), reading `httpResp.Body` returns a non-nil error that propagates through `bufio.Scanner.Err()` and is surfaced by `scanSSE`. For the **Claude CLI agent**: `exec.CommandContext` cancels the child context synchronously before sending the kill signal; `ctx.Err()` returns `context.Canceled` by the time `cmd.Wait()` returns. Furthermore, Ctrl+C sets `m.interrupted = true` before cancelling the context (repl.go:486–490), so even if a tiny race allowed `ctx.Err() == nil` at `cmd.Wait()`, `handleAgentDone` would still print `[interrupted]` via the `m.interrupted` branch. This hypothesis is not an independent root cause.

---

## Fixes

### Fix 1 — handleAgentDone: show `[no response]` when nothing was streamed (RC-1)

**File:** `cmd/milk/repl.go`  
**Line range changed:** 589–603

**Before:**
```go
	if m.interrupted {
		m.interrupted = false
		m.appendTranscript(dim("[interrupted]") + "\n")
	} else if msg.err != nil {
		errText := msg.err.Error()
		switch {
		case isContextCanceled(msg.err):
			m.appendTranscript(dim("[interrupted]") + "\n")
		case isContextDeadlineExceeded(msg.err):
			m.appendTranscript(milkTag() + " turn timed out — the agent did not respond in time\n")
		default:
			m.appendTranscript(milkTag() + " error: " + errText + "\n")
		}
	}
```

**After:**
```go
	if m.interrupted {
		m.interrupted = false
		m.appendTranscript(dim("[interrupted]") + "\n")
	} else if msg.err != nil {
		errText := msg.err.Error()
		switch {
		case isContextCanceled(msg.err):
			m.appendTranscript(dim("[interrupted]") + "\n")
		case isContextDeadlineExceeded(msg.err):
			m.appendTranscript(milkTag() + " turn timed out — the agent did not respond in time\n")
		default:
			m.appendTranscript(milkTag() + " error: " + errText + "\n")
		}
	} else if m.currentTurnChars == 0 {
		// No text or thinking was streamed and no error was reported — the agent
		// produced no visible output. Show a placeholder so the turn is not silent.
		m.appendTranscript(dim("[no response]") + "\n")
	}
```

**Rationale:** `currentTurnChars` is reset to 0 at the start of every turn (line 1459) and incremented only by `chunkMsg` and `thinkChunkMsg` (lines 965, 970). The agent-name prefix is sent as `prefixChunkMsg` and does not count. So `currentTurnChars == 0` at turn-end means the agent emitted no visible content. This is the correct invariant: it catches empty SSE streams (RC-3), Claude CLI crashes producing no output (RC-2 after its own fix), and any future silent-exit paths, without changing the display for tool-only turns (tool hints are written to `out` which routes through `chunkMsg`, incrementing `currentTurnChars`).

---

### Fix 2 — runPipe: surface non-zero exit as an error (RC-2)

**File:** `internal/agent/claude/claude.go`  
**Line range changed:** 457–462

**Before:**
```go
		// Process was killed because the context was cancelled (e.g. user Ctrl+C
		// or turn timeout). Surface the context error so callers can distinguish
		// a clean cancellation from a successful empty turn.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return res, ctxErr
		}
		return res, nil
```

**After:**
```go
		// Process was killed because the context was cancelled (e.g. user Ctrl+C
		// or turn timeout). Surface the context error so callers can distinguish
		// a clean cancellation from a successful empty turn.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return res, ctxErr
		}
		// Process exited non-zero with no stderr and no parse/API error.
		// This is an unexpected crash (OOM, signal, bug). Surface it so the TUI
		// shows an error label rather than a silent blank turn.
		return res, fmt.Errorf("claude exited unexpectedly (no output)")
```

**Rationale:** After the context-cancellation check, the only remaining scenario is a process crash not caused by user action: OOM kill, SIGSEGV in the claude binary, etc. Returning `nil` here discards the diagnostic signal; returning an error lets the existing error branch in `handleAgentDone` print `milk error: claude exited unexpectedly (no output)`, which is far more actionable than silence.

---

### Fix 3 — dispatch.go: skip empty-content assistant turns (RC-3, session history)

**Files:** `cmd/milk/dispatch.go` — both `runPrimary` (~line 113) and `runEscalation` (~line 262)

#### runPrimary

**Before:**
```go
	// For local HTTP runners, text is only set when a real response came back.
	// Only add the assistant turn when there is content (mirrors runLocal behaviour).
	if res.Text != "" || res.EscalationReason == "" {
		sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentLocal, Content: res.Text})
		sess.RebuildSummaryBricks(cfg.AgentContextBudget(ac))
	}
```

**After:**
```go
	// For local HTTP runners, text is only set when a real response came back.
	// Only add the assistant turn when there is content: skip both self-escalation
	// (res.EscalationReason != "") and truly empty responses (res.Text == "").
	// The previous condition `res.Text != "" || res.EscalationReason == ""` was
	// incorrect — it evaluated to true when both were empty (false || true), causing
	// a blank assistant turn to be written to session history.
	if res.Text != "" {
		sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentLocal, Content: res.Text})
		sess.RebuildSummaryBricks(cfg.AgentContextBudget(ac))
	}
```

**Rationale:** The old condition `res.Text != "" || res.EscalationReason == ""` was a display-only fix for RC-3: it correctly suppressed the turn for self-escalating responses (`res.EscalationReason != ""`), but failed for the RC-3 case where `res.Text == ""` AND `res.EscalationReason == ""` — `false || true` evaluated to `true`, so a blank-content assistant turn was written to session history. On the next turn the local model would see `{"role": "assistant", "content": ""}` in its context, which is confusing and may cause the model to stutter or repeat.

The correct guard is simply `res.Text != ""`. From `runner.go`, the local runner returns `TurnResult{EscalationReason: esc.Reason}` (empty Text) for self-escalation and `TurnResult{Text: text}` otherwise. Tool-only turns: after the local agent processes tool calls it always emits a final assistant message with summary text (`last.Role == "assistant"` in runner.go:215); `text` will be non-empty. So there is no legitimate case where `res.Text == ""` and `res.EscalationReason == ""` should result in an assistant turn being recorded.

#### runEscalation

**Before (line ~262):**
```go
	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentEscalation, Content: res.Text})
	sess.RebuildSummaryBricks(cfg.AgentContextBudget(escAC))
	if res.Text != "" && cbs.OnResponse != nil {
		cbs.OnResponse(res.Text)
	}
```

**After:**
```go
	// Only persist the assistant turn when there is actual content.
	// A local-HTTP escalation agent with an empty SSE stream returns res.Text == ""
	// with no error; writing a blank turn to history corrupts the session context.
	// For CLI agents: if Claude finished with only tool calls and no closing text,
	// res.Text is also empty — skipping the AddTurn is safe because the escalation
	// agent manages its own conversation state via --resume; milk's local history
	// copy is only used for context handoff back to the primary, where a blank entry
	// is more harmful than a missing one.
	if res.Text != "" {
		sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentEscalation, Content: res.Text})
		sess.RebuildSummaryBricks(cfg.AgentContextBudget(escAC))
		if cbs.OnResponse != nil {
			cbs.OnResponse(res.Text)
		}
	}
```

**Rationale:** The original code was unconditional — it always wrote the assistant turn regardless of whether `res.Text` was empty, while the `cbs.OnResponse` call below it already had a `res.Text != ""` guard. This asymmetry meant an empty-stream escalation (RC-3 via a local-HTTP escalation agent) would still corrupt session history with a blank-content turn. For CLI agents (`provider: "claude-cli"`), `ParseResult.Text` is the final assistant text after all tool calls complete; it can legitimately be empty if Claude used only tools with no closing prose. Suppressing the `AddTurn` in that case is safe: the escalation agent maintains its own canonical session state via `--resume`; milk's session history copy is only used to build context for the primary agent, and a blank assistant entry there is more harmful than a missing one.

The `cbs.OnResponse` call is moved inside the guard so it is co-located with the `AddTurn`; there is no point invoking it when there is nothing to display.

---

## How to test

### Reproduce RC-1 + RC-3 (local agent empty response)

1. Start milk with a local inference server.
2. Configure the server to return an empty SSE stream for the next request (or temporarily kill the server process after it starts responding but before it sends any content tokens).
3. Send any prompt in the TUI.
4. **Before fix:** turn ends with a bare blank line — no label, no error.
5. **After fix:** `[no response]` appears in dim text after the agent-name prefix.

### Reproduce RC-1 + RC-2 (Claude CLI crash)

1. Set the Claude CLI path to a wrapper script that exits 1 with no output: `#!/bin/sh\nexit 1`
2. Send any prompt in the TUI using the escalation agent path.
3. **Before fix:** turn ends silently with only a blank line.
4. **After fix (both fixes applied):** `milk error: claude exited unexpectedly (no output)` appears.

### Confirm Ctrl+C still shows `[interrupted]`

1. Send a prompt that takes several seconds (e.g., a slow tool call).
2. Press Ctrl+C during the turn.
3. **Expected (unchanged):** `[interrupted]` appears, not `[no response]`.
4. `m.interrupted` is set to `true` before `currentTurnChars` can matter; the `m.interrupted` branch fires first.

### Confirm normal turns are unaffected

1. Send any prompt that produces real output.
2. `currentTurnChars > 0` after streaming → `[no response]` branch is skipped.
3. Turn ends with a bare newline as before.
