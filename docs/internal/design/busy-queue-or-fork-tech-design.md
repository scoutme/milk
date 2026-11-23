# Busy-input disposition: queue for the live agent or fork to a background agent — tech design

> **Status: DESIGN PROPOSAL** (pre-ADR, pre-implementation). Companion doc:
> [busy-queue-or-fork-ui-design.md](busy-queue-or-fork-ui-design.md) (the
> UI-facing design). On implementation this becomes **ADR-0082** plus a plan in
> `docs/internal/plans/`.
>
> This doc answers: *when a turn is running and the user types a prompt, how do
> we give it two first-class destinations — queue it as the live agent's next
> turn, or fork it to a background agent (Ctrl+Enter) — and how does the queue
> live safely alongside the existing background-job follow-up and remote-input
> drain paths?*

## 1. Problem

Today, text typed while an agent turn is in progress has exactly one escape
hatch (ADR-0043):

- **Ctrl+Enter / Ctrl+J** — `handleBusyKey` (`cmd/milk/repl.go:1338`) calls
  `startUserBackgroundAgent`, spawning a background research job from the
  typed text (`Job.Role = "user"`).
- **Enter** — a dead end: `m.busyHint = "agent is working — Ctrl+Enter to
  spawn a background agent with this"` and the text stays in the textarea.

Forking is the right disposition for a *self-contained side question*, but a
large share of mid-turn input is conversation-continuation: a correction
("actually don't touch X"), an "also do Y", a next step the user thought of
while watching. Those want the **live agent's next turn** — same session, same
context — not an isolated fork. The precedent already exists in the codebase
for a non-keyboard surface: remote (Telegram) input is queued while busy in
`interactiveState.pendingRemoteInputs` and drained one per turn by
`handleAgentDone` (`cmd/milk/repl.go:1653`). The TUI is the only surface where
typed-while-busy input has **no** queue path. (ACP `session/prompt` already
queues behind `turnMu` — see §11.)

## 2. Decision summary

While `m.busy`:

| Input | Disposition |
|---|---|
| **Enter** (text in the buffer) | **Queue** for the live agent: FIFO, submitted as its own turn the moment the current turn ends |
| **Ctrl+Enter / Ctrl+J** (text in the buffer) | **Fork**: spawn a background agent (unchanged, `spawnUserBackgroundAgent`) |
| Enter (empty buffer) | unchanged hint (`agent is responding — Ctrl+C to interrupt`) |
| Enter-as-accept (tab completion/hint active) | unchanged — accepts the completion, never queues (checked first, as today) |
| `!` bang, busy-safe slash commands | unchanged — execute immediately, never queue |
| non-busy-safe slash commands | unchanged — refused with a hint; **commands are never queued** (delayed command execution without the user at the screen is surprising) |

Both dispositions coexist: a user can queue three follow-ups and fork one side
question during the same turn; the queue drains into the live conversation
while the fork's result arrives via the existing background-job delivery path.

## 3. State model

New entry type and queue, in `cmd/milk` (TUI model layer; nothing in
`internal/` changes):

```go
// queuedPrompt is one prompt typed while a turn was running, waiting to become
// the live agent's next turn. The queue side of busy-input disposition;
// Ctrl+Enter's background fork (spawnUserBackgroundAgent) is the other.
type queuedPrompt struct {
    text          string
    leadingPasted bool                // issue #151 paste-provenance gate, replayed at flush
    attachments   []PendingAttachment // captured at queue time (attachments.go)
    queuedAt      time.Time
}
```

- Storage: `interactiveState.queuedPrompts []queuedPrompt`, next to
  `pendingRemoteInputs` (`cmd/milk/interactive.go:287`) — same owner, same
  lifecycle family. In-memory only: **not persisted** to session JSON (like
  `pendingRemoteInputs`; a stale prompt replayed after a restart is more
  surprising than losing it).
- Cap: `queuedPromptMax = 20` (the permission `permQueueMax = 5` precedent,
  `cmd/milk/repl.go:1548`, scaled for free text). At cap the prompt is
  **refused, not dropped silently** — busy hint `queue is full (20) — /queue
  to manage`, text left in the textarea.
