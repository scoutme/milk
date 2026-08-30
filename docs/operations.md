# Operations

Running milk day to day: memory, observability, loop detection, task tracking, config reload, and graceful degradation.

---

## Memory

milk keeps a **Percept store** — small remembered facts that survive across sessions, separate from conversation history. Percepts are reinforced when relevant, decay when not, and get promoted to long-term ("core") status over time (NREM consolidation, run at session end).

### Commands

| Command | Action |
|---|---|
| `/learn <statement>` | Explicitly remember a fact |
| `/memory [global\|session\|<pattern>]` | List percepts, optionally scoped or filtered |
| `/memory show <pattern or #id>` | Show full detail for matching percepts |
| `/forget <pattern or #id>` | Delete matching percepts |
| `/panel memory` | Toggle the right-side memory panel (open by default) |

The `#id` form accepts a short hex prefix (4–64 chars); the `#` is optional. The primary agent can also call `get_memory`, `list_memory`, and `forget_memory` directly (same short-ID resolution).

The memory panel is a 34-column right-side panel with SESSION / GLOBAL / GLOBAL (core) sections, polling every 5s. Each percept shows a `#<6hex>` ID, content wrapped to 2 lines, and a right-aligned weight; percepts updated in the last 60s are highlighted.

### Tuning

