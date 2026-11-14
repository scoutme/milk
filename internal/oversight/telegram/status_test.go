package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/oversight"
)

// statusServer fakes the Bot API for the busy-status indicator: sendMessage
// (assigning ids, optionally failing), editMessageText (recording every
// call, optionally 429-ing the first N), and sendChatAction (recording
// timestamps, optionally 429-ing the first one).
type statusServer struct {
	mu           sync.Mutex
	messages     []string
	failSend     bool
	nextID       int64
	edits        []editCall // in arrival order
	editCalls    int
	edit429First int // first N editMessageText calls get429 retry_after=1
	actionTimes  []time.Time
	action429s   int // first N sendChatAction calls get429 retry_after=1
	srv          *httptest.Server
}

type editCall struct {
	msgID int64
	text  string
	ok    bool // false when answered429
}

func newStatusServer() *statusServer {
	s := &statusServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			s.mu.Lock()
			var payload struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if s.failSend {
				s.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "bad request"})
				return
			}
			s.nextID++
			id := s.nextID
			s.messages = append(s.messages, payload.Text)
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": id}})
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			q := r.URL.Query()
			msgID, _ := strconv.ParseInt(q.Get("message_id"), 10, 64)
			s.mu.Lock()
			s.editCalls++
			call := s.editCalls
			limited := call <= s.edit429First
			if !limited {
				s.edits = append(s.edits, editCall{msgID: msgID, text: q.Get("text"), ok: true})
			}
			s.mu.Unlock()
			if limited {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok": false, "error_code": 429, "description": "Too Many Requests",
					"parameters": map[string]any{"retry_after": 1},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
			s.mu.Lock()
			s.actionTimes = append(s.actionTimes, time.Now())
			n := len(s.actionTimes)
			limited := n <= s.action429s
			s.mu.Unlock()
			if limited {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok": false, "error_code": 429, "description": "Too Many Requests",
					"parameters": map[string]any{"retry_after": 1},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	return s
}

func (s *statusServer) actionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.actionTimes)
}

// msgCount/message read sent messages under the server lock (test closures
// run on the test goroutine while the fake server writes from workers).
func (s *statusServer) msgCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages)
}

func (s *statusServer) message(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= 0 && i < len(s.messages) {
		return s.messages[i]
	}
	return ""
}

// lastEdit returns the most recent successful edit (msgID, text).
func (s *statusServer) lastEdit() (int64, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.edits) == 0 {
		return 0, ""
	}
	e := s.edits[len(s.edits)-1]
	return e.msgID, e.text
}

func (s *statusServer) editCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.edits)
}

// editAttempts counts every editMessageText request, 429s included.
func (s *statusServer) editAttempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.editCalls
}

