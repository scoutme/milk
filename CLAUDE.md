# milk

Local-first agentic orchestrator CLI. Routes prompts between a local LLM (Qwen2.5 via llama.cpp) and Claude Code CLI, with session-aware state management and real-time streaming.

## Quick orientation

- [docs/spec.md](docs/spec.md) — full product and architecture spec
- [docs/adr/README.md](docs/adr/README.md) — architecture decision records (why things are the way they are)

## Project structure

```
cmd/milk/main.go              # Cobra root command
internal/config/              # config loading (~/.milk/config.json)
internal/session/             # session state + store (~/.milk/sessions/)
internal/router/              # routing logic (rules + local model)
internal/agent/local/         # llama.cpp OpenAI-compat client + tool loop
internal/agent/claude/        # claude CLI subprocess + stream-json parser
internal/escalation/          # context builder (local transcript → Claude prompt)
```

## Key design decisions

- **One llama.cpp instance**: Qwen2.5-Coder serves as both router classifier and local agent
- **Claude via CLI subprocess**: `claude --print --output-format stream-json`, not direct API
- **Context handoff**: local transcript passed via `--append-system-prompt`; Claude orients itself
- **CLAUDE_WAITING state**: once Claude asks a follow-up, next turn bypasses router → `--resume`
- **Self-escalation**: local model can call `escalate_to_claude(reason)` as a function call

## Session states

```
ROUTING → LOCAL | CLAUDE
LOCAL   → CLAUDE (on --escalate or escalate_to_claude())
CLAUDE  → CLAUDE_WAITING (when Claude asks a question)
CLAUDE_WAITING → ROUTING (on --local)
CLAUDE_WAITING → CLAUDE (default: next turn goes via --resume)
```

## Routing order (per turn)

1. Explicit flags (`--escalate`, `--local`)
2. Session state (`CLAUDE_WAITING` → bypass)
3. Rules layer (heuristics)
4. Local model (Qwen2.5 classification call)
5. Default: local

## Session storage

```
~/.milk/sessions/index.json       # cwd → [{id, name, last_used}]
~/.milk/sessions/<uuid>.json      # full session (history, state, claude_session_id)
```

Default behavior: resume most recent session for cwd. `--new` creates a fresh session.

## Graceful degradation

| llama.cpp | claude CLI | behavior |
|-----------|-----------|----------|
| up | available | normal routing |
| down | available | warn, route all to Claude |
| up | unavailable | warn, local-only |
| down | unavailable | error + exit |

## Tech stack

- Go 1.21+, Cobra CLI
- llama.cpp OpenAI-compatible API (default `http://localhost:8080`)
- `claude` CLI binary (Claude Code)

## Implementation order

1. `go.mod` + Cobra skeleton + config
2. Session store (read/write/index)
3. Local agent (OpenAI client + tool loop)
4. Router (rules + model classification)
5. Claude agent (subprocess + stream parser)
6. Escalation builder
7. Session state machine wiring

## Backlog

- Interactive (REPL) mode
- Planning mode (offline)
- Demotion from Claude back to local mid-session
- MCP server integration for local tools
