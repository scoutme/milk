# S3 Outcome: Add ContextModeContinuation for uninterrupted sticky turns

## What was changed and why

A fourth `ContextMode` value, `ContextModeContinuation`, was added to `internal/escalation/builder.go` to represent the case where the escalation agent is already active and the user types the next message directly — i.e., `EscalationSessionID != ""` and `LocalTurnsSinceLastEscalation() == 0`. `BuildStaticContext` always returns `""` for this mode (overriding even `injectInstructions=true`), and `BuildDynamicContext` returns only the summary-diff block, identical to `ContextModeResume`. In `BuildContext` (the legacy path), `ContextModeContinuation` is treated the same as `ContextModeResume`: dynamic-only output. In `cmd/milk/dispatch.go`, a new case was inserted in the context-mode switch — after `ContextModeResume` and before the generic `ContextModeReturning` arm — that selects `ContextModeContinuation` when the session has an escalation ID and no local turns have run since the last escalation turn. `resuming` is also set to `true` for `ContextModeContinuation` so that `shouldInjectMemoryInstructions` applies threshold-based re-injection rather than always injecting. In `cmd/milk/runner.go`, both the CLI and subprocess `RunResume` branches were updated to treat `ContextModeContinuation` as a resume turn (using `--resume` / `RunResume`), since by definition the session ID is valid.

## Test results

- `go build ./...` — clean
- `go test ./internal/escalation/...` — ok (0.004s)
- `go test ./cmd/milk/...` — ok (0.075s)
- `go test ./...` — all 19 run packages pass, 6 no-test-files packages skipped, no failures

## Edge cases and invariants for the evaluator

- **ContextModeResume takes priority**: `ContextModeContinuation` is only reached when `sess.State != StateEscalationWaiting`. If the escalation agent asked a question, the state is `ESCALATION_WAITING` and the existing `ContextModeResume` arm fires first.
- **Local-HTTP agents without session IDs**: the new arm checks `sess.EscalationSessionID != ""` explicitly, so local-HTTP escalation agents (which never set a session ID) fall through to `ContextModeReturning` unchanged. The design intent is for `ContextModeContinuation` to apply to CLI/subprocess agents where `--resume` is meaningful.
- **BuildStaticContext unconditional suppression**: the `ContextModeContinuation` guard fires before the `injectInstructions` check, so even a threshold-crossed re-injection turn cannot inject the static block in continuation mode. This is intentional: in continuation mode Claude already holds the static instructions in its cached context from the previous turn in the same session.
- **NeedChangedSinceLastEscalation and turnGap checks**: the existing `ContextModeReturning → ContextModeFirst` upgrade block is not extended to `ContextModeContinuation`. When `LocalTurnsSinceLastEscalation() == 0`, there have been no local turns, so `needStale` cannot be true (any need set by the escalation agent itself registers as "after last escalation turn" and returns false). The `turnGap` threshold is irrelevant when zero local turns have passed.
- **Interaction with S1 hash suppression**: S1 hashes `staticCtx + dynamicCtx` and zeros both on a match. For continuation turns, `staticCtx` is already `""` and `dynamicCtx` is only the summary diff (or `""`). This gives the hash mechanism a very small target to suppress, and any change in the summary will naturally differ.
