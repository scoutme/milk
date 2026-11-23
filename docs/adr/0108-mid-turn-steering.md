# ADR-0108: Mid-turn steering — queued input reaches the running turn

- **Status:** Accepted 2026-11-23
- **Context:** issue #441 — while a turn runs (most acutely during a long reasoning stretch), the only ways to add an element were Ctrl+C and re-send, or queue for the *next* turn; Claude Code's steering (type while it works, the agent picks it up mid-task) was the inspiration
- **Amends:** [ADR-0083](0083-busy-input-disposition.md) — the queue's *delivery timing* (capture, review surface and escape hatch are unchanged)

## Context

ADR-0083 built the busy-input queue losslessly but lazily: Enter-while-busy
captured the text and `handleAgentDone` flushed it as the **next** turn, one
per turn; Telegram input captured while busy sat in `pendingRemoteInputs`
until the turn ended; ACP prompts serialized behind `turnMu`. A user watching
their own carefully tuned prompt arrive mid-thought had no way to hand the
running turn a correction — cancel, retype, resend, lose the work in between.

Two realities shaped the design:

1. **A running HTTP stream cannot be appended to.** While the model is
   reasoning or answering, the request is in flight — nothing can be
   injected. The earliest a running turn can see new input is the next
   request boundary: after a tool round executes, or — for a tool-less
   response — when the stream ends.
2. **Not every host has a boundary.** milk's local provider (OpenAI-compat,
   Bedrock, Responses — one shared `runToolLoop`) builds its next request
   itself and can carry input into it. Subprocess agents (Claude CLI, aider,
   smolagent) are opaque processes — there is no channel to inject into.

## Decision

**Capture is unchanged; delivery moves to the boundary.** One queue, two
drain points, promotion as the universal fallback.

1. **The seam.** `local.Agent.WithSteerDrain(fn)` (wired through
   `localAgentWiring.steerDrain`, filled per turn by `buildTUIAgents` over
   `interactiveState`) exposes the host's queues to the tool loop. The loop
   pops at exactly two moments: the **top of every iteration** (so input
   queued during a tool round rides the request that follows the tool
   results) and the **no-tool-calls termination branch** (so input queued
   during a tool-less stream continues the *same turn*: the answer-so-far is
   persisted as an assistant message, the input appends as a user message,
   the loop goes around again). This is the "as if it were a tool result"
   shape from the issue — a plain **user** message in the next round's
   payload, which is how steering reads to the model.
2. **Never where the host doesn't own the queue.** Background clones drop
   `steerDrain` (`cloneForBackground`'s explicit field list; jobs have their
   own `bgInbox` follow-up channel, issue #250), and `buildToolRunner`
   deliberately does not select the field into agent-as-tool runners — a
   nested tool-agent call must never consume the session's queue. CLI-backed
   agents get no drain: their queue keeps flushing one per next turn (the
   honest degradation — no injection channel exists).
3. **Steerable = what the flush path would send as a plain prompt.**
   Steering bypasses `submitInput` entirely, so an entry may steer only when
   `submitInput` would dispatch it as prompt text: no staged attachments (the
   flush must attach them), no slash command (the flush must execute it), no
   bang direct-exec, no direct-bash shell text (confirmation/PTY), and paste
   provenance honored exactly like `submitInput`'s issue-#151 gate (a pasted
   slash-looking line is inert text and steers). The drain takes each
   queue's **FIFO steerable prefix and stops at the first blocked entry** —
   order cannot skip — and remote (Telegram) input drains only once the
   prompt queue is empty, preserving the flush chain's global order
   (queued prompts before remote inputs).
4. **The echo is the flush's echo.** Each drained item is rendered exactly
   as its flush would have — `[queued]`/`[telegram]` label + tokenized text,
   loop-detector user-turn feed, histories recorded once (queue-time
   recording, issue #368, owns queued entries; remote input records at
   delivery). The pop runs on the agent goroutine, so the echo rides the tea
   queue into Update, and both slices are guarded by a new `steerMu` —
   agent-goroutine pop vs UI-goroutine queue/drop/review.
5. **Promotion is the fallback, not a second mechanism.** Whatever cannot
   steer — or was queued before the turn ended (interrupt, deadline,
   provider error) — flushes one per next turn through `handleAgentDone`
   exactly as ADR-0083 described. The iteration budget guards the
   termination drain: past `maxIter` nothing is popped, so no entry is ever
   consumed by a continuation that cannot run. `queue_while_busy: false`,
   the two-step Enter confirm, Ctrl+C undo and `/queue` are untouched.
6. **Observability:** every delivered batch counts as
   `milk.steer.delivered` (count, model, agent role).

## Consequences

- The busy hint and the queue receipt now say "the agent picks it up on its
  next step" instead of "for the next turn" (the phrase ADR-0083 quoted);
  `/queue` help reads "queued while the agent works". The ADR-0083 body
  keeps its original quotes with an amendment note.
- Telegram input captured while busy can now act on the *live* turn — it
  previously waited for idle. ACP parity (buffer instead of serializing
  behind `turnMu`) is the follow-up half of issue #441 and is not in this
  decision's shipped scope.
- Queued input reaches the model even mid-reasoning — but only at the next
  boundary; the transcript shows the delivery line where the model's next
  round begins, which is the honest depiction of what happened.
- A steered message is not re-routed through command handling, PTY or
  attachment staging by construction; anything needing those paths can never
  be mis-delivered as model text.

## Alternatives rejected

- **Rewriting the in-flight request** — impossible over one HTTP stream;
  providers expose no append.
- **Waiting for turn end (status quo)** — the cancel-and-resend problem the
  issue exists to remove.
- **Delivering at queue time into the transcript only** — an echo with no
  model delivery is the invisible-prompt bug ADR-0083 already forbade.
- **A steering gesture separate from Enter** — a third busy disposition to
  learn; the queue already exists and already confirms explicitly, so its
  delivery timing is the only thing that had to change.
