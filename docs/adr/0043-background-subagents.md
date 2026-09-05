# 43. Background Sub-Agents for the Local Agent's Tool Loop

Date: 2026-09-05

Status: Accepted

## Context

ADR-0034 (Agent-as-Tool) lets a localRunner primary agent call a *different*, specifically configured peer agent as a tool (`agent_<name>`). That call is synchronous and single-shot: `RunToolCall` runs exactly one inference turn with no tool loop, no memory/percept injection, and blocks the caller until it returns.

Separately, when `provider: "claude-cli"` fills either the primary or escalation role, the underlying `claude` binary already has its own native background/fork sub-agent capability (the Task/fork tool) — this is a property of Claude Code itself, not something milk implements, and it requires no work here.

The gap is `internal/agent/local`'s own tool loop — the implementation shared by whichever role (primary or escalation) is backed by an inference-server provider (local model, Bedrock, OpenRouter, etc.). That implementation has no way to:

- Delegate an open-ended research question ("read these files and summarize the level-gen code") to an independent copy of itself, with its own multi-step tool loop, without that exploration consuming the calling turn's own context window.
- Fan out several such independent questions in parallel and be notified as each completes, rather than blocking on one at a time.

This is the same shape of capability Claude Code's own fork/Task tool provides when *it* is used directly (not via milk): spawn a copy of yourself against a narrow question, let it run its own tool loop, and report back a distilled result once, asynchronously — no polling, no mid-flight peeking.

Note: the deciding factor for whether an agent needs this is **provider type**, not role. An inference-server-backed agent lacks it whether it's serving as primary or escalation; a `claude-cli`-backed agent has it natively in both roles. Earlier framing that tied this to "the escalation agent" specifically was a mistake — `escalation_agent` defaults to `claude-cli` as a convenience (see `docs/providers.md`), but any backend can serve either role, and role does not determine whether native fork capability exists.

## Decision

### 1. New tool: `spawn_background_agent`

Added to `internal/agent/local`'s built-in tool schema (`schemas()` in `tools.go`), alongside the existing `agent_<name>` synthesized tools:

```json
{
  "type": "function",
  "function": {
    "name": "spawn_background_agent",
    "description": "Fork an independent copy of yourself to research a narrow, self-contained question in the background — reading files, grepping, running commands — without using up your own context. You are notified with a summary when it completes; you do not block on it and must not fabricate a result before that notification arrives.",
    "parameters": {
      "type": "object",
      "properties": {
        "task": { "type": "string", "description": "The self-contained question or task for the background agent. Include everything it needs — it does not see your conversation." },
        "label": { "type": "string", "description": "Short human-readable label for status display, e.g. \"analyze level-gen code\"." }
      },
      "required": ["task", "label"]
    }
  }
}
```

Unlike `agent_<name>`, this tool does not target a different configured agent — it forks the *calling* agent's own config (same model, same built-in tools), the same way Claude Code's fork does.

### 2. Execution model: scoped background run

A background job runs the full built-in tool loop (`read_file`, `grep`, `find_files`, `bash`, etc. — the same set the caller has), not a single inference call like `RunToolCall`. This requires a new execution mode on `local.Agent`, distinct from both normal `Run()` (session-attached) and `RunToolCall` (single-shot, no tools):

- No session recording, no state transitions (like `RunToolCall`).
- No memory/percept injection (like `RunToolCall`).
- Unlike `RunToolCall`: runs the normal iterative tool-call loop (`executeToolCalls`) until the model produces a final answer or `maxIter` is hit.
- Uses a new "background sub-agent" system-prompt branch (alongside the existing primary/escalation branches in `buildSystemPrompt`) that frames the task as a scoped, self-contained research job with no awareness of the parent conversation.
- Tool list excludes `agent_<name>`, `spawn_background_agent`, and `escalate` — no recursive forking, no chaining to other tool-agents, no self-escalation. This mirrors ADR-0034's existing "no recursive tool-agent chaining" decision.

### 3. Background job manager

A new small manager (package/location TBD in the implementation plan) tracks in-flight jobs:

