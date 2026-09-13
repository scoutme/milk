package main

// Regression tests for the dispatch-layer half of start_workflow: when a
// TurnRunner's TurnResult carries WorkflowStart (set by localRunner.Execute
// when Run() returns a *local.WorkflowStartSignal), runPrimaryWithSession /
// runEscalationWithSession must call onWorkflowStart with it rather than
// treating the turn as an ordinary response.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/session"
)

func TestRunPrimaryWithSession_WorkflowStart_CallsCallback(t *testing.T) {
	ws := &local.WorkflowStartSignal{Name: "dev", Task: "ship it", Roles: map[string]string{"designer": "escalation"}}
	runner := &flakyExecRunner{name: "test-primary", res: TurnResult{WorkflowStart: ws}}
	sess := &session.Session{ID: "test-session"}
	cfg := config.Config{Agents: []config.AgentConfig{{Name: "test-primary", Provider: "local"}}}
	var out bytes.Buffer

	var got *local.WorkflowStartSignal
	onWorkflowStart := func(w *local.WorkflowStartSignal) { got = w }

	if err := runPrimaryWithSession(context.Background(), cfg, sess, runner, nil, nil,
		"hi", "hi", &out, nil, nil, nil, onWorkflowStart); err != nil {
		t.Fatalf("runPrimaryWithSession returned error: %v", err)
	}
	if got != ws {
		t.Fatalf("expected onWorkflowStart to be called with the signal, got %+v", got)
	}
	if sess.State != session.StateRouting {
		t.Errorf("want session state ROUTING after a workflow-start turn, got %v", sess.State)
	}
	for _, turn := range sess.History {
		if turn.Role == session.RoleAssistant {
			t.Errorf("expected no assistant turn recorded for a workflow-start turn, got %+v", turn)
		}
	}
}

func TestRunPrimaryWithSession_WorkflowStart_NilCallback_ReportsUnsupported(t *testing.T) {
	ws := &local.WorkflowStartSignal{Name: "dev", Task: "ship it"}
	runner := &flakyExecRunner{name: "test-primary", res: TurnResult{WorkflowStart: ws}}
	sess := &session.Session{ID: "test-session"}
	cfg := config.Config{Agents: []config.AgentConfig{{Name: "test-primary", Provider: "local"}}}
	var out bytes.Buffer

	// onWorkflowStart is nil, matching single-prompt CLI mode (main.go) — must
	// not panic, and must tell the user this path isn't supported there.
	if err := runPrimaryWithSession(context.Background(), cfg, sess, runner, nil, nil,
		"hi", "hi", &out, nil, nil, nil, nil); err != nil {
		t.Fatalf("runPrimaryWithSession returned error: %v", err)
	}
	if !strings.Contains(out.String(), "only supported in the TUI") {
		t.Errorf("expected an explanatory message in transcript output, got %q", out.String())
	}
}

func TestRunEscalationWithSession_WorkflowStart_CallsCallback(t *testing.T) {
	ws := &local.WorkflowStartSignal{Name: "pair", Task: "review this"}
	runner := &flakyExecRunner{name: "test-escalation", res: TurnResult{WorkflowStart: ws}}
	sess := &session.Session{ID: "test-session"}
	cfg := config.Config{}
	var out bytes.Buffer

	var got *local.WorkflowStartSignal
	onWorkflowStart := func(w *local.WorkflowStartSignal) { got = w }

	if err := runEscalationWithSession(context.Background(), cfg, sess, runner, "", nil,
		"hi", "hi", "", &out, nil, nil, nil, onWorkflowStart); err != nil {
		t.Fatalf("runEscalationWithSession returned error: %v", err)
	}
	if got != ws {
		t.Fatalf("expected onWorkflowStart to be called with the signal, got %+v", got)
	}
	if sess.State != session.StateRouting {
		t.Errorf("want session state ROUTING after a workflow-start turn, got %v", sess.State)
	}
}

func bytesContains(b []byte, sub string) bool {
	return string(b) != "" && (func() bool {
		return len(b) >= len(sub) && indexOf(string(b), sub) >= 0
	})()
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
