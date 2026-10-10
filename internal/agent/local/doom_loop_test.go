package local

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

// canned server: returns the same "create_task" tool call for the first n
// requests, then a plain final text response afterward.
func doomLoopServer(n int32) (*httptest.Server, *int32) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if count <= n {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc","function":{"name":"create_task","arguments":"{\"title\":\"same\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	return srv, &requests
}

func TestDoomLoopGate_InteractiveApprove_ContinuesAndResets(t *testing.T) {
	srv, requests := doomLoopServer(3)
	defer srv.Close()

	var asked int32
	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 15})
	agent = agent.WithPermissions(nil, func(tool, summary string) bool {
		atomic.AddInt32(&asked, 1)
		if tool != "doom_loop" {
			t.Errorf("expected the ask to be for tool %q, got %q", "doom_loop", tool)
		}
		return true // approve — let the loop continue
	})
	sess := &session.Session{}
	var out strings.Builder

	history, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if atomic.LoadInt32(&asked) != 1 {
		t.Errorf("expected exactly one doom_loop ask, got %d", asked)
	}
	last := history[len(history)-1]
	if !strings.Contains(last.Content, "done") {
		t.Errorf("expected the turn to complete normally after approval, got %q", last.Content)
	}
	if got := atomic.LoadInt32(requests); got != 4 {
		t.Errorf("expected 4 requests (3 identical + 1 final), got %d", got)
	}
}

// TestDoomLoopGate_InteractiveDeny_Escalates: a denial is itself a strong
// signal the primary model isn't handling the task well, so — as of
// docs/escalation-and-context-enhancements-plan.md Track A Phase 3 — a
// denied doom_loop ask escalates to a more capable agent instead of just
// terminating the turn, whenever a real escalation path exists (ordinary
// live turn: not a workflow step, not a background job, not a tool-agent
// call — see canEscalate).
func TestDoomLoopGate_InteractiveDeny_Escalates(t *testing.T) {
	srv, requests := doomLoopServer(10) // would keep repeating well past 3 if not stopped
	defer srv.Close()

	var asked int32
	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 15})
	agent = agent.WithPermissions(nil, func(tool, summary string) bool {
		atomic.AddInt32(&asked, 1)
		return false // deny
	})
	sess := &session.Session{}
	var out strings.Builder

	_, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil)
	esc, ok := err.(*EscalationSignal)
	if !ok {
		t.Fatalf("expected an *EscalationSignal, got %v (%T)", err, err)
	}
	if !strings.Contains(esc.Reason, "the user declined to let it continue") {
		t.Errorf("expected the escalation reason to explain the denial, got %q", esc.Reason)
	}
	if atomic.LoadInt32(&asked) != 1 {
		t.Errorf("expected exactly one doom_loop ask, got %d", asked)
	}
	if got := atomic.LoadInt32(requests); got != 3 {
		t.Errorf("expected exactly 3 requests before stopping, got %d", got)
	}
}

// TestDoomLoopGate_InteractiveDeny_NoEscalationPath_StillTerminates covers
// the one case where a denial can't escalate even though a*permAsk callback
// exists: a stateless tool-agent call (RunToolCall) has no session/runner
// behind it to escalate into (see canEscalate), so it must keep today's
// plain termination behavior.
func TestDoomLoopGate_InteractiveDeny_NoEscalationPath_StillTerminates(t *testing.T) {
	srv, requests := doomLoopServer(10)
	defer srv.Close()

	var asked int32
	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 15}).WithToolAgentRole()
	agent = agent.WithPermissions(nil, func(tool, summary string) bool {
		atomic.AddInt32(&asked, 1)
		return false // deny
	})
	sess := &session.Session{}
	var out strings.Builder

	history, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	last := history[len(history)-1]
	if !strings.Contains(last.Content, "the user declined to let it continue") {
		t.Errorf("expected a user-declined termination message, got %q", last.Content)
	}
	if got := atomic.LoadInt32(requests); got != 3 {
		t.Errorf("expected exactly 3 requests before stopping, got %d", got)
	}
}

