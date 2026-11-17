# 46. Interactive Attach to the Claude CLI Escalation Session

Date: 2026-09-26

Status: Proposed (grounded on `main` 2026-11-17)

## Context

ADR-0003 chose to drive Claude Code as a `claude --print --output-format stream-json` subprocess rather than embedding its interactive TUI. That decision still holds: the stream-json path is what gives milk structured per-turn data — token usage and cache hits, subagent/workflow rollups, tool events, permission denials, percept/need tags, the final result text — and it is what the router, sticky escalation, loop detection, and the `/usage` table are built on.

What the `--print` path cannot offer is Claude Code's own interactive surface: its slash commands (`/model`, `/compact`, `/agents`, plan mode, …), native permission dialogs instead of milk's `--permission-prompt-tool stdio` bridge (ADR-0037), background agents/workflows that keep running while Claude stays open instead of milk polling the workflow journal after `--print` exits, and Claude's own rendering of long tool-heavy turns. For a stretch of hands-on work with Claude, users today have to leave milk, run `claude --resume <id>` by hand (after digging the ID out of the session file), and come back to a milk session that knows nothing about what happened in between.

[herdr](https://github.com/herdrdev/herdr) was reviewed as prior art. It takes the opposite approach to ADR-0003 entirely: it runs the unmodified interactive `claude` TUI in a PTY (libghostty-vt as the terminal emulator), infers `working`/`blocked`/`idle` by regex-matching screen regions (`distribution/agent-detection/claude.toml`), types prompts in via bracketed paste + delayed Enter, and recovers transcript text by synthesising mouse-wheel scrolls over Claude's alternate screen. Its only Claude hook is `SessionStart`, used to learn `session_id`/`transcript_path` for `claude --resume <id>` after a restart. Two things stand out:

- **Screen-scraping is a maintenance treadmill.** herdr ships remotely-updatable, versioned detection manifests with comments like *"Braille covers <= 2.1.227; half-circles are the 2.1.228 busy spinner"*. Adopting that model as milk's primary integration would trade structured data for regexes that break on Claude UI releases. Rejected (see Alternatives).
- **"Attach" is the valuable part, and it does not need scraping.** Letting the user take over the real Claude TUI for a while — and getting structured data back afterwards from Claude's own transcript JSONL rather than the screen — gives the interactive surface without any of herdr's fragility.

### Verified behaviour (Claude Code 2.1.283, 2026-09-26)

Checked live in a scratch git repo before writing this ADR; the code-grounded facts were re-checked against `main` on 2026-11-17 (see Grounding update at the end), the CLI-behaviour claims remain dated to that version:

1. **Sessions are shared across modes.** A session created by `claude --print --output-format stream-json` resumes interactively with `claude --resume <id>`: prior turns are shown, the **same `session_id`** is kept, and new turns are appended to the **same** `~/.claude/projects/<encoded-cwd>/<id>.jsonl`. The reverse also holds: a later `--print --resume <id>` sees the interactive turns. So milk's `EscalationSessionID` works unchanged in both directions.
2. **An assistant message is split across several JSONL lines** — one per content block (`thinking`, `text`, `tool_use`) — which share `message.id` and **each repeat the full `usage`**. Token accounting must deduplicate by `message.id`.
3. **An interrupted turn** leaves a synthetic assistant line (non-`msg_` id, zero usage, text `"No response requested."`).
4. **A `SessionStart` hook injected via `--settings`** fires in interactive mode with `session_id`, `transcript_path`, `cwd`, and `source` on stdin. **`/clear` starts a new session**: the hook fires again with `source: "clear"`, a new `session_id`, and a new transcript file. `/resume` inside Claude can switch sessions the same way.
5. **`--system-prompt-snapshot` defaults to `on`**: the system prompt (including `--append-system-prompt[-file]`) is recorded on a conversation's first request and replayed verbatim on every later resume until compaction. An `--append-system-prompt-file` passed on resume is **silently ignored** — in `--print` and interactive mode alike — unless `--system-prompt-snapshot off` is passed. (This also affected the existing `--print` path's per-turn dynamic context; see Consequences.)
6. **After a compaction the snapshot is re-recorded** from the `--append-system-prompt-file` of the launch that compacted, and replayed from then on.
7. A positional prompt (`claude --resume <id> '<prompt>'`) is submitted as the first interactive turn.

