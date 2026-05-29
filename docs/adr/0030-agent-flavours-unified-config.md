# 30. Agent Flavours: Unified Config and Escalation Refactor

Date: 2026-05-29

Status: Accepted

## Context

Several independent concerns accumulated after [ADR-0028](0028-multi-provider-local-agent-config.md):

1. **Claude CLI was a special case** — `claude_bin`, `dangerously_skip_permissions`, `allowed_tools`, `add_dirs` lived as root `Config` fields separate from the `local_agents` array. Any agent in the system could in principle be the escalation target (another local model, a different Bedrock model, etc.), but the config structure hardcoded Claude as the only possible escalation target.

2. **`local_agents` / `local_agent` naming** was tied to the original "local LLM only" framing. With Bedrock, OpenRouter, and CLI backends all coexisting, the "local" qualifier had become misleading.

3. **The `escalate_to_claude` tool name** embedded Claude's identity in the protocol. If the escalation target is a Bedrock model, the tool name was factually wrong and confusing to the primary agent.

4. **`/local` slash command** was the symmetric counterpart of `/escalate`, but "local" was ambiguous — it referred to the primary agent role, not the physical location of the model.

5. **`isRepeatedPrompt` in the escalation agent** caused spurious `EscalationSignal` errors when the primary agent escalated a prompt it had just answered: the escalation agent saw the prompt in history, fired the repeat check, and returned an error that masked the real problem.

6. **`~` in tool paths** was not expanded, causing file operations with `~`-relative paths to create literal `~/` directories relative to the working directory.

## Decision

### 1. Unified `agents` array — all backends in one place

Rename `local_agents` → `agents`, `local_agent` → `agent`. The `AgentConfig` struct (formerly `LocalAgentConfig`) gains a new `provider` value: `"claude-cli"`. Fields that were on the root `Config` (`claude_bin`, `dangerously_skip_permissions`, `allowed_tools`, `add_dirs`) move onto the `AgentConfig` entry as `bin`, `dangerously_skip_permissions`, `allowed_tools`, `add_dirs`.

A built-in entry named `"claude"` with `provider: "claude-cli"` and `bin: "claude"` is injected if no entry named "claude" exists, so existing users without an explicit entry still get the default behaviour.

Old config files are migrated automatically on first load (`migrateConfig`): `local_agents` → `agents`, root `claude_bin` / `dangerously_skip_permissions` / `allowed_tools` / `add_dirs` → into a `claude-cli` entry. The migrated config is written back to disk immediately so the old keys do not persist.

### 2. Escalation agent as first-class config

`EscalationAgent` in `Config` now points to any `agents` entry by name. `EscalationAgentConfig()` resolves it. `UseClaudeEscalation()` is removed; callers check `ac.IsCLI()` instead. The active escalation agent can be changed at runtime with `/agent switch <name> as escalation`.

### 3. `escalate_to_claude` → `escalate`

The self-escalation tool is renamed to `escalate`. The primary agent's system prompt refers to it by the new name. The escalation agent's system prompt does not expose the tool at all (it should not escalate further).

### 4. Role-aware system prompt

`buildSystemPrompt` now takes the agent's escalation role. Primary agents get a prompt that includes `escalate` and instructions to use it for tasks beyond their capabilities. Escalation agents get a different prompt: they are identified by name as the escalation target, told not to escalate further, and instructed to check `agent: "local"` (not `agent: "claude"`) when looking for the primary agent's prior work.

`AsEscalationTarget(name string)` marks a `local.Agent` for the escalation role. It sets `skipRepeatCheck = true` and stores the name for the system prompt.

### 5. `skipRepeatCheck` — escalation agent bypass

`isRepeatedPrompt` is correct for the primary agent (avoiding infinite local loops) but wrong for the escalation agent (which is supposed to handle prompts the primary already tried). `Agent.skipRepeatCheck` disables the check when the agent is built via `AsEscalationTarget`. The escalation history function also strips any trailing unanswered user turn matching the escalated prompt, eliminating the double-append that previously caused Bedrock validation errors (consecutive user messages).

### 6. `/local` → `/primary`

The `/local` slash command is renamed to `/primary`. It pins subsequent turns to the primary agent role, regardless of what technology backs it. The symmetric pair is `/escalate` ↔ `/primary`. Internal field names `forceLocal` / `stickyLocal` → `forcePrimary` / `stickyPrimary`.

### 7. `/agent switch` interactive wizard

`/agent switch` now prompts for both name and role when they are missing. Inline form: `/agent switch <name> as primary|escalation`. Missing arguments trigger a multi-step wizard (same pattern as `/agent add`). Switching the escalation role rebuilds `agents.escalationLocal` or clears it (for `claude-cli`).

### 8. `expandTilde` in tool paths

All file-operating tools (`read_file`, `write_file`, `edit_file`, `list_dir`, `find_files`, `grep`) now call `expandTilde` on the path argument before passing it to the OS. A leading `~` is replaced with the result of `os.UserHomeDir()`.

### 9. Constant renames

No JSON wire format is changed. Internal Go identifiers are renamed for consistency:

| Old | New |
|---|---|
| `AgentClaude` | `AgentEscalation` |
| `StateClaude` | `StateEscalation` |
| `StateClaudeWaiting` | `StateEscalationWaiting` |
| `TargetClaude` | `TargetEscalation` |
| `ProducerClaude` | `ProducerEscalation` |
| `ConsumerClaude` | `ConsumerEscalation` |
| `ClaudeSessionID` | `EscalationSessionID` |
| `LastClaudeSummary` | `LastEscalationSummary` |
| `DebugClaudeCode` | `DebugCLILog` |
| `ClaudeDebugLogPath` | `CLIDebugLogPath` |
| `IsClaudeCLI` | `IsCLI` |

## Consequences

**Good:**
- The config structure is honest: every backend — inference server or CLI — is an `agents` entry. No root-level special cases.
- Escalation target is fully configurable; two-local-model setups work without depending on the Claude CLI at all.
- Spurious "repeated question" errors on self-escalation are eliminated.
- `~`-relative paths in tool calls no longer create stray directories.
- `/primary` ↔ `/escalate` is a symmetric, role-neutral pair.

**Neutral:**
- Migration logic must be maintained for the lifetime of the project; old configs are rewritten on first load.
- The `agent/claude` package is still named for Claude (it implements the CLI subprocess protocol, which is Claude-specific); package renaming is out of scope for this ADR.

**Bad:**
- Existing session files use the old JSON keys (`claude_session_id`, `state: "CLAUDE"`, `agent: "claude"`) which are kept as-is for backward compatibility — sessions can still be resumed after upgrading. The Go constants changed but the serialized values did not.

## Supersedes

[ADR-0028](0028-multi-provider-local-agent-config.md) (partially — the `agents` array extension supersedes the `local_agents` decision)