func waitForStatus(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func statusNotifier(s *statusServer, mutate func(*Config)) *Notifier {
	cfg := Config{
		Token:              "test-token",
		ChatID:             42,
		APIBase:            s.srv.URL,
		PermTimeout:        300 * time.Millisecond,
		BusyIndicator:      true,
		StatusInterval:     40 * time.Millisecond,
		ChatActionInterval: 50 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return New(cfg)
}

// TestBusyStatus_Lifecycle walks one turn through all three states and the
// final restore: 💭 thinking on the turn-start message, ⌨️ typing (with the
// native chat action) when the response streams, ⏳ waiting for you during a
// remote ask — with no chat action while waiting — then the status line
// stripped back to the bare turn-start message at turn end.
func TestBusyStatus_Lifecycle(t *testing.T) {
	s := newStatusServer()
	defer s.srv.Close()
	n := statusNotifier(s, nil)

	n.NotifyTurnStart(context.Background(), "mimo", "local", "do the thing")
	waitForStatus(t, 2*time.Second, "turn-start message", func() bool {
		return s.msgCount() == 1
	})
	waitForStatus(t, 2*time.Second, "first 💭 thinking… render", func() bool {
		_, text := s.lastEdit()
		return strings.Contains(text, "💭 thinking…") && strings.HasPrefix(text, s.message(0))
	})

	// Nothing streams yet: no native chat action during thinking.
	actionsBefore := s.actionCount()
	time.Sleep(150 * time.Millisecond)
	if got := s.actionCount(); got != actionsBefore {
		t.Fatalf("chat action fired while only thinking: %d → %d", actionsBefore, got)
	}

	// Response streams: ⌨️ label + native typing action.
	n.NotifyResponse(context.Background(), "mimo", "done deal")
	waitForStatus(t, 2*time.Second, "⌨️ typing… label", func() bool {
		_, text := s.lastEdit()
		return strings.Contains(text, "⌨️ typing…")
	})
	waitForStatus(t, 2*time.Second, "native typing chat action", func() bool {
		return s.actionCount() >= 1
	})

	// Remote ask: ⏳ waiting for you, and the chat action stops (the agent
	// isn't working — we're waiting on the human).
	askDone := make(chan struct{})
	go func() {
		defer close(askDone)
		_, _ = n.AskPermission(context.Background(), oversight.PermRequest{ToolName: "bash", Input: "rm -rf /", SilenceResolves: true})
	}()
	waitForStatus(t, 2*time.Second, "⏳ waiting for you… label", func() bool {
		_, text := s.lastEdit()
		return strings.Contains(text, "⏳ waiting for you…")
	})
	mid := s.actionCount()
	time.Sleep(120 * time.Millisecond)
	if got := s.actionCount(); got != mid {
		t.Fatalf("chat action fired during a wait: %d → %d", mid, got)
	}
	<-askDone // timeout path (PermTimeout=300ms, no reply)
	waitForStatus(t, 2*time.Second, "label restored after the ask", func() bool {
		_, text := s.lastEdit()
		return strings.Contains(text, "⌨️ typing…")
	})

	// Turn end: status line stripped — the bare 🔄 message again.
	n.NotifyTurnDone(context.Background(), "mimo", nil)
	waitForStatus(t, 2*time.Second, "restore to the bare turn-start message", func() bool {
		_, text := s.lastEdit()
		return text == s.message(0)
	})
	afterRestore := s.editCount()
	actionsAfter := s.actionCount()
	time.Sleep(200 * time.Millisecond)
	if got := s.editCount(); got != afterRestore {
		t.Fatalf("edits continued after restore: %d → %d", afterRestore, got)
	}
	if got := s.actionCount(); got != actionsAfter {
		t.Fatalf("chat actions continued after turn end: %d → %d", actionsAfter, got)
	}
}

// TestBusyStatus_OverlappingTurns pins the LIFO stack: only the most
// recent turn's message is edited; each NotifyTurnDone restores the top
// entry's own message; one loop serves both.
func TestBusyStatus_OverlappingTurns(t *testing.T) {
	s := newStatusServer()
	defer s.srv.Close()
	n := statusNotifier(s, nil)

	n.NotifyTurnStart(context.Background(), "mimo", "local", "turn A")
	waitForStatus(t, 2*time.Second, "message A", func() bool { return s.msgCount() >= 1 })
	n.NotifyTurnStart(context.Background(), "mimo", "escalation", "turn B")
	waitForStatus(t, 2*time.Second, "message B", func() bool { return s.msgCount() >= 2 })

	// Top entry (B, id 2) carries the status; once B is bound, A (id 1)
	// must stay bare — only edits after B's first render are in question
	// (A legitimately rendered while it was still the only turn).
	waitForStatus(t, 2*time.Second, "status on the top turn's message", func() bool {
		id, text := s.lastEdit()
		return id == 2 && strings.Contains(text, "💭 thinking…")
	})
	since := s.editCount()
	time.Sleep(150 * time.Millisecond)
	s.mu.Lock()
	for _, e := range s.edits[since:] {
		if e.msgID == 1 {
			s.mu.Unlock()
			t.Fatalf("status rendered on the non-top turn's message after B bound: %+v", e)
		}
	}
	s.mu.Unlock()

	// First done pops the top (B): B restored, A's message becomes the
	// status target (LIFO — see the turnStatus comment).
	n.NotifyTurnDone(context.Background(), "mimo", nil)
	waitForStatus(t, 2*time.Second, "B restored to its bare message", func() bool {
		id, text := s.lastEdit()
		return id == 2 && text == s.message(1)
	})
	waitForStatus(t, 2*time.Second, "A's message now carries the status", func() bool {
		id, text := s.lastEdit()
		return id == 1 && strings.Contains(text, "💭 thinking…")
	})

	// Last done: A restored, loop stops.
	n.NotifyTurnDone(context.Background(), "mimo", nil)
	waitForStatus(t, 2*time.Second, "A restored to its bare message", func() bool {
		id, text := s.lastEdit()
		return id == 1 && text == s.message(0)
	})
	final := s.editCount()
	time.Sleep(200 * time.Millisecond)
	if got := s.editCount(); got != final {
		t.Fatalf("edits continued after the last turn ended: %d → %d", final, got)
	}
}

// TestBusyStatus_SendFailure: with no message id (failed 🔄 send) the loop
// never edits anything and never panics — and the native typing action
// still fires when text streams, the belt under a lost id.
func TestBusyStatus_SendFailure(t *testing.T) {
	s := newStatusServer()
	defer s.srv.Close()
	s.failSend = true
	n := statusNotifier(s, nil)

	n.NotifyTurnStart(context.Background(), "mimo", "local", "doomed")
	n.NotifyResponse(context.Background(), "mimo", "some answer")
	waitForStatus(t, 2*time.Second, "typing action despite the lost id", func() bool {
		return s.actionCount() >= 1
	})
	time.Sleep(150 * time.Millisecond)
	if got := s.editCount(); got != 0 {
		t.Fatalf("edits attempted without a message id: %d", got)
	}
	n.NotifyTurnDone(context.Background(), "mimo", nil)
	time.Sleep(150 * time.Millisecond)
	if got := s.editCount(); got != 0 {
		t.Fatalf("restore attempted without a message id: %d", got)
	}
}

// TestBusyStatus_Disabled: busy_indicator off — messages still send, no
// status machinery at all.
func TestBusyStatus_Disabled(t *testing.T) {
	s := newStatusServer()
	defer s.srv.Close()
	n := statusNotifier(s, func(c *Config) { c.BusyIndicator = false })

	n.NotifyTurnStart(context.Background(), "mimo", "local", "quiet turn")
	waitForStatus(t, 2*time.Second, "turn-start message", func() bool {
		return s.msgCount() == 1
	})
	n.NotifyResponse(context.Background(), "mimo", "answer")
	n.NotifyTurnDone(context.Background(), "mimo", nil)
	time.Sleep(200 * time.Millisecond)
	if got := s.editCount(); got != 0 {
		t.Fatalf("disabled indicator still rendered status: %d edits", got)
	}
	if got := s.actionCount(); got != 0 {
		t.Fatalf("disabled indicator still sent chat actions: %d", got)
	}
}

// TestBusyStatus_AnnounceNeverEngages: startup/setup announcements share
// the message but never the status line (no turn ever completes them).
func TestBusyStatus_AnnounceNeverEngages(t *testing.T) {
	s := newStatusServer()
	defer s.srv.Close()
	n := statusNotifier(s, nil)

	n.Announce("milk", "startup", "oversight active")
	waitForStatus(t, 2*time.Second, "announcement message", func() bool {
		return s.msgCount() == 1
	})
	time.Sleep(200 * time.Millisecond)
	if got := s.editCount(); got != 0 {
		t.Fatalf("announcement engaged the status line: %d edits", got)
	}
	if got := s.actionCount(); got != 0 {
		t.Fatalf("announcement engaged the chat action: %d", got)
	}
}

// TestBusyStatus_Edit429Backoff: a rate-limited render backs off by the
// server's retry_after instead of hammering editMessageText.
func TestBusyStatus_Edit429Backoff(t *testing.T) {
	s := newStatusServer()
	defer s.srv.Close()
	s.edit429First = 1
	n := statusNotifier(s, nil)

	n.NotifyTurnStart(context.Background(), "mimo", "local", "backoff turn")
	waitForStatus(t, 3*time.Second, "first (429'd) edit attempt", func() bool {
		return s.editAttempts() >= 1
	})
	// The next attempt must wait out retry_after (1s + slack), not the 40ms
	// render tick.
	waitForStatus(t, 3*time.Second, "render after the backoff window", func() bool {
		return s.editCount() >= 1
	})
	n.NotifyTurnDone(context.Background(), "mimo", nil)
	waitForStatus(t, 3*time.Second, "restore after backoff", func() bool {
		_, text := s.lastEdit()
		return text == s.message(0)
	})
}

// TestNotifyTurnDone_InterruptedRendersAsInterrupt: a cancelled turn (Ctrl+C,
// /interrupt, session/cancel — plain or wrapped) reports "⏹ turn
// interrupted", never the ❌ error rendering; other errors keep it.
func TestNotifyTurnDone_InterruptedRendersAsInterrupt(t *testing.T) {
	s := newStatusServer()
	defer s.srv.Close()
	n := statusNotifier(s, func(c *Config) { c.BusyIndicator = false })

	n.NotifyTurnDone(context.Background(), "mimo", context.Canceled)
	wrapped := fmt.Errorf("turn: %w", context.Canceled)
	n.NotifyTurnDone(context.Background(), "mimo", wrapped)
	n.NotifyTurnDone(context.Background(), "mimo", context.DeadlineExceeded)

	waitForStatus(t, 2*time.Second, "all three done messages", func() bool {
		return s.msgCount() == 3
	})
	for i := 0; i < 2; i++ {
		msg := s.message(i)
		if !strings.Contains(msg, "⏹ *mimo* turn interrupted") || strings.Contains(msg, "error:") {
			t.Fatalf("message %d = %q, want ⏹ interrupted without error", i, msg)
		}
	}
	if msg := s.message(2); !strings.Contains(msg, "❌") || !strings.Contains(msg, "deadline") {
		t.Fatalf("non-cancel error lost its ❌ rendering: %q", msg)
	}
}