- Naming: deliberately *queued prompts*, not "prompt queue" — the ADR-0052
  **permission** prompt queue (`permQueue`, `permQueueMax`) and ADR-0043's
  job *queueing* (jobs waiting for a concurrency slot) both already own that
  phrase. Identifiers: `queuedPrompt`, `queuedPrompts`, `queuePrompt`,
  `flushQueuedPrompt`, `cmdQueue = "/queue"`.

## 4. Input routing changes

`handleBusyKey`'s `"enter", "ctrl+m"` branch (`cmd/milk/repl.go:1339`) keeps
its order — completion accept → bang → slash → empty-input hint — and the
final "agent is working — Ctrl+Enter…" dead-end case becomes
`return m.queuePrompt(input)`.

`queuePrompt(input)` mirrors `handleEnter`'s reset block exactly (textarea
reset, tab-completion state, `histIdx`/`saved`) — factor that block into a
shared `resetInputState()` helper rather than copying it — then:

1. moves `m.pendingAttachments` into the entry and clears the field (so a
   queued attachment can never ride along with the *next* typed prompt);
2. records `m.leadingPasted` in the entry and clears the field;
3. appends the one-line receipt (UI design §4.1) and syncs layout.

It does **not** feed the loop detector or history — that happens once, at
flush, inside `submitInput` (see §5). Config knob `queue_while_busy: false`
(default `true`) restores the old hint-only Enter verbatim (escape hatch for
users who rely on the dead end; `internal/config/config.go`,
`docs/reference/spec.md`).

## 5. Turn-end drain

`handleAgentDone` (`cmd/milk/repl.go:1653`) already chains turn-end work:
background follow-ups (`maybeAutoFollowupBackgroundJobs`) then one
`pendingRemoteInputs` entry. The queue slots in **first**:

```go
if len(m.st.queuedPrompts) > 0 {
    q := m.st.queuedPrompts[0]
    m.st.queuedPrompts = m.st.queuedPrompts[1:]
    m.leadingPasted = q.leadingPasted
    m.pendingAttachments = q.attachments
    // Its turn drains finished jobs via drainBackgroundJobs anyway
    // (dispatch.go:221/:435), so any pending follow-up is satisfied by it.
    m.pendingBackgroundFollowup, m.pendingUserBackgroundFollowup = false, false
    return m.submitInput(q.text, dim("[queued]")+" ")
}
```

Drain order, one dispatch per turn end (each path re-enters this chain via the
next `agentDoneMsg`):

1. **queued user prompts** (FIFO, one per turn),
2. user-started background follow-up (`waitForWholeWave=false`),
3. agent-wave follow-up (`waitForWholeWave=true`),
4. remote inputs (one per turn, as today).

Rationale for (1) first: a queued prompt is the freshest, most
context-sensitive user intent ("now that you've done that, …"). It costs the
background-delivery path nothing: `drainBackgroundJobs` runs inside **every**
dispatch (`cmd/milk/dispatch.go:221`, `:435`), so completed job results ride
along in the queued prompt's turn; when the queue drains a synthetic
`backgroundFollowupPrompt` we clear both follow-up retry flags exactly the way
`applyFollowupDecision`'s `followupNow` does (`cmd/milk/followup_core.go`) —
if a job finishes *after* that turn started, its own done-msg re-arms the flag
as usual.

The flush delegates to `submitInput(q.text, dim("[queued]")+" ")` — the
`[telegram]`-precedent label (`cmd/milk/repl.go:1793`) keeps the transcript
turn-shaped and preserves everything `submitInput` already owns: loop-detector
feed (`IsUserTurn: true`), `appendDeduped` history, the `!leadingPasted`
command/bang/direct-bash gates (issue #151 — a queued paste starting with
`/learn` stays inert text), and `dispatchAgent`. No changes to
`dispatchAgent`, `runTurn`, or `internal/`.

Flush fires after **interrupted and failed turns too** — same as the
remote-input drain today (it sits after the `[interrupted]` handling
unconditionally). The queue is explicit user intent and is cancelable with
`/queue drop` in the meantime; branching on `m.interrupted` would add a mode
for little gain.

