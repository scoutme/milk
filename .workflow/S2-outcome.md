# S2 Outcome: Zero lastEscalationContextHash on ForceFreshEscalation

## What was changed and why

In `cmd/milk/repl.go`, a `pendingForceFresh bool` field was added to the `model` struct. In `dispatchAgent`, this field is set to `m.st.sess.ForceFreshEscalation` immediately before launching the agent goroutine — capturing the flag while it is still true, before `dispatch.go` consumes and clears it during the turn. In `handleAgentDone`, if `pendingForceFresh` is true, `m.st.lastEscalationContextHash` is zeroed and `pendingForceFresh` is cleared. This ensures that after a `/escalate fresh` turn, the stale hash from the previous session cannot suppress context injection on the new session's first real turn; the next context block will always be compared against an empty baseline and written.

## Test results

- `go build ./...` — clean
- `go test ./cmd/milk/...` — ok (0.077s)
- `go test ./...` — all 25 test packages pass (19 run + 6 no test files), no failures

## Edge cases and invariants for the evaluator

- **Timing of capture**: `pendingForceFresh` is read from `sess.ForceFreshEscalation` in `dispatchAgent` synchronously, before the goroutine starts. By the time `agentDoneMsg` arrives, `dispatch.go` has already cleared `sess.ForceFreshEscalation`, so the TUI model's `pendingForceFresh` is the only surviving record of the flag for this turn.
- **Hash zeroed after the turn, not before**: zeroing in `handleAgentDone` means the fresh turn itself runs against the old (non-zero) hash. This is correct: the turn's new context was already computed by `dispatch.go` using `ContextModeFirst` (which ignores the hash check in `runner.go`). The zeroing prepares the baseline for the *next* turn so that turn's context is always injected afresh and then cached normally.
- **Non-escalation turns**: `pendingForceFresh` is set for every dispatch, but `ForceFreshEscalation` is only true when `/escalate fresh` was invoked; for primary-agent turns it will be false and the handler is a no-op.
- **Interaction with S1**: S1 made hash suppression apply to all ContextModes. Without S2, the first post-fresh turn could still be suppressed if the new session's combined context happened to match the hash stored by a prior same-topic escalation. S2 eliminates that risk by zeroing the stored hash before the post-fresh turn runs its own context check.