## Decision

Add **interactive attach** as an *alternative presentation* of the `claude-cli` escalation agent, alongside — not replacing — the `--print` integration. Both operate on the same Claude session (`EscalationSessionID`); the user can alternate between them freely, and the `--print` path remains the default for every routed, `/escalate`d, and sticky turn.

### 1. Command

`/handoff [prompt]` — suspends the milk TUI and hands the terminal to the real interactive `claude` resumed on the current escalation session. With a `prompt`, it is passed positionally and submitted as the first turn. Exiting Claude (`/exit`, Ctrl+D) returns to milk, which ingests what happened.

Named `/handoff`, not `/attach`: this ADR originally proposed `/attach`, but `/attach <path>` has since been taken (file-attachment staging, `handleAttachCmd`) and ADR-0047 has adopted "attach" for its live-attach view of background jobs and workflows. `/handoff` is free and matches the feature name.

Preconditions (otherwise a transcript error explains why):

- The current escalation agent's provider is `claude-cli`. (v1 targets the escalation role only; a `claude-cli` primary is out of scope.)
- No agent turn is in flight (`m.busy`) and no PTY pane is active. Background jobs (ADR-0043) may keep running.

No escalation session yet → a fresh one is started with a pre-assigned `--session-id <uuid>` (as `RunFirst` does), so a hand-off doubles as a way to escalate into an interactive session.

### 2. Launch — one argument builder for both modes

`(*claude.Agent).run` currently builds the shared prefix (`--dangerously-skip-permissions` / `--permission-prompt-tool stdio`, `--allowedTools`, `--add-dir`, `--settings`, `--mcp-config`) inline. Extract it into a helper used by both `run` and a new `(*claude.Agent).InteractiveCmd(...) (*exec.Cmd, cleanup func(), error)` so the two modes cannot drift (same MCP servers via `mcpauth.ResolveHeader`, same allowed tools, same extra dirs, same env). Differences in interactive mode:

- No `--print`, `--output-format`, `--verbose`, `--include-partial-messages`.
- No `--permission-prompt-tool stdio` — Claude's native permission dialogs are used; milk's permission bridge (ADR-0037) does not apply while attached. `skip_permissions` still maps to `--dangerously-skip-permissions`.
- Context (full static instructions + any pending `BuildDynamicContext` delta, e.g. the summary of local turns since the last escalation) goes in `--append-system-prompt-file` **with `--system-prompt-snapshot off`** for the hand-off launch. The `--print` path instead prepends per-turn context to the resumed user message (see Consequences), but a hand-off cannot do that without auto-submitting a turn when the user gave no prompt. With the snapshot off, the file applies to every request of the attached process and is never recorded, so the next `--print` resume replays the session's original snapshot again. Cost: one prompt-cache rewrite when the hand-off starts and one on the first `--print` turn after it.

The command runs via `tea.ExecProcess` (already used for `/config edit` and opening files), which releases and restores the alt-screen.

### 3. Session tracking — an injected `SessionStart` hook

