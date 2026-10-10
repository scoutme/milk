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
	// The launch is recorded as the assistant half of the turn, so the request
	// is not left unanswered in history (the next turn's model would re-issue it).
	if len(sess.History) != 2 || sess.History[0].Role != session.RoleUser || sess.History[1].Role != session.RoleAssistant {
		t.Fatalf("want [user, assistant] history after a workflow-start turn, got %+v", sess.History)
	}
	if !strings.Contains(sess.History[1].Content, `Started workflow "dev"`) {
		t.Errorf("assistant turn should record the launch, got %q", sess.History[1].Content)
	}
}

// A workflow-start turn followed by an ordinary turn must keep user/assistant
// alternating, and the next turn's model must see the real start_workflow call.
func TestRunPrimaryWithSession_WorkflowStart_HistoryAlternatesAndReplaysCall(t *testing.T) {
	ws := &local.WorkflowStartSignal{Name: "swarm", Task: "build it"}
	trail := []session.TrailStep{{Calls: []session.TrailCall{{ID: "c1", Name: "start_workflow", Args: `{"name":"swarm"}`, Result: `{"output":"Starting workflow \"swarm\"."}`}}}}
	sess := &session.Session{ID: "test-session"}
	cfg := config.Config{Agents: []config.AgentConfig{{Name: "test-primary", Provider: "local"}}}
	var out bytes.Buffer

	wf := &flakyExecRunner{name: "test-primary", res: TurnResult{WorkflowStart: ws, Trail: trail}}
	if err := runPrimaryWithSession(context.Background(), cfg, sess, wf, nil, nil,
		"run the swarm", "run the swarm", &out, nil, nil, nil, func(*local.WorkflowStartSignal) {}); err != nil {
		t.Fatal(err)
	}
	next := &flakyExecRunner{name: "test-primary", res: TurnResult{Text: "issue filed"}}
	if err := runPrimaryWithSession(context.Background(), cfg, sess, next, nil, nil,
		"new gh issue", "new gh issue", &out, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	want := []session.Role{session.RoleUser, session.RoleAssistant, session.RoleUser, session.RoleAssistant}
	if len(sess.History) != len(want) {
		t.Fatalf("want %d turns, got %+v", len(want), sess.History)
	}
	for i, r := range want {
		if sess.History[i].Role != r {
			t.Fatalf("turn %d: want role %s, got %s (history %+v)", i, r, sess.History[i].Role, sess.History)
		}
	}

	// What the next turn's model would be sent (before the new user turn is added).
	msgs := buildAgentHistory(&session.Session{History: sess.History[:2]}, 0, session.AgentLocal, "test-primary", false)
	var sawCall bool
	for i, m := range msgs {
		if i > 0 && m.Role == msgs[i-1].Role && m.Role == "user" {
			t.Errorf("two user messages in a row at %d: %+v", i, msgs)
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == "start_workflow" {
				sawCall = true
			}
		}
	}
	if !sawCall {
		t.Errorf("replayed history should contain the start_workflow call, got %+v", msgs)
	}
	if last := msgs[len(msgs)-1]; last.Role != "assistant" || !strings.Contains(last.Content, "Started workflow") {
		t.Errorf("history should end with the launch note, got %+v", last)
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
	if n := len(sess.History); n != 2 || !strings.Contains(sess.History[1].Content, "not started") {
		t.Errorf("without a callback the recorded turn must not claim a launch, got %+v", sess.History)
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
		"hi", "hi", &out, nil, nil, nil, onWorkflowStart); err != nil {
		t.Fatalf("runEscalationWithSession returned error: %v", err)
	}
	if got != ws {
		t.Fatalf("expected onWorkflowStart to be called with the signal, got %+v", got)
	}
	if sess.State != session.StateRouting {
		t.Errorf("want session state ROUTING after a workflow-start turn, got %v", sess.State)
	}
	if n := len(sess.History); n != 2 || sess.History[1].Role != session.RoleAssistant || sess.History[1].Agent != session.AgentEscalation {
		t.Errorf("want [user, escalation assistant] history, got %+v", sess.History)
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
