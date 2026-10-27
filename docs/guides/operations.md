# Operations

Running milk day to day: memory, observability, loop detection, task tracking, config reload, and graceful degradation.

---

## Memory

milk keeps a **Percept store** — small remembered facts that survive across sessions, separate from conversation history. Percepts are **reinforced** when re-stated (a repeated fact gains weight), **decay** when not (session end runs NREM consolidation: decay → edges → prune → promote), and graduate to the global store when their weight crosses the promotion threshold. The full weight model is below ([Memory weight model](#memory-weight-model)) and in [ADR-0053](../adr/0053-memory-weight-model.md).

### Commands

| Command | Action |
|---|---|
| `/learn <statement>` | Explicitly remember a fact |
| `/memory [global\|session\|<pattern>]` | List percepts, optionally scoped or filtered |
| `/memory show <pattern or #id>` | Show full detail for matching percepts |
| `/forget <pattern or #id>` | Delete matching percepts |
| `/panel memory` | Toggle the right-side memory panel (closed by default; opens when memory content is present) |

The `#id` form accepts a short hex prefix (4–64 chars); the `#` is optional.

### Memory tools

Agents backed by an inference server (HTTP or Bedrock) get four memory tools with no configuration — the same store the slash commands above operate on (issue #172):

| Tool | Parameters | Returns |
|---|---|---|
| `record_memory` | `content`, optional `subject`, `producer`, `consumer`, `relation`, `related_to` | new percept ID — or a "reinforced existing percept" note (near-duplicates strengthen instead of duplicating); `relation`/`related_to` link the fact to an existing percept (see below) |
| `get_memory` | `query`, optional `min_confidence`, `max_results` | relevant percepts, best-first |
| `list_memory` | optional `scope`, `producer`, `consumer`, `min_w`, `pattern` | percept table |
| `forget_memory` | `id` and/or `ids` and/or `pattern` | deleted percepts — or an error |

- **Who has them**: live primary and escalation turns, workflow roles, agent-as-tool peers (`agent_<name>` calls), and background subagents (`spawn_background_agent`) — the last two inherit their invoker's memory visibility. `claude-cli` and subprocess agents (aider, smolagents) don't use milk's tool loop and keep their existing write-path tags instead.
- **`forget_memory` matching is the `/forget` contract**: `id`/`ids` accept a percept ID (leading `#` optional), an ID prefix, or a description that must match exactly one percept (several matches are ambiguous — an error listing them); `pattern` deletes every percept whose content contains the substring (the batch path). All targets resolve before anything is deleted — an unresolvable or ambiguous target returns an error-shaped result and the store is left untouched.
- **Visibility is role-derived**: `get_memory`/`list_memory` see the calling agent's own consumer tag plus shared percepts — an escalation-role call also sees `consumer: "escalation"` percepts (it used to be hardcoded to the primary view). `list_memory`'s `consumer` argument narrows to a named tag explicitly.
- **Limits apply**: `limits.included_tools`/`limits.excluded_tools` scope the memory tools like any other built-in. The system prompt's memory-tool mandate is only injected when the tools actually survived that filtering — a model is never told to call a tool it doesn't have.

### Memory weight model

Every Percept carries a confidence weight `w` in `[0,1]`. Five forces move it (ADR-0053). "Consolidation" below runs after every turn handled by the primary agent, when milk exits, and after a single-prompt run — so the per-consolidation decay amounts tick per turn, not once per session:

1. **Birth** — `user` facts start at 0.9, inference (`primary`/`escalation`) at 0.7, system hints at 0.4, `core` (`/learn`) at 1.0.
2. **Reinforcement (re-confirmation)** — re-stating a near-duplicate fact (Jaccard token overlap ≥ 0.60) strengthens the existing percept instead of inserting a copy: `w += 0.05`, capped at 0.90, `updated_at` touched (which also restores the recency bonus). The cap is `promote_threshold + decay_per_session` on purpose: a fully re-confirmed inference percept survives the pre-promotion decay and graduates at the next consolidation — repeated confirmation is a real promotion path. Weights never decrease through reinforcement.
3. **NREM consolidation** (session end) — session percepts decay −0.10, typed edges fire (below), percepts at `w ≤ 0.20` are pruned, and `w ≥ 0.80` percepts are promoted to the global store (promotion happens *after* the global pass, so fresh promotions aren't decayed twice).
4. **Global decay and archiving** — promoted (non-core) global percepts get a gentle −0.02 per consolidation, floored at 0.20: old facts sink in rank. Decay alone never removes them; **staleness** does (below). `core` percepts never decay and are never archived.
5. **Recency (recall ordering only)** — `get_memory` orders by token-hit count, then `w + 0.10 × 0.5^(age/30d)`: a recently updated fact wins ties but a more relevant old fact always ranks first, and `min_confidence` gates raw `w` — age never hides a fact.

**Stale global percepts are archived** (ADR-0056). A global percept is stale when it has decayed to `prune_threshold` *and* has gone unused for `global_archive_after_days` (default 14) of milk running time. The clock counts the time milk is running, summed over sessions — one 20-day session counts like ten 2-day ones — and is independent of how often consolidation runs. *Used* means re-stated, returned by `get_memory`, or injected into a prompt; any use restarts the clock. Stale percepts are **moved**, not deleted, to `~/.milk/memory/global-archive.jsonl` (one JSON line each, with when and why, plus the edges touching them). Percepts that existed before this feature start their clock at zero, so nothing is archived right after an upgrade. Set `global_archive_after_days` negative to disable.

- `/memory stale` — global percepts by idle time and what is due for archiving.
- `/memory archive` — list the archived ones.
- `/memory restore <id>` — bring one back (its clock restarts).

**Edges** (wired via `record_memory`'s `relation` + `related_to`): `extends`/`updates`/`derives` corroborate the target (+0.05 at consolidation), `contradicts` marks a conflict (both endpoints −0.10). The producer agent decides semantics — milk never fabricates relations. Edges are stored with the "from" percept's store file and apply to endpoints in that same file.

All knobs are prepopulated in the generated config's `memory` block (`decay_per_session`, `global_decay_per_session`, `reconfirm_boost`, `reconfirm_cap`, `promote_threshold`, `prune_threshold`, `recency_boost`, `recency_half_life_days`, `global_archive_after_days`); zero/missing fields fall back to the defaults above. OTel: `milk.memory.consolidation.*` counters (including `global_decayed`) and `milk.memory.percept_events{event=reinforce}`, and `milk.memory.global_archived`.

### Tuning

The knobs that control injection limits, re-injection cadence, and per-agent overrides live in [docs/guides/providers.md — Memory configuration](providers.md#memory-configuration) and [Context budget configuration](providers.md#context-budget-configuration), since they're usually set per-agent alongside the rest of that agent's config.

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

### Colored structured log (`log_format: "pretty"`)

`milk.log` is written as `text` by default. `"log_format": "pretty"` writes the same records through Charm's `log` handler: colored level, timestamp, message and `key=value` attributes. Colors are forced into the file, so read it with `tail -f` or `less -R`. `milk otel debug enable` switches a plain format to `pretty` and `milk otel debug disable` restores the previous one (an explicit `pretty`, `json` or `off` is respected; the change applies from the next start).

Every answer from an agent logs one `agent response` line at INFO, so it shows without debug: `agent`, `model`, `role`, `input`, `output`, `cache_read`/`cache_write`, `ttft_ms`, `tok_s` and `cost_usd`. Values the provider did not report are left out, never written as 0.

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
| **Duplicate tool calls** | `local.go` | Model re-issues a tool call already executed with identical arguments while *nothing changed in between* | Exact match on tool name + arguments. Read-only tools (reads, searches, `bash_output`, `web_fetch`, status readers) never count in a mixed batch; a pure read-only batch is only flagged after `readOnlyRepeatLimit` (3) identical executions. Any file write **or `bash` call** resets the memory (`resetExecutedKeysAfterStateChange`) — state changes made through `bash` (`git checkout`, `gh pr merge`, …) used to make legitimate re-check batches look like loops. Nudges first (similarly to MiMo-Code's approach); terminates after max recovery. |
| **Doom-loop gate** | `local.go`, `loop_streak.go` | 3 *consecutive* iterations issuing the exact same tool-call batch (stronger signal than "duplicate tool calls" above, which fires on any repeat seen anywhere earlier in the turn) | Not a nudge — raises an interactive permission ask before letting the model continue, or fails closed immediately for a background job/workflow step (no one to ask) |

### TUI-level detector (`internal/loop/detector.go`)

Operates at the streaming/TUI layer. Catches patterns the agent-internal trackers can't see:

| Signal | Scope | Catches | Default threshold |
|---|---|---|---|
| `chunk_repetition` | Intra-turn | Same text repeating consecutively in streaming output | 5 occurrences / 50-chunk window |
| `chunk_repetition` (scattered) | Intra-turn | The same chunk recurring within the window without needing to be back-to-back | Chunks ≥ 40 runes only, to avoid flagging short boilerplate phrases |
| `reasoning_chunk_flood` | Intra-turn | Too many reasoning chunks with no content output or tool call since the last progress | 5000 chunks |
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

Transcript messages split two categories: `[⚠ loop detected: …]` for genuine repetition-based loop evidence (chunk/n-gram repetition, streaks, duplicate tool calls, doom loop) and `[⚠ consumption: …]` for consumption/volume threshold crossings (`reasoning_chunk_flood`, `token_velocity`, `silent_burn`, `turn_flood`) — a counter crossing a threshold is a burn-rate warning, not evidence of a loop. `reasoning_chunk_flood` counts reasoning chunks *since the last observable progress*: content output or a committed tool call resets the accumulation, so a tool-heavy turn that keeps producing never builds toward its limit — only an unbroken thinking burst with no output and no tool call does. No numeric confidence percentage is shown anywhere: the internal severity score is a fixed per-signal constant, not a measured probability (`token_velocity`, the one signal with a computed heuristic, carries a qualitative `severity:` word instead). The status bar shows `[⚠ <category> — auto-interrupted]` when a signal auto-interrupts the turn, or `[⚠ <category>: <message>]` for warn-only signals. A user turn resets all warnings and the turn-flood counter. Works identically across every provider — the intra-turn monitor sits at the TUI layer, not inside any specific agent driver.

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

`spawn_background_agent` (ADR-0043 — see [docs/guides/tooling.md](tooling.md#spawn_background_agent-forking-yourself-for-background-research) for the tool itself) forks an inference-server-backed agent to research a self-contained question asynchronously, one job per call, tracked for the life of the session by a per-session job manager. The model-facing lifecycle tools (`status_background_agent`, `wait_background_agent`, `send_background_agent` — issue #250) let the spawning agent check on jobs, block on a result when genuinely dependent on it, and deliver follow-up input to a running job; the human side is `/bg`, `/bg show`, and `/bg stop`.

| Field | Default | Meaning |
|---|---|---|
| `max_background_agents` | `3` | Maximum number of background jobs allowed to actually execute concurrently per session; further calls queue rather than block the spawning turn. Non-positive values fall back to the default. |
| `background_agent_timeout_minutes` | `20` | Per-job hard timeout once a job starts executing (queue time doesn't count); a job still running past this is terminated as failed, not retried. Deliberately generous: loop detection (streak tracker, streaming n-gram monitor, duplicate-tool-call detection — all run unmodified on a background job) is the actual defense against a job that's *stuck*; this timeout only needs to catch one that's genuinely still working but never finishing. Non-positive values fall back to the default. |

Completed/failed jobs surface three ways: immediately in the TUI as each one finishes — a toast with the lifecycle notice (see [Notification toasts](#notification-toasts)) **and the job's result appended to the transcript** (the result is the job's content, not lifecycle noise: it is capped to the same budget the model receives, with `/bg show <id>` printing it in full, so it can never pass by unseen just because the model chose not to report it) plus the status bar (`» N background agent(s) running`); a live list in the background panel (`/panel background` or **F3** — label, status, elapsed time; mirrors the tasks/memory panels; opens itself the moment a job is spawned, not just once one finishes — see [Keyboard shortcuts](#keyboard-shortcuts); background shells started via `bash run_in_background` list in the same panel under `shells (n)` and double-click-attach to their live output — [ADR-0064](../adr/0064-background-shell-surface.md)); and, once the model is next free, an actual follow-up turn the agent produces automatically — no further input needed. Agent-initiated waves (the `spawn_background_agent` tool call) wait for every job in the wave to finish before that follow-up fires, so a multi-part research plan gets one consolidated report; a job the *user* spawns directly (**Ctrl+Enter** while the model is busy — requires terminal support for extended key protocols, or **Ctrl+J** as a universal fallback) delivers as soon as the model is free instead, since there's no wave to consolidate it with.

The `/bg` slash command manages background agents — and background shells — interactively (one merged surface, [ADR-0064](../adr/0064-background-shell-surface.md)):

| Command | Action |
|---|---|
| `/bg` or `/bg list` | List all background agents and shells (ID, status, label/command, elapsed time) |
| `/bg show <id>` | Print one job's full result (status metadata, error, or result text — uncapped), or one shell's state and current output |
| `/bg start [--agent <name>] <task>` | Spawn a background agent to research `<task>` (same as Ctrl+Enter while busy, which has no flag and asks instead) |
| `/bg stop <id>` | Terminate a running background agent — or kill a background shell (`/bg stop s1` is the UI's `kill_bash`) — by its ID |

**Which agent runs a user-started job** works like a workflow role: `--agent <name>` takes any configured inference-server-backed agent or the aliases `primary` / `escalation`; without it the TUI asks (`background agent [...] (blank = <default>):`, Esc cancels) whenever more than one such agent is configured. The default — a blank answer, ACP (which has no prompt), or `/bg start` with a single candidate — is the `background_agent` config key if set, otherwise the escalation agent's inference-server backend, else the primary's. The job carries the chosen agent's model and MCP servers, so a job on `gemma` sees only `gemma`'s `mcp_servers`. (A job an *agent* spawns with `spawn_background_agent` always forks that agent itself.)

`/bg` is safe to use while an agent turn is in progress (it never dispatches a new turn). Use `/bg list` to see job IDs, then `/bg stop job_N` to cancel one that's no longer needed.

The spawning agent has a model-facing equivalent to `/bg stop`: a `cancel_background_agent(job_id)` tool call, using the same cancellation path, for when the agent itself decides mid-turn that a job it spawned is no longer needed (e.g. the user's request changed).

**Watching a job (or workflow) live** (ADR-0047): double-click a job's row in the background panel (**F3**), or the workflow panel (**F4**) while a `/workflow` is running, to swap the main transcript for that job's/workflow's own live output — its tool calls and streamed text, kept off the main transcript the whole time, not just summarized after the fact. Esc detaches back to the main transcript, which keeps accumulating underneath the whole time. A workflow with a `parallel_group` (e.g. `swarm`) shows a header note there: its workers run without live output, so the fan-out stage shows only `[item N] started`/`done`/`failed` lines. The buffer keeps growing after the job/workflow finishes, so re-attaching (or never detaching) still shows the full output; nothing auto-detaches on completion. `obs.Debug` logs `attach.start`/`attach.stop` with the job/workflow ID and label, so this is checkable from `milk.log` even without watching the terminal live.

### Prompt queue (Enter while busy)

Typed input during a running turn has two dispositions (ADR-0083): **Enter** queues it for the live agent's next turn — FIFO, one flushed per turn, before the background-followup and remote-input chain — while **Ctrl+Enter**/**Ctrl+J** forks it to a background agent as before. Queueing is two-step and explicit: the first Enter announces both choices in the hint, the second Enter on the same gesture queues (any other key disarms the confirm). Ctrl+C drops the most recent queued prompt first — one press per entry — before falling through to its usual chain (clear input → interrupt the turn → quit). A queued entry takes the staged attachments and paste provenance with it, and its flush echoes with a `[queued]` label so no turn answers an invisible prompt. The receipt line at queue time names the review surface: `/queue` (`list`, `show <n>`, `drop <n|last|all>`, `clear`), also visible as a `» N queued` status-bar chip. The queue is in-memory (cap 20, refused with a hint past it) and drops on session switch with a notice; `queue_while_busy: false` restores Enter's old hint-only behavior.

---

## Live configuration reload

milk watches `~/.milk/config.json` while the TUI is running; a save from another terminal is parsed and applied to in-memory state within ~200ms. `/reload` forces an immediate re-parse (useful after a symlink swap or atomic editor replace).

**What gets merged**: the global base is the state root's `config.json` — `~/.milk` by default, redirected by `--config-root` / `$MILK_CONFIG_ROOT` — deep-merged with a project-local overlay at `<dir>/.milk/config.json`, where `<dir>` is the *currently selected* directory: the bound session's cwd (the ACP client's workspace dir, or a `/resume`d session's dir), falling back to the process cwd. Resuming a session stored under another project re-merges against that project's overlay (and rebuilds the agents when the agent-relevant fields changed) — the overlay follows the session, not the launch directory.

On success: `[milk] config reloaded`. On error: `[milk] config reload error: <reason>` — the existing in-memory config is kept, nothing crashes.

**Hot-reloaded**: scalar fields (`direct_bash`, `show_reasoning`, `sticky_escalation`, routing rules, OTel settings, …), agent configurations — when the `agents` list, `agent`/`escalation_agent`, or the agent-as-tool list changes, the primary/escalation runners are rebuilt in place (`[milk] agents rebuilt from config`; claude-cli, subprocess/aider-cli and local providers all included) and take effect on the next turn — and MCP server connections (stale connections closed and the toolset rebuilt for affected roles, gated on an actual change — a turn already in flight keeps its original snapshot). The same rebuild runs when `/config init` or `/agent add`/`/agent switch` commit a change, so a configured agent is usable immediately — no restart.

**Not hot-reloaded**: a running turn's config snapshot, and MCP OAuth authorization (still requires the interactive `/mcp auth <server>` flow).

**Availability is never frozen**: a rebuilt runner with a `run_cmd` has its server started right away (as at startup), and if an agent still reads unavailable when the runners were built — server booting, or config reloaded while it was down — every turn re-checks it (bounded, so a dead host can't stall a turn) and routes to it again as soon as it answers. No restart needed to recover.

### Recovering from a corrupted `config.json`

Two safety nets:
- **While running**: the reload path above keeps the last-known-good in-memory config on a parse error — nothing is lost, the session keeps working.
- **At startup**: if `config.json` fails to parse, milk falls back to `config.json.bak` (refreshed on every successful save/parse) and starts with a warning instead of refusing to launch. Only a `config.json` that was *never* successfully loaded before (no backup exists yet) hard-fails, pointing at `milk config open`.

---

## Self-update

`/update install` (TUI, ACP) and `milk update install` (CLI) download the release, verify its SHA-256 and replace the running binary. The running process always keeps the old version until you **restart the milk process/agent** — a new session alone does not pick the new version up.

- **POSIX**: one atomic rename over the binary; the running process keeps its (now unlinked) image and the restart lands on the new version.
- **Windows**: an executing image is locked against writes but *can be renamed*, so milk renames the running `milk.exe` aside to `milk.exe.old`, moves the verified download into place and keeps running the renamed image until restart; the leftover `milk.exe.old` is deleted at a later start (it stays locked while any milk instance executes it).
- **Windows, rename denied** (antivirus, exclusive lock, Program Files ACLs): the download is staged as `milk.exe.new` instead. `updater.StartupUpdate` runs at every process start and swaps the staged file in — retried each start until it succeeds, so the update applies *lazily on restart* — and `/update install` reports `staged — … applied on next start` rather than failing. Only when staging is also denied does milk fall back to the manual path (the temp file path is printed for a hand replace).

---

## Graceful degradation

| Primary agent | Escalation agent | Behavior |
|---|---|---|
| up | available (any provider) | normal routing |
| down | available, configured (`escalation_agent` set, or a `claude` entry) | warn once per session, route everything to escalation |
| down | only the built-in Claude CLI default | **ask** before running a turn on it (default No, once per session; ADR-0055) — set `escalation_agent: "claude"` to make it automatic |
| up | only the built-in Claude CLI default | `/escalate`, router escalation and the model's `escalate()` also ask before the first use in a session |
| up | unavailable/not installed | warn once per session, stay primary-only |
| down | unavailable | single-prompt: error and exit. TUI: the turn is still attempted and reports the real request error (a failed health ping alone never blocks a turn) |

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
    "notify_tools": true,
    "busy_indicator": true
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
| `busy_indicator` | `true` | Show the agent's live business status (💭 thinking…/⌨️ typing…/⏳ waiting for you…) while a turn runs |

**Forwarded**: turn start (agent, target, prompt snippet — workflow turns labeled `workflow:<role>`), tool calls and results (summaries kept to ~500 chars), response text (relayed in full — long responses are split into ordered chunks under Telegram's 4096-character message limit, never trimmed; each chunk carries a `(n/m)` part marker), permission prompts with y/n reply — or `a`/`d` to answer that and all remaining tool asks that way (issue #302's session mode; not offered on safety confirmations) — (first response from either surface wins). **Busy indicator** (`busy_indicator`, issues #284/#286): while a turn runs, its 🔄 turn-start message carries a **status line edited in place** — `💭 thinking… m:ss` while the agent reasons and works tools, `⌨️ typing… m:ss` while response segments stream, and `⏳ waiting for you… m:ss` for the whole of a remote ask (instead of the old silent pause) — with the elapsed counter ticking and state changes rendering immediately. It is *not an extra message*: the line is stripped again when the turn ends, returning the 🔄 message to its bare two lines, so nothing permanent lands in the transcript. The native `typing…` chat action runs **only while text actually streams** (at a rate-limit-safe cadence ≥5s, where its fixed label is accurate); non-turn notices (startup, `/setup telegram` confirmations) never engage any of it, `retry_after` is honored, and a lost turn-start message id degrades to "no status line" rather than a failure. Every permission ask reaches the remote surface in both hosts — the claude-cli escalation agent's tool prompts, the local agent's tool prompts and safety confirmations (e.g. the doom-loop gate's "allow it to continue?"), and background-job tool prompts — not just the ones a given provider path happens to render locally.

**Tool styling** (ADR-0070, issue #290): tool lines are quiet single-line tags mirroring the TUI's dim hints (ADR-0026) — italic tool name + inline-code summary (`» _Bash_ · \`grep …\``, `📤 _Bash_ · \`…\``), never bold; **error results stay bold** (`❌ *Bash* error · \`…\``) so failures are never quietly dimmed. Summaries are flattened (newlines → ` ⏎ `, multi-line MarkdownV2 code spans parse unreliably) and capped rune-safe (use 200 / result 497 runes), which also keeps every unchunked tool send far under Telegram's 4096-unit limit — a preview too large used to make the message silently vanish.

**Background asks (timed answer)**: permission asks from background jobs, workflow steps, and safety confirmations like the doom-loop gate surface everywhere — the main TUI prompt queue (with a countdown and `a`/`d` bulk answers) and the remote surface — instead of failing closed. Each ask carries a **timed answer**: if neither surface replies before the deadline, the configured default applies (default `deny` after 360s). Configure it in `~/.milk/config.json`:

```json
{
  "permissions": {
    "background_timeout_secs": 360,
    "background_default": "deny",
    "safety_timeout_secs": 360,
    "safety_default": "deny"
  }
}
```

`safety_*` governs the doom-loop gate's unattended confirmations only (separate from tool asks on purpose — `safety_default: "allow"` re-enables unattended runaway loops and warns at startup). Override both for the session with `/permissions default allow|deny [safety]` and `/permissions timeout <secs> [safety]`; `/permissions` lists the policy and any pending asks. Foreground asks are unchanged: direct input and remote oversight, no deadline; `timeout_action` (above) resolves remote-side silence for those. `dangerously_skip_permissions` keeps auto-approving tool asks (including background ones) but never bypasses the doom-loop gate.

**Session permission mode (allow all / deny all)**: a turn that touches several tools used to mean one prompt after another. At any permission prompt, `a` answers it *and* every later tool ask in the session (`allow-all`), `d` does the same for denials (`deny-all`) — the prompt reads `Allow? [Y/n/a/d]`, and the same keys still answer a whole queued burst in one keystroke. Over ACP the equivalent is the `Always allow` / `Always deny` option in the client's permission dialog. The mode is session-only (like `/skip-permissions`): it never writes the persistent grant store, it is visible in the TUI status bar as `[perms: allow-all|deny-all]` and in `/permissions`, and it is cleared with `/permissions mode off` or by any `/skip-permissions on|off` toggle. It answers **tool asks only** — the doom-loop safety gate keeps prompting no matter the mode. Remote oversight gets the same choice: a Telegram reply of `a`/`d` to a permission prompt sets the mode (the prompt offers it only where the mode applies — safety confirmations keep their plain `y`/`n` wording), and `/permissions mode …` typed into the chat works too.

**Remote input**: any message sent to the bot is injected as a new turn (`[telegram] …` in the transcript); queued while a turn is in progress, delivered as the next turn once it completes — except a message **starting with** `/interrupt` (first token: exact, or `/interrupt <words>`; mid-input occurrences are ordinary text like every other slash command), which acts immediately: it cancels the running turn instead of queueing (issue #288, the Ctrl+C equivalent for surfaces without Ctrl+C; the TUI and ACP accept it as a regular slash command too).

**Questions (`ask_user`)**: the agent's clarifying questions are relayed too — the question and its options are posted to the chat and any reply is returned to the agent as the answer, through the same three-source ask layer as permission prompts (direct input, this remote surface, and — for unattended asks only — the timed answer). Silence is never an answer here: unlike permission prompts there is no remote timeout action, and the timed "no answer" only ever applies to background questions at their deadline.

**Over ACP** (`milk serve --acp`): everything above runs too, with one notifier per serve process — permission prompts race the ACP client (first answer wins, remote asks serialized process-wide), and bot messages run as turns in the live session the user touched most recently (queued until one exists if none do), echoed to the client as ordinary user messages. See docs/reference/acp-integration.md's "Remote oversight (Telegram)" section.

---

## Inline diff view

When an agent calls `edit_file`/`write_file` (primary) or `Edit`/`Write` (Claude CLI), milk renders a colored inline diff in the transcript right after the tool-hint line — deleted lines in red, added in green, 3 lines of context on each side.

## Idle tips

Below the input area, milk shows a one-line **idle tip** (`tip: …`) while you work — short one-liners for features and key bindings you might not know about (`@`-path references, `!` direct bash, `ctrl+r` history search, the F1-F6 panels, `ctrl+enter` background agents, …). The line rotates every 20 seconds while nothing is running, freezing while a turn executes or a prompt is pending. It sits in the same slot as the tab-completion hints and yields to them whenever they are up. The tip spans the full window width — it sits below the side panels (which end on the row above), so open panels never narrow it.

Three content sources, in precedence order (see ADR-0061):

1. **Contextual hints** — derived from live state at render time, so always fresh: no primary agent configured → `/config init`; an update waiting → `/update install` with the release tag.
2. **Curated tips** — hand-written one-liners in priority order; a fresh session starts at the most valuable ones.
3. **Registry-derived tips** — generated from the slash-command registry, so every command has a tip that cannot drift from the real command set.

`/tips` shows the current state and where the shown tip sits in the rotation. `/tips off` hides the line (persisted as `show_tips` in config; it is on by default), `/tips on` brings it back.

Tips are browsable, not just rotatable:

- `/tips next` / `/tips prev` step through the pool (wrapping at both ends) — or press **Alt+Up / Alt+Down**, the shortcut the tip line itself references at its right edge (`· prev/next: Alt+Up/Down`; dropped on narrow terminals). Every manual move also restarts the 20-second cycling timer (the tick in flight is dropped), so a browsed tip is never cut short by the old schedule, and pins the tip over contextual hints until the next automatic rotation step.
- `/tips pick` lists every tip with its number (`*` marks the one showing); `/tips pick <n>` jumps to a tip number, `/tips pick <text>` jumps to the tip matching `<text>` (listing candidates when several match). A picked tip is echoed to the transcript at full width.
- `/tips search <query>` lists the tips whose text contains `<query>` (case-insensitive).

## Keyboard shortcuts

| Shortcut | Action |
|---|---|
| **Enter** | Submit prompt / accept tab completion (accepting a completion or hint works while an agent turn is running too — only plain Enter-as-submission is trapped mid-turn) |
| **Ctrl+Enter** | Spawn a background agent from the current input (while an agent turn is in progress). **Ctrl+J** works as a universal fallback in terminals without extended key protocols. |
| **Enter** (while busy) | Two-step disposition: the first Enter shows the hint (queue vs fork), the second queues the input for the live agent's next turn — `/queue` reviews it, `/queue drop last` undoes. `queue_while_busy: false` restores the hint-only behavior. |
| **Tab** | Cycle slash-command and @-path completions (also available while a turn is in progress). `@` paths come from a cached index of the project's tracked and untracked-not-ignored files (git's own ignore rules; a bounded, ballast-pruning walk outside git), ranked file-name-prefix first — see ADR-0057 |
| **Shift-Tab** | Reverse cycle completions |
| **Ctrl-C** | Copy selection → clear input → cancel the running turn → quit (double press; also cancels a running workflow — to stop only the workflow use `/workflow cancel`) |
| **Ctrl-D** | Quit (when input is empty) |
| **Ctrl-R** | Search backward in transcript |
| **Ctrl-S** | Search forward in transcript |
| **Ctrl-Left/Right** | Word navigation |
| **Shift-Arrows** | Text selection (transcript and input) |
| **Ctrl-X** | Cut selected input text |
| **F1 / F2 / F3 / F4 / F5 / F6** | Show/hide the memory / tasks / background-agents / workflow / project tree / git graph panel — same effect as `/panel <name>`. Works in any mode, including mid-turn. |
| **Alt+Up / Alt+Down** | Show the previous / next idle tip (same as `/tips prev`, `/tips next`). Referenced at the right edge of every tip line. |
| **Ctrl+G** | Dismiss all open notification toasts (history stays: `/notifications`) |
| **Ctrl+O** | Open the command palette: filter the slash commands, Enter puts the command in the input line (Esc closes) |
| `/keys` | List every shortcut (panels, navigation, input) from the one key registry (`keymap.go`) |
| **Ctrl+T** | Toggle the most recent thinking block — the one still streaming, else the last completed one (works during streaming). Reasoning renders as foldable blocks, folded by default (streaming included — the header's line count updates live as reasoning arrives): click a `▸ thinking · N lines` header to expand/collapse just that block. |

Turn-unrelated informational events — fold-state changes, panel open/close, background-job lifecycle, credential refreshes — surface as floating toasts in the top-right corner (timestamped, each with the related slash-command hint, auto-expiring after a few seconds). They are **not** part of the transcript; `/notifications` shows the full history with timestamps (`/notifications clear` empties them). Tool calls and turn-bound output stay in the transcript. One deliberate exception: a finished background job's *result* is content rather than lifecycle noise and is appended to the transcript as the job completes (capped to the model's delivery budget, `/bg show <id>` for the rest — see [Background sub-agents](#background-sub-agents)); only its lifecycle *notice* stays toast-only.

Each panel's title shows its shortcut (e.g. `tasks    F2`) as a reminder. The tasks, background-agents, and workflow panels also open themselves automatically the moment their content becomes active — a task is created, a background job starts, a workflow launches — so you don't have to notice and press the shortcut first; the memory panel follows the same content-driven rule (it is closed by default and opens when the percept store has content). The whole behaviour is a preference: `/panel autoopen off` (config `panel_auto_open`, default on) makes panels open *only* when you ask — no auto-opens, no startup opens — and `/panel autoopen on` restores it ([ADR-0087](../adr/0087-panel-auto-open-master-switch.md)). Manually toggling a panel (`/panel <name>` or its F-key) overrides this in either direction while you work: closing a panel sticks for the content you dismissed it over, and the veto expires the moment that content finalizes — every task done, no background job still running, the workflow completed or cleared, the percept store emptied by `/forget` — so the *next* new content brings the panel back (ADR-0059); closing a panel whose content has already settled never locks it out in the first place. With `remember_panel_state` on (the default) your open/closed choice is *also* saved per folder to `~/.milk/panel-state/<name>-<hash>.json` (preferences follow the project, never another folder's session) and restored at startup — as startup layout only: a remembered close does not block new content from auto-opening the panel ([ADR-0078](../adr/0078-persisted-panel-state.md) as amended by [ADR-0087](../adr/0087-panel-auto-open-master-switch.md)). Set `"remember_panel_state": false` for session-only panels with nothing read or written. When two or more panels are open, every other one (by left-to-right position among the panels currently showing, not a fixed panel) gets a subtle background tint so adjacent panels are easier to tell apart.

When the terminal is too narrow to fit every open panel, the budget fills with the panels whose content changed — or that you clicked or scrolled — *most recently*: a content event (a new task, a job update, workflow progress, a new percept, a file or commit change) swaps its panel in and lets the least recently relevant one step out, so what you see keeps tracking the freshest information instead of a fixed panel order; opening a panel by hand claims a slot immediately. Panels that never saw an event fall back to the fixed left-to-right order (the startup baseline), whatever mix of panels is open, and the transcript always keeps at least 40 columns ([ADR-0089](../adr/0089-side-panel-width-budget.md)).

**Project tree and git graph** (issue #206, [ADR-0066](../adr/0066-project-tree-and-git-graph-panels.md)): `/panel project` (**F5**) shows the working directory's file tree — backed by the same git file listing the `@`-completion uses, top-level directories expanded, deeper ones collapsed; click a directory to expand/collapse it, click a file to preview it in the main area (Esc returns to the transcript), double-click a file to insert an `@`-mention into the input. `/panel git` (**F6**) shows the commit graph — lanes laid out in code from `git log` parent hashes, current branch and dirty count on top; click a commit to see its summary and diff in the main area. Both refresh every few seconds while open and degrade gracefully outside a git repo (the tree falls back to a pruned walk; the graph shows a note). The panels sit to the right of the workflow panel and never auto-open.

**Reasoning blocks** (issue #208, [ADR-0067](../adr/0067-thinking-first-class-view.md)): reasoning is a view of its own, not dim text mixed into the transcript. Each reasoning run renders as a fold container — a one-line header (`▸ thinking · N lines` folded, `▾ thinking · N lines` expanded, `· streaming` on a block you opened while it is still arriving) with the body shown only when expanded. Fold containers render on their **own background** (the thinking surface) — a full-row surface: every row of the container (header, body lines, blank lines and word-wrap continuation rows alike) paints the background across the whole row width, not just behind the glyphs — so the reasoning region is a visibly different surface from the standard transcript at every fold state. The header is information-only: glyph, line count and fold/streaming state, never a key hint (those live in `/help` and the idle tips). **Blocks are folded by default at all times — streaming or complete, no auto-open** (long reasoning doesn't flood the transcript): while reasoning streams in you see the same single `▸ thinking · N lines` header as a completed block, with the line count updating live. Click a header to expand/collapse just that block, or press **Ctrl+T** to toggle the most recent block (the streaming one, if any) — either action pins the block, so one you opened mid-stream stays open when streaming ends and only blocks you never touched remain folded. **A new block keeps the folding status the previous one was left in** (folded by default; open-and-pinned while you're watching) — open one and the rest arrive open, fold one and the next arrive folded. **A new turn collapses the previous turns' blocks** while that status is folded — `/think expand` means "expand all" and thereby defines the current default, so in that mode the tidy does not run at all and older turns keep their blocks expanded (the tidy is display-only and never changes the fold status itself). `/think expand` / `/think collapse` for all blocks at once, and `/think view` swaps the main area to a thinking-only stream (Esc returns). There is no hidden mode — every block always shows its header row — and `/think on|off` are gone; bare `/think` reports block count and fold state. The *starting* status is a config knob (`think_fold_default`, default `folded`): `"expanded"` seeds every session so blocks arrive open and the tidy is opted out from the start — per-session actions still win for the rest of the session, and a session switch re-seeds it. Loop detection and session history keep consuming reasoning regardless of fold state. The fold commands are TUI-only; over ACP, `/think on|off` keeps its data-plane meaning (relay or withhold `agent_thought_chunk` updates, seeded from `show_reasoning` in config) — TUI fold state is independent of it.
