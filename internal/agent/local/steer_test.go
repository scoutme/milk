package local

// Mid-turn steering (issue #441): queued user input drains into the RUNNING
// turn at the tool loop's two model-request boundaries — after a tool round,
// and at the would-be-final text round (continuation instead of return).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

// steerRound scripts one request: a create_task tool-call round (the shape
// the existing loop tests use) or a final content round.
type steerRound struct {
	tool    bool
	content string
}

// bodyLog collects request bodies safely across the server goroutines.
type bodyLog struct {
	mu     sync.Mutex
	bodies []string
}

func (l *bodyLog) add(b string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bodies = append(l.bodies, b)
}

func (l *bodyLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.bodies...)
}

func (l *bodyLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.bodies)
}

// steerServer serves the scripted rounds and captures every request body.
func steerServer(t *testing.T, rounds ...steerRound) (*httptest.Server, *bodyLog) {
	t.Helper()
	log := &bodyLog{}
	var mu sync.Mutex
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		log.add(string(body))
		mu.Lock()
		idx := n
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if idx < len(rounds) && rounds[idx].tool {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc","function":{"name":"create_task","arguments":"{\"title\":\"a\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		} else {
			content := "done"
			if idx < len(rounds) {
				content = rounds[idx].content
			}
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":`+strconv.Quote(content)+`},"finish_reason":"stop"}]}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

// steerScript is the host queue's test double: call n delivers steps[n-1]
// (empty beyond the script), and every call is counted.
type steerScript struct {
	mu    sync.Mutex
	steps [][]string
	calls int
}

func (s *steerScript) drain() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls <= len(s.steps) {
		return s.steps[s.calls-1]
	}
	return nil
}

func (s *steerScript) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestSteer_DeliversAtToolBoundary: input queued while the first round
// streams is injected as a user message after the tool results, in the SAME
// turn — request 2 carries it, behind the tool-role message.
func TestSteer_DeliversAtToolBoundary(t *testing.T) {
	srv, log := steerServer(t,
		steerRound{tool: true},
		steerRound{content: "done with it"},
	)
	script := &steerScript{steps: [][]string{
		nil,                                   // boundary before round 1: nothing queued yet
		{"actually focus on the billing bug"}, // boundary after the tool round
	}}
	agent := New(srv.URL, "test-model").
		WithMemConfig(MemConfig{MaxToolIterations: 15}).
		WithSteerDrain(script.drain)

	var out strings.Builder
	history, err := agent.Run(context.Background(), nil, "fix the thing", &out, &session.Session{}, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := log.count(); got != 2 {
		t.Fatalf("expected 2 requests (tool round + steered continuation), got %d", got)
	}
	body2 := log.all()[1]
	iSteer := strings.Index(body2, "actually focus on the billing bug")
	if iSteer < 0 {
		t.Fatalf("steered input missing from request 2:\n%s", body2)
	}
	if iTool := strings.Index(body2, `"role":"tool"`); iTool < 0 || iSteer < iTool {
		t.Errorf("steered input must ride after the tool results (tool=%d steer=%d)", iTool, iSteer)
	}
	if last := history[len(history)-1]; last.Content != "done with it" || last.Role != "assistant" {
		t.Errorf("turn must end on the final answer, got %s/%q", last.Role, last.Content)
	}
	// The steered input is part of the persisted conversation.
	found := false
	for _, msg := range history {
		if msg.Role == "user" && msg.Content == "actually focus on the billing bug" {
			found = true
		}
	}
	if !found {
		t.Errorf("steered input not persisted into history: %#v", history)
	}
}

// TestSteer_ContinuesSameTurnAfterTextResponse: the reasoning-phase caveat —
// while a tool-less response streams nothing can be injected, so the queued
// input delivers at stream end as a same-turn continuation: request 2
// carries the answer-so-far plus the input, and the turn does not end first.
func TestSteer_ContinuesSameTurnAfterTextResponse(t *testing.T) {
	srv, log := steerServer(t,
		steerRound{content: "partial answer"},
		steerRound{content: "final answer"},
	)
	script := &steerScript{steps: [][]string{
		nil,                        // boundary before round 1
		{"also rename the helper"}, // termination drain after the streaming response
		nil,                        // boundary before round 2
		nil,                        // termination drain after round 2: empty → return
	}}
	agent := New(srv.URL, "test-model").
		WithMemConfig(MemConfig{MaxToolIterations: 15}).
		WithSteerDrain(script.drain)

	var out strings.Builder
	history, err := agent.Run(context.Background(), nil, "start the work", &out, &session.Session{}, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := log.count(); got != 2 {
		t.Fatalf("expected 2 requests (original + continuation), got %d", got)
	}
	body2 := log.all()[1]
	if !strings.Contains(body2, "partial answer") {
		t.Errorf("continuation request must carry the answer-so-far:\n%s", body2)
	}
	if !strings.Contains(body2, "also rename the helper") {
		t.Errorf("continuation request must carry the steered input:\n%s", body2)
	}
	// History shape: … assistant(partial) → user(steer) → assistant(final).
	var idx int
	for i, msg := range history {
		if msg.Role == "assistant" && msg.Content == "partial answer" {
			idx = i
			break
		}
	}
	if idx == 0 && history[0].Content != "partial answer" {
		t.Fatalf("partial answer not in history: %#v", history)
	}
	next := history[idx+1]
	if next.Role != "user" || next.Content != "also rename the helper" {
		t.Errorf("steered input must follow the partial answer, got %s/%q", next.Role, next.Content)
	}
	if last := history[len(history)-1]; last.Content != "final answer" {
		t.Errorf("turn must end on the final answer, got %q", last.Content)
	}
}

// TestSteer_BudgetGuardNeverPops: at the max-iteration ceiling the
// termination drain must not run — popping would consume queue entries a
// continuation can no longer send (maxIter < 0 = unlimited always continues).
func TestSteer_BudgetGuardNeverPops(t *testing.T) {
	srv, log := steerServer(t, steerRound{content: "last answer"})
	script := &steerScript{steps: [][]string{
		nil,                       // boundary before round 1 (inside the budget)
		{"would be lost forever"}, // termination drain: must NOT happen at the ceiling
	}}
	agent := New(srv.URL, "test-model").
		WithMemConfig(MemConfig{MaxToolIterations: 1}).
		WithSteerDrain(script.drain)

	var out strings.Builder
	if _, err := agent.Run(context.Background(), nil, "go", &out, &session.Session{}, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := script.callCount(); got != 1 {
		t.Errorf("termination drain popped at the iteration ceiling: %d drain calls, want 1", got)
	}
	if got := log.count(); got != 1 {
		t.Errorf("expected exactly 1 request, got %d", got)
	}
	for _, body := range log.all() {
		if strings.Contains(body, "would be lost forever") {
			t.Error("popped-but-undeliverable input leaked into a request")
		}
	}
}

// TestCloneForBackgroundDropsSteerDrain: a background job's clone must never
// pop the session's steer queue — the queue belongs to the session's running
// turn, and jobs have their own follow-up inbox (bgInbox).
func TestCloneForBackgroundDropsSteerDrain(t *testing.T) {
	a := New("http://127.0.0.1:1", "test-model").
		WithSteerDrain(func() []string { return []string{"x"} })
	if a.steerDrain == nil {
		t.Fatal("WithSteerDrain must set the field")
	}
	if bg := a.cloneForBackground(); bg.steerDrain != nil {
		t.Error("background clone must drop steerDrain (jobs own bgInbox, never the session queue)")
	}
}

// TestSteer_NilSourceIsInert: without a wired drain the loop never calls
// anything — the pre-#441 behavior (covered end to end by every other loop
// test) stays the default for CLI-backed agents, tool agents and jobs.
func TestSteer_NilSourceIsInert(t *testing.T) {
	srv, log := steerServer(t, steerRound{content: "plain"})
	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 15})
	var out strings.Builder
	history, err := agent.Run(context.Background(), nil, "hi", &out, &session.Session{}, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if log.count() != 1 {
		t.Errorf("expected exactly 1 request, got %d", log.count())
	}
	if last := history[len(history)-1]; last.Content != "plain" {
		t.Errorf("unexpected response %q", last.Content)
	}
}
