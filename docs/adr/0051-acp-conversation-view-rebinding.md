# 51. ACP Conversation Views Can Rebind Store Sessions

- **Status:** accepted
- **Date:** 2026-10-09
- **Issue:** Session-management refactor — plan D4
  ([docs/session-management-refactor-plan.md](../session-management-refactor-plan.md)):
  make `/new`, `/resume` and `/drop` work in a chat typed into an ACP client,
  not just in the milk TUI.

## Context

ACP's wire vocabulary fixes a conversation's **handle**: `session/resume`'s
response carries no `sessionId` (the client keeps the ID it asked with), and
only `session/new` returns a fresh one. A chat-typed `/new` or `/resume` can
therefore never change the handle the client holds — the pane keeps calling
`session/prompt` and `session/cancel` with the ID it was given at thread
creation.

Two ways to give ACP clients in-chat session management anyway:

1. **Refuse it over ACP** — "use the client's own session UI". Rejected: ACP
   channel session management is exactly what the request asked for, and many
   clients have no session UI beyond the four JSON-RPC methods.
2. **Decouple the conversation from the store session** — the view keeps its
   handle, the thing it is *looking at* moves. Chosen.

## Decision

An `acpSession` is a **conversation view** identified by its wire handle,
holding a **current binding** to one store session (`as.sess`).

- **Handle invariant.** A view's handle equals the store session ID it was
  created with (true for both `session/new` and `session/resume` today — no
  wire change). A store session is bound by **at most one live view** (keeps
  the "two clients can't bind one session ID" rule).
- **Slash rebind.** `/new`, `/resume` and `/drop` rebind the view in place
  (`as.rebind`, `cmd/milk/acp_sessionmgmt.go`): session-scoped state is torn
  down and rebuilt through the seams `newACPSession` uses — memory store,
  task store, the runners' session context — while view/user state survives
  (`think`/`routing` config options, routing pins, `skipPerms`, client
  capabilities, the background-job manager with its state file re-pointed).
- **Switching never hides state.** Every rebind replays the new binding's
  retained history (bounded, like adoption and `session/resume`) with a
  one-line notice naming it; a fresh binding replays nothing and the op
  report says so.
- **`session/resume(X)` resolution**, in priority order:
  1. X is the **current binding** of a live view V → return V and register X
     as an **alias handle** for it (dispatch, `session/cancel`,
     `session/close` accept every alias; `srv.sessions` maps alias → view);
  2. a live view **has handle X** → rebind that view back to store session X
     ("the conversation that started at X") and return it — the idempotent
     reattach, and the way back after the view `/new`-ed away;
  3. otherwise register a new view with handle X (the classic path).

  Rule 1 before rule 2 is a deliberate refinement of the plan's "handle >
  binding" wording (which assumed the two never disagree): in the one
  collision case where a handle view and a bound view are different views,
  the one-view-per-binding invariant outranks handle identity — otherwise a
  reattach could split-write one store session from two panes. In every
  non-pathological case the order is unobservable.
- **`session/close(H)`** closes the view behind H **and its current
  binding**: every handle key pointing at the view is removed and joins the
  closed-set, as does the binding's ID (resume-by-default adoption
  suppression, unchanged).
- **`session/delete(X)`**: X is a view handle → close that view, then `Drop`;
  X is merely the current binding of a surviving view → `Drop` and rebind
  that view to a fresh session (announced). Unknown → `-32002` (unchanged).
  `/drop` over ACP mirrors it: drop + rebind-to-fresh, dropped ID into the
  closed-set.

**Message identity after a rebind.** Replay IDs (`hist-u3`, …) are derived
from the history index and would collide across rebinds inside one client
pane — an upsert would *patch* the wrong message. The view's **first**
binding keeps the plain `hist-*` form (already-replayed panes keep patching,
no duplicate regression on upgrade); after any rebind the IDs are
session-qualified (`hist-<sess8>-u3`), and return to plain if the view
rebinds back to its first session.

## Rejected alternatives

- **Plain `as.sess` swap without the view model** — leaves
  `session/resume(<retired handle>)` resolving to a live view bound to the
  wrong conversation: the exact silent-state bug the resume-by-default work
  ruled out. The alias map and the resolution rules are the price of not
  having it.
- **Refusing slash session switching over ACP** — rejected above; it is the
  feature.

## Consequences

- No wire-layer changes (`internal/transport/acp` untouched): the existing
  vocabulary already expresses everything, which is the point of the design.
- A client sees freshly-ID'd messages after a switch (and after a switch
  back, the original IDs again). Documented deviation stands — "agents are
  not required to retain `messageId`s"; `docs/acp-integration.md`'s "Message
  identity" section carries the note.
- `docs/acp-integration.md`'s "Session commands and view rebinding"
  subsection is the user-facing spec of these rules.
