package main

// Pins for the session-management JSON-RPC surface (session/list, resume,
// close, delete) — docs/acp-session-resume-plan.md §4's method/lifecycle
// cases. Replay-delivery pins live in acp_history_test.go (step 3) and the
// resume-by-default adoption pins in acp_resume_default_test.go (step 4).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/transport/acp"
)

// seedSession persists a session with the given name and history under the
// test HOME, with LastUsed forced so listings order deterministically.
func seedSession(t *testing.T, cwd, name string, lastUsed time.Time, turns ...session.Turn) *session.Session {
	t.Helper()
	sess, err := session.New(cwd, name)
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	sess.Name = name
	sess.LastUsed = lastUsed
	sess.History = append(sess.History, turns...)
	if err := session.Save(sess); err != nil {
		t.Fatalf("session.Save: %v", err)
	}
	return sess
}

func userTurn(text string) session.Turn {
	return session.Turn{Role: session.RoleUser, Content: text, Timestamp: time.Now()}
}

func assistantTurn(text string) session.Turn {
	return session.Turn{Role: session.RoleAssistant, Content: text, Timestamp: time.Now()}
}

// wantCoded asserts err carries exactly the given JSON-RPC code.
func wantCoded(t *testing.T, err error, code int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a coded error %d, got nil", code)
	}
	var ce *acp.CodedError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want *acp.CodedError", err, err)
	}
	if ce.Code != code {
		t.Errorf("error code = %d, want %d (%s)", ce.Code, code, ce.Message)
	}
}

// acpRequestErr issues a request and returns the raw error (for negative pins).
func acpRequestErr(t *testing.T, s *acpServer, method string, params any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return s.HandleRequest(context.Background(), method, raw)
}

func TestACPServer_SessionList_SortedFilteredTitled(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	base := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	cwdA, cwdB := t.TempDir(), t.TempDir()

	seedSession(t, cwdA, "older", base)
	seedSession(t, cwdA, "", base.Add(2*time.Minute), userTurn("fix   the thing"), assistantTurn("done"))
	seedSession(t, cwdB, "other cwd", base.Add(time.Minute))

	result := acpRequest(t, server, "session/list", acp.ListSessionsRequest{})
	list := result.(acp.ListSessionsResponse)
	if len(list.Sessions) != 3 {
		t.Fatalf("got %d sessions, want 3: %+v", len(list.Sessions), list.Sessions)
	}
	// Sorted by last use descending across cwds (the unnamed one takes its
	// title from its first user turn).
	wantOrder := []string{"fix the thing", "other cwd", "older"}
	for i, want := range wantOrder {
		if list.Sessions[i].Title != want {
			t.Errorf("session[%d].Title = %q, want %q", i, list.Sessions[i].Title, want)
		}
	}
	if list.NextCursor != nil {
		t.Errorf("NextCursor = %v, want absent on the last page", *list.NextCursor)
	}

	// Title fallback: whitespace-collapsed first user turn ("fix   the thing").
	if got := list.Sessions[0].Title; got != "fix the thing" {
		t.Errorf("fallback title = %q, want %q", got, "fix the thing")
	}
	if list.Sessions[0].UpdatedAt == "" {
		t.Error("UpdatedAt is empty, want an RFC 3339 timestamp")
	}
	if _, err := time.Parse(time.RFC3339, list.Sessions[0].UpdatedAt); err != nil {
		t.Errorf("UpdatedAt %q is not RFC 3339: %v", list.Sessions[0].UpdatedAt, err)
	}

	// cwd filter.
	result = acpRequest(t, server, "session/list", acp.ListSessionsRequest{CWD: cwdB})
	filtered := result.(acp.ListSessionsResponse)
	if len(filtered.Sessions) != 1 || filtered.Sessions[0].CWD != cwdB {
		t.Fatalf("cwd filter returned %+v, want exactly the %s session", filtered.Sessions, cwdB)
	}
}

func TestACPServer_SessionList_LongTitleTruncated(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	cwd := t.TempDir()
	long := ""
	for i := 0; i < 20; i++ {
		long += "abcdefghij"
	} // 200 chars
	seedSession(t, cwd, "", time.Now(), userTurn(long))

	result := acpRequest(t, server, "session/list", acp.ListSessionsRequest{CWD: cwd})
	list := result.(acp.ListSessionsResponse)
	if len(list.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(list.Sessions))
	}
	title := list.Sessions[0].Title
	if r := []rune(title); len(r) != 61 { // 60 runes + ellipsis
		t.Errorf("title length = %d, want 61 (%q)", len(r), title)
	}
	if title[len(title)-3:] != "…" {
		t.Errorf("title %q does not end in an ellipsis", title)
	}
}

