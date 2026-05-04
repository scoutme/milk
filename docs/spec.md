# milk — specification

## Overview

milk is a local-first agentic orchestrator CLI. It routes user prompts between a local LLM agent (Qwen2.5 via llama.cpp) and a rich cloud agent (Claude Code CLI), maintaining session state across turns and supporting context promotion from local to cloud.

The primary use case is code assistance and shell automation for a single user.

---

## Architecture

### Components

```
milk [prompt | flags]
       │
       ▼
┌─────────────────────────────────────────────────────┐
│  Session State Machine                              │
│  ROUTING → LOCAL | CLAUDE | CLAUDE_WAITING          │
└──────────┬──────────────────────────────────────────┘
           │
           ▼
┌──────────────────────────┐
│  Router                  │
│  1. Explicit flags       │  --escalate, --local
│  2. Session state check  │  CLAUDE_WAITING → bypass
│  3. Rules layer          │  heuristics
│  4. Local model          │  Qwen2.5 self-classification
│  5. Default: try local   │
└────────┬─────────────────┘
         │
    ┌────┴─────┐
    ▼           ▼
LOCAL           CLAUDE
agent           agent
llama.cpp       claude --print
OpenAI API      --output-format stream-json
tool loop       --session-id / --resume
```

### Session state machine

```
States: ROUTING | LOCAL | CLAUDE | CLAUDE_WAITING

ROUTING        → rules + local model decision → LOCAL or CLAUDE
LOCAL          → --escalate OR local model escalate_to_claude() → CLAUDE
CLAUDE         → Claude ends turn with question → CLAUDE_WAITING
CLAUDE_WAITING → next user input bypasses router → direct --resume to Claude
CLAUDE_WAITING → user --local flag → back to ROUTING
```

---

## Router

Decision order per turn:

1. **Explicit flags** — `--escalate` forces Claude; `--local` forces local (always wins)
2. **Session state** — if `CLAUDE_WAITING`, bypass router, send directly to `claude --resume`
3. **Rules layer** — heuristics: prompt length, complexity markers, keyword patterns
4. **Local model classification** — ask Qwen2.5 with minimal prompt, expect `route: local | escalate`
5. **Default** — attempt local; escalate if local returns `escalate_to_claude(reason)`

The classifier uses the same Qwen2.5 model instance as the local coding agent. No second model or second llama.cpp instance.

---

## Local Agent

- Backend: llama.cpp OpenAI-compatible server, default `http://localhost:8080`
- Model: Qwen2.5-Coder (7B or 14B; user-configured)
- Tool loop: standard agentic loop — call → check tool calls → execute → feed result → repeat until final answer
- Built-in tools (implemented in Go, exposed as OpenAI function schemas):
  - `bash(command string) → stdout, stderr, exit_code`
  - `grep(pattern string, path string, recursive bool) → matches`
  - `read_file(path string, offset int, limit int) → content`
- Self-escalation: local model may return `escalate_to_claude(reason string)` as a tool call to trigger promotion

---

## Claude Agent

- Interface: `claude` CLI subprocess
- First escalation turn:
  ```
  claude --print --output-format stream-json \
         --session-id <new-uuid> \
         --append-system-prompt "<formatted transcript + context>"
  ```
- Subsequent turns in same escalation:
  ```
  claude --print --output-format stream-json \
         --resume <claude-session-id> \
         "<user prompt>"
  ```
- `session_id` is extracted from the first NDJSON message and persisted to the milk session file
- Claude orients itself from the appended context — no separate reformulation step

### Context handoff (escalation)

When promoting from local to Claude, milk formats the local conversation history as a plain transcript and passes it via `--append-system-prompt`. Format:

```
[Context from local agent session]
User: <turn>
Assistant: <turn>
[Tool: bash] <command>
[Tool result] <output>
...
User: <final prompt that triggered escalation>
```

---

## Session Model

### Storage layout

```
~/.milk/
├── config.json
└── sessions/
    ├── index.json          # cwd → [{id, name, last_used}] sorted by last_used desc
    └── <uuid>.json         # full session data
```

### Session file schema

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "name": "optional-user-name",
  "cwd": "/absolute/path/to/project",
  "created_at": "2026-05-05T10:00:00Z",
  "last_used": "2026-05-05T11:32:00Z",
  "state": "CLAUDE_WAITING",
  "claude_session_id": "abc123",
  "history": [
    {
      "role": "user | assistant | tool_result",
      "agent": "local | claude",
      "content": "...",
      "tool_calls": [],
      "timestamp": "2026-05-05T10:01:00Z"
    }
  ]
}
```

### Session index file schema

```json
{
  "/absolute/path/to/project": [
    {"id": "uuid", "name": "refactor-auth", "last_used": "2026-05-05T11:32:00Z"},
    {"id": "uuid", "name": "", "last_used": "2026-05-05T10:00:00Z"}
  ]
}
```

### Session lookup on invocation

1. `milk <prompt>` → most recent session for cwd → resume (or create new if none)
2. `milk --session refactor-auth <prompt>` → find by name within cwd → resume or create
3. `milk --new <prompt>` → always create fresh session
4. `milk --session refactor-auth --new <prompt>` → create new named session

Names are cwd-scoped. Same name can exist in different projects.

---

## CLI Interface

### Usage

```
milk [flags] <prompt>
milk [flags]                  # enters interactive mode (backlog)
```

### Flags

| Flag | Description |
|------|-------------|
| `--escalate` | Force route to Claude for this turn |
| `--local` | Force route to local model for this turn; breaks CLAUDE_WAITING state |
| `--new` | Start a new session (old sessions for cwd untouched) |
| `--session <name>` | Target session by name (resume or create) |
| `--continue` | Alias for default resume behavior (explicit) |
| `--list` | List sessions for current cwd |
| `--list --all` | List all sessions across all directories |
| `--drop` | Delete current session |

---

## Configuration

`~/.milk/config.json`:

```json
{
  "llama_url": "http://localhost:8080",
  "llama_model": "qwen2.5-coder",
  "claude_bin": "claude",
  "default_route": "local",
  "rules": {
    "escalate_above_tokens": 2000,
    "escalate_keywords": ["architect", "refactor entire", "design", "explain why"]
  }
}
```

---

## Graceful Degradation

| llama.cpp | claude CLI | behavior |
|-----------|-----------|----------|
| up | available | normal routing |
| down | available | warn once per session, route all to Claude |
| up | unavailable/not installed | warn once per session, stay local-only |
| down | unavailable | error + exit |

---

## Streaming

Both agents stream output in real time:
- **Local agent**: SSE from llama.cpp OpenAI-compat API (`stream: true`)
- **Claude agent**: NDJSON from `--output-format stream-json`, parsed line by line

milk relays tokens to stdout as they arrive.

---

## Backlog

- Interactive (REPL) mode without prompt argument
- Planning mode (offline, no LLM execution)
- Demotion from Claude back to local mid-session
- Web UI / TUI
- MCP server integration for local tools
- Multi-user / daemon mode
