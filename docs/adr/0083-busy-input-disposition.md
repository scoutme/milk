# 83. Busy-Input Disposition: Enter Queues, Ctrl+Enter Forks

- **Status:** accepted
- **Date:** 2026-10-26
- **Amended by:** [ADR-0108](0108-mid-turn-steering.md) — steerable queue
  entries (plain prompt text) now drain into the *running* turn at its next
  model-request boundary instead of waiting for the next turn; the
  one-per-turn flush below remains the path for everything that cannot steer
  and the fallback when the turn ends first. Receipt/hint wording changed
  with it (`⋯ queued — the agent picks it up on its next step — …`). The
  ACP half followed in #446: a busy `session/prompt` now buffers in the
  session's steer queue and is pulled by the running turn the same way, so
  the "ACP parity is a non-goal" consequence below is this record's
  original scope, superseded by the amendment.

## Context

While an agent turn runs, typed input had one escape hatch: Ctrl+Enter/Ctrl+J
a background agent (ADR-0043) — great for a self-contained side
question, wrong for conversation continuation (corrections, "also do X",
next steps), which belongs to the *live* agent's next turn. Enter was a dead
end: a hint pointing at Ctrl+Enter. The design work (tech + UI, kept in
`docs/internal/design/busy-queue-or-fork-*.md`) gave both dispositions
first-class, one-keystroke reach.

## Decision

- **Enter = queue it for this agent's next turn.** `queuePrompt` captures the
  text with its paste provenance and staged attachments (they were staged for
  that typed input) into `interactiveState.queuedPrompts` (in-memory, cap 20 —
  refused with a hint past it, text kept in the box) and prints a receipt
  naming the review surface: `⋯ queued for next turn — "<preview>" ·
  /queue to review`, with `⋯ N lines` / `+ N attachments` variants.
- **Drain: one per turn, first.** `handleAgentDone` → `drainQueuedPrompt`
  flushes the oldest entry *before* the background-followup and remote-input
  chain, delegating to `submitInput(q.text, dim("[queued]")+" ")` — the
  `[telegram]` label precedent — so history, the loop-detector feed and the
  issue-#151 paste gate replay correctly. The flushed turn **is** the next
  turn (drainBackgroundJobs rides along inside it), so both follow-up flags
  are cleared at flush and re-armed by any job that finishes later. FIFO
  order; survives Ctrl+C; dropped on session switch with a notice (like
  staged attachments).
- **Ctrl+Enter/Ctrl+J = fork**, unchanged (ADR-0043). Slash commands are
  never queued (non-busy-safe ones keep their own hint); `!` bang runs
  immediately as before.
- **Surface**: `/queue list|show <n>|drop <n|last|all>|clear` (TUI-only,
  busy-safe), a `» N queued` status-bar chip, and the flush echo's
  `[queued]` label so no turn answers an invisible prompt.
- **Escape hatch**: `queue_while_busy: false` restores Enter's hint-only
  behavior.

## Alternatives rejected (design §12)

A Ctrl+Enter fork/queue chooser (modal friction), Shift+Enter (newline
binding + terminal mangling), flipping Ctrl+Enter's meaning
(established/documented/tested), merging queued prompts into one turn,
persisting the queue, queueing slash commands.

## Consequences

- Enter-while-busy is now lossless by default: the receipt line is the proof
  the input was not dropped, `/queue drop last` is a full undo until the turn
  ends — queueing is the cheap, reversible action and owns the default key.
- ACP parity is a non-goal: `session/prompt` already queues behind the
  session's turn mutex.
- obs counters (`milk.prompt_queued`) were listed as optional in the design
  and are not in this cut.
- Historical note: ADR-0043's "Enter shows a hint" contract is amended by
  this record.

## Amendment

- **2026-10-26 (two-step confirm + Ctrl+C undo).** Queuing on the first
  Enter made the fork alternative invisible — the user saw their input
  vanish with only a receipt after the fact. Enter is now **two-step**: the
  first press announces both choices in the busy hint (`Enter again to
  queue it for the next turn · Ctrl+Enter to spawn a background agent with
  this`), the second press on the same gesture queues. Any other key
  disarms the confirm (it chains across consecutive Enters only), and a
  confirm never survives a turn boundary. **Ctrl+C gains an undo stage
  first**: with a non-empty queue it drops the *last* queued entry (one
  press per entry, notice + `/queue` pointer) before falling through to its
  usual chain (clear input → interrupt the turn → quit) — the receipt's
  counterpart, so queueing is reversible by keystroke alone.