The knobs that control injection limits, re-injection cadence, and per-agent overrides live in [docs/providers.md — Memory configuration](providers.md#memory-configuration) and [Context budget configuration](providers.md#context-budget-configuration), since they're usually set per-agent alongside the rest of that agent's config.

---

## Observability

milk exports OpenTelemetry-shaped signals to JSONL files under `~/.milk/otel/` — no external backend required.

| Command | Action |
|---|---|
| `/metrics` | Latest value for each metric+label combination |
| `/otel` | File sizes, record counts, timestamp bounds |
| `/otel trim` | Archive current files, recreate empty ones |
| `search_signals` (tool) | Case-insensitive search over the raw JSONL |
| `/usage` | Token usage report — cumulative, this session, since start — broken down by agent role and model |

### Config

```json
{
  "otel": {
    "enabled": true,
    "log_level": "DEBUG",
    "log_context": true,
    "traces": true,
    "metrics": true,
    "warn_mb": 50,
    "max_mb": 0,
    "metrics_flush_minutes": 5
  }
}
```

`otel.log_context: true` logs the full content of every request payload at DEBUG level to `~/.milk/otel/logs.jsonl` (requires `log_level: "DEBUG"`) — covers the claude-cli static/dynamic context files and prompt, the full local/Bedrock inference request body, and subprocess agents' context/prompt temp files.

### Raw debug logs

Separate from OTel, three flags capture raw protocol traffic verbatim:

| Field | Default | Writes to |
|---|---|---|
| `debug_claude_code` | `false` | `~/.milk/claude_debug.ndjson` — every raw NDJSON line from the Claude CLI subprocess |
| `debug_local` | `false` | `~/.milk/local_debug.log` — every raw SSE line from the local/Bedrock agent's HTTP stream, including unparsed/blank lines |
| `debug_subprocess` | `false` | `~/.milk/subprocess_debug.log` — every raw stdout line from subprocess agents (aider, smolagents) |

`milk otel debug enable` turns all of the above on in one command (and prints the paths); `milk otel debug disable` reverts them.

---

## Loop detection

milk monitors agent output for signs of looping — repeating the same phrase, tool call, or response pattern — to prevent runaway token consumption when nobody notices and interrupts manually. Two complementary systems work together:

### Agent-internal streak tracker (`internal/agent/local/loop_streak.go`)

Operates inside the local agent's tool iteration loop. Detects when the model repeats the same reasoning or tool-call pattern across consecutive iterations using SHA-256 hashing. Recovery: injects a mild recovery nudge, then a strong nudge, then terminates the turn. Also **crops looping messages from context** so the model can't see its own loop anymore — significantly more effective than just nudging.

### TUI-level detector (`internal/loop/detector.go`)

Operates at the streaming/TUI layer. Catches patterns the agent-internal tracker can't see:

| Signal | Scope | Catches | Default threshold |
|---|---|---|---|
| `chunk_repetition` | Intra-turn | Same text repeating consecutively in streaming output | 5 occurrences / 50-chunk window |
| `chunk_repetition` (scattered) | Intra-turn | The same chunk recurring within the window without needing to be back-to-back | Chunks ≥ 40 runes only, to avoid flagging short boilerplate phrases |
| `reasoning_chunk_repetition` | Intra-turn | Same text repeating in streaming reasoning output | 10 / 50-chunk window |
| `reasoning_chunk_flood` | Intra-turn | Too many reasoning chunks without content output | 5000 chunks |
| `token_velocity` | Cross-turn | Rapid token consumption without progress | 300k tokens / 60s |
| `silent_burn` | Per-turn | High input tokens, near-zero output | 20k input tokens |
| `turn_flood` | Session | Excessive turns without user input | 10 consecutive non-user turns |

### Try-best detector (`internal/loop/try_best.go`)

Operates at the tool-execution layer. Catches the most common real-world loops:

| Signal | Catches | Default threshold |
|---|---|---|
| `edit_repeat` | Near-identical edits to the same file (Jaccard similarity on normalized diffs) | 0.8 similarity × 2 prior matches in window of 12 |
| `bash_retry` | Same failing bash command retried without success | 3 consecutive failures |
| `action_streak` | Non-progressing actions of the same kind (edit or verify) | 4 consecutive failures |

**Intra-turn** (the primary case): every streaming chunk passes through a ring buffer of the last 50 chunks, checked two ways — consecutive identical chunks, and (for longer chunks only) the same chunk recurring anywhere in the window without needing adjacency. Either fires at high confidence and auto-interrupts the turn. **Cross-turn**: after each turn, token velocity, silent burn, and turn count are checked. **Tool-level**: after each tool call, edit similarity, bash retries, and action streaks are checked.

Status bar shows `⚠ loop — auto-interrupting` (high confidence) or `⚠ <signal>` (warning); the transcript logs `[⚠ loop detected: <signal> (confidence N%)]`. A user turn resets all warnings and the turn-flood counter. Works identically across every provider — the intra-turn monitor sits at the TUI layer, not inside any specific agent driver.

```json
{
  "loop_detection": {
    "enabled": true,
    "chunk_repetition_threshold": 5,
    "chunk_window_size": 50,
    "chunk_repetition_min_scattered_length": 40,
    "reasoning_chunk_repetition_threshold": 10,
    "reasoning_chunk_flood_threshold": 5000,
    "token_velocity_window_seconds": 60,
    "token_velocity_threshold": 300000,
    "max_silent_burn_tokens": 20000,
    "max_consecutive_turns_without_user": 10,
    "auto_interrupt": false
  }
}
```

Default: detection on, `auto_interrupt` off (warn only). Set `auto_interrupt: true` for unattended sessions.

---

## Persistent task tracking

A lightweight task tracker for the primary agent (HTTP/Bedrock backends only — subprocess and claude-cli agents don't receive these tools), stored in `~/.milk/tasks/<session-id>.json` (session-scoped) and `~/.milk/tasks/global.json` (cross-session, survives restart).

| Tool | Parameters | Returns |
|---|---|---|
| `create_task` | `title`, `tags?` | `{"id": "<8-char id>"}` |
| `update_task` | `id`, `status` (`pending`\|`in_progress`\|`done`\|`blocked`), `title?` | `"ok"` |
| `list_tasks` | `include_global?` | `[{id, title, status, tags}]` |
| `complete_task` | `id` | `"ok"` |

| Command | Description |
|---|---|
| `/tasks` | List session + global tasks inline |
| `/task done <id>` | Mark done (accepts id prefix ≥ 4 chars) |
| `/panel tasks` | Toggle the tasks side-panel (32 cols), auto-updates as the agent works |

---

## Live configuration reload

milk watches `~/.milk/config.json` while the TUI is running; a save from another terminal is parsed and applied to in-memory state within ~200ms. `/reload` forces an immediate re-parse (useful after a symlink swap or atomic editor replace).

On success: `[milk] config reloaded`. On error: `[milk] config reload error: <reason>` — the existing in-memory config is kept, nothing crashes.

**Hot-reloaded**: scalar fields (`direct_bash`, `show_reasoning`, `sticky_escalation`, routing rules, OTel settings, …), agent configs (effective next turn), and MCP server connections (stale connections closed and the toolset rebuilt for affected roles, gated on an actual change — a turn already in flight keeps its original snapshot).

**Not hot-reloaded**: a running turn's config snapshot, new `TurnRunner` instances for the `agents` list (built next turn), and MCP OAuth authorization (still requires the interactive `/mcp auth <server>` flow).

### Recovering from a corrupted `config.json`

Two safety nets:
- **While running**: the reload path above keeps the last-known-good in-memory config on a parse error — nothing is lost, the session keeps working.
- **At startup**: if `config.json` fails to parse, milk falls back to `config.json.bak` (refreshed on every successful save/parse) and starts with a warning instead of refusing to launch. Only a `config.json` that was *never* successfully loaded before (no backup exists yet) hard-fails, pointing at `milk config open`.

---

## Graceful degradation

| Primary agent | Escalation agent | Behavior |
|---|---|---|
| up | available (any provider) | normal routing |
| down | available | warn once per session, route everything to escalation |
| up | unavailable/not installed | warn once per session, stay primary-only |
| down | unavailable | error and exit |

---

## Remote oversight (Telegram)

Forward agent activity and permission prompts to a mobile device.

**Quick setup** (interactive wizard): `/setup telegram` — paste your bot token from @BotFather, message the bot, milk resolves your chat ID and saves the config automatically.

**Manual config** (token/chat_id redacted — get these from @BotFather and your first message to the bot):

```json
{
  "remote_oversight": {
    "backend": "telegram",
    "telegram": { "token": "<bot-token-from-botfather>", "chat_id": "<your-numeric-chat-id>" },
    "perm_timeout_secs": 120,
    "timeout_action": "deny",
    "notify_tools": true
  }
}
```

**Enable/disable at runtime** (credentials preserved): `/setup telegram on` / `/setup telegram off`.

| Key | Default | Description |
|---|---|---|
| `backend` | `""` | `"telegram"` to enable, `""` to disable |
| `perm_timeout_secs` | 120 | Wait time for a remote permission reply before `timeout_action` |
| `timeout_action` | `"deny"` | `"allow"` or `"deny"` on timeout |
| `notify_tools` | `true` | Forward tool-call notifications |

**Forwarded**: turn start (agent, target, prompt snippet — workflow turns labeled `workflow:<role>`), tool calls and results (truncated to 500 chars), response text (capped at 3000 chars, streamed as it's produced), permission prompts with y/n reply (first response from either surface wins).

**Remote input**: any message sent to the bot is injected as a new turn (`[telegram] …` in the transcript); queued while a turn is in progress, delivered as the next turn once it completes.

---

## Inline diff view

When an agent calls `edit_file`/`write_file` (primary) or `Edit`/`Write` (Claude CLI), milk renders a colored inline diff in the transcript right after the tool-hint line — deleted lines in red, added in green, 3 lines of context on each side.

## Keyboard shortcuts

See the [keyboard shortcuts reference in README.md](../README.md#interactive-mode).
