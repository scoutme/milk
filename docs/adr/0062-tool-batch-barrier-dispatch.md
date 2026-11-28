# 62. Tool batches execute under a barrier model

Date: 2026-10-19

Status: Accepted

## Context

`docs/internal/analysis/local-agent-toolset-review.md` (B1) found that `executeToolCalls` dispatched **every** call in a response's tool batch in its own goroutine at once — writes and shell included. Only the permission prompts were ordered. Two concrete failure modes:

- `edit_file a.go` plus `bash "go build"` in one response could build before the edit landed.
- Two `edit_file` calls on the same file raced on read-modify-write; one edit was silently lost.

Small models emit multi-call batches routinely (the stream detector explicitly supports one call per plan step), so this was not an edge case. MiMo-Code's `gate.ts` runs only read/grep/glob in parallel and treats every other call as a barrier; the review recommended the same shape and asked for an explicit decision on the failure semantics.

## Decision

A batch is executed in **call order** as alternating *read runs* and *barriers*:

- **Read run**: a maximal run of consecutive pure-read calls — `read_file`, `list_dir`, `grep`, `find_files`, `project_map`, `get_memory`, `list_memory`, `get_session_context`, `get_context_stats`, `get_metrics`, `search_signals`, `list_tasks`, `milk_config_help` — runs concurrently within the run (`isParallelSafeTool`).
- **Barrier**: everything else (writes, `bash`, `http_*`, memory writes, `current_need`, orchestration like `spawn_background_agent`/`start_workflow`, agent-as-tool, MCP tools) executes alone, in order.
- **Cancel-on-failure for mutating barriers**: when `bash`, `write_file`, `edit_file`, `delete_file` or `move_file` fails (`isToolError`, or a non-zero exit code for bash — grep's exit 1 "no matches" is read-only and never cancels), the rest of the batch is **skipped** with `skipped: <tool> failed earlier in this batch, so this call was cancelled` results (`isCancellingTool`). A failed *read* cancels nothing; nor does a corrective `tool_search` guidance result (a deferred-tool refusal or schema echo — the tool never ran, so the world is unchanged).

Kept from the old behavior: results are appended in original call order regardless of finish order; per-tool timeouts; permission checks collected synchronously before dispatch (so prompts stay ordered and never interleave); Ctrl-C/context cancellation propagates to every in-flight tool immediately.

## Consequences

- A batch of independent writes is now serialized — slower, but the ordering a model plans with is the ordering that happens. This is the explicit trade-off: correctness over batch latency.
- A denied permission on a mutating tool now also cancels the remainder of the batch (a denial is a failure result) — the model is told, per skipped call, why nothing ran.
- `docs/guides/tooling.md`'s "Concurrent tool dispatch" section now documents the barrier model instead of promising all-concurrent execution.
- Regression tests: `internal/agent/local/batch_dispatch_test.go` (write-then-bash ordering, two edits on one file, cancel-on-failure, read-run concurrency via FIFOs), `oob_tools_test.go` (barrier serialization, timeout ⇒ skip).

Implementation: `dispatchBatch` / `isParallelSafeTool` / `isCancellingTool` / `toolCallFailed` in [`internal/agent/local/local.go`](../../internal/agent/local/local.go).

## Update (2026-10-19, issue #271): stderr alone is not a bash failure

The Decision above says a bash call fails on `isToolError` *or* a non-zero exit code. In the implementation `bash` returns stderr in `toolResult.Error`, so `isToolError` was true for **any** stderr output, including a successful (exit 0) command that merely warned (`grep` "binary file matches", compiler or `git` notices). Such a call cancelled the rest of the batch and told the model the command had failed.

Corrected rule: for `bash`, a call failed when its exit code is non-zero, or it could not be launched, timed out, or was killed (those results carry `Error` with no command output). stderr on an exit-0 run is informational and cancels nothing. `write_file`, `edit_file`, `multiedit`, `delete_file` and `move_file` keep `isToolError`, since their `Error` field is a genuine failure.
