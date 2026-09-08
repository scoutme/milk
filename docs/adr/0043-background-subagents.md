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
- Runs on an independent clone of the calling `*Agent` (`cloneForBackground`, built from an explicit field list of known-stable configuration — not `c := *a`), not the instance itself. A job runs in its own goroutine and can still be executing when the parent starts its own next turn on the same `*Agent`; several fields are mutated in place on the instance during a turn (`reasoningNgram`, `detectedFormat`, `pendingImageParts`, …), and even a shallow struct copy racily reads all of them at once against the parent's concurrent writes.

### 3. Background job manager

A new small manager (`internal/agent/local.Manager`) tracks in-flight jobs:

- `Spawn(label, task, role, model string, run func(context.Context) (string, session.TokenUsage, error)) *Job` launches the scoped run in a goroutine and returns immediately with a `Job` handle (id, label, status). `Spawn` takes no per-call context — `run` receives a context derived from the Manager's own long-lived context, supplied once at construction. A per-call context (the spawning turn's) would be wrong here: the TUI cancels each turn's context the instant that turn returns, which happens almost immediately after `Spawn` itself returns — a job needs to survive past that, since its entire purpose is to keep running across whatever later turns eventually drain it.
- Every job's context is bounded by a per-job timeout (default 20 minutes, configurable via `background_agent_timeout_minutes`) applied once it starts executing (not counting time queued for a concurrency slot). This is a hard requirement, not a nicety: without it, any job that gets stuck — for any reason — holds its concurrency slot forever, degrading `max_background_agents` for the rest of the session. A timed-out job is terminated, not retried (it's more likely stuck than making progress on a task meant to be narrow). Live-verified: one incident's root cause (below) couldn't be caught by the timeout alone, since it wasn't context-aware — both fixes were needed together.
- **Timeout raised from 10 to 20 minutes, found live**: two jobs doing genuine multi-file analysis on a slower reasoning model were still issuing new tool-loop requests 8+ minutes in when a live session hit the original 10-minute cutoff — killed mid-progress, not mid-hang. The timeout's actual job is narrower than it first sounds: an actually-*stuck* job (repeating itself, re-issuing the same tool call) is already caught by loop detection, which runs on a background job exactly as it would on a normal turn (`cloneForBackground` leaves `workflowRole` false specifically so none of the workflow-role skip conditions apply). This timeout only needs to catch a job that's genuinely still working but never finishing — so it can afford to be generous. Made configurable (`background_agent_timeout_minutes`, default 20, mirroring `max_background_agents`'s config pattern) rather than just bumping the constant, since the right value depends on the model and task size in ways a single hardcoded default can't anticipate.
- Background jobs never inherit the parent's interactive permission-ask callback (`cloneForBackground` copies the shared `permStore` but not `permAsk`). It blocks synchronously on a plain channel receive with no timeout or context-awareness — a background job hitting a permission-gated tool would show an unattributed prompt nobody knew to expect, hang forever, and permanently hold its slot. Already-granted tools still work silently via the shared store; anything else denies immediately instead of asking.
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
- **Auto-follow-up, added after live-verifying the feature**: an escalation agent routinely tells the user "I'll follow up automatically when they finish" after spawning a wave — but the per-job notification above is passive (just a transcript line) and the turn-boundary drain only runs if some *other* turn happens to be dispatched. Without an explicit trigger, that promise is never actually kept: results sit in the Manager's queue indefinitely if the user doesn't send anything further. `Manager.SetOnBatchDone` fires once when the last job of a wave finishes (distinct from the per-job hook — firing per-job would produce one redundant follow-up turn per job instead of one consolidated turn for the wave); `cmd/milk` submits a synthetic prompt through the same dispatch path a real user turn takes if idle, or defers to the next turn-completion if busy — mirroring the existing pattern for queued Telegram remote-oversight input.
- **Repeated-prompt exemption, found live**: that synthetic prompt is a fixed string, submitted verbatim every time a wave finishes. `Run`'s `isRepeatedPrompt` check exists to catch a human repeating themselves out of frustration and self-escalate on their behalf — a milk-generated string recurring across several completed waves in the same session looks identical to that pattern from history alone, and was triggering a bogus self-escalation with nothing to do with the user. `local.BackgroundFollowupPrompt` (exported specifically so `cmd/milk`'s dispatch site and this check can never drift apart) is now whitelisted inside `isRepeatedPrompt` itself, rather than at its one current call site, so any future caller inherits the exemption automatically.
- **User-initiated spawning, also added after live use**: the user, not just the agent, can spawn a background job — pressing Enter while the model is busy no longer just tells them to wait or interrupt; it arms a "press Enter again to spawn a background agent with this" hint (3s window, matching the existing busy-hint timer), and a second Enter within that window spawns one from whatever is currently typed (`Job.Role = "user"`, its own `user:subagent` token bucket — there's no agent role to tag it with). Delivery is deliberately **not** wave-gated the way agent-initiated jobs are: since the user spawned this one specific side-question rather than a coordinated multi-part research plan, its result reaches the main agent as soon as it's free, even if other jobs (agent- or user-initiated) are still running — there's nothing to consolidate it with, and holding it back would just be waiting on unrelated work.
- **Background-agents panel, added once several jobs running at once made the status-bar count alone not enough**: `/panel background` (or **F3**) shows a live list of every job (label, status, elapsed), the same shape as the pre-existing tasks/memory panels (`Manager.Jobs()` — a read-only snapshot, distinct from `Drain()`, safe to call on every render). Adding a fourth toggleable panel alongside memory/tasks/workflow is also why **F1**-**F4** exist now as global show/hide shortcuts (equivalent to `/panel <name>`, available in any mode) — three-plus panels competing for screen width makes a one-key toggle worth more than typing the command each time. **F1**-**F4** follow the same left-to-right order the panels are joined in `View()`: memory, tasks, background, workflow (not the order each panel happened to be built in — background shipped after workflow, but sits before it in the shortcut order).
- **Panel border/scrollbar consistency, found live**: the new background panel was built by mirroring the tasks panel, which (like tasks) had no scrollbar-thumb indicator to show scroll position — only the pre-existing workflow panel had one, and workflow additionally had a left border that none of the others did. With four side panels now competing for the same edge of the screen, the inconsistency was visually confusing (which is scrollable? where does one panel end and the next begin?). The fix settled on no left border anywhere — a left border doubles as a divider between panels, which is redundant when panels are already visually separated by their own right-hand scrollbar column — and instead gave every panel (memory, tasks, background, workflow) the same right-hand scrollbar-thumb treatment memory already had (`scrollThumb`, already shared, reused directly for the new `renderTasksPanelScrollbar`/`renderBackgroundPanelScrollbar`; workflow's pre-existing left border was removed and its inner width restored to the same 32 columns as the other three). `panelContentCol` (`panel_select.go`) simplified back to a pass-through, since no panel reserves any left-side columns anymore.
- **Full panel parity, requested after the scrollbar fix**: scrolling being consistent surfaced the next inconsistency — text click-drag selection only ever worked on the memory and workflow panels; tasks and background could scroll but not select/copy. Rather than special-case a third and fourth panel, the four panels' near-identical render/scrollbar/selection logic was consolidated into shared helpers (`panel_common.go`): `sidePanelLines(region)` is now the single place that maps a region to its content-builder call and inner width, `panelOffsetPtr(region)` returns a pointer straight into the model's own offset field for that region (letting wheel-scroll and render-time clamping share one read-modify-write site instead of a per-region switch in every caller), and `renderSidePanel`/`renderSidePanelScrollbar` implement the body/scrollbar once for all four. Each panel's `render<Name>Panel`/`render<Name>PanelScrollbar` are now one-line wrappers over these, and `handleMouse`'s `MouseButtonLeft` case generalized from `region == regionMemory || region == regionWorkflow` to `region != regionNone` so tasks/background reach `handlePanelMouse` exactly like the other two. The only panel-specific behavior left is content generation (`build<Name>PanelLines`) and the memory panel's own double-click-for-detail lookup (`handleMemoryPanelClick`), which stays gated to `region == regionMemory` inside the now-shared `handlePanelMouse` — the intentionally special part, not accidental drift.
- **Divider consistency, found immediately after**: tasks and background separated their title from content with a full-width `───` rule (and tasks used a second one between its SESSION/GLOBAL sections); memory and workflow never had one, just a blank line. Converged on the blank-line style everywhere — removing the rule is strictly simpler than adding one to two more panels, and a title/content divider adds no information a blank line doesn't already convey. `buildTasksPanelLines`/`buildBackgroundPanelLines` no longer define an `hr()` helper.

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
