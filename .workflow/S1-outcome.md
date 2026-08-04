# S1 Outcome: Extend contextHash suppression to all ContextModes

## What was changed and why

In `cmd/milk/runner.go`, the context-hash suppression block inside `cliRunner.Execute` was updated in two ways: (1) the hash is now computed over `staticCtx + dynamicCtx` (the full combined content) rather than `dynamicCtx` alone, so a change to either half still produces a new hash and triggers a write; (2) the `ctxMode == escalation.ContextModeResume` guard was removed so suppression fires for every ContextMode, not just Resume. When the hash matches the stored value, both `staticCtx` and `dynamicCtx` are zeroed before being passed to `RunResume`/`RunFirst`. This fixes the primary cache-miss regression in sticky-escalation mode: after ROUTING resets the state, subsequent turns re-enter `ContextModeReturning` rather than `ContextModeResume`, causing the old guard to never fire and redundant context files to be sent on every turn.

## Test results

All tests pass:
- `go build ./...` — clean
- `go test ./cmd/milk/...` — ok (0.072s)
- `go test ./...` — all 19 test packages pass, no failures

## Edge cases and invariants for the evaluator

- **First turn**: `contextHash` starts as `""` (zero value of string); on the first turn the hash will never equal `""` (SHA-256 hex is non-empty), so the initial context block is always injected. After that the hash is stored and suppression can fire.
- **`/escalate fresh`**: S2 is responsible for zeroing `lastEscalationContextHash` when `EscalationNonce` changes. Without S2, a fresh escalation that happens to produce the same combined content could be incorrectly suppressed. The current change is intentionally scoped to S1 — S2 addresses the zero-on-fresh case.
- **`staticCtx = ""`**: when `contextHash` suppression fires and zeroes both fields, passing an empty `staticCtx` to `RunResume` is safe — the CLI runner already passed `""` for `staticCtx` on plain resume turns before this change; the suppression just extends that to all modes.
- **Hash collision**: the truncated 16-hex-char prefix (64-bit) is sufficient to avoid accidental suppression in practice; the cost of a false positive (one missed injection) is low and self-correcting on the next distinct turn.
