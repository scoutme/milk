# Prompt & context management review — primary, escalation, background agents, workflows

Date: 2026-09-29

## Implementation status (2026-09-30)

All 12 recommendations in §8 have been implemented, tested (unit + integration; live-verified
against real backends where the fix's correctness depended on model behavior — project
instructions, compaction summarization, the bash permission-pattern gate), and merged to `main`
locally, one commit per step:

| # | Recommendation | Commit |
|---|---|---|
| 1 | Project-instruction file loading (AGENTS.md/CLAUDE.md) | `feat(prompt): load project instructions...` |
| 2 | Tail-aware tool-result truncation | `fix(local): keep both head and tail...` |
| 3 | Bound background-agent result hand-off | `fix(dispatch): bound background-agent result size...` |
| 4 | LLM-driven context compaction fallback | `feat(local): summarize dropped history...` |
| 5 | ADR-0004 doc correction + caching measurement | `docs(adr-0004): correct the "two separate flags" claim...` |
| 6 | Consolidate duplicated instruction prose | `refactor(prompt): share the config-write warning...` |
| 7 | Percept re-injection on resumed escalation turns | `feat(escalation): re-inject new percepts...` |
| 8 | Pattern-scoped bash permission pre-approval | `feat(local): pattern-scoped bash permission pre-approval` |
| 9 | Hard doom-loop gate for identical repeated tool calls | `feat(local): hard doom-loop gate...` |
| 10 | Distinguish budget-exhausted from stuck-loop in workflows | `fix(workflow): distinguish budget-exhausted steps...` |
| 11 | Multi-breakpoint Bedrock prompt caching | `feat(bedrock): rolling double-buffer cache breakpoints...` — **unverified**, no Bedrock agent available |
| 12 | Structured bg-job results, cancellation tool, per-workflow memory opt-in | `feat: structured background-job results...` |

Not yet pushed to `origin/main` or opened as PRs — still local-only pending review. See the
implementation plan this executed: `~/.claude/plans/moonlit-questing-puffin.md` (session-local,
not in the repo).

## Method

