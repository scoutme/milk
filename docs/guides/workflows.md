# Routing, sessions & workflows

How a prompt gets routed between agents, what happens across a turn, and milk's native multi-agent pipeline engine.

**The only rule milk enforces**: the escalation agent should be smarter (and typically pricier) than the primary agent; the primary agent should be cheaper than the escalation agent. Beyond that, there is no preferred or default backend for either role — every backend, including `claude-cli`, can serve either role (`claude-cli` as primary works but isn't a smart everyday choice — see [docs/guides/providers.md](providers.md#primary-agent)). See [docs/guides/providers.md](providers.md) for the full backend catalog, including an example of the same provider used at two model tiers for exactly this pairing.

---

## Routing

Decision order per turn:

0. **`!` direct-execution mode** — pressing `!` as the first character of an empty textarea enters bang mode (Claude Code-style): the `!` is consumed rather than inserted, the prompt symbol switches live to a red `!`, and the rest of the line is typed as plain text. Submitting runs it immediately as a shell command, no confirmation, always taking priority over everything below — including while an agent turn is already in progress: it runs directly via the same embedded-PTY path rather than being funneled into the busy-input disposition (that disposition — ADR-0083 — is for everything else typed while busy: Enter queues it for the live agent's next turn, Ctrl+Enter spawns a background agent; bang commands are neither — the agent has no special handling for milk's own `!` convention, so handing it the raw string would be meaningless). Output is appended to the transcript immediately and, if the agent turn was already running, queued to be included in its next turn's context.
0. **Direct-bash shortcut** — when `direct_bash: true` and the input matches the shell-command heuristic, milk prompts `y/N` before running it locally via `sh -c` inside an embedded PTY pane. Skips confirmation if the first token is in `direct_bash_allow`. Fires after slash-command handling but before agent routing. On Windows, where no real PTY is available, milk instead pipes the child's stdout/stderr and streams them into the transcript live (preferring `bash`/`sh` on PATH — Git Bash, MSYS2, WSL interop — falling back to `cmd /C`); the child gets no stdin, so interactive prompts (editors, y/n confirmations) won't work there. WSL itself is unaffected and keeps using the real embedded-PTY path.
1. **Explicit flags** — `--escalate` forces the escalation agent; `--primary` forces the primary agent. Always wins.
2. **Session state** — if `ESCALATION_WAITING`, bypass the router entirely and send directly to the escalation agent via `--resume`.
3. **Confirmation prompts** — a short "proceed / go ahead / yes / do it" prompt carries no routing signals of its own, so it is routed on the **confirmed content** instead (see [Confirmation prompts](#confirmation-prompts-proceed--go-ahead)): the scope the prompt itself adds, the agent's new-scope question, or the pending task the confirmation stands for.
4. **Rules layer** — a layered scorer:
   - Hard rules: prompt length above `escalate_above_tokens` → escalation; keyword match → escalation.
   - Short-prompt shortcut: at or below `local_below_tokens` → conclusive local.
   - Weighted signal scorer: local verbs, escalate verbs, path references, code blocks, and open-question prefixes each contribute a signed score; conclusive once the score reaches `escalate_threshold` or `local_threshold`.
5. **Primary model classification** — when the scorer is inconclusive, the primary model itself is asked to classify (`route: local | escalate`); behavior configurable via `classifier_fallback`.
6. **Default** — attempt the primary agent; escalate if it calls `escalate(reason)`.

### Direct-bash configuration

```json
{ "direct_bash": true, "direct_bash_allow": ["ls", "git", "docker"] }
```

The heuristic is conservative: the first token must be a known binary name; natural-language openers (`what`, `how`, `explain`, …) always fall through to routing; prompts ending in `?` are always questions; long non-flag, non-path tokens trigger rejection. The classifier reuses the primary agent's own model instance — no second inference server.

### Customizing the `rules` block

All fields have built-in defaults (English + Italian keyword/verb lists); override only what you need. Lists are **fully replaced**, not merged — copy the default set and extend it if you add languages.

Real example (from a live config) adding bilingual open-question prefixes:

```json
"rules": {
  "escalate_above_tokens": 2000,
  "local_below_tokens": 30,
  "escalate_threshold": 6,
  "local_threshold": -4,
  "local_verb_weight": -3,
  "escalate_verb_weight": 4,
  "path_ref_weight": -2,
  "code_block_weight": -2,
  "open_question_weight": 3,
  "classifier_fallback": "local",
  "escalate_keywords": [
    "architect", "refactor entire", "design", "explain why",
    "analyze", "describe", "summarize", "context brick"
  ],
  "local_verbs": ["grep", "find", "list", "run", "read", "fix", "debug", "show", "cat", "ls", "check", "print", "count", "search"],
  "escalate_verbs": ["architect", "design", "refactor entire", "explain why", "compare", "evaluate", "plan", "propose", "summarize", "review"],
  "open_question_prefixes": [
    "what", "why", "how", "when", "where", "who", "which",
    "could you", "can you", "would you", "should", "is it", "are there", "do you", "does",
    "cosa", "come", "perché", "quando", "dove", "chi", "quale", "quali",
    "potresti", "puoi", "dovresti", "è possibile", "ci sono", "sai"
  ]
}
```

| Field | Type | Default | Description |
|---|---|---|---|
| `escalate_above_tokens` | int | 2000 | Prompt exceeding this approximate token count is unconditionally escalated |
| `local_below_tokens` | int | 30 | Prompt at or below this token count is unconditionally kept local |
| `escalate_keywords` | []string | see spec | Substring matches that unconditionally (hard) escalate. Keep short and specific — use `escalate_verbs` for soft signals |
| `escalate_threshold` | int | 6 | Soft score ≥ this → conclusive escalation |
| `local_threshold` | int | -4 | Soft score ≤ this → conclusive local |
| `local_verb_weight` | int | -3 | Score delta per `local_verbs` match |
| `escalate_verb_weight` | int | 4 | Score delta per `escalate_verbs` match |
| `path_ref_weight` | int | -2 | Score delta when the prompt contains a path that resolves on disk |
| `code_block_weight` | int | -2 | Score delta when the prompt contains a fenced code block |
| `open_question_weight` | int | 3 | Score delta when the prompt starts with an open-question prefix |
| `classifier_fallback` | string | `"local"` | `"local"` asks the primary model to classify; `"escalation"` escalates directly when the scorer is inconclusive |
| `local_verbs` / `escalate_verbs` | []string | English + Italian | Substring-matched; one match per prompt (first hit wins) |
| `open_question_prefixes` | []string | English + Italian | Prefix-matched (word boundary) against the trimmed prompt start |
| `confirmation_phrases` | []string | English + Italian | Word-exact phrase list that marks a prompt as a confirmation (see below) |
| `confirmation_new_scope_markers` | []string | English + Italian | Words/marks that make an agent question *new scope* ("want me to also …?") rather than a continuation ask ("shall I proceed?") |

**Guidelines**: `escalate_keywords` should only contain multi-word or highly specific phrases where local routing would *always* be wrong — single common words like "design" are too broad (*"the design looks off"* is a local task). Put ambiguous terms in `escalate_verbs` instead, where they contribute a score rather than a binding decision.

### Confirmation prompts ("proceed" / "go ahead")

A bare *"proceed", "go ahead", "yes", "do it", "sounds good"* carries no routing signals — scored in isolation it hits the short-prompt shortcut and goes local, handing the **execution of the hard task it confirms** to the cheap primary (issue #263). Instead, confirmation-class prompts are routed on the **confirmed content** (ADR-0065):

1. **The prompt's own scope** wins — *"yes, do the big refactor"* routes on `do the big refactor`. A short scope with an escalate keyword/verb marker escalates (marker-aware shortcut); a short scope without one stays local (*"yes, list the files too"*). This is the over-escalation guard: new small scope is never dragged to the escalation agent just because the session is on a hard task.
2. **The agent's new-scope question** — when the last agent message proposes new scope (contains a `confirmation_new_scope_markers` hit: *"Want me to also rename the test file?"*), the bare confirmation routes on that question, with ordinary rules semantics.
3. **The pending task** — otherwise the confirmation stands for the last non-confirmation user message (chained confirmations walk back past their siblings; a tail-bearing one contributes its own scope). User words bind on their escalate markers (*"refactor the whole dispatch layer" + "proceed"* → escalation, even though the task text itself is 7 tokens).
4. **No history** → the prompt itself is routed as before (status quo).

Everything else is the ordinary rules layer applied to the resolved text, so a long confirmed task routes exactly as the same text would typed directly — and the model classifier, when the scorer is inconclusive, classifies the *resolved content*, not "proceed". `ESCALATION_WAITING`, explicit pins (`/escalate`, `/primary`, auto-sticky) and single-turn overrides are checked first and are unaffected.

Phrases match word-exact and case-insensitive (*"proceeding with the fix"* is not a confirmation), and may chain (*"yes, please go ahead and do it"*). Detection only *starts* the confirmation path; a prompt that merely opens with a lexicon word keeps its own routing.

---

## Session states

```
ROUTING              → rules + primary model decision → PRIMARY or ESCALATION
PRIMARY              → --escalate OR primary model escalate() → ESCALATION
ESCALATION           → escalation agent ends turn with a question → ESCALATION_WAITING
ESCALATION_WAITING   → next user input bypasses router → direct --resume to escalation agent
ESCALATION_WAITING   → user passes --primary → back to ROUTING
```

### Sticky and auto-sticky escalation

- **`/escalate` alone** (no inline prompt) pins every subsequent turn to the escalation agent until `/primary` or Ctrl-C — shown as `<agent> (pinned)`.
- **`/primary` alone** does the same for the primary agent.
- **`/escalate <prompt>` / `/primary <prompt>`** is a single-turn override; normal routing resumes after.
- **Auto-sticky**: when the router escalates *without* an explicit `/escalate`, milk automatically keeps subsequent turns on the escalation agent (`<agent> (sticky)`) to avoid the cold-start context penalty on every turn. A one-shot `/escalate <prompt>` turns exactly one turn into an escalation turn and never becomes auto-sticky. Cleared by `/primary` or a single-turn override. Disable globally with `"sticky_escalation": false`; explicit pinning is unaffected by this setting.

### Context handoff

When promoting from primary to escalation, milk formats the local conversation as a plain transcript and hands it to the escalation agent so it can orient itself with no separate setup step:

```
[Context from primary agent session]
User: <turn>
Assistant: <turn>
[Tool: bash] <command>
[Tool result] <output>
...
User: <final prompt that triggered escalation>
```

The transcript is evidence-preserving (ADR-0093): the primary's tool calls and results are narrated in third person (`[mimo-local called read_file(…)]` / `[result: …]`) under the same retention plan the primary replays for itself (the newest 2 tool-using turns expanded, older ones a one-line manifest) — escalation sees what was done and found, not just claims. The hand-off framing sets an owner posture ("continue as the owner; don't recap unless asked") rather than priming a reviewer. When the conversation must be trimmed to fit the budget, head+tail retention keeps the original request and first plan plus the recent turns, and the middle is summarized (or dropped with an explicit marker) — the task definition is never the first casualty. After a goal switch the *old* ask is what goes first (ADR-0094): requests before the point where the current goal was declared are dropped or summarized oldest-first, and the switch turn stays pinned.

For Claude CLI, a new session gets this across two `--append-system-prompt-file` flags to preserve Claude's prompt cache — a **static** file (identity block, per-session nonce tags, remembered percepts; byte-identical across turns) and a **dynamic** file (escalation brief, current need, last local summary). On a resumed session (`--resume`) Claude Code replays the system prompt it recorded on the session's first request (`--system-prompt-snapshot`, on by default) and ignores new system-prompt files until a compaction, so milk instead prepends whatever is new for that turn — the dynamic content, plus the static instructions when they are being re-injected — to the user message in a `<milk-context>…</milk-context>` block (dropped when identical to the previous resumed turn's block). The full static block is still passed as a file on every resume so that a compaction re-records it. Inference-server escalation agents receive the same content as the first system message.

### Returning to a stale escalation

If the topic has switched, or `returning_fresh_start_local_turns` local turns (default 8) have passed since the last escalation, milk treats the next escalation as a fresh start rather than a resume: Claude CLI drops `--resume` and re-orients from scratch (its session still holds the raw conversation). Inference-server escalation agents keep their full history on such stale returns — they have no session behind them, so milk's history is their only memory (ADR-0032 update, 2026-10-29); an explicit `/escalate fresh` still scopes history to post-escalation turns.

### Session storage

```
~/.milk/sessions/
├── index.json      # cwd → [{id, name, last_used}] sorted by last_used desc
└── <uuid>.json     # full session: history, state, escalation_session_id, escalation_nonce
```

`milk <prompt>` resumes the most recent session for the current directory; `milk --new` always creates a fresh one; `milk --session <id|prefix|name>` targets an existing session by exact id, unambiguous id prefix, or name (cwd-scoped — the same name can exist in different projects) — or, with `--new`, names the fresh session.

---

## The native `/workflow` engine

Routing and escalation handle a single agent handing off to a smarter one mid-conversation. The `/workflow` engine is different: it runs a **predefined multi-agent pipeline** with structured roles, loops, and a typed completion contract, rather than an open-ended conversation.

The motivating problem: orchestrating a multi-stage pipeline (e.g. design → implement → review, looping until the reviewer signs off) through an interactive coding agent is fragile — there's no structured handoff between stages, no typed verdict, and no reliable way to know a background stage actually finished. `/workflow` gives milk direct control over stage transitions instead.

Workflows are **data, not code**: each one is a YAML (or JSON) *definition* — a list of stages an interpreter (`internal/workflow/interp`) walks through — not a hand-written Go pipeline. Four ship built in; you can add your own the same way.

### Built-in workflows

| Name | Roles | Shape |
|---|---|---|
| `dev` | designer, generator, evaluator | Designer plans sprints → generator implements each sprint → evaluator verdicts `good_to_go` / `needs_refinement` (retry) / `sprint_done` (advance). |
| `pair` | designer, generator, evaluator | Same shape as `dev`, but the evaluator pauses after writing findings for a **user checkpoint** — you can add context or say "continue" — before it decides the verdict. A human-in-the-loop variant of `dev`. |
| `swarm` | designer, worker, evaluator, implementer | Designer splits the task into independent (or dependency-ordered) items; a worker+evaluator retry loop runs **per item, concurrently** (bounded fan-out); a final evaluator checks cross-item integration; an optional implementer pass fixes integration issues, skipped entirely if the final evaluator found none. |
| `team` | designer, worker, evaluator, coordinator, implementer | Like `swarm`, but for **loosely coupled** work over a shared board (issue #197): items are claimed (`board_claim` — first claimer wins) instead of assumed-disjoint, every stage sees the live board (`{{.board}}`), findings (`board_post`) unblock teammates mid-flight, and a **coordinator stand-up** between passes reconciles, re-queues blocked items and verdicts `all_done` / `more_work` (ADR-0071). |

All four separate ambiguity resolution from execution: the designer either asks clarifying questions up front (paused for a user reply, with the design proceeding on stated defaults if you don't answer) or proceeds straight to planning when the task is already clear.

### Usage

```
/workflow                                          # list available workflows (built in + yours)
/workflow dev <task description>                   # run; wizard collects missing agent assignments
/workflow dev <task> --designer <agent> \
              --generator <agent> --evaluator <agent>   # inline agent assignment
/workflow swarm <task> --designer <agent> --worker <agent> \
                --evaluator <agent> --implementer <agent>
/workflow status                                   # show the running (or paused) workflow
/workflow cancel                                   # stop the running workflow; /workflow resume continues it
/workflow resume                                   # resume from the last checkpoint
/workflow reconfigure                              # reassign agent roles without losing saved state
/workflow clear                                    # delete saved state for this session (type "clear" to confirm)
```

The role flags are whatever the chosen definition declares (`dev`/`pair`: `--designer`/`--generator`/`--evaluator`; `swarm`: `--designer`/`--worker`/`--evaluator`/`--implementer`; `team`: `--designer`/`--worker`/`--evaluator`/`--coordinator`/`--implementer`) — the wizard asks for any role you didn't pass inline. Each role accepts any configured agent name, or the aliases `primary`/`escalation`, resolved once at workflow start.

A workflow always runs in the background, whether you start it with `/workflow` or the model starts it with the `start_workflow` tool: the status bar shows `[» workflow <name> running · <role>]`, and you keep a free prompt — chat, run commands, spawn background agents. Only one workflow runs per session at a time, and while it runs `/workflow <name>`, `resume`, `reconfigure`, `clear` and the session-switching commands (`/new`, `/clear`, `/resume`, `/drop`) are refused with a pointer to `/workflow cancel` (the checkpoint and the completion notice belong to the session). `/workflow status` and `/workflow cancel` work even mid-turn. **Ctrl+C** cancels the agent turn in flight if there is one; with an idle prompt it arms the usual "press again to exit" confirmation (quitting cancels the workflow) rather than cancelling the workflow, so stopping only the workflow is always the explicit `/workflow cancel`. If the designer asks clarifying questions, your next Enter answers them even if a turn is running, and Ctrl+C at that point cancels the workflow. When it ends, the outcome is queued and delivered with your next prompt, so the model knows how it went.

State is checkpointed to `~/.milk/sessions/<session-id>.workflow.<id>.interp.json` after every stage. `/workflow resume` re-launches from that checkpoint. `/workflow reconfigure` re-runs the agent-assignment wizard against the saved state without touching progress, so the next `/workflow resume` continues from the same point with new agents.

The workflow progress panel (`/panel workflow` or **F4**, auto-opens when a workflow starts unless you've manually closed it — see [docs/guides/operations.md — Keyboard shortcuts](operations.md#keyboard-shortcuts)) shows current stage/iteration and verdict history — a structural summary, not the stage's actual output. Double-click the panel (ADR-0047) to watch that output live instead (for a workflow with a `parallel_group`, like `swarm`, the view carries a note: the workers run without live output, so during the fan-out it shows a `[item N] started` / `done` / `failed` line per worker instead): the main transcript swaps for the running stage's streamed text, Esc to swap back. This also fixed a real bug: stage output previously landed in the main session transcript directly, mixed in with the live conversation with no way to view it separately — it now accumulates in the workflow's own buffer, viewable only on demand. Remote oversight (Telegram) labels workflow turns `workflow:<role>` — see [docs/guides/operations.md](operations.md#remote-oversight-telegram).

### Writing a custom workflow

Drop a `.yaml` or `.json` file in `~/.milk/workflows/` — same `Definition` schema as the built-ins (`internal/workflow/definitions/*.yaml`, embedded in the binary). A file whose `name` matches a built-in replaces it; a new name registers a new workflow, listed by plain `/workflow`.

```yaml
name: my-workflow
roles: [designer, generator, evaluator]   # RunConfig.Runners must provide one agent per role
vars:                                      # seeds the template context every stage's prompt renders against
  shared_instructions: "..."
stages:
  - id: designer
    kind: agent_turn
    role: designer
    prompt: "Task: {{.task}}\n\n{{.shared_instructions}}"
    save_as: plan                          # subsequent stages see this as {{.plan}}
```

**Stage kinds**:

| Kind | Purpose | Key fields |
|---|---|---|
| `agent_turn` | Run one turn on a role's agent | `role`, `prompt` (Go template), `save_as`; optional `verdict` (map of outcome → `{action: break\|retry, warn}`, read from the turn's output for an enclosing `loop` — the map's **keys are the response words to match**, scanned from the end of the output, longest first, so a stage can prompt for its own vocabulary; the historical `good_to_go` / `needs_refinement` / `sprint_done` are still recognized when no declared key matches, and `unknown` is the no-match fallback — ADR-0071; a rule may also carry `board_status: todo|in_progress|blocked|done` to set the current fan-out item's board status when it fires — e.g. `todo` on a retry reopens the item so the retried worker can claim it again, ADR-0073), `skip_user_checkpoint_marker`/`on_answer_prompt` (treat output as clarifying questions unless a marker line is present), `empty_output_fallback: git_diff`, `run_unless_marker_in`/`run_unless_contains` (skip this stage based on a prior stage's output), `use_memory: true` (opt this stage into the same percept injection primary/escalation turns get — off by default, since workflow roles are otherwise fully isolated: a fresh scratch session per role, no session-orientation context) |
| `loop` | Repeat its `body` | Either `over`+`from` (iterate a declared collection parsed out of another stage's saved output — see below) or `max_iterations`/`max_iterations_from` (a bounded retry loop; the last body stage must declare `verdict`) |
| `user_checkpoint` | Pause for a real user reply | `prompt` (rendered from current vars), `save_as` |
| `parallel_group` | Like `loop over`, but each item's `body` runs **concurrently** — an item starts the moment *its own* `depends_on` items finish, not when a whole wave does | `over`, `from`, `max_concurrency`, `save_as` (aggregated per-item results), `skip_if_board_status` (board definitions: items in a listed status are not dispatched and keep their last result — a repeated fan-out re-runs only reopened items; ADR-0073) |
| `board_seed` | Populate the shared board from the plan document (deterministic, no agent turn; requires `board: true` — see below) | `over`, `from` |

**Per-item isolation (issue #297)**: each fan-out item gets its own clone of the role's runner adapter (`workflow.CloneableTurnRunner` — own scratch session, session ID, nonce and one-shot flags), so concurrent items never share or interleave session state; an item's passes resume within *that item's* session. Vision attachments for workflow turns ride the call's context (`local.WithImageParts`) rather than any shared agent field, so a background workflow running concurrently with a REPL turn touches no shared mutable state either. Runners that don't implement the optional clone interface keep the previous shared-instance behavior.

**Declared collections**: a designer-style stage's saved output can declare structure the interpreter parses automatically — numbered sections (`## Sprint 1`, or `## Item 2 (depends_on: 1)` for dependency-ordered `parallel_group` items) and scalar limits (`max_passes: 4` on its own line). `dev.yaml`'s `plan_instructions` var is the exact text instructing the designer agent to produce this format — reuse or adapt it in a custom definition rather than inventing a new plan syntax from scratch.

### Shared board (`board: true`)

A definition can opt into a shared, file-backed coordination board — the single source of truth
every role of the run reads and writes (ADR-0071, issue #197; `team` is the built-in consumer):

- **`board: true`** on the definition. Seeds via a `board_seed` stage (`over` + `from` parse the
  plan's `## Item N (depends_on: …)` sections into items), or start empty.
- **Read**: `{{.board}}` renders into every stage prompt at stage start (a compact status table
  plus latest findings), and `board_get` reads the live picture mid-turn.
- **Write** (tools, local-provider roles only — CLI roles get the snapshot): `board_claim`
  (compare-and-set: first claimer wins, idempotent for the same identity), `board_update`
  (`todo`/`in_progress`/`blocked`/`done` + notes; `claimed` only via claim), `board_post`
  (short cross-role findings). Claims and findings are attributed to the acting identity
  (`worker·Item-2`) threaded on the turn's context.
- **Reopening and re-runs** (ADR-0073): a `done` item cannot be claimed (not even by its owner);
  reopen it by setting it back to `todo` — a verdict rule's `board_status: todo` does it for a
  retry, and the coordinator's `board_update` does it between passes. A `parallel_group` with
  `skip_if_board_status: [done]` then re-runs only the reopened items on the next pass; the other
  items keep the result they last produced in the group's `save_as` aggregate. A claimed item
  moved on to `in_progress` can be re-claimed by the same identity (retry/resume); identical
  repeated `board_post`s from one sender are absorbed.
- **Persistence**: every mutation is written through atomically to
  `<checkpoint>.board.json` next to the workflow checkpoint — resume restores live claims;
  caps (200 findings, trimmed notes) keep long runs bounded.

All workflows — including the built-in `dev`, `pair`, `swarm`, and `team` — run through the YAML-driven interpreter. Legacy dev-format checkpoints (pre-migration) can be cleared with `/workflow clear` and restarted.
