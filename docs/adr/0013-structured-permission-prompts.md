# ADR-0013: Structured permission prompts via --permission-prompt-tool stdio

Date: 2026-05-08

## Status

Accepted

## Context

ADR-0012 introduced a reactive permission system that scanned Claude's natural-language text output for English phrases like "need permission" and "is restricted" to detect when Claude was blocked. This approach has two fundamental problems:

1. **Language dependency**: Claude mirrors the user's language. A non-English user receives non-English refusals, which never match the English phrase list.
2. **Fragility**: Claude's phrasing is not under our control and changes across versions. The phrase list requires ongoing maintenance.

Investigation of the Claude Code CLI event schema (v2.1.132) revealed a structured alternative: the `--permission-prompt-tool stdio` flag.

### How it works

When `--permission-prompt-tool stdio` is passed, Claude does not emit English prose when blocked. Instead it:

1. Emits a `control_request` NDJSON event on stdout and **pauses**, keeping the session alive
2. Waits for a `control_response` written to its stdin before continuing

The `control_request` schema includes:

- `request.subtype: "can_use_tool"` — always this value for permission checks
- `request.tool_name` — the tool Claude wants to use (e.g. `"Bash"`, `"Read"`)
- `request.input` — the tool arguments
- `request.decision_reason_type` — language-neutral enum: `workingDir`, `rule`, `mode`, `safetyCheck`, `classifier`, `hook`, `sandboxOverride`, `asyncAgent`, `subcommandResults`, `other`
- `request.blocked_path` — the specific path that triggered a `workingDir` denial
- `request.display_name`, `request.title`, `request.description` — human-readable context (in Claude's language, not used for logic)
- `request.classifier_approvable` — false when a safety check requires explicit human approval

The response milk sends back:

```json
{"type":"control_response","response":{"subtype":"success","request_id":"<same uuid>","response":{"behavior":"allow","updatedInput":{}}}}
```

or `"behavior":"deny"` to refuse.

Additionally, the final `result` event includes a `permission_denials` array listing all tools that were silently blocked (when `--dangerously-skip-permissions` is not set and `--permission-prompt-tool` is not used). This is used for post-hoc diagnostics only.

### Known risk

Bug #34046 (filed ~v2.1.73-74) reported `control_request` events not reliably emitting. The installed version is v2.1.132; testing against the actual binary is required before shipping.

## Decision

Replace the phrase-based reactive permission system (ADR-0012) with `--permission-prompt-tool stdio`:

- Pass `--permission-prompt-tool stdio` on all Claude invocations (alongside `--print --output-format stream-json --verbose`)
- Connect Claude's **stdin to a pipe** controlled by milk (previously `cmd.Stdin = nil`)
- In `Stream()`, detect `control_request` events and invoke a `PermissionHandler` callback synchronously before continuing to read the stream
- The handler receives `ControlRequest` (tool name, input, reason type, blocked path) and returns `"allow"` or `"deny"`
- milk's default handler asks the user interactively: show the tool name and input, prompt y/n; for `workingDir` blocks show the blocked path explicitly
- Remove `PermissionDenied`, `DeniedTool`, `DirRestricted` from `ParseResult`
- Remove `permissionPhrases`, `dirRestrictionPhrases`, `detectPermissionDenied`, `detectDirRestricted` from `stream.go`
- Remove `retryWithTool`, `retryWithDir`, `handleClaudeRetry` from `main.go`
- Remove `WithExtraTools`, `WithExtraDirs` from `claude.go` (no longer needed — the session stays live)
- `AllowedTools` / `add_dirs` config fields and `--allowedTools` / `--add-dir` flags are kept as proactive pre-approval, unchanged

## Consequences

- Permission detection is fully language-neutral and version-stable
- The session stays alive during the approval prompt — no `--resume` round-trip, no context loss
- `blocked_path` is surfaced directly to the user without asking them to type a path
- Tool input is shown to the user so they can make an informed decision
- `cmd.Stdin` must now be a pipe (not nil), and the `Stream()` goroutine must write responses to it while simultaneously reading stdout — requires careful goroutine coordination
- If bug #34046 is still present in the installed version, `control_request` may not emit reliably; the fallback is to re-enable `--dangerously-skip-permissions` via config