Code-level analysis of milk's four prompt/context surfaces (primary agent, escalation agent,
background sub-agents, workflow engine), cross-referenced against three coding-harness
references: Claude Code (this tool, self-knowledge), and two local clones read directly —
[MiMo-Code](https://github.com/XiaomiMiMo/MiMo-Code) (Xiaomi's fork of OpenCode) and
[OpenCode](https://github.com/anomalyco/opencode) itself. All findings below are backed by
file:line citations gathered in the underlying research passes; this document is the synthesis.

One incidental finding while reading the OpenCode clone: several of its prompt files
(`session/prompt/plan-reminder-anthropic.txt`, `default.txt`) are near-verbatim scrapes of
Claude Code's own real system/reminder prompts (including one captured real file path), which
briefly tripped this session's prompt-injection heuristics when quoted. It's inert — data found
in a third-party repo, not a live instruction — but worth knowing if you read that repo yourself.

---

## 1. Primary (local) agent — `internal/agent/local/`

### Current mechanism

- System prompt is a static Go template (`buildSystemPrompt`, `local.go:973-1046`), role-aware
  (primary/escalation/workflow) and tiered (`minimal`/`standard`/`full` via `system_prompt_tier`).
  `systemPromptShared` (`local.go:915-937`) hardcodes tool-use rules, a memory-tool mandate, a
  git-commit protocol, and a config-help pointer — all Go string constants, nothing loaded from
  the target repo.
- Percepts / current-goal orientation are injected as synthetic leading `system` messages in the
  per-turn history array (`runner.go:266-280`), not folded into the cached system prompt.
- History: `sessionToUnifiedMessages` → `buildAgentHistory`, trimmed by `trimLocalMessages`
  against a char budget (`message_budget_chars`/`local_context_budget_chars`, default 24000,
  `docs/providers.md:673,702`).
- Tool-result truncation: `capMemToolResult` (`local.go:1787-1813`) is a **tail-cut** — keeps the
  head, drops the tail, appends `"... (truncated)"`.
- Prompt caching: OpenAI-compat implicit caching is read-only telemetry (parses
  `usage.prompt_tokens_details.cached_tokens`). Bedrock explicit caching appends exactly **one**
  `cachePoint` at the end of the system array (`bedrock.go:209-218,263`) — system-only, single
  breakpoint, marked "experimental, not live-tested" in the code itself.
- Task-state surfacing (`taskToolGuidance`, `backgroundAgentGuidance`, `local.go:956-971`) is a
  genuine strength: a working analogue of Claude Code's TodoWrite guidance, conditionally appended.

### Weaknesses / gaps

1. **No project-instruction file loading at all.** No AGENTS.md/CLAUDE.md-equivalent is ever
   read by the primary agent. Every other harness reviewed here (Claude Code, MiMo-Code,
   OpenCode) has this. Biggest single gap — see §6 for the cross-project detail.
2. **Context management is hard truncation, not summarization.** `trimLocalMessages` and the
   session "brick" builder both drop the *oldest* turns verbatim past a char budget; there is no
   LLM-driven compaction step. Facts are silently and permanently lost mid-session. Docs are
   honest about this (`docs/providers.md:672-673`) but it's a real functional gap.
3. **Tail-cut tool-result truncation discards the part that usually matters.** For shell/build/
   test output, the error or exit status is typically at the *end* — `capMemToolResult` keeps the
   head and cuts the tail, the opposite of what's useful.
4. **Single, coarse Bedrock cache breakpoint.** Only the (small) system block is cache-pointed;
   the conversation/tool-history prefix — the part that actually grows — never is. Cache hits
   degrade toward zero on any session with substantial tool-call history.
5. **Coarse, whole-tool-name permission granularity** (`permissions.go`) — no per-command or
   per-path pattern rules (e.g. `Bash(git diff:*)`).
6. **Duplicated, drifting memory-instruction prose.** The local agent's own percept rules
   (`systemPromptShared`, `local.go:922-925`) and `escalation.MemoryInstruction` are two
   independently-worded copies of the same contract in different files — only the escalation
   copy documents the `@name:` consumer-hint prefix, so the local agent has no textual instruction
   for scoping a percept to a specific agent.

---

## 2. Escalation agent (Claude CLI) — `internal/agent/claude/` + `internal/escalation/`

### Current mechanism

- `escalation.BuildStaticContext`/`BuildDynamicContext` (`builder.go:44-164`) split stable vs.
  per-turn content, gated by `ContextMode` (`First`/`Resume`/`Returning`/`Continuation`).
- Both are funneled through `appendContextFiles` (`claude.go:340-369`), which **concatenates
  static+dynamic into a single temp file** behind exactly **one** `--append-system-prompt-file`
  flag — because, per the function's own comment, the Claude CLI only honors the *last* such flag
  when more than one is given.
- On `--resume`, `--system-prompt-snapshot` replays the recorded system prompt and ignores new
  `--append-system-prompt-file` content until the next compaction. Per-turn novel content is
  instead prepended to the user prompt as a `<milk-context>` block (`WithTurnContext`,
  `claude.go:319-331`) so it survives in conversation history.
- Stale-returning logic (`dispatch.go:411-419`) downgrades `Returning` to `First` when the topic
  changed or `returning_fresh_start_local_turns` (default 8) local turns have elapsed since the
  last escalation — a sound design.
- Percepts are injected **only** inside `BuildStaticContext`; `Resume`/`Continuation` turns return
  early with just the identity block, so a percept recorded mid-way through a long escalation
  session is invisible to Claude until the session naturally re-enters `First`/`Returning` mode.

### Weaknesses / gaps / doc-code mismatches

1. **ADR-0004 documents a caching design the code doesn't implement.** The ADR states context is
   passed as "two separate `--append-system-prompt-file` flags" specifically so the byte-identical
   static block hits Claude's prompt cache independently of the dynamic content. The actual code
   merges both into one file/flag every turn (necessarily — two flags silently drop the first on
   the real CLI). The static content is a stable *prefix* inside that combined file, but it's
   rewritten into a fresh temp file every turn — a materially weaker and unverified caching
   guarantee than the ADR claims. Either the ADR needs a correction pass, or the caching strategy
   needs revisiting given the real CLI constraint.
2. **No project-instruction-file cross-check.** Claude Code itself loads the target repo's
   `CLAUDE.md` internally (invisible to milk), but milk's own injected identity/memory
   instructions are never reconciled against it — no check that milk's guidance and the repo's
   own `CLAUDE.md` don't duplicate or conflict (e.g. both could describe a memory system).
3. **No semantic compaction of milk's own context brief.** `<milk-context>` and the
   `LastLocalSummary`/`LastEscalationSummary` "bricks" are budget-truncated verbatim excerpts
   (same drop-oldest-first mechanism as §1.2), not LLM-summarized — if the primary agent did a lot
   of work before handing off and it overflows the budget (default 12000 chars), the part dropped
   is whatever's oldest, not whatever's least relevant.
4. **Percept staleness on long resumed sessions** (mechanism note above) is a real gap, not just
   a design tradeoff — worth an explicit re-injection point (e.g. on every Nth resumed turn).
5. **Duplicated self-config instruction prose** — `SelfConfigInstruction()` and a near-identical
   paragraph baked into `systemPromptShared` are two independently-maintained copies of "never
   hand-edit config.json" guidance. Same drift risk as the memory-instruction duplication in §1.

---

## 3. Background sub-agents — `spawn_background_agent` (ADR-0043/0047)

### Current mechanism

- Fully isolated context by design: a bare 3-sentence system prompt
  (`backgroundSystemPrompt`, `local.go:1504-1515`), **not** `systemPromptShared` — no
  no-hallucination warning, no memory-tool mandate, no git-commit protocol — even though the job
  still has `bash` and file-write tools.
- Zero memory/percept injection (`schemas(nil, ...)`, `mem=nil`, `local.go:1601,1612`) and zero
  conversation history (`msgs = [system, user=task]`, single-shot).
- Depth-1 fork cap; no further `spawn_background_agent`/`escalate`/`start_workflow` from inside a
  job. Timeout, token accounting, permission-deny-instead-of-hang, and TUI live-attach all check
  out cleanly against ADR-0043/0047.
- **Unbounded result hand-off**: `drainBackgroundJobs` (`dispatch.go:28-60`) splices `j.Result`
  verbatim into the next turn's prompt with no size cap — in direct contrast to the workflow
  engine's own `truncateLargeVarsWithBudget`/`summarizeLongOutput`, which caps every hand-off at
  ~8-12K chars.

### Weaknesses / gaps

1. **No result-size guard on the hand-off path.** The one place milk demonstrably knows how to do
   this (the workflow engine) wasn't applied to background jobs — a job whose whole point is
   protecting the caller's context budget can blow that budget right back open on delivery.
2. **No safety-checklist propagation.** A background job with inherited bash/git access runs with
   none of the parent's "MANDATORY — git operations" guardrails.
3. **No structured/typed result contract** — a job returns a raw string only; no machine-readable
   status/files-touched/confidence fields.
4. **No mid-flight cancellation signal exposed to the calling model** — `/bg stop` exists for the
   human user, but the spawning agent itself has no tool to cancel a job it no longer needs.

---

## 4. Native `/workflow` engine — `internal/workflow/interp`

### Current mechanism

- Each role gets a fresh, isolated scratch session — no memory percepts, no dynamic/static
  session-orientation context, no primary/escalation framing (`workflow_runner.go:164-170`,
  `local.go:997-1002` uses the bare `systemPromptWorkflow`). Cross-role hand-off is *only* via
  explicit `save_as` template vars in the workflow YAML — a deliberate reproducibility choice, not
  an oversight.
- Real context-size discipline: per-variable truncation, whole-prompt truncation (30K char cap),
  and output summarization before saving — genuinely more careful than the background-agent path.
- Loop-detection is correctly role-scoped: streak/n-gram/text-loop trackers are all skipped for
  `workflowRole`; duplicate-tool-call detection is **not** skipped — matches CLAUDE.md exactly.

### Weaknesses / gaps

1. **Iteration-exhaustion and a genuine stuck loop are indistinguishable to the interpreter.**
   Non-workflow roles get a forced "wrap up with a real summary" nudge at `maxIter-1`
   (`local.go:1303-1310`); this is explicitly skipped for `workflowRole`. A generator that simply
   needed one more tool call than its iteration budget falls into the same mechanical
   `summarizeToolTrail` dump that a genuinely stuck loop produces, and `isTerminatedTurn`
   (`interp.go:451-461`) treats both identically as `"break"` — advance the workflow with partial
   output. A budget-exhausted step is a different failure mode from a stuck-loop step and should
   be surfaced differently (e.g. retried with a larger budget, or flagged to the user), not
   silently marked done.
2. **No memory/percept access for any workflow role, and no per-workflow opt-in.** If the user
   recorded a durable preference via `record_memory`, no workflow run ever sees it. A reasonable
   isolation default, but currently undocumented as a limitation.

---

## 5. Shared session/history plumbing

- **History-contract smell is real and live**, matching prior project memory
  (`project_history_contract.md`): `runPrimaryWithSession`/`runEscalationWithSession` both run
  `Execute` *before* `sess.AddTurn(...RoleUser...)` (`dispatch.go:244-253,470-479`). Consequence
  found in this pass: the `turnsAgo` arithmetic in both `escalation.BuildDynamicContext`
  (`builder.go:135-139`) and `runner.go:271-280` has to manually `+1` to compensate — a
  correctness-by-convention pattern with nothing structurally enforcing it, so a future call site
  can silently miscompute if it forgets the `+1`.
- One already-fixed swapped-argument bug is visible only via a code comment
  (`dispatch.go:264-268`, primary-role `AddTokensFull` cache-read/creation args) — the escalation
  call site (`dispatch.go:491`) and other `AddTokensFull` calls were not re-audited with the same
  rigor in this pass; worth a follow-up grep.

## 6. Stale memory record found during this review

`project_aider_tool_file_context.md` (existing auto-memory) claims aider tool-calls get no file
context. This is now **stale**: `internal/agent/aider/args.go:80-82` conditions `--no-git` on
`!inGitRepo()` (not unconditional), and `FirstArgs` (`args.go:113-126`) now adds `--file` for
every real file path regex-extracted from the prompt text, plus `--read` for milk's generated
context temp files. Recommend updating/removing that memory entry. Residual limitation: file
discovery still depends on the calling agent naming exact paths in its prompt text — no
repo-map-driven discovery, and no MCP wiring for smolagents (that one's an explicitly-deferred
non-bug per `docs/tooling.md:207`).

---

## 7. Cross-project comparison

| Capability | milk | Claude Code | MiMo-Code | OpenCode |
|---|---|---|---|---|
| Project instructions (AGENTS.md/CLAUDE.md) | **None** | Yes (native) | Yes — ancestor search, AGENTS.md→CLAUDE.md fallback, global `~/.claude/CLAUDE.md` interop, per-subdirectory incremental discovery | Yes — same ancestor-search + CLAUDE.md interop, plus monorepo-aware incremental discovery on file Read |
| Context compaction | Drop-oldest-first, char budget only | LLM-summarized `/compact` | LLM-summarized, triggered at 90% of context window; file manifest + tail preservation (40K tok) + per-tool-result shrink (8K tok cap); separate lighter "prune" pass | LLM-summarized, triggered on overflow; tail-budget preservation + head serialization; separate non-LLM prune pass for old tool outputs |
| Large tool-output truncation | Tail-cut (keeps head, drops tail) | Head-anchored with re-read hint | Head/tail-anchored, full text persisted to disk (7-day retention), model told to delegate re-reading to a subagent | Same pattern — persisted to disk, model told to delegate to a subagent if available, else Grep/Read with offset |
| Sub-agent context isolation | Background jobs: fully isolated (good); workflows: fully isolated (good) | Task tool: fresh isolated context per subagent | `actor` tool: `"none"` (fresh, default) or `"full"` (frozen parent snapshot incl. system+history, used only for peer/checkpoint forks) | Task tool: fresh isolated context, `subagent_depth` config (default 1) |
| Sub-agent result hand-off | Background: **unbounded** raw string; Workflow: budgeted/summarized | Summarized transcript | Wrapped `<actor_result>` text only, no full transcript | Wrapped `<task_result>`, last text part only |
| Duplicate/repeated tool-call handling | Nudge (not termination) | N/A (no built-in analog surfaced) | Hard **permission-ask / fail-closed** at 3rd identical repeat (`doom_loop`) — distinct from milk's self-recovering detectors | N/A found |
| Prompt caching breakpoints | 1 (system only), Bedrock-only, experimental | Native, provider-managed | Up to 3 of 4 Anthropic breakpoints: static system prefix + rolling double-buffer over last 2 messages (survives edits/retries) | Same double-buffer strategy; also sets `promptCacheKey`=session ID for non-Anthropic providers |
| Permission granularity | Whole-tool-name allow/deny | Pattern-scoped (`Bash(git diff:*)`) | N/A analyzed in depth | Rule-based per-pattern, default-allow with carve-outs, `deny`/`ask`/`allow`/`always` |
| Durable cross-session memory | Percept store + NREM consolidation | `CLAUDE.md` + user memory files | Checkpoint/distillation (`dream`/`distill` subagents) writing `MEMORY.md`-style files, FTS-indexed | Todo tracking only (no long-term memory system found) |

---

## 8. Prioritized recommendations

Ranked roughly by impact ÷ effort, highest first.

1. **Add project-instruction-file loading** (AGENTS.md, falling back to/interop with CLAUDE.md)
   for the primary agent's system prompt, and cross-reference the same file when building the
   escalation agent's static context. This is the single largest capability gap versus every
   reference harness and is comparatively cheap to implement: ancestor-directory search + inject
   as a static, cacheable prompt block.
2. **Fix tool-result truncation to be tail-aware (or head+tail-aware) with a re-fetch hint**,
   matching `capMemToolResult`'s replacement pattern in MiMo-Code/OpenCode: persist the full
   output somewhere retrievable, show a preview that includes the *end* of the output, and tell
   the model how to get more (offset re-read or delegate to a background agent).
3. **Bound the background-job result hand-off** using the same budget/summarization logic the
   workflow engine already has (`truncateLargeVarsWithBudget`/`summarizeLongOutput`) — this is
   reusing existing code, not new design.
4. **Add a real LLM-driven compaction path** as a fallback when the char-budget trim would
   otherwise drop turns — even a minimal version (summarize the oldest N turns into one message
   before dropping them) closes most of the gap with MiMo-Code/OpenCode's compaction agents.
5. **Reconcile ADR-0004 with the actual single-flag `--append-system-prompt-file` behavior** —
   either correct the ADR's caching claim or invest in verifying whether the merged-file approach
   still yields a usable cache hit in practice (log cache-read token deltas across turns with a
   large static prefix to check).
6. **Consolidate the duplicated memory-instruction and self-config-instruction prose** into one
   source of truth consumed by both the local-agent system prompt and the escalation builder —
   currently two independently-maintained copies of each, a drift risk already partially
   manifested (the `@name:` consumer-hint prefix is documented in only one of the two).
7. **Re-inject percepts on resumed/continuation escalation turns**, not only on `First`/
   `Returning` — even a periodic re-injection (every N resumed turns) would close the staleness
   window.
8. **Extend permission granularity to pattern-scoped rules** (per-command, per-path) instead of
   whole-tool-name allow/deny, especially for `bash`/`git` given the git-commit protocol the
   system prompt already tries to enforce through instruction alone.
9. **Consider a hard "doom-loop" gate** for verbatim-identical repeated tool calls (3rd identical
   repeat → permission-ask or fail-closed for non-interactive contexts), as a complement to —
   not replacement for — the existing self-recovering nudge detectors, matching MiMo-Code's
   layered approach (self-recovering text/reasoning loops vs. a hard gate specifically for
   identical tool-call repetition).
10. **Distinguish "iteration budget exhausted" from "stuck loop" in the workflow interpreter** —
    give budget-exhausted steps a distinct outcome from loop-terminated steps so a workflow
    doesn't silently mark a still-productive step as done.
11. **Multi-breakpoint prompt caching for Bedrock** — extend beyond the single system-block
    `cachePoint` to a rolling breakpoint over the growing tool-call history, using the
    double-buffer pattern both reference projects converged on independently.
12. Lower priority / worth tracking: structured (typed) results for background jobs and
    sub-agents instead of raw strings; a model-facing cancellation tool for background jobs;
    optional per-workflow memory/percept opt-in.

## 9. Distinctive techniques worth studying further (not immediate recommendations)

Follow-up (2026-09-30): all four items below were revisited. Two were implemented (scaled down
to fit milk's existing safety posture); two were researched and are written up here as decisions
for you to make, not implemented — both are genuinely new product features/capabilities, not
bug fixes, and exceed a "low-risk" bar on their own.

### 9.1 MiMo-Code's checkpoint/distillation memory vs. milk's Percept/NREM — researched, not implemented

Read `agent/prompt/dream.txt` and `session/checkpoint.ts` directly (`~/altworkspace/MiMo-Code`).
These are fundamentally different in kind from milk's memory system, not just in degree:

| | milk's Percept/NREM | MiMo-Code's `dream` |
|---|---|---|
| Trigger | Automatic, every session end | Manual, user-invoked slash command ("the user intentionally started it and is watching") |
| Mechanism | Arithmetic decay/promote/prune over percept weights — no LLM call | A full LLM agent turn with bash/SQLite access |
| Source material | Only facts the model proactively tagged (`<milk:percept:>`/`record_memory`) *during* the session | Retroactively mines raw trajectory (SQL over the full conversation + subagent history) for facts the model never flagged |
| Verification | None — a percept is whatever string was tagged | Checks candidate facts against actual code (`glob`/`grep` for mentioned paths/functions), marks unverifiable claims `[unverified]` |
| Output shape | Atomic fact strings with weight/producer/consumer metadata | Structured narrative Markdown (`MEMORY.md` with `## Rules`/`## Architecture decisions`/`## Patterns`/`## Gotchas` sections, size-capped, deduplicated, source-session-cited) |

The practical gap this exposes: if the model *forgets* to call `record_memory` for something
genuinely important, milk has no fallback — that fact is simply never captured, no matter how
consequential. MiMo-Code's `dream` exists specifically to catch what a real-time tagging
discipline misses, by reviewing raw history after the fact.

**Why not implemented now:** a real equivalent means a new, fairly substantial capability — a
dedicated review pass over raw session history (turns the model never flagged), likely its own
prompt/subagent, a decision on whether it writes new percepts through the existing
`record_memory` pipeline or a separate narrative file, and a decision on trigger (automatic at
session end alongside `Consolidate()`, or a manual `/memory review`-style command). None of that
is a small, additive tweak the way the four items below turned out to be — it's a new feature
that changes what "memory" means for milk. Recommendation if you want to pursue it: start with
manual/opt-in (mirrors `dream`'s own "user is watching" design), reusing `record_memory`'s
existing consumer/producer/weight fields rather than inventing a parallel narrative-file system,
so it plugs into the memory panel and NREM consolidation that already exist instead of running
alongside them as a second, disconnected memory store.

### 9.2 MiMo-Code's "max mode" ensemble — researched, not implemented

Confirmed via `session/max-mode.ts`: the built-in `max` agent runs `DEFAULT_CANDIDATES = 5`
parallel "propose-only" candidate streams for a turn, then a separate judge pass picks the best.
A genuine self-consistency/ensemble technique, with no milk analog.

**Why not implemented:** this is a cost/latency multiplier (5x the inference calls for one
turn, plus a judge call) with real product implications — when does it trigger, who pays for it,
how is the judge selected, does it apply to local models (where 5x calls to a small/cheap model
might still be worth it) or only high-stakes escalation turns (where 5x calls to Claude is real
money). None of that is answerable from the code alone; it's a scope decision for you. Flagging
it here rather than guessing at a default. If it's interesting, the natural entry point would be
scoped narrowly — e.g. an explicit `/escalate --ensemble` or a config flag on the escalation agent
only, never the default path.

### 9.3 OpenCode/MiMo-Code's frozen `ForkContext` — implemented (scaled down)

Read `actor/spawn.ts` directly. `ForkContext` snapshots the parent's exact system prompt, full
message history, permission ruleset, and tool schema at spawn time, for "full context" peer
forks (used narrowly — checkpoint-writer and peer-agent replace, not ordinary subagents) — this
preserves prompt-cache parity between parent and fork on infrastructure where that's affordable
(the same provider/model reusing its own cache).

Implemented as `spawn_background_agent`'s new `full_context` parameter
(`feat(local): optional full_context for spawn_background_agent`), scaled down deliberately: a
job requesting it gets the spawning agent's already-capped `sess.LastLocalSummary`, not a raw
message-array snapshot. A raw snapshot doesn't get the same cache-parity benefit in milk's
world (a fresh background job is frequently a different model/process with no shared cache to
preserve) and would reintroduce exactly the unbounded-cost risk the rest of this review flagged
elsewhere (§8 rec #3). Default behavior (parameter omitted) is unchanged — still fully isolated.

### 9.4 Cache-prefix stability as an explicit invariant — implemented

Checked whether milk's system-prompt tiering/role-awareness ever changes the *shape* of the
system-message array turn-to-turn in a way that would defeat Bedrock's explicit caching. It did:
`convertMessagesToConverse` flattens every system-role message into its own `system[]` entry, in
order — `system[0]` is always the large, stable `buildSystemPrompt` output, and any
percepts/current-need orientation land at `system[1:]`, small and turn-to-turn-varying. The
original `appendSystemCachePoint` placed the single cachePoint at the *end* of that array,
bundling the stable prompt and the volatile entries into one cached unit — invalidating the
whole thing, including the expensive-to-reprocess system prompt, on every percept/need change.

Fixed (`fix(bedrock): anchor the system cachePoint after the stable prefix, not the end`):
the cachePoint now sits right after `system[0]`, so the actually-stable part caches independently
of whatever dynamic content follows it. No change in breakpoint budget.

---

## 10. Follow-up (2026-10-09): thresholds and between-turn context vs. OpenCode / MiMo-Code / Claude Code

Triggered by a long escalation turn in which milk's own loop recovery erased ~80K tokens of reads
(fixed separately: crop only the looping iterations), after which the model re-read everything.
Comparison of the other harnesses, and what milk did about each difference:

| | OpenCode | MiMo-Code | Claude Code | milk before → now |
|---|---|---|---|---|
| One tool result | 50 KB / 2000 lines, not window-scaled (`tool/truncate.ts`) | same (`tool/preview.ts`); bash 30K tokens | bash 30K chars (ceiling 150K), MCP 25K tokens | 20 KB flat → ~10% of window, 4–50 KB; marker says how to read the rest |
| Overflow trigger | window − min(20K, output) (`session/overflow.ts`) | 0.9 × window (`flag.ts`) | near the limit, undocumented | 900 KB request size → 85% of (window − output reserve), from provider `prompt_tokens` |
| Request-size cap | none | none | none | 900 KB fixed → follows the window above that (safety net only) |
| Old tool output | kept; pruned past the newest 40K tokens (skips the last 2 user turns) to a placeholder | same, plus file manifest after compaction | kept; older outputs cleared first | **all dropped at turn end** → trail stored and replayed the same way |
| Loop recovery | doom-loop asks, deletes nothing | crop bounded to the identical run | n/a | 64-message tail crop → only the repeating iterations |

Model limits come from models.dev in OpenCode and MiMo-Code; milk now also reads the catalog's
output limit, and resolves providers that disagree about a model by majority instead of map order.
Spilling over-cap output to a file (OpenCode / MiMo-Code do) is done too: `~/.milk/tool-output/`, 7-day
sweep, not for `read_file`. Not done: cache-cold gating of pruning (MiMo-Code's soft trim) — milk's
replay boundary moves once per turn, which costs one partial prefix-cache miss at that point.
