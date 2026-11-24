package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestChunkMessageShortStaysSingle(t *testing.T) {
	chunks := chunkMessage("hello world", 100)
	if len(chunks) != 1 || chunks[0] != "hello world" {
		t.Fatalf("got %q", chunks)
	}
}

func TestChunkMessageEmpty(t *testing.T) {
	if chunks := chunkMessage("", 100); chunks != nil {
		t.Fatalf("want nil for empty text, got %q", chunks)
	}
}

func TestChunkMessagePreservesContentAndFitsBudget(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		b.WriteString("line with some content and markdown specials _*[]! `x` ")
		b.WriteString("and emoji 🚀 plus CJK 中文字\n")
	}
	text := b.String()
	budget := 600
	chunks := chunkMessage(text, budget)
	if len(chunks) < 5 {
		t.Fatalf("expected chunking, got %d chunks", len(chunks))
	}
	if got := strings.Join(chunks, ""); got != text {
		t.Fatalf("chunks do not reassemble into the original text")
	}
	for i, c := range chunks {
		if utf16Len(escMD(c)) > budget {
			t.Fatalf("chunk %d escaped length %d exceeds budget %d", i, utf16Len(escMD(c)), budget)
		}
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d is not valid UTF-8 (rune split)", i)
		}
	}
}

func TestChunkMessagePrefersLineBoundaries(t *testing.T) {
	text := "first line\nsecond line\nthird line\nfourth line\n"
	chunks := chunkMessage(text, 30)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %q", chunks)
	}
	if got := strings.Join(chunks, ""); got != text {
		t.Fatalf("reassembly mismatch: %q", chunks)
	}
	for i, c := range chunks[:len(chunks)-1] {
		if !strings.HasSuffix(c, "\n") {
			t.Fatalf("chunk %d does not end at a line boundary: %q", i, c)
		}
	}
}

func TestChunkMessageLongLineSplitsAtWhitespace(t *testing.T) {
	words := make([]string, 40)
	for i := range words {
		words[i] = "word" + strings.Repeat("x", 10)
	}
	text := strings.Join(words, " ")
	chunks := chunkMessage(text, 80)
	if got := strings.Join(chunks, ""); got != text {
		t.Fatalf("reassembly mismatch")
	}
	for i, c := range chunks {
		if utf16Len(escMD(c)) > 80 {
			t.Fatalf("chunk %d exceeds budget: %q", i, c)
		}
		if strings.TrimSpace(c) == "" {
			t.Fatalf("chunk %d is whitespace-only: %q", i, c)
		}
	}
}

func TestChunkMessageNoRuneSplitsWithEmojiAndCJK(t *testing.T) {
	text := strings.Repeat("🚀🧑‍💻中文字émoji", 300) // astral + CJK + combining
	chunks := chunkMessage(text, 50)
	if got := strings.Join(chunks, ""); got != text {
		t.Fatalf("reassembly mismatch — a rune was split or lost")
	}
	for i, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d invalid UTF-8", i)
		}
		if utf16Len(escMD(c)) > 50 {
			t.Fatalf("chunk %d exceeds budget", i)
		}
	}
}

func TestChunkMessageAccountsForEscaping(t *testing.T) {
	// Raw size well below the budget, but every rune escapes to 2 — the
	// chunker must split on escaped size, not raw size.
	text := strings.Repeat("a.b-c_d!e+f=g|h{i}j[k]l(m)n~o*p`q ", 50)
	budget := 200
	if utf16Len(text) <= budget {
		t.Fatalf("test setup: raw text must exceed budget")
	}
	chunks := chunkMessage(text, budget)
	if len(chunks) < 2 {
		t.Fatalf("expected chunking on escaped size, got %d chunks", len(chunks))
	}
	for i, c := range chunks {
		if utf16Len(escMD(c)) > budget {
			t.Fatalf("chunk %d escaped size %d exceeds %d", i, utf16Len(escMD(c)), budget)
		}
	}
	if got := strings.Join(chunks, ""); got != text {
		t.Fatalf("reassembly mismatch")
	}
}

func TestTrimPreviewRuneSafe(t *testing.T) {
	s := "日本語のテキストです"
	got := trimPreview(s, 4, "...")
	if !utf8.ValidString(got) {
		t.Fatalf("trimPreview split a rune: %q", got)
	}
	if got != "日本語の..." {
		t.Fatalf("got %q", got)
	}
	if trimPreview("short", 10, "...") != "short" {
		t.Fatalf("short text must be returned unchanged")
	}
}

