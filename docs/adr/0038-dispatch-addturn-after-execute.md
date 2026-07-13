# ADR 0038 — Move user-turn AddTurn to after Execute

**Status:** Proposed  
**Date:** 2026-07-13

---

## Context

`runPrimary` and `runEscalation` in `dispatch.go` both call `sess.AddTurn(user turn)` **before** calling `runner.Execute()`. `local.Agent.Run` then appends `userPrompt` to the inference message slice itself (at `local.go:658`), because `Run` treats `history` as prior turns only and owns the wire-format assembly.

This two-owner contract forces every function that converts `sess.History` to agent messages to defensively strip the trailing user turn before passing it to `Run`. There are currently four such sites:

| Site | File | Type |
|---|---|---|
| `sessionToMessages` | `cmd/milk/main.go:1346` | Unconditional trailing-user drop |
| `escalationLocalHistory` | `cmd/milk/main.go:703` | Conditional: only if content == prompt |
| `escalationLocalHistoryFresh` | `cmd/milk/main.go:728` | Same; sliced from `LastEscalationBoundary` |
| `isRepeatedPrompt` (indirect) | `internal/agent/local/local.go:610` | Depends on callers having stripped |

The unconditional strip in `sessionToMessages` and the conditional strip in the escalation builders are subtly different contracts — a latent inconsistency that could produce a bug if the session history ever had a legitimate unanswered user turn at the tail (e.g. from a crash recovery path or a future feature).

## Decision

Move `sess.AddTurn(user turn)` in both `runPrimary` and `runEscalation` to **after** `runner.Execute()` returns successfully, symmetric with how `sess.AddTurn(assistant turn)` is already recorded. Remove all four strip sites once the invariant is gone.

The single owner of "add user turn to session" becomes dispatch, after the turn completes. `Run` continues to own "add user turn to the wire-format slice" via its `userPrompt` parameter — these are now non-overlapping responsibilities.

## Consequences

**Positive:**
- Eliminates the implicit contract that every history-builder must know about the pre-add
- Makes `sessionToMessages`, `escalationLocalHistory`, and `escalationLocalHistoryFresh` straightforward iteration with no special tail logic
- `isRepeatedPrompt` remains correct without depending on callers stripping
- Session state on crash: a user turn that never completed will no longer appear in session history. This is acceptable — the turn was never answered; re-sending it on resume is the correct behavior, not replaying a ghost turn.

**Negative / risks:**
- Any code that reads `sess.History` *during* a turn (i.e. inside `Execute`) and expects the current user turn to be present will break. This must be audited before the change. Currently known reads during Execute: `escalation/builder.go` (uses `len(sess.History)` for `turnsAgo` arithmetic), `tools.go` (`ReadHistory` tool, `read_history` / `list_sessions`). Both must be verified.
- The `turnsAgo` calculation in `escalation/builder.go` uses `len(sess.History)` relative to `CurrentNeedSetAt`. If the current user turn is no longer in history during `Execute`, the count will be off by one. This is a **known side-effect** that must be corrected as part of the refactor (add +1 to the affected calculations, or record the turn before building the escalation context but after building the history slice).

---

## Refactoring Plan

### Step 0 — Regression test baseline (before touching any production code)

Write table-driven unit tests that pin the exact message sequences sent to `agent.Run` for each history builder:

1. **`TestSessionToMessages`** — seed `sess.History` with N local-agent turns (user+assistant pairs) plus a trailing pre-added user turn. Assert the returned slice ends with the last assistant turn (no trailing user). After the refactor: assert the slice includes all turns including the most recent assistant, with no stripping needed.

2. **`TestEscalationLocalHistory_Strip`** — seed a mixed-agent history, add a trailing user turn matching `prompt`. Assert it is stripped. Test the non-matching case: trailing user turn with different content is *not* stripped (this is the current conditional behavior). After refactor: neither case strips.

3. **`TestEscalationLocalHistoryFresh_Strip`** — same as above but with a boundary offset via a fake `LastEscalationBoundary`.

4. **`TestIsRepeatedPrompt_NoPendingTurn`** — verify `isRepeatedPrompt` does not fire when `history` contains only answered turns (i.e. no trailing unanswered user turn). This documents the dependency on callers stripping; after the refactor it becomes the only contract.