func TestDoomLoopGate_BackgroundJob_FailsClosedWithoutAsking(t *testing.T) {
	srv, requests := doomLoopServer(10)
	defer srv.Close()

	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 15})
	agent = agent.WithPermissions(nil, func(tool, summary string) bool {
		t.Fatal("permAsk must not be called for a background job — there's no one to ask")
		return false
	})
	agent.jobID = "test-job"
	sess := &session.Session{}
	var out strings.Builder

	history, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	last := history[len(history)-1]
	if !strings.Contains(last.Content, "this context has no way to ask for confirmation") {
		t.Errorf("expected a fail-closed termination message, got %q", last.Content)
	}
	if got := atomic.LoadInt32(requests); got != 3 {
		t.Errorf("expected exactly 3 requests before stopping, got %d", got)
	}
}

func TestDoomLoopGate_DifferentToolCalls_NeverFires(t *testing.T) {
	// Each request returns a DIFFERENT tool call, then a final response —
	// the doom-loop gate must never fire when calls aren't identical.
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if count <= 3 {
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc","function":{"name":"create_task","arguments":"{\"title\":\"task-%d\"}"}}]}}]}`+"\n\n", count)
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 15})
	agent = agent.WithPermissions(nil, func(tool, summary string) bool {
		t.Fatal("permAsk must not be called when tool calls differ across iterations")
		return false
	})
	sess := &session.Session{}
	var out strings.Builder

	history, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	last := history[len(history)-1]
	if !strings.Contains(last.Content, "done") {
		t.Errorf("expected the turn to complete normally, got %q", last.Content)
	}
}

// TestDoomLoopGate_SessionIDIsLoggingOnly guards the ctx/sessionID parameters
// threaded into loopRecoveryAction (and the doom-loop gate's own logging) for
// observability — a session ID must only ever be attributed on a log/metric
// line, never change the gate's actual allow/deny/escalate/terminate
// decision. A real (non-empty) session ID here must behave identically to
// the ""-ID case covered by TestDoomLoopGate_InteractiveDeny_Escalates.
func TestDoomLoopGate_SessionIDIsLoggingOnly(t *testing.T) {
	srv, requests := doomLoopServer(10)
	defer srv.Close()

	var asked int32
	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 15})
	agent = agent.WithPermissions(nil, func(tool, summary string) bool {
		atomic.AddInt32(&asked, 1)
		return false // deny
	})
	sess := &session.Session{ID: "sess-with-a-real-id"}
	var out strings.Builder

	_, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil)
	esc, ok := err.(*EscalationSignal)
	if !ok {
		t.Fatalf("expected an *EscalationSignal, got %v (%T)", err, err)
	}
	if !strings.Contains(esc.Reason, "the user declined to let it continue") {
		t.Errorf("expected the escalation reason to explain the denial, got %q", esc.Reason)
	}
	if atomic.LoadInt32(&asked) != 1 {
		t.Errorf("expected exactly one doom_loop ask, got %d", asked)
	}
	if got := atomic.LoadInt32(requests); got != 3 {
		t.Errorf("expected exactly 3 requests before stopping, got %d", got)
	}
}

// TestDoomLoopGate_Unattended_RemoteAsk_Approved: in an unattended context
// (background job, workflow step) the doom-loop gate asks the remote
// oversight surface — never the local permAsk — and an approval lets the turn
// continue past the 3 identical calls.
func TestDoomLoopGate_Unattended_RemoteAsk_Approved(t *testing.T) {
	srv, requests := doomLoopServer(3)
	defer srv.Close()

	var asked, perm int32
	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 15})
	agent = agent.WithPermissions(nil, func(tool, summary string) bool {
		atomic.AddInt32(&perm, 1)
		return true
	})
	agent.jobID = "test-job"
	agent = agent.WithRemoteSafetyAsk(func(tool, summary string) (bool, bool) {
		atomic.AddInt32(&asked, 1)
		if tool != "doom_loop" {
			t.Errorf("expected the ask to be for tool %q, got %q", "doom_loop", tool)
		}
		if summary != doomLoopAskSummary {
			t.Errorf("ask summary = %q, want %q", summary, doomLoopAskSummary)
		}
		return true, true // remote oversight approved
	})
	sess := &session.Session{}
	var out strings.Builder

	history, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	last := history[len(history)-1]
	if !strings.Contains(last.Content, "done") {
		t.Errorf("expected the turn to complete normally after remote approval, got %q", last.Content)
	}
	if got := atomic.LoadInt32(&asked); got != 1 {
		t.Errorf("expected exactly one doom_loop ask, got %d", got)
	}
	if got := atomic.LoadInt32(&perm); got != 0 {
		t.Errorf("permAsk must not be called for a background job, got %d calls", got)
	}
	if got := atomic.LoadInt32(requests); got != 4 {
		t.Errorf("expected 4 requests (3 identical + 1 final), got %d", got)
	}
}

// TestDoomLoopGate_Unattended_RemoteAsk_Declined: a remote decline (or the
// remote prompt timing out) stops the turn — the fail-closed ladder — with
// wording that admits remote oversight was asked instead of claiming there
// was no one to ask.
func TestDoomLoopGate_Unattended_RemoteAsk_Declined(t *testing.T) {
	srv, requests := doomLoopServer(10)
	defer srv.Close()

	var asked int32
	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 15})
	agent = agent.WithPermissions(nil, func(tool, summary string) bool {
		t.Fatal("permAsk must not be called for a background job")
		return false
	})
	agent.jobID = "test-job"
	agent = agent.WithRemoteSafetyAsk(func(tool, summary string) (bool, bool) {
		atomic.AddInt32(&asked, 1)
		return true, false // asked remotely, declined (or timed out)
	})
	sess := &session.Session{}
	var out strings.Builder

	history, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	last := history[len(history)-1]
	if !strings.Contains(last.Content, "remote oversight declined or timed out") {
		t.Errorf("expected a remote-declined termination message, got %q", last.Content)
	}
	if got := atomic.LoadInt32(&asked); got != 1 {
		t.Errorf("expected exactly one doom_loop ask, got %d", got)
	}
	if got := atomic.LoadInt32(requests); got != 3 {
		t.Errorf("expected exactly 3 requests before stopping, got %d", got)
	}
}

// TestDoomLoopGate_Unattended_NoRemote_FailsClosedWithoutAsking: a workflow
// step whose remote seam reports "nobody was asked" (no remote oversight
// active) keeps the original fail-closed wording and asks nobody.
func TestDoomLoopGate_Unattended_NoRemote_FailsClosedWithoutAsking(t *testing.T) {
	srv, requests := doomLoopServer(10)
	defer srv.Close()

	var asked int32
	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 15})
	agent = agent.WithPermissions(nil, func(tool, summary string) bool {
		t.Fatal("permAsk must not be called for a workflow step")
		return false
	})
	agent = agent.AsWorkflowExecutor()
	agent = agent.WithRemoteSafetyAsk(func(tool, summary string) (bool, bool) {
		atomic.AddInt32(&asked, 1)
		return false, false // no remote surface active
	})
	sess := &session.Session{}
	var out strings.Builder

	history, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	last := history[len(history)-1]
	if !strings.Contains(last.Content, "this context has no way to ask for confirmation") {
		t.Errorf("expected the fail-closed termination message, got %q", last.Content)
	}
	if got := atomic.LoadInt32(&asked); got != 1 {
		t.Errorf("expected the remote seam to be consulted exactly once, got %d", got)
	}
	if got := atomic.LoadInt32(requests); got != 3 {
		t.Errorf("expected exactly 3 requests before stopping, got %d", got)
	}
}
