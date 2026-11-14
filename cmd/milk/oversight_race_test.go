package main

// Tests for the TUI-side remote-oversight permission race: local-agent asks
// (makeLocalPermAsk — tool permissions and the doom-loop gate's confirmation)
// must reach the remote notifier and not just the TUI prompt, the first
// answer wins, and a losing TUI prompt is withdrawn via permDismissMsg so the
// input isn't trapped on a question that has already been decided.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/events"
	"github.com/scoutme/milk/internal/oversight"
)

// blockingHost is an events.Host whose RequestPermission blocks until its ctx
// ends — the shape of a TUI prompt nobody answers (the remote side is expected
// to win the race and cancel it).
type blockingHost struct {
	calls chan string
}

func (h *blockingHost) Notify(events.Notification) {}
func (h *blockingHost) State(events.StateUpdate)   {}
func (h *blockingHost) RequestPermission(ctx context.Context, req events.PermissionRequest) (events.PermissionOutcome, error) {
	if h.calls != nil {
		h.calls <- req.Tool
	}
	<-ctx.Done()
	return events.PermissionOutcome{}, ctx.Err()
}
func (h *blockingHost) Elicit(ctx context.Context, req events.ElicitationRequest) (events.ElicitationResult, error) {
	<-ctx.Done()
	return events.ElicitationResult{}, ctx.Err()
}

