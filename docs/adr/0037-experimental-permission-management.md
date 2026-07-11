# ADR 0037 — Experimental permission management

## Status

Accepted

## Context

When the Claude CLI subprocess encounters a tool or directory it has not been granted
permission for, it emits a `tool_result` with `is_error:true` and content `"Stream closed"`
before the interactive `--permission-prompt-tool stdio` handler is active (a pre-flight
directory-trust check). Milk already detects these via `ParseResult.StreamClosedDenials`
(added in ADR 0036) and retries the turn via `handleStreamClosedDenials` after the user
grants the permission.

The problem is that Claude does not know it should stop and wait. Without guidance it
typically attempts workarounds, produces confused partial output, or continues the task
in a degraded way — and only then does milk's retry fire on whatever `StreamClosedDenials`
were accumulated at turn end.

## Decision

Add `ExperimentalPermissionManagement bool` to `Config`
(`json:"experimental_permission_management,omitempty"`). When true, a fixed instruction
block (`permissionManagementInstruction`) is appended to the static context passed to
every `cliRunner.Execute` call.

The instruction tells Claude:

> If a tool call returns an error with content that includes "Stream closed", do not
> retry the tool or attempt workarounds. Output one short message announcing the pause,
> then end your turn immediately. Milk will grant the permission and resume your task.

The existing `handleStreamClosedDenials` retry path is unchanged — it remains the
actual mechanism that prompts the user and calls `RunResume`. The instruction changes
only Claude's *behaviour within the turn*: graceful early termination instead of noise.

### Why system-prompt injection, not an early-termination hook

An early-termination hook would require detecting the denial mid-stream (before
`Stream()` returns), killing the subprocess, and re-entering the permission flow from
outside `cliRunner.Execute`. That would need a new callback in `StreamOpts`, a sentinel
error type from `cliRunner.Execute`, and a retry loop in `runEscalation` — significant
plumbing for what is ultimately a Claude behaviour problem. The instruction approach
is one config field plus one constant; the existing retry path is the safety net if
Claude ignores the instruction.

### Why experimental

- We cannot unit-test Claude's instruction-following for this scenario without a live
  permission gate; correctness is observable only at runtime.
- The instruction adds tokens to every claude-cli turn when enabled — users who never
  encounter stream-closed denials pay the cost unnecessarily.
- The feature is expected to graduate to default-on once field-tested, with possible
  refinement of the instruction wording.

### Injection point

`staticCtx` in `cliRunner.Execute` (`cmd/milk/runner.go`), after
`escalation.BuildStaticContext`. The static context is written to a temp file and
passed via `--append-system-prompt-file`. Appending to `staticCtx` (not `dynamicCtx`)
ensures the instruction lands in the cached prefix and does not invalidate the cache on
every turn.

## Consequences

- No changes to `handleStreamClosedDenials`, `runEscalation`, or the stream parser.
- `ExperimentalPermissionManagement: false` (default) — zero runtime impact.
- When enabled: one extra paragraph in the system prompt per escalation turn.
- Future phase: if the instruction proves reliable, the feature can be made default-on
  and the flag removed.
