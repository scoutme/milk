# ACP session-resume parity — implementation plan (design only)

Status: **plan, reviewed — no implementation yet.** Baseline: `main` @ `a69c886`
(PR #196 merged), verified against `cmd/milk/acp_server.go`, `acp_session.go`,
`acp_commands.go`, `interactive.go`, `commands.go`, `internal/session/*`,
`internal/transport/acp/*`, and the upstream ACP v2 schema
(`schema/v2/schema.json`, fetched copy at `/tmp/acp-schema.json`).

Goal (user request): (1) resume feature parity over ACP — resume by default plus
session management commands, with an `export` command so previous turns can be
explored after resume; (2) implement ACP's standard chat-history mechanism if one
exists.

**Answer to (2) up front: yes, ACP v2 standardizes it.** `session/resume` carries
an optional `replayFrom` cursor ("Omitted or `null` both mean the Agent should
resume without replaying previous conversation history… `{"type":"start"}` means
the Agent should replay all retained conversation history before responding"),
and history is delivered as `session/update` **message upserts**
(`user_message` / `agent_message` / `agent_thought`, stable `messageId`, patch
semantics). `PromptResponse.messageId` docs confirm the identity rule: "If
retained and replayed, the message keeps this identifier." The legacy v1
mechanism (`session/load`, client-supplied history) is the old shape — out of
scope (see Non-goals).

---

## 1. Verified baseline

Schema (v2) standardizes exactly the methods the user asked for, all `x-side:
agent`:

| Method | Request/Response | Notes from schema |
|---|---|---|
| `session/list` | `ListSessionsRequest{cwd?, cursor?}` / `ListSessionsResponse{sessions: []SessionInfo, nextCursor?}` | `SessionInfo{sessionId, cwd, additionalDirectories?, title?, updatedAt?}`; opaque `SessionListCursor` |
| `session/resume` | `ResumeSessionRequest{sessionId, cwd, additionalDirectories?, mcpServers?, replayFrom?}` / `ResumeSessionResponse{configOptions?, availableCommands?}` | `replayFrom: ReplayFrom` = `{"type":"start"}` \| future/custom cursor types; "replay includes the position identified by the cursor" |
| `session/close` | `CloseSessionRequest{sessionId}` / `{}` | "must cancel any ongoing work (treat as `session/cancel`) and then free up any resources" |
| `session/delete` | `DeleteSessionRequest{sessionId}` / `{}` | "Only available if the Agent supports the `session.delete` capability" (`SessionCapabilities.delete: {}`) |

`SessionCapabilities{}` (baseline) already covers new/list/resume/close/prompt/
cancel/update — milk's existing capability advertisement stays honest the moment
the four methods are wired (today they answer `-32601`; the lifecycle.go package
doc's scope note gets outdated — see Docs). `session/delete` additionally needs
the `delete` capability field added to `SessionCapabilities`.

Code today:

- `handleSessionNew` (acp_server.go:131) → `session.New(req.CWD, "")` unconditionally.
  `HandleRequest` wires only initialize/session/new/session/prompt; the rest →
  `MethodNotFoundError` (acp_server.go:63–70).
- TUI/CLI default is resume: `loadSession` → `session.Resume(cwd, hint)` =
  most-recent-for-cwd or new (store.go:228–251).
- Store primitives already cover the needed CRUD: `New`, `Load(id)`, `Save`
  (bumps `LastUsed` + index), `Resume`, `Drop(id, cwd)`, `List(cwd)` (index
  sorted `LastUsed` desc + `repairIndex`), `ExportJSON/Text/TextColorized`.
- `/export` and `/list` **already work over ACP** (acp_commands.go rows, `viaTUI`
  → `handleSlashCommand`, ANSI stripped at acp_commands.go:219/222). `execExport`
  (interactive.go:728) = text (colorized to TUI, plain to file) / `json` / `<path>`.
- `newACPSession` (acp_session.go) already rebuilds *everything* session-scoped
  (memory store, task store, background-jobs state file, runners) around a
  `*session.Session` — the perfect seam for "attach an existing session".
- One-way cross-pollination exists (TUI resumes ACP sessions via the index).
- Live message IDs are per-process counters (`msg-%d`, `thought-%d`, `perm-%d`
  over `as.msgCounter`) — they collide across restarts and can't be replayed.

## 2. Design decisions

### D1 — `session/resume`: exact semantics (standard-conformant)

- Load by `sessionId` via `session.Load`; **`cwd` must equal the stored
  session's cwd** (the schema conditions everything on "the request `cwd`
  matches the session's `cwd`") → mismatch = `-32602 invalid params`.
- Unknown `sessionId` → `-32002 resource not found` (needs coded-error plumbing,
  see §3.1 — today every non-MethodNotFound error maps to `-32000`).
- If the target is already attached to a live `acpSession` in this process
  (client re-issued resume): **idempotent reattach** — return the live session,
  don't rebuild (a rebuild would clobber a possibly-running turn). `replayFrom`
  is still honored (replay is a pure read of `sess.History`).
- Replay timing: emit the replay `session/update` notifications **before writing
  the response** ("replay … before responding"), then return
  `ResumeSessionResponse{availableCommands: acpAdvertisedCommands()}`.
  `AfterResponse` (which today only handles `session/new`) is extended to also
  fire the update-available notice for `session/resume`.
- `replayFrom` handling (schema-faithful): `null`/omitted → **no replay**;
  `{"type":"start"}` → replay all retained history; any other cursor type
  (including `_`-prefixed extensions we don't implement) → **reject with
  `-32602`** ("otherwise reject the request rather than guessing where to
  replay from") before emitting anything.

### D2 — Resume-by-default: `session/new` adopts, precisely

Decision: keep `session/new` as the entry point that clients actually call, and
give it the TUI's `--continue` default — **adopt** an existing session when it's
clearly the continuation case. Full rule (all five conditions):

> `session/new(req)` adopts the most-recent stored session for `req.cwd`
> (equivalent to `session.Resume(req.cwd, "")`) **iff**:
> 1. config `acp_resume` is on (**default: on**),
> 2. the request's `_meta.milk.fresh` is not `true` (extensibility escape hatch
>    on `NewSessionRequest._meta` — `_meta` is ACP's sanctioned channel for
>    exactly this),
> 3. this serve process has **no live `acpSession` whose cwd == `req.cwd`**,
> 4. the candidate session was **not closed earlier in this process** (tracked
>    set of closed IDs), and
> 5. the store has ≥1 session for `req.cwd`.
>
> Otherwise it behaves exactly as today (`session.New`).

On adopt: rebuild via the shared register helper (§3.2), **always replay from the
start** (bounded — §D5; the client's panel is empty, and adopt has no
`replayFrom` to consult), then send a one-line out-of-turn notice:
`resumed session <id8> — N earlier turns; /export prints the full transcript`.
The response returns the **adopted session's ID** (`NewSessionResponse`), so the
client binds the conversation to the conversation that actually continues.

Why adopt-on-new is defensible against the spec's intent:

- The contract the client depends on is "the ID to use for all subsequent
  requests for this conversation" — honored: the response always names the
  conversation the client will continue. Crucially, **the client's view is never
  inconsistent with agent state**: full replay means no hidden context (the
  usual objection to session hijacking).
- milk's session model is one-conversation-per-cwd (that *is* the TUI product
  behavior being asked about); "new" reads as "a new conversation view", which
  for milk continues the thread — the same semantics as launching `milk` in a
  terminal. The strict reading ("new = empty context") stays one config flip
  away (`acp_resume: false`) and one `_meta` key away per request.
- Conditions 3+4 preserve ordinary multi-conversation behavior inside a running
  client: the second `session/new` for the same cwd is **fresh** ("new thread"
  works), and a thread the user explicitly closed this session won't silently
  resurrect on the next "new thread". The only changed case is the one the user
  asked for: **first open of a workspace ≈ relaunching milk ≈ resume**.

Tradeoffs, stated honestly (reviewable):
- A spec-strict client that always expects `session/new` to be empty will see
  old context once per workspace open (with the notice + replay, so not
  silently). Mitigations above.
- An editor restoring several named threads should use `session/list` +
  `session/resume` (it knows its sessionIds); its *first bare* `session/new`
  adopts only when nothing is live for that cwd yet.

### D3 — `session/close` and `session/delete`

- `session/close`: per schema — cancel any running turn (`as.cancel()`, i.e. the
  same path as `session/cancel`), drop pending background follow-ups (a
  `closed` flag gates `flushPendingFollowup`/drain announcements), persist the
  session file (`session.Save`), remove it from `acpServer.sessions`, and record
  the ID in the process's closed-set (D2.4). **Idempotent**: closing an unknown /
  already-closed session returns `{}` (nothing to free). The file is kept —
  close ≠ delete.
- `session/delete`: advertise `SessionCapabilities.Delete: &SessionDeleteCapabilities{}`
  (schema: "Supplying `{}` means the agent supports deleting sessions from
  `session/list`"). Handler: close-if-open (same teardown), then
  `session.Drop(id, cwd)` (cwd from the stored session). Unknown ID →
  `-32002`. Dropping updates `index.json` under the existing `indexMu` — no new
  store concurrency surface.

### D4 — `session/list`

- Source: `session.List("")` + `repairIndex` (already drops entries whose file
  vanished), flattened and sorted `LastUsed` desc.
- `SessionInfo` mapping: `sessionId` = ID; `cwd`; `title` = `Session.Name`, else
  the first user turn truncated (~60 chars), else `""`; `updatedAt` =
  `LastUsed` (RFC 3339). `additionalDirectories` omitted (milk has none).
- `cwd` param: filter when absolute path given; omitted/null lists all cwds
  (schema-legal).
- Cursor: opaque offset cursor (`base64("o:<n>")`), page size 50,
  `nextCursor` absent on the last page. Enough for human session pickers;
  no keyset pagination needed at this scale.

### D5 — Chat-history replay (the ACP standard)

- **Wire vehicle (v2 clients)**: message **upserts** — `user_message` /
  `agent_message` / `agent_thought` (`{messageId, content:[TextBlock]}`), the
  vocabulary the schema explicitly designates for replay ("Agents can send this
  when they accept or replay a user message"), plus `tool_call_update` rows
  (already implemented vocabulary) for tool trails.
- **v1 clients** (`v1Client`, `clientProtocol == 1`): chunk-form fallback —
  `user_message_chunk` / `agent_message_chunk` / `agent_thought_chunk` via the
  existing `ContentChunk` type (one new `UserMessageChunk` constructor). Chunks
  with the same `messageId` append, so the client assembles the same messages.
- **Mapping** (`sess.History`, append-only):
  - `RoleUser` turn → `user_message` `hist-u<i>`
  - `RoleAssistant` → `agent_message` `hist-a<i>` (content = `t.Content`), plus
    `agent_thought` `hist-think<i>` when `t.Thinking != ""` and thinking is
    visible
  - `t.ToolCalls` → `tool_call_update` `hist-t<i>-<j>` status completed,
    `rawInput` from `Arguments`, `rawOutput` paired by `ToolCall.ID` from the
    nearest following `RoleToolResult` turn (kept as text in `rawOutput`)
- **Identity**: replay IDs are deterministic (`hist-…` by history index), so a
  second replay **patches** instead of duplicating (upsert semantics). Live IDs
  become collision-free across restarts: `msg-<run6>-<n>` etc., where `<run6>` is
  a random per-`acpSession` suffix — otherwise a post-resume live `msg-1` could
  patch over a pre-restart `msg-1` the client still holds. Deviation noted in
  docs: milk does not retain original ACP messageIds in `session.Turn` ("Agents
  … are not required to retain it"), so replayed messages carry synthesized
  stable IDs. (Optional follow-up: persist `Turn.MessageID`.)
- **Bounds** ("all *retained* history" gives license): per-message content
  capped via `internal/textbudget.SummarizeLong` (~8KB), whole replay windowed
  head+tail over turns (first 10 + last 200), with one explicit marker message
  in the gap: `[… N earlier turns omitted from replay — /export prints the full
  transcript …]`. The export command is the full-fidelity escape hatch (D6).

### D6 — Export

- Core exists and is shared per house pattern already: `internal/session/export.go`
  (ExportJSON/ExportText/ExportTextColorized) + `execExport` reachable from both
  hosts (`viaTUI` row in `acpCommandTable`; ANSI stripped over ACP). On a
  resumed session `/export` naturally includes pre-resume turns.
- **Extension for the "explore previous turns" flow**: `/export session
  <id|prefix>` — dump *another* session without attaching it. Full grammar:
  `/export [session <id|prefix>] [json|<path>]`. Powers
  `/list` → `/export session a1b2` → (decide) → `session/resume`.
  Implementation: arg parsing in `execExport` (both hosts), target resolution
  via new `session.Lookup(idOrPrefix)` (index scan by prefix, error on
  ambiguity/miss — mirrors `memory.FindByIDPrefix` conventions).
- No new `_core.go` needed: the export surface is already core-split
  (`internal/session/export.go` is the core; `execExport` is host-shared through
  `handleSlashCommand`). Update the `cmdExport` hint row +
  `interactiveHelp` + tab-completion pin (`tab_test.go`) for the new form.

## 3. Files & seams

| File | Change |
|---|---|
| `internal/transport/acp/sessions.go` **(new)** | Wire vocabulary for `session/list|resume|close|delete`: `ListSessionsRequest/Response`, `SessionInfo`, `SessionListCursor`, `ResumeSessionRequest/Response`, `ReplayFrom` (+ `ReplayStart()` ctor and `ParseReplayFrom` → error on unknown cursor types), `CloseSessionRequest/Response`, `DeleteSessionRequest/Response`, `SessionDeleteCapabilities`; `SessionCapabilities.Delete` field; message upserts `MessageUpsert{sessionUpdate, messageId, content}` + `UserMessageUpsert/AgentMessageUpsert/AgentThoughtUpsert` ctors and `UserMessageChunk` ctor. Shapes pinned to the v2 schema like `TestLifecycleWireShapes` does. |
| `internal/transport/acp/stdio.go` | `CodedError{Code, Message}` (or `interface{ RPCCode() int }`) honored by `handleRequest` (today only `MethodNotFoundError` gets a deliberate code; everything else → `-32000`). Update `MethodNotFoundError`/`lifecycle.go` package-doc scope notes (they name session/list\|resume\|close as "not implemented yet"). |
| `internal/transport/acp/lifecycle.go` | `NewSessionRequest.Meta map[string]any` (`_meta`), `InitializeResponse` unchanged. |
| `internal/session/store.go` (small) | `Lookup(idOrPrefix) (*Session, error)` for `/export session …` and delete-by-prefix consistency. Everything else (Resume/Load/List/Drop/Save) already fits. |
| `cmd/milk/acp_server.go` | Wire the 4 methods in `HandleRequest`; extract `registerACPSession(cfg, sess) (*acpSession, error)` from `handleSessionNew`'s tail (config re-read + `newACPSession` + `srv`/`v1Client`/`formElicit` wiring) shared by new/resume; D2 adopt logic in `handleSessionNew` (conditions 1–5, closed-set, live-cwd scan over `s.sessions`); `handleSessionList/Resume/Close/Delete`; `AfterResponse` extended to `session/resume` (commands + update notice); doc comments updated. |
| `cmd/milk/acp_session.go` | `as.close()` teardown (cancel turn, `closed` flag, persist); runID-suffixed message IDs; live `user_message` upsert echo at turn start (same `messageId` as `PromptResponse` — the schema's echo rule); `replayHistory` call seams. |
| `cmd/milk/acp_history.go` **(new)** | `replayHistory(as, req)` — history → upserts/chunks per D5, deterministic IDs, textbudget bounds + omitted-range marker. |
| `cmd/milk/interactive.go` | `execExport` gains `session <id|prefix>` targeting; `listSessions` newline fix if confirmed. |
| `internal/config/config.go` | `ACPResume *bool \`json:"acp_resume,omitempty"\`` (default-true merge like `UpdateCheck`), + merge-list entry. |
| tests | see §4 |
| docs | see §5 |

Not touched: `internal/agent/local` (PR #196 territory), `dispatch.go`,
`commands.go` (`refreshSessionScopedState` is the TUI model's hook — the ACP
path gets the same effect by rebuilding `acpSession` via `newACPSession`).

## 4. Tests (all pins, no network, `HOME=t.TempDir()` style)

`internal/transport/acp/sessions_test.go` (new):
- wire-shape pins (marshal) for the 8 request/response types + `SessionInfo`;
  `SessionCapabilities` with `delete:{}`;
- `ParseReplayFrom`: start / null / `{"type":"_x"}` / `{"type":"later"}` →
  last two reject;
- `CodedError` → exact JSON-RPC code through `handleRequest`.

`cmd/milk/acp_sessionmgmt_test.go` (new):
- `session/list`: entries sorted by LastUsed desc, cwd filter, title/updatedAt
  mapping, cursor page-through (50+2 → 2 pages, second `nextCursor` absent);
- `session/resume`: happy path (registers `acpSession`, history intact), cwd
  mismatch → `-32602`, unknown ID → `-32002`, already-open target → same
  instance reused;
- `replayFrom` semantics: null → zero updates; `{"type":"start"}` → full replay
  **emitted before the response** (pin ordering via recording conn);
  unknown cursor → `-32602` and zero updates;
- replay identity: two resumes of the same session emit identical messageIds
  (patch, not duplicate); live IDs after resume don't collide with replayed or
  pre-restart IDs;
- replay bounds: oversized history → head+tail + marker message referencing
  `/export`;
- `session/close`: cancels a running turn (pin via fake runner + cancel),
  removes from map, session file persists, second close still `{}`,
  follow-up suppressed after close;
- `session/delete`: file + index entry gone; open target is closed first;
  capability advertised; unknown → `-32002`.

`cmd/milk/acp_resume_default_test.go` (new):
- first `session/new` in a cwd with stored history adopts (response ID = old ID,
  replay emitted, notice sent); second `session/new` in-process is fresh;
- `_meta.milk.fresh` → fresh; `acp_resume:false` → fresh;
- session closed this process is not re-adopted (falls through to fresh);
- adopt skips candidates live in-process (two rapid `session/new`s → distinct IDs).

`cmd/milk/export_session_test.go` (new):
- `/export session <prefix>` renders the right session (both hosts),
  `json`/`<path>` compose with `session <id>`, ambiguity/miss errors,
  resumed session's `/export` includes pre-resume turns;
- `tab_test.go` pin updated for the new hint.

## 5. Docs (per the docs rule — with the PR, not after)

- **`docs/acp-integration.md`**: four ✅ rows in "What's implemented" +
  `user_message`/`agent_message` upsert rows; new section **"Session resume,
  list, and history replay"** (D1/D2 semantics table: default adopt rule, the
  five conditions, `_meta.milk.fresh`, `acp_resume`, `replayFrom` handling and
  rejection rules, replay bounds, message-identity deviation); `/export` slash
  row (`[json|<path>|session <id>]`); "Error handling" gains `-32002`/`-32602`;
  "Known gaps" drops the four methods (leaves `auth/*`,
  `session/set_config_option`, v1 `session/load`, `mcpServers` merge, …) and the
  example exchange's `session/list → -32601` demo is replaced with a real
  `session/list` + `session/resume` exchange.
- **`docs/spec.md`**: `acp_resume` config field (default `true`) in the
  Configuration section + example JSON; a short "ACP session parity" note
  (TUI `--continue` ≈ ACP `session/new` adopt; `--new` ≈ `acp_resume:false` /
  `_meta.milk.fresh`).
- **`CLAUDE.md`**: structure entries for `acp_history.go` / `acp.acp sessions.go`;
  one design-decision bullet (resume parity + standard replay).
- **`docs/machine-readable-output-design.md`** Phase-2 status note (comments in
  `acp_server.go` point at it): deferred list updated the same way.
- lifecycle.go / stdio.go package-doc scope notes updated (they currently
  justify the capability advertisement *because* list/resume/close are missing).

## 6. Phasing (branch-per-step, conventional commits)

1. `feat(acp): session lifecycle wire types and coded errors` — §3 rows 1–3
   only (pure vocabulary + transport error codes), all green.
2. `feat(acp): session/list, session/resume, session/close, session/delete` —
   handlers without replay (`replayFrom` accepted but only `null`/`start` with
   empty→full replay stub), plus `session.Lookup`, delete capability.
3. `feat(acp): standard history replay on session/resume` — `acp_history.go`,
   upsert/chunk vocabulary use, message-identity fixes, live `user_message` echo.
4. `feat(acp): resume-by-default session/new and /export session targeting` —
   D2 + D6 + `acp_resume` config.
5. Docs ride with each step; final `docs(acp)` pass if anything is left.

Each step: `gofmt`/`go build`/`go vet`/`go test ./...` green before PR; PRs small
per the branching strategy.

## 7. Non-goals / explicit follow-ups

- **v1 `session/load`** (client-supplied history): wrong shape (history lives
  client-side there); stays `-32601`, documented as a gap with rationale.
- Retaining original ACP `messageId`s in `session.Turn` (nice-to-have;
  synthesized stable IDs are conformant — "not required to retain").
- TUI in-process `/resume <id>` switch — **done** by the session-management
  refactor (step 2 of `docs/session-management-refactor-plan.md`); ACP got
  parity via view rebinding (ADR-0051).
- `session/set_config_option`, `auth/*`, `additionalDirectories`, `mcpServers`
  merge — unchanged gaps.
- Keyset pagination for `session/list`; `SessionInfoUpdate`-based titling.

## 8. Self-review / risks

- **Spec strictness of D2** is the one judgment call. It is opt-out at three
  levels (config, per-request `_meta`, in-process guards) and never hides state
  (mandatory replay + notice). If review prefers strictness-by-default, only the
  default of `acp_resume` flips — the rest of the plan is unchanged.
- Replay volume bounded (textbudget + window) — no unbounded notification blast
  on a long session.
- Concurrency: `acpServer.mu` covers sessions map + closed-set; `indexMu`
  already covers the index; `as.close()` cancels before unmapping so no orphan
  running turn; replay is a pure function of `sess.History` snapshot
  (`session.Clone`-like copy at load time).
- Adopt-on-new never runs while a live session exists for the cwd, so two
  clients/panels can't bind one session ID; `session/resume` of a live target is
  idempotent rather than a second binding.
