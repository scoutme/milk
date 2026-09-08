package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
)

// TestLocalRunner_RunToolCall_NoPanicOrFalseEscalation is a regression test
// for two bugs found together while wiring the agent-as-tool bridge into the
// escalation path:
//
//  1. RunToolCall used to pass the current prompt as its own "history", so
//     Run's repeated-prompt check saw an exact self-match (score 1.0 >= the
//     0.9 threshold) and force-escalated on every single call, regardless of
//     actual repetition.
//  2. RunToolCall passed a nil *session.Session. That was masked by bug #1
//     (Run returned before ever touching sess), so fixing #1 alone exposed a
//     nil pointer dereference on sess.CWD inside Run.
//
// A tool-agent call for a longish, non-repeated prompt should complete
// normally: no panic, no error, and no spurious escalation reply.
func TestLocalRunner_RunToolCall_NoPanicOrFalseEscalation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{
			"choices": []map[string]any{
				{"delta": map[string]any{"content": "the viewport shows an empty scene"}, "finish_reason": "stop"},
			},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
	defer srv.Close()

	la := local.NewFromConfig(config.AgentConfig{
		Name:  "test-tool-agent",
		URL:   srv.URL,
		Model: "test-model",
	}).WithToolAgentRole()

	r := newLocalRunner(la, "test-tool-agent")

	result, err := r.RunToolCall(context.Background(), config.Config{},
		"get a blender viewport screenshot and describe what you see", nil, io.Discard)
	if err != nil {
		t.Fatalf("RunToolCall returned error: %v", err)
	}
	if result != "the viewport shows an empty scene" {
		t.Errorf("want the model's actual reply, got a spurious result: %q", result)
	}
}
