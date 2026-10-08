package main

// Remote oversight over ACP — the ACP half of docs/operations.md's
// "Remote oversight (Telegram)". Three things run here, mirroring the TUI:
//
//   - turn/tool notifications: acpSession.runTurn and the tool-use hooks
//     forward start/response/done/tool events to the serve process's
//     notifier (one per process, not per session — Telegram has one bot and
//     one chat);
//   - permission prompts are raced against the client: when the user
//     answers session/request_permission in the editor, that wins; when the
//     client sits silent, the remote backend's allow/deny (or its timeout
//     action) decides — exactly makeTUIPermissionHandler's race with the ACP
//     client in the TUI's seat;
//   - messages the user sends the bot run as turns in the live session most
//     recently used, or queue until one exists (the TUI's remoteInputMsg
//     path).
//
// Lifecycle: runServe calls startOversight (not newACPServer — tests must
// never spawn a poller or a network ping), and /setup telegram on|off (or
// the wizard's commit) calls resetNotifier to rebuild it from the saved
// config, cancelling the previous polling loop first so only one getUpdates
// consumer ever runs. When remote oversight is disabled or unconfigured the
// notifier is oversight.Noop and every path below degrades to "client only".

import (
	"context"
	"strings"
	"sync"

	"github.com/scoutme/milk/internal/events"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/transport/acp"
)

// startOversight arms the serve process's remote oversight: build the
// notifier from config (newNotifier pings the backend and warns on stderr),
// route incoming remote messages to sessions, and start the polling loop.
// Called from runServe with the serve context, so polling dies with the
// process.
func (s *acpServer) startOversight(ctx context.Context) {
	s.mu.Lock()
	s.serveCtx = ctx
	s.mu.Unlock()
	s.resetNotifier()
}

// resetNotifier rebuilds the notifier from s.cfg — at startup and whenever
// /setup telegram (or the wizard's commit) changes remote_oversight — and
// re-points remote-input routing at it. The previous notifier's polling loop
// is cancelled first: two concurrent getUpdates consumers on the same bot
// would split the message stream between two stale callbacks.
func (s *acpServer) resetNotifier() {
	n := newNotifierFor(s.cfg)
	tn, polling := n.(interface {
		SetOnInput(func(string))
		StartPolling(context.Context)
	})
	if polling {
		tn.SetOnInput(s.handleRemoteInput)
	}

	s.mu.Lock()
	if s.notifierStop != nil {
		s.notifierStop()
		s.notifierStop = nil
	}
	s.notifier = n
	var loopCtx context.Context
	if polling && s.serveCtx != nil {
		var cancel context.CancelFunc
		loopCtx, cancel = context.WithCancel(s.serveCtx)
		s.notifierStop = cancel
	}
	s.mu.Unlock()

	if polling && loopCtx != nil {
		tn.StartPolling(loopCtx)
	}
}

// notifierOrNil returns the serve process's notifier — oversight.Noop before
// startOversight, in tests, or whenever remote oversight is off.
func (s *acpServer) notifierOrNil() oversight.Notifier {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notifier == nil {
		return oversight.Noop{}
	}
	return s.notifier
}

// notifier returns the owning server's notifier for this session (Noop when
// the session was built without a server — bare unit-test setups).
func (as *acpSession) notifier() oversight.Notifier {
	if as.srv == nil {
		return oversight.Noop{}
	}
	return as.srv.notifierOrNil()
}

// notifyTools reports whether tool-use events are forwarded (the same
// config gate the TUI checks before every NotifyToolUse/NotifyToolResult).
func (as *acpSession) notifyTools() bool {
	return as.st.cfg.RemoteOversight.NotifyToolsEnabled()
}

// oversightRemote reports whether n is an active remote backend (not the
// Noop used when oversight is off, or before startOversight ran).
func oversightRemote(n oversight.Notifier) bool {
	_, noop := n.(oversight.Noop)
	return !noop
}

// --- remote input: messages the user sends the bot ---

// handleRemoteInput receives a message from the remote backend's polling
// loop and runs it as a turn — the ACP counterpart of the TUI's
// remoteInputMsg handler. Target: the live session with the most recent
// prompt activity. With no live session the message is queued and handed to
// the next session created (drainRemoteQueue).
func (s *acpServer) handleRemoteInput(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	as := s.mostRecentSession()
	if as == nil {
		s.mu.Lock()
		s.remoteQueue = append(s.remoteQueue, text)
		s.mu.Unlock()
		return
	}
	as.requestRemoteInput(text)
}

