// Chat-history replay — ACP's standard mechanism (docs/acp-session-resume-plan.md
// D5): `session/resume` with `replayFrom: {"type":"start"}` (and resume-by-
// default adoption) deliver the session's retained history as session/update
// **message upserts** (user_message / agent_message / agent_thought — "Agents
// can send this when they accept or replay a user message") plus tool_call_update
// rows for tool trails. A v1 client, which has no upserts, gets the chunk-form
// fallback (same messageId appends into one message).
//
// Identity: replayed messages carry deterministic `hist-*` IDs derived from
// their history index, so a second replay **patches** the same messages
// instead of duplicating them (upsert semantics). Live IDs are runID-suffixed
// (see acpSession.liveID) so they can never collide with `hist-*` or with
// pre-restart live IDs. After a view rebinds to a different store session
// (sessionmgmt.go, ADR-0051) the IDs are session-qualified (`hist-<sess8>-*`)
// so a new binding's replay can never patch a message left by a previous
// binding's; the view's first binding keeps the plain form (see replayMsgID).
//
// Bounds ("all *retained* history" gives the license): per-message text is
// capped via internal/textbudget (head+tail inside the message), and the whole
// replay windows over history turns — first replayHeadTurns + last
// replayTailTurns — with one explicit marker message in the gap pointing at
// /export, the full-fidelity escape hatch.
package main

import (
	"encoding/json"
	"fmt"

	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/textbudget"
	"github.com/scoutme/milk/internal/transport/acp"
)

const (
	// replayMaxMessageChars caps one replayed message's text (head+tail kept).
	replayMaxMessageChars = 8 * 1024
	// replayHeadTurns / replayTailTurns bound the replay window over history
	// turns: the first N and the last M turns are replayed, the middle is
	// replaced by one marker message.
	replayHeadTurns = 10
	replayTailTurns = 200
)

// histMsgKind selects the upsert/chunk variant for one replayed or echoed
// message.
type histMsgKind int

const (
	histUser histMsgKind = iota
	histAgent
	histThought
)

// replayHistory replays this session's retained conversation history
// (sess.History, append-only) to the client. Called before the
// session/resume response is written ("replay … before responding").
func (as *acpSession) replayHistory() {
	as.replayTurns(as.sess.History)
}

// replayTurns maps a history slice onto message upserts/chunks and tool_call_
// update rows, windowing long histories head+tail with a gap marker. Split
// from replayHistory so tests can drive it with synthetic histories.
func (as *acpSession) replayTurns(hist []session.Turn) {
	n := len(hist)
	if n > replayHeadTurns+replayTailTurns {
		as.replayRange(hist, 0, replayHeadTurns)
		as.emitHistoryMessage(histAgent, as.replayGapID(), fmt.Sprintf(
			"[… %d earlier turns omitted from replay — /export prints the full transcript …]",
			n-replayHeadTurns-replayTailTurns))
		as.replayRange(hist, n-replayTailTurns, n)
		return
	}
	as.replayRange(hist, 0, n)
}

func (as *acpSession) replayRange(hist []session.Turn, from, to int) {
	for i := from; i < to; i++ {
		as.replayTurn(hist, i)
	}
}

// replayTurn replays one history turn: a user turn as user_message, an
// assistant turn as agent_message (plus agent_thought when reasoning is
// visible), its tool calls as completed tool_call_update rows. RoleToolResult
// turns carry no message of their own — their content rides out as the paired
// call's rawOutput.
func (as *acpSession) replayTurn(hist []session.Turn, i int) {
	t := hist[i]
	switch t.Role {
	case session.RoleUser:
		as.emitHistoryMessage(histUser, as.replayMsgID("u", i), boundReplayText(t.Content))
	case session.RoleAssistant:
		if t.Thinking != "" && as.showThinking.Load() {
			as.emitHistoryMessage(histThought, as.replayMsgID("think", i), boundReplayText(t.Thinking))
		}
		if t.Content != "" {
			as.emitHistoryMessage(histAgent, as.replayMsgID("a", i), boundReplayText(t.Content))
		}
		as.replayToolCalls(hist, i)
	}
}

// histMsgID is the plain deterministic replay ID for history index i — stable
// across replays so a second replay patches rather than duplicates. This is
// the first-binding form; use replayMsgID to replay through a view.
func histMsgID(kind string, i int) acp.MessageID {
	return acp.MessageID(fmt.Sprintf("hist-%s%d", kind, i))
}

