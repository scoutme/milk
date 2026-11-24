package telegram

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// sendMessageResponder answers /sendMessage with 429{retry_after: first429}
// while first429 >= 0 (the first call), then success with messageID. Every
// call is counted.
func sendMessageResponder(t *testing.T, first429 int, messageID int64) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if first429 >= 0 && n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":          false,
				"error_code":  429,
				"description": "Too Many Requests: retry after 1",
				"parameters":  map[string]any{"retry_after": first429},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": messageID},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestSend_HonorsShortRetryAfterThenDelivers: a short 429 penalty is waited
// out and the same message retried — before the fix it was dropped silently
// (issue #325).
func TestSend_HonorsShortRetryAfterThenDelivers(t *testing.T) {
	srv, calls := sendMessageResponder(t, 1, 42)
	n := New(Config{Token: "t", ChatID: 1, APIBase: srv.URL})

	start := time.Now()
	id := n.send("hello")
	elapsed := time.Since(start)

	if id != 42 {
		t.Fatalf("send after short cooldown: id = %d, want 42 (message was dropped)", id)
	}
	if c := calls.Load(); c != 2 {
		t.Fatalf("server calls = %d, want 2 (429 + retry)", c)
	}
	if elapsed < 1200*time.Millisecond {
		t.Fatalf("send returned after %v — did not wait out retry_after=1s (+200ms slack)", elapsed)
	}
}

// TestSend_LongCooldownDropsAndGatesTheWire: a flood-scale penalty (> maxInlineSendWait)
// drops the message without stalling the queue — and, crucially, every
// subsequent send stays off the wire entirely, so an active turn can no longer
// renew the penalty with failed attempts (the outage-forever mechanism).
func TestSend_LongCooldownDropsAndGatesTheWire(t *testing.T) {
	srv, calls := sendMessageResponder(t, 3600, 42)
	n := New(Config{Token: "t", ChatID: 1, APIBase: srv.URL})

	if id := n.send("flooded"); id != 0 {
		t.Fatalf("during long cooldown: id = %d, want 0", id)
	}
	if c := calls.Load(); c != 1 {
		t.Fatalf("server calls after first 429 = %d, want 1 (the 429 itself)", c)
	}
	if wait := time.Until(n.gateUntil()); wait < 3500*time.Second {
		t.Fatalf("cooldown gate not armed: gateUntil in %v, want ~3600s", wait)
	}

	// Further sends — including concurrent ones from the queue-full overflow
	// path — must return immediately without reaching the API.
	done := make(chan int64, 4)
	for i := 0; i < 4; i++ {
		go func() { done <- n.send("queued") }()
	}
	start := time.Now()
	for i := 0; i < 4; i++ {
		if id := <-done; id != 0 {
			t.Errorf("gated send returned id %d, want 0", id)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("gated sends took %v — they should drop instantly, not wait the cooldown", elapsed)
	}
	if c := calls.Load(); c != 1 {
		t.Fatalf("server calls while gated = %d, want 1 — the cooldown must keep sends off the wire", c)
	}
}

// TestSend_Non429FailureKeepsHistoricalBehavior: errors without retry_after
// still fail fast with no retry (single call, id 0).
func TestSend_Non429FailureKeepsHistoricalBehavior(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":          false,
			"error_code":  400,
			"description": "Bad Request: chat not found",
		})
	}))
	t.Cleanup(srv.Close)
	n := New(Config{Token: "t", ChatID: 1, APIBase: srv.URL})

	if id := n.send("nope"); id != 0 {
		t.Fatalf("id = %d, want 0", id)
	}
	if c := calls.Load(); c != 1 {
		t.Fatalf("server calls = %d, want 1 — non-429 failures must not retry", c)
	}
	if !n.gateUntil().IsZero() {
		t.Fatal("non-429 failure must not arm the cooldown gate")
	}
}
