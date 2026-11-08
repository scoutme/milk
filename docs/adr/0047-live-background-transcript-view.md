# 47. Live-Attach View for Background Jobs and Workflows

Date: 2026-09-27

Status: Accepted

## Context

GitHub issue #154 asked for background jobs (`spawn_background_agent`, ADR-0043) and milk's native `/workflow` engine (ADR-0039) to be viewable **while running**, not just after they finish — the background/workflow panels showed only a status badge and elapsed time, with no way to see what was actually happening.

Two concrete problems, not just a missing nice-to-have:

- **Background jobs had no live content at all.** `internal/agent/local.Job` stored only a final `Result` string, set once at completion. `RunBackgroundTask`'s tool-loop output writer was hardcoded to `io.Discard` — the same tool-call/streamed-text output a normal turn shows was simply thrown away for a background job.
- **The native `/workflow` engine already streamed live stage output, into the wrong place.** `workflow.WorkflowChunkMsg` was piped straight into the *main session transcript* (`cmd/milk/repl.go`, `m.appendTranscript(msg.Text)`), unconditionally. This is the concrete bug behind the issue's complaint that background/workflow activity isn't kept separate from the live conversation: workflow output was already live, just mixed into the wrong buffer.

ADR-0043's own "Neutral" section anticipated this tension: background jobs are deliberately not recorded in the session transcript, "consistent with ADR-0034's decision not to record tool-agent internal reasoning in the session." Any fix needed to add visibility without reopening that decision — the internal activity still must never leak into the main transcript or into what gets sent to the escalation agent as context. It only needed to become *viewable on demand*, not *always shown*.

## Decision

### 1. `internal/livebuf.Buffer` — a shared live-content primitive

A new small package: a thread-safe, byte-capped buffer of streamed text (`New(maxBytes int) *Buffer`, `Writer() io.Writer`, `Snapshot() string`, `Len() int`), oldest bytes dropped once over cap (default 256KB). One implementation shared by both consumers below — background jobs and workflows both write the same shape of data (arbitrary, possibly-mid-ANSI-escape chunks) for the same purpose, so one tested primitive avoids two buffers with subtly different cap/trim/concurrency behavior.

Content in a `livebuf.Buffer` is in-memory only: never persisted, and never fed into any prompt or the session transcript. `State.Live` is tagged `json:"-"` (`workflow.State` is itself serialized directly to a checkpoint file); `Job.Live` needs no such tag — `Job` is never JSON-marshaled directly, only the separate, explicitly-fielded `jobRecord` (`jobstore.go`) is persisted, and it simply has no `Live` field to marshal.

### 2. Wiring: `workflow.State.Live` and `local.Job.Live`