func TestACPServer_SessionList_CursorPages(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	cwd := t.TempDir()
	base := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 52; i++ {
		seedSession(t, cwd, fmt.Sprintf("s%02d", i), base.Add(time.Duration(i)*time.Minute))
	}

	result := acpRequest(t, server, "session/list", acp.ListSessionsRequest{CWD: cwd})
	page1 := result.(acp.ListSessionsResponse)
	if len(page1.Sessions) != 50 {
		t.Fatalf("page 1 has %d sessions, want 50", len(page1.Sessions))
	}
	if page1.NextCursor == nil {
		t.Fatal("page 1 has no nextCursor, want one (52 sessions, page size 50)")
	}
	if got := page1.Sessions[0].Title; got != "s51" {
		t.Errorf("page 1 starts at %q, want s51 (last used desc)", got)
	}

	result = acpRequest(t, server, "session/list", acp.ListSessionsRequest{CWD: cwd, Cursor: page1.NextCursor})
	page2 := result.(acp.ListSessionsResponse)
	if len(page2.Sessions) != 2 {
		t.Fatalf("page 2 has %d sessions, want 2", len(page2.Sessions))
	}
	if page2.NextCursor != nil {
		t.Errorf("page 2 nextCursor = %v, want absent on the last page", *page2.NextCursor)
	}
	if got := page2.Sessions[0].Title; got != "s01" {
		t.Errorf("page 2 starts at %q, want s01", got)
	}
}

func TestACPServer_SessionList_InvalidCursor(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	bad := acp.SessionListCursor("not-base64-o:n")
	_, err := acpRequestErr(t, server, "session/list", acp.ListSessionsRequest{Cursor: &bad})
	wantCoded(t, err, acp.CodeInvalidParams)
}

func TestACPServer_SessionResume_HappyPath(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "my task", time.Now(), userTurn("hello"), assistantTurn("hi"))

	result := acpRequest(t, server, "session/resume", acp.ResumeSessionRequest{
		SessionID: acp.SessionID(sess.ID), CWD: cwd,
	})
	resp, ok := result.(resumeResult)
	if !ok {
		t.Fatalf("session/resume result = %T, want resumeResult", result)
	}
	if resp.sessionID != acp.SessionID(sess.ID) {
		t.Errorf("resumed sessionID = %q, want %q", resp.sessionID, sess.ID)
	}
	if len(resp.AvailableCommands) == 0 {
		t.Error("AvailableCommands is empty, want milk's slash commands")
	}

	as := server.session(acp.SessionID(sess.ID))
	if as == nil {
		t.Fatal("session/resume did not register an acpSession")
	}
	if len(as.sess.History) != 2 {
		t.Errorf("resumed history has %d turns, want 2", len(as.sess.History))
	}

	// Re-issuing resume for an open session is an idempotent reattach: the
	// same instance, never rebuilt.
	result = acpRequest(t, server, "session/resume", acp.ResumeSessionRequest{
		SessionID: acp.SessionID(sess.ID), CWD: cwd,
	})
	if got := server.session(acp.SessionID(sess.ID)); got != as {
		t.Errorf("second resume rebuilt the session (%p != %p)", got, as)
	}
	_ = conn
}

func TestACPServer_SessionResume_Errors(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "", time.Now(), userTurn("hi"))

	// Unknown session → -32002 resource not found.
	_, err := acpRequestErr(t, server, "session/resume", acp.ResumeSessionRequest{
		SessionID: "does-not-exist", CWD: cwd,
	})
	wantCoded(t, err, acp.CodeResourceNotFound)

	// cwd mismatch → -32602 invalid params.
	_, err = acpRequestErr(t, server, "session/resume", acp.ResumeSessionRequest{
		SessionID: acp.SessionID(sess.ID), CWD: t.TempDir(),
	})
	wantCoded(t, err, acp.CodeInvalidParams)
}

