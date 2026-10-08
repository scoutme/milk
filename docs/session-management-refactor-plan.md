# Session-management refactor — TUI, ACP and CLI parity (design only)

Status: **plan, ready for review — no implementation yet.** Baseline: `main` after
the ACP session-resume parity work (`docs/acp-session-resume-plan.md`, implemented),
verified against `cmd/milk/interactive.go`, `commands.go`, `completion.go`,
`acp_commands.go`, `acp_server.go`, `acp_session.go`, `acp_history.go`,
`main.go`, `internal/session/{session,store}.go`, `internal/transport/acp/sessions.go`,
`docs/{spec,acp-integration}.md`.

Goal (user request): (1) ACP needs session management as slash commands
(`/new`, `/drop`, `/clear`, …); (2) `/list` is ambiguous ("list *what*") — refine
the naming; (3) milk has no `/resume` command; (4) CLI resume is half-implemented.
Both the TUI and ACP channels get one refined session-management surface.

---

## 1. Verified baseline (what exists, what is missing)

### The command surfaces today

| Capability | TUI | ACP slash | ACP JSON-RPC | CLI |
|---|---|---|---|---|
| list sessions | `/list` (`execNonPromptCmd` → `listSessions`) | `/list` (viaTUI) | `session/list` | `milk --list [--all]` (`runList`) |
| new session | `/new`, `/clear` (alias) | **missing** ("only available in the milk TUI") | `session/new` | `milk --new` |
| drop session | `/drop` (current only) | **missing** | `session/delete` | `milk --drop` (current only) |
| resume/switch | **missing** | **missing** | `session/resume` | partial — see below |
| export | `/export [json\|<path>\|session <id>]` | same | — | **missing** |

### Concrete gaps and defects (the complaints, pinned to code)

1. **ACP slash session management is absent.** `acpCommandTable()`
   (acp_commands.go:44) contains only `"/list"` (viaTUI). `/new`, `/clear`,
   `/drop` are TUI strings handled by `execNonPromptCmd` (interactive.go:616);
   over ACP `runSlashCommand` answers *"is only available in the milk TUI"*.
   docs/acp-integration.md even lists `/new` as deliberately TUI-only.
2. **`/list` is ambiguous.** The name collides conceptually with `/memory`,
   `/tasks`, `/bg`, `/mcp list`, `/bash list`, `/agent list` — "list what?".
   Also `listSessions` (interactive.go:949) and `runList` (main.go:1706) are
   copy-pasted renderers (the TUI one even misses the trailing newline the CLI
   one has), and neither marks which entry is current.
3. **No `/resume` anywhere.** `session.Lookup(idOrPrefix)` exists
   (store.go:305, exact ID or unambiguous prefix) and `/export session <id>`
   already previews another session — but nothing can *attach* one in a running
   host. The previous plan listed "TUI in-process `/resume <id>` switch" as a
   non-goal/follow-up (acp-session-resume-plan.md §7) — this plan does it.