// captureServer is a fake Telegram API that records sendMessage payloads.
type captureServer struct {
	mu   sync.Mutex
	text []string
	srv  *httptest.Server
}

func newCaptureServer() *captureServer {
	c := &captureServer{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		c.mu.Lock()
		c.text = append(c.text, payload.Text)
		c.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	return c
}

func (c *captureServer) wait(t *testing.T, want int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := len(c.text)
		out := append([]string(nil), c.text...)
		c.mu.Unlock()
		if n >= want {
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("timed out: got %d messages, want %d", len(c.text), want)
	return nil
}

func TestNotifyResponseChunksInOrderUntrimmed(t *testing.T) {
	cs := newCaptureServer()
	defer cs.srv.Close()
	n := New(Config{Token: "t", ChatID: 1, APIBase: cs.srv.URL})

	var b strings.Builder
	for i := 0; i < 300; i++ {
		b.WriteString("response line containing markdown _specials_ and emoji 🚀 plus CJK 中文字\n")
	}
	text := b.String() // > 4x the per-message budget

	n.NotifyResponse(context.Background(), "agent", text)

	msgs := cs.wait(t, 2)
	// Allow the queue to drain fully, then verify all of it.
	time.Sleep(50 * time.Millisecond)
	cs.mu.Lock()
	msgs = append([]string(nil), cs.text...)
	cs.mu.Unlock()

	if len(msgs) < 4 {
		t.Fatalf("expected >=4 chunks, got %d", len(msgs))
	}
	var body strings.Builder
	for i, m := range msgs {
		if utf16Len(m) > 4096 {
			t.Fatalf("message %d length %d exceeds Telegram limit", i, utf16Len(m))
		}
		if !strings.HasPrefix(m, "💬 *agent*") {
			t.Fatalf("message %d missing header: %q", i, m)
		}
		// The part marker ships ESCAPED since #439: an unescaped '(' is a
		// reserved character and Telegram 400s the whole chunk.
		if len(msgs) > 1 && !strings.Contains(m, `\(`+strconv.Itoa(i+1)+"/"+strconv.Itoa(len(msgs))+`\)`) {
			t.Fatalf("message %d missing (escaped) part marker: %q", i, m)
		}
		parts := strings.SplitN(m, "\n", 2)
		if len(parts) != 2 {
			t.Fatalf("message %d has no body: %q", i, m)
		}
		body.WriteString(unescapeMD(parts[1]))
	}
	if got := body.String(); got != text {
		t.Fatalf("delivered text != original (trimmed or reordered): got %d bytes, want %d", len(got), len(text))
	}
}

func TestNotifyResponseSingleMessageUnchanged(t *testing.T) {
	cs := newCaptureServer()
	defer cs.srv.Close()
	n := New(Config{Token: "t", ChatID: 1, APIBase: cs.srv.URL})

	n.NotifyResponse(context.Background(), "agent", "short reply")
	msgs := cs.wait(t, 1)
	if len(msgs) != 1 {
		t.Fatalf("want exactly 1 message, got %d", len(msgs))
	}
	if !strings.HasPrefix(msgs[0], "💬 *agent*\nshort reply") {
		t.Fatalf("unexpected message: %q", msgs[0])
	}
	if strings.Contains(msgs[0], "(1/1)") {
		t.Fatalf("single chunk must not carry a part marker: %q", msgs[0])
	}
}

func TestSendQueuePreservesOrderAcrossCalls(t *testing.T) {
	cs := newCaptureServer()
	defer cs.srv.Close()
	n := New(Config{Token: "t", ChatID: 1, APIBase: cs.srv.URL})

	for i := 1; i <= 20; i++ {
		n.sendAsync("msg " + strconv.Itoa(i))
	}
	msgs := cs.wait(t, 20)
	for i, m := range msgs {
		want := "msg " + strconv.Itoa(i+1)
		if m != want {
			t.Fatalf("out-of-order delivery at %d: got %q want %q", i, m, want)
		}
	}
}

// unescapeMD reverses escMD for content-reassembly checks. It handles exactly
// the escapes escMD produces (backslash before MarkdownV2 specials).
func unescapeMD(s string) string {
	var b strings.Builder
	esc := false
	for _, r := range s {
		if esc {
			b.WriteRune(r)
			esc = false
			continue
		}
		if r == '\\' {
			esc = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
