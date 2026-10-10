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

The `#id` form accepts a short hex prefix (4–64 chars); the `#` is optional.

### Memory tools

Agents backed by an inference server (HTTP or Bedrock) get four memory tools with no configuration — the same store the slash commands above operate on (issue #172):

| Tool | Parameters | Returns |
|---|---|---|
| `record_memory` | `content`, optional `subject`, `producer`, `consumer` | new percept ID, or a "skipped — similar percept exists" note |
| `get_memory` | `query`, optional `min_confidence`, `max_results` | relevant percepts, best-first |
| `list_memory` | optional `scope`, `producer`, `consumer`, `min_w`, `pattern` | percept table |
| `forget_memory` | `id` and/or `ids` and/or `pattern` | deleted percepts — or an error |

- **Who has them**: live primary and escalation turns, workflow roles, agent-as-tool peers (`agent_<name>` calls), and background subagents (`spawn_background_agent`) — the last two inherit their invoker's memory visibility. `claude-cli` and subprocess agents (aider, smolagents) don't use milk's tool loop and keep their existing write-path tags instead.
- **`forget_memory` matching is the `/forget` contract**: `id`/`ids` accept a percept ID (leading `#` optional), an ID prefix, or a description that must match exactly one percept (several matches are ambiguous — an error listing them); `pattern` deletes every percept whose content contains the substring (the batch path). All targets resolve before anything is deleted — an unresolvable or ambiguous target returns an error-shaped result and the store is left untouched.
- **Visibility is role-derived**: `get_memory`/`list_memory` see the calling agent's own consumer tag plus shared percepts — an escalation-role call also sees `consumer: "escalation"` percepts (it used to be hardcoded to the primary view). `list_memory`'s `consumer` argument narrows to a named tag explicitly.
- **Limits apply**: `limits.included_tools`/`limits.excluded_tools` scope the memory tools like any other built-in. The system prompt's memory-tool mandate is only injected when the tools actually survived that filtering — a model is never told to call a tool it doesn't have.

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
| `/usage` | Token usage report — cumulative, this session, since start — broken down by agent role and model, with a `tok/s` throughput column |

### Throughput (tok/s)

milk measures generation throughput per completion request and aggregates it per turn and per (model, role):

- **Decode tok/s** — output tokens ÷ the generation window (first streamed output token → last). Tool execution and other between-request time is excluded, so a slow tool call never dilutes the rate.
- **TTFT** — request send → first output token (queue + prefill).

Sources: local/Bedrock/Responses providers are timed directly from the SSE stream; claude-cli turns use Claude Code's own `duration_api_ms` / `ttft_ms` from the result event; generic subprocess agents (aider, smolagents) report no timing and show `—` rather than a fabricated number.

Where it shows: the TUI status bar (live `tok/s` estimate while streaming; `(last:↑…↓… · 42 tok/s · ttft 0.8s)` when idle), the `/usage` table's `tok/s` column, and the stream-json result event's `output_tokens_per_second` / `ttft_ms` fields. OTel counters/histograms: `milk.tokens.decode_seconds` (summed, the /usage denominator), `milk.tokens.ttft_ms`, `milk.tokens.decode_ms`.

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

milk monitors agent output for signs of looping — repeating the same phrase, tool call, or response pattern — to prevent runaway token consumption when nobody notices and interrupts manually. Four complementary systems work together:

### Agent-internal detectors (`internal/agent/local/`)

Operate inside the local agent's tool iteration loop. All share the same recovery flow: crop looping messages from context, inject an escalating recovery nudge (mild → strong), then terminate after max attempts.

| Detector | File | Catches | How |
|---|---|---|---|
| **Streak tracker** | `loop_streak.go` | Same reasoning hash or tool-call signature across consecutive iterations | SHA-256 of normalised reasoning (truncated to 500 chars, leading phrases stripped) |
| **Streaming n-gram** | `reasoning_ngram.go` | Periodic reasoning repetition during streaming ("I'm done → let me check → I'm done") | Sliding 500-token window, detects blocks of 4+ tokens repeating 10+ times consecutively. **Cuts the stream immediately** to save tokens. |
| **Text-loop tracker** | `loop_streak.go` | Same output text across consecutive steps | Normalised text (200 chars, leading phrases stripped) compared across steps |
| **Duplicate tool calls** | `local.go` | Model re-issues a tool call already executed with identical arguments | Exact match on tool name + arguments. Nudges first (similarly to MiMo-Code's approach); terminates after max recovery. |
| **Doom-loop gate** | `local.go`, `loop_streak.go` | 3 *consecutive* iterations issuing the exact same tool-call batch (stronger signal than "duplicate tool calls" above, which fires on any repeat seen anywhere earlier in the turn) | Not a nudge — raises an interactive permission ask before letting the model continue, or fails closed immediately for a background job/workflow step (no one to ask) |

### TUI-level detector (`internal/loop/detector.go`)

Operates at the streaming/TUI layer. Catches patterns the agent-internal trackers can't see:

| Signal | Scope | Catches | Default threshold |
|---|---|---|---|
| `chunk_repetition` | Intra-turn | Same text repeating consecutively in streaming output | 5 occurrences / 50-chunk window |
| `chunk_repetition` (scattered) | Intra-turn | The same chunk recurring within the window without needing to be back-to-back | Chunks ≥ 40 runes only, to avoid flagging short boilerplate phrases |
| `reasoning_chunk_flood` | Intra-turn | Too many reasoning chunks without content output | 5000 chunks |
| `token_velocity` | Cross-turn | Rapid token consumption without progress | 300k tokens / 60s |
| `silent_burn` | Per-turn | High input tokens, near-zero output | 20k input tokens |
| `turn_flood` | Session | Excessive turns without user input | 10 consecutive non-user turns |

Note: consecutive reasoning chunk repetition (`SignalReasoningChunkRepetition`) was removed from the TUI detector — it is now handled more effectively by the streaming n-gram detector, which cuts the stream immediately instead of just warning.

### Try-best detector (`internal/loop/try_best.go`)

Operates at the tool-execution layer. Catches the most common real-world loops:

| Signal | Catches | Default threshold |
|---|---|---|
| `edit_repeat` | Near-identical edits to the same file (Jaccard similarity on normalized diffs) | 0.8 similarity × 2 prior matches in window of 12 |
| `bash_retry` | Same failing bash command retried without success | 3 consecutive failures |
| `action_streak` | Non-progressing actions of the same kind (edit or verify) | 4 consecutive failures |

**Intra-turn** (the primary case): every streaming chunk passes through a ring buffer of the last 50 chunks, checked two ways — consecutive identical chunks, and (for longer chunks only) the same chunk recurring anywhere in the window without needing adjacency. Either one auto-interrupts the turn. **Cross-turn**: after each turn, token velocity, silent burn, and turn count are checked. **Tool-level**: after each tool call, edit similarity, bash retries, and action streaks are checked.

Transcript messages split two categories: `[⚠ loop detected: …]` for genuine repetition-based loop evidence (chunk/n-gram repetition, streaks, duplicate tool calls, doom loop) and `[⚠ consumption: …]` for consumption/volume threshold crossings (`reasoning_chunk_flood`, `token_velocity`, `silent_burn`, `turn_flood`) — a counter crossing a threshold is a burn-rate warning, not evidence of a loop. No numeric confidence percentage is shown anywhere: the internal severity score is a fixed per-signal constant, not a measured probability (`token_velocity`, the one signal with a computed heuristic, carries a qualitative `severity:` word instead). The status bar shows `[⚠ <category> — auto-interrupted]` when a signal auto-interrupts the turn, or `[⚠ <category>: <message>]` for warn-only signals. A user turn resets all warnings and the turn-flood counter. Works identically across every provider — the intra-turn monitor sits at the TUI layer, not inside any specific agent driver.

```json
{
  "loop_detection": {
    "enabled": true,
    "chunk_repetition_threshold": 5,
    "chunk_window_size": 50,
    "chunk_repetition_min_scattered_length": 40,
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

The panel also opens itself the first time a task is created in the session (see [Keyboard shortcuts](#keyboard-shortcuts) for the auto-open/manual-override rule shared by all four side panels).

Two guards keep an agent that lost its context from re-planning the same work: the primary agent's user message carries a bounded system-reminder of the session's open tasks (15 tasks, 100 chars per title; background-job, workflow and tool-agent prompts don't get it), and `create_task` returns the existing open task when the new title matches an open one (case- and whitespace-insensitive) instead of piling up a copy.

---

## Background sub-agents

`spawn_background_agent` (ADR-0043 — see [docs/tooling.md](tooling.md#spawn_background_agent--forking-yourself-for-background-research) for the tool itself) forks an inference-server-backed agent to research a self-contained question asynchronously, one job per call, tracked for the life of the session by a per-session job manager.

| Field | Default | Meaning |
|---|---|---|
| `max_background_agents` | `3` | Maximum number of background jobs allowed to actually execute concurrently per session; further calls queue rather than block the spawning turn. Non-positive values fall back to the default. |
| `background_agent_timeout_minutes` | `20` | Per-job hard timeout once a job starts executing (queue time doesn't count); a job still running past this is terminated as failed, not retried. Deliberately generous: loop detection (streak tracker, streaming n-gram monitor, duplicate-tool-call detection — all run unmodified on a background job) is the actual defense against a job that's *stuck*; this timeout only needs to catch one that's genuinely still working but never finishing. Non-positive values fall back to the default. |

Completed/failed jobs surface three ways: immediately in the TUI as each one finishes — a toast with the lifecycle notice (see [Notification toasts](#notification-toasts)) **and the job's result appended to the transcript** (the result is the job's content, not lifecycle noise: it is capped to the same budget the model receives, with `/bg show <id>` printing it in full, so it can never pass by unseen just because the model chose not to report it) plus the status bar (`⚙ N background agent(s) running`); a live list in the background-agents panel (`/panel background` or **F3** — label, status, elapsed time; mirrors the tasks/memory panels; opens itself the moment a job is spawned, not just once one finishes — see [Keyboard shortcuts](#keyboard-shortcuts)); and, once the model is next free, an actual follow-up turn the agent produces automatically — no further input needed. Agent-initiated waves (the `spawn_background_agent` tool call) wait for every job in the wave to finish before that follow-up fires, so a multi-part research plan gets one consolidated report; a job the *user* spawns directly (**Ctrl+Enter** while the model is busy — requires terminal support for extended key protocols, or **Ctrl+J** as a universal fallback) delivers as soon as the model is free instead, since there's no wave to consolidate it with.

The `/bg` slash command manages background agents interactively:

| Command | Action |
|---|---|
| `/bg` or `/bg list` | List all background agents (ID, status, label, elapsed time) |
| `/bg show <id>` | Print one job's full result (status metadata, error, or result text — uncapped) |
| `/bg start <task>` | Spawn a background agent to research `<task>` (same as Ctrl+Enter while busy) |
| `/bg stop <id>` | Terminate a running background agent by its ID |

`/bg` is safe to use while an agent turn is in progress (it never dispatches a new turn). Use `/bg list` to see job IDs, then `/bg stop job_N` to cancel one that's no longer needed.

The spawning agent has a model-facing equivalent to `/bg stop`: a `cancel_background_agent(job_id)` tool call, using the same cancellation path, for when the agent itself decides mid-turn that a job it spawned is no longer needed (e.g. the user's request changed).

**Watching a job (or workflow) live** (ADR-0047): double-click a job's row in the background panel (**F3**), or the workflow panel (**F4**) while a `/workflow` is running, to swap the main transcript for that job's/workflow's own live output — its tool calls and streamed text, kept off the main transcript the whole time, not just summarized after the fact. Esc detaches back to the main transcript, which keeps accumulating underneath the whole time. The buffer keeps growing after the job/workflow finishes, so re-attaching (or never detaching) still shows the full output; nothing auto-detaches on completion. `obs.Debug` logs `attach.start`/`attach.stop` with the job/workflow ID and label, so this is checkable from `milk.log` even without watching the terminal live.

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

**Over ACP** (`milk serve --acp`): everything above runs too, with one notifier per serve process — permission prompts race the ACP client (first answer wins, remote asks serialized process-wide), and bot messages run as turns in the live session the user touched most recently (queued until one exists if none do), echoed to the client as ordinary user messages. See docs/acp-integration.md's "Remote oversight (Telegram)" section.

---

## Inline diff view

When an agent calls `edit_file`/`write_file` (primary) or `Edit`/`Write` (Claude CLI), milk renders a colored inline diff in the transcript right after the tool-hint line — deleted lines in red, added in green, 3 lines of context on each side.

## Keyboard shortcuts

| Shortcut | Action |
|---|---|
| **Enter** | Submit prompt / accept tab completion (accepting a completion or hint works while an agent turn is running too — only plain Enter-as-submission is trapped mid-turn) |
| **Ctrl+Enter** | Spawn a background agent from the current input (while an agent turn is in progress). **Ctrl+J** works as a universal fallback in terminals without extended key protocols. |
| **Tab** | Cycle slash-command and @-path completions (also available while a turn is in progress) |
| **Shift-Tab** | Reverse cycle completions |
| **Ctrl-C** | Copy selection → clear input → cancel workflow/turn → quit (double press) |
| **Ctrl-D** | Quit (when input is empty) |
| **Ctrl-R** | Search backward in transcript |
| **Ctrl-S** | Search forward in transcript |
| **Ctrl-Left/Right** | Word navigation |
| **Shift-Arrows** | Text selection (transcript and input) |
| **Ctrl-X** | Cut selected input text |
| **F1 / F2 / F3 / F4** | Show/hide the memory / tasks / background-agents / workflow panel — same effect as `/panel <name>`. Works in any mode, including mid-turn. |
| **Ctrl+G** | Dismiss all open notification toasts (history stays: `/notifications`) |

Turn-unrelated informational events — thinking-visibility switches, panel open/close, background-job lifecycle, credential refreshes — surface as floating toasts in the top-right corner (timestamped, each with the related slash-command hint, auto-expiring after a few seconds). They are **not** part of the transcript; `/notifications` shows the full history with timestamps (`/notifications clear` empties them). Tool calls and turn-bound output stay in the transcript. One deliberate exception: a finished background job's *result* is content rather than lifecycle noise and is appended to the transcript as the job completes (capped to the model's delivery budget, `/bg show <id>` for the rest — see [Background sub-agents](#background-sub-agents)); only its lifecycle *notice* stays toast-only.

Each panel's title shows its shortcut (e.g. `tasks    F2`) as a reminder. The tasks, background-agents, and workflow panels also open themselves automatically the moment their content becomes active — a task is created, a background job starts, a workflow launches — so you don't have to notice and press the shortcut first. Manually toggling a panel (`/panel <name>` or its F-key) overrides this for the rest of the session: once you've explicitly shown or hidden a panel, automatic management leaves it alone, in either direction. When two or more panels are open, every other one (by left-to-right position among the panels currently showing, not a fixed panel) gets a subtle background tint so adjacent panels are easier to tell apart.
