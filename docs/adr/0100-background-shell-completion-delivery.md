# ADR-0100: Background shell completion delivery — `await` opt-in and the failure net

**Status:** Accepted · 2026-11-05

## Context

Background shells (`bash` with `run_in_background`, [ADR-0063](0063-background-shells.md))
were pull-only for the *model*: the registry pushed lifecycle events to the UI
(toast, background panel — [ADR-0064](0064-background-shell-surface.md)), but
nothing crossed into the agent's context. Background **agent jobs** got the
full treatment ([ADR-0043](0043-background-subagents.md)): result queued,
drained into the next prompt, follow-up turn when idle.

The gap surfaced as a broken promise: an agent ending a turn with
"reporting as soon as the watcher fires" has no mechanism to be told — the
shell exits while the agent is not running, and the completion dies in a
toast the model never reads. A failure-only design (noticed earlier) covered
the dangerous cousin (a watcher failing silently while idle) but not the
case actually hit: an **awaited success** going unreported.

Constraints that shaped the design:

- **context economy** — shell completions must not inject mid-turn (noise);
  the natural delivery point is the next dispatched turn, jobs-style;
- **no pull-model regressions** — clean, un-awaited exits (git status checks,
  one-shot helpers) must stay toast/panel-only, exactly as before;
- **re-use, don't invent** — the wake rules (`decideFollowup`, pending==0
  guard, busy-defer) already exist for jobs; shells have no wave concept.

## Decision

1. **`await: true` on `bash(run_in_background)`** — the agent opts in per
   shell ("wake me when this fires"). The registry queues a completion event
   on exit, **whatever the status** — that is the promise. The spawn result
   echoes the guarantee so the agent knows it holds.
2. **The failure net** — an *un-awaited* shell also queues its completion
   when it **failed** (nonzero exit or exec error), so failures surface even
   without `await`. A **deliberate stop** (`kill_bash`, lifetime cap) is not
   failure news. One predicate owns both rules: `local.ShellWantsNotice`.
3. **Delivery is the jobs pattern** (ADR-0043's drain), in three aligned
   halves sharing the predicate:
   - **content**: events drained at dispatch next to `drainBackgroundJobs`,
     rendered as a `[shell-finished]` prompt/history prefix with a 15-line
     output tail (32-event queue, oldest dropped);
   - **visibility**: the TUI writes a transcript block for the same events
     (what the user sees is what the model gets; clean un-awaited exits stay
     toast-only per ADR-0048);
   - **wake**: awaited completions run the follow-up machinery — TUI
     `maybeAutoFollowupShells` and ACP `requestShellFollowup`, both
     `decideFollowup(false, 0, PendingShellEvents(), busy)`: no wave, a
     pending==0 skip (an intervening turn already drained the events — never
     burn an empty turn), busy defers via `pendingShellFollowup` /
     `pendingShellFollowup` until `handleAgentDone` / `flushPendingFollowup`.
4. **The event is queued by the registry itself, before listeners fire** —
   a wake decision made from a listener already sees it pending, and
   multiple attached hosts (TUI + ACP) queue once.

## Consequences

- "Report as soon as it fires" is now a **tool guarantee** (the spawn
  result says so) rather than an agent's promise, for shells carrying
  `await`.
- Clean un-awaited exits remain pull — zero change for fire-and-forget
  helpers; failures reach the next turn anyway.
- If two hosts are attached simultaneously, both may wake on the same
  awaited shell; the second finds an empty queue and its follow-up
  delivers nothing extra (the pending==0 guard) — accepted rarity.
- The queue is process-global like the shells themselves: whichever turn
  runs next delivers, matching the shell's own tool contract.
