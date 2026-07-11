# milk

Switch models, not context. Routes prompts between a local LLM (any OpenAI-compatible inference server) and a configurable escalation agent (Claude Code CLI or another inference backend), with session-aware state management and real-time streaming.

## Quick orientation

- [docs/setup.md](docs/setup.md) — local inference server setup (llama.cpp), tested models, local testing procedure
- [docs/providers.md](docs/providers.md) — all agent/provider config (Claude CLI, Bedrock, OpenRouter, smolagents, …), memory/context tuning, remote oversight
- [docs/spec.md](docs/spec.md) — full product and architecture spec
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
internal/obs/                 # OpenTelemetry file exporters (~/.milk/otel/)
```

## Key design decisions

- **Unified agent config**: all backends — local inference servers and the Claude CLI — are entries in the `agents` array in `~/.milk/config.json`. Each entry has a `provider` field: `""` / `"local"` (plain HTTP), `"bedrock"` (AWS SigV4), `"claude-cli"` (Claude Code subprocess), or any string for Bearer-token providers. Auth transports: none (local), AWS SigV4 (Bedrock), Bearer token (OpenRouter, Together.ai, Groq, …), dynamic tokens via `token_cmd`, arbitrary extra headers. Bedrock also uses a native Converse API path (not OpenAI-compat). Tested: Qwen2.5-Coder 7B/3B, Gemma 4 E4B.
- **Bedrock credential renewal**: `aws_refresh_cmd` wires a `credential_process`-compatible command into the SigV4 transport; on 403 it refreshes credentials atomically and retries once, with TUI status-bar feedback.
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
