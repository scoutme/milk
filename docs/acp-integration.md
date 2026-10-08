# Integrating with milk via ACP

`milk serve --acp` runs milk as a long-lived [Agent Client
Protocol](https://agentclientprotocol.com) v2 agent: an editor (or any other
client) spawns it as a subprocess and speaks JSON-RPC 2.0 over its
stdin/stdout. This doc describes **exactly what's implemented today** —
separately from [docs/machine-readable-output-design.md](machine-readable-output-design.md),
which is the full design proposal covering the complete planned surface, most
of which isn't built yet. If you're integrating an editor or client against
milk right now, this page — not the design doc — is the contract to code
against. Where the two disagree, this page wins; the design doc's own status
notes track what's landed phase by phase.

## Launching it

```
milk serve --acp
```

- Speaks newline-delimited JSON-RPC 2.0: one JSON object per line, on both
  stdin (client → agent) and stdout (agent → client).
- **stdout carries protocol traffic only.** Human-readable warnings (e.g. a
  stale config recovery notice) go to stderr, never stdout — never try to
  parse stdout as anything but JSON-RPC lines.
- The process keeps running until stdin closes or it receives `SIGINT`/`SIGTERM`.
- One process serves **multiple sessions**: `session/new` can be called more
  than once, and each session's `session/prompt` call runs independently —
  a long-running turn in one session never blocks another session's calls.

## What's implemented