## 6. `/queue` command surface

New command `cmdQueue = "/queue"`, TUI-only (`hosts: hostTUI`), registered in
`commandRegistry` (`cmd/milk/slashcmd.go:140`) and added to
`busySafeCommands` (`cmd/milk/interactive.go:347`) — managing the queue is
exactly what a user waiting on a turn needs:

| Invocation | Behavior |
|---|---|
| `/queue` | list entries: index, age, line/attachment count, first-line preview |
| `/queue show <n>` | print one entry's full text (the `/bg show` uncapped convention) |
| `/queue drop <n\|last\|all>` | remove entry/entries (`all` ≡ `clear`) |
| `/queue clear` | alias of `drop all` |

No `add`/`run` subcommands: typing + Enter *is* the add path, and "run now" is
interrupt-then-retyping (or just waiting — the drain is at most one turn away).

## 7. Session lifecycle

The queue is conversation-bound, so explicit session switches (`/new`,
`/clear`, `/resume`, `/drop` — the `sessionmgmt_core.go` vocabulary) **drop it
with a transcript notice** (`[milk] dropped 2 queued prompt(s) — they were
typed into the previous conversation`). This deliberately diverges from
`pendingRemoteInputs`, which survives switches: a remote message is
sender-intent addressed to *the agent*, while a queued prompt was typed into a
specific conversation the user was watching. Ctrl+C is orthogonal: it cancels
the running turn and leaves the queue intact; the quit arming flow is
unchanged.

## 8. Edge-case matrix

