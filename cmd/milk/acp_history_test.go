package main

// Replay-delivery pins for the ACP standard chat-history mechanism
// (docs/acp-session-resume-plan.md §4/D5): replay emitted before the
// session/resume response, deterministic message identity across replays,
// runID-suffixed live IDs, the bounded head+tail window with its gap marker,
// tool-trail pairing, and the v1-client chunk fallback. Method/lifecycle pins
// live in acp_sessionmgmt_test.go.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/transport/acp"
)

// sentUpdates flattens the recorded session/update payloads in send order.
func sentUpdates(t *testing.T, conn *fakeACPConn) []acp.SessionUpdate {
	t.Helper()
	var out []acp.SessionUpdate
	for _, n := range conn.sent() {
		if wrapped, ok := n.Params.(acp.UpdateSessionNotification); ok {
			out = append(out, wrapped.Update)
		}
	}
	return out
}

// upsertByID finds the message upsert with the given messageId.
func upsertByID(updates []acp.SessionUpdate, id acp.MessageID) (acp.MessageUpsert, int, bool) {
	for i, u := range updates {
		if m, ok := u.(acp.MessageUpsert); ok && m.MessageID == id {
			return m, i, true
		}
	}
	return acp.MessageUpsert{}, -1, false
}

func upsertText(m acp.MessageUpsert) string {
	if len(m.Content) == 0 {
		return ""
	}
	return m.Content[0].Text
}

func TestACPHistory_ReplayStartBeforeResponse(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "", time.Now(), userTurn("hello"), assistantTurn("hi"))

	result := acpRequest(t, server, "session/resume", acp.ResumeSessionRequest{
		SessionID:  acp.SessionID(sess.ID),
		CWD:        cwd,
		ReplayFrom: json.RawMessage(`{"type":"start"}`),
	})
	if _, ok := result.(resumeResult); !ok {
		t.Fatalf("session/resume result = %T, want resumeResult", result)
	}

	// The whole replay is already on the wire now that HandleRequest has
	// returned — the transport writes the response only afterwards, so this
	// pins the schema's "replay … before responding" ordering.
	updates := sentUpdates(t, conn)
	user, uAt, okU := upsertByID(updates, "hist-u0")
	agent, aAt, okA := upsertByID(updates, "hist-a1")
	if !okU || user.SessionUpdate != "user_message" || upsertText(user) != "hello" {
		t.Fatalf("hist-u0 = %+v (found=%v), want a user_message upsert carrying %q", user, okU, "hello")
	}
	if !okA || agent.SessionUpdate != "agent_message" || upsertText(agent) != "hi" {
		t.Fatalf("hist-a1 = %+v (found=%v), want an agent_message upsert carrying %q", agent, okA, "hi")
	}
	if uAt > aAt {
		t.Errorf("replay out of order: hist-u0 at %d, hist-a1 at %d", uAt, aAt)
	}
}

func TestACPHistory_ReplayIdentityStable(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "", time.Now(), userTurn("one"), assistantTurn("two"))

	acpRequest(t, server, "session/resume", acp.ResumeSessionRequest{
		SessionID:  acp.SessionID(sess.ID),
		CWD:        cwd,
		ReplayFrom: json.RawMessage(`{"type":"start"}`),
	})
	as := server.session(acp.SessionID(sess.ID))
	if as == nil {
		t.Fatal("session/resume did not register an acpSession")
	}
	first := sentUpdates(t, conn)

	// A second replay must emit the same deterministic hist-* IDs so the
	// client's upsert semantics patch rather than duplicate.
	as.replayHistory()
	second := sentUpdates(t, conn)[len(first):]

	var ids1, ids2 []string
	for _, u := range first {
		if m, ok := u.(acp.MessageUpsert); ok {
			ids1 = append(ids1, string(m.MessageID))
		}
	}
	for _, u := range second {
		if m, ok := u.(acp.MessageUpsert); ok {
			ids2 = append(ids2, string(m.MessageID))
		}
	}
	if strings.Join(ids1, ",") != strings.Join(ids2, ",") {
		t.Errorf("replay messageIds drifted: %v vs %v", ids1, ids2)
	}
	if len(ids1) == 0 {
		t.Fatal("replay emitted no message upserts")
	}
}

func TestACPHistory_LiveIDsRunSuffixed(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	cwdA, cwdB := t.TempDir(), t.TempDir()
	respA := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwdA}).(acp.NewSessionResponse)
	respB := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwdB}).(acp.NewSessionResponse)
	asA := server.session(respA.SessionID)
	asB := server.session(respB.SessionID)
	if asA == nil || asB == nil {
		t.Fatal("session/new did not register acpSessions")
	}

	// The per-acpSession runID suffix keeps live IDs from colliding with
	// pre-restart live IDs — and two sessions' counters both start at 1, so
	// without it these two would be equal.
	a, b := asA.liveID("msg"), asB.liveID("msg")
	if a == b {
		t.Errorf("live IDs collide across sessions: %q == %q (runID suffix missing?)", a, b)
	}
	if again := asA.liveID("msg"); again == a {
		t.Errorf("liveID repeated within a session: %q", again)
	}
	for _, id := range []acp.MessageID{a, b} {
		if strings.HasPrefix(string(id), "hist-") {
			t.Errorf("live ID %q can collide with replayed hist-* IDs", id)
		}
	}
}