| Method | Direction | Status |
|---|---|---|
| `initialize` | client → agent | ✅ real |
| `session/new` | client → agent | ✅ real: starts a fresh session — or, by default, **adopts** the cwd's most recent stored session (resume-by-default; see "Session resume, list, and history replay" below) |
| `session/list` | client → agent | ✅ real: every stored session sorted by last use descending (all cwds, or `cwd` filter), `SessionInfo{sessionId, cwd, title, updatedAt}` (title = session name, else the first user turn truncated), opaque `base64("o:<n>")` offset cursor, 50 per page |
| `session/resume` | client → agent | ✅ real: attach the stored session (the request `cwd` must match the session's, else `-32602`; unknown ID → `-32002`), idempotent reattach when it is already open in this process; `replayFrom` — omitted/`null` resumes without replaying, `{"type":"start"}` replays all retained history before the response (message upserts, see "Session resume, list, and history replay" below), any other cursor → `-32602` before anything is emitted; returns `availableCommands` |
| `session/close` | client → agent | ✅ real: cancel any running turn + suppress follow-ups + persist + free (idempotent, the file is kept — close ≠ delete) |
| `session/delete` | client → agent | ✅ real, behind the `session.delete` capability (`{}`): close-if-open, then drop the session file and index entry (accepts an unambiguous ID prefix) |
| `session/prompt` | client → agent | ✅ real |
| `session/cancel` | client → agent (notification) | ✅ real |
| `session/update` (`state_update`) | agent → client | ✅ real: `running` at turn start, `idle` at turn end |
| `session/update` (`available_commands_update`) | agent → client | ✅ real, sent once right after the `session/new` response. Advertises exactly the commands listed under "Slash commands" below (no per-argument granularity; the list itself is static for the process, while the config it may edit is re-read from disk at `session/new` — see "The setup wizard" below) |
| `session/update` (`agent_message_chunk`) | agent → client | ✅ real: turn output as **one chunk per completed turn, not per token** (see caveat below), plus occasional out-of-turn notices — the background-follow-up and update-available announcements |
| `session/update` (`user_message` / `agent_message` / `agent_thought` upserts) | agent → client | ✅ real: chat-history replay on `session/resume` (and resume-by-default adoption) plus the accept-echo of each user message at turn start (same `messageId` the `session/prompt` response returns). Upsert semantics: the same `messageId` patches rather than duplicates |
| `session/update` (`user_message_chunk`) | agent → client | ✅ real: the v1-client fallback shape of the accept-echo and replay (v1 has no message upserts; same `messageId` appends into one message) |
| `session/update` (`tool_call_update`) | agent → client | ✅ real, for both the local-provider and claude-cli-escalation paths |
| `session/request_permission` | agent → client | ✅ real. Tool approvals are **local-provider agents only** — see caveat below. The setup wizard also uses it as clickable choice prompts for **any** provider (see "The setup wizard" below) |
| `elicitation/create` | agent → client | ✅ real: sent by the setup wizard's form dialogs (form mode, session scope) |
| `elicitation/complete` | agent → client (notification) | ✅ real: fire-and-forget once the user's answer has been consumed |

Everything else a real ACP client might try — `auth/login`, `auth/logout`,
`session/set_config_option`, the legacy v1 `session/load` (client-supplied
history — the old wrong shape; history replay is standardized via
`session/resume`'s `replayFrom` instead), `$/cancel_request` as a
standalone per-request cancel — returns the standard JSON-RPC **`-32601`
method not found** error. That's a deliberate, documented gap, not a bug: see
[Deferred](#deferred-not-silently-missing) below.

## Slash commands

A `session/prompt` whose text starts with a known slash command is executed by
milk itself rather than sent to the model. Output comes back as one
`agent_message_chunk` (ANSI stripped) between the usual `running`/`idle`
`state_update`s. The advertised list and the executable list come from one
table (`cmd/milk/acp_commands.go`), so nothing is advertised that doesn't run.

| Command | Effect |
|---|---|
| `/escalate [fresh] [<msg>]` | pin all turns to the escalation agent; with a message, force that one turn |
| `/primary [<msg>]` | pin all turns to the primary agent; with a message, force that one turn |
| `/learn <fact>` | store a persistent memory |
| `/memory [global\|session\|<pattern>]` | list percepts |
| `/usage`, `/metrics` | token usage / recent metrics |
| `/export [json\|<path>]` | print the transcript, or write it to a file |
| `/list` | list sessions for the working directory |
| `/skip-permissions [on\|off]` | approve every tool call without asking (seeded from `dangerously_skip_permissions`, like the TUI) |
| `/think [on\|off]` | show or hide `agent_thought_chunk` updates for this session (default on, as before, unless `show_reasoning` is set to false) |
| `/config` | print the merged config as fenced JSON |
| `/config show` | the same, annotated per field `[global]`/`[local]`/`[default]` |
| `/config open` | open the config file: launches the platform file opener (`xdg-open` / `open` / `start`) detached on the machine milk runs on and reports exactly what happened — an ACP agent is headless and never launches an interactive editor itself; when no opener is available it says so and falls back to the config path(s) for the client's editor |
| `/config init`, `/init` | run the interactive setup wizard (see below) |
| `/agent [list]` | list configured agents (switching is TUI-only) |
| `/update check\|status\|install\|skip` | check for milk updates, install one, or skip a release (honored by every later check). `install` replaces the binary under the running process, so the reply says to restart the milk agent — and on Windows, where the running binary cannot be replaced, it reports the saved download path instead of claiming success |
| `/setup telegram [on\|off\|status]` | configure Telegram remote oversight (see "Remote oversight (Telegram)"). The bare form runs the interactive wizard: the bot token is typed in chat and consumed by the wizard (never sent to the model; `cancel` aborts), then a clickable confirm — or typed `done` as the floor. `on`/`off` flip the backend without touching stored credentials, `status` reports the current state |
| `/tasks`, `/task done <id>` | list / complete tasks (session and global) |
| `/bg [list\|show <id>\|start <task>\|stop <id>]` | list, inspect, start or stop background agents |
| `/workflow <name> <task> [--<role> <agent>]` | run a workflow (see "Workflows, tasks and background agents") |
| `/workflow resume\|status\|clear` | continue, inspect or clear the session's saved workflow |
| `/help` | list the above |

TUI-only commands (`/panel`, `/colorize`, `/paste`, `/attach`, `/mcp`,
`/reload`, `/new`, `/workflow reconfigure`,
`/task add`, …) are **not** advertised. If sent anyway they get a "only available in the
milk TUI" reply instead of reaching the model. Text that merely mentions a
command mid-sentence is an ordinary prompt. Routing pins from `/escalate` and
`/primary` are per ACP session.

### The setup wizard

`/config init` (or its `/init` alias) runs the same guided first-run wizard
the TUI runs, driven entirely over ACP — the whole point being that someone
who only ever meets milk through their editor can configure it:

- **Form dialogs where the client supports them.** When the client advertises
  form-mode elicitation (`clientCapabilities.elicitation.form` at
  `initialize`), the wizard asks its steps as `elicitation/create` form
  dialogs inside the `/config init` turn — one per step, that step's prompt as
  the dialog message. Choices (provider, the escalation question) come back as
  titled single-selects and the agent-tools question as a multi-select, and
  every step's default is pre-populated in the form — and spelled out in the
  field description (`type 'default' to accept …`) for clients that
  pre-populate nothing.
- **Clickable choice prompts everywhere else.** Every non-secret step with
  something worth clicking is also offered as `session/request_permission`
  options — the one prompt surface every ACP client renders as buttons (it is
  how tool approvals work): one button per choice or example, a
  `use default — …` button standing in for the empty turn ACP chat can't
  send, `type my own value…` handing that step to typed input, and
  `cancel setup`. A client whose permission responses carry no recognizable
  outcome is detected once and never asked again (its steps go to typed
  input).
- **One prompt per answer as the floor.** Questions the client can't render
  as dialogs or buttons come back as the command's `agent_message_chunk`;
  every following `session/prompt` is consumed as that step's answer
  (validation errors re-prompt the same step, exactly as in the TUI) until
  the wizard finishes with the config written and a completion summary. While
  the wizard is pending, answers are *not* sent to the model. Since an empty
  turn can't be sent, every question states what the word `default` (or `-`)
  means for that step, and it applies like pressing Enter in the TUI.
- **The credential step is typed, never form-rendered.** Elicitation form
  mode must not carry secrets (the spec forbids it — API keys included), so
  the API-key question is skipped with one click (`skip — no credential`,
  which routes to the token-command step) or typed in chat (still never
  forwarded to the model); the remaining steps return to dialogs. A dismissed
  dialog or a failed round trip likewise hands the current step to typed
  input with every applied answer kept.
- **Escape hatches** (the TUI cancels with esc, which ACP doesn't have): a
  plain `cancel` / `quit` / `abort` answer aborts the wizard, and any
  recognized slash command cancels it first, then runs — so `/help` mid-wizard
  works. Re-sending `/config init` restarts it.
- **No restart needed.** When the wizard commits, the session rebuilds its
  primary/escalation runners from the freshly written config, so the very next
  prompt uses the agent that was just configured. Sessions created afterwards
  get the new config too: `session/new` re-reads the config from disk rather
  than the server's startup snapshot.
- **No editor question.** The TUI's trailing "open config in editor now?" is
  skipped — `/config open` answers with the path for the client's editor.

## Workflows, tasks and background agents

These work over ACP without the TUI's panels; their state reaches the client
as session updates instead.

- **Tasks.** The model gets the task tools (`create_task`, …) and `/tasks`,
  `/task done <id>` work. Every change to the session's task list sends a
  plan update carrying the whole list (`pending`/`in_progress`/`completed`;
  `blocked` has no ACP status and is sent as `pending` with a "(blocked)"
  suffix and `_meta.blocked`).
- **Background agents.** The model's `spawn_background_agent` tool and
  `/bg start|list|show|stop` work. Each job is a `tool_call_update` row
  (`background_agent`, in_progress → completed/failed; the finished row
  carries the result in `rawOutput`, and `/bg show <id>` prints it in full).
  Jobs outlive the turn
  that started them, and `session/cancel` does not stop them (`/bg stop`
  does). **milk follows up on its own:** when a wave of agent-spawned jobs
  finishes (or a `/bg start` job does) and no turn is running, it announces
  itself with a `[milk] background agents finished …` message chunk (the
  client never sees the synthetic prompt, so without it the turn would start
  with no visible cause) and starts a
  turn without a prompt — `state_update` `running`, the agent's report of the
  results as ordinary message chunks, then `idle` — as the TUI does. ACP v2
  permits this ("background activity MAY … emit updates while the Agent is
  idle"); a v1 client has no such notion and may not expect it. If a turn is
  already running when the jobs finish, the follow-up runs right after it. A
  client prompt that arrives during a follow-up waits for it to finish (it
  isn't cancelled, which would lose the results it already consumed), so
  turns never overlap. `session/cancel` does abort a follow-up.
- **Workflows.** The model's `start_workflow` tool and
  `/workflow <name> <task> [--<role> <agent>]` run the native workflow engine
  *inside* the `session/prompt` that started it, so the prompt stays open for
  the whole run and `session/cancel` cancels it (`/workflow resume` continues
  from the last checkpoint). Roles without an explicit agent use the
  escalation agent. Progress is a plan update with one entry per stage, plus a
  `workflow` tool-call row and start/finish messages. Stage output streams
  into that row (see "Live output" below), under a `── <role> ──` header per
  stage; the workflow definition still decides what it writes to disk.
  **User questions are auto-answered:** a step that would ask the user
  something (designer questions, `user_checkpoint`) is shown to the client and
  answered with "continue" (accept the designer's defaults), because the user
  cannot reply while the prompt is open. Put specifics in the task text to
  steer it. Only one workflow runs per session at a time.
- **Live output.** A background job's output and a workflow's stage output
  stream into their tool-call rows (`job:<id>`, `workflow:<n>`), flushed
  before the row's `completed`/`failed` update. v2 clients get
  `tool_call_content_chunk` appends; v1 has no append, so v1 clients get a
  `tool_call_update` with the full content so far, at most every 400 ms.
  Output is capped by the live buffer, so a very long run shows its tail.
- **Plan shape depends on the client.** A client that sent
  `protocolVersion: 1` in `initialize` gets the v1 `plan` update (one plan per
  session, no `planId`; tasks and workflow entries are concatenated, and
  `cancelled` is sent as `completed` with a "(cancelled)" suffix). Anything
  else gets v2 `plan_update` with a `planId` per plan (`tasks-<session>`,
  `workflow-<n>`).

## Tool permissions

A local-provider agent asks before running side-effecting tools (file writes,
shell commands, …) with `session/request_permission`, unless the tool is
already granted, matches `bash_allowed_patterns`, or skip-permissions is on.

- **Request shape.** Title `Allow <tool>?`, the call summary as description,
  and the pending tool call as subject: v2 clients get
  `subject: {type: "tool_call", toolCall: {toolCallId}}`; a v1 client (one
  that sent `protocolVersion: 1`) gets v1's `toolCall` object instead.
  Options are `allow_once` / `reject_once`. An approval is remembered for that
  tool, as in the TUI.
- **A client that can't answer.** If `session/request_permission` fails (for
  instance a client that doesn't implement it), the tool is denied and milk
  says so in a message, naming the cause, rather than only reporting "denied
  by user". Use `/skip-permissions on` (or set `dangerously_skip_permissions`
  in the config) to run tools without asking.
- **Background agents** ask too. Their prompts point at the job's own
  `job:<id>` tool-call row and say which job is asking; the wait is bounded by
  the background-agent timeout, after which the tool is denied. `/skip-permissions`
  and `dangerously_skip_permissions` apply to jobs as well. (In the TUI a
  background job never asks and can only use tools already granted.)
- **Not covered yet:** the claude-cli escalation agent (its tool permissions
  are denied by default over ACP, and no request is ever sent), and agents
  that a workflow role or a tool-agent entry builds separately from the
  session's primary/escalation agents.
- **Remote oversight races along.** When `remote_oversight` is configured,
  the backend's allow/deny (or its `timeout_action` on timeout) is asked in
  parallel with the client and the first answer wins — the TUI's rule. The
  remote side is serialized process-wide: the Telegram backend has a single
  prompt slot, so concurrent sessions queue their remote asks instead of
  stealing each other's.

## Remote oversight (Telegram)

`milk serve --acp` runs the same remote oversight as the TUI
(docs/operations.md's "Remote oversight (Telegram)") — one notifier per
serve process, built at serve start from `remote_oversight` in the config
and rebuilt when `/setup telegram` changes it:

- **Turn and tool notifications** — turn start/response/done, slash-command
  output, and (gated by `notify_tools`) tool use/results forward to the
  backend for both providers, plus workflow turns (`workflow:<role>`).
- **Permission prompts race the client** — `session/request_permission`
  goes to the editor as always; the remote answer wins the race if it
  arrives first (see the Tool permissions section above).
- **Bot messages run as turns** — a message you send the bot runs in the
  live session with the most recent activity, echoed to the client as an
  ordinary user message; it queues while a turn is running and runs at turn
  end. Messages that arrive before any session exists are held and run on
  the next `session/new`.

## Routing and warnings

- **Routing.** Each model turn sends a `_milk/route` notification
  (`agent`, `target`, `reason`) once the router has decided, and the turn's
  closing `idle` `state_update` carries the same as
  `_meta["milk/route"]`. A one-line message ("→ now handled by <agent>
  (<target>): <reason>") appears only when the handling target *changes*
  from the previous model turn.
- **Loop and consumption warnings.** milk's loop detector runs per session:
  streamed reasoning is checked for repetition as it arrives (even with
  `/think off`), and each finished turn is checked for token velocity, silent
  burn, turn flood and cross-turn repetition. A verdict is sent as a
  `[⚠ loop detected: …]` or `[⚠ consumption: …]` message chunk and as a
  `_milk/warning` notification (`category`, `consumption`, `message`). With
  `loop_detection.auto_interrupt: true` the running turn is cancelled too.
  Workflow steps are exempt from the reasoning check, as in the TUI.
- **Custom methods.** ACP requires custom notification methods to start with
  `_`, and clients should ignore ones they don't know. milk lists the ones it
  emits in `initialize`'s `capabilities._meta.milk.notifications`.

### Caveat: `agent_message_chunk` is not token-level streaming

ACP's naming suggests incremental delivery, but milk's underlying callback
(`OnResponseSegment`) only fires once per tool-call boundary and once more
for the final text — for the common case of a text-only turn with no tool
calls, that's exactly **one** `agent_message_chunk` carrying the whole
response, not a token stream. A client expecting live token-by-token
rendering will instead see the full text arrive in one notification right
before the `idle` `state_update`. Real partial-token streaming would need a
new, lower-level hook in `internal/agent/local`/`internal/agent/claude` that
doesn't exist yet.

Outside of turn output, `agent_message_chunk` is also milk's channel for the
occasional one-line notices that fire while the agent is `idle`: why a
background-follow-up turn is starting, the one-shot "new release
available" announcement (debounced across sessions — at most one per session,
never for a release chosen via `/update skip`), and the `resumed session …`
notice that resume-by-default adoption sends (see "Session resume, list, and
history replay" below).

### Caveat: permission prompts only cover local-provider agents

If the session's active agent (primary or, after routing, escalation) is a
local OpenAI-compatible/Bedrock provider, a tool call needing confirmation
sends a real `session/request_permission` request and blocks for your
response. If the active agent is `claude-cli` (milk's default escalation
agent), **no permission request is ever sent** — every tool call it attempts
is silently denied by `claude`'s own default behavior, visible only in its
response text. claude-cli speaks its own separate control-request wire
protocol for permissions; bridging that through ACP is unbuilt. If you need
an escalation agent that can actually use tools under ACP today, configure a
local-provider agent as `escalation_agent` instead of the `claude-cli` default
(see [docs/providers.md](providers.md)).

### Capability negotiation note

`initialize`'s response advertises `capabilities.session: {"delete": {}}`.
Per the upstream ACP schema, supplying *any* value for `session` (even `{}`)
declares support for the **entire** baseline — `session/new`, `session/list`,
`session/resume`, `session/close`, `session/prompt`, `session/cancel`, and
`session/update` — as one bundle; there's no finer-grained flag for a subset.
milk now implements the whole bundle, so the advertisement is simply
accurate; the one add-on flag is `delete` (`{}`), which additionally
advertises `session/delete`.

## Example exchange

A single-turn conversation, request/response pairs annotated (actual traffic
is newline-delimited, one JSON object per line — formatted here for
readability):

```jsonc
// → client sends
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"my-editor"}}}

// ← agent responds
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":2,"info":{"name":"milk","version":"0.9.0"},"capabilities":{"session":{"delete":{}}}}}

// → client sends
{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/home/me/myproject"}}

// ← agent responds
{"jsonrpc":"2.0","id":2,"result":{"sessionId":"a1b2c3d4-..."}}

// → client sends
{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"a1b2c3d4-...","prompt":[{"type":"text","text":"what does go.mod declare as the module name?"}]}}

// ← agent sends notifications WHILE the request above is still pending —
//   the response to id 3 only arrives after the final one of these:
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"a1b2c3d4-...","update":{"sessionUpdate":"state_update","state":"running"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"a1b2c3d4-...","update":{"sessionUpdate":"tool_call_update","toolCallId":"toolu_1","name":"read_file","kind":"read","status":"in_progress","rawInput":{"path":"go.mod"}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"a1b2c3d4-...","update":{"sessionUpdate":"tool_call_update","toolCallId":"toolu_1","name":"read_file","kind":"read","status":"completed","rawOutput":"module example.com/t\n\ngo 1.26\n"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"a1b2c3d4-...","update":{"sessionUpdate":"agent_message_chunk","messageId":"msg-1","content":{"type":"text","text":"go.mod declares module example.com/t."}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"a1b2c3d4-...","update":{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}}}

// ← agent responds (only now — session/prompt was held open for the whole turn)
{"jsonrpc":"2.0","id":3,"result":{"messageId":"msg-1"}}
```

Continuing an earlier conversation (see "Session resume, list, and history
replay" below for the full semantics):

```jsonc
// → client asks what conversations exist for this workspace
{"jsonrpc":"2.0","id":4,"method":"session/list","params":{"cwd":"/home/me/myproject"}}

// ← agent responds (last-use first; title falls back to the first user turn)
{"jsonrpc":"2.0","id":4,"result":{"sessions":[{"sessionId":"d4c3b2a1-...","cwd":"/home/me/myproject","title":"rename the config flag","updatedAt":"2026-09-28T09:30:00Z"}]}}

// → client continues that conversation
{"jsonrpc":"2.0","id":5,"method":"session/resume","params":{"sessionId":"d4c3b2a1-...","cwd":"/home/me/myproject"}}

// ← agent responds (commands come inline; an update-available notice, if
//   any, follows as a session/update)
{"jsonrpc":"2.0","id":5,"result":{"availableCommands":[{"name":"/export","description":"print the session transcript (or another session's), or write it to a file","input":{"type":"text","hint":"[json|<path>|session <id>]"}}]}}
```

Trying an unimplemented method (the legacy v1 history-load — deliberately not
implemented, see "Known gaps"):

```jsonc
// → client sends
{"jsonrpc":"2.0","id":6,"method":"session/load","params":{"sessionId":"d4c3b2a1-...","history":[]}}

// ← agent responds
{"jsonrpc":"2.0","id":6,"error":{"code":-32601,"message":"method not found: session/load"}}
```

## Session resume, list, and history replay

`session/list`, `session/resume`, `session/close` and `session/delete` are the
ACP v2 standard session-management surface; this section is their full
semantics, including milk's one deliberate extension: resume-by-default on
`session/new`.

### Resume-by-default: `session/new` adopts

milk's session model is one-conversation-per-cwd — launching the TUI in a
terminal continues that directory's conversation unless asked not to
(`--continue` is the default, `--new` opts out). Over ACP, `session/new` is
the entry point clients actually call, so it gets the same default: it
**adopts** the cwd's most-recent stored session (equivalent to resuming that
session) iff **all five** conditions hold:

1. config `acp_resume` is on (default: on; `false` restores strict-spec
   behavior — `session/new` always starts empty),
2. the request's `_meta.milk.fresh` is not `true` (the per-request escape
   hatch — `_meta` is ACP's sanctioned channel for exactly this),
3. this serve process has no live session whose cwd is the request's `cwd`
   (the second `session/new` for a workspace is therefore always fresh —
   "new thread" keeps working inside a running client),
4. the candidate session was not closed earlier in this process (an
   explicitly closed thread is never silently resurrected), and
5. the store holds at least one session for the cwd.

Anything else behaves exactly as before. On adopt, the response returns the
**adopted session's id** — the id the client keeps using for this
conversation — and adoption never hides state: the bounded history replay
below is mandatory (the client's panel is empty), and a one-line notice
precedes it (`resumed session … — N earlier turns; /export prints the full
transcript`). A spec-strict client that expects `session/new` to be empty
therefore sees old context at most once per workspace open — announced and
replayed, never silently. TUI parity: `--continue` ≈ adopt; `--new` ≈
`acp_resume: false` or `_meta.milk.fresh: true`.

### History replay (`replayFrom`)

- omitted or `null` → resume without replaying anything.
- `{"type":"start"}` → replay all retained history **before the response**,
  as `session/update` message upserts (`user_message` / `agent_message` /
  `agent_thought`) plus `tool_call_update` rows for tool trails (each
  `rawOutput` paired from the recorded tool result). A v1 client gets the
  chunk-form fallback instead (`user_message_chunk` etc. — same `messageId`
  appends into one message).
- any other cursor type (including future `_`-prefixed extensions) →
  `-32602`, rejected before anything is emitted — milk refuses rather than
  guessing where to replay from.

**Message identity.** Replayed messages carry deterministic ids derived from
their history index (`hist-u3`, `hist-a5`, `hist-t5-0`, …), so replaying twice
patches the client's existing messages instead of duplicating them. Live ids
are suffixed with a per-process run id, so they can never collide with
`hist-*` or with ids minted before a restart. One documented deviation: milk
does not retain the original ACP `messageId`s in its session file ("Agents …
are not required to retain" them), so replayed messages carry synthesized
stable ids.

**Bounds.** "All *retained* history" is windowed to keep the notification
blast finite: each message's text is capped (~8 KB head+tail), and the replay
covers the first 10 and last 200 turns, with one explicit marker message in
the gap — `[… N earlier turns omitted from replay — /export prints the full
transcript …]`.

### Exploring after resume: `/export session <id|prefix>`

`/export` prints the current session's transcript — including pre-resume
turns on a resumed session. To preview *another* session without attaching
it, `/export session <id|prefix>` dumps that session (exact id or unambiguous
prefix; ambiguity and misses are errors, never a guess), composable with
`json` or a file path: `/export [session <id|prefix>] [json|<path>]`. The
exploration flow is `/list` → `/export session a1b2` → `session/resume`.

### close vs delete

`session/close` cancels a running turn, drops pending follow-ups, persists
the session file and frees the in-process resources (idempotent; the file is
kept — close ≠ delete). `session/delete` closes an open session first, then
removes its file and index entry. Either way the id joins this process's
closed-set: the next `session/new` will not resurrect it (condition 4 above).

## Error handling

- **`-32601`** (JSON-RPC reserved "method not found"): the method isn't
  implemented at all (see the gap list below).
- **`-32602`** ("invalid params"): a well-formed request whose params fail
  validation — e.g. `session/resume` with a `cwd` that doesn't match the
  session's, or an unknown `replayFrom` cursor type.
- **`-32002`** ("resource not found"): the requested session doesn't exist
  (`session/resume`, `session/delete`).
- **`-32000`** (generic server error): every other failure (a turn-level
  error, an internal store failure) — don't pattern-match on its text, just
  surface it.
- `session/cancel` is a **notification** (no `id`, no response). Sending one
  for a session with no turn currently running is a safe no-op.
- A malformed JSON-RPC line on stdin is silently dropped, not reported —
  there's no response channel for a line that failed to parse into even a
  method name.

## Known gaps vs. the full design (deferred, not silently missing)

Tracked explicitly in `docs/machine-readable-output-design.md`'s Phase 2
status note, repeated here for integrators who don't want to read the whole
design doc:

- `auth/login`, `auth/logout`
- the legacy v1 `session/load` (client-supplied history): the old wrong
  shape — history lives client-side there. Stays `-32601` by design; use
  `session/resume` with `replayFrom` for history replay.
- `session/set_config_option` (the internal handler exists, isn't wired to
  the dispatcher)
- the tool-facing `events.Host.Elicit` seam (`cmd/milk/host_acp.go`) — still a
  stub returning a cancelled result; the setup wizard's form dialogs use the
  form-capable `acp.ACPHost.Elicit` round trip instead
- `terminal_update` (PTY/agent-owned terminal streaming)
- The `_milk/notification` and `_milk/memory` extension channels — never
  emitted (`_milk/route` and `_milk/warning` are; see "Routing and warnings")
- `NewSessionRequest.mcpServers` — accepted and parsed, never merged with
  the agent's own configured MCP servers
- `cwd` isn't validated as an absolute path (the ACP spec says it should be),
  only checked for non-empty

## Shared with the TUI

ACP and the TUI run the same turn loop (`runPrimary` / `runEscalation`) and
the same workflow engine. They also share host-independent cores, so a rule
changed in one shows up in the other: **routing** (`turn_routing.go` —
pins, single-turn `/escalate` and `/primary`, availability fallback,
auto-sticky escalation after the router first escalates, turn metrics),
**workflow launch/resume/clear** (`workflow_core.go`), **when a background
follow-up turn runs** (`followup_core.go`), the **setup wizard**
(`initwizard_core.go` — step machine, validation, field descriptors, config
commit; `acp_initwizard.go` — the elicitation form drive) and the
**config display** (`configview.go` — `/config` and `/config show`). Each
host still does its own channel work: wiring agents to its output, deciding
whether a turn is running, and rendering progress.

## For implementers extending this

- `internal/transport/acp` — wire shapes (`lifecycle.go` for the inbound
  request/response structs, `acp.go`/`host.go`/`map.go`/`plan.go`/
  `terminal.go`/`commands.go`/`ext.go` for the rest of the ACP v2 vocabulary,
  most of it already defined but unwired) and the JSON-RPC transport
  (`stdio.go`'s `StdioConn` — generic, no milk-specific knowledge, testable
  via `io.Pipe`).
- `cmd/milk/serve.go` — the `serve --acp` cobra command.
- `cmd/milk/acp_server.go` — method dispatch (`initialize`/`session/new`/
  `session/prompt`/`session/cancel`); add a new `case` here plus the
  matching struct in `lifecycle.go` to wire up another method.
- `cmd/milk/acp_session.go` — per-session state and the actual turn dispatch,
  structurally mirroring `cmd/milk/repl.go`'s `buildTUIAgents` (TUI callbacks
  → `tea.Msg`; this file's callbacks → `conn.Notify`/ACP `Mapper` calls).
- `cmd/milk/host_acp.go` — the `events.Host` adapter bridging milk's
  host-agnostic permission/elicitation interface onto ACP's wire shapes, for
  local-provider agents.
- Tests: `internal/transport/acp/stdio_test.go` (transport-layer, race-clean,
  no subprocess), `cmd/milk/acp_server_test.go` (direct handler tests, fake
  `Conn`, no subprocess), `cmd/milk/serve_acp_e2e_test.go` (real compiled
  binary, real subprocess — the closest thing to a live integration test
  available without a real ACP client in this environment).
