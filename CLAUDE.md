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
- [docs/acp-integration.md](docs/acp-integration.md) — `milk serve --acp`: what's actually implemented today (not the full design), example JSON-RPC exchange, known gaps
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
cmd/milk/attach.go            # TUI attach view: swap main transcript for a background job's/workflow's internal/livebuf.Buffer (ADR-0047)
cmd/milk/outputformat.go      # --output-format text|json|stream-json builders, wired in main.go's run()
cmd/milk/serve.go             # `milk serve --acp` cobra command
cmd/milk/acp_server.go        # ACP method dispatch (initialize/session/new/prompt/cancel) — see docs/acp-integration.md
cmd/milk/acp_session.go       # per-ACP-session state + turn dispatch, mirrors repl.go's buildTUIAgents
cmd/milk/host_acp.go          # events.Host adapter over ACP (local-provider agents only)
internal/transport/streamjson/ # typed §6 event model + JSONL encoder/decoder for --output-format stream-json
internal/transport/acp/       # ACP v2 wire vocabulary + stdio.go's JSON-RPC transport (StdioConn)
internal/config/              # config loading (~/.milk/config.json)
internal/session/             # session state + store (~/.milk/sessions/)
internal/router/              # routing logic (rules + weighted scorer + local model)
internal/agent/local/         # OpenAI-compat client + Bedrock Converse native path + auth transports (SigV4, Bearer, custom headers) + tool loop + stream detector
internal/agent/claude/        # claude CLI subprocess + stream-json parser
internal/agent/subprocess/    # generic subprocess agent (NDJSON protocol); base for aider and smolagent
internal/agent/aider/         # aider-cli provider (wraps subprocess agent)
internal/agent/smolagent/     # subprocess provider (wraps subprocess agent)
internal/escalation/          # context builder (local transcript → escalation agent prompt)
internal/instructions/        # loads the target repo's AGENTS.md/CLAUDE.md into agent system/static context
internal/textbudget/          # shared head+tail truncation helper for bounding large text before prompt hand-off
internal/livebuf/             # thread-safe byte-capped text buffer for live background-job/workflow output (ADR-0047)
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
- **Local-agent prompt caching**: two independent mechanisms, both feeding the same `TokenUsage.CacheRead/CacheCreation` → status bar/memory panel pipeline. (1) OpenAI-compatible implicit caching — parses `usage.prompt_tokens_details.cached_tokens` (OpenAI's own standard field, mirrored by e.g. Xiaomi MiMo) with zero request-side changes or config; live-verified. (2) Bedrock explicit `cachePoint` caching — opt-in via `prompt_caching` in the agent config entry; up to 3 breakpoints: one anchored right after the stable system-prompt prefix specifically (not at the end of the `system` array, where it would otherwise get bundled with — and invalidated by — per-turn percepts/current-need entries that follow it), plus a rolling double-buffer over the last two conversation messages; **experimental, not live-tested** (no Bedrock agent available during development). Provider-reported prompt-token totals that include the cached portion (subset, not additive) are normalized to fresh-only before recording — see `internal/agent/local/local.go`'s `streamCompletion`.
- **Single inference server instance**: same server handles both router classification and local coding/tool tasks
- **models.dev context-window fallback**: `Config.AgentContextWindowTokens` falls back to a best-effort lookup (`internal/modelsdev`) against the [models.dev](https://models.dev) catalog when an agent entry omits `context_window_tokens`, matched case-insensitively against the agent's `model` string across every provider in the catalog (milk stores no models.dev provider ID). Since a model's context window essentially never changes once published, the primary source is a catalog snapshot embedded at build time (`internal/modelsdev/snapshot.json`, regenerated via `scripts/update-models-dev-snapshot.sh`) — works fully offline, zero startup latency. A live fetch, cached to `~/.milk/models_dev.json` and refreshed in the background past a 24h TTL, is only a fallback for a miss against the embedded snapshot (e.g. a newer model); served from memory, so a lookup never blocks a turn. Explicit `context_window_tokens` always wins over either source. `disable_models_dev_lookup: true` turns off both the embedded and live lookup (e.g. a custom/fine-tuned model sharing a name with a catalog entry). Documented under `docs/providers.md`'s "Context window declaration" section, which `internal/selfdocs` also exposes to any agent via the `models.dev`/`context window` topic aliases — so milk's own agent can answer a user's question about a model's context window using this mechanism.
- **Escalation agent**: any `agents` entry can be the escalation target — set `escalation_agent` to its name. Defaults to the built-in `claude-cli` entry. Use `/agent switch <name> as escalation` to change it at runtime.
- **Claude via CLI subprocess**: `claude --print --output-format stream-json`, not direct API. Configured as `provider: "claude-cli"` in `agents`.
- **Background sub-agents** (ADR-0043): any inference-server-backed agent (primary or escalation — the deciding factor is provider, not role) can call `spawn_background_agent` to fork an independent copy of itself for a self-contained research task with its own tool loop, running asynchronously via `internal/agent/local.Manager` and reporting back on the next turn (`dispatch.go`'s `drainBackgroundJobs`) plus immediately in the TUI. The local-agent analogue of Claude Code's own fork/Task tool; `claude-cli` needs no equivalent since it already has that natively.
- **Side-panel auto-open** (ADR-0044): tasks, background-agents, and workflow panels open themselves the moment their content becomes active (a task created, a background job spawned, a workflow started) via `(*model).autoOpenPanel`, but a manual `/panel <name>`/F-key toggle (`model.panelManualOverride`) sticks over automatic management for the rest of the session, in either direction. Panel titles show their F1-F4 hint (`panelTitleLine`); every other currently-open panel (by visible left-to-right position, not fixed per region) gets a subtle background tint (`panelAltBackground`), with `withPanelBackground` re-applying the tint after every embedded ANSI reset so it survives behind already-styled badges/titles/dim text instead of being wiped by their own reset codes.
- **Notification toasts** (ADR-0048): turn-unrelated informational events (`/think on|off` state changes, panel toggles, ADR-0044 auto-open transitions, background-job lifecycle, config reload, MCP OAuth/credential refresh) render as a floating timestamped toast overlay pinned to the top-right of the main area — a pure render-time overlay consuming zero layout rows (viewport/panel/PTY geometry untouched) — instead of the transcript. Each toast carries its related slash-command hint (fallback `/notifications`); events queue (max 3 visible, fresh 6s TTL on promotion) and never expire unseen; history ring (500) is browsed via `/notifications [clear]`. Dismiss interactively with **Ctrl+G** or let them time out — dismissal keeps history. The expiry tick is generation-guarded (the #168 lesson). Scope boundary: tool calls, turn-bound output, warnings and errors stay in the transcript.
- **Live-attach view** (ADR-0047): double-clicking a background-job row (F3) or the workflow panel (F4) swaps the main transcript viewport for that job's/workflow's own `internal/livebuf.Buffer` (`model.attached`, `cmd/milk/attach.go`) — its tool calls and streamed text, kept off the main transcript the whole time — modeled on the existing `m.ptyPane` viewport-substitution pattern but with side panels staying visible; Esc detaches. Fixed a real bug in the same change: `workflow.WorkflowChunkMsg` previously leaked stage output straight into the main transcript; it now accumulates in `workflow.State.Live` instead. Claude CLI's own Task-tool subagents and background-workflow `journal.jsonl` remain out of scope — no content is exposed via `stream-json` for the former, and the latter's schema is unverified.
- **Context handoff**: local transcript passed to a new CLI session via **one** `--append-system-prompt-file` flag holding the static instructions and dynamic summary concatenated — the Claude CLI only honors the *last* such flag when more than one is given, so a literal two-flags approach (as ADR-0004 originally specified) silently drops the first; corrected 2026-09-30, see the ADR's Update section. On `--resume` Claude Code replays its recorded system prompt (`--system-prompt-snapshot`) and ignores new files until a compaction, so per-turn context is prepended to the prompt in a `<milk-context>` block instead (full static block still passed as a file, re-recorded on compaction); local providers receive a `BuildDynamicContext` orientation block as a prepended system message. Both paths inject percepts on `First`/`Returning` turns; a percept recorded mid-resume is also re-surfaced via a small diff block on `Resume`/`Continuation` turns rather than waiting for the next `First`/`Returning` turn. On stale returning escalations (topic switched or ≥`returning_fresh_start_local_turns` local turns since last escalation, default 8), CLI drops `--resume` and local providers scope history to post-escalation turns only.
- **ESCALATION_WAITING state**: once the escalation agent asks a follow-up, next turn bypasses router → `--resume`
- **Self-escalation**: local model can call `escalate(reason)` as a function call
- **Role-aware system prompt**: primary agent and escalation agent receive different system prompts — the escalation agent knows it is the escalation target and should not escalate further
- **Streaming tool-format detector**: FSM detects tool-call markup format from the stream; handles Qwen fenced JSON, `<tool_call>` tags, Gemma special tokens, bare JSON without pre-configuration
- **Persistent TUI**: bubbletea alt-screen with viewport (transcript) + textarea (input) + status bar; agent turns run in goroutines, output streamed via `p.Send()`
- **Input history**: per-session (`~/.milk/sessions/<id>.history`) and global (`~/.milk/input_history`); Ctrl+R/Ctrl+S incremental search
- **Memory**: Percept store with NREM consolidation — decay/prune/promote cycle at session end; memory panel (`/panel memory`) shows SESSION/GLOBAL/GLOBAL(core) sections in real time, open by default; `/forget` and `/memory show` for interactive management
- **Reasoning visibility**: thinking/reasoning tokens kept in a separate transcript variant; `/think on|off` toggles retroactively; both variants maintained in parallel during streaming (no rebuild on toggle); default configurable via `show_reasoning` in config
- **Project instructions**: the primary/local agent loads the target repo's own `AGENTS.md` into its system prompt (falling back to `CLAUDE.md` when no `AGENTS.md` exists, since a local model has no other path to it), the same way Claude Code loads `CLAUDE.md` and OpenCode loads `AGENTS.md` (`internal/instructions`, cached by mtime). The escalation agent's static context (`internal/escalation`) also injects `AGENTS.md` — but never `CLAUDE.md`, since the Claude CLI subprocess already loads that natively. Opt out per agent with `disable_project_instructions: true`.
- **Context compaction**: when message history exceeds budget, `trimLocalMessagesWithCompaction` (`cmd/milk/main.go`) makes one extra inference call (`Agent.Summarize`, a plain tool-free completion) to summarize exactly the span that would be dropped, splicing the summary in as a single message instead of discarding it outright. Falls back to the old plain drop-oldest-first trim when there's nothing to summarize, the call fails, or the provider doesn't support it (Bedrock, Responses API). Opt out per agent with `disable_compaction: true`.
- **Bash permission pre-approval**: `bash_allowed_patterns` (per agent) is a static allow-list of bash command prefixes (`"git status*"`, exact match with no trailing `*`) that skip the permission ask/grant entirely, checked before the normal `PermStore`/interactive-ask flow — a finer-grained, purely additive complement to a blanket `bash` grant, not a replacement for it.
- **Background sub-agent enhancements**: `spawn_background_agent` gained `full_context` (opt-in; injects the spawning agent's already-capped `sess.LastLocalSummary` as extra orientation for a job whose task genuinely depends on the caller's recent activity — not a raw conversation snapshot, which would reopen the cost risk isolation exists to prevent) and an optional structured result convention (a job's answer may end with `<result status="ok|error|partial" files_touched="..."/>`, parsed by `drainBackgroundJobs` instead of requiring prose parsing). `cancel_background_agent(job_id)` is the model-facing counterpart to the human-only `/bg stop`.
- **Per-workflow memory opt-in**: workflow roles are isolated from memory by default (fresh scratch session, no percepts) — a stage can opt in via `use_memory: true` in its YAML definition, threaded through an optional `workflow.MemoryAwareTurnRunner` capability interface (mirrors `imagePartReceiver`) rather than widening the base `TurnRunner` interface.

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
| `primary:subagent` / `escalation:subagent` | `spawn_background_agent` job (ADR-0043), tagged by the spawning agent's own role — the same suffix as Claude Code's subagents, but recorded directly by milk rather than parsed from a subprocess's stream JSON |
| `user:subagent` | A background job the *user* spawned directly (pressing Enter again while busy — see ADR-0043's TUI section), not an agent's own tool call — there is no agent role to tag it with, so it gets its own bucket rather than being miscategorized under `primary` or `escalation` |

The colon-separated convention allows prefix queries — `SessionTokensByRolePrefix("escalation")` matches all escalation-related roles (main turns, Claude Code's own subagents/workflows, and any spawn_background_agent jobs an escalation-role local agent ran).

### How it works

1. `stream.go` parses `SubagentUsage`/`WorkflowUsage` from the `result` event's stream JSON when present.
2. `dispatch.go` records these via `sess.AddTokensFull` with the appropriate role string and via `obs.RecordTokens` / `obs.AccumulateCacheTokens` for OTel metrics.
3. `FormatTokenUsage` in the TUI displays each role string as its own row in the `/usage` table.
4. The eval harness (`adapter_claude.go`) captures subagent/workflow tokens from the transcript JSONL.
5. Graceful degradation: when Claude Code does not provide subagent/workflow data, all tokens are attributed to the main `escalation` role as before.
6. `spawn_background_agent` jobs (ADR-0043) follow the same `<role>:subagent` convention but are recorded directly by `dispatch.go`'s `drainBackgroundJobs` when a job completes, not parsed from anything — see [docs/adr/0043-background-subagents.md](docs/adr/0043-background-subagents.md).

## Loop detection

Milk detects when an LLM agent gets stuck in a loop — repeating the same phrase/tool-call within a turn, or producing identical responses across turns. This prevents runaway token consumption when the user doesn't interrupt. Five complementary systems work together:

### Agent-internal detectors (`internal/agent/local/`)

| Detector | Catches | Recovery |
|---|---|---|
| **Streak tracker** (`loop_streak.go`) | Same reasoning hash or tool-call signature across consecutive iterations | Crop + nudge → strong nudge → terminate |
| **Streaming n-gram** (`reasoning_ngram.go`) | Periodic reasoning repetition during streaming | **Cuts stream immediately** + crop + nudge → terminate |
| **Text-loop tracker** (`loop_streak.go`) | Same output text across consecutive steps | Crop + nudge → strong nudge → terminate |
| **Duplicate tool calls** (`local.go`) | Model re-issues a tool call already executed | Nudge → strong nudge → terminate |
| **Doom-loop gate** (`local.go`, `loop_streak.go`) | 3 *consecutive* iterations issuing the exact same tool-call batch (stronger signal than "duplicate tool calls," which fires on any repeat seen anywhere earlier in the turn, not just back-to-back) | Interactive permission ask → continue if approved, or fail closed (deny) — not a nudge; treated as a safety event, since a model that repeats identically 3 times *in a row* has already ridden out whatever self-recovery the other detectors offer |

Escalation mechanics (crop → mild nudge → strong nudge → terminate) are consolidated in one shared helper (`loopRecoveryAction` in `loop_streak.go`) called by the first four detectors, rather than each re-implementing it — this is what fixed the text-loop tracker's termination path above, which previously had no working counter of its own. The doom-loop gate is deliberately separate: it's not a self-recovery nudge ladder, so it doesn't go through `loopRecoveryAction`.

**Workflow-role scoping**: when an agent is executing a workflow step (`workflowRole == true`), the streak tracker, streaming n-gram, and text-loop tracker are all skipped — reasoning and output text legitimately repeat across workflow passes, and the workflow interpreter (`internal/workflow/interp`) handles recovery for those at a higher level (see `isTerminatedTurn`). Duplicate tool calls and the doom-loop gate are **not** skipped for workflow role: a literal repeat of a write-tool call with identical arguments has no legitimate cross-pass explanation, so it's caught early regardless of caller — the doom-loop gate specifically fails closed for a workflow step (no one to ask), instead of the interactive permission prompt an ordinary turn gets. Critically, the n-gram monitor's mid-stream *feed* is gated by workflow role too, not just its Run-loop recovery handling — feeding it and ignoring the trigger would still cut every subsequent stream in the turn with no recovery, since the monitor's window is never reset.

**Budget-exhausted vs. loop-terminated** (workflow steps only): a step that simply runs out of tool-call iteration budget (no forced-summary nudge fires for workflow roles at the last iteration, unlike ordinary turns) produces a distinct `iterationBudgetExhaustedMarker`-prefixed message, checked by the interpreter's `isBudgetExhaustedTurn` *before* the generic `isTerminatedTurn` check. Both currently advance the workflow the same way ("break" with partial output), but get different trace outcomes and transcript messages — a budget-exhausted step is not reported to the user as "terminated by loop detection," since it may simply have needed more iterations, not evidence of a stuck loop.

### TUI-level signals (`internal/loop/detector.go`)

| Signal | Scope | What it catches | Default threshold |
|---|---|---|---|
| **chunk_repetition** | Intra-turn | Same text repeating in streaming output (consecutive, or scattered for chunks ≥40 runes) | 5 occurrences in 50-chunk window |
| **reasoning_chunk_flood** | Intra-turn | Too many reasoning chunks without content output | 5000 chunks |
| **token_velocity** | Cross-turn | Rapid token consumption without progress | 300k tokens in 60s window |
| **silent_burn** | Per-turn | High input tokens, near-zero output | 20k input tokens |
| **turn_flood** | Session | Excessive turns without user input | 10 consecutive non-user turns |

Note: consecutive reasoning chunk repetition was removed from TUI signals — now handled by the streaming n-gram detector which cuts the stream immediately.

**Observability**: the four agent-internal recovery-ladder detectors and the doom-loop gate (not the TUI-level signals above) emit a `milk.loop.recovery` OTel counter on every crop/nudge/terminate/doom-loop decision, tagged with low-cardinality `model`/`agent`/`detector`/`outcome` attributes only — deliberately *not* `session_id` (unbounded cardinality would make the counter's time series grow forever). The same detectors' plain-text `slog` lines (`internal/obs`'s `milk.log`, via `(*Agent).logWarn`) *do* carry `session_id` (and `job`, for background jobs, via the pre-existing `jobAttrs`) — fine for a rotated text log, and the attribution a prior manual investigation (`docs/session-2026-10-01-primary-overload-analysis.md`) had to reconstruct by hand from timing/model fields alone. See `docs/escalation-and-context-enhancements-plan.md` Track B Phase 1 for the full rationale.

### How it works

1. **Streaming n-gram**: During reasoning streaming, every `reasoning_content` delta feeds a sliding 1000-token window. Two independent checks run: a block of 4+ tokens repeating 5+ times **consecutively** (verbatim back-to-back loop — an unambiguous signal, so the threshold is low), or any 4+ token block recurring 20+ times **within a 200-token span** anywhere in the window (a weaker, "thinking in circles" signal — a phrase can legitimately recur many times across a long analysis, so it needs more repetitions bounded by distance before it counts as a loop). Either check cuts the stream immediately and injects a recovery nudge.
2. **Streak tracker**: After each tool-calling iteration, the reasoning text (truncated to 500 chars, normalised) is hashed. Three consecutive identical hashes trigger crop + nudge.
3. **Duplicate tool calls**: After each iteration, tool calls are checked against previously executed calls. Exact matches trigger a nudge (not termination), similarly to MiMo-Code's approach (an independent implementation compared during a 2026-09-29 review, not a port — see docs/prompt-context-management-review.md).
4. **Doom-loop gate**: A separate, independent tracker compares each iteration's full tool-call batch signature (order-sensitive, exact-match) to the immediately preceding iteration's. On the 3rd consecutive match, raises a `doom_loop` permission ask through the same interactive-ask plumbing ordinary tool permissions use — approving resets the streak and lets the model continue; denying, or having no one to ask (background job/workflow role), terminates the turn immediately.
5. **TUI-level**: `FeedChunk()` is called for every streaming chunk. A ring buffer tracks the last 50 chunks. Cross-turn signals fire after each turn completes.
6. **Status bar**: Shows `[⚠ loop — auto-interrupted]` / `[⚠ consumption — auto-interrupted]` on an auto-interrupt, or `[⚠ <category>: <message>]` for warn-only signals.
7. **Transcript**: Shows `[⚠ loop detected: …]` for repetition-based loop evidence and `[⚠ consumption: …]` for consumption/volume threshold crossings (`reasoning_chunk_flood`, `token_velocity`, `silent_burn`, `turn_flood`). No numeric confidence percentage is rendered anywhere — the internal severity score is a fixed per-signal constant, not a measured probability (issue #173).

### Configuration

```json
{
  "loop_detection": {
    "enabled": true,
    "chunk_repetition_threshold": 5,
    "chunk_window_size": 50,
    "chunk_repetition_min_scattered_length": 40,
    "reasoning_chunk_flood_threshold": 5000,
    "max_consecutive_similar_responses": 3,
    "response_similarity_threshold": 0.85,
    "reasoning_max_consecutive_similar_responses": 6,
    "token_velocity_window_seconds": 60,
    "token_velocity_threshold": 300000,
    "auto_interrupt": false
  }
}
```

Default: detection ON, auto-interrupt OFF (warn only). Set `auto_interrupt: true` for unattended sessions.
