# Background-job soft deadline and bash-failure fix — plan

Status: **implemented** — merged as PR #274 (`fix(local): bash exit code as the
single failure signal + background-job soft deadline`).

Issue: #271. Decision records: [ADR-0068](../../adr/0068-background-job-soft-deadline.md) (new), [ADR-0062](../../adr/0062-tool-batch-barrier-dispatch.md) (Update section).

## Evidence

`~/.milk/jobs/<session>.json`, `job_1` (2026-10-19, `mimo-v2.6-flash`, role `primary`): `status: failed`, `error: timed out after 20m0s`, `timed_out: true`, 25 requests, decode 305 s, TTFT 90 s. Result was the no-final-summary trail only. The trail shows a `grep -ao … | sort -u | head` returning `binary file matches` on stderr, followed by `skipped: bash failed earlier in this batch`.

## Step 1 — bash failure semantics (small, independent)

`internal/agent/local/local.go` `toolCallFailed`: for `name == "bash"`, decide from the exit code and launch/timeout errors, not from `Error != ""`.

- Failed: `ExitCode != 0`; or a result with `Error` and empty `Output` and exit 0 that came from `cmd.Start` failure, `bash: command timed out`, or a non-`ExitError` wait error. Distinguish these structurally rather than by string match — e.g. have `runBash` set a non-zero `ExitCode` (-1) on those paths so exit code is the single signal.
- Not failed: exit 0 with stderr text.
- Other cancelling tools unchanged (`isToolError`).

Tests (`batch_dispatch_test.go`): exit-0-with-stderr does not cancel the following call; exit 1 does; start failure and timeout do; `grep` no-match is still not a barrier.

## Step 2 — soft deadline

`internal/agent/local/background.go`:

- In `Spawn`'s goroutine, after the slot is acquired and `execCtx` created, start a `time.AfterFunc(timeout*8/10, …)` that calls `m.Send(job.ID, wrapUpMessage(remaining))`; stop it (`defer t.Stop()`) when the job body returns. Skip when `timeout < 2*time.Minute`.
- Ignore `ErrJobFinished` / `ErrJobInboxFull`; log other errors at WARN. Emit an `obs.Event("background.soft_deadline", "job", …)` so it is visible in `milk.log`.
- `wrapUpMessage`: minutes left, "stop exploring, write your final answer now from what you have", the `<result status="partial" files_touched="…"/>` hint.

Salvage: where the no-final-summary trail is assembled for a cancelled loop (`noFinalSummaryTrailHeader`, `local.go`), prepend the last non-empty assistant text when present, so a hard-timed-out job's `Result` carries its latest conclusions plus the trail.

Tests: fake clock/short timeout job receives exactly one wrap-up message at ~80%; none for a sub-2-minute timeout; none after the job finished; hard-timeout result includes last assistant text.

## Step 3 — docs and verification

- `docs/guides/tooling.md`: one sentence on the soft deadline under background agents; the barrier section already describes failure — adjust wording to "non-zero exit code".
- CLAUDE.md "Background sub-agents" bullet: one clause on the soft deadline.
- Live-test per project rule before merge: run a TUI session with `background_agent_timeout_minutes: 3` and a deliberately long research task; confirm the wrap-up message appears in the job's live buffer at ~2:24 and the job returns `partial`. Separately confirm a `bash` call that only writes to stderr no longer cancels the next call.

## Out of scope

Raising the default timeout; per-job timeout parameter on `spawn_background_agent`; changing loop-detection behavior.
