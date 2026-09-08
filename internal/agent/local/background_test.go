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
	"time"

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

// TestRunBackgroundTask_ConcurrentWithParentRun_NoRace verifies the fix for
// a real bug: a background job (run in its own goroutine, per Manager.Spawn)
// used to mutate the parent *Agent's shared fields (onTokens, reasoningNgram,
// ...) directly, which would race the moment the parent started its own next
// turn on the same Agent while the job was still in flight. RunBackgroundTask
// now clones before touching any of that state. Run this test with -race.
func TestRunBackgroundTask_ConcurrentWithParentRun_NoRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"reasoning_content":"thinking a bit"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	var parentTokenCalls atomic.Int32
	agent.WithOnTokens(func(model, role string, prompt, completion, cacheRead, cacheCreation int64) {
		parentTokenCalls.Add(1)
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var out strings.Builder
		if _, _, err := agent.RunBackgroundTask(context.Background(), "/tmp", "investigate", &out); err != nil {
			t.Errorf("RunBackgroundTask returned error: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		var out strings.Builder
		sess := &session.Session{}
		if _, err := agent.Run(context.Background(), nil, "hi", &out, sess, nil); err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	}()
	wg.Wait()

	if parentTokenCalls.Load() == 0 {
		t.Error("expected the parent's real onTokens callback to fire for its own Run() call")
	}
}

// TestDispatchOneTool_SpawnBackgroundAgent is the end-to-end wiring test: a
// model-issued spawn_background_agent tool call returns an immediate
// acknowledgement (not the eventual result) and the job actually runs to
// completion via the wired Manager.
func TestDispatchOneTool_SpawnBackgroundAgent(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc1","function":{"name":"spawn_background_agent","arguments":"{\"task\":\"investigate X\",\"label\":\"investigate X\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	mgr := NewManager(context.Background(), 3)
	agent.SetBackgroundManager(mgr)

	var wg sync.WaitGroup
	wg.Add(1)
	mgr.SetOnDone(func(j *Job) { wg.Done() })

	sess := &session.Session{CWD: "/tmp"}
	var out strings.Builder
	history, err := agent.Run(context.Background(), nil, "please investigate", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	found := false
	for _, m := range history {
		if m.Role == "tool" && strings.Contains(m.Content, "Spawned background agent") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a tool result acknowledging the spawn (not the eventual result), got history: %+v", history)
	}

	wg.Wait()
	jobs := mgr.Drain()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 completed job, got %d", len(jobs))
	}
	if jobs[0].Result != "done" {
		t.Errorf("expected job result %q, got %q", "done", jobs[0].Result)
	}
	if jobs[0].Role != "primary" {
		t.Errorf("expected job role %q, got %q", "primary", jobs[0].Role)
	}
	if jobs[0].Label != "investigate X" {
		t.Errorf("expected job label %q, got %q", "investigate X", jobs[0].Label)
	}
}

// requestSystemPrompt extracts the system message content from a captured
// chat-completions request body.
func requestSystemPrompt(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	for _, m := range req.Messages {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// TestRun_BackgroundAgentGuidance_OnlyWhenManagerSet verifies the system
// prompt mentions spawn_background_agent only when a Manager is actually
// wired — otherwise the prompt would reference a tool the model doesn't
// have.
func TestRun_BackgroundAgentGuidance_OnlyWhenManagerSet(t *testing.T) {
	var mu sync.Mutex
	var gotPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPrompt = requestSystemPrompt(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	sess := &session.Session{}
	var out strings.Builder
	if _, err := agent.Run(context.Background(), nil, "hi", &out, sess, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	mu.Lock()
	withoutMgr := gotPrompt
	mu.Unlock()
	if strings.Contains(withoutMgr, "spawn_background_agent") {
		t.Errorf("did not expect background-agent guidance without a Manager, got prompt: %q", withoutMgr)
	}

	agent.SetBackgroundManager(NewManager(context.Background(), 3))
	if _, err := agent.Run(context.Background(), nil, "hi", &out, sess, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	mu.Lock()
	withMgr := gotPrompt
	mu.Unlock()
	if !strings.Contains(withMgr, "spawn_background_agent") {
		t.Errorf("expected background-agent guidance once a Manager is set, got prompt: %q", withMgr)
	}
}

// stubTaskStore is a minimal TaskStore satisfying the local.TaskStore
// interface for tests that only need a.taskStore != nil.
type stubTaskStore struct{}

func (stubTaskStore) Create(string, []string) (TaskEntry, error) { return TaskEntry{}, nil }
func (stubTaskStore) Update(string, string, string) error        { return nil }
func (stubTaskStore) Complete(string) error                      { return nil }
func (stubTaskStore) List(bool) ([]TaskEntry, error)             { return nil, nil }

// TestRun_TaskToolGuidance_OnlyWhenTaskStoreSet mirrors
// TestRun_BackgroundAgentGuidance_OnlyWhenManagerSet: bare tool availability
// doesn't reliably get used without guidance on when it's expected, so
// taskToolGuidance must appear in the system prompt exactly when a task
// store is wired up, and not otherwise.
func TestRun_TaskToolGuidance_OnlyWhenTaskStoreSet(t *testing.T) {
	var mu sync.Mutex
	var gotPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPrompt = requestSystemPrompt(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	sess := &session.Session{}
	var out strings.Builder
	if _, err := agent.Run(context.Background(), nil, "hi", &out, sess, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	mu.Lock()
	withoutStore := gotPrompt
	mu.Unlock()
	if strings.Contains(withoutStore, "create_task") {
		t.Errorf("did not expect task-tool guidance without a task store, got prompt: %q", withoutStore)
	}

	agentWithTasks := agent.WithTaskStore(stubTaskStore{})
	if _, err := agentWithTasks.Run(context.Background(), nil, "hi", &out, sess, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	mu.Lock()
	withStore := gotPrompt
	mu.Unlock()
	if !strings.Contains(withStore, "create_task") {
		t.Errorf("expected task-tool guidance once a task store is set, got prompt: %q", withStore)
	}
}

// TestRunBackgroundTask_PermissionGatedToolDeniesInsteadOfHanging verifies
// the fix for a real incident: background jobs used to inherit the parent's
// interactive permAsk callback, which blocks synchronously on a plain
// channel receive with no timeout or context-awareness. A background job
// calling a permission-gated tool (bash, write_file, ...) would show an
// unattributed "Allow? [Y/n]" prompt the user had no reason to expect, hang
// forever waiting for an answer, and permanently hold its concurrency slot.
// cloneForBackground no longer copies permAsk, so this must deny instead —
// fast, no hang — and let the model react to the denial.
func TestRunBackgroundTask_PermissionGatedToolDeniesInsteadOfHanging(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc1","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	never := make(chan struct{}) // never closed
	agent := New(srv.URL, "test-model")
	agent.WithPermissions(nil, func(tool, summary string) bool {
		<-never // would hang forever if a background job ever called this
		return true
	})

	done := make(chan struct{})
	var resp string
	var runErr error
	go func() {
		var out strings.Builder
		resp, _, runErr = agent.RunBackgroundTask(context.Background(), "/tmp", "run ls", &out)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunBackgroundTask hung — permAsk was called from a background job")
	}
	if runErr != nil {
		t.Fatalf("RunBackgroundTask returned error: %v", runErr)
	}
	if resp != "done" {
		t.Errorf("expected the model to react to the denial and finish normally with %q, got %q", "done", resp)
	}
}
