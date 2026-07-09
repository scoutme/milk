# ADR-0035: Stream-closed pre-flight denial detection and retry

Date: 2026-07-10

## Status

Accepted

## Context

ADR-0013 introduced `--permission-prompt-tool stdio`, which emits a `control_request` NDJSON event when Claude wants to use an unapproved tool, keeping the session alive while the user decides. This covers the common case well.

There is a second, earlier class of permission failure that `--permission-prompt-tool stdio` cannot intercept: Claude Code's **directory-trust pre-flight check**. Before the permission-prompt-tool stdio handler becomes active, Claude checks whether the tools it is about to use are allowed to operate in the current working directory. If they are not, the tool returns `"Stream closed"` as its result — no `control_request` event is ever emitted, no user prompt fires, and the turn ends with partial or empty output.

This failure is silent from the user's perspective: the stream ends cleanly (no error flag, no natural-language explanation) and only becomes visible when the NDJSON `type:"user"` message containing the failed `tool_result` is parsed.

### Why it triggers

Claude Code maintains an internal list of trusted directories. On a fresh workspace (no prior interactive approval, no `~/.claude/settings.json` with an existing trust entry), the first invocation can hit this pre-flight check for common tools (`Bash`, `Read`, `Write`, `Edit`) before the permission-prompt handler has a chance to run.

`/tmp` is a frequent victim: scratch-space paths under `/tmp` are not part of the project directory tree and are therefore not implicitly trusted.

### Why it is hard to detect

The failure does not set `is_error` on the top-level result event. The signal is buried inside a `type:"user"` NDJSON message, inside a `message.content[]` array, inside a `tool_result` block whose `content` is the string `"Stream closed"` (or a `[{"type":"text","text":"Stream closed"}]` block array).

To correlate the failing `tool_result` back to a tool name, the `tool_use_id` in the `tool_result` must be matched against the `id` field from the earlier `content_block_start` stream event. That `id` field was previously not extracted.

## Decision

Handle this failure class with two complementary mechanisms.

### 1. Baseline tools pre-approved on every invocation

A fixed set of tools that are always safe to run — `Bash`, `Read`, `Write`, `Edit` — is merged into the `allowed_tools` list on every `claude-cli` invocation, ahead of user-configured entries. This is implemented as `cliBaselineTools` in `cmd/milk/main.go` and combined without duplicates in `newCLIAgent()`.

`/tmp` is added to the trusted directory list alongside `cwd` in `applyPersistedGrants()` unconditionally. Scratch-space blocks on `/tmp` are always spurious.

These two changes eliminate the failure for the majority of common workloads with no user interaction required.

### 2. Post-turn stream-closed detection and interactive retry

For tools outside the baseline set, or directory paths outside `cwd`+`/tmp`, the failure is detected after the turn completes:

**Detection** (`internal/agent/claude/stream.go`):

- `msgTypeUser` is added as a recognised NDJSON message type.
- `streamContentBlock` gains an `ID` field so the `tool_use_id` → tool name mapping can be built.
- `eventCallbacks` gains a `toolRegistry map[string]StreamClosedRecord` that accumulates `id → {ToolUseID, Name, Input}` as each `content_block_stop` is processed. This accumulation runs unconditionally — it no longer depends on `onToolUseReady != nil`.
- `applyUserMessage` scans each `type:"user"` message for `tool_result` blocks whose content is `"Stream closed"` (both plain-string and `[]contentBlock` encodings), looks up the tool name in the registry, and appends a `StreamClosedRecord` to `ParseResult.StreamClosedDenials`.

**Retry** (`cmd/milk/main.go`, `cmd/milk/runner.go`):

- `cliRunner.Execute` calls `handleStreamClosedDenials` after `handlePermissionDenials` when `ParseResult.StreamClosedDenials` is non-empty.
- `handleStreamClosedDenials` presents each failed tool to the user, offering a tool grant (y/n) and a directory grant (free-text path). Grants are persisted to `~/.claude/settings.json` via the existing `ClaudeSettings` writer so they survive future sessions.
- On any grant, the agent is cloned with `WithExtraAllowedTool` / `WithExtraDir` and a `RunResume` call is issued asking Claude to retry the failed invocations.

### Why not widen the baseline further

Adding more tools to `cliBaselineTools` increases the blast radius of any bug or misuse. The four tools (`Bash`, `Read`, `Write`, `Edit`) cover the overwhelming majority of first-turn failures on a fresh workspace; anything else should go through the explicit grant flow.

### Why not persist the baseline to `~/.claude/settings.json`

Persisting the baseline would mean a milk upgrade could silently widen the persisted allow-list in the user's Claude Code settings. Keeping it in-memory (passed as `--allowedTools` on each invocation) is scoped to milk's use of Claude and does not affect other Claude Code sessions.

## Consequences

- Silent "Stream closed" failures on fresh workspaces are largely eliminated for common tools.
- The detection path is purely additive — no existing `control_request` / `PermissionDenial` flow is changed.
- `ParseResult` gains a new field (`StreamClosedDenials`); callers that ignore it are unaffected.
- The `toolRegistry` accumulation is unconditional, adding negligible overhead per tool block streamed.
- The retry message is fixed ("Continue — … retry them now with the newly granted permissions"); Claude may occasionally re-explain context before acting.
- `type:"user"` lines that do not contain `tool_result` blocks with "Stream closed" content are parsed and immediately discarded — no behavioural change for normal turns.
