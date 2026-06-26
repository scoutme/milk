package local

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

// sseServer returns a test server that streams the given text as a single SSE
// data event followed by [DONE], using the OpenAI stream format.
func sseServer(t *testing.T, text string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{
			"choices": []map[string]any{
				{"delta": map[string]any{"content": text}, "finish_reason": nil},
			},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
}

// sseServerCapture behaves like sseServer but also captures the decoded request
// body so tests can inspect the messages sent to the model.
func sseServerCapture(t *testing.T, text string, captured *chatRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captured != nil {
			json.NewDecoder(r.Body).Decode(captured) //nolint:errcheck
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{
			"choices": []map[string]any{
				{"delta": map[string]any{"content": text}, "finish_reason": nil},
			},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
}

// TestWithTagCallbacks_NeedTagFiresCallback verifies that when the model response
// contains a <milk:need:NONCE> tag, the onNeed callback is called with the tag
// body and the tag is stripped from the visible output.
func TestWithTagCallbacks_NeedTagFiresCallback(t *testing.T) {
	const nonce = "test01"
	openTag := "<milk:need:" + nonce + ">"
	closeTag := "</milk:need:" + nonce + ">"
	responseText := "Sure! " + openTag + "explain router logic" + closeTag + " Here is the router explanation."

	srv := sseServer(t, responseText)
	defer srv.Close()

	agent := New(srv.URL, "test-model")

	var gotNeed string
	agent = agent.WithTagCallbacks(nonce, "primary", "escalation",
		func(content string) { gotNeed = content },
		nil,
	)

	sess := &session.Session{}
	var out strings.Builder
	_, err := agent.Run(context.Background(), nil, "explain router logic", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if gotNeed != "explain router logic" {
		t.Errorf("onNeed: want %q, got %q", "explain router logic", gotNeed)
	}
	if strings.Contains(out.String(), "milk:need") {
		t.Errorf("need tag must be stripped from output, got: %q", out.String())
	}
	if strings.Contains(out.String(), openTag) || strings.Contains(out.String(), closeTag) {
		t.Errorf("tag markup must not appear in output, got: %q", out.String())
	}
	if !strings.Contains(out.String(), "Here is the router explanation.") {
		t.Errorf("surrounding text must be preserved in output, got: %q", out.String())
	}
}

// TestWithTagCallbacks_NeedUpdatesCurrentNeed verifies the end-to-end path:
// onNeed callback writes to sess.CurrentNeed just as runLocal wires it.
func TestWithTagCallbacks_NeedUpdatesCurrentNeed(t *testing.T) {
	const nonce = "test02"
	openTag := "<milk:need:" + nonce + ">"
	closeTag := "</milk:need:" + nonce + ">"

	srv := sseServer(t, openTag+"review test coverage"+closeTag+" done")
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	sess := &session.Session{}

	agent = agent.WithTagCallbacks(nonce, "primary", "escalation",
		func(content string) { sess.CurrentNeed = content },
		nil,
	)

	var out strings.Builder
	if _, err := agent.Run(context.Background(), nil, "any prompt", &out, sess, nil); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if sess.CurrentNeed != "review test coverage" {
		t.Errorf("sess.CurrentNeed: want %q, got %q", "review test coverage", sess.CurrentNeed)
	}
}

// TestWithTagCallbacks_NoTagNoCallback verifies that onNeed is NOT called when
// the response contains no need tag.
func TestWithTagCallbacks_NoTagNoCallback(t *testing.T) {
	const nonce = "test03"
	srv := sseServer(t, "plain response with no tags")
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	called := false
	agent = agent.WithTagCallbacks(nonce, "primary", "escalation",
		func(string) { called = true },
		nil,
	)

	sess := &session.Session{}
	var out strings.Builder
	if _, err := agent.Run(context.Background(), nil, "hello", &out, sess, nil); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if called {
		t.Error("onNeed must not be called when response contains no need tag")
	}
}

// TestWithTagCallbacks_WrongNonceNotRecorded verifies that a tag with a
// different nonce is stripped from output but does NOT fire onNeed.
func TestWithTagCallbacks_WrongNonceNotRecorded(t *testing.T) {
	const nonce = "test04"
	responseText := "<milk:need:othernonce>wrong goal</milk:need:othernonce> text"

	srv := sseServer(t, responseText)
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	called := false
	agent = agent.WithTagCallbacks(nonce, "primary", "escalation",
		func(string) { called = true },
		nil,
	)

	sess := &session.Session{}
	var out strings.Builder
	if _, err := agent.Run(context.Background(), nil, "hello", &out, sess, nil); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if called {
		t.Error("onNeed must not fire for a tag with a different nonce")
	}
	if strings.Contains(out.String(), "milk:need") {
		t.Errorf("wrong-nonce tag must still be stripped from output, got: %q", out.String())
	}
}

// TestWithTagCallbacks_NoTagInstructionForLocalAgent verifies that local HTTP agents
// never receive tag instructions regardless of role — they use injected tool calls
// (record_memory, current_need) instead. Tags are only for external-process agents
// (CLI, subprocess) where milk cannot inject tools.
func TestWithTagCallbacks_NoTagInstructionForLocalAgent(t *testing.T) {
	const nonce = "test05"
	var captured chatRequest
	srv := sseServerCapture(t, "ok", &captured)
	defer srv.Close()

	needle := "<milk:need:" + nonce + ">"

	for _, name := range []string{"primary-role", "escalation-role"} {
		t.Run(name, func(t *testing.T) {
			a := New(srv.URL, "test-model")
			if name == "escalation-role" {
				a = a.AsEscalationTarget("escalation")
			}
			a = a.WithTagCallbacks(nonce, "primary", "escalation", func(string) {}, nil)
			sess := &session.Session{}
			var out strings.Builder
			if _, err := a.Run(context.Background(), nil, "hello", &out, sess, nil); err != nil {
				t.Fatalf("Run error: %v", err)
			}
			for _, msg := range captured.Messages {
				if strings.Contains(msg.Content, needle) {
					t.Errorf("local agent must not receive tag instructions; found %q in message: %q", needle, msg.Content)
					return
				}
			}
		})
	}
}

// TestWithoutTagCallbacks_TagNotStripped verifies that without WithTagCallbacks,
// a need tag in the response passes through to output unmodified.
func TestWithoutTagCallbacks_TagNotStripped(t *testing.T) {
	const nonce = "test06"
	openTag := "<milk:need:" + nonce + ">"
	closeTag := "</milk:need:" + nonce + ">"
	responseText := "before " + openTag + "some goal" + closeTag + " after"

	srv := sseServer(t, responseText)
	defer srv.Close()

	agent := New(srv.URL, "test-model") // no WithTagCallbacks

	sess := &session.Session{}
	var out strings.Builder
	if _, err := agent.Run(context.Background(), nil, "hello", &out, sess, nil); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	// Without tag callbacks the raw tag bytes should appear in output — the
	// stream detector passes unknown angle-bracket content through once it
	// determines it is not a tool-call fence.
	if !strings.Contains(out.String(), "some goal") {
		t.Errorf("without tag callbacks, tag content must pass through; got: %q", out.String())
	}
}

// TestWithTagCallbacks_PerceptTagFiresCallback verifies that <milk:percept:NONCE>
// tags are intercepted and the onPercept callback is called.
func TestWithTagCallbacks_PerceptTagFiresCallback(t *testing.T) {
	const nonce = "test07"
	openTag := "<milk:percept:" + nonce + ">"
	closeTag := "</milk:percept:" + nonce + ">"
	responseText := "answer " + openTag + "router uses weighted scorer" + closeTag + " done"

	srv := sseServer(t, responseText)
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	var gotPercept string
	agent = agent.WithTagCallbacks(nonce, "primary", "escalation",
		nil,
		func(content, _ string) { gotPercept = content },
	)

	sess := &session.Session{}
	var out strings.Builder
	if _, err := agent.Run(context.Background(), nil, "hello", &out, sess, nil); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if gotPercept != "router uses weighted scorer" {
		t.Errorf("onPercept: want %q, got %q", "router uses weighted scorer", gotPercept)
	}
	if strings.Contains(out.String(), "milk:percept") {
		t.Errorf("percept tag must be stripped from output, got: %q", out.String())
	}
}