- `internal/workflow/state.go`: `State` gets `Live *livebuf.Buffer`, lazily created via `LiveBuffer()`. `cmd/milk/repl.go`'s `WorkflowChunkMsg` handler now appends into it instead of calling `appendTranscript` — the fix for the concrete bug above. The existing start/complete/error one-line transcript breadcrumbs (`⚙ starting workflow …` / `workflow complete` / `workflow error: …`) were already sufficient signal for the main transcript and needed no change.
- `internal/agent/local/background.go`: `Job` gets `Live *livebuf.Buffer`, created in `Manager.Spawn` before the job is registered. `JobRun`'s signature gained an `out io.Writer` parameter, threaded through `safeJobRun` into the job body — a breaking, not additive, change, so every existing `JobRun`-literal in the codebase (production call sites and tests alike) was swept in the same change. `RunBackgroundTask` already had an `out io.Writer` parameter (previously always `io.Discard`); both call sites (`spawn_background_agent`'s tool dispatch, and the user-initiated background-spawn path in `cmd/milk/repl.go`) now forward the real writer. A retried attempt (transient-error retry, unrelated to this ADR) reuses the same buffer across attempts, so an attached viewer sees the retry happen rather than losing the earlier attempt's output — intentional, not a bug.
- `Job.Live`'s pointer is set once at `Spawn` and never reassigned, so — unlike every other `Job` field — it's safe to read without `Manager.mu` even though `Manager.Jobs()` copies the rest of `Job` under that lock for display: the pointer itself never changes, and `livebuf.Buffer` has its own internal lock guarding concurrent `Append` (the job's own goroutine) against `Snapshot` (the UI goroutine).

### 3. TUI: an "attach" mode, not a permanent split or a post-hoc dump

Two other shapes were considered and rejected:

- **A permanent horizontal split** (main transcript + a fixed pane always showing the attached buffer) halves the width of both at all times, which is rough given side panels already compete for columns on a typical terminal width — and most of the time nothing is attached, so the split would be dead weight.
- **A post-hoc dump into the main transcript** (mirroring the memory panel's existing double-click-for-detail behavior, which prints a percept's full content into the transcript) satisfies "viewable" but not "live" — the whole point raised in issue #154 was watching a job/workflow *while* it runs, not after.

Instead: double-clicking a background-job row (background panel, **F3**) or the workflow panel (**F4**) swaps the main transcript viewport for that job's/workflow's `livebuf.Buffer`, Esc to detach back — modeled directly on the existing `m.ptyPane` pattern (an embedded shell fully replacing the transcript viewport), generalized from a real VT100 emulation to a plain streamed-text buffer, since this is read-only, not an interactive terminal. One deliberate divergence from that precedent: side panels (`memory`/`tasks`/`background`/`workflow`) stay visible during attach, unlike `ptyPane`'s full-screen takeover — the point of attach mode is to watch something a side panel is showing while it keeps running, so hiding that panel would defeat the purpose.

Mechanics:

- `cmd/milk/attach.go`: `attachState{kind, jobID, label, buf, vp}` (`vp` is the attach view's own independent scroll position), `model.attached *attachState`. `startAttach`/`detachAttach`/`syncAttachedContent` (rewrap-and-set-content, sticky-bottom scroll matching the main transcript's own convention). A 500ms poll (`attachRefreshTick`/`attachRefreshMsg`, mirroring the existing memory-panel poll pattern) redraws the attach view while attached, since a background job's buffer is written to by its own goroutine with no accompanying `tea.Msg` to trigger a redraw on its own; workflow chunks additionally get an immediate resync from their own message handler, so that path feels snappier, with the poll as a fallback.
- Double-click detection reuses the exact arm/trigger timing already used by `handleMemoryPanelClick` (400ms), on the same `m.lastPanelClickID`/`m.lastPanelClickTime` fields (namespaced with a `"bg:"`/`"wf"` prefix so a job ID string and a memory-panel percept/brick ID can never collide) — but attaches instead of printing, the deliberate divergence from that existing behavior.
- Esc detaches; every other key is swallowed while attached (a read-only view, not an input surface) — arrow keys in particular stay reserved for input-history navigation elsewhere in this TUI, never repurposed here. Amended by ADR-0103: the text-editing keys (backspace, delete, ctrl+h) are input intent, not view navigation — they hand focus to the input and apply there (type-to-focus parity with the focused panels); everything else still swallows. Mouse wheel over the main-viewport area scrolls the attach buffer instead of the main transcript while attached; side-panel wheel scrolling is untouched. Detaching never happens automatically on job/workflow completion — the buffer simply stops growing, so the user can read the tail at their own pace.
- `obs.Debug("attach.start"/"attach.stop", ...)` logs the job/workflow ID and label at each transition, into the existing `milk.log`/OTel pipeline — the observability substitute for a TUI interaction that's inherently hard to unit-test end-to-end.

### 4. Explicitly out of scope

- **Claude CLI's own Agent-tool (Task-tool) subagents.** `internal/agent/claude/stream.go` parses only aggregated `subagent_usage`/`workflow_usage` token counts from the `result` stream-json event — no content field exists to surface. This is a hard protocol limitation of Claude Code's `stream-json` output, not a milk parsing gap, and there is nothing to build here today.
- **Claude CLI's own background-workflow `journal.jsonl` tail.** Claude Code's separate background-workflow feature (distinct from milk's native `/workflow` engine) writes a `journal.jsonl` file milk already locates (`cmd/milk/runner.go`'s `waitForWorkflowResult`) but only scans for a final `{"type":"result"}` line. The line schema for anything else is undocumented anywhere in this repo and unverified against a real sample — shipping a parser against zero real samples risks looking safe (never crashes) while being silently useless (never renders anything meaningful). Deferred pending an actual captured sample of a real journal file's shape.

## Consequences

**Positive:**

- Closes issue #154 for the two transcript sources milk fully controls (background jobs, native workflows) with one shared primitive and one shared TUI mechanism, rather than two bespoke ones.
- Fixes a real, live bug (workflow output leaking into the main transcript) as a side effect of building the feature, not a separate fix.
- Preserves ADR-0043/ADR-0034's non-leak intent by construction: the new buffers are opt-in-to-view, never auto-injected into the transcript or any prompt.

**Negative:**

- `JobRun`'s signature change touched every existing call site (production and test) — a real, if mechanical, migration cost paid once.
- The 500ms poll for background-job attach content is a real (if minor) inefficiency compared to a push-based update; accepted because adding a new push channel from the job's goroutine to the TUI would introduce concurrency surface area (another `tea.Send` call site, another ordering question) for a UI-only convenience the poll already satisfies at negligible cost.
- Attach is mouse-only (double-click), matching the memory panel's existing detail-view convention, but with no keyboard equivalent — consistent with existing precedent, not a new gap, but still a real limitation for a keyboard-first user.

**Neutral:**

- Claude CLI's Task-tool subagents remain opaque; nothing regresses, but the feature request is only partially satisfied for that transcript source, and cannot be fully satisfied without a `stream-json` protocol change upstream.
- The background-workflow `journal.jsonl` tail is deferred, not abandoned — a real sample would let a future increment build the same best-effort, defensively-parsed attach view for that third source.