4. **CLI resume is not fully implemented.**
   - `--session <name>` matches **by name only**, find-or-create
     (`session.Resume(cwd, name)`, store.go:229). `milk --list` prints 8-char
     session IDs that **no flag or command accepts** — a dead end.
   - `--continue`/`-c` is a **dead flag**: `flagContinue` is written and never
     read anywhere (main.go:54/98).
   - `milk --drop` drops "the current session" = whichever is most recent,
     silently; `milk --drop --session foo` **creates** a session named `foo`
     (find-or-create in `runDrop`'s `session.Resume`) and then drops it.
   - No CLI equivalent of `/export` for another session (out of scope here,
     listed as follow-up).

Store primitives need no redesign: `New / Load / Save / Resume / Drop / List /
Lookup` cover everything; one addition (`Resolve`, below) is enough.

---

## 2. Design decisions

### D1 — One command vocabulary, shared by TUI and ACP

The lifecycle group under `── Sessions ──` becomes (flat commands, as requested):

| Command | Semantics |
|---|---|
| `/sessions [all]` | list sessions (cwd; `all` = every directory). `*` marks the current one; columns: id8, name, turns, last-used |
| `/new [name]` | start a fresh session (named optional). Previous session stays on disk, resumable |
| `/clear` | alias of `/new` (muscle memory; keeps today's meaning) |
| `/resume <id\|prefix\|name>` | attach the referenced stored session in the running host (the missing piece). Bare `/resume` = most recent for cwd; error if it is already current |
| `/drop [<id\|prefix>]` | delete the current (or referenced) session from disk, then land on a fresh one |

- **Rename `/list` → `/sessions`.** `/list` stays one release as a hidden
  deprecated alias of `/sessions` that appends a one-line hint
  (`/list is now /sessions`), then dies. It is removed from `/help` and from
  `available_commands_update` immediately (the table drives both).
- Refs resolve through one rule everywhere (D2): **exact ID → unambiguous ID
  prefix → exact name (cwd-scoped)**. Ambiguity and misses are errors, never a
  guess — the `memory.FindByIDPrefix` convention the codebase already follows.
- `/drop` gains an optional ref. `/drop` on the current session keeps today's
  "drop + fresh" behavior in both hosts; `/drop <ref>` on a non-current session
  deletes it and reports, without touching the current conversation.

### D2 — Host-neutral core: `cmd/milk/sessionmgmt_core.go`

Following the established `*_core.go` split (`workflow_core.go`,
`update_core.go`, `initwizard_core.go`, `configview.go`): one file with the
session-management operations + renderers, consumed by

- TUI: `handleSlashCommand`/`execNonPromptCmd` (interactive.go) + `commands.go`
  switch plumbing,
- ACP: `acpCommandTable` handlers (acp_commands.go),
- CLI: `run()`/`runList`/`runDrop` (main.go).

Shape (illustrative):

```go
type sessionOpResult struct {
    sess *session.Session // the session the host should bind after the op (may be unchanged)
    out  string           // rendered output
}
func renderSessionList(entries map[string][]session.IndexEntry, currentID string) string
func opSessionsList(st *interactiveState, all bool) string
func opSessionNew(st *interactiveState, name string) (*session.Session, string)
func opSessionResume(st *interactiveState, ref string) (*session.Session, string)
func opSessionDrop(st *interactiveState, ref string) (*session.Session, string) // returns the fresh session
```

`internal/session` gains one function:

```go
// Resolve finds a session by exact ID, unambiguous ID prefix, or exact name
// within cwd (name matches are cwd-scoped like Resume). Returns ErrNotFound /
// ErrAmbiguous; never guesses and never creates.
func Resolve(cwd, ref string) (*Session, error)
```

(`Lookup` stays for delete/export paths; `Resolve` adds name matching and a
typed not-found so `--new --session <name>` can create on top of it.)

The TUI↔ACP sharing seam is `interactiveState` — both hosts already run
`handleSlashCommand` against it (`viaTUI` in acp_commands.go), so the core
operates on `st` and stays host-agnostic. ACP-specific side effects (D4) live
in thin handlers, not in the core.

### D3 — TUI `/resume`: in-process switch

- Swap `st.sess = resolved`, then run the existing switch seam
  `refreshSessionScopedState` (commands.go:117 — history files, memory store,
  task store, prompt) — it already fires on ID change after any slash command.
- Transcript: **reseed** with the resumed session's bounded replay —
  `seedTranscriptFromHistory` (transcript.go:463) generalized from
  "startup only" (its `m.transcript.Len() > 0` guard) to "empty **or** reseeded
  view", with the same `resumed session <id8> (N turns)` banner. `/new`,
  `/clear`, `/drop` keep the transcript untouched (current documented
  behavior — unchanged).
- Busy rule: `/resume`, `/new`, `/drop` are **not** in `busySafeCommands`
  (state-changing); `/sessions` is (read-only).
- Tab completion (completion.go iterates `slashCommands`) gains `/sessions`,
  `/resume` with `<id|prefix|name>` completion candidates from `session.List`
  (ids + names); `/help`, `interactiveHelp`, and the `tab_test.go` pins update.

### D4 — ACP: conversation views that can rebind store sessions (the central decision)

Wire constraints (v2 schema, verified in `internal/transport/acp`):

- `session/resume`'s response carries **no sessionId** — the client keeps using
  the ID it requested as its handle.
- `session/new`'s response carries the ID milk returns — milk chooses the handle.

So a chat-typed `/new`/`/resume`/`/drop` cannot change the handle the client
holds. Two options: reject them over ACP ("use the client's session UI") —
rejected, it is exactly what the user asked for — or decouple *view* from
*store session*. Decision: **decouple**.

Model (to be recorded as **ADR-0051 — ACP conversation views can rebind store
sessions**):

- An `acpSession` is a **conversation view** identified by its wire handle.
  Invariant: *the handle equals the store session ID the view was created
  with* (true for both `session/new` and `session/resume` today — no wire
  change needed).
- The view has a **current binding** to one store session (`as.sess`).
  A store session is bound by at most one live view (keeps the existing
  "two clients can't bind one session ID" rule).
- Slash `/new`, `/resume`, `/drop` **rebind the view in place**:
  1. tear down / rebuild session-scoped state via `newACPSession`'s seam
     (`as.rebind(sess)` extracted from it — memory store, task store, runners'
     session context; `as.cfg`, `think`/`routing` config options and routing
     pins survive: they are view/user state, not session state),
  2. then **bounded history replay** of the new binding
     (acp_history.go, same bounds as resume) + one-line notice — adoption never
     hides state, so neither does rebind. `/drop`'s fresh binding replays
     nothing and says so.
- Resolution rules for `session/resume(X)` (priority order):
  1. a live view **has handle X** → rebind *that* view to store session X if
     it isn't already (this is "the conversation that started at X") and
     return it — idempotent reattach;
  2. X is the **current binding** of a live view V → return V and register X
     as an **alias handle** for it (dispatch, `session/cancel`, `session/close`
     accept every alias; `srv.sessions` maps alias → view);
  3. otherwise register a new view with handle X (today's path).
- `session/close(H)` closes the view behind H **and its current binding**
  (existing `as.close()` teardown); the binding's ID joins the closed-set
  (adopt suppression, D2 of the previous plan, unchanged).
- `session/delete(X)`: X is a view handle → close that view then `Drop`; X is
  merely the current binding of another view → `Drop` and rebind that view to a
  fresh session (announced). Unknown → `-32002` (unchanged).
- `/drop` over ACP = drop + rebind-to-fresh (parity with the TUI), the dropped
  ID joins the closed-set.

**Message identity wrinkle.** Replay IDs (`hist-u3`, …) are derived from the
history index and would collide across rebinds inside one client pane (upsert
would *patch* the wrong message). Fix: after a rebind, replay IDs are
session-qualified (`hist-<sess8>-u3`). The view's **first** binding keeps the
plain `hist-*` form, so already-replayed clients see no change (no duplicate
regression on upgrade). Documented deviation stands ("agents are not required
to retain messageIds"); the "Message identity" section in acp-integration.md
updates.

Rejected alternative (documented in the ADR): plain "swap `as.sess` without the
view model" — leaves `session/resume(<retired handle>)` resolving to a live
view bound to the wrong conversation, i.e. the exact silent-state bug the
resume-by-default work ruled out.

### D5 — CLI: finish resume

- **`--session <ref>` becomes full targeting** via `session.Resolve`: exact ID,
  unambiguous ID prefix, or exact name. Unknown ref:
  - with `--new`: create (named) — unchanged create path,
  - without `--new`: **error** (`no session matches %q — use --new --session
    <name> to create one`). *Deliberate behavior change:* today a typo'd
    `--session` silently creates a fresh session and "resumes" nothing; strict
    is the only honest reading of "resume". Called out in the changelog.
- **`milk --drop [--session <ref>]`** drops the *targeted* session (Resolve) —
  no more create-then-drop; without a ref it keeps dropping the most recent,
  but now **prints the full ID + name it dropped** instead of silently picking.
- **`--continue`/`-c`**: fixed as *documented no-op* — help text says
  "explicit alias of the default resume behavior" and the flag stays accepted;
  it is intentionally not removed (muscle memory) but stops implying
  functionality it doesn't have. (Alternative considered: `milk resume <ref>`
  subcommand — rejected: `--session <ref>` + `--new` already span the matrix,
  and a subcommand family would fork the surface a third way.)
- `milk --list` renders through the shared `renderSessionList` (kills the
  runList/listSessions duplication; gains the `*` current marker).

### D6 — Naming/deprecation summary

| Old | New | Compat |
|---|---|---|
| `/list` | `/sessions [all]` | `/list` hidden alias + hint, one release |
| (none) | `/resume <ref>` | — |
| `/drop` | `/drop [<ref>]` | superset |
| `/new`, `/clear` | unchanged semantics, now also over ACP | — |
| `--session <name>` | `--session <id\|prefix\|name>` | superset (unknown name stops silently creating) |
| `--drop` | `--drop [--session <ref>]` | superset |

---

## 3. Files & seams

| File | Change |
|---|---|
| `internal/session/store.go` (+tests) | `Resolve(cwd, ref)` with `ErrNotFound`/`ErrAmbiguous` (exact ID → unique prefix → exact name in cwd); no creation |
| `cmd/milk/sessionmgmt_core.go` **(new)** | `opSessionNew/Resume/Drop/List` + `renderSessionList` + ref-resolution error text; pure host-independent logic over `interactiveState`-shaped input |
| `cmd/milk/interactive.go` | `slashCommands` gains `/sessions`, `/resume`; `/list` alias; `execNonPromptCmd` cases move to the core; `seedTranscriptFromHistory` gains reseed mode (D3); `listSessions`/`loadSession` deleted (superseded) |
| `cmd/milk/commands.go` | `/resume`/`/drop <ref>` plumbing through `handleSlashInput` + `refreshSessionScopedState` (already fires) |
| `cmd/milk/completion.go` (+`tab_test.go`) | `/sessions`, `/resume` rows; ref completion candidates from `session.List`; hint pins updated |
| `cmd/milk/main.go` | `runList`/`runDrop`/`loadSessionForRun` → core + `session.Resolve`; `--session` strict semantics; `--continue` help text |
| `cmd/milk/acp_commands.go` | table rows: `/sessions`, `/new`, `/clear`, `/resume`, `/drop` (handlers in `acp_sessionmgmt.go`, below); `/list` row replaced; `acpCommandNames`/help ride along |
| `cmd/milk/acp_sessionmgmt.go` **(new)** | ACP handlers per D4: `as.rebind(sess)` (extracted from `newACPSession`), replay+notice on rebind, closed-set updates |
| `cmd/milk/acp_server.go` | `handleSessionResume` resolution rules (D4: handle-first, binding, alias registration); `handleSessionDelete` rebind rule; `sessions` map + alias handling; doc comments |
| `cmd/milk/acp_history.go` | session-qualified replay IDs after a rebind (`hist-<sess8>-*`) |
| `cmd/milk/transcript.go` | `seedTranscriptFromHistory` reseed support |
| `docs/adr/0051-*.md` **(new)** | D4 decision record |
| tests | see §4 |
| docs | see §5 |

Not touched: `internal/transport/acp` (no wire changes — the whole D4 trick is
that the vocabulary already supports it), `internal/agent/*`, `dispatch.go`,
the ACP resume-by-default adopt rule (`acp_resume` conditions 1–5), `Lookup`
consumers (`/export session`, `session/delete`).

## 4. Tests (pins, no network, `HOME=t.TempDir()` style)

`internal/session/store_test.go` (add):
- `Resolve`: exact ID wins; unique prefix; ambiguous prefix error names the
  count; name match is cwd-scoped; name shadowed by another session's ID prefix
  → ID resolution wins; miss → `ErrNotFound`; never creates.

`cmd/milk/sessionmgmt_core_test.go` (new):
- list renderer: `*` marker on current, `all` grouping by dir, empty case;
- new/resume/drop ops: state transitions (which session is bound after each),
  ref error texts, `/drop <non-current>` keeps the binding;
- `/list` alias emits the rename hint; `/clear` == `/new`.

`cmd/milk/session_switch_test.go` (extend):
- `/resume <prefix>` switches: `refreshSessionScopedState` effects visible
  (memory/task stores re-keyed), transcript reseeded with the resumed turns +
  banner; `/resume` on the current session errors; busy model rejects it.

`cmd/milk/acp_sessionmgmt_test.go` (extend):
- `/new` over ACP: handle unchanged, new binding, replay-notice text, dropped
  into no-replay for the fresh binding; old binding resumable afterwards;
- `session/resume(X)` where X is a retired handle → the handle-view rebinds to
  X (pin the priority order: handle > binding > new view);
- alias dispatch: resume of a session bound inside another view returns that
  view; `session/cancel`/`close` via alias;
- `session/delete` of a current binding → view rebinds to fresh + notice;
- replay ID qualification: first binding `hist-u3`, post-rebind
  `hist-<sess8>-u3`; re-replaying the first binding still patches (no dup);
- `/drop`/`/clear` advertised and executable (table driven), `/list` absent
  from `available_commands_update` but still executable with the hint.

`cmd/milk/main_cli_test.go` (new or extend existing CLI tests):
- `--session <id8>` / prefix / name resolution; unknown ref errors without
  `--new`; `--new --session foo` creates; `--drop --session <ref>` drops the
  target and never creates; `--drop` prints the dropped id+name.

## 5. Docs (with the PRs, not after)

- **`docs/acp-integration.md`**: slash table — `/sessions`, `/new`, `/clear`,
  `/resume`, `/drop` rows; `/list` row updated (alias); the TUI-only list drops
  `/new`; new subsection **"Session commands and view rebinding"** (D4 rules,
  handle invariant, alias semantics, closed-set interplay); "Message identity"
  gains the post-rebind qualification note.
- **`docs/spec.md`**: CLI flag table (`--session <id|prefix|name>` strict
  semantics, `--drop [--session <ref>]`, `--continue` = documented no-op);
  "ACP session parity" note gains the `/resume` ≈ `session/resume` line and the
  `/sessions` rename.
- **`docs/workflows.md`** (sessions paragraph, line ~130): `milk --session`
  accepts id/prefix/name.
- **`CLAUDE.md`**: structure entries (`sessionmgmt_core.go`,
  `acp_sessionmgmt.go`), one design-decision bullet (view rebinding).
- **`docs/adr/0051-…`**: D4 rationale + rejected alternative.
- **`docs/acp-session-resume-plan.md` §7**: the "TUI in-process `/resume`"
  non-goal marked done by this work (historical doc — one-line pointer only).

## 6. Phasing (branch-per-step, conventional commits)

1. `feat(session): Resolve by id/prefix/name + shared session-mgmt core`
   — `internal/session.Resolve`, `sessionmgmt_core.go`, renderers; CLI wired
   (`--session` strict, `--drop [--session]`, `--continue` help, `runList` via
   shared renderer). All green; docs/spec.md CLI rows land here.
2. `feat(tui): /sessions rename and /resume switch`
   — TUI rows, `/list` alias, reseeded transcript on switch, completion +
   help + pins. Docs ride along.
3. `feat(acp): session slash commands with view rebinding`
   — `acp_sessionmgmt.go`, `as.rebind`, server resolution rules + aliases,
   replay-ID qualification. ADR-0051 + acp-integration.md with this PR.
4. `docs: session-management cross-links` — only if anything is left over.

Each step: `gofmt` / `go build` / `go vet` / `go test ./...` green before PR;
small PRs per `docs/branching-strategy.md`.

## 7. Non-goals / follow-ups

- CLI `milk --export [session <ref>]` (one-shot export outside the TUI).
- Session rename (`/rename`) and multi-conversation-per-cwd — the
  one-conversation-per-cwd model stays.
- Keyset pagination for `session/list`; `SessionInfoUpdate` titling (unchanged
  from the previous plan's follow-ups).
- Removing the `/list` alias and the `--continue` flag (kept until the release
  after this one).
- v1 `session/load`, `auth/*`, `additionalDirectories` (unchanged gaps).

## 8. Self-review / risks

- **D5 strict `--session`** is a real behavior change (typo'd names used to
  silently create). Mitigation: explicit error message with the create recipe;
  changelog entry. Alternative (find-or-create kept) makes "resume" lie.
- **D4 complexity** is concentrated in `handleSessionResume`'s three-way
  resolution and the alias map. Mitigation: the priority order is small and
  fully pinned by tests; the ADR records the rejected simpler variant and why
  it loses. Wire layer untouched — no spec risk.
- **Replay-ID qualification** must not re-key already-replayed panes: first
  binding keeps plain `hist-*` (pinned by a regression test).
- **Rebind while a turn is running**: slash commands execute inside the turn
  loop under `turnMu` on ACP; in the TUI they are not busy-safe. No new
  concurrency; `as.rebind` runs at the same points `newACPSession` does today.
- `/sessions` list output stays cwd-scoped by default (privacy + relevance);
  `all` is explicit, mirroring `milk --list --all`.
