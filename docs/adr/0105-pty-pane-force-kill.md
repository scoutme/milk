# 0105. PTY pane force-kill escape: double Ctrl+C

Date: 2026-11-09
Status: accepted
Implemented: cmd/milk/pty_pane.go (handlePTYKey escalation + forceKillPTYPane), pty_kill_unix.go / pty_kill_windows.go (process-group SIGKILL), repl.go (identity-guarded directBashDoneMsg), status.go (gesture hint)

## Context

Bang-mode (`!`) commands run in an embedded PTY pane (`launchPTYPane`), and
while that pane is up `Update` gives it top key-dispatch priority: *every*
key — Ctrl+C included — is forwarded to the child as raw bytes (`^C`), because
a pane that behaves like a terminal cannot intercept the interrupt itself. The
status bar said so: `keys forwarded · Ctrl+C = ^C · Ctrl+D = EOF`.

That contract had a trap. A child that ignores or swallows `^C` — a
network-stuck FTP deploy, a task runner trapping SIGINT to "cancel gracefully"
and then waiting on a child that never notices, a raw-mode tool reading `0x03`
as data — left the user stuck: milk's own Ctrl+C stack (undo queue → clear
input → interrupt → quit, `handleCtrlC`) never sees a key while a pane is up,
so there was *no* gesture that returned control to milk. The reported bug was
exactly this shape: `!FTP_HOST=… task site:deploy` hung, `^C` did nothing,
milk was unreachable. The read goroutine compounds it: it only delivers
`directBashDoneMsg` after `ptm.Read` errors out, which requires every slave fd
to close — a descendant that leaked the slave fd keeps the pane "running" even
after the foreground command is gone.

## Decision

**A second Ctrl+C within 2s (`ptyCtrlCKillWindow`) is the pane's force-kill
escape; the first press remains a plain forwarded `^C`.** The escalation
lives in `handlePTYKey` and keys on the exact bytes about to be forwarded
(`0x03`), so every encoding of Ctrl+C takes it and no other key can.

`forceKillPTYPane` then:

1. snapshots the VT screen and appends it to the transcript with a
   `[milk] direct-bash force-killed: <cmd>` note — the same hand-off a normal
   exit produces, plus the reason the output stops where it does;
2. mirrors `directBashDoneMsg`'s busy bookkeeping: an agent turn that was
   already running when the bang launched keeps its busy/cancelTurn (issue
   #128) and receives the output via `PendingBangOutput` instead;
3. SIGKILLs the child's *process group* (`killPTYProcessGroup` — the pane
   child is a session leader, so its pid doubles as its pgid; `^C` alone only
   ever reached the foreground group) and closes the master fd;
4. tears the pane state down **eagerly**, right in the key handler — never
   waiting for the waiter goroutine, because its `cmd.Wait` may never return.

The eager teardown orphans the read goroutine, so `directBashDoneMsg` gains a
`pane *ptyPaneState` identity field and the handler skips any message whose
pane is no longer the active one. That guard is what makes a late waiter
message — or one arriving after a *new* pane launched — harmless, instead of
closing the pane that is on screen now.

## Consequences

- The contract is discoverable before it is needed: the status bar reads
  `Ctrl+C = ^C · Ctrl+C ×2 = force-kill`.
- Double-tapping `^C` quickly force-kills even a *responsive* child — asking
  twice is the point, and the victim is only the pane's own process group.
- A wedged waiter goroutine (unkillable child, leaked slave fd) can outlive
  the pane; it holds one goroutine and a closed fd, writes only to its private
  VT state, and its eventual message is dropped by the identity guard.
- The Windows piped fallback never sets `ptyPane`, so the gesture is
  PTY-platform-only there (`killPTYProcessGroup` still compiles to a plain
  `Process.Kill` for completeness).
