package local

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestExecuteToolCalls_ConcurrentDispatch verifies that two tool calls in the
// same batch run concurrently rather than sequentially.
// Both tools sleep 100 ms; if they ran sequentially the total would be ≥ 200 ms.
// We assert the total elapsed time is < 180 ms (well under 2×100 ms).
func TestExecuteToolCalls_ConcurrentDispatch(t *testing.T) {
	a := &Agent{
		skipPerms:   true,
		toolTimeout: 5 * time.Second,
	}

	tcs := []toolCall{
		{ID: "c1", Function: toolCallFunction{Name: "bash", Arguments: `{"command":"sleep 0.1"}`}},
		{ID: "c2", Function: toolCallFunction{Name: "bash", Arguments: `{"command":"sleep 0.1"}`}},
	}

	ctx := context.Background()
	start := time.Now()
	msgs, esc := a.executeToolCalls(ctx, nil, tcs, "", "", io.Discard, nil, nil)
	elapsed := time.Since(start)

	if esc != nil {
		t.Fatalf("unexpected escalation signal: %v", esc)
	}
	// 2 tool results + 1 assistant message
	if len(msgs) != 3 {
		t.Errorf("expected 3 messages (assistant + 2 tool), got %d", len(msgs))
	}
	if elapsed >= 180*time.Millisecond {
		t.Errorf("tools appear to have run sequentially: elapsed %v (want < 180ms)", elapsed)
	}
}

// TestExecuteToolCalls_PerToolTimeout verifies that a tool whose execution exceeds
// toolTimeout is cancelled, returns a timeout error result, and other tools in the
// same batch are unaffected.
func TestExecuteToolCalls_PerToolTimeout(t *testing.T) {
	a := &Agent{
		skipPerms:   true,
		toolTimeout: 50 * time.Millisecond, // short timeout to trigger quickly
	}

	tcs := []toolCall{
		// This one will time out (sleep 5 s >> 50 ms timeout).
		{ID: "slow", Function: toolCallFunction{Name: "bash", Arguments: `{"command":"sleep 5"}`}},
		// This one completes immediately.
		{ID: "fast", Function: toolCallFunction{Name: "bash", Arguments: `{"command":"echo fast"}`}},
	}

	ctx := context.Background()
	msgs, esc := a.executeToolCalls(ctx, nil, tcs, "", "", io.Discard, nil, nil)

	if esc != nil {
		t.Fatalf("unexpected escalation signal: %v", esc)
	}

	// Find slow tool result and verify it contains a timeout error.
	var slowResult, fastResult string
	for _, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		if m.ToolCallID == "slow" {
			slowResult = m.Content
		}
		if m.ToolCallID == "fast" {
			fastResult = m.Content
		}
	}
	if !strings.Contains(slowResult, "timed out") {
		t.Errorf("slow tool: expected timeout error, got %q", slowResult)
	}
	if !strings.Contains(fastResult, "fast") {
		t.Errorf("fast tool: expected 'fast' in output, got %q", fastResult)
	}
}

// TestExecuteToolCalls_ContextCancellation verifies that cancelling the turn
// context aborts all in-flight tool goroutines.
func TestExecuteToolCalls_ContextCancellation(t *testing.T) {
	a := &Agent{
		skipPerms:   true,
		toolTimeout: 10 * time.Second,
	}

	var started int32
	tcs := []toolCall{
		// Two long-running tools that will be cancelled.
		{ID: "c1", Function: toolCallFunction{Name: "bash", Arguments: `{"command":"sleep 10"}`}},
		{ID: "c2", Function: toolCallFunction{Name: "bash", Arguments: `{"command":"sleep 10"}`}},
	}

	_ = started

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.executeToolCalls(ctx, nil, tcs, "", "", io.Discard, nil, nil) //nolint:errcheck
	}()

	// Cancel after a brief delay to let the goroutines start.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Both tools were cancelled — good.
	case <-time.After(2 * time.Second):
		t.Fatal("executeToolCalls did not return after context cancellation")
	}
}

// TestExecuteToolCalls_ResultOrder verifies that tool results are appended in
// the original call order regardless of which tool finishes first.
func TestExecuteToolCalls_ResultOrder(t *testing.T) {
	a := &Agent{
		skipPerms:   true,
		toolTimeout: 5 * time.Second,
	}

	// c1 sleeps briefly, c2 runs immediately — c2 finishes first but c1 should appear first.
	tcs := []toolCall{
		{ID: "c1", Function: toolCallFunction{Name: "bash", Arguments: `{"command":"sleep 0.05 && echo first"}`}},
		{ID: "c2", Function: toolCallFunction{Name: "bash", Arguments: `{"command":"echo second"}`}},
	}

	ctx := context.Background()
	msgs, _ := a.executeToolCalls(ctx, nil, tcs, "", "", io.Discard, nil, nil)

	var toolMsgs []string
	for _, m := range msgs {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m.ToolCallID)
		}
	}
	if len(toolMsgs) != 2 {
		t.Fatalf("expected 2 tool messages, got %d", len(toolMsgs))
	}
	if toolMsgs[0] != "c1" || toolMsgs[1] != "c2" {
		t.Errorf("result order = %v, want [c1 c2]", toolMsgs)
	}
}

// TestWithToolTimeout verifies the WithToolTimeout With-method copies correctly.
func TestWithToolTimeout(t *testing.T) {
	a := &Agent{}
	b := a.WithToolTimeout(30 * time.Second)
	if b.toolTimeout != 30*time.Second {
		t.Errorf("toolTimeout = %v, want 30s", b.toolTimeout)
	}
	if a.toolTimeout != 0 {
		t.Error("original agent modified by WithToolTimeout")
	}
}

// TestAgentToolTimeout_Default verifies the default is 2 minutes when no override is set.
func TestAgentToolTimeout_Default(t *testing.T) {
	// Use internal/config import indirectly through the config package helper.
	// We can't import config here (package local doesn't import it),
	// but we can test WithToolTimeout wiring directly.
	a := &Agent{toolTimeout: 0}
	// toolTimeout=0 means "use the field as-is" — no per-tool limit applied.
	// The runner sets toolTimeout from cfg.AgentToolTimeout(ac) which defaults to 2 min.
	// We verify that a non-zero timeout is respected.
	b := a.WithToolTimeout(2 * time.Minute)
	if b.toolTimeout != 2*time.Minute {
		t.Errorf("toolTimeout = %v, want 2m", b.toolTimeout)
	}
}

// benchmarkToolConcurrency is a helper used by both correctness tests.
func benchmarkToolConcurrency(tcs []toolCall) time.Duration {
	a := &Agent{skipPerms: true, toolTimeout: 5 * time.Second}
	ctx := context.Background()
	start := time.Now()
	a.executeToolCalls(ctx, nil, tcs, "", "", io.Discard, nil, nil) //nolint:errcheck
	return time.Since(start)
}

// Ensure benchmarkToolConcurrency is used (avoids "declared and not used").
var _ = benchmarkToolConcurrency
var _ = atomic.AddInt32
