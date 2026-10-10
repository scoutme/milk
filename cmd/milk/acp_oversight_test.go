package main

// Tests for remote oversight over ACP (acp_oversight.go): turn/tool
// notification forwarding from a session's run, the client-vs-remote
// permission race, and remote-input routing (including the no-session
// queue). Everything runs against acpTestServer's fake conn with the
// notifier swapped for a recording fake — no Telegram, no polling.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/transport/acp"
)

// fakeNotifier records every oversight call as a "kind:…"-shaped string.
// AskPermission records the request, then either returns a programmed
// decision (permCh) or blocks until its context ends (a client that never
// answers).
type fakeNotifier struct {
	mu     sync.Mutex
	calls  []string
	permCh chan oversight.PermDecision
}

func (f *fakeNotifier) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeNotifier) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeNotifier) NotifyTurnStart(_ context.Context, agent, target, prompt string) {
	f.record("start:" + agent + ":" + target + ":" + prompt)
}
func (f *fakeNotifier) NotifyToolUse(_ context.Context, name, summary string) {
	f.record("tool:" + name + ":" + summary)
}
func (f *fakeNotifier) NotifyToolResult(_ context.Context, name, _ string, isError bool) {
	status := "ok"
	if isError {
		status = "err"
	}
	f.record("toolres:" + name + ":" + status)
}
func (f *fakeNotifier) NotifyTurnDone(_ context.Context, agent string, err error) {
	if err != nil {
		f.record("doneerr:" + agent)
		return
	}
	f.record("done:" + agent)
}
func (f *fakeNotifier) NotifyResponse(_ context.Context, agent, text string) {
	f.record("response:" + agent + ":" + text)
}
func (f *fakeNotifier) AskPermission(ctx context.Context, req oversight.PermRequest) (oversight.PermDecision, bool) {
	f.record("perm:" + req.ToolName)
	if f.permCh != nil {
		select {
		case d := <-f.permCh:
			return d, true
		case <-ctx.Done():
			return oversight.PermTimeout, false
		}
	}
	<-ctx.Done()
	return oversight.PermTimeout, false
}

// setTestNotifier installs a fake under the server's lock — the field is
// read through notifierOrNil (s.mu) by every turn goroutine.
func setTestNotifier(s *acpServer, n oversight.Notifier) {
	s.mu.Lock()
	s.notifier = n
	s.mu.Unlock()
}

