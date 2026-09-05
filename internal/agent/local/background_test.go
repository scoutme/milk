package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/session"
)

// requestToolNames decodes the tools array from a captured OpenAI-compat
// chat-completions request body.
func requestToolNames(body []byte) []string {
	var req struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		names = append(names, t.Function.Name)
	}
	return names
}

// TestRunBackgroundTask_ExcludesRecursiveAndEscalationTools verifies the
// depth cap and isolation from ADR-0043: a background job's tool list must
// never include escalate, agent_<name> tool-agents, or spawn_background_agent
// — even when the spawning agent itself has tool-agents configured — while
// still including ordinary built-in tools like bash.
func TestRunBackgroundTask_ExcludesRecursiveAndEscalationTools(t *testing.T) {
	var mu sync.Mutex
	var gotNames []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotNames = requestToolNames(body)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	// Simulate the spawning agent having tool-agents configured, to prove
	// they don't leak into the background job's tool list.
	agent.toolAgentEntries = []config.AgentToolEntry{{Agent: "helper", Description: "a helper"}}

	var out strings.Builder
	resp, _, err := agent.RunBackgroundTask(context.Background(), "/tmp", "investigate something", &out)
	if err != nil {
		t.Fatalf("RunBackgroundTask returned error: %v", err)
	}
	if resp != "done" {
		t.Errorf("expected response %q, got %q", "done", resp)
	}

	mu.Lock()
	names := gotNames
	mu.Unlock()

	for _, forbidden := range []string{"escalate", "agent_helper", "spawn_background_agent"} {
		for _, n := range names {
			if n == forbidden {
				t.Errorf("tool list must not include %q, got %v", forbidden, names)
			}
		}
	}
	found := false
	for _, n := range names {
		if n == "bash" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected ordinary built-in tool %q in list, got %v", "bash", names)
	}
}

// TestRunBackgroundTask_TokenUsageNotAttributedToParent verifies that a
// background job's token usage is accumulated locally and returned to the
// caller, rather than fed through onTokens — which would mis-attribute it to
// the spawning agent's own "primary"/"escalation" session totals instead of
// the "<role>:subagent" bucket the job manager is responsible for recording
// it under (ADR-0043 §4).
func TestRunBackgroundTask_TokenUsageNotAttributedToParent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"}}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	var parentCalls atomic.Int32
	agent.WithOnTokens(func(model, role string, prompt, completion, cacheRead, cacheCreation int64) {
		parentCalls.Add(1)
	})

	var out strings.Builder
	_, usage, err := agent.RunBackgroundTask(context.Background(), "/tmp", "investigate", &out)
	if err != nil {
		t.Fatalf("RunBackgroundTask returned error: %v", err)
	}
	if parentCalls.Load() != 0 {
		t.Errorf("expected onTokens NOT to be called during a background task, got %d calls", parentCalls.Load())
	}
	if usage.Prompt != 100 || usage.Completion != 20 {
		t.Errorf("expected usage to accumulate locally (prompt=100, completion=20), got %+v", usage)
	}
	if !strings.HasSuffix(usage.Agent, ":subagent") {
		t.Errorf("expected usage.Agent to end with :subagent, got %q", usage.Agent)
	}

	// The real callback must still be wired for the agent's normal turns
	// after the background task returns (restored, not permanently cleared).
	var out2 strings.Builder
	sess := &session.Session{}
	_, err = agent.Run(context.Background(), nil, "hi", &out2, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if parentCalls.Load() == 0 {
		t.Error("expected onTokens to fire again for a normal Run() turn after the background task completed")
	}
}

// TestRunBackgroundTask_RespectsMaxIterations verifies a background job that
// never stops calling tools is still bounded by MaxToolIterations, producing
// the same tool-trail fallback as a normal turn instead of hanging.
func TestRunBackgroundTask_RespectsMaxIterations(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := n.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc%d","function":{"name":"bash","arguments":"{\"command\":\"echo %d\"}"}}]}}]}`+"\n\n", i, i)
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 3})
	agent.skipPerms = true
	var out strings.Builder
	resp, _, err := agent.RunBackgroundTask(context.Background(), "/tmp", "keep going forever", &out)
	if err != nil {
		t.Fatalf("RunBackgroundTask returned error: %v", err)
	}
	if !strings.Contains(resp, "turn ended without a final summary") {
		t.Errorf("expected the maxIter tool-trail fallback, got %q", resp)
	}
	if n.Load() != 3 {
		t.Errorf("expected exactly MaxToolIterations=3 requests, got %d", n.Load())
	}
}
