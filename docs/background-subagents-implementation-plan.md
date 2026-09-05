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
       baseCtx  context.Context // supplied at construction; NOT any turn's context — see below
       sem      chan struct{}
       jobs     map[string]*Job
       pending  []*Job // completed, not yet drained
       onDone   func(*Job) // optional immediate-notify hook (TUI), called off the holder's goroutine
   }

   func NewManager(baseCtx context.Context, maxConcurrent int) *Manager
   func (m *Manager) SetOnDone(fn func(*Job))
   func (m *Manager) Spawn(label, task, role, model string, run func(context.Context) (string, TokenUsage, error)) *Job
   func (m *Manager) Drain() []*Job // returns and clears pending completed jobs
   func (m *Manager) ActiveCount() int
   ```

   **Deviation from the original sketch, found during implementation:** `Spawn` does not take a per-call `ctx`. The TUI cancels each turn's context the instant that turn's `runTurn` call returns (`repl.go`'s `defer cancel()` immediately after starting it) — which happens almost immediately after `Spawn` returns, since `Spawn` itself is fast. A job spawned mid-turn must survive past that instant; its entire purpose is to keep running across whatever later turns eventually drain it. `run` receives a context derived from the Manager's own `baseCtx` (supplied once at construction — the session/TUI-root context) instead.

   **Second deviation, found live in a real session:** spawned background jobs never came back with a result at all. Root cause was two compounding gaps in "every job eventually finishes":
   - `cloneForBackground` (Phase 3) was copying the parent's `permAsk` callback — which blocks synchronously on a plain channel receive with no timeout or context-awareness (`readLineLabeled`'s `<-respCh` in `cmd/milk`). A background job hitting a permission-gated tool (`bash`, `write_file`, ...) showed an unattributed "Allow? [Y/n]" prompt nobody knew to expect, and hung forever holding its concurrency slot waiting for an answer that was never coming. Fixed: `permAsk` is no longer copied to the clone (the shared `permStore` still is, so already-granted tools keep working silently) — anything not already granted denies immediately instead of asking.
   - Even with that fixed, nothing bounded how long a job's `run()` could take once started — any other stuck call (a hung network read, a future bug) would still hold its slot forever. `Manager` gained a `jobTimeout` field (default 10 minutes, `SetJobTimeout` to override — a per-instance field, not a shared package `var`, specifically so a test mutating it can't race against another test's leaked goroutine), applied via `context.WithTimeout` around each job's `run()` call. A timed-out job is terminated, not retried.
2. `Spawn` generates an ID (`job_<n>`), acquires a semaphore slot in a goroutine (so `Spawn` itself never blocks the caller — if the semaphore is full, the job sits queued inside the goroutine, not on the caller's stack), runs `run(jobCtx)` (bounded by `jobTimeout`), records status/result/tokens/EndedAt, appends to `pending`, and invokes `onDone` if set.
3. Tests: concurrent spawns respect `maxConcurrent` (use a channel-gated fake `run` to assert no more than N execute simultaneously); `Drain` returns and clears; failed `run` sets `JobFailed` + `Err`; `onDone` fires exactly once per job; a job outlives a caller-side context cancelled right after `Spawn` returns; a stuck job's timeout fires, reaches `JobFailed`, and frees its slot for the next job to start immediately; a permission-gated tool call from a background job denies instead of hanging on a deliberately hang-simulating `permAsk`.

---

### Phase 3 — Scoped background execution mode on `local.Agent`

**Files:** `internal/agent/local/local.go`, `internal/agent/local/local_test.go`

1. Add a new system-prompt branch alongside the existing primary/escalation branches in `buildSystemPrompt` (local.go:809, branches at :833-869): a `background` role variant —
   > "You are a background research agent forked to answer one self-contained question. You have no knowledge of any parent conversation beyond the task given to you. Investigate using your tools and produce a concise, complete written answer — this is the only thing that will be reported back."
2. Add `RunBackgroundTask(ctx context.Context, cwd, task string, out io.Writer) (string, session.TokenUsage, error)` on `*Agent`:
   - Builds a fresh message list: `[{role: "system", content: <background prompt>}, {role: "user", content: task}]`.
   - Runs the existing iterative tool loop, factored out of `Run` into a shared `runToolLoop(msgs, tools, ...)` method so both paths reuse the same loop-detection/tool-dispatch code instead of duplicating it.
   - No session/history threading — this call owns its own message slice, not the caller's.
   - No memory/percept injection.
   - Tool schema list omits `agent_<name>*` and `spawn_background_agent` structurally (this method builds the list independently of `Run`'s, which is the only place either gets added) and excludes `escalate` via the existing `IncludedTools`/`ExcludedTools` limits mechanism rather than new filtering code.
   - Returns the final assistant text, accumulated token usage, and any error.

   **Deviation, found during implementation:** operates on `a.cloneForBackground()`, not `a` directly. A background job runs in its own goroutine and can easily still be running when the parent starts its own next turn on the same `*Agent` — `Run`/`runToolLoop`/`scanSSE` mutate several fields in place on the instance (`reasoningNgram`, `reasoningNgramTriggered`, `detectedFormat`, `pendingImageParts`), and reading them (even to reassign) races the parent's concurrent writes. `cloneForBackground` builds the clone from an explicit field list of known-stable configuration rather than `c := *a`, so it never reads the racy fields at all. Verified with a `-race` regression test.
3. Tests: verify a background run's tool list omits `escalate`/`agent_*`/`spawn_background_agent`; verify token usage is accumulated locally rather than fed to the parent's `onTokens`; verify `maxIter` still applies; verify no data race running concurrently with the parent's own `Run()` call (`-race`, `-count=30`+).

---

### Phase 4 — `spawn_background_agent` tool wiring

**Files:** `internal/agent/local/tools.go`, `internal/agent/local/local.go`, `cmd/milk/dispatch.go`

1. Add `spawnBackgroundAgentSchema()` (per ADR-0043 §1), appended at `Run`'s tool-list call site only when `a.backgroundManager != nil` — mirrors the existing conditional append of `a.mcpToolSet.Schemas(ctx)` right next to it. Never appended when `RunBackgroundTask` builds a background job's own tool list (Phase 3), which is what actually enforces the depth-1 cap.
2. Add a `backgroundManager *Manager` field on `Agent`, set via `SetBackgroundManager(m *Manager)` (mirroring `SetToolAgentDispatcher`). No separate `roleName` field needed — `agentRoleForMetrics(a.escalationName)` (already used for token-metric role tagging) doubles as the job's role tag.
3. In `dispatchOneTool`, add a case for `spawn_background_agent`:
   ```go
   if tc.Function.Name == "spawn_background_agent" && a.backgroundManager != nil {
       var args struct{ Task, Label string `json:"task"` `json:"label"` }
       json.Unmarshal([]byte(tc.Function.Arguments), &args)
       cwd := ""
       if sess != nil { cwd = sess.CWD }
       role := agentRoleForMetrics(a.escalationName)
       job := a.backgroundManager.Spawn(args.Label, args.Task, role, a.model,
           func(jobCtx context.Context) (string, session.TokenUsage, error) {
               return a.RunBackgroundTask(jobCtx, cwd, args.Task, io.Discard)
           })
       return "Spawned background agent " + job.ID + " (...). You will be notified when it completes."
   }
   ```
   `Spawn` takes no `ctx` (see Phase 2's deviation note) — the job runs under the Manager's own `baseCtx`, not this call's, so it survives past this turn ending. Returns immediately since `Spawn` itself doesn't block — no change needed to `executeToolCalls`'s batching/`WaitGroup` logic.
4. Construct one `*local.Manager` per session in `runREPL` (sized via `cfg.EffectiveMaxBackgroundAgents()`, using the session/TUI-root `ctx` — not any turn's — per Phase 2's deviation note), stored on `dispatchAgents.backgroundMgr` so it survives `buildTUIAgents` rebuilding the underlying `*local.Agent` copies every turn. `buildTUIAgents` re-attaches the same `*Manager` via `SetBackgroundManager` onto each turn's fresh copy. `SetOnDone` is wired right after `tea.NewProgram` (where the `*tea.Program` reference first exists) to send a `backgroundJobDoneMsg` (Phase 6) immediately on completion.
5. `drainBackgroundJobs(ctx, mgr, sess) string` (new helper in `dispatch.go`) drains the manager, records each job's tokens, and formats completed/failed jobs into a block. Called at the start of `runPrimaryWithSession`/`runEscalationWithSession`, prepended to a separate `dispatchPrompt` variable — not the plain `prompt`, which stays the user's actual text for `RecordNeed`/percept-matching/session bookkeeping. `runEscalation`/`runEscalationWithSession` gained a new `mgr *local.Manager` parameter threaded through every call site (self-escalation from `runPrimary`, single-prompt CLI mode passes `nil`, the main TUI turn path, two test files) — no special-casing by role.
6. Record tokens for each drained job via `sess.AddTokensFull(job.Model, job.Role+":subagent", job.Tokens.Prompt, job.Tokens.Completion, job.Tokens.CacheRead, job.Tokens.CacheCreation)` and mirror into OTel via `obs.RecordTokens`/`obs.AccumulateCacheTokens`, matching the existing `escalation:subagent` recording pattern in `dispatch.go`.

---

### Phase 5 — System-prompt guidance for when to use it

**Files:** `internal/agent/local/local.go` (primary/escalation branches, local.go:833-869)

1. Add a short paragraph, appended at `Run`'s call site (only when `a.backgroundManager != nil`) rather than threaded through `buildSystemPrompt`'s role branches — see `backgroundAgentGuidance` in `local.go`. Revised after live-verifying the feature in a real session (escalation agent had the tool available but never called it): leads with context economy as the reason, and explicitly tells the caller to keep each spawned task narrow and pass it concrete pointers (file paths, what's already been ruled out) rather than a vague question — a vague or overly broad task wastes real tokens/latency rediscovering things the caller already knew, which undermines the "smaller tasks" framing as much as an oversized result would. Deliberately does **not** add a hard cap on job result length or a tighter iteration budget for background jobs — both would silently degrade a job's actual capability for a problem that's really about prompting the caller well, not about enforcing a budget.
2. No test beyond a prompt-snapshot/contains-substring check, consistent with how other prompt branches are tested (if any existing test does this — check `local_test.go` conventions first).

---

### Phase 6 — TUI surfacing

**Files:** `cmd/milk/repl.go`, `cmd/milk/status.go`

1. `backgroundJobDoneMsg{job *local.Job}` as a `tea.Msg`, sent via `p.Send(...)` from the manager's `onDone` hook — wired right after `st.program = p` in `runREPL` (the earliest point a `*tea.Program` reference exists), mirroring the existing `taskStore.SetOnChange` → `p.Send(memoryRefreshMsg{})` pattern immediately below it.
2. `repl.go`'s `Update` handles `backgroundJobDoneMsg`: appends a transcript line (`⚙ background agent "<label>" completed` / `failed: <err>`) via the existing `appendTranscript` helper. No separate in-memory status slice needed for the status-bar count — that reads `ActiveCount()` directly (item 3).
3. Status bar (`status.go`): a standalone `if m.agents.backgroundMgr != nil { if n := ...ActiveCount(); n > 0 { ... } }` block renders `[⚙ N background agent(s) running]`, alongside — not part of the mutually-exclusive loop/perm/selection `else if` chain, since it's independent information.
4. **Skipped** (marked optional in the original plan): repurposing the dead `panelTasks`/`tasksOffset` scaffolding into a full background-jobs panel. The transcript line + status bar count already deliver visibility; a dedicated panel remains a reasonable follow-up but wasn't needed for this pass.
5. **Added after live-verifying the feature, not in the original plan**: items 1-4 above are all passive — none of them actually generate a response. An escalation agent that had just spawned a wave told the user "I'll follow up automatically when they finish," and nothing made that true. `Manager.SetOnBatchDone(fn func())` (`background.go`) fires exactly once when the last job of the current wave finishes (`ActiveCount()` reaches 0 inside `finish()`, guarded by a `batchSignaled` flag reset on `Drain()` so the next wave can signal again) — deliberately separate from the per-job `SetOnDone`, since firing per-job would produce one redundant follow-up turn per job in a batch rather than one consolidated turn for the whole wave. Wired to a new `backgroundBatchDoneMsg`, handled by `maybeAutoFollowupBackgroundJobs` (`repl.go`): if idle (`!m.busy && m.pendingPerm == nil && m.pendingDirectBash == nil && m.ptyPane == nil`), submits a synthetic prompt (`backgroundFollowupPrompt`, labeled `"[background]"` in the transcript so it reads as distinct from the user's own input) through the exact same `submitInput`/`dispatchAgent` path a real user turn takes; if not idle, sets `pendingBackgroundFollowup` and retries from `handleAgentDone` once the in-flight turn completes — mirroring the pre-existing pattern for queued Telegram remote-oversight input (`m.st.pendingRemoteInputs`) exactly.

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
- **Completion delivery has three paths, and live-testing showed all three are needed**: (a) immediate TUI notification via `onDone` → `tea.Program.Send` (Phase 6) — a passive transcript line only; (b) next-turn context injection via `Drain()` (Phase 4.5) — only runs if some turn happens to be dispatched for an unrelated reason; (c) `onBatchDone`-triggered auto-follow-up (Phase 6, added after (a)+(b) alone left a live session's spawned wave with no final response at all) — actually dispatches a turn once the whole wave finishes. (a) alone leaves the calling agent unaware next turn; (b) alone leaves the TUI silent until the user happens to send another turn; without (c), nothing generates the response the agent typically promises ("I'll follow up automatically") unless the user prompts again.
- **Token accounting**: use the generalized `<role>:subagent` convention (Phase 4.6), not a hardcoded `"primary:subagent"` — read the spawning agent's actual role at spawn time.
- **Cleanup on session end**: `Manager` needs a way to be told the session is ending so in-flight goroutines don't outlive it unbounded; at minimum, cancel their context (jobs are given a context derived from the session's lifetime, not `context.Background()`).