Because `/clear` and `/resume` change the session ID mid-attach (#4), milk cannot assume the ID it launched with is the one to resume afterwards. Following herdr's one reliable hook, milk merges a `SessionStart` hook into the `--settings` JSON it already passes:

```json
{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command",
  "command":"<milk-executable> internal handoff-hook <state-file>"}]}]}}
```

`milk internal handoff-hook` is a hidden Cobra subcommand (the `internal` namespace is free) that appends the hook's stdin JSON as one line to a per-attach temp file — using milk's own binary keeps it portable (no `sh`/`python3` dependency, unlike herdr's script). On return, the last line is authoritative for the new `EscalationSessionID`; every line's `transcript_path` is read, so work done before a `/clear` is ingested too. If no hook line exists (hook failed), milk falls back to the **deterministic transcript path** — the same computation eval's `claude-code` adapter already uses (`~/.claude/projects/<encoded-cwd>/<id>.jsonl`, honouring `CLAUDE_CONFIG_DIR`, valid when the workdir is its own git repository and otherwise falling back to the enclosing repo's project directory) — rather than globbing.

### 4. Ingestion — transcript JSONL, not the screen

Before launch, milk records the byte size of the current transcript (0 for a new session). On return, a new `internal/agent/claude` transcript reader parses the lines appended since then and produces a delta. The snapshot-then-parse-since technique is exactly what eval's `adapter_claude.go` already does per turn (`prevSize := fileSize(a.transcriptPath)` before the turn), so the approach is proven in-repo:

- **Exchanges**: user lines whose content is a plain prompt become user turns; `tool_result`-only user lines and Claude local-command/meta lines are skipped. Assistant `text` blocks become escalation turns; `thinking` blocks feed the reasoning transcript variant; `tool_use` blocks become one-line tool entries.
- **Tokens**: summed once per distinct `message.id` (#2); synthetic zero-usage messages are ignored (#3). Model taken from `message.model`.
- **Percepts/needs**: assistant text is scanned for the session's `<milk:percept:NONCE>` / need tags with the same parser the stream path uses (`perceptWriter`), so memory keeps working across a hand-off.

The eval harness's `adapter_claude.go` parses the same JSONL format with its own types — it has since gained `message.id` deduplication independently (`seen` map over `l.Message.ID`), so it no longer over-counts — but the type duplication remains; it switches to the shared reader.

### 5. Bookkeeping on return — same path as a `--print` escalation turn

The post-turn bookkeeping in `dispatch.go`'s `runEscalationWithSession` (AddTurn, `EscalationSessionID`, `AddTokensFull` + `obs.RecordTokens`/`AccumulateCacheTokens` under the `escalation` role, state transition, auto-sticky) is extracted into a helper that both `runEscalationWithSession` and the hand-off-return handler call, rather than re-implemented. Consequently:

- The session state after a hand-off is whatever a completed escalation turn with that final assistant message would produce (`ESCALATION`, or `ESCALATION_WAITING` if it ends on a question), and `autoStickyEscalate` follows `sticky_escalation` exactly as for a routed escalation.
- A hand-off counts as an escalation for `returning_fresh_start_local_turns`.
- The milk transcript gets a delimited block — `── handed off to <agent>: N turns · tokens ──` — with the user prompts, assistant text, and one-line tool entries, so the milk-side record reads like the turns happened in milk.
- No new lines → a one-line "hand-off ended, no new turns" note and no state change. A non-zero Claude exit still ingests whatever was appended, then reports the error.

### 6. Presentation phases

- **Phase 1 (this ADR):** full-terminal via `tea.ExecProcess`, as above.
- **Phase 2 (optional, later):** render the attached Claude inside milk's existing embedded PTY pane (`cmd/milk/pty_pane.go`, creack/pty + midterm), keeping milk's status bar and panels visible and enabling live token updates by tailing the transcript during the attach. Requires upgrading the pane first: resize propagation (`WindowSizeMsg` → `pty.Setsize` + VT resize), modifier keys Claude relies on (Shift+Tab, Alt/Shift+Enter, Ctrl+O/R), bracketed paste, and mouse forwarding — all still open in `pty_pane.go`. An alternative to weigh at that point: drive Claude inside `internal/tmux` (milk's tmux driver, which eval's `claude-code` adapter already uses to run Claude Code) instead of upgrading the pane — external tmux dependency versus pane work. Decided on its own once phase 1 has been used.

## Alternatives considered

- **Replace `--print` with a PTY-driven interactive Claude (herdr's model).** Rejected: loses the structured stream milk's routing, loop detection, and accounting depend on, and needs a screen-scraping rule set that must track every Claude UI release (herdr's versioned, remotely-updated manifests are the evidence). Attach gets the interactive surface without this.
- **Persistent `--input-format stream-json` process instead of per-turn `--resume`.** Orthogonal — it would improve the `--print` path (warm process, prompting while a turn runs) but gives no interactive UI. Worth its own ADR.
- **Screen-scrape the attached session for results.** Rejected: the transcript JSONL is structured, complete (not limited to the visible screen), and already parsed by the eval harness.
- **Claude hooks as lifecycle/state authority.** Not needed — milk only needs state *after* the attach ends — and herdr documents that Claude's hooks miss permission outcomes and Esc interrupts.

## Consequences

- Users get Claude Code's full interactive surface on demand without leaving milk or losing session continuity; milk's session, token accounting, memory, and routing state stay correct afterwards.
- While attached, milk is not in the loop: no routing, no loop detection, no milk permission bridge, no live status-bar tokens (phase 1). This is the point of the mode, and it ends as soon as the user exits Claude.
- Percept tags the escalation agent writes are visible raw in Claude's UI during a hand-off (the `--print` path filters them from display). Cosmetic; accepted.
- Subagent/workflow token rollups are only attributed to the main `escalation` role in v1 — interactive transcripts carry no `result` line, and subagent transcripts live in separate files. Follow-up.
- **Found while verifying #5, fixed independently of this ADR (landed as `WithTurnContext` in `internal/agent/claude/claude.go`):** the `--print` path's `RunResume` used to deliver per-turn context only through `--append-system-prompt-file`, which Claude Code's snapshot default ignores — returning escalations never saw the brief, current goal, local-turn summary, or re-injected instructions. The landed fix splits what `RunResume` sends: the full stable static block always goes to `--append-system-prompt-file` (inert until a compaction re-records it, #6, so milk's instructions survive compaction), and the changed per-turn context is prepended to the user message in a `<milk-context>` block, so it reaches Claude and stays in the conversation. The attach ingester (§4) must strip that block from ingested user prompts.
- Open to verify during implementation: that a `SessionStart` hook in `--settings` is additive to hooks in the user's `~/.claude/settings.json` rather than replacing them; and whether bubbletea delivers background-job messages to `Update` during `ExecProcess` or only after return.

## Grounding update — 2026-11-17

Re-checked against `main` before landing as Proposed. Three weaknesses fixed in the text above; none affects the decision:

1. **Command-name collision (the one real defect).** `/attach` was free when this ADR was drafted; it is now `/attach <path>` file staging (`handleAttachCmd`), and ADR-0047 has since adopted "attach" for its live-attach view. Renamed to `/handoff`, hook subcommand to `milk internal handoff-hook` (hidden `internal` namespace confirmed free).
2. **Stale eval-adapter claim.** `eval/adapter_claude.go` now deduplicates usage by `message.id` (it did not in September). The shared-reader consolidation remains worthwhile — it still carries its own JSONL types — but is no longer motivated by a dedupe bug.
3. **The `fix/claude-resume-context` work has landed** (`WithTurnContext`, `<milk-context>` block) — the Consequences note now points at the landed code instead of a parallel branch, and the ingester's strip requirement is concrete.

New in-repo evidence strengthening the design (no decision changes): eval's `claude-code` adapter already runs Claude Code under `internal/tmux` and performs the §4 technique per turn — deterministic transcript path, byte-size snapshot before, parse-lines-since after. `tea.ExecProcess`, `mcpauth.ResolveHeader`, `EscalationSessionID` + `RunFirst`'s `--session-id` pre-assignment, `perceptWriter`, and `runEscalationWithSession`'s bookkeeping shape all confirmed in place.
