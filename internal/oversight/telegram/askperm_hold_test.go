package telegram

// Tests for the prompt-slot hold rules (ADR-0052 amendment): a remote prompt
// is released by a reply or the ask's own cancellation — never by an
// internal timeout while the ask is still open on its other surfaces, and
// cancellation withdraws quietly (the winning surface shows its own outcome;
// a "⏱ timeout" notice there would contradict it).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/oversight"
)

// sentSnapshot returns a copy of the messages the fake server recorded
// (locked — the HTTP handler writes concurrently).
func sentSnapshot(t *testing.T, s *apiServer) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

func timeoutNotices(sent []string) []string {
	var out []string
	for _, m := range sent {
		if strings.Contains(m, "⏱") {
			out = append(out, m)
		}
	}
	return out
}

// expectNoTimeoutNotice watches the chat briefly for a ⏱ notice — the post
// is async (sendAsync), so asserting absence needs a grace window or a lost
// notice would pass unnoticed.
func expectNoTimeoutNotice(t *testing.T, s *apiServer) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if notices := timeoutNotices(sentSnapshot(t, s)); len(notices) != 0 {
			t.Fatalf("no timeout notice may be posted here: %v", notices)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Without SilenceResolves the backend holds its prompt past PermTimeout: no
// timeout notice, slot still registered — only the ask's cancellation ends
// the wait (a late reply must still be able to answer it).
func TestAskPermission_HoldsPastTimeoutWhenSilenceDoesNotResolve(t *testing.T) {
	s := newAPIServer() // no replies ever
	defer s.srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := New(Config{Token: "t", ChatID: 1, APIBase: s.srv.URL, PermTimeout: 30 * time.Millisecond})
	n.StartPolling(ctx)

	done := make(chan struct{})
	var answered bool
	go func() {
		defer close(done)
		_, answered = n.AskPermission(ctx, oversight.PermRequest{ToolName: "bash"})
	}()
	select {
	case <-done:
		t.Fatal("the prompt must outlive PermTimeout while the ask is open")
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation must end the wait")
	}
	if answered {
		t.Fatal("cancellation is not an answer")
	}
	expectNoTimeoutNotice(t, s)
}

// SilenceResolves opts into the historical behavior: PermTimeout applies
// TimeoutAction and releases the slot, with the notice on the chat.
func TestAskPermission_TimesOutOnlyWhenSilenceResolves(t *testing.T) {
	s := newAPIServer() // no replies ever
	defer s.srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := New(Config{Token: "t", ChatID: 1, APIBase: s.srv.URL, PermTimeout: 30 * time.Millisecond})
	n.StartPolling(ctx)

	dec, ok := n.AskPermission(ctx, oversight.PermRequest{ToolName: "bash", SilenceResolves: true})
	if ok {
		t.Fatal("silence must not be an answer")
	}
	if dec != oversight.PermTimeout {
		t.Fatalf("decision = %v, want PermTimeout (deny default)", dec)
	}
	// The notice is queued (sendAsync) — wait for it to land.
	var notices, all []string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		all = sentSnapshot(t, s)
		notices = timeoutNotices(all)
		if len(notices) != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(notices) == 0 || !strings.Contains(notices[0], "Denied (timeout)") {
		t.Fatalf("timeout notice missing: %v (sent: %v)", notices, all)
	}
}

// timeout_action=allow is honoured the same way: the fuse fires, PermAllow
// comes back unanswered, and the chat says "Allowed (timeout)" — the config
// feature the foreground prompt's countdown promises.
func TestAskPermission_TimeoutActionAllow(t *testing.T) {
	s := newAPIServer() // no replies ever
	defer s.srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := New(Config{Token: "t", ChatID: 1, APIBase: s.srv.URL, PermTimeout: 30 * time.Millisecond, TimeoutAction: "allow"})
	n.StartPolling(ctx)

	dec, ok := n.AskPermission(ctx, oversight.PermRequest{ToolName: "bash", SilenceResolves: true})
	if ok {
		t.Fatal("silence must not be an answer")
	}
	if dec != oversight.PermAllow {
		t.Fatalf("decision = %v, want PermAllow (timeout_action allow)", dec)
	}
	var notices []string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		notices = timeoutNotices(sentSnapshot(t, s))
		if len(notices) != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(notices) == 0 || !strings.Contains(notices[0], "Allowed (timeout)") {
		t.Fatalf("timeout notice missing: %v", notices)
	}
}

// Cancellation is the ask ending on another surface (e.g. the TUI prompt was
// answered): withdraw quietly — the old code posted the timeout notice here
// too, contradicting the answer the user had just given.
func TestAskPermission_CancelIsQuiet(t *testing.T) {
	s := newAPIServer() // no replies ever
	defer s.srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := New(Config{Token: "t", ChatID: 1, APIBase: s.srv.URL, PermTimeout: 10 * time.Second})
	n.StartPolling(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = n.AskPermission(ctx, oversight.PermRequest{ToolName: "bash", SilenceResolves: true})
	}()
	// Give the prompt time to go out, then end the ask from another surface.
	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation must end the wait")
	}
	expectNoTimeoutNotice(t, s)
}
