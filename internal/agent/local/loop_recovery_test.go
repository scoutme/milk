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

// TestRun_DuplicateToolCall_TerminatesEvenForWorkflowRole verifies that
// duplicate-tool-call detection now applies to workflow-role agents too: a
// literal repeat of the same tool call with identical arguments is
// unambiguous evidence of a stuck loop in any calling context, unlike the
// reasoning-based detectors (n-gram, streak, text-loop) which stay
// workflow-gated because reasoning legitimately repeats across workflow
// passes.
func TestRun_DuplicateToolCall_TerminatesEvenForWorkflowRole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Always the same tool + identical arguments — never a final answer.
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc","function":{"name":"create_task","arguments":"{\"title\":\"same\"}"}}]}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 15})
	agent.workflowRole = true
	sess := &session.Session{}
	var out strings.Builder

	history, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	last := history[len(history)-1]
	if !strings.Contains(last.Content, "[turn terminated: the model kept repeating the same tool call") {
		t.Errorf("expected duplicate-tool-call termination for a workflow-role agent, got %q", last.Content)
	}
}

// TestRun_TextLoop_Terminates verifies the fix for a dead termination path:
// the text-loop detector previously had no counter of its own (it borrowed
// streak.recoveryCount, which a varying-tool-args turn never advances) and
// never terminated the turn — textLoopMaxRecovery was declared but unused.
// Vary the tool-call arguments each iteration (so duplicate-tool and
// loop-streak detection never fire) while keeping the visible response text
// identical, isolating text-loop detection.
func TestRun_TextLoop_Terminates(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := n.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Let me check this again."}}]}`+"\n\n")
		fmt.Fprintf(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc%d","function":{"name":"create_task","arguments":"{\"title\":\"t%d\"}"}}]}}]}`+"\n\n", i, i)
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 20})
	sess := &session.Session{}
	var out strings.Builder

	history, err := agent.Run(context.Background(), nil, "keep going", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	last := history[len(history)-1]
	if !strings.Contains(last.Content, "[turn terminated: the model kept repeating identical output text") {
		t.Errorf("expected text-loop termination, got %q", last.Content)
	}
}
