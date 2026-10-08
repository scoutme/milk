package main

import (
	"context"

	"github.com/scoutme/milk/internal/transport/acp"
)

// Automatic background follow-up: when background jobs finish and the session
// is idle, milk runs a model turn on its own so the agent reports the
// results — the ACP counterpart of the TUI's maybeAutoFollowupBackgroundJobs.
// ACP v2 allows this: work with no prompt in flight is bracketed by
// state_update running/idle like any other, and its messages carry their own
// messageIds. (A v1 client has no such concept and may not expect it.)
//
// The rules mirror the TUI's:
//   - a wave of agent-spawned jobs follows up once, when the last one ends;
//   - a user-started job (/bg start) follows up as soon as it ends;
//   - if a turn is running at that moment, the request is remembered and
//     retried when the turn ends;
//   - a client prompt never cancels a follow-up (it would lose the results
//     the follow-up already drained); it waits its turn instead.

// requestFollowup starts a follow-up turn now if the session is idle and
// finished-job results are waiting, else remembers it for turn end. The
// when-to-follow-up rules are decideFollowup's, shared with the TUI. A closed
// session (session/close) runs no further automatic work — job results stay
// in the manager store for the next attach.
func (as *acpSession) requestFollowup(waitForWholeWave bool) {
	if as.closed.Load() {
		return
	}
	mgr := as.mgr
	if mgr == nil {
		return
	}
	wave, user := &as.pendingWaveFollowup, &as.pendingUserFollowup
	if mgr.PendingCount() == 0 {
		// ACP-only guard: a client prompt may have drained the results
		// already, and a follow-up with nothing to report is a wasted turn.
		as.mu.Lock()
		applyFollowupDecision(waitForWholeWave, followupSkip, wave, user)
		as.mu.Unlock()
		return
	}
	locked := as.turnMu.TryLock()
	d := decideFollowup(waitForWholeWave, mgr.ActiveCount(), !locked)
	if d != followupNow && locked {
		as.turnMu.Unlock()
	}
	as.mu.Lock()
	applyFollowupDecision(waitForWholeWave, d, wave, user)
	as.mu.Unlock()
	if d == followupNow {
		go as.runFollowup()
	}
}

// flushPendingFollowup retries a follow-up requested while a turn was
// running. No-op after session/close (the closed flag).
func (as *acpSession) flushPendingFollowup() {
	if as.closed.Load() {
		return
	}
	as.mu.Lock()
	wave, user := as.pendingWaveFollowup, as.pendingUserFollowup
	as.mu.Unlock()
	switch {
	case user:
		as.requestFollowup(false)
	case wave:
		as.requestFollowup(true)
	}
}

// runFollowup runs the follow-up turn; the caller holds turnMu.
func (as *acpSession) runFollowup() {
	defer func() {
		as.turnMu.Unlock()
		as.flushPendingFollowup()
	}()
	if _, err := as.runTurn(context.Background(), backgroundFollowupPrompt); err != nil {
		as.notify(acp.AgentMessageChunk(as.liveID("followup"),
			"Background follow-up failed: "+err.Error()))
	}
}

// announceFollowup tells the client why a synthetic turn is starting. Unlike
// the TUI — which echoes the follow-up prompt under its [background] label —
// an ACP client never sees the synthetic prompt at all: without this the
// assistant would simply start talking (or say nothing) with no visible
// cause. Sent as a plain agent message chunk before the turn runs.
func (as *acpSession) announceFollowup() {
	as.notify(acp.AgentMessageChunk(as.liveID("followup"),
		"[milk] background agents finished — running a follow-up turn to report their results.\n"))
}