func TestMakeLocalPermAsk_RemoteAnswerWins(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision oversight.PermDecision
		want     bool
	}{
		{"remote allows", oversight.PermAllow, true},
		{"remote denies", oversight.PermDeny, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &fakeNotifier{permCh: make(chan oversight.PermDecision, 1)}
			n.permCh <- tc.decision
			host := &blockingHost{calls: make(chan string, 1)}
			ask := makeLocalPermAsk(host, nil, n, &interactiveState{})

			done := make(chan bool, 1)
			go func() { done <- ask("bash", "ls") }()
			select {
			case allow := <-done:
				if allow != tc.want {
					t.Errorf("allow = %v, want %v", allow, tc.want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("ask did not return — the remote answer must win the race")
			}
			// #323 quiescence wait: the detached direct goroutine reads the
			// isTTY global while building its prompt (permAskPrompt is
			// evaluated as RequestPermission's argument, i.e. strictly
			// before the host call), and the remote answer can resolve the
			// ask without that goroutine being joined. Receiving the host
			// call therefore orders its isTTY read before this test
			// returns, so every later isTTY write in the package happens
			// after it instead of racing with it.
			select {
			case tool := <-host.calls:
				if tool != "bash" {
					t.Errorf("host call tool = %q, want bash", tool)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("direct prompt goroutine never reached the host — the #323 quiescence wait needs it")
			}
			if !hasCall(n.recorded(), "perm:bash") {
				t.Errorf("remote notifier not asked; recorded: %v", n.recorded())
			}
		})
	}
}

// TestMakeLocalPermAsk_NoRemote_ClientOnly: with no remote backend the ask is
// just the TUI prompt (nil and Noop notifiers both degrade to client-only).
func TestMakeLocalPermAsk_NoRemote_ClientOnly(t *testing.T) {
	for _, n := range []oversight.Notifier{nil, oversight.Noop{}} {
		// Deliberately never touches the isTTY global (#323): under
		// `go test` it is already false (the test binary's stdout is a
		// pipe), the assertions below are styling-independent, and this
		// write was the racing half of the reported data race against
		// the previous test's detached prompt goroutine.
		var gotPrompt, gotLabel string
		ir := &tuiInputReader{send: fakeSyncSend(t, "y", &gotPrompt, &gotLabel)}
		ask := makeLocalPermAsk(newTUIHost(ir), nil, n, &interactiveState{})
		if !ask("bash", "ls") {
			t.Errorf("notifier %T: expected the client's y to allow", n)
		}
		if !strings.Contains(gotPrompt, "permission request") {
			t.Errorf("notifier %T: prompt = %q, want the standard permission prompt", n, gotPrompt)
		}
	}
}

// TestTUIHost_RequestPermission_WithdrawnOnCancel: when ctx ends before the
// user answers (remote oversight won), the prompt is withdrawn with a
// permDismissMsg keyed to the same respCh and an error is returned — the TUI
// must never stay parked on a decided question. An ask resolved before it was
// ever shown must not flash a prompt at all.
func TestTUIHost_RequestPermission_WithdrawnOnCancel(t *testing.T) {
	var mu sync.Mutex
	var msgs []tea.Msg
	ir := &tuiInputReader{send: func(m tea.Msg) {
		mu.Lock()
		msgs = append(msgs, m)
		mu.Unlock()
	}}
	snapshot := func() []tea.Msg {
		mu.Lock()
		defer mu.Unlock()
		out := make([]tea.Msg, len(msgs))
		copy(out, msgs)
		return out
	}
	host := newTUIHost(ir)

	// Already-resolved ask: error without ever showing a prompt (no flash,
	// nothing to withdraw).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := host.RequestPermission(ctx, events.PermissionRequest{Prompt: "allow? "}); err == nil {
		t.Fatal("expected an error when the request is cancelled")
	}
	if n := len(snapshot()); n != 0 {
		t.Fatalf("an already-resolved ask must not show a prompt, got %d message(s)", n)
	}

	// Cancel while the prompt is up: it is withdrawn with a matching dismiss.
	ctx2, cancel2 := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := host.RequestPermission(ctx2, events.PermissionRequest{Prompt: "allow? "})
		errCh <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	var req permRequestMsg
	for time.Now().Before(deadline) {
		found := false
		for _, m := range snapshot() {
			if v, ok := m.(permRequestMsg); ok {
				req, found = v, true
			}
		}
		if found {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if req.respCh == nil {
		t.Fatal("no permRequestMsg sent")
	}
	cancel2()
	if err := <-errCh; err == nil {
		t.Fatal("expected an error when the request is cancelled")
	}

	var dismiss *permDismissMsg
	for _, m := range snapshot() {
		if v, ok := m.(permDismissMsg); ok {
			dismiss = &v
		}
	}
	if dismiss == nil {
		t.Fatal("no permDismissMsg sent — the prompt would stay parked in the TUI")
	}
	if req.respCh != dismiss.respCh {
		t.Error("permDismissMsg respCh must match the withdrawn prompt's respCh")
	}
}

// TestHandlePermDismiss_ClearsPendingAndPromotesQueue: dismissing the pending
// prompt removes it and promotes the next queued one; dismissing a queued
// prompt drops just that entry.
func TestHandlePermDismiss_ClearsPendingAndPromotesQueue(t *testing.T) {
	pendingResp := make(chan string, 1)
	queuedResp := make(chan string, 1)
	m := model{st: &interactiveState{}, transcript: &strings.Builder{}, ta: textarea.New()}
	m.pendingPerm = &permRequestMsg{prompt: "first", respCh: pendingResp}
	m.permQueue = []permRequestMsg{{prompt: "second", respCh: queuedResp}}

	nm, _ := m.handlePermDismiss(permDismissMsg{respCh: pendingResp})
	mm := nm.(model)
	if mm.pendingPerm == nil || mm.pendingPerm.respCh != queuedResp {
		t.Fatalf("expected the queued prompt promoted, got %+v", mm.pendingPerm)
	}
	if len(mm.permQueue) != 0 {
		t.Fatalf("expected an empty queue, got %d entries", len(mm.permQueue))
	}
}

func TestHandlePermDismiss_QueuedPromptDropped(t *testing.T) {
	pendingResp := make(chan string, 1)
	queuedResp := make(chan string, 1)
	m := model{st: &interactiveState{}, transcript: &strings.Builder{}, ta: textarea.New()}
	m.pendingPerm = &permRequestMsg{prompt: "first", respCh: pendingResp}
	m.permQueue = []permRequestMsg{{prompt: "second", respCh: queuedResp}}

	nm, _ := m.handlePermDismiss(permDismissMsg{respCh: queuedResp})
	mm := nm.(model)
	if mm.pendingPerm == nil || mm.pendingPerm.respCh != pendingResp {
		t.Fatalf("the pending prompt must stay untouched, got %+v", mm.pendingPerm)
	}
	if len(mm.permQueue) != 0 {
		t.Fatalf("expected the queued prompt dropped, got %d entries", len(mm.permQueue))
	}
}
