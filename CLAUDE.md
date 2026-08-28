# milk

Switch models, not context. Routes prompts between a local LLM (any OpenAI-compatible inference server) and a configurable escalation agent (Claude Code CLI or another inference backend), with session-aware state management and real-time streaming.

## Quick orientation

- [docs/getting-started.md](docs/getting-started.md) — fastest path to a working setup, provider-agnostic
- [docs/providers.md](docs/providers.md) — all agent/provider config (Claude CLI, Bedrock, OpenRouter, smolagents, local llama.cpp reference setup, …), memory/context tuning
- [docs/workflows.md](docs/workflows.md) — routing, session states, sticky escalation, the native `/workflow` engine
- [docs/tooling.md](docs/tooling.md) — built-in tools, agent-as-tool, MCP servers, attachments
- [docs/operations.md](docs/operations.md) — memory usage, observability, loop detection, task tracking, remote oversight
- [docs/spec.md](docs/spec.md) — architecture and CLI reference
- [docs/eval.md](docs/eval.md) — `milk eval` usage: commands, scenario format, adapters and their per-adapter options, judging, reports
- [docs/adr/README.md](docs/adr/README.md) — architecture decision records (why things are the way they are)
- [docs/branching-strategy.md](docs/branching-strategy.md) — branch naming, conventional commits, per-step branch plan

## Project structure

```
cmd/milk/main.go              # Cobra root command, single-prompt mode; buildPrimaryRunner / buildEscalationRunner
cmd/milk/repl.go              # bubbletea TUI (transcript + textarea + status bar)
cmd/milk/runner.go            # TurnRunner interface + localRunner / cliRunner / subprocessRunner implementations
cmd/milk/dispatch.go          # runPrimary / runEscalation — role-specific session bookkeeping
cmd/milk/interactive.go       # slash commands, tab completion, prompt label
cmd/milk/ansi.go              # ANSI color helpers and spinner
cmd/milk/panel_memory.go      # right-side memory panel (open by default, toggle /panel memory)
internal/config/              # config loading (~/.milk/config.json)
internal/session/             # session state + store (~/.milk/sessions/)
internal/router/              # routing logic (rules + weighted scorer + local model)
internal/agent/local/         # OpenAI-compat client + Bedrock Converse native path + auth transports (SigV4, Bearer, custom headers) + tool loop + stream detector
internal/agent/claude/        # claude CLI subprocess + stream-json parser
internal/agent/subprocess/    # generic subprocess agent (NDJSON protocol); base for aider and smolagent
internal/agent/aider/         # aider-cli provider (wraps subprocess agent)
internal/agent/smolagent/     # subprocess provider (wraps subprocess agent)
internal/escalation/          # context builder (local transcript → escalation agent prompt)
internal/memory/              # Percept store + NREM consolidation (~/.milk/memory/)
internal/mcpauth/             # native MCP OAuth: RFC 9728/8414 discovery, RFC 7591 DCR, PKCE, token store (~/.milk/mcp_oauth/)
internal/obs/                 # OpenTelemetry file exporters (~/.milk/otel/)
eval/                          # `milk eval` subcommand — harness, adapters (claude-code, milk-tui), judge, report (see docs/eval.md)
```

## Key design decisions

