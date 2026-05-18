# milk — specification

## Overview

milk is a local-first agentic orchestrator CLI. It routes user prompts between a local LLM agent (any OpenAI-compatible inference server) and a rich cloud agent (Claude Code CLI), maintaining session state across turns and supporting context promotion from local to cloud.

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
│  3. Rules layer          │  heuristics + weighted scorer
│  4. Local model          │  local model self-classification
│  5. Default: try local   │
└────────┬─────────────────┘
         │
    ┌────┴─────┐
    ▼           ▼
LOCAL           CLAUDE
agent           agent
OpenAI API      claude --print
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
3. **Rules layer** — layered scorer:
   - Hard rules: token length above `escalate_above_tokens` → Claude; keyword match → Claude
   - Short-prompt shortcut: ≤ `local_below_tokens` tokens → conclusive local
   - Weighted signal scorer: local verbs, escalate verbs, path references, code blocks, open questions each contribute a signed score; conclusive if score reaches `escalate_threshold` or `local_threshold`
4. **Local model classification** — when scorer is inconclusive, ask the local model with minimal prompt, expect `route: local | escalate`; behaviour configurable via `classifier_fallback`
5. **Default** — attempt local; escalate if local returns `escalate_to_claude(reason)`

The classifier uses the same model instance as the local coding agent. No second model or second inference server instance.

---

## Local Agent

- Backend: any OpenAI-compatible inference server, default `http://localhost:8080` (llama.cpp reference; also works with Ollama, LM Studio, vLLM, remote endpoints)
- Model: user-configured via `llama_model`; any tool-calling-capable model works. Tested: Qwen2.5-Coder 7B/3B, Gemma 4 E4B.
- Tool loop: standard agentic loop — call → check tool calls → execute → feed result → repeat until final answer
- Built-in tools (implemented in Go, exposed as OpenAI function schemas):
  - `bash(command string) → stdout, stderr, exit_code`
  - `grep(pattern string, path string, recursive bool) → matches`
  - `read_file(path string, offset int, limit int) → content`
  - `write_file(path string, content string) → ok` — creates parent directories
  - `edit_file(path string, old_string string, new_string string) → ok` — exact-string replacement, rejects ambiguous matches
  - `list_dir(path string) → entries` — names, types, sizes
  - `http_get(url string, max_bytes int) → body` — bounded HTTP fetch
  - `get_session_context() → history` — returns the full shared session history (both agents) so the local model can see prior Claude turns
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
milk                          # interactive REPL mode
milk [flags] <prompt>         # single-prompt mode
```

### Interactive mode

`milk` with no prompt argument starts a REPL built on charmbracelet/bubbletea. The prompt label (`[local] >`, `[claude] >`, `[claude:waiting] >`) is embedded in the textarea and reflects the current routing state, updated after each turn.

**Slash commands:** `/escalate`, `/local`, `/new`, `/drop`, `/list`, `/paste`, `/help`, `/exit`

**Memory commands:** `/learn <statement>`, `/memory [global|session|<pattern>]`, `/memory show <pattern or #id>`, `/forget <pattern or #id>`, `/export [json|<path>]`

**Panel commands:** `/panel memory` — toggle the right-side memory panel (open by default)

**Multi-line input:** Shift+Enter or Alt+Enter inserts a newline; Enter submits. Bracketed paste is handled transparently — multi-line pastes are sent as a single block.

**Keyboard:** Up/Down navigates input history (single-line mode only); Ctrl-C clears a pending force-mode flag or exits; Ctrl-D exits.

**Memory panel:** A 34-column right-side panel shows SESSION / GLOBAL / GLOBAL (core) percept sections in real time (polls every 5s). Each percept displays a short `#<6hex>` ID (dim), content wrapped to 2 lines, and weight right-aligned. Percepts updated within the last 60s are highlighted bold+yellow. Toggle with `/panel memory`.

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
    "local_below_tokens": 30,
    "escalate_keywords": ["architect", "refactor entire", "design", "explain why", "analyze", "describe", "summarize"],
    "escalate_threshold": 6,
    "local_threshold": -4,
    "local_verb_weight": -3,
    "escalate_verb_weight": 4,
    "path_ref_weight": -2,
    "code_block_weight": -2,
    "open_question_weight": 3,
    "classifier_fallback": "local",
    "local_verbs": ["grep", "find", "list", "run", "read", "fix", "debug", "show", "cat", "ls", "check", "print", "count", "search"],
    "escalate_verbs": ["architect", "design", "refactor entire", "explain why", "compare", "evaluate", "plan", "propose", "summarize", "review"]
  }
}
```

---

## Graceful Degradation

| Inference server | claude CLI | behavior |
| --- | --- | --- |
| up | available | normal routing |
| down | available | warn once per session, route all to Claude |
| up | unavailable/not installed | warn once per session, stay local-only |
| down | unavailable | error + exit |

---

## Streaming

Both agents stream output in real time:

- **Local agent**: SSE from OpenAI-compat API (`stream: true`)
- **Claude agent**: NDJSON from `--output-format stream-json`, parsed line by line

milk relays tokens to stdout as they arrive.

---

## Backlog

- Planning mode (offline, no LLM execution)
- Demotion from Claude back to local mid-session
- Web UI / TUI
- MCP server integration for local tools
- Multi-user / daemon mode
