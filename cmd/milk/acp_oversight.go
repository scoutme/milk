package main

// Remote oversight over ACP — the ACP half of docs/guides/operations.md's
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
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/events"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/transport/acp"
)

// startOversight arms the serve process's remote oversight: build the
// notifier from config (network-free — verification happens in
// resetNotifier's armOversight call, issue #388), route incoming remote
// messages to sessions, and start the polling loop once the backend has
// answered its background ping. Called from runServe with the serve
// context, so polling dies with the process.
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
//
// Verification is backgrounded (issue #388): newNotifier builds without
// touching the network, and armOversight pings the backend from its own
// goroutine — serve startup never waits on Telegram. Polling starts only for
// a backend that answered; a failure lands as the old synchronous ping's
// fail-closed verdict — a stderr warning and oversight.Noop — unless a newer
// notifier replaced this one meanwhile.
func (s *acpServer) resetNotifier() {
	n := newNotifierFor(s.cfg)

	s.mu.Lock()
	if s.notifierStop != nil {
		s.notifierStop()
		s.notifierStop = nil
	}
	s.notifier = n
	var loopCtx context.Context
	if s.serveCtx != nil {
		var cancel context.CancelFunc
		loopCtx, cancel = context.WithCancel(s.serveCtx)
		s.notifierStop = cancel
	}
	s.mu.Unlock()

	if loopCtx == nil {
		// Not serving yet: register input routing only — polling (and its
		// verification) arms on the startOversight call, which carries the
		// serve context.
		if plain, ok := n.(interface {
			SetOnInput(func(string))
			StartPolling(context.Context)
		}); ok {
			plain.SetOnInput(s.handleRemoteInput)
		}
		return
	}
	armOversight(loopCtx, n, s.handleRemoteInput, func(err error) {
		fmt.Fprintf(os.Stderr, milkTag()+" warning: telegram oversight ping failed: %v — oversight disabled\n", err)
		s.mu.Lock()
		if s.notifier == n {
			s.notifier = oversight.Noop{}
		}
		s.mu.Unlock()
	})
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

// applyOversight publishes a config whose remote_oversight section changed
// (/setup telegram on|off, the wizard's commit): keep the server's startup
// copy in step, refresh every live session's copy (the notify_tools gate
// and status replies read as.st.cfg), and rebuild the process notifier.
// The per-session copies are plain struct writes — the same unsynchronized
// swap buildRunners already does when /config init commits a new config.
func (s *acpServer) applyOversight(cfg config.Config) {
	s.mu.Lock()
	s.cfg = cfg
	sessions := make([]*acpSession, 0, len(s.sessions))
	for _, as := range s.sessions {
		sessions = append(sessions, as)
	}
	s.mu.Unlock()
	for _, as := range sessions {
		as.cfg = cfg
		as.st.cfg = cfg
	}
	s.resetNotifier()
}

// applyOversight is the session-level entry point for the server method
// above (acpSetup's on|off and the wizard's commit); a session built
// without a server — bare unit-test setups — just takes the config in
// place.
func (as *acpSession) applyOversight(cfg config.Config) {
	if as.srv != nil {
		as.srv.applyOversight(cfg)
		return
	}
	as.cfg = cfg
	as.st.cfg = cfg
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
// Noop used when oversight is off, before startOversight ran, or a nil
// notifier in bare unit-test setups).
func oversightRemote(n oversight.Notifier) bool {
	if n == nil {
		return false
	}
	_, noop := n.(oversight.Noop)
	return !noop
}

// --- remote input: messages the user sends the bot ---

// jobPermSummary attributes a background-job question to its job — the same
// wording on every surface (the TUI's permission prompt text, ACP's
// session/request_permission summary, and the remote oversight request).
func jobPermSummary(summary, jobID string) string {
	if strings.TrimSpace(jobID) == "" {
		return strings.TrimSpace(summary + " — requested by a workflow step")
	}
	return strings.TrimSpace(summary + " — requested by background agent " + jobID)
}

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
	// /interrupt acts now instead of queueing behind the turn it targets —
	// the remote counterpart of the TUI's remoteInputMsg bypass.
	if cmd, _, found := extractSlashCommand(text); found && cmd == cmdInterrupt {
		s.remoteInterrupt()
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

// remoteInterrupt runs /interrupt from the remote surface (Telegram): the
// ACP counterpart of the TUI's remoteInputMsg bypass. It cancels the most
// recent session's running turn; with nothing to cancel the remote surface
// is told so. Success feedback is the cancelled turn's own NotifyTurnDone
// (⏹ interrupted) — deliberately no client-view writes here: the running
// turn owns those until it unwinds.
func (s *acpServer) remoteInterrupt() {
	as := s.mostRecentSession()
	if as == nil {
		s.mu.Lock()
		n := s.notifier
		s.mu.Unlock()
		if n != nil {
			notifyAnnouncement(n, "milk", "interrupt", "no turn running — no session yet")
		}
		return
	}
	as.mu.Lock()
	cancel := as.cancel
	as.mu.Unlock()
	if cancel == nil || !as.turnBusy() {
		if n := as.notifier(); n != nil {
			notifyAnnouncement(n, "milk", "interrupt", "no turn running")
		}
		return
	}
	cancel()
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
		as.releaseTurn()
		as.flushPendingFollowup()
		as.flushRemoteInputs()
	}()
	if _, err := as.runTurn(context.Background(), text, nil); err != nil {
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

// The client-vs-remote permission race lives in permask.go (askPermission,
// ADR-0052's three-source ask layer) — shared by the TUI, ACP and the
// claude-cli path.

// askPermissionWithOversight is the local agent's permission callback over
// ACP: the client is asked via session/request_permission (as before —
// makeLocalPermAsk's prompt text, now shared in permAskPrompt) under the
// foreground policy — direct input and remote oversight, no timed answer.
// The session allow-all/deny-all mode (issue #302) answers first (policy),
// and a client's "Always allow"/"Always deny" choice sets it.
func (as *acpSession) askPermissionWithOversight(tool, summary string) bool {
	as.mu.Lock()
	ov := as.permOverrides
	as.mu.Unlock()
	p := foregroundPermPolicy(ov)
	p.sticky = stickySetter(as.setSessionPermMode, func(note string) {
		as.host.Notify(events.Notification{Text: note})
	})
	allow, _ := askPermission(context.Background(), as.notifier(),
		oversight.PermRequest{ToolName: tool, Input: summary},
		func(ctx context.Context) bool {
			outcome, _ := as.host.RequestPermission(ctx, events.PermissionRequest{
				Prompt:  permAskPrompt(tool, summary),
				Tool:    tool,
				Summary: summary,
			})
			if outcome.Always {
				mode := sessionPermDenyAll
				if outcome.Allow {
					mode = sessionPermAllowAll
				}
				as.setSessionPermMode(mode)
			}
			return outcome.Allow
		},
		p)
	return allow
}

// setSessionPermMode records the session allow-all/deny-all mode (issue #302)
// — the ACP counterpart of the TUI's model.setSessionPermMode. The mode is
// silent in effect, so setting it is always announced by the caller's dialog
// choice or command echo.
func (as *acpSession) setSessionPermMode(mode sessionPermMode) {
	as.mu.Lock()
	as.permOverrides.sessionMode = mode
	as.mu.Unlock()
}

// safetyAsk is the doom-loop gate's unattended-context channel over ACP
// (background jobs, workflow steps): the full background flow — client
// dialog, remote oversight, and the safety policy's timed answer — mapped to
// the gate's outcome vocabulary (permask.go's safetyAskRun, ADR-0052).
func (as *acpSession) safetyAsk(jobID, tool, summary string) local.SafetyOutcome {
	as.mu.Lock()
	cfg, ov := as.cfg, as.permOverrides
	as.mu.Unlock()
	p := safetyAskPolicy(cfg, ov)
	return safetyAskRun(context.Background(), as.notifier(), jobID, tool, summary,
		func(ctx context.Context) bool {
			out, err := as.host.RequestPermission(ctx, events.PermissionRequest{
				Tool:        tool,
				Summary:     jobPermSummary(summary, jobID),
				ToolCallID:  string(acp.JobToolCallID(jobID)),
				AutoDefault: autoDefaultAnswer(p.def),
				AutoAt:      time.Now().Add(p.deadline),
			})
			return err == nil && out.Allow
		}, p)
}