- **Unified agent config**: all backends — local inference servers and the Claude CLI — are entries in the `agents` array in `~/.milk/config.json`. Each entry has a `provider` field: `""` / `"local"` (plain HTTP), `"bedrock"` (AWS SigV4), `"claude-cli"` (Claude Code subprocess), or any string for Bearer-token providers. Auth transports: none (local), AWS SigV4 (Bedrock), Bearer token (OpenRouter, Together.ai, Groq, …), dynamic tokens via `token_cmd`, arbitrary extra headers. Bedrock also uses a native Converse API path (not OpenAI-compat). Tested: Qwen2.5-Coder 7B/3B, Gemma 4 E4B.
- **Bedrock credential renewal**: `aws_refresh_cmd` wires a `credential_process`-compatible command into the SigV4 transport; on 403 it refreshes credentials atomically and retries once, with TUI status-bar feedback.
- **Native MCP OAuth**: `/mcp auth <server>` runs milk's own RFC 9728/8414 discovery + RFC 7591 dynamic client registration + Authorization Code/PKCE flow (`internal/mcpauth/`) — no Claude CLI hand-off. TUI stays interactive: local loopback listener, best-effort browser open, URL also printed to the transcript, status-bar "MCP OAuth: `<server>`" indicator. Tokens in `~/.milk/mcp_oauth/`, refreshed proactively before expiry and reactively on 401.
- **Unified MCP auth resolution**: `internal/mcpauth.ResolveHeader` is the single auth-resolution path (`bearer` / `token_cmd` / `oauth`) shared by every agent that builds MCP request headers — `internal/mcp.Client` (local/primary agent) and claude-cli's `--mcp-config` generation (escalation agent) both call it. A server needs authorizing once regardless of which agent(s) use it; fixes a prior gap where `token_cmd` was silently dropped for escalation-agent MCP servers.
- **Local-agent prompt caching**: two independent mechanisms, both feeding the same `TokenUsage.CacheRead/CacheCreation` → status bar/memory panel pipeline. (1) OpenAI-compatible implicit caching — parses `usage.prompt_tokens_details.cached_tokens` (OpenAI's own standard field, mirrored by e.g. Xiaomi MiMo) with zero request-side changes or config; live-verified. (2) Bedrock explicit `cachePoint` caching — opt-in via `prompt_caching` in the agent config entry, appends `{"cachePoint":{"type":"default"}}` to the Converse API system array; **experimental, not live-tested** (no Bedrock agent available during development). Provider-reported prompt-token totals that include the cached portion (subset, not additive) are normalized to fresh-only before recording — see `internal/agent/local/local.go`'s `streamCompletion`.
- **Single inference server instance**: same server handles both router classification and local coding/tool tasks
- **Escalation agent**: any `agents` entry can be the escalation target — set `escalation_agent` to its name. Defaults to the built-in `claude-cli` entry. Use `/agent switch <name> as escalation` to change it at runtime.
- **Claude via CLI subprocess**: `claude --print --output-format stream-json`, not direct API. Configured as `provider: "claude-cli"` in `agents`.
- **Context handoff**: local transcript passed via two `--append-system-prompt-file` flags (static instructions + dynamic summary) for the CLI path; local providers receive a `BuildDynamicContext` orientation block as a prepended system message. Both paths inject percepts. On stale returning escalations (topic switched or ≥`returning_fresh_start_local_turns` local turns since last escalation, default 8), CLI drops `--resume` and local providers scope history to post-escalation turns only.
- **ESCALATION_WAITING state**: once the escalation agent asks a follow-up, next turn bypasses router → `--resume`
- **Self-escalation**: local model can call `escalate(reason)` as a function call
- **Role-aware system prompt**: primary agent and escalation agent receive different system prompts — the escalation agent knows it is the escalation target and should not escalate further
- **Streaming tool-format detector**: FSM detects tool-call markup format from the stream; handles Qwen fenced JSON, `<tool_call>` tags, Gemma special tokens, bare JSON without pre-configuration
- **Persistent TUI**: bubbletea alt-screen with viewport (transcript) + textarea (input) + status bar; agent turns run in goroutines, output streamed via `p.Send()`
- **Input history**: per-session (`~/.milk/sessions/<id>.history`) and global (`~/.milk/input_history`); Ctrl+R/Ctrl+S incremental search
- **Memory**: Percept store with NREM consolidation — decay/prune/promote cycle at session end; memory panel (`/panel memory`) shows SESSION/GLOBAL/GLOBAL(core) sections in real time, open by default; `/forget` and `/memory show` for interactive management
- **Reasoning visibility**: thinking/reasoning tokens kept in a separate transcript variant; `/think on|off` toggles retroactively; both variants maintained in parallel during streaming (no rebuild on toggle); default configurable via `show_reasoning` in config

## Session states

```text
ROUTING          → LOCAL | ESCALATION
LOCAL            → ESCALATION (on --escalate or escalate())
ESCALATION       → ESCALATION_WAITING (when escalation agent asks a question)
ESCALATION_WAITING → ROUTING (on --primary)
ESCALATION_WAITING → ESCALATION (default: next turn goes via --resume)
```

### Sticky mode (`/escalate` / `/primary` without a prompt)

Typing `/escalate` alone (no inline prompt) sets `stickyEscalate = true`: every subsequent turn is routed to the configured escalation agent, bypassing the router, until the user types `/primary` or presses Ctrl+C. The prompt label shows `<agent> (pinned)`.

Symmetrically, `/primary` alone sets `stickyPrimary = true`: every turn goes to the primary agent until `/escalate` or Ctrl+C. The prompt label shows `<agent> (pinned)`.

Typing `/escalate <prompt>` or `/primary <prompt>` is a **single-turn override** (`forceEscalate` / `forcePrimary`): the flag is reset to false after the turn completes, and normal routing resumes.

### Auto-sticky escalation

When the router first escalates (without an explicit `/escalate`), `autoStickyEscalate` is set automatically — every subsequent turn stays on the escalation agent, showing `<agent> (sticky)` in the status bar. This avoids the "RETURNING" context-mode where Claude would otherwise lose continuity between sessions. Cleared by `/primary` or a single-turn `forcePrimary` override.

Disable via `sticky_escalation: false` in `~/.milk/config.json`. Explicit `/escalate` (pinned) is unaffected by this setting.

## Routing order (per turn)

1. Explicit flags (`--escalate`, `--primary`)
2. Session state (`ESCALATION_WAITING` → bypass)
3. Rules layer (hard thresholds → short-prompt shortcut → weighted signal scorer)
4. Local model (classification call, when scorer is inconclusive)
5. Default: local

## Session storage

```text
~/.milk/sessions/index.json        # cwd → [{id, name, last_used}]
~/.milk/sessions/<uuid>.json       # full session (history, state, escalation_session_id)
~/.milk/sessions/<uuid>.history    # per-session input history (plain text, one entry/line)
~/.milk/input_history              # global input history across all sessions
```

Default behavior: resume most recent session for cwd. `--new` creates a fresh session.

## Graceful degradation

| Primary agent | Escalation agent | behavior |
| --- | --- | --- |
| up | available | normal routing |
| down | available | warn, route all to escalation agent |
| up | unavailable | warn, primary-only |
| down | unavailable | warn both unavailable, TUI stays open (use /agent to reconfigure) |

## Tech stack

- Go 1.21+, Cobra CLI
- charmbracelet/bubbletea, bubbles/viewport, bubbles/textarea, lipgloss
- Local agent: OpenAI-compatible inference API **or** AWS Bedrock Converse API (native, not OpenAI-compat)
- `claude` CLI binary (Claude Code) — configured as `provider: "claude-cli"` in `agents`
- OpenTelemetry Go SDK with custom file exporters

## Backlog

- Planning mode (offline)
- Demotion from escalation back to primary mid-session
- MCP stdio transport for local subprocess tools ✓ (done — `transport: "stdio"`, `command`, `args` fields in `MCPServerConfig`)
- TUI: app-managed drag selection (currently terminal-native; selection highlight sticks to screen coords during scroll — Claude Code works around this with non-native selection)

## Token tracking: subagents and workflows

When the escalation agent (Claude Code) spawns subagents via the Agent tool or runs background workflows, milk tracks their token usage separately from the main process. This applies to:

- **Subagent tokens** — subagents spawned by Claude Code's Agent tool
- **Workflow tokens** — background workflows run by Claude Code

### Agent role strings

Token usage is stored in the session `Tokens` map keyed by `"model\x00role"`. The role strings follow this convention:

| Role string | Meaning |
|---|---|
| `primary` | Local agent (router + local model) |
| `escalation` | Main escalation agent (Claude Code) |
| `escalation:subagent` | Subagent spawned by the escalation agent |
| `escalation:workflow` | Background workflow run by the escalation agent |

The colon-separated convention allows prefix queries — `SessionTokensByRolePrefix("escalation")` matches all three escalation-related roles.

### How it works

1. `stream.go` parses `SubagentUsage`/`WorkflowUsage` from the `result` event's stream JSON when present.
2. `dispatch.go` records these via `sess.AddTokensFull` with the appropriate role string and via `obs.RecordTokens` / `obs.AccumulateCacheTokens` for OTel metrics.
3. `FormatTokenUsage` in the TUI displays each role string as its own row in the `/usage` table.
4. The eval harness (`adapter_claude.go`) captures subagent/workflow tokens from the transcript JSONL.
5. Graceful degradation: when Claude Code does not provide subagent/workflow data, all tokens are attributed to the main `escalation` role as before.

## Loop detection

Milk detects when an LLM agent gets stuck in a loop — repeating the same phrase/tool-call within a turn, or producing identical responses across turns. This prevents runaway token consumption when the user doesn't interrupt.

### Signals

| Signal | Scope | What it catches | Default threshold |
|---|---|---|---|
| **chunk_repetition** | Intra-turn | Same text repeating in streaming output | 3 occurrences in 50-chunk window |
| **reasoning_chunk_repetition** | Intra-turn | Same text repeating in streaming reasoning/thinking output | 10 occurrences in 50-chunk window |
| **response_repetition** | Cross-turn | Identical/near-identical responses across turns | 3 consecutive similar responses |
| **reasoning_repetition** | Cross-turn | Identical/near-identical reasoning text across turns | 6 consecutive similar responses |
| **token_velocity** | Cross-turn | Rapid token consumption without progress | 50k tokens in 30s window |
| **tool_call_echo** | Cross-turn | Same tool+args in consecutive turns | 3 consecutive turns |
| **silent_burn** | Per-turn | High input tokens, near-zero output | 20k input tokens |
| **turn_flood** | Session | Excessive turns without user input | 10 consecutive non-user turns |

### How it works

1. **Intra-turn**: `FeedChunk()` is called for every streaming `chunkMsg`. A ring buffer tracks the last 50 chunks. When the same text appears 3+ times, `SignalChunkRepetition` fires (confidence 0.9) and the turn is auto-interrupted via `cancelTurn()`.
2. **Cross-turn**: `Feed()` is called after each turn completes. It checks response similarity (trigram Jaccard), token velocity, tool call patterns, and turn count.
3. **Status bar**: Shows `⚠ loop — auto-interrupted` or `⚠ <signal>` when a signal fires.
4. **Transcript**: Shows `[⚠ loop detected: <signal> (confidence N%)]` for high-confidence signals.

### Configuration

```json
{
  "loop_detection": {
    "enabled": true,
    "chunk_repetition_threshold": 3,
    "chunk_window_size": 50,
    "reasoning_chunk_repetition_threshold": 10,
    "max_consecutive_similar_responses": 3,
    "response_similarity_threshold": 0.85,
    "reasoning_max_consecutive_similar_responses": 6,
    "token_velocity_threshold": 50000,
    "auto_interrupt": false
  }
}
```

Default: detection ON, auto-interrupt OFF (warn only). Set `auto_interrupt: true` for unattended sessions.
