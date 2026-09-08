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

// TestStreamCompletion_RetriesImageDropSilently is a regression test for
// backend flakiness observed live against xiaomimimo's mimo-v2.5: a
// well-formed image_url part sometimes gets silently ignored (no error, no
// image_tokens in the usage), and a byte-identical retry succeeds. The user
// must never see the resulting non-answer from a dropped attempt — only the
// eventually-accepted response.
func TestStreamCompletion_RetriesImageDropSilently(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n := calls.Add(1)
		if n < 3 {
			// Backend silently dropped the image: no image_tokens in usage.
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"I don't see any image attached."},"finish_reason":"stop"}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":0}}}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		// Third attempt: backend actually processes the image this time.
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"I see a red square."},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":300,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":0,"image_tokens":192}}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := New(srv.URL, "test-model")
	a.supportsVision = true
	a.SetPendingImageParts([]ContentPart{
		{Type: "image_url", ImageURL: &ImageURLPart{URL: "data:image/png;base64,abc123"}},
	})

	var out strings.Builder
	history, err := a.Run(context.Background(), nil, "what is in this image", &out, &session.Session{CWD: "/tmp"}, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := calls.Load(); got != 3 {
		t.Errorf("expected 3 attempts (2 dropped + 1 accepted), got %d", got)
	}
	if strings.Contains(out.String(), "don't see any image") {
		t.Errorf("a dropped attempt's non-answer leaked to the live output: %q", out.String())
	}
	if !strings.Contains(out.String(), "red square") {
		t.Errorf("expected the accepted attempt's answer in the live output, got: %q", out.String())
	}

	found := false
	for _, m := range history {
		if m.Role == "assistant" && strings.Contains(m.Content, "red square") {
			found = true
		}
		if m.Role == "assistant" && strings.Contains(m.Content, "don't see any image") {
			t.Errorf("a dropped attempt's non-answer leaked into session history: %+v", m)
		}
	}
	if !found {
		t.Errorf("expected the accepted attempt's answer in history, got: %+v", history)
	}
}

// TestStreamCompletion_NoRetryForTextOnlyRequests verifies the retry loop
// never engages for a request with no image — image_tokens is meaningless
// there, and retrying would just double the cost of every ordinary turn.
func TestStreamCompletion_NoRetryForTextOnlyRequests(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"hello"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":50,"completion_tokens":5}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := New(srv.URL, "test-model")
	var out strings.Builder
	_, err := a.Run(context.Background(), nil, "hi", &out, &session.Session{CWD: "/tmp"}, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 attempt for a text-only request, got %d", got)
	}
}
