# 52. Three-Source Permission Asks with a Timed Answer for Background Work

Date: 2026-10-12

Status: Accepted

## Context

Permission asks used to resolve over two sources — direct input (the TUI prompt queue / the ACP client's dialog) and remote oversight (Telegram), first answer wins. Two failure modes fell out of that model:

- **Unattended contexts failed closed without asking.** The doom-loop gate (3 identical tool-call batches) in a background job or workflow step terminated the turn immediately ("this context has no way to ask for confirmation"), and the TUI didn't even wire `WithBackgroundPermissionAsk` — job tool asks were denied outright. Silence was treated as absence of intent rather than absence of the user.
- **The remote surface was the only escape hatch, and its timeout was the wrong knob.** `timeout_action` decides what a *remote-side* silence means for a foreground ask; reusing it for background asks would have made two timers with two configs race to decide the same question.

The user-facing requirement: surface background permissions to the main TUI too — never fail immediately when a human *could* answer — and bound the wait with a third, explicit answer source: a **timed answer** that applies a configured default (`deny` by default) at a deadline (360s by default: long enough to notice and answer, short enough not to stall a long unattended task). The default must be settable via config **and** an ad-hoc slash command, and the safety default (doom-loop confirmations) must be independently settable so one `allow` cannot silently re-enable unattended runaway loops.

## Decision

`cmd/milk/permask.go` is the single ask layer. Every ask races up to three **answer sources**; the first *real* answer wins:

| Source | Applies to | Semantics |
|---|---|---|
| direct input | all asks | TUI prompt queue / ACP `session/request_permission` |
| remote oversight | all asks | real Telegram y/n replies only — a timeout is not a reply |
| timed answer | background asks only | at `ask_time + deadline`, the configured default applies |

- **Foreground asks** (live turns) are unchanged: sources 1–2, no deadline, and remote-side silence still resolves with the remote's `timeout_action` (`remoteTimeoutResolves` in `askPolicy`). `oversight.Notifier.AskPermission` now returns `(decision, answered)` so the layer can tell a reply from a silence; `TimeoutAction` application stays in the backend but its result is only *used* where the policy says so. This also fixed a latent bug where `oversight.Noop` answered `PermAllow` and made the claude-cli race auto-allow everything with oversight disabled.
- **Background asks** — background-job tool asks, workflow-step tool asks (routed through `bgPermAsk` by `AsWorkflowExecutor`), and the doom-loop gate's unattended confirmations (`WithSafetyAsk` → `local.SafetyOutcome`: approved / declined / timed-out / unreachable) — surface to **all** surfaces and resolve with the configured default at the deadline. Only when *no* surface exists at all (single-prompt mode) does the gate fail closed without waiting.
- **Prompt queue UX** (user-approved): background asks join the TUI's existing prompt queue with attribution and a countdown, the timed answer labeled in the prompt ("auto-deny in 6m0s"), bulk keys `a`/`d` (allow/deny all) to defuse bursts, a queue cap (5) whose overflow resolves straight to the timed default, and the user's input draft stashed/restored around prompts. A prompt withdrawn because it was answered elsewhere gets a one-line `(answered elsewhere)` note (`permDismissMsg`).
- **Config** (`permissions` in `~/.milk/config.json`): `background_timeout_secs` (360), `background_default` (`deny`), `safety_timeout_secs` (360), `safety_default` (`deny`); `safety_default: "allow"` warns at validation. **Session overrides** via `/permissions default allow|deny [safety]` and `/permissions timeout <secs> [safety]` (never persisted, like `/skip-permissions`).
- **Invariants kept**: `dangerously_skip_permissions` still auto-approves tool asks everywhere (including background) and still does **not** bypass the doom-loop gate; a remote decline is a real answer and wins over the timer; the remote side of asks stays serialized process-wide (`oversightPermMu` — Telegram has a single prompt slot).

## Alternatives considered

- **Fail closed as before (no third source)** — rejected: "nobody answered instantly" is not "no"; it silently discards work and hides the question from the only people who could answer it.
- **Remote-only asks for unattended contexts** (the interim shape after PR #217) — rejected as the *permanent* design: it made Telegram a single point of failure for every safety decision and still never asked the primary surface.
- **One shared default knob for tool and safety asks** — rejected: a single `background_default: "allow"` would auto-allow runaway-loop confirmations as a side effect.
- **Non-blocking `/permissions`-only answer path for background asks** — deferred: it would need stable ask IDs and an answering workflow of its own; the queue keeps one answering model everywhere, and its deadline bounds the input trap. Revisit if prompt bursts prove annoying in practice.
