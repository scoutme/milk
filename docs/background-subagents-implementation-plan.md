# Implementation Plan: Background Sub-Agents (ADR-0043)

## Overview

Give `internal/agent/local`'s tool loop a `spawn_background_agent` tool that forks an independent copy of the calling agent (same config, same built-in tools) to research a self-contained question in the background, with its own multi-step tool loop, and reports back a distilled result asynchronously — without blocking the caller's turn or consuming the caller's context.

This is a companion feature to ADR-0034 (Agent-as-Tool): that ADR lets an agent call a *different*, specific peer agent synchronously with no tool loop; this one lets an agent fork *itself* asynchronously with a full tool loop. Applies to any role (primary or escalation) backed by an inference-server provider — not to `claude-cli` (already has native fork) or subprocess agents (no built-in tool loop at all).

## Phases

---

### Phase 1 — Config schema

**Files:** `internal/config/config.go`, `internal/config/config_test.go`

1. Add to `Config` (top-level, near existing agent/tool settings):
   ```go
   MaxBackgroundAgents int `json:"max_background_agents,omitempty"` // default 3 if zero
   ```
2. Add a helper `func (c Config) EffectiveMaxBackgroundAgents() int` returning the configured value or a default of 3.
3. Tests: zero value defaults to 3; explicit value is honored; negative/zero-after-explicit-set is clamped to at least 1.

---

### Phase 2 — Job manager

**Files:** new `internal/agent/local/background.go`, `internal/agent/local/background_test.go`

1. Define:
   ```go
   type JobStatus string
   const (
       JobRunning   JobStatus = "running"
       JobCompleted JobStatus = "completed"
       JobFailed    JobStatus = "failed"
   )

   type Job struct {
       ID        string
       Label     string
       Task      string
       Status    JobStatus
       Result    string
       Err       error
       Role      string // "primary" or "escalation" — role of the spawning agent
       Model     string
       Tokens    TokenUsage // reuse existing token-usage type
       StartedAt time.Time
       EndedAt   time.Time
   }

   type Manager struct {
       mu       sync.Mutex
       sem      chan struct{}
       jobs     map[string]*Job
       pending  []*Job // completed, not yet drained
       onDone   func(*Job) // optional immediate-notify hook (TUI), called off the holder's goroutine
   }

   func NewManager(maxConcurrent int) *Manager
   func (m *Manager) SetOnDone(fn func(*Job))
   func (m *Manager) Spawn(ctx context.Context, label, task, role, model string, run func(context.Context) (string, TokenUsage, error)) *Job
   func (m *Manager) Drain() []*Job // returns and clears pending completed jobs
   func (m *Manager) ActiveCount() int
   ```