// mostRecentSession returns the live session with the latest prompt activity
// (lastActive), or nil when none exists. Closed sessions never match.
func (s *acpServer) mostRecentSession() *acpSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *acpSession
	var bestAt int64
	for _, as := range s.sessions {
		if as.closed.Load() {
			continue
		}
		if at := as.lastActive.Load(); best == nil || at > bestAt {
			best, bestAt = as, at
		}
	}
	return best
}

// drainRemoteQueue hands messages that arrived before any session existed to
// a freshly created one (AfterResponse, session/new and session/resume).
func (s *acpServer) drainRemoteQueue(as *acpSession) {
	s.mu.Lock()
	queue := s.remoteQueue
	s.remoteQueue = nil
	s.mu.Unlock()
	for _, text := range queue {
		as.requestRemoteInput(text)
	}
}

// requestRemoteInput runs text as a turn now if the session is idle, else
// remembers it for turn end — requestFollowup's turnMu gate, same rules.
func (as *acpSession) requestRemoteInput(text string) {
	if as.closed.Load() {
		return
	}
	if as.turnMu.TryLock() {
		go as.runRemoteTurn(text)
		return
	}
	as.mu.Lock()
	as.pendingRemoteInputs = append(as.pendingRemoteInputs, text)
	as.mu.Unlock()
}

// runRemoteTurn runs one remote-oversight turn; the caller holds turnMu.
// Unlike backgroundFollowupPrompt the text is echoed to the client as an
// ordinary user message — it *is* something the user typed, just from
// another channel.
func (as *acpSession) runRemoteTurn(text string) {
	defer func() {
		as.turnMu.Unlock()
		as.flushPendingFollowup()
		as.flushRemoteInputs()
	}()
	if _, err := as.runTurn(context.Background(), text); err != nil {
		as.notify(acp.AgentMessageChunk(as.liveID("remote"),
			"Remote-oversight turn failed: "+err.Error()))
	}
}

// flushRemoteInputs runs the next queued remote message, if any. Called only
// after turnMu has been released (prompt, runFollowup and runRemoteTurn
// defers); requestRemoteInput re-acquires it.
func (as *acpSession) flushRemoteInputs() {
	if as.closed.Load() {
		return
	}
	as.mu.Lock()
	if len(as.pendingRemoteInputs) == 0 {
		as.mu.Unlock()
		return
	}
	next := as.pendingRemoteInputs[0]
	as.pendingRemoteInputs = as.pendingRemoteInputs[1:]
	as.mu.Unlock()
	as.requestRemoteInput(next)
}

// --- permission race: client vs remote backend ---

// oversightPermMu serializes the *remote* side of permission races
// process-wide: the Telegram backend has a single prompt slot
// (internal/oversight/telegram's permCh), so two concurrent AskPermission
// calls would steal it from each other — the loser would time out, and its
// reply could fall through to the input callback and be injected as a typed
// message. The lock is taken by the racing goroutine and held until
// AskPermission has fully returned (including its permCh cleanup), so a
// waiter can never clear the slot of the next one.
var oversightPermMu sync.Mutex

// racePermAsk runs askClient and the remote backend's AskPermission
// concurrently; the first answer wins — makeTUIPermissionHandler's rule,
// with the ACP client in the TUI's seat. With no active backend it is just
// the client ask. askClient receives a context cancelled the moment the
// remote side answers, so a losing client wait stops promptly.
func racePermAsk(ctx context.Context, n oversight.Notifier, askClient func(context.Context) bool, req oversight.PermRequest) bool {
	if !oversightRemote(n) {
		return askClient(ctx)
	}
	if ctx.Err() != nil {
		return askClient(ctx)
	}
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan bool, 2)
	go func() { ch <- askClient(raceCtx) }()
	go func() {
		oversightPermMu.Lock()
		defer oversightPermMu.Unlock()
		if raceCtx.Err() != nil {
			return // already answered by the client — don't prompt the backend
		}
		ch <- n.AskPermission(raceCtx, req) == oversight.PermAllow
	}()
	return <-ch
}

// askPermissionWithOversight is the local agent's permission callback over
// ACP: the client is asked via session/request_permission (as before —
// makeLocalPermAsk's prompt text, now shared in permAskPrompt), raced
// against the remote backend when one is active.
func (as *acpSession) askPermissionWithOversight(tool, summary string) bool {
	return racePermAsk(context.Background(), as.notifier(),
		func(ctx context.Context) bool {
			outcome, _ := as.host.RequestPermission(ctx, events.PermissionRequest{
				Prompt:  permAskPrompt(tool, summary),
				Tool:    tool,
				Summary: summary,
			})
			return outcome.Allow
		},
		oversight.PermRequest{ToolName: tool, Input: summary})
}
