# milk — specification

## Overview

milk is a terminal agent host: it routes each prompt between a primary inference agent and a configurable escalation agent mid-workflow, maintaining full session context across the switch, and layers tools, MCP, multi-agent orchestration, memory, and evaluation on top. Six surfaces make up the product — **routing & sessions**, **providers**, **tools & MCP**, **orchestration** (background agents + the native `/workflow` engine), **memory & safety**, and **observability/eval**. Any backend can serve either role — see [docs/guides/providers.md](../guides/providers.md) for the full catalog and the one routing constraint milk expects you to honor (escalation should be smarter/pricier than primary).

The primary use case is code assistance and shell automation for a single user.

This page is the architecture/CLI reference. For task-oriented docs, start at [docs/start/getting-started.md](../start/getting-started.md); see also [docs/guides/workflows.md](../guides/workflows.md), [docs/guides/tooling.md](../guides/tooling.md), [docs/guides/operations.md](../guides/operations.md), and [docs/reference/eval.md](eval.md).

---

## Architecture

### Components

```
milk [prompt | flags]
       │
       ▼
┌─────────────────────────────────────────────────────┐
│  Session State Machine                              │
│  ROUTING → LOCAL | ESCALATION | ESCALATION_WAITING  │
└──────────┬──────────────────────────────────────────┘
           │
           ▼
┌──────────────────────────┐
│  Router                  │
│  1. Explicit flags       │  --escalate, --primary
│  2. Session state check  │  ESCALATION_WAITING → bypass
│  3. Rules layer          │  heuristics + weighted scorer
│  4. Primary model        │  primary model self-classification
│  5. Default: try local   │
└────────┬─────────────────┘
         │
    ┌────┴──────────┐
    ▼               ▼
PRIMARY              ESCALATION
agent                agent
(any AgentConfig)    (any AgentConfig)
local / bedrock /    local / bedrock /
claude-cli / …       claude-cli / …
```

Routing mechanics, session states, and the native `/workflow` engine are covered in [docs/guides/workflows.md](../guides/workflows.md).

---

## Project layout

```
cmd/milk/
  main.go           Cobra root command (single-prompt mode); builds TurnRunner instances
  repl.go           bubbletea TUI — transcript viewport, textarea, status bar, /agent switch
  runner.go         TurnRunner interface + three implementations (localRunner, cliRunner, subprocessRunner)
  dispatch.go       runPrimary / runEscalation — role-specific session bookkeeping shared by single-shot and TUI
  interactive.go    slash commands, help text (canonical command signatures), prompt label helpers
  completion.go     tab completion: command/subcommand cycling, @-paths, value-mode insertion
  namespaces.go     parameter name-space index + multi-value list positions for slash-command completion (agents, MCP, panels, …)
  ansi.go           ANSI colour helpers and activity spinner
  notify.go         notification toasts: queue/history, expiry tick, overlay render, /notifications (ADR-0048)
  panel_memory.go   right-side memory panel (/panel memory)
  panel_workflow.go workflow progress panel (/panel workflow)
  panel_project.go  project tree panel (/panel project)
  panel_git.go      git graph panel (/panel git)
  gitgraph.go       git data fetch + code-laid commit-graph lanes
  workflow_cmd.go   /workflow slash command + interactive wizard
  workflow_runner.go workflowTurnRunner adapter (wraps TurnRunner with per-role session tracking)

internal/
  workflow/         workflow engine: Workflow interface, TurnRunner adapter interface, State/verdict/turn
                    definitions (dev.yaml, pair.yaml, swarm.yaml, team.yaml); interpreter (interp/)
  config/           config loading and defaults (~/.milk/config.json)
  session/          session state machine + JSON store (~/.milk/sessions/)
  router/           routing: rules layer → weighted scorer → primary-model classifier
  agent/local/      OpenAI-compat HTTP client; Bedrock Converse native path; SigV4/Bearer/token_cmd
                    auth transports; tool loop; streaming tool-format FSM detector
  agent/claude/     claude CLI subprocess driver; stream-json parser; permission-prompt protocol
  agent/subprocess/ generic subprocess agent: NDJSON stream protocol, tag interception
                    (<milk:need:>, <milk:percept:>, <milk:escalate:>)
  agent/aider/      aider-cli provider (wraps subprocess agent)
  agent/smolagent/  subprocess provider (wraps subprocess agent)
  escalation/       context builders: static instruction block + dynamic summary sent to escalation agent
  instructions/     loads the target repo's AGENTS.md (falling back to CLAUDE.md for the local-model
                    path only) into agent system/static context, cached by file mtime
  textbudget/       shared head+tail truncation helper for bounding large text before prompt hand-off
  memory/           Percept store; NREM decay/prune/promote consolidation (~/.milk/memory/)
  obs/              OpenTelemetry file exporters (~/.milk/otel/)
  claudesettings/   ~/.claude/settings.json reader (allowed tools, directories, AWS refresh command)
  oversight/        remote oversight interface (Telegram notifier)
  tags/             milk tag constants (<milk:need:>, <milk:percept:>, <milk:escalate:>)
```