| Case | Behavior |
|---|---|
| Tab completion active, Enter | accept wins (checked first, unchanged) |
| Bang `!cmd` + Enter while busy | runs immediately in the PTY pane (unchanged; `bang_while_busy_test.go`) |
| Slash command + Enter while busy | busy-safe: runs now; else refused with hint — never queued |
| Permission prompt pending (`handlePermKey`) | owns Enter; queueing resumes after the ask resolves (unchanged) |
| Workflow designer questions pending | Enter answers the designer (unchanged); queue untouched |
| Queued text that looks like a shell command (`direct_bash` on) | shelldetect gate runs at flush inside `submitInput`; the confirmation prompt appears then |
| Pasted prompt starting with `/…` or `!` | queued, flushed as inert text (leadingPasted replayed; issue #151) |
| Attachments in the input when queued | captured with the entry, re-installed at flush |
| Interrupt (Ctrl+C) mid-turn | queue survives; drains at `agentDoneMsg` like any turn end |
| Queue + background jobs finishing together | job results drain into the first flushed prompt's turn; follow-up flags cleared |
| Session switch with non-empty queue | dropped with notice (§7) |
| Queue cap hit | refused with busy hint, text kept in the textarea |
| Batch/ACP modes | no change (§11) |

## 9. Observability

Minimal: the transcript receipt + status chip are the product surface. Optional
(implementation phase 3): `obs` counters `milk.prompt_queued` /
`milk.prompt_queue_flushed` alongside `obs.IncrementTurnCount` for queue-depth
telemetry; nothing else in `internal/obs` changes.

## 10. Test plan

New `cmd/milk/queued_prompts_test.go` (model-level, message-driven like
`background_ui_test.go`):

1. Enter-while-busy with text → textarea cleared, receipt appended,
   `len(st.queuedPrompts) == 1` with `leadingPasted`/`attachments` captured;
   `pendingAttachments` empty afterwards.
2. FIFO + one-per-turn: 3 queued, `agentDoneMsg` flushes #1 and re-dispatches;
   the next `agentDoneMsg` flushes #2.
3. Drain order: queued prompt beats `pendingUserBackgroundFollowup`,
   `pendingBackgroundFollowup` and `pendingRemoteInputs` at the same
   `agentDoneMsg`; flushing clears both follow-up flags.
4. `/queue` list/show/drop/last/all/clear; `/queue` is accepted while busy
   (in `busySafeCommands`).
5. Cap: 21st prompt refused with `queue is full` busy hint, buffer kept.
6. Paste provenance: queued `"/learn …"` with `leadingPasted` flushes as text
   (no slash dispatch).
7. Session switch drops the queue and prints the notice.
8. `queue_while_busy: false` restores the old
   `agent is working — Ctrl+Enter to spawn a background agent with this` hint
   and queues nothing.
9. Interrupted turn still drains the queue.

Updated (their Enter-while-busy assertions move from "hint mentions
Ctrl+Enter" to "prompt is queued"): `background_ui_test.go` (lines ~287–360),
`completion_while_busy_test.go` (~79–139). Unchanged:
`zprobe_ctrlenter_test.go`, `bang_while_busy_test.go`, follow-up tests in
`background_ui_test.go`. Contract-style pin for the drain chain: a table test
over `handleAgentDone`'s priority order, mirroring `followup_core.go`'s
`decideFollowup` tests.

## 11. Out of scope / non-goals

- **ACP parity** — none needed: ACP `session/prompt` already queues behind
  `turnMu` (the "prompt queue" `acp_commands.go` refers to), so editors get
  queue semantics for free. The TUI is the gap this feature closes.
  *(Update 2026-11-23: superseded — [ADR-0108](../../adr/0108-mid-turn-steering.md)
  plus #446 gave ACP real mid-turn steering; serialization behind `turnMu` is
  now ACP's fallback for what cannot steer, not its queue.)*
- **Reordering/editing queued prompts** — drop + retype is enough for a queue
  whose lifetime is bounded by one turn; `promote`/`edit` can come later if
  live use demands it.
- **Queuing slash commands, background-panel surface, persistence across
  restarts** — explicitly rejected (§12).
- The Ctrl+Enter fork path itself is untouched (ADR-0043's spawn, wizard,
  delivery).

## 12. Alternatives considered

1. **Ctrl+Enter opens a fork-or-queue chooser** — rejected: modal friction on
   both actions, and both deserve one-keystroke reach. Queueing is reversible
   (cheap), forking is not (spawns a job, burns tokens) — the reversible one
   gets the default key.
2. **Shift+Enter queues, Enter unchanged** — rejected: shift/alt+enter/ctrl+n
   are the newline bindings (`keymap.go` `newline`), many terminals mangle
   Shift+Enter, and it leaves the Enter dead end in place — the main complaint.
3. **Ctrl+Enter queues, Enter forks** — rejected: flips established,
   documented, tested and tipped Ctrl+Enter semantics (ADR-0043,
   `docs/guides/operations.md`, `tips.go`).
4. **Merge all queued prompts into one turn** — rejected: each prompt is a
   separate thought; FIFO turns match how they were typed and keep turn
   boundaries (and the loop detector's per-turn signals) honest.
5. **Persist the queue in session JSON** — rejected: ephemeral working state,
   like `pendingRemoteInputs`.
6. **Queue slash commands too** — rejected: delayed `/restart`, `/escalate`
   etc. without the user watching is surprising; busy-safe commands already
   run immediately.
7. **Queue survives session switches** (parity with `pendingRemoteInputs`) —
   rejected: the prompt was typed into a specific conversation (§7).

## 13. Implementation phasing

1. **Phase 1 — core queue**: `queuedPrompt` state, `queuePrompt` +
   `resetInputState` refactor, `handleBusyKey` reroute, `handleAgentDone`
   drain + follow-up-flag clearing, receipt + status chip (UI design §4).
   Tests 1–3, 6, 9 + updated busy tests.
2. **Phase 2 — management**: `/queue` command family, `busySafeCommands`,
   cap behavior, session-switch drop, `queue_while_busy` knob. Tests 4, 5, 7, 8.
3. **Phase 3 — docs & polish**: `docs/guides/operations.md` (keyboard table,
   a "Prompt queue" subsection under input handling, cross-link from
   *Background sub-agents*), `docs/reference/spec.md` (config key), ADR-0043
   *Update* note, **ADR-0082**, `CLAUDE.md` key-design-decisions bullet,
   `tips.go` tip text (mention Enter-queues alongside Ctrl+Enter),
   `docs/guides/workflows.md` bang-note parenthetical. Optional obs counters.