func TestACPHistory_ReplayBoundsWindowAndGapMarker(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "", time.Now(), userTurn("seed"))
	acpRequest(t, server, "session/resume", acp.ResumeSessionRequest{
		SessionID: acp.SessionID(sess.ID), CWD: cwd,
	})
	as := server.session(acp.SessionID(sess.ID))
	if as == nil {
		t.Fatal("session/resume did not register an acpSession")
	}

	// Synthetic oversized history, driven through the test seam.
	n := replayHeadTurns + replayTailTurns + 5
	hist := make([]session.Turn, n)
	for i := range hist {
		hist[i] = userTurn(fmt.Sprintf("turn %d", i))
	}
	before := len(conn.sent())
	as.replayTurns(hist)
	updates := sentUpdates(t, conn)[before:]

	head, _, okHead := upsertByID(updates, histMsgID("u", 0))
	if !okHead || upsertText(head) != "turn 0" {
		t.Errorf("head turn missing from the replay window: %+v (found=%v)", head, okHead)
	}
	tail, _, okTail := upsertByID(updates, histMsgID("u", n-1))
	if !okTail || upsertText(tail) != fmt.Sprintf("turn %d", n-1) {
		t.Errorf("tail turn missing from the replay window: %+v (found=%v)", tail, okTail)
	}
	// The omitted window is turns replayHeadTurns .. n-replayTailTurns-1.
	omittedMid := replayHeadTurns + (n-replayHeadTurns-replayTailTurns)/2
	if _, _, okMid := upsertByID(updates, histMsgID("u", omittedMid)); okMid {
		t.Errorf("turn %d was replayed, want it inside the omitted window", omittedMid)
	}
	if _, _, okEdge := upsertByID(updates, histMsgID("u", n-replayTailTurns)); !okEdge {
		t.Errorf("tail window start (turn %d) missing from the replay", n-replayTailTurns)
	}
	gap, _, okGap := upsertByID(updates, "hist-gap")
	if !okGap {
		t.Fatal("no hist-gap marker message — the omitted range is silent")
	}
	gapText := upsertText(gap)
	if want := fmt.Sprintf("%d earlier turns omitted", n-replayHeadTurns-replayTailTurns); !strings.Contains(gapText, want) {
		t.Errorf("gap marker %q does not mention %q", gapText, want)
	}
	if !strings.Contains(gapText, "/export") {
		t.Errorf("gap marker %q does not point at /export", gapText)
	}
}

func TestACPHistory_ToolCallsPairedWithResults(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "", time.Now(), userTurn("seed"))
	acpRequest(t, server, "session/resume", acp.ResumeSessionRequest{
		SessionID: acp.SessionID(sess.ID), CWD: cwd,
	})
	as := server.session(acp.SessionID(sess.ID))
	if as == nil {
		t.Fatal("session/resume did not register an acpSession")
	}

	hist := []session.Turn{
		userTurn("read it"),
		{Role: session.RoleAssistant, Timestamp: time.Now(),
			ToolCalls: []session.ToolCall{{ID: "call-1", Name: "read_file", Arguments: `{"path":"x.txt"}`}}},
		{Role: session.RoleToolResult, Timestamp: time.Now(),
			ToolCalls: []session.ToolCall{{ID: "call-1"}}, Content: "file body"},
		assistantTurn("done"),
	}
	before := len(conn.sent())
	as.replayTurns(hist)

	var tool *acp.ToolCallUpdate
	for _, u := range sentUpdates(t, conn)[before:] {
		if tc, ok := u.(acp.ToolCallUpdate); ok {
			tool = &tc
		}
	}
	if tool == nil {
		t.Fatal("replay emitted no tool_call_update for the history tool trail")
	}
	if tool.ToolCallID != "hist-t1-0" {
		t.Errorf("ToolCallID = %q, want hist-t1-0", tool.ToolCallID)
	}
	if tool.Status != acp.ToolCallCompleted {
		t.Errorf("Status = %q, want completed", tool.Status)
	}
	if tool.RawOutput != "file body" {
		t.Errorf("RawOutput = %v, want the paired tool result %q", tool.RawOutput, "file body")
	}
	input, ok := tool.RawInput.(map[string]any)
	if !ok || input["path"] != "x.txt" {
		t.Errorf("RawInput = %v, want the parsed arguments {path:x.txt}", tool.RawInput)
	}
}

func TestACPHistory_V1ClientGetsChunks(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "", time.Now(), userTurn("hello"))
	acpRequest(t, server, "session/resume", acp.ResumeSessionRequest{
		SessionID: acp.SessionID(sess.ID), CWD: cwd,
	})
	as := server.session(acp.SessionID(sess.ID))
	if as == nil {
		t.Fatal("session/resume did not register an acpSession")
	}

	// v1 has no message upserts: the fallback is chunk-form (same messageId
	// appends into one message), never a v2-only shape.
	as.v1Client = true
	before := len(conn.sent())
	as.replayTurns([]session.Turn{userTurn("hello")})

	found := false
	for _, u := range sentUpdates(t, conn)[before:] {
		if c, ok := u.(acp.ContentChunk); ok && c.SessionUpdate == "user_message_chunk" && c.MessageID == "hist-u0" {
			found = true
			if c.Content.Text != "hello" {
				t.Errorf("chunk text = %q, want %q", c.Content.Text, "hello")
			}
		}
		if _, isUpsert := u.(acp.MessageUpsert); isUpsert {
			t.Errorf("v1 client received a v2 message upsert: %+v", u)
		}
	}
	if !found {
		t.Error("no user_message_chunk replayed for the v1 client")
	}
}