// waitForOversightCall polls until a recorded call with the given prefix
// appears (remote input runs on its own goroutine).
func waitForOversightCall(t *testing.T, f *fakeNotifier, prefix string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range f.recorded() {
			if strings.HasPrefix(c, prefix) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("oversight call %q not seen within 3s; recorded: %v", prefix, f.recorded())
}

func hasCall(calls []string, exact string) bool {
	for _, c := range calls {
		if c == exact {
			return true
		}
	}
	return false
}

// A routed model turn forwards start, response and done to the notifier —
// the TUI's runTurn notifications, mirrored.
func TestACPOversight_TurnNotifications(t *testing.T) {
	server, _ := acpTestServer(t, "model reply")
	fake := &fakeNotifier{}
	setTestNotifier(server, fake)

	id := acpNewSession(t, server)
	acpPromptText(t, server, id, "hello there")

	calls := fake.recorded()
	if !hasCall(calls, "start:test-local:local:hello there") {
		t.Errorf("missing NotifyTurnStart; calls: %v", calls)
	}
	if !hasCall(calls, "response:test-local:model reply") {
		t.Errorf("missing final NotifyResponse; calls: %v", calls)
	}
	if !hasCall(calls, "done:test-local") {
		t.Errorf("missing NotifyTurnDone; calls: %v", calls)
	}
}

// Slash-command output reaches remote oversight the way the TUI's
// handleSlashInput forwards it (cmd+": "+output).
func TestACPOversight_SlashOutputNotified(t *testing.T) {
	server, _ := acpTestServer(t, "unused")
	fake := &fakeNotifier{}
	setTestNotifier(server, fake)

	id := acpNewSession(t, server)
	acpPromptText(t, server, id, "/help")

	found := false
	for _, c := range fake.recorded() {
		if strings.HasPrefix(c, "response:milk:/help:") {
			found = true
		}
	}
	if !found {
		t.Errorf("slash output not forwarded; calls: %v", fake.recorded())
	}
}

// Tool-use hooks forward use/result to the notifier, gated by the
// notify_tools config (the TUI's gate).
func TestACPOversight_ToolNotifications(t *testing.T) {
	server, _ := acpTestServer(t, "unused")
	fake := &fakeNotifier{}
	setTestNotifier(server, fake)

	id := acpNewSession(t, server)
	as := server.session(id)

	as.onLocalToolUse("t1", "Bash", "ls -la", map[string]any{"command": "ls -la"})
	as.onLocalToolResult("t1", "Bash", "out", false)
	as.onClaudeToolUseReady("t2", "Read", map[string]any{"file_path": "/tmp/x"})
	as.onClaudeToolResult("t2", "Read", "data", true)

	calls := fake.recorded()
	for _, want := range []string{"tool:Bash:ls -la", "toolres:Bash:ok", "toolres:Read:err"} {
		if !hasCall(calls, want) {
			t.Errorf("missing %q; calls: %v", want, calls)
		}
	}
	summarySeen := false
	for _, c := range calls {
		if strings.HasPrefix(c, "tool:Read:") {
			summarySeen = true
		}
	}
	if !summarySeen {
		t.Errorf("claude tool-use never notified; calls: %v", calls)
	}

	// notify_tools: false silences both directions.
	off := false
	as.st.cfg.RemoteOversight = &config.RemoteOversightConfig{NotifyTools: &off}
	if as.notifyTools() {
		t.Fatal("notifyTools() = true, want false with notify_tools: false")
	}
	before := len(fake.recorded())
	as.onLocalToolUse("t3", "Bash", "pwd", map[string]any{"command": "pwd"})
	if got := len(fake.recorded()); got != before {
		t.Errorf("gate off but %d new calls recorded", got-before)
	}
}

// A message from the bot runs as a turn in the live session: the client
// sees it echo as a user message and the notifier sees the turn.
func TestACPOversight_RemoteInputIdle_RunsTurn(t *testing.T) {
	server, _ := acpTestServer(t, "remote ok")
	fake := &fakeNotifier{}
	setTestNotifier(server, fake)

	acpNewSession(t, server)
	server.handleRemoteInput("  check the build  ")

	waitForOversightCall(t, fake, "start:test-local:local:check the build")
	waitForOversightCall(t, fake, "done:test-local")

	// The message echoed to the client as a user message (message upsert),
	// not silently.
	found := false
	for _, n := range server.conn.(*fakeACPConn).sent() {
		w, ok := n.Params.(acp.UpdateSessionNotification)
		if !ok {
			continue
		}
		up, ok := w.Update.(acp.MessageUpsert)
		if !ok || up.SessionUpdate != "user_message" {
			continue
		}
		for _, b := range up.Content {
			if strings.Contains(b.Text, "check the build") {
				found = true
			}
		}
	}
	if !found {
		t.Error("remote message was not echoed to the client as a user message")
	}
}

// A message arriving while a turn is running is queued and runs at turn
// end (the follow-up queue's turnMu gate).
func TestACPOversight_RemoteInputQueuedWhileBusy(t *testing.T) {
	server, _ := acpTestServer(t, "queued ok")
	fake := &fakeNotifier{}
	setTestNotifier(server, fake)

	id := acpNewSession(t, server)
	as := server.session(id)

	as.turnMu.Lock() // simulate a running turn
	server.handleRemoteInput("later please")
	as.mu.Lock()
	queued := len(as.pendingRemoteInputs)
	as.mu.Unlock()
	if queued != 1 {
		as.turnMu.Unlock()
		t.Fatalf("queued = %d, want 1", queued)
	}

	as.turnMu.Unlock()
	as.flushRemoteInputs()
	waitForOversightCall(t, fake, "start:test-local:local:later please")
}

// A message that arrives before any session exists is queued and drained
// into the next session created (AfterResponse).
func TestACPOversight_RemoteInputQueuedWithoutSession_DrainsOnNew(t *testing.T) {
	server, _ := acpTestServer(t, "drained ok")
	fake := &fakeNotifier{}
	setTestNotifier(server, fake)

	server.handleRemoteInput("no session yet")
	server.mu.Lock()
	queued := len(server.remoteQueue)
	server.mu.Unlock()
	if queued != 1 {
		t.Fatalf("remoteQueue = %d, want 1", queued)
	}

	id := acpNewSession(t, server)
	server.AfterResponse("session/new", acp.NewSessionResponse{SessionID: id})

	server.mu.Lock()
	remaining := len(server.remoteQueue)
	server.mu.Unlock()
	if remaining != 0 {
		t.Errorf("remoteQueue after drain = %d, want 0", remaining)
	}
	waitForOversightCall(t, fake, "start:test-local:local:no session yet")
}

// The permission race (permask.go's askPermission, ADR-0052): Noop/nil
// means client-only; first real answer wins on both sides; a remote deny
// arriving first denies. Foreground policy resolves remote-side silence with
// the remote's timeout action; the background policy treats it as no answer.
func TestACPOversight_PermissionRace(t *testing.T) {
	ctx := context.Background()

	t.Run("noop backend asks client only", func(t *testing.T) {
		fake := &fakeNotifier{} // would record if ever called
		asked := false
		got, src := askPermission(ctx, oversight.Noop{}, oversight.PermRequest{ToolName: "Bash"},
			func(context.Context) bool {
				asked = true
				return true
			}, foregroundAskPolicy)
		if !got || !asked {
			t.Errorf("got=%v asked=%v, want true/true", got, asked)
		}
		if src != permSrcDirect {
			t.Errorf("src = %q, want %q", src, permSrcDirect)
		}
		if calls := fake.recorded(); len(calls) != 0 {
			t.Errorf("unexpected remote calls: %v", calls)
		}
	})

	t.Run("client answer wins over a silent remote", func(t *testing.T) {
		fake := &fakeNotifier{} // blocks until cancelled
		got, src := askPermission(ctx, fake, oversight.PermRequest{ToolName: "Bash"},
			func(context.Context) bool { return true }, foregroundAskPolicy)
		if !got {
			t.Error("askPermission = false, want true (client answered allow)")
		}
		if src != permSrcDirect {
			t.Errorf("src = %q, want %q", src, permSrcDirect)
		}
	})

	t.Run("remote answer wins over a silent client", func(t *testing.T) {
		fake := &fakeNotifier{permCh: make(chan oversight.PermDecision, 1)}
		fake.permCh <- oversight.PermAllow
		got, src := askPermission(ctx, fake, oversight.PermRequest{ToolName: "Bash"},
			func(c context.Context) bool {
				<-c.Done() // client never answers on its own
				return false
			}, foregroundAskPolicy)
		if !got {
			t.Error("askPermission = false, want true (remote answered allow)")
		}
		if src != permSrcRemote {
			t.Errorf("src = %q, want %q", src, permSrcRemote)
		}
	})

	t.Run("remote deny denies", func(t *testing.T) {
		fake := &fakeNotifier{permCh: make(chan oversight.PermDecision, 1)}
		fake.permCh <- oversight.PermDeny
		got, _ := askPermission(ctx, fake, oversight.PermRequest{ToolName: "Bash"},
			func(c context.Context) bool {
				<-c.Done()
				return true
			}, foregroundAskPolicy)
		if got {
			t.Error("askPermission = true, want false (remote denied)")
		}
	})

	t.Run("background ask resolves with the timed default at the deadline", func(t *testing.T) {
		fake := &fakeNotifier{} // remote never replies
		start := time.Now()
		got, src := askPermission(ctx, fake, oversight.PermRequest{ToolName: "bash"},
			func(c context.Context) bool {
				<-c.Done() // nobody answers locally either
				return false
			}, askPolicy{deadline: 30 * time.Millisecond, def: false})
		if got {
			t.Error("askPermission = true, want false (timed default is deny)")
		}
		if src != permSrcDefault {
			t.Errorf("src = %q, want %q", src, permSrcDefault)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("timed answer took %s, want ~30ms", elapsed)
		}
	})

	t.Run("background ask: remote silence does not resolve before the deadline", func(t *testing.T) {
		// The remote's own timeout would answer deny-as-timeout; for
		// background asks that must NOT resolve the ask — the direct surface
		// still gets its window (here: it answers after the remote is gone).
		fake := &fakeNotifier{} // returns (PermTimeout, false) on ctx end only
		answered := make(chan struct{})
		go func() {
			time.Sleep(20 * time.Millisecond)
			close(answered)
		}()
		got, src := askPermission(ctx, fake, oversight.PermRequest{ToolName: "bash"},
			func(c context.Context) bool {
				<-answered
				return true
			}, askPolicy{deadline: 2 * time.Second, def: false})
		if !got {
			t.Error("askPermission = false, want true (direct surface answered after remote silence)")
		}
		if src != permSrcDirect {
			t.Errorf("src = %q, want %q", src, permSrcDirect)
		}
	})

	t.Run("no surface at all resolves immediately with the default", func(t *testing.T) {
		got, src := askPermission(ctx, nil, oversight.PermRequest{ToolName: "bash"},
			nil, askPolicy{deadline: time.Hour, def: true})
		if !got || src != permSrcNoSurface {
			t.Errorf("got=%v src=%q, want true/%q", got, src, permSrcNoSurface)
		}
	})
}
