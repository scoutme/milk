# milk

Switch models, not context. Routes prompts between a local LLM (any OpenAI-compatible inference server) and Claude Code CLI, with session-aware state management and real-time streaming.

## Quick orientation

- [docs/setup.md](docs/setup.md) — inference server setup, tested models, local testing procedure
- [docs/spec.md](docs/spec.md) — full product and architecture spec
- [docs/adr/README.md](docs/adr/README.md) — architecture decision records (why things are the way they are)
- [docs/branching-strategy.md](docs/branching-strategy.md) — branch naming, conventional commits, per-step branch plan

## Project structure

```
cmd/milk/main.go              # Cobra root command, single-prompt mode
cmd/milk/repl.go              # bubbletea TUI (transcript + textarea + status bar)
cmd/milk/interactive.go       # slash commands, tab completion, prompt label
cmd/milk/ansi.go              # ANSI color helpers and spinner
cmd/milk/panel_memory.go      # right-side memory panel (open by default, toggle /panel memory)
internal/config/              # config loading (~/.milk/config.json)
internal/session/             # session state + store (~/.milk/sessions/)
internal/router/              # routing logic (rules + weighted scorer + local model)
internal/agent/local/         # OpenAI-compat client + Bedrock Converse native path + auth transports (SigV4, Bearer, custom headers) + tool loop + stream detector
internal/agent/claude/        # claude CLI subprocess + stream-json parser
internal/escalation/          # context builder (local transcript → Claude prompt)
internal/memory/              # Percept store + NREM consolidation (~/.milk/memory/)
internal/obs/                 # OpenTelemetry file exporters (~/.milk/otel/)
```

## Key design decisions

- **OpenAI-compat local agent**: any compliant inference server works (llama.cpp, Ollama, LM Studio, vLLM, or remote cloud providers). Auth transports: none (local), AWS SigV4 (Bedrock), Bearer token (OpenRouter, Together.ai, Groq, …), dynamic tokens via `token_cmd`, arbitrary extra headers. Set `provider` in the `local_agents` entry to select. Bedrock also uses a native Converse API path (not OpenAI-compat). Tested: Qwen2.5-Coder 7B/3B, Gemma 4 E4B.
- **Bedrock credential renewal**: `aws_refresh_cmd` wires a `credential_process`-compatible command into the SigV4 transport; on 403 it refreshes credentials atomically and retries once, with TUI status-bar feedback.
- **Single inference server instance**: same server handles both router classification and local coding/tool tasks
- **Claude via CLI subprocess**: `claude --print --output-format stream-json`, not direct API
- **Context handoff**: local transcript passed via `--append-system-prompt`; Claude orients itself
- **CLAUDE_WAITING state**: once Claude asks a follow-up, next turn bypasses router → `--resume`
- **Self-escalation**: local model can call `escalate_to_claude(reason)` as a function call
- **Streaming tool-format detector**: FSM detects tool-call markup format from the stream; handles Qwen fenced JSON, `<tool_call>` tags, Gemma special tokens, bare JSON without pre-configuration
- **Persistent TUI**: bubbletea alt-screen with viewport (transcript) + textarea (input) + status bar; agent turns run in goroutines, output streamed via `p.Send()`
- **Input history**: per-session (`~/.milk/sessions/<id>.history`) and global (`~/.milk/input_history`); Ctrl+R/Ctrl+S incremental search
- **Memory**: Percept store with NREM consolidation — decay/prune/promote cycle at session end; memory panel (`/panel memory`) shows SESSION/GLOBAL/GLOBAL(core) sections in real time, open by default; `/forget` and `/memory show` for interactive management
- **Reasoning visibility**: thinking/reasoning tokens kept in a separate transcript variant; `/think on|off` toggles retroactively; both variants maintained in parallel during streaming (no rebuild on toggle); default configurable via `show_reasoning` in config

## Session states

```text
ROUTING → LOCAL | CLAUDE
LOCAL   → CLAUDE (on --escalate or escalate_to_claude())
CLAUDE  → CLAUDE_WAITING (when Claude asks a question)
CLAUDE_WAITING → ROUTING (on --local)
CLAUDE_WAITING → CLAUDE (default: next turn goes via --resume)
```

### Sticky mode (`/escalate` / `/local` without a prompt)

Typing `/escalate` alone (no inline prompt) sets `stickyEscalate = true`: every subsequent turn is routed to Claude, bypassing the router, until the user types `/local` or presses Ctrl+C. The prompt label switches to `[claude]` immediately.

Symmetrically, `/local` alone sets `stickyLocal = true`: every turn goes to the local model until `/escalate` or Ctrl+C.

Typing `/escalate <prompt>` or `/local <prompt>` is a **single-turn override** (`forceEscalate` / `forceLocal`): the flag is reset to false after the turn completes, and normal routing resumes.

Ctrl+C on an empty input clears both sticky and force flags before quitting — so it acts as a "reset mode" shortcut.

## Routing order (per turn)

1. Explicit flags (`--escalate`, `--local`)
2. Session state (`CLAUDE_WAITING` → bypass)
3. Rules layer (hard thresholds → short-prompt shortcut → weighted signal scorer)
4. Local model (classification call, when scorer is inconclusive)
5. Default: local

## Session storage

```text
~/.milk/sessions/index.json        # cwd → [{id, name, last_used}]
~/.milk/sessions/<uuid>.json       # full session (history, state, claude_session_id)
~/.milk/sessions/<uuid>.history    # per-session input history (plain text, one entry/line)
~/.milk/input_history              # global input history across all sessions
```

Default behavior: resume most recent session for cwd. `--new` creates a fresh session.

## Graceful degradation

| Inference server | claude CLI | behavior |
| --- | --- | --- |
| up | available | normal routing |
| down | available | warn, route all to Claude |
| up | unavailable | warn, local-only |
| down | unavailable | error + exit |

## Tech stack

- Go 1.21+, Cobra CLI
- charmbracelet/bubbletea, bubbles/viewport, bubbles/textarea, lipgloss
- Local agent: OpenAI-compatible inference API **or** AWS Bedrock Converse API (native, not OpenAI-compat)
- `claude` CLI binary (Claude Code)
- OpenTelemetry Go SDK with custom file exporters

## Backlog

- Planning mode (offline)
- Demotion from Claude back to local mid-session
- MCP server integration for local tools
- TUI: app-managed drag selection (currently terminal-native; selection highlight sticks to screen coords during scroll — Claude Code works around this with non-native selection)