2. `Spawn` generates an ID (`job_<n>`), acquires a semaphore slot in a goroutine (so `Spawn` itself never blocks the caller — if the semaphore is full, the job sits queued inside the goroutine, not on the caller's stack), runs `run(ctx)`, records status/result/tokens/EndedAt, appends to `pending`, and invokes `onDone` if set.
3. Tests: concurrent spawns respect `maxConcurrent` (use a channel-gated fake `run` to assert no more than N execute simultaneously); `Drain` returns and clears; failed `run` sets `JobFailed` + `Err`; `onDone` fires exactly once per job.

---

### Phase 3 — Scoped background execution mode on `local.Agent`

**Files:** `internal/agent/local/local.go`, `internal/agent/local/local_test.go`

1. Add a new system-prompt branch alongside the existing primary/escalation branches in `buildSystemPrompt` (local.go:809, branches at :833-869): a `background` role variant —
   > "You are a background research agent forked to answer one self-contained question. You have no knowledge of any parent conversation beyond the task given to you. Investigate using your tools and produce a concise, complete written answer — this is the only thing that will be reported back."
2. Add `RunBackgroundTask(ctx context.Context, task string, out io.Writer) (string, TokenUsage, error)` on `*Agent`:
   - Builds a fresh message list: `[{role: "system", content: <background prompt>}, {role: "user", content: task}]`.
   - Runs the existing iterative tool loop (`Run`'s inner loop / `executeToolCalls`), reusing built-in tool dispatch, but:
     - No session/history threading — this call owns its own message slice, not the caller's.
     - No memory/percept injection.
     - Tool schema list excludes `agent_<name>*`, `spawn_background_agent`, and `escalate` (filter in the `schemas()` call for this mode — add a `excludeToolAgents bool` / `mode` parameter).
   - Returns the final assistant text, accumulated token usage, and any error.
3. Tests: verify a background run's tool list omits `escalate`/`agent_*`/`spawn_background_agent`; verify no calls to session-recording hooks occur (use a fake session spy); verify `maxIter` still applies.

---

### Phase 4 — `spawn_background_agent` tool wiring

**Files:** `internal/agent/local/tools.go`, `internal/agent/local/local.go`, `cmd/milk/dispatch.go`

1. Add the `spawn_background_agent` schema (per ADR-0043 §1) to `schemas()`, gated the same way `agent_*` tools are — omitted entirely when building the background-mode tool list from Phase 3 (depth cap).
2. Add a `*Manager` field on `Agent` (set via `SetBackgroundManager(m *Manager)`, mirroring `SetToolAgentDispatcher`) and a `roleName string` already available for prompt building — reuse it to tag jobs.
3. In `dispatchOneTool` (local.go:1532), add a case for `spawn_background_agent`:
   ```go
   case "spawn_background_agent":
       var args struct{ Task, Label string }
       json.Unmarshal([]byte(tc.Function.Arguments), &args)
       job := a.backgroundManager.Spawn(ctx, args.Label, args.Task, a.roleName, a.model,
           func(jctx context.Context) (string, TokenUsage, error) {
               return a.RunBackgroundTask(jctx, args.Task, io.Discard)
           })
       return fmt.Sprintf("Spawned background agent %s (%q). You will be notified when it completes.", job.ID, args.Label), nil
   ```
   This runs inside the per-tool-call goroutine already used by `executeToolCalls`'s batch (local.go:1471-1479), but returns immediately since `Spawn` itself doesn't block — no change needed to the batching/`WaitGroup` logic.
4. In `dispatch.go`, construct one `*Manager` per session (sized via `cfg.EffectiveMaxBackgroundAgents()`), wire it into whichever runner(s) are localRunner instances for that session (both primary and escalation, if both are local-backed), and call `SetOnDone` with a hook that sends a `BackgroundJobMsg` to the TUI program (Phase 6) immediately on completion.
5. At the start of `runPrimary`/`runEscalation`, before building this turn's context, call `mgr.Drain()` and prepend any completed jobs' results as a synthetic context block (same injection point as percepts) — format per ADR-0043 §4.
6. Record tokens for each drained job via `sess.AddTokensFull(job.Model, job.Role+":subagent", job.Tokens)` and mirror into OTel via `obs.RecordTokens`/`obs.AccumulateCacheTokens`, matching the existing `escalation:subagent` recording at dispatch.go:354-364.

---

### Phase 5 — System-prompt guidance for when to use it

**Files:** `internal/agent/local/local.go` (primary/escalation branches, local.go:833-869)

1. Add a short paragraph to both the primary and escalation prompt branches (since both can be inference-server-backed):
   > "When a task requires reading or searching through a large amount of code or many independent things, prefer `spawn_background_agent` for each independent question rather than reading everything into your own context. You will be told when each one finishes — do not guess or fabricate its result before that, and do not poll; continue other work or respond to the user in the meantime."
2. No test beyond a prompt-snapshot/contains-substring check, consistent with how other prompt branches are tested (if any existing test does this — check `local_test.go` conventions first).

---

### Phase 6 — TUI surfacing

**Files:** `cmd/milk/repl.go`, new `cmd/milk/panel_background.go` (or extend `panel_workflow.go`)

1. Define `BackgroundJobMsg{Job *local.Job}` (or a slimmed view struct) as a `tea.Msg`, sent via `p.Send(...)` from the manager's `onDone` hook (Phase 4.4).
2. `repl.go`'s `Update` handles `BackgroundJobMsg`: appends a transcript line (`⚙ background agent "<label>" completed`) and updates a small in-memory slice of recent job statuses for the status bar count.
3. Status bar: while `ActiveCount() > 0`, render `⚙ N background agents running` next to the existing loop-detection indicator.
4. Optional: repurpose the currently-unused `panelTasks`/`tasksOffset` scaffolding (repl.go:495-496, `regionTasks` at :997-1017) as the background-jobs panel rather than leaving it dead, following the same render pattern as `buildWorkflowPanelLines`/`renderStageTree` in `panel_workflow.go` but as a flat list (id, label, status, elapsed) instead of a stage tree.

---

### Phase 7 — Docs

**Files:** `docs/tooling.md`, `docs/operations.md`, `docs/spec.md`, `CLAUDE.md`

1. `docs/tooling.md`: add `spawn_background_agent` to the built-in tool list (§ built-in tools), with a short explanation and a pointer to ADR-0043.
2. `docs/operations.md`: add `max_background_agents` to the config/operations reference table.
3. `docs/spec.md`: add `max_background_agents` to the config field table.
4. `CLAUDE.md`: extend the existing "Token tracking: subagents and workflows" section's role-string table with `primary:subagent` (locally-spawned) alongside the existing Claude-CLI-observed `escalation:subagent`/`escalation:workflow`, and add a one-line pointer to ADR-0043 under "Key design decisions."

---

## Sequencing

```
Phase 1 (config)
    ↓
Phase 2 (job manager)  →  Phase 3 (scoped background run mode)
                                    ↓
                          Phase 4 (tool wiring + dispatch drain/injection + token accounting)
                                    ↓
                  Phase 5 (prompt guidance) ──┐
                  Phase 6 (TUI panel)         ├─ independent after Phase 4
                  Phase 7 (docs)      ────────┘
```

---

## Key constraints / notes

- **No recursion**: background jobs never see `spawn_background_agent`, `agent_<name>`, or `escalate` in their tool list — depth capped at 1, matching ADR-0034's no-chaining precedent.
- **No session writes from a background job**: `RunBackgroundTask` must never call `sess.AddTurn`/`sess.Save`/state transitions — only the drained result is injected into the *next* real turn's context, at the same injection point as percepts.
- **Role-generic, not role-specific**: both `runPrimary` and `runEscalation` in `dispatch.go` need the manager wired if their respective runner is a `localRunner`; do not special-case escalation as "already covered" — it's the *provider* (`claude-cli` vs inference-server), not the role, that determines whether this is needed.
- **Bounded concurrency**: `max_background_agents` (default 3) is a semaphore inside `Manager`, shared across however many jobs a session spawns across however many turns — not reset per turn.
- **Don't race**: the prompt guidance (Phase 5) must explicitly tell the model not to fabricate a background job's result before the completion notification arrives — this is the same discipline Claude Code's own fork tool documents.
- **Completion delivery has two independent paths**: (a) immediate TUI notification via `onDone` → `tea.Program.Send` (Phase 6), and (b) next-turn context injection via `Drain()` (Phase 4.5) — both must exist; (a) alone would leave the calling agent unaware next turn, (b) alone would leave the TUI silent until the user happens to send another turn.
- **Token accounting**: use the generalized `<role>:subagent` convention (Phase 4.6), not a hardcoded `"primary:subagent"` — read the spawning agent's actual role at spawn time.
- **Cleanup on session end**: `Manager` needs a way to be told the session is ending so in-flight goroutines don't outlive it unbounded; at minimum, cancel their context (jobs are given a context derived from the session's lifetime, not `context.Background()`).
