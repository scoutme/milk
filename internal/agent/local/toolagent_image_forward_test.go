package local

import (
	"context"
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

// TestRun_ForwardsPendingImagesToToolAgent is a regression test for the bug
// reported live: a non-vision escalation agent with a pasted image either
// tried (and 404'd) to attach the image to its own turn, or — before that —
// silently dropped it with no way to reach the vision-capable tool-agent set
// up specifically to handle it. An agent that isn't vision-configured but has
// tool-agents available must forward its pending image(s) to an agent_<name>
// call instead of trying to see them itself.
func TestRun_ForwardsPendingImagesToToolAgent(t *testing.T) {
	var visionMu sync.Mutex
	var visionReqBody string

	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		visionMu.Lock()
		visionReqBody = string(body)
		visionMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"the image shows a red square"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer visionSrv.Close()

	visionAgent := New(visionSrv.URL, "vision-model")
	visionAgent.supportsVision = true

	var calls atomic.Int32
	callerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc1","function":{"name":"agent_visionagent","arguments":"{\"request\":\"what is in this image\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"delegated: red square"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer callerSrv.Close()

	caller := New(callerSrv.URL, "caller-model")
	caller.supportsVision = false
	caller.toolAgentEntries = []config.AgentToolEntry{{Agent: "visionagent", Description: "vision helper"}}
	caller.SetToolAgentDispatcher(func(ctx context.Context, agentName, request string, images []ContentPart, out io.Writer) (string, error) {
		if agentName != "visionagent" {
			t.Fatalf("dispatcher got agent name %q, want %q", agentName, "visionagent")
		}
		if len(images) == 0 {
			t.Fatal("dispatcher received no images to forward")
		}
		visionAgent.SetPendingImageParts(images)
		history, err := visionAgent.Run(ctx, nil, request, out, &session.Session{CWD: "/tmp"}, nil)
		if err != nil {
			return "", err
		}
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].Role == "assistant" {
				return history[i].Content, nil
			}
		}
		return "", nil
	})
	caller.SetPendingImageParts([]ContentPart{
		{Type: "image_url", ImageURL: &ImageURLPart{URL: "data:image/png;base64,abc123"}},
	})

	var out strings.Builder
	history, err := caller.Run(context.Background(), nil, "this is an image - ask agent-visionagent", &out, &session.Session{CWD: "/tmp"}, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	visionMu.Lock()
	body := visionReqBody
	visionMu.Unlock()
	if body == "" {
		t.Fatal("vision tool-agent server never received a request")
	}
	if !strings.Contains(body, `"image_url"`) || !strings.Contains(body, "data:image/png;base64,abc123") {
		t.Errorf("expected the forwarded image_url part in the tool-agent's request, got: %s", body)
	}

	found := false
	for _, m := range history {
		if m.Role == "tool" && strings.Contains(m.Content, "red square") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a tool result containing the vision agent's answer, got history: %+v", history)
	}
}
