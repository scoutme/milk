package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/oversight"
)

// apiServer fakes the Telegram Bot API: sendMessage payloads are recorded,
// getUpdates yields scripted replies in order.
type apiServer struct {
	mu      sync.Mutex
	sent    []string
	replies []string
	srv     *httptest.Server
}

func newAPIServer(replies ...string) *apiServer {
	s := &apiServer{replies: replies}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var payload struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			s.mu.Lock()
			s.sent = append(s.sent, payload.Text)
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			s.mu.Lock()
			var result []map[string]any
			// Serve a scripted reply only once the question has been sent:
			// AskInput registers its reply channel *before* sending, so
			// "question recorded" proves the waiter is registered — without
			// this gate a reply served on the very first poll can beat the
			// registration and be dispatched nowhere (CI flake: the ask
			// then times out with answer="" ok=false).
			if len(s.replies) > 0 && len(s.sent) > 0 {
				result = append(result, map[string]any{
					"update_id": 1,
					"message": map[string]any{
						"chat": map[string]any{"id": 1},
						"text": s.replies[0],
					},
				})
				s.replies = s.replies[1:]
			}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	return s
}

func TestAskInputDeliversFreeTextReply(t *testing.T) {
	s := newAPIServer("blue, definitely")
	defer s.srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := New(Config{Token: "t", ChatID: 1, APIBase: s.srv.URL, PermTimeout: 2 * time.Second})
	n.StartPolling(ctx)

	answer, ok := n.AskInput(ctx, oversight.InputRequest{Question: "blue or green?", Options: []string{"blue", "green"}})
	if !ok || answer != "blue, definitely" {
		t.Fatalf("answer=%q ok=%v", answer, ok)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sent) == 0 || !strings.Contains(s.sent[0], "blue or green?") {
		t.Fatalf("question not delivered: %q", s.sent)
	}
	if !strings.Contains(s.sent[0], "Options:") {
		t.Fatalf("options missing from prompt: %q", s.sent[0])
	}
}

func TestAskInputSilenceIsNotAnAnswer(t *testing.T) {
	s := newAPIServer() // no replies ever
	defer s.srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := New(Config{Token: "t", ChatID: 1, APIBase: s.srv.URL, PermTimeout: 50 * time.Millisecond})
	n.StartPolling(ctx)

	// Silence never ends the wait: the question stays open past PermTimeout
	// (the caller's timed answer bounds it) and only the ask's cancellation
	// releases the prompt slot.
	done := make(chan struct{})
	go func() {
		defer close(done)
		answer, ok := n.AskInput(ctx, oversight.InputRequest{Question: "q"})
		if ok || answer != "" {
			t.Errorf("silence must not answer: answer=%q ok=%v", answer, ok)
		}
	}()
	select {
	case <-done:
		t.Fatal("the wait must outlive PermTimeout — the question is still open")
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation must end the wait")
	}
}

func TestAskInputQuestionIsEscapedAndChunkSafe(t *testing.T) {
	s := newAPIServer()
	defer s.srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // returns right after the prompt is sent — escaping is under test
	n := New(Config{Token: "t", ChatID: 1, APIBase: s.srv.URL, PermTimeout: 20 * time.Millisecond})
	n.StartPolling(ctx)

	n.AskInput(ctx, oversight.InputRequest{Question: "use `code` or _plain_?"})
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sent) == 0 {
		t.Fatal("question not delivered")
	}
	if !strings.Contains(s.sent[0], "\\`code\\`") || !strings.Contains(s.sent[0], "\\_plain\\_") {
		t.Fatalf("MarkdownV2 specials not escaped: %q", s.sent[0])
	}
}
