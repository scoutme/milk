package main

// Tests for the TUI-side remote-oversight permission race: local-agent asks
// (makeLocalPermAsk — tool permissions and the doom-loop gate's confirmation)
// must reach the remote notifier and not just the TUI prompt, the first
// answer wins, and a losing TUI prompt is withdrawn via permDismissMsg so the
// input isn't trapped on a question that has already been decided.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

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
			host := &blockingHost{}
			ask := makeLocalPermAsk(host, nil, n)

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
		oldTTY := isTTY
		isTTY = false
		t.Cleanup(func() { isTTY = oldTTY })

		var gotPrompt, gotLabel string
		ir := &tuiInputReader{send: fakeSyncSend(t, "y", &gotPrompt, &gotLabel)}
		ask := makeLocalPermAsk(newTUIHost(ir), nil, n)
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
// must never stay parked on a decided question.
func TestTUIHost_RequestPermission_WithdrawnOnCancel(t *testing.T) {
	var msgs []tea.Msg
	ir := &tuiInputReader{send: func(m tea.Msg) { msgs = append(msgs, m) }}
	host := newTUIHost(ir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := host.RequestPermission(ctx, events.PermissionRequest{Prompt: "allow? "})
	if err == nil {
		t.Fatal("expected an error when the request is cancelled")
	}

	var req, dismiss *permRequestMsg
	for _, m := range msgs {
		switch v := m.(type) {
		case permRequestMsg:
			req = &v
		case permDismissMsg:
			d := permRequestMsg{respCh: v.respCh}
			dismiss = &d
		}
	}
	if req == nil {
		t.Fatal("no permRequestMsg sent")
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
	m := model{st: &interactiveState{}, transcript: &strings.Builder{}, transcriptNoThink: &strings.Builder{}, ta: textarea.New()}
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
	m := model{st: &interactiveState{}, transcript: &strings.Builder{}, transcriptNoThink: &strings.Builder{}, ta: textarea.New()}
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
