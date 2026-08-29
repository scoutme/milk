# Workflows

How a prompt gets routed between agents, what happens across a turn, and milk's native multi-agent pipeline engine.

**The only rule milk enforces**: the escalation agent should be smarter (and typically pricier) than the primary agent; the primary agent should be cheaper than the escalation agent. Beyond that, there is no preferred or default backend for either role — every backend, including `claude-cli`, can serve either role (`claude-cli` as primary works but isn't a smart everyday choice — see [docs/providers.md](providers.md#primary-agent)). See [docs/providers.md](providers.md) for the full backend catalog, including an example of the same provider used at two model tiers for exactly this pairing.

---

## Routing

Decision order per turn:

0. **`!` direct-execution mode** — pressing `!` as the first character of an empty textarea enters bang mode (Claude Code-style): the `!` is consumed rather than inserted, the prompt symbol switches live to a red `!`, and the rest of the line is typed as plain text. Submitting runs it immediately as a shell command, no confirmation, always taking priority over everything below.
0. **Direct-bash shortcut** — when `direct_bash: true` and the input matches the shell-command heuristic, milk prompts `y/N` before running it locally via `sh -c`. Skips confirmation if the first token is in `direct_bash_allow`. Fires after slash-command handling but before agent routing.
1. **Explicit flags** — `--escalate` forces the escalation agent; `--primary` forces the primary agent. Always wins.
2. **Session state** — if `ESCALATION_WAITING`, bypass the router entirely and send directly to the escalation agent via `--resume`.
3. **Rules layer** — a layered scorer:
   - Hard rules: prompt length above `escalate_above_tokens` → escalation; keyword match → escalation.
   - Short-prompt shortcut: at or below `local_below_tokens` → conclusive local.
   - Weighted signal scorer: local verbs, escalate verbs, path references, code blocks, and open-question prefixes each contribute a signed score; conclusive once the score reaches `escalate_threshold` or `local_threshold`.
4. **Primary model classification** — when the scorer is inconclusive, the primary model itself is asked to classify (`route: local | escalate`); behavior configurable via `classifier_fallback`.
5. **Default** — attempt the primary agent; escalate if it calls `escalate(reason)`.

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

**Guidelines**: `escalate_keywords` should only contain multi-word or highly specific phrases where local routing would *always* be wrong — single common words like "design" are too broad (*"the design looks off"* is a local task). Put ambiguous terms in `escalate_verbs` instead, where they contribute a score rather than a binding decision.

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
- **Auto-sticky**: when the router escalates *without* an explicit `/escalate`, milk automatically keeps subsequent turns on the escalation agent (`<agent> (sticky)`) to avoid the cold-start context penalty on every turn. Cleared by `/primary` or a single-turn override. Disable globally with `"sticky_escalation": false`; explicit pinning is unaffected by this setting.

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

For Claude CLI, this is split across two `--append-system-prompt-file` flags to preserve Claude's prompt cache — a **static** file (identity block, per-session nonce tags, remembered percepts; byte-identical across turns) and a **dynamic** file (escalation brief, current need, last local summary; changes per turn, suppressed when unchanged). Inference-server escalation agents receive the same content as the first system message.

### Returning to a stale escalation

If the topic has switched, or `returning_fresh_start_local_turns` local turns (default 8) have passed since the last escalation, milk treats the next escalation as a fresh start rather than a resume: Claude CLI drops `--resume`, and inference-server escalation agents scope history to post-escalation turns only.

### Session storage

```
~/.milk/sessions/
├── index.json      # cwd → [{id, name, last_used}] sorted by last_used desc
└── <uuid>.json     # full session: history, state, escalation_session_id, escalation_nonce
```

`milk <prompt>` resumes the most recent session for the current directory; `milk --new` always creates a fresh one; `milk --session <name>` targets a named session (cwd-scoped — the same name can exist in different projects).

---

## The native `/workflow` engine

Routing and escalation handle a single agent handing off to a smarter one mid-conversation. The `/workflow` engine is different: it runs a **predefined multi-agent pipeline** with structured roles, loops, and a typed completion contract, rather than an open-ended conversation.

The motivating problem: orchestrating a multi-stage pipeline (e.g. design → implement → review, looping until the reviewer signs off) through an interactive coding agent is fragile — there's no structured handoff between stages, no typed verdict, and no reliable way to know a background stage actually finished. `/workflow` gives milk direct control over stage transitions instead.

Workflows are **data, not code**: each one is a YAML (or JSON) *definition* — a list of stages an interpreter (`internal/workflow/interp`) walks through — not a hand-written Go pipeline. Three ship built in; you can add your own the same way.

### Built-in workflows

| Name | Roles | Shape |
|---|---|---|
| `dev` | designer, generator, evaluator | Designer plans sprints → generator implements each sprint → evaluator verdicts `good_to_go` / `needs_refinement` (retry) / `sprint_done` (advance). |
| `pair` | designer, generator, evaluator | Same shape as `dev`, but the evaluator pauses after writing findings for a **user checkpoint** — you can add context or say "continue" — before it decides the verdict. A human-in-the-loop variant of `dev`. |
| `swarm` | designer, worker, evaluator, implementer | Designer splits the task into independent (or dependency-ordered) items; a worker+evaluator retry loop runs **per item, concurrently** (bounded fan-out); a final evaluator checks cross-item integration; an optional implementer pass fixes integration issues, skipped entirely if the final evaluator found none. |

All three separate ambiguity resolution from execution: the designer either asks clarifying questions up front (paused for a user reply, with the design proceeding on stated defaults if you don't answer) or proceeds straight to planning when the task is already clear.

### Usage

```
/workflow                                          # list available workflows (built in + yours)
/workflow dev <task description>                   # run; wizard collects missing agent assignments
/workflow dev <task> --designer <agent> \
              --generator <agent> --evaluator <agent>   # inline agent assignment
/workflow swarm <task> --designer <agent> --worker <agent> \
                --evaluator <agent> --implementer <agent>
/workflow resume                                   # resume from the last checkpoint
/workflow reconfigure                              # reassign agent roles without losing saved state
/workflow clear                                    # delete saved state for this session (type "clear" to confirm)
```

The role flags are whatever the chosen definition declares (`dev`/`pair`: `--designer`/`--generator`/`--evaluator`; `swarm`: `--designer`/`--worker`/`--evaluator`/`--implementer`) — the wizard asks for any role you didn't pass inline. Each role accepts any configured agent name, or the aliases `primary`/`escalation`, resolved once at workflow start.

State is checkpointed to `~/.milk/sessions/<session-id>.workflow.<id>.interp.json` after every stage. `/workflow resume` re-launches from that checkpoint. `/workflow reconfigure` re-runs the agent-assignment wizard against the saved state without touching progress, so the next `/workflow resume` continues from the same point with new agents.

The workflow progress panel (`/panel workflow`, auto-opens when a workflow starts) shows current stage/iteration and verdict history. Remote oversight (Telegram) labels workflow turns `workflow:<role>` — see [docs/operations.md](operations.md#remote-oversight-telegram).

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
| `agent_turn` | Run one turn on a role's agent | `role`, `prompt` (Go template), `save_as`; optional `verdict` (map of outcome → `{action: break\|retry, warn}`, read from the turn's output for an enclosing `loop`), `skip_user_checkpoint_marker`/`on_answer_prompt` (treat output as clarifying questions unless a marker line is present), `empty_output_fallback: git_diff`, `run_unless_marker_in`/`run_unless_contains` (skip this stage based on a prior stage's output) |
| `loop` | Repeat its `body` | Either `over`+`from` (iterate a declared collection parsed out of another stage's saved output — see below) or `max_iterations`/`max_iterations_from` (a bounded retry loop; the last body stage must declare `verdict`) |
| `user_checkpoint` | Pause for a real user reply | `prompt` (rendered from current vars), `save_as` |
| `parallel_group` | Like `loop over`, but each item's `body` runs **concurrently** | `over`, `from`, `max_concurrency`, `save_as` (aggregated per-item results) |

**Declared collections**: a designer-style stage's saved output can declare structure the interpreter parses automatically — numbered sections (`## Sprint 1`, or `## Item 2 (depends_on: 1)` for dependency-ordered `parallel_group` items) and scalar limits (`max_passes: 4` on its own line). `dev.yaml`'s `plan_instructions` var is the exact text instructing the designer agent to produce this format — reuse or adapt it in a custom definition rather than inventing a new plan syntax from scratch.

The hand-written Go `dev` workflow (`internal/workflow/dev`, predating this data-driven system) still exists solely to resume checkpoints saved before the migration — every fresh `/workflow` run, `dev` included, goes through the YAML-driven interpreter now.
