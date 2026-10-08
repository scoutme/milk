package main

// Resume-by-default adoption pins (docs/acp-session-resume-plan.md D2/§4):
// session/new adopts the cwd's most-recent stored session iff acp_resume is
// on, `_meta.milk.fresh` is not true, no live session exists for that cwd in
// this process, the candidate wasn't closed this process, and the store holds
// one — otherwise it behaves exactly as before (fresh).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/transport/acp"
)

func TestACPServer_ResumeByDefault_AdoptsAndReplays(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "", time.Now(), userTurn("hello"), assistantTurn("hi"))

	result := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwd})
	resp := result.(acp.NewSessionResponse)
	if resp.SessionID != acp.SessionID(sess.ID) {
		t.Fatalf("session/new adopted %q, want the cwd's stored session %q", resp.SessionID, sess.ID)
	}

	// Adoption never hides state: mandatory bounded replay (the client's
	// panel is empty) plus a one-line notice pointing at /export.
	updates := sentUpdates(t, conn)
	if user, _, ok := upsertByID(updates, "hist-u0"); !ok || upsertText(user) != "hello" {
		t.Errorf("adoption did not replay the stored history (hist-u0 = %+v, found=%v)", user, ok)
	}
	notice := false
	for _, u := range updates {
		if c, ok := u.(acp.ContentChunk); ok && strings.Contains(c.Content.Text, "resumed session") {
			notice = true
			if !strings.Contains(c.Content.Text, "/export") {
				t.Errorf("resumed-notice %q does not point at /export", c.Content.Text)
			}
		}
	}
	if !notice {
		t.Error("no one-line resumed-session notice — adoption must not hide state")
	}
}

func TestACPServer_ResumeByDefault_SecondNewIsFresh(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "", time.Now(), userTurn("hello"))

	first := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwd}).(acp.NewSessionResponse)
	if first.SessionID != acp.SessionID(sess.ID) {
		t.Fatalf("first session/new = %q, want adopt %q", first.SessionID, sess.ID)
	}

	// A live session now exists for this cwd ("new thread" must keep
	// working inside a running client) — the second session/new is fresh.
	second := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwd}).(acp.NewSessionResponse)
	if second.SessionID == first.SessionID || second.SessionID == acp.SessionID(sess.ID) {
		t.Errorf("second session/new = %q, want a fresh session distinct from %q", second.SessionID, first.SessionID)
	}
}

func TestACPServer_ResumeByDefault_MetaFreshEscape(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "", time.Now(), userTurn("hello"))

	result := acpRequest(t, server, "session/new", acp.NewSessionRequest{
		CWD:  cwd,
		Meta: map[string]any{"milk": map[string]any{"fresh": true}},
	})
	resp := result.(acp.NewSessionResponse)
	if resp.SessionID == acp.SessionID(sess.ID) {
		t.Errorf("_meta.milk.fresh was ignored: adopted %q", resp.SessionID)
	}
}

func TestACPServer_ResumeByDefault_ConfigOff(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "", time.Now(), userTurn("hello"))

	// Flip acp_resume off the way a user would: rewrite the config file
	// (currentConfig re-reads from disk per decision).
	cfgPath := filepath.Join(os.Getenv("HOME"), ".milk", "config.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	m["acp_resume"] = false
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(cfgPath, out, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	result := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwd})
	resp := result.(acp.NewSessionResponse)
	if resp.SessionID == acp.SessionID(sess.ID) {
		t.Error("acp_resume:false was ignored — session/new adopted instead of starting fresh")
	}
}

func TestACPServer_ResumeByDefault_ClosedNotResurrected(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "", time.Now(), userTurn("hello"))

	// Explicitly closed this process: the next session/new must not
	// resurrect the thread (D2.4).
	if _, err := acpRequestErr(t, server, "session/close", acp.CloseSessionRequest{SessionID: acp.SessionID(sess.ID)}); err != nil {
		t.Fatalf("session/close: %v", err)
	}

	result := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwd})
	resp := result.(acp.NewSessionResponse)
	if resp.SessionID == acp.SessionID(sess.ID) {
		t.Error("session/new resurrected a session that was closed this process")
	}
}

func TestACPServer_ResumeByDefault_EmptyStoreStaysFresh(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	cwd := t.TempDir() // no stored sessions for this cwd — condition 5 fails

	result := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwd})
	resp := result.(acp.NewSessionResponse)
	if resp.SessionID == "" {
		t.Error("session/new returned an empty session id")
	}
}