5. **`TestRunPrimary_UserTurnRecordedAfterComplete`** (integration-style, using `evalCaptureServer` pattern from the existing eval tests) — drive a full `runPrimary` turn through a mock HTTP server, assert `sess.History` does not contain the user turn until after `Execute` returns. After the refactor: assert it is recorded exactly once and only after Execute.

6. **`TestTurnsAgo_OffByOne`** — pin the `turnsAgo` values computed by `escalation/builder.go` for a known session shape, so the builder fix in Step 3 can be verified.

All tests should be written to **pass against the current code first** (pinning the pre-refactor behavior), then updated to pass against the refactored code. Keep both versions in a commit boundary.

### Step 1 — Fix `escalation/builder.go` turnsAgo (pre-work)

`builder.go` calculates `turnsAgo = len(sess.History) - (sess.CurrentNeedSetAt - 1)`. After the move, `len(sess.History)` during Execute will be one less (the current user turn has not been added yet). Add a `+1` to the relevant `len(sess.History)` expressions, or extract a helper `func sessionLenDuringExecute(sess)` that adds the implicit pending turn. Verify with `TestTurnsAgo_OffByOne`.

### Step 2 — Audit all reads of sess.History inside Execute

Search for every `sess.History` access that occurs on a code path reachable from `runner.Execute`. For each one, determine whether it expects the current user turn to be present:

- `escalation/builder.go` — **affected** (see Step 1)
- `internal/agent/local/tools.go` — `ReadHistory` / `list_sessions` tool: presents history to the model. The current user turn being absent is arguably *correct* behavior (the model shouldn't see a turn it's still in the middle of). Mark as acceptable, document in a comment.
- `repl.go:579` — walks history to attach Thinking to the last assistant turn. This occurs after Execute returns, so unaffected.

### Step 3 — Move AddTurn in dispatch.go

In `runPrimary`:
- Remove `sess.AddTurn(user turn)` at line 54
- Add it after the `err != nil` guard at line 94, before `res.NewSessionID` handling. The existing assistant-turn AddTurn at line 113 moves to just below it.

In `runEscalation`:
- Remove `sess.AddTurn(user turn)` at line 210
- Add it after the `err != nil` guard at line 238, before `res.NewSessionID` handling.

No change to the assistant-turn AddTurn calls (lines 113, 256) — these are already post-Execute and stay where they are.

### Step 4 — Remove the four strip sites

In `sessionToMessages` (`main.go`): delete the trailing-user strip block (lines 1343–1348).

In `escalationLocalHistory` (`main.go`): delete the conditional strip block (lines 700–705).

In `escalationLocalHistoryFresh` (`main.go`): delete the conditional strip block (lines 728–730). If the two functions are now identical except for the input slice, consider merging them into one with a `start int` parameter.

`isRepeatedPrompt` (`local.go`): no code change needed. Remove the comment that says callers must strip before calling; replace with a comment stating the invariant: `history` must not contain the current user turn (it hasn't been added yet when Run is called).

### Step 5 — Run full test suite and eval tests

```
go test ./...
go test ./cmd/milk/ -run TestEval_EscalationLocal -v
go test ./cmd/milk/ -run TestEval_EscalationCLI -v
```

All six new regression tests from Step 0 should pass. All existing tests should pass unchanged.

### Step 6 — Merge `escalationLocalHistory` and `escalationLocalHistoryFresh` (optional cleanup)

After Step 4, both functions iterate `sess.History[start:]` with the same loop body and return the same type. Merge into:

```go
func buildEscalationHistory(sess *session.Session, start int) []local.Message
```

Call sites in `runner.go:158` and `runner.go:160` become:

```go
history = buildEscalationHistory(sess, session.LastEscalationBoundary(sess)) // fresh
history = buildEscalationHistory(sess, 0)                                     // full
```

This is purely cosmetic and can be its own commit.

---

## Commit sequence

```
refactor(dispatch): add regression tests for history-builder strip sites   ← Step 0
fix(escalation): correct turnsAgo len(sess.History) off-by-one             ← Step 1+2
refactor(dispatch): move user-turn AddTurn to after Execute returns        ← Step 3
refactor(dispatch): remove history-builder strip sites                     ← Step 4
refactor(dispatch): merge escalationLocalHistory into buildEscalationHistory ← Step 6 (optional)
```

Steps 3 and 4 should land in the same commit so the codebase is never in a state where strips are removed but AddTurn is still pre-execute (which would send the user turn twice).
