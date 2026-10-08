package main

// /export session <id|prefix> targeting pins (docs/acp-session-resume-plan.md
// D6/§4): the /list → /export session a1b2 → session/resume preview flow —
// export another session without attaching it, on both hosts, composing with
// json/<path>, with miss/ambiguity errors mirroring session.Lookup. On a
// resumed session /export naturally includes pre-resume turns.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/transport/acp"
)

// scratchSessionStore points the session store at a scratch HOME (session.Save
// must never touch the developer's real ~/.milk/sessions).
func scratchSessionStore(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".milk"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Setenv("HOME", home)
}

// seedSessionWithID persists a session under an explicit ID so the tests can
// pin exact-ID, prefix, and ambiguity behavior deterministically.
func seedSessionWithID(t *testing.T, id, cwd, name string, turns ...session.Turn) *session.Session {
	t.Helper()
	now := time.Now()
	sess := &session.Session{
		ID: id, Name: name, CWD: cwd,
		CreatedAt: now, LastUsed: now,
		State:   session.StateRouting,
		History: turns,
	}
	if err := session.Save(sess); err != nil {
		t.Fatalf("session.Save: %v", err)
	}
	return sess
}

// exportState builds the TUI-side interactiveState handleSlashCommand runs on.
func exportState(sess *session.Session) *interactiveState {
	return &interactiveState{sess: sess, cwd: sess.CWD, notifier: oversight.Noop{}}
}

// runACPExport drives an export command through the ACP slash-command table
// (the exact path session/prompt takes).
func runACPExport(t *testing.T, as *acpSession, prompt string) string {
	t.Helper()
	handled, out, _ := as.runSlashCommand(
		&acpTurn{ctx: context.Background(), as: as, say: func(string) {}}, prompt)
	if !handled {
		t.Fatalf("%q was not handled as a slash command", prompt)
	}
	return out
}

func TestExportSession_TargetsOtherSession_TUIHost(t *testing.T) {
	scratchSessionStore(t)
	cwd := t.TempDir()
	cur := seedSessionWithID(t, "cafe0001", cwd, "current", userTurn("alpha turn"))
	seedSessionWithID(t, "beef1234", cwd, "other", userTurn("beta turn"))

	_, _, out := handleSlashCommand(cmdExport, "session beef", exportState(cur))
	if !strings.Contains(out, "beta turn") {
		t.Errorf("/export session beef did not render the target session: %q", out)
	}
	if strings.Contains(out, "alpha turn") {
		t.Errorf("/export session beef leaked the current session's transcript: %q", out)
	}
}

func TestExportSession_TargetsOtherSession_ACPHost(t *testing.T) {
	server, _ := acpTestServer(t, "ok") // scratch $HOME for the session store
	cwd := t.TempDir()
	resp := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwd}).(acp.NewSessionResponse)
	as := server.session(resp.SessionID)
	if as == nil {
		t.Fatal("session/new did not register an acpSession")
	}
	// Seed after session/new so adoption can't bind the target session.
	seedSessionWithID(t, "beef1234", cwd, "other", userTurn("beta turn"))

	out := runACPExport(t, as, "/export session beef")
	if !strings.Contains(out, "beta turn") {
		t.Errorf("ACP /export session beef did not render the target session: %q", out)
	}
}

func TestExportSession_ComposesWithJSONAndPath(t *testing.T) {
	scratchSessionStore(t)
	cwd := t.TempDir()
	cur := seedSessionWithID(t, "cafe0001", cwd, "current", userTurn("alpha turn"))
	seedSessionWithID(t, "beef1234", cwd, "other", userTurn("beta turn"))
	st := exportState(cur)

	_, _, out := handleSlashCommand(cmdExport, "session beef json", st)
	if !strings.Contains(out, "beta turn") || strings.Contains(out, "alpha turn") {
		t.Errorf("/export session beef json = %q, want the target session's JSON", out)
	}

	path := filepath.Join(t.TempDir(), "dump.txt")
	_, _, out = handleSlashCommand(cmdExport, "session beef "+path, st)
	if !strings.Contains(out, "session exported to") {
		t.Errorf("/export session beef <path> = %q, want the exported-to confirmation", out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("export file: %v", err)
	}
	if !strings.Contains(string(data), "beta turn") || strings.Contains(string(data), "alpha turn") {
		t.Errorf("export file = %q, want the target session's plain transcript", string(data))
	}
}

func TestExportSession_MissAmbiguityAndUsage(t *testing.T) {
	scratchSessionStore(t)
	cwd := t.TempDir()
	cur := seedSessionWithID(t, "cafe0001", cwd, "current", userTurn("alpha turn"))
	seedSessionWithID(t, "aaaa0001", cwd, "", userTurn("one"))
	seedSessionWithID(t, "aaaa0002", cwd, "", userTurn("two"))
	st := exportState(cur)

	_, _, out := handleSlashCommand(cmdExport, "session deadbeef", st)
	if !strings.Contains(out, "not found") {
		t.Errorf("miss = %q, want a not-found error", out)
	}

	_, _, out = handleSlashCommand(cmdExport, "session aaaa", st)
	if !strings.Contains(out, "ambiguous") {
		t.Errorf("ambiguity = %q, want an ambiguous-prefix error, never a guess", out)
	}

	_, _, out = handleSlashCommand(cmdExport, "session", st)
	if !strings.Contains(out, "usage:") {
		t.Errorf("/export session = %q, want a usage line", out)
	}
}

func TestExportSession_ResumedIncludesPreResumeTurns(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	cwd := t.TempDir()
	sess := seedSession(t, cwd, "", time.Now(), userTurn("pre-resume turn"), assistantTurn("and answer"))

	acpRequest(t, server, "session/resume", acp.ResumeSessionRequest{
		SessionID: acp.SessionID(sess.ID), CWD: cwd,
	})
	as := server.session(acp.SessionID(sess.ID))
	if as == nil {
		t.Fatal("session/resume did not register an acpSession")
	}

	out := runACPExport(t, as, "/export")
	for _, want := range []string{"pre-resume turn", "and answer"} {
		if !strings.Contains(out, want) {
			t.Errorf("/export on a resumed session is missing pre-resume content %q: %q", want, out)
		}
	}
}