// replayMsgID is the deterministic replay ID for history index i under the
// view's current binding. The first binding keeps the plain hist-* form (so
// already-replayed panes keep patching instead of duplicating on upgrade);
// after a rebind (sessionmgmt.go, ADR-0051) IDs are session-qualified —
// hist-<sess8>-u3 — so upserts can never patch a message left by a previous
// binding's replay.
func (as *acpSession) replayMsgID(kind string, i int) acp.MessageID {
	if as.replayKey == "" {
		return histMsgID(kind, i)
	}
	return acp.MessageID(fmt.Sprintf("hist-%s-%s%d", as.replayKey, kind, i))
}

// replayGapID is the gap-marker message ID under the current binding
// (hist-gap on the first binding, hist-<sess8>-gap after a rebind).
func (as *acpSession) replayGapID() acp.MessageID {
	if as.replayKey == "" {
		return "hist-gap"
	}
	return acp.MessageID("hist-" + as.replayKey + "-gap")
}

// replayToolID is the tool-call row ID for call j of history turn i under the
// current binding (hist-t<i>-<j>, session-qualified after a rebind).
func (as *acpSession) replayToolID(i, j int) acp.ToolCallID {
	if as.replayKey == "" {
		return acp.ToolCallID(fmt.Sprintf("hist-t%d-%d", i, j))
	}
	return acp.ToolCallID(fmt.Sprintf("hist-%s-t%d-%d", as.replayKey, i, j))
}

// replayToolCalls re-emits history turn i's tool calls as completed
// tool_call_update rows (hist-t<i>-<j>), each rawOutput paired with the
// result from the nearest following RoleToolResult turn (matched by
// ToolCall.ID when the result carries one, positional otherwise).
func (as *acpSession) replayToolCalls(hist []session.Turn, i int) {
	t := hist[i]
	if len(t.ToolCalls) == 0 {
		return
	}
	results := toolResultsAfter(hist, i)
	used := make([]bool, len(results))
	for j, tc := range t.ToolCalls {
		var rawOutput any
		if k := matchToolResult(results, used, tc.ID); k >= 0 {
			rawOutput = boundReplayText(results[k].Content)
		}
		var rawInput any
		if tc.Arguments != "" {
			rawInput = tc.Arguments
			var parsed any
			if err := json.Unmarshal([]byte(tc.Arguments), &parsed); err == nil {
				rawInput = parsed // structured when the arguments were JSON
			}
		}
		as.notify(acp.ToolCallUpdate{
			SessionUpdate: "tool_call_update",
			ToolCallID:    as.replayToolID(i, j),
			Name:          tc.Name,
			Title:         tc.Name,
			Kind:          acp.ToolKindFor(tc.Name),
			Status:        acp.ToolCallCompleted,
			RawInput:      rawInput,
			RawOutput:     rawOutput,
		})
	}
}

// toolResultsAfter collects the RoleToolResult turns of the tool-call batch
// that started at hist[i]: everything after it up to the next user turn or the
// next assistant turn carrying calls.
func toolResultsAfter(hist []session.Turn, i int) []session.Turn {
	var out []session.Turn
	for k := i + 1; k < len(hist); k++ {
		t := hist[k]
		switch t.Role {
		case session.RoleUser:
			return out
		case session.RoleAssistant:
			if len(t.ToolCalls) > 0 {
				return out
			}
		case session.RoleToolResult:
			out = append(out, t)
		}
	}
	return out
}

// matchToolResult picks the result for callID: a result carrying an explicit
// ID must match it, an ID-less result pairs positionally (first unused).
// Returns the index into results, or -1 when the call has no recorded result.
func matchToolResult(results []session.Turn, used []bool, callID string) int {
	for k, r := range results {
		if used[k] {
			continue
		}
		if len(r.ToolCalls) > 0 && r.ToolCalls[0].ID != "" && r.ToolCalls[0].ID != callID {
			continue
		}
		used[k] = true
		return k
	}
	return -1
}

// emitHistoryMessage sends one replayed or echoed message in the vocabulary
// the client understands: v2 message upserts (same messageId patches — what
// makes replay idempotent) or, for a v1 client with no upserts, content chunks
// (same messageId appends into one message).
func (as *acpSession) emitHistoryMessage(kind histMsgKind, id acp.MessageID, text string) {
	if as.v1Client {
		switch kind {
		case histUser:
			as.notify(acp.UserMessageChunk(id, text))
		case histAgent:
			as.notify(acp.AgentMessageChunk(id, text))
		case histThought:
			as.notify(acp.AgentThoughtChunk(id, text))
		}
		return
	}
	switch kind {
	case histUser:
		as.notify(acp.UserMessageUpsert(id, text))
	case histAgent:
		as.notify(acp.AgentMessageUpsert(id, text))
	case histThought:
		as.notify(acp.AgentThoughtUpsert(id, text))
	}
}

// boundReplayText caps one replayed message at replayMaxMessageChars.
func boundReplayText(s string) string { return textbudget.SummarizeLong(s, replayMaxMessageChars) }