func TestACPServer_SessionResume_ReplayCursor(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "", time.Now(), userTurn("hello"), assistantTurn("hi"))

	// replayFrom: null → resume without replaying (zero updates emitted).
	acpRequest(t, server, "session/resume", acp.ResumeSessionRequest{
		SessionID: acp.SessionID(sess.ID), CWD: cwd,
		ReplayFrom: json.RawMessage(`null`),
	})
	if n := len(conn.sent()); n != 0 {
		t.Fatalf("null replayFrom emitted %d notifications, want 0", n)
	}
	// Detach again so the next resume re-registers from the store.
	server.mu.Lock()
	delete(server.sessions, acp.SessionID(sess.ID))
	server.mu.Unlock()

	// An unknown cursor type is rejected before anything is emitted.
	_, err := acpRequestErr(t, server, "session/resume", acp.ResumeSessionRequest{
		SessionID: acp.SessionID(sess.ID), CWD: cwd,
		ReplayFrom: json.RawMessage(`{"type":"later"}`),
	})
	wantCoded(t, err, acp.CodeInvalidParams)
	if n := len(conn.sent()); n != 0 {
		t.Fatalf("unknown replayFrom cursor emitted %d notifications, want 0", n)
	}
}

func TestACPServer_SessionClose(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "", time.Now(), userTurn("hello"))

	result := acpRequest(t, server, "session/resume", acp.ResumeSessionRequest{
		SessionID: acp.SessionID(sess.ID), CWD: cwd,
	})
	as := server.session(acp.SessionID(sess.ID))
	if as == nil {
		t.Fatal("session/resume did not register an acpSession")
	}
	_ = result

	// Simulate a running turn, then close: the turn must be cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	as.mu.Lock()
	as.cancel = cancel
	as.turnCtx = ctx
	as.mu.Unlock()
	as.pendingUserFollowup = true // a follow-up pending at close time is dropped

	if _, err := acpRequestErr(t, server, "session/close", acp.CloseSessionRequest{SessionID: acp.SessionID(sess.ID)}); err != nil {
		t.Fatalf("session/close: %v", err)
	}
	if ctx.Err() == nil {
		t.Error("session/close did not cancel the running turn")
	}
	if server.session(acp.SessionID(sess.ID)) != nil {
		t.Error("closed session is still in the server's session map")
	}
	if !as.closed.Load() {
		t.Error("closed flag not set — background follow-ups would still fire")
	}

	// The session file persists — close ≠ delete.
	if _, err := session.Load(sess.ID); err != nil {
		t.Errorf("session file gone after close: %v", err)
	}

	// Follow-ups requested after close never start.
	before := len(conn.sent())
	as.flushPendingFollowup()
	as.requestFollowup(false)
	if got := len(conn.sent()); got != before {
		t.Errorf("follow-up after close sent %d notifications, want 0", got-before)
	}

	// Idempotent: a second close is still a clean {}.
	res, err := acpRequestErr(t, server, "session/close", acp.CloseSessionRequest{SessionID: acp.SessionID(sess.ID)})
	if err != nil {
		t.Fatalf("second session/close: %v", err)
	}
	if _, ok := res.(acp.CloseSessionResponse); !ok {
		t.Errorf("second close result = %T, want acp.CloseSessionResponse", res)
	}
}

func TestACPServer_SessionDelete(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "", time.Now(), userTurn("hello"))

	// Unknown session → -32002.
	_, err := acpRequestErr(t, server, "session/delete", acp.DeleteSessionRequest{SessionID: "nope"})
	wantCoded(t, err, acp.CodeResourceNotFound)

	// Open the session first: delete must close it (same teardown) before dropping.
	acpRequest(t, server, "session/resume", acp.ResumeSessionRequest{
		SessionID: acp.SessionID(sess.ID), CWD: cwd,
	})
	as := server.session(acp.SessionID(sess.ID))
	if as == nil {
		t.Fatal("session/resume did not register an acpSession")
	}

	if _, err := acpRequestErr(t, server, "session/delete", acp.DeleteSessionRequest{SessionID: acp.SessionID(sess.ID)}); err != nil {
		t.Fatalf("session/delete: %v", err)
	}
	if server.session(acp.SessionID(sess.ID)) != nil {
		t.Error("deleted session is still in the server's session map")
	}
	if !as.closed.Load() {
		t.Error("delete did not close the open session first")
	}
	if _, err := session.Load(sess.ID); !os.IsNotExist(err) {
		t.Errorf("session file still exists after delete (err=%v)", err)
	}
	idx, err := session.List(cwd)
	if err != nil {
		t.Fatalf("session.List: %v", err)
	}
	for _, e := range idx[cwd] {
		if e.ID == sess.ID {
			t.Error("index entry survived session/delete")
		}
	}
}

func TestACPServer_SessionDeleteCapabilityAdvertised(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	result := acpRequest(t, server, "initialize", acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersion})
	resp := result.(acp.InitializeResponse)
	if resp.Capabilities.Session == nil || resp.Capabilities.Session.Delete == nil {
		t.Fatalf("session.delete capability not advertised: %+v", resp.Capabilities.Session)
	}
}