- `Spawn(ctx, task, label string) *Job` launches the scoped run in a goroutine and returns immediately with a `Job` handle (id, label, status).
- Bounded concurrency: a semaphore sized by `max_background_agents` (config, default 3) prevents runaway fan-out from a single turn or across turns.
- Depth is capped at 1: a background job's own tool list has no `spawn_background_agent`, so it cannot itself fork (see §2) — this is the fork-bomb guard.
- On completion, the job stores its result text, token usage, and end time; it does not push itself into any conversation on its own — delivery is pull-based (see §4).

### 4. Completion delivery, session, and token accounting

- The manager exposes a way to drain newly-completed jobs. The dispatch layer (`cmd/milk/dispatch.go`) drains it at the start of every turn for the owning session, before building that turn's context, and injects each completed job's result as a synthetic block (e.g. `[Background agent "<label>" completed: <result>]`) — the same place percepts are already injected, not as a fabricated user or assistant turn.
- Independently of the next turn, the manager notifies the running TUI immediately on completion (not waiting for the user's next input) so the transcript and status bar reflect it as soon as it lands — see §5.
- Token usage for a completed job is recorded via `sess.AddTokensFull(model, "<role>:subagent", ...)`, where `<role>` is whichever role (`primary` or `escalation`) the *spawning* agent held at spawn time. This generalizes the existing `escalation:subagent` / `escalation:workflow` convention (currently populated only by passively parsing Claude CLI's own `result` event — see CLAUDE.md "Token tracking") to also cover locally-spawned jobs under `primary:subagent` or `escalation:subagent`, driven by milk's own accounting rather than observed from a subprocess.

### 5. TUI surfacing

- Status bar shows a live count while jobs are running (e.g. `⚙ 2 background agents running`), analogous to existing loop-detection warnings.
- On completion, a transcript line is printed so the user sees the job land, distinct from the next turn's synthetic injection block.
- Reuses the `tea.Program.Send`-driven live-update pattern already used by the workflow engine's `ProgressMsg` (`internal/workflow/workflow.go`, rendered by `panel_workflow.go`), generalized to arbitrary background jobs rather than only `/workflow`-driven pipelines.

### 6. Scope

- Applies to any agent role backed by `internal/agent/local` (localRunner) — primary or escalation, any inference-server provider.
- Does not apply to `claude-cli` — it already has this natively, regardless of role.
- Does not apply to subprocess agents (`aider-cli`, `subprocess`/smolagent) — they don't use milk's built-in tool schema/loop at all, in either role.

## Consequences

**Positive:**
- Closes the specific gap observed when comparing to Claude Code's own fork/Task tool: an inference-server-backed agent (primary or escalation) can now delegate open-ended exploration without spending its own context budget or blocking its turn.
- Reuses established conventions rather than inventing new ones: the `role:subcategory` token accounting convention, the `tea.Program.Send` live-panel pattern, and ADR-0034's no-recursive-chaining guard all carry over directly.
- Symmetric across roles by construction — no special-casing "primary vs escalation," which was the mistake in an earlier framing of this problem.

**Negative:**
- Introduces a third execution mode on `local.Agent` (scoped background run) alongside `Run()` and `RunToolCall`, increasing the surface area to reason about when changing the tool loop.
- Bounded concurrency (`max_background_agents`) trades throughput for cost/runaway-token safety; a user running many independent research questions may still have to wait for a semaphore slot.
- Completion delivery is turn-boundary-pulled for context injection (the calling agent only "sees" the result on its next turn), which is correct for context hygiene but means a fast follow-up turn issued before the job finishes will not have the result yet — the agent must be prompted (via system prompt guidance) not to fabricate results in that window, mirroring Claude Code's own "don't race" fork discipline.
- Adds a new manager/goroutine-lifecycle component that must be cleanly shut down on session end to avoid leaking goroutines past process exit.

**Neutral:**
- Background jobs are not recorded in the session transcript as their own turns — only the distilled result appears, injected the same way percepts are, consistent with ADR-0034's decision not to record tool-agent internal reasoning in the session.