### Agent dispatch layers

```
TurnRunner.Execute()       provider-specific inference (one of three implementations)
       │
runPrimary / runEscalation role-specific session bookkeeping (dispatch.go)
       │
run() / runTurn()          single-shot or TUI entry point; builds runners, drives router
```

Any agent can be exposed as a callable tool to any other agent (Agent-as-Tool) — see [docs/guides/tooling.md](../guides/tooling.md#agent-as-tool).

---

## Claude CLI escalation mechanics

Implementation detail for the `claude-cli` provider specifically — see [docs/guides/providers.md — Claude Code CLI](../guides/providers.md#claude-code-cli) for configuration, and [docs/guides/workflows.md — Context handoff](../guides/workflows.md#context-handoff) for the cross-provider handoff format.

- **AWS credential injection**: when `aws_auth_refresh: true`, milk reads the `awsAuthRefresh` command from `~/.claude/settings.json`, runs it before each turn for fresh STS credentials, and injects them as explicit `AWS_*` env vars into the subprocess. Conflicting inherited vars (`AWS_BEARER_TOKEN_BEDROCK`, `ANTHROPIC_DEFAULT_*_MODEL`, `AWS_PROFILE`, etc.) are stripped to prevent wrong-account overrides. See ADR 23.
- **First escalation turn**:
  ```
  claude --print --output-format stream-json \
         --session-id <new-uuid> \
         --append-system-prompt-file <static-ctx> \
         --append-system-prompt-file <dynamic-ctx> \
         -- "<user prompt>"
  ```
- **Subsequent turns** (`--resume`):
  ```
  claude --print --output-format stream-json \
         --resume <escalation-session-id> \
         --append-system-prompt-file <full-static-ctx> \
         -- "<milk-context>…</milk-context>

  <user prompt>"
  ```
  Claude Code replays the system prompt recorded on the session's first request (`--system-prompt-snapshot`, default on), so a system-prompt file on resume only takes effect if that turn compacts — milk passes the full static block for exactly that case. Context that is new for the turn (dynamic summary, re-injected instructions) travels in a `<milk-context>` block prepended to the prompt, omitted when there is nothing new or it is identical to the previous resumed turn's block (`claude.WithTurnContext`, `cliRunner.Execute`).
- `session_id` is extracted from the first NDJSON message and persisted to the milk session file.

### Permission prompt flow

milk passes `--permission-prompt-tool stdio` on every invocation. When Claude wants to use a tool that isn't pre-approved, it emits a `control_request` NDJSON event and pauses; milk intercepts it and, in TUI mode, routes a blocking prompt through the bubbletea message queue (ADR-0015):

1. The agent goroutine calls `tuiInputReader.readLine(prompt)` and blocks on a channel.
2. The TUI appends the prompt to the transcript and switches key events to `handlePermKey`.
3. The user types `y`/`n` and Enter; Ctrl-C sends `n`.
4. milk writes a `control_response` JSON to Claude's stdin; the agent goroutine unblocks.

The prompt shows the tool name, key arguments, and — for `workingDir` blocks — the restricted path. The session stays alive throughout; no `--resume` round-trip needed.

`dangerously_skip_permissions` bypasses this flow entirely (auto-approve all); `/skip-permissions on|off` overrides it per session without restarting. `allowed_tools`/`add_dirs` pre-approve specific tools/directories without ever prompting.

#### Pre-flight "Stream closed" failures

A second failure class sits *before* the permission-prompt handler: Claude Code's directory-trust pre-flight check. When it fires, the tool returns `"Stream closed"` — no `control_request` event, no prompt, the turn ends silently with partial output. milk handles this in two layers:

1. **Baseline pre-approval**: `Bash`, `Read`, `Write`, `Edit` are always merged into `--allowedTools` on every invocation; `/tmp` is always added to the trusted directory list alongside `cwd`. Eliminates the failure for most fresh-workspace first turns.
2. **Post-turn detection and retry**: milk's stream parser tracks `type:"user"` NDJSON messages; a `tool_result` block whose content is `"Stream closed"` is correlated back to the tool name via a per-turn registry and recorded. After the turn, milk presents each failed tool, offers a tool/directory grant, persists it to `~/.claude/settings.json`, and retries via `--resume`. See ADR-0035.

---

## CLI Interface

### Usage

```
milk                          # interactive REPL mode
milk [flags] <prompt>         # single-prompt mode
milk [flags] <prompt> --output-format json|stream-json  # machine-readable single-prompt mode
milk serve --acp              # long-lived ACP v2 agent server (editor embedding)
milk update check             # check GitHub for a newer milk release
milk update install           # download and install it (replaces the milk binary)
```

`--output-format` and `milk serve --acp` are machine-readable output
surfaces — see [docs/guides/tooling.md — Machine-readable output](../guides/tooling.md#machine-readable-output)
and, for ACP integration specifically, [docs/reference/acp-integration.md](acp-integration.md).

### Interactive mode

`milk` with no prompt argument starts a REPL built on charmbracelet/bubbletea. The input prompt uses `❯` as the prefix. The status bar reflects the current routing state and active agent. The header line shows the logo, the tagline, the **running version** (`github.com/scoutme/milk v0.7.2  sess:…  ctrl+o palette  /help` — bold against the dim identity group, yellow `dev` for builds without version metadata), the session token/cache totals, and the discoverability pair `ctrl+o palette  /help` (the command-palette key sits beside `/help` in every width variant; under the supermilk skin both hints carry the accent — [ADR-0077](../adr/0077-tui-skins.md)); on narrow terminals the right group degrades to `v…  sess:…  ctrl+o palette  /help`, keeping the version and both hints visible (the CLI equivalent is `milk --version`, ACP clients get it from `initialize`'s `implementation.version`).

**Slash commands:** `/escalate`, `/primary`, `/sessions`, `/new`, `/clear`, `/resume`, `/drop`, `/paste`, `/skip-permissions`, `/agent`, `/colorize`, `/theme`, `/think`, `/need`, `/workflow`, `/config`, `/init`, `/open`, `/update`, `/restart`, `/interrupt`, `/notifications`, `/help`, `/exit`, plus `/permissions`, `/mcp`, `/tips`, `/bg`, `/attach`, `/otel`, `/metrics`, `/usage`, `/tasks`, `/history`, `/reload` (`/help` in-app always lists the full current set; `/list` is a deprecated hidden alias of `/sessions`, kept one release)

**Memory commands:** `/learn <statement>`, `/memory [global|session|<pattern>]`, `/memory show <pattern or #id>`, `/forget <pattern or #id>`, `/export [--no-reasoning] [session <id|prefix>] [json|<path>]` — reasoning (`<reasoning>…</reasoning>` fences / JSON `thinking` fields) is included by default and stripped by the flag (deterministic either way — fold state never enters an export); see [docs/guides/operations.md — Memory](../guides/operations.md#memory).

**Panel commands:** `/panel memory`, `/panel tasks`, `/panel background`, `/panel workflow`, `/panel project`, `/panel git` (also F1-F6) — see [docs/guides/operations.md — Keyboard shortcuts](../guides/operations.md#keyboard-shortcuts) and [docs/guides/workflows.md](../guides/workflows.md#the-native-workflow-engine). Panel open/closed choices are durable preferences by default (`remember_panel_state`, ADR-0078): saved to the folder's `~/.milk/panel-state/<name>-<hash>.json` on every manual toggle, restored at startup, and never overturned by content-driven auto-open. The memory panel is closed by default and opens when the percept store has content.

#### Tab completion and multi-value parameters

Parameter value completion is name-space aware: agent names, MCP servers, panels, workflows and tool-agents each complete at their declared positions (the `multiValueParams`/name-space tables in `cmd/milk/namespaces.go` are the single source of truth shared by completion, validation and help), with the name space shown beside the hints. On list-taking parameters a comma starts a fresh completion segment — `/mcp assign gh,sl<TAB>` completes the second server while keeping the first.

Multi-value positions — `/mcp assign|unassign` on both sides of `for`, and the `for <agent>|global` scope of `/agent tool enable|disable|add|remove` — accept comma-separated lists and apply every combination (several servers × several agents for `/mcp assign`). Validation is all-or-nothing: one invalid name rejects the whole call with a per-item report and applies nothing (no silent partial application). `global` is a scope, not an agent name, and cannot appear in a list.

**/skip-permissions** toggles `dangerously_skip_permissions` for the current session: `on` makes the escalation agent auto-approve all tool uses; `off` (default) re-enables per-tool prompting. Alone, shows current state. A red warning banner appears at startup if already on via config. A real toggle also clears the session permission mode (below) so the two "stop asking" states cannot stack.

**/permissions** manages the permission-ask policy. The bare form lists the session mode, the timed-answer policy (background asks and doom-loop confirmations) and any pending asks. `mode allow-all|deny-all|off` sets the **session permission mode** (issue #302): when set, every tool ask is answered without prompting — approved (`allow-all`) or denied with the usual `tool X was denied by user` error (`deny-all`) — and no grant is ever written to the persistent store. The mode is also settable from the prompt itself: typing `a` or `d` at a permission prompt answers the pending ask(s) and adopts the mode for the rest of the session (`Allow? [Y/n/a/d]`); over ACP the same choices are the `Always allow` / `Always deny` dialog options; the TUI status bar shows `[perms: allow-all|deny-all]` while active. The doom-loop safety gate is deliberately exempt — it keeps prompting no matter the mode. `default allow|deny [safety]` and `timeout <secs> [safety]` override the timed answers for the session (see docs/guides/operations.md).

**/agent** manages agent backends at runtime:

| Subcommand | Action |
|---|---|
| `/agent` | Show active primary and escalation backends |
| `/agent list` | List all configured backends; active marked with `*` |
| `/agent switch <name> as primary\|escalation` | Switch role to the named backend (prompts if args missing) |
| `/agent add` | Add a backend via interactive wizard |
| `/agent add name=… url=… model=… [provider=…] [api_key=…] [aws_region=…]` | Add inline |
| `/agent tool list [<agent>\|global]` | List tool-agents (effective merged for primary by default) |
| `/agent tool enable\|disable <tool> [for <agent>\|global]` | Enable/disable a tool-agent entry (prompts for missing args) |
| `/agent tool add <tool> description=<desc> [for <agent>\|global]` | Add a new tool-agent entry (prompts for missing args) |
| `/agent tool remove <tool> [for <agent>\|global]` | Remove a tool-agent entry (prompts for missing args) |

`/agent tool` arguments are parsed quote-aware: a value wrapped in `"` or `'` keeps embedded spaces and the reserved word `for` — `/agent tool add mimo-pro-local description="coding agent for specific coding task" for gemma` stores the description verbatim and scopes the entry to `gemma`. The scope separator is the last standalone `for` token, so an unquoted description may itself contain `for` as long as the scope follows it. Missing required arguments (the tool-agent name, and `description=` for `add`) open a step-by-step wizard — name → description → scope, with Enter at the scope step meaning the default (the active primary agent) — instead of a bare usage line, mirroring `/agent add` and `/agent switch`.

New backends are appended to `agents` in `~/.milk/config.json` immediately; `/agent switch` assigns a role in the current session.

**/colorize** controls transcript syntax and Markdown rendering:

| Subcommand | Action |
|---|---|
| `/colorize` | Show current mode |
| `/colorize off` | Disable all colorization |
| `/colorize fenced` | Highlight fenced code blocks only |
| `/colorize balanced` | Fenced blocks + inline Markdown (default) |
| `/colorize full` | Full glamour Markdown render — experimental |

Persisted to config immediately, effective on next render.

**/theme** switches the TUI skin:

| Subcommand | Action |
|---|---|
| `/theme` | Show the active theme |
| `/theme off` | The default look: neutral graphite chrome on dark terminals, warm cream chrome on light ones |
| `/theme supermilk` | Semi-skimmed milk skin: milk-white ground, warm ink, hairline separators, muted olive accent |

A skin restyles chrome only — header, status bar, turn labels, panel tabs, scrollbars, staleness ramp, tool rows and the reasoning surface. Slash commands, config keys and session files are unchanged. The choice is persisted as `theme` in `config.json` and applied live; `supermilk` also sets the terminal's default background/foreground/cursor and 16-colour ANSI ramp through OSC while active, and hands them back on `/theme off` or exit. `/theme` re-detects the terminal background after a switch, so `off` returns to the dark or light variant the terminal actually uses. See [ADR-0077](../adr/0077-tui-skins.md).

**/think** manages reasoning/thinking blocks (issue #208, [ADR-0067](../adr/0067-thinking-first-class-view.md)): `/think` (status: block count and fold state), `/think expand` / `/think collapse` (pin every block open/closed), `/think view` (swap the main area to a thinking-only stream; Esc returns). Every reasoning run renders as a fold container whose header is information-only — `▸ thinking · N lines` folded, `▾ thinking · N lines` expanded (`· streaming` on a block opened while it is still arriving) — folded by default at all times, streaming or complete, with no auto-open: while reasoning streams in the header's line count updates live. Fold containers render on their own background (the thinking surface), visually distinct from the standard transcript at every fold state — full-row: every container row (header, body, blank and wrapped continuation rows) paints the background across the whole row width, not just behind the glyphs. Click a header to expand/collapse just that block; **Ctrl+T** toggles the most recent block (the streaming one, if any) — either action pins the block, so one opened mid-stream stays open when streaming ends. A new block keeps the folding status the previous one was left in (folded by default, open while the user is watching); a new turn collapses the previous turns' blocks while that status is folded — `/think expand` defines the current default and opts out of the tidy entirely, which never changes the status itself. There is no hidden mode: headers always render. `/think on|off` were removed with the fold model (a `/think on|off` typed in the TUI answers with a pointer to the fold commands). Applies to both primary-model `<think>` blocks and Claude extended thinking; loop detection consumes reasoning regardless of fold state. Over ACP, `/think on|off` keeps its data-plane meaning — relay or withhold `agent_thought_chunk` thought updates — the same state as the `think` session config option, seeded from `show_reasoning`; TUI fold state is independent of it. The *starting* fold status is itself configurable via `think_fold_default` (default `folded`; `"expanded"` seeds sessions so blocks arrive open and the tidy is opted out from the start — per-session actions still win).

**/need** sets the current session goal (`/need <one-sentence goal>`); the primary agent calls this automatically when the user states a new objective. Shown in the memory panel and injected into escalation context.

**/workflow** runs a named multi-agent pipeline in the background (`/workflow status` and `/workflow cancel` manage it) — see [docs/guides/workflows.md — The native `/workflow` engine](../guides/workflows.md#the-native-workflow-engine).

**/config** manages configuration:

| Subcommand | Action |
|---|---|
| `/config` | Print current config JSON in the transcript |
| `/config show` | The same, annotated per field `[global]`/`[local]`/`[default]` (merging a local `.milk/config.json` when present) |
| `/config init` | Run the interactive setup wizard (alias: `/init`) |
| `/config open` | Open `~/.milk/config.json` in the configured editor |

Same commands on the CLI as `milk config`, `milk config init`, `milk config show`, `milk config open`. Editor selection uses `config_editors` (see below).

**`milk otel`** manages observability settings — see [docs/guides/operations.md — Observability](../guides/operations.md#observability).

**/open** opens any file in the configured editor: `/open <path>` (or `/open @<path>`, `@` stripped automatically). The agent can also open files via the `open_file` tool.

**/update** manages self-updates: it checks GitHub for newer milk releases, compares versions, and downloads and installs the appropriate binary for the current platform.

| Subcommand | Action |
|---|---|
| `/update` / `/update check` | Check GitHub now (15s timeout); explicit checks bypass the 24h throttle. In the TUI, a release already found is reported without a new request |
| `/update install` | Download and apply the newest release (status-bar progress in the TUI). The binary is replaced in place; the running milk process must be restarted to use it — `/restart` does exactly that. On Windows the running `milk.exe` is renamed aside and the verified download takes its place (applied on restart); if the rename is refused the download is staged as `milk.exe.new` and swapped in automatically at the next start (`staged — … applied on next start`), with a printed temp-path manual fallback only when staging is denied too (see docs/guides/operations.md — Self-update) |
| `/update skip` | Suppress the pending release: its tag is written to `update_skipped_version` and filtered from every later check (including the startup check) until a newer release appears |

Startup/background release checks are throttled by `update_last_check` (at most once per 24h, shared between the TUI and ACP) and gated by `update_check`/`update_channel` (see Configuration). When a newer release is found at startup, the TUI shows a status-bar badge (`⬆ <tag> available — /update install`); `milk serve --acp` sends an equivalent one-shot notice per session (see [docs/reference/acp-integration.md](acp-integration.md)).

The CLI equivalent is `milk update check` and `milk update install` (non-interactive; `install` ignores `/update skip` and always takes the latest release).

**/restart** (TUI only, issue #258) quits and relaunches milk with the very same starting command, resuming the current session:

- **Binary launch** — re-execs the (possibly just `/update install`-replaced) binary with the original argv; session-shaping flags (`--new`, `--continue`, `--session`) are replaced by `--session <current-id>` so the conversation resumes even when milk was launched with `--new`. Every other flag is replayed verbatim.
- **`task run` launch** — re-runs the same task (`task run`) in the launch directory, so the binary is rebuilt from the changed codebase instead of re-running a stale one. Detected from the parent process at startup; the session then resumes via the default resume-most-recent behavior.
- **Busy refusal** — refused while a turn, PTY shell pane, workflow, background agent or background shell is running (finish or cancel it first, e.g. Ctrl+C or `/bg stop`).
- **Safety** — the relaunch argv comes only from the process's own startup capture (argv, binary path, parent process), never from config or a model-provided string. The terminal is restored (alt-screen left) before the exec.
- **Windows handover** — Windows has no exec, so the old process stays alive as a silent waiter: the shell stays blocked on it, it stays blocked on the relaunched instance. The shell therefore can't print its prompt or read console input while the new instance takes over — the relaunched milk owns the terminal from its first frame, keystrokes can't be split between the two, and quitting it returns exactly one clean prompt (previously the two processes raced the shared console and garbled the screen). The waiter absorbs console Ctrl+C/Ctrl+Break events (both surface as `os.Interrupt` on Windows; `signal.Ignore` would not protect it), and process-global subsystems — the oversight poller, background jobs — are cancelled before the handover so only one instance owns them. Cost: one idle process per past `/restart` in the same window. Live-verified on Windows (see issue #313).

Not available over ACP (the client cannot survive its server process being replaced) or in one-shot CLI mode. A `go run ./cmd/milk` launch is not restartable (the temporary binary is gone after exit) — use `task run` or an installed binary.

**/interrupt** (issue #288) cancels the running turn — the Ctrl+C equivalent (`handleBusyKey`'s action: `cancelTurn` + the `[interrupted]` completion note) — on every surface:

- **TUI** — typed like any command (also via busy-Enter; `busySafeCommands` keeps it executable while a turn runs, where other commands answer "unavailable while agent is responding"). With no turn running: `nothing to interrupt`.
- **Remote input (Telegram)** — routed around the busy queue: it acts on the very turn that would otherwise swallow the message; an idle interrupt never reaches the model as prompt text.
- **Recognition** — the shared first-token rule, exactly like every other slash command: the message/prompt must *start* with `/interrupt` (exact, or with trailing words like `/interrupt stop it` — rest ignored); an occurrence **mid-input** (`please /interrupt now`) or a lookalike (`/interrupted`) is ordinary text on every surface — it queues (busy) or reaches the model (idle), it never cancels.
- **ACP** — an input whose **first token** is `/interrupt` is intercepted before `turnMu` (it must not queue behind the turn it targets), echoes to the client, cancels and answers immediately; mid-input stays plain prompt text, and idle it runs through `acp_commands.go` like every other command. Native `session/cancel` is unchanged.
- Interrupted turns notify remote oversight as `⏹ *agent* turn interrupted` (a cancellation is not an error).

**Commands usable while a turn is running**: beyond `/interrupt`, the permission family — `/permissions` (listing and `mode|default|timeout` overrides) and `/skip-permissions` — runs at once instead of waiting for the turn: they act on session-level settings guarded by a mutex / atomics (the running turn's background asks and permission checks read the same state), dispatch nothing, and are what a user needs while a turn is waiting on, or about to raise, an ask. TUI: in `busySafeCommands`; ACP: `acpBusySafe` is intercepted before `turnMu` like `/interrupt` (echo + result, the running turn untouched); a test pins ACP's list as a subset of the TUI's.

**Multi-line input**: Shift+Enter/Alt+Enter inserts a newline; Enter submits. Bracketed paste is handled transparently.

**Clipboard binary paste**: when a bracketed paste yields empty text, milk probes the system clipboard (`xclip`/`wl-paste`) for non-text content (e.g. `image/png`); found content is saved to a temp file and staged as a pending attachment, same as `/attach`. No special key combination beyond a normal paste gesture.

**Pasted content never triggers commands**: a slash command, `!`-prefixed shell command, or direct-bash shortcut only fires when its leading token was typed, not pasted — pasting a transcript that happens to start with `/learn` or `!rm -rf` is sent to the agent as inert prompt text instead of executing. Typing a command and then pasting its argument (e.g. `/escalate ` followed by a pasted paragraph) is unaffected, since the paste doesn't occupy the leading position.

**Keyboard**: Up/Down navigates input history (single-line mode); Ctrl-C clears a pending force-mode flag or exits; Ctrl-D exits.

### Flags

| Flag | Description |
|---|---|
| `--config-root <dir>` | Global (applies to every command): use `<dir>` as milk's state root — `config.json`, sessions, memory, jobs, history, otel, `mcp_oauth` — instead of `~/.milk`, and export it to child processes. `$MILK_CONFIG_ROOT` is the env equivalent. Use for a clean sandboxed setup (tests, demos) without touching the real config |
| `--escalate` | Force route to escalation agent for this turn |
| `--primary` | Force route to primary agent for this turn; breaks ESCALATION_WAITING state |
| `--new` | Start a new session (old sessions for cwd untouched) |
| `--session <id|prefix|name>` | Target a stored session by exact id, unambiguous id prefix, or cwd-scoped name; with `--new`, the new session's name. An unknown reference is an error (no silent create) unless `--new` is also given |
| `--continue` | No-op explicit alias of the default resume behavior (the default already resumes) |
| `--list` | List sessions for current cwd |
| `--list --all` | List all sessions across all directories |
| `--drop [--session <ref>]` | Delete the referenced session, or — without a ref — the cwd's current session |
| `--output-format text\|json\|stream-json` | Single-prompt mode only (ignored by the REPL); `text` (default) is unchanged, `json`/`stream-json` emit machine-readable output — see [docs/guides/tooling.md](../guides/tooling.md#machine-readable-output) |
| `--turn-timeout <duration>` | Single-prompt mode only: hard wall-clock budget for the run (default `20m`, `0` disables). On expiry the turn fails with a named `turn budget of … exceeded (--turn-timeout, 0 disables)` error on stderr (exit 1), `stream-json` adds a `system/error` banner before the terminal `result`, and any text streamed before the kill is kept as partial output. TUI/session turns are bounded by `limits.turn_deadline_secs` instead |

**Resumed sessions in the TUI.** When the TUI starts on a stored session (the default for the cwd, or `--session`/`--continue`), the transcript is pre-populated with its prior user/assistant turns instead of the welcome screen, under a `[milk] resumed session …` banner. The window mirrors ACP's history replay: the first 10 and last 200 turns, each message capped at 8 KB, with a gap marker pointing at `/export` for the rest; tool calls and reasoning are not replayed. `--new` (and a session with no history) starts empty; `/new`, `/clear` and `/drop` keep the visible transcript as is.

---

## Configuration

`~/.milk/config.json` — see [docs/guides/providers.md](../guides/providers.md) for the full `agents`/provider field reference, [docs/guides/workflows.md](../guides/workflows.md#customizing-the-rules-block) for the `rules` block, and [docs/guides/tooling.md](../guides/tooling.md) for `agent_tools`/`mcp_servers`. This section covers the remaining root-level behavior fields.

```json
{
  "agent": "local",
  "agents": [ { "name": "local", "url": "http://localhost:8080", "model": "qwen2.5-coder", "provider": "local" } ],
  "escalation_agent": "claude",
  "colorization": "balanced",
  "show_reasoning": true,
  "think_fold_default": "folded",
  "sticky_escalation": true,
  "copy_on_select": false,
  "queue_while_busy": true,
  "acp_resume": true,
  "experimental_lazy_history_management": false,
  "aws_auth_refresh": false,
  "update_check": true,
  "update_channel": "pre"
}
```

`agent` names the active primary backend; empty defaults to the first non-`claude-cli` entry. `escalation_agent` selects which entry handles escalated turns (default `"claude"`) — a built-in `claude-cli` entry named `"claude"` is always available even if unlisted.

### `colorization` field

See [/colorize](#interactive-mode) above for the four modes and their behavior.

### `show_reasoning` field

The default for the ACP `think` session option: whether thinking/reasoning tokens relay to ACP clients as `agent_thought_chunk` updates (and are replayed in ACP history). The TUI always renders reasoning as foldable blocks — its fold state is independent of this field. See [/think](#interactive-mode) above.

### `think_fold_default` field

Unset (or `"folded"`) or `"expanded"` — the TUI's **starting** fold status for thinking blocks (issue #278): by default blocks arrive folded, exactly as before; `"expanded"` seeds each session so blocks arrive open and the turn-boundary tidy is opted out from the start. It seeds only — per-session fold actions (header click, Ctrl+T, `/think expand|collapse`) still win for the rest of the session, and it never re-couples `show_reasoning` (the data-plane relay toggle) to TUI display. A session switch (`/resume`) re-seeds the configured starting status. See [/think](#interactive-mode) above.

### `copy_on_select` field

Default `false`. When `true`, releasing the mouse after a drag selection (transcript, side panels, or the live-attach view) copies the selected text to the clipboard immediately and shows the `copied N chars` status-bar feedback, instead of waiting for a right-click or Ctrl+C. The highlight is cleared after the copy, so a following right-click pastes. A plain click without a drag never copies. Hot-reloaded.

Independently of `copy_on_select`, dragging a selection past the top or bottom of the transcript, a side panel or the live-attach view scrolls that view (about 20 lines/s while the pointer rests outside) and extends the selection with it. Releasing outside the view ends the drag.

### `remember_panel_state` field

Default `true`. When `true`, a manual side-panel toggle (`/panel <name>` or F1-F6) is a durable preference: the open/closed state of every panel is saved to the folder-bound `~/.milk/panel-state/<name>-<hash>.json` and restored at startup — as *startup layout* only (ADR-0087 amending ADR-0078): a restored choice never seeds the manual override, so new content still auto-opens the panel it belongs to. Only manually toggled panels are saved — auto-opened state is transient. In a session the in-session veto follows ADR-0059 either way: a close sticks exactly while the content it was dismissed over stays live (task store drained, jobs finished, workflow done, percept store emptied), then re-arms auto-open for the next content. When `false`, panels are session-only with nothing read or written. See [ADR-0078](../adr/0078-persisted-panel-state.md) and [ADR-0087](../adr/0087-panel-auto-open-master-switch.md).

### `panel_auto_open` field

Default `true`. Master switch for content-driven panel auto-open (ADR-0087): with it on, panels open themselves when their content becomes active (a task is created, a background job spawns, a workflow starts, percepts arrive), the memory panel follows its content-driven startup default, and manual closes stick per the ADR-0059 rule above. With it off, panels open only on an explicit `/panel <name>` toggle (or F1-F6) — no auto-opens, no toasts, no startup opens. Toggled by `/panel autoopen [on|off]`, which writes this key.

### `config_editors` field

Ordered list of editor commands tried by `/config open` and `/open`; first found on `$PATH` wins. Env vars (`$EDITOR`, `$VISUAL`) are expanded before lookup. Default: `["$EDITOR", "$VISUAL", "nano", "vim", "vi"]`.

### `max_background_agents` field

Maximum concurrent `spawn_background_agent` jobs (ADR-0043) per session; non-positive or unset falls back to `3`. See [docs/guides/operations.md — Background sub-agents](../guides/operations.md#background-sub-agents).

### `background_agent` field

Default agent for user-started background jobs (`/bg start`, Ctrl+Enter while busy): an agent name, or `primary` / `escalation`. Unset keeps the escalation agent's inference-server backend, else the primary's. The TUI still asks per job (blank answer = this default); `/bg start --agent <name>` skips the question. See [docs/guides/operations.md — Background sub-agents](../guides/operations.md#background-sub-agents).

### `background_agent_timeout_minutes` field

Per-job hard timeout for a `spawn_background_agent` job (ADR-0043), in minutes, once it starts executing; non-positive or unset falls back to `20`. See [docs/guides/operations.md — Background sub-agents](../guides/operations.md#background-sub-agents).

```json
"config_editors": ["code --wait", "$EDITOR", "nano"]
```

### `update_check` field

Whether milk checks GitHub for new releases at startup (TUI and `milk serve --acp`). Checks are additionally throttled to one per 24h via `update_last_check`, and a release suppressed with `/update skip` is remembered in `update_skipped_version`. Default: `true`.

### `update_channel` field

Which releases count as updates: `"pre"` (default) includes pre-releases, `"stable"` only final releases.

### `acp_resume` field

Resume-by-default adoption over ACP (`milk serve --acp`): when on (the default), `session/new` adopts the working directory's most recent stored session — with mandatory bounded history replay and a one-line notice — instead of starting empty. It is the TUI's `--continue` default mapped onto the entry point ACP clients actually call. Set `false` for strict-spec behavior (`session/new` always starts fresh); the per-request escape hatch is `_meta.milk.fresh: true`. Full semantics in [docs/reference/acp-integration.md — Session resume, list, and history replay](acp-integration.md#session-resume-list-and-history-replay).

### ACP session parity

The TUI and `milk serve --acp` share one session model — one conversation per working directory — and one session store, so a conversation continues the same way whichever surface opens it: TUI `--continue` ≈ ACP `session/new` adopting (the `acp_resume` default), TUI `--new` ≈ `acp_resume: false` or a per-request `_meta.milk.fresh: true`, and the in-chat vocabulary is one set on both surfaces: `/sessions` (renamed from `/list`), `/new`/`/clear`, `/resume <id|prefix|name>` ≈ `session/resume`, `/drop`. Over ACP a switch rebinds the conversation's store session behind its fixed wire handle (view rebinding — see [ADR-0051](../adr/0051-acp-conversation-view-rebinding.md)). Session management over ACP (`session/list`/`resume`/`close`/delete, the session slash commands, `/export [--no-reasoning] [session <id|prefix>] [json|<path>]`) and standard chat-history replay are specified in [docs/reference/acp-integration.md](acp-integration.md).

### `experimental_lazy_history_management` field

When `true`, agent context is built lazily: only the current agent's own turns and user turns directed at it are included verbatim; contiguous blocks of other-agent turns collapse into a placeholder (e.g. `[escalation]: (14 turns omitted)`), retrievable on-demand via `get_session_context`. Significantly reduces context size for sessions with heavy cross-agent activity. User turns always show `[user to <agent>]` labels regardless. Default: `false`.

---

## Streaming

Both agents stream output in real time: SSE from OpenAI-compat APIs (`stream: true`), AWS binary event-stream from Bedrock Converse, or NDJSON from Claude's `--output-format stream-json`. milk relays tokens to stdout as they arrive.

---

## Backlog

- Planning mode (offline, no LLM execution)
- Demotion from escalation back to primary mid-session
- Web UI / TUI
- MCP stdio transport for local tools ✓ (done)
- Multi-user / daemon mode

---

## Acknowledgments

milk's prompt/context-management and loop-detection design was informed by studying (not copying
code from) other coding-agent harnesses — independent implementations compared directly against
milk's own, not ports:

- **[Claude Code](https://claude.com/claude-code)** — the escalation-agent subprocess milk drives via `claude --print`, and the reference for milk's own project-instruction loading (AGENTS.md/CLAUDE.md).
- **[MiMo-Code](https://github.com/XiaomiMiMo/MiMo-Code)** (Xiaomi's fork of OpenCode) — compared for loop-detection thresholds (the doom-loop gate, n-gram repetition monitor), prompt-caching breakpoint strategy, and checkpoint/distillation memory design.
- **[OpenCode](https://github.com/anomalyco/opencode)** — compared for `AGENTS.md` project-instruction loading, sub-agent context isolation (`ForkContext`), and prompt-caching strategy.
- **[Aider](https://aider.chat/)** — its [repo map](https://aider.chat/2023/10/22/repomap.html) design informed `project_map`'s outline contract (definition lines, never bodies) and its reference-ranked budget pruning, plus the `project_map_context` push/hybrid (ADR-0081).

The full comparison (findings, what was and wasn't adopted, and why) lives in a design analysis
kept out of this published site — see the `prompt-context-management-review.md` doc in the
repository root's `docs/` directory on [GitHub](https://github.com/scoutme/milk).
