package main

// Tests for ACP mid-turn steering (issue #441, ADR-0108): a session/prompt
// that arrives while a turn runs is queued for the running turn's
// steerDrain instead of only serializing behind turnMu. Covers the three
// outcomes — delivered into the running turn (one echo, no second turn),
// non-steerable entries keeping the flush path, a pending wizard refusing
// delivery — plus the queue cap and the remote-input drain order. The unit
// tests drive popSteer/enqueueSteer directly on a bare acpSession with a
// fakeACPConn for the echoes.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/transport/acp"
)

// asyncPromptResult is one session/prompt answered off the test goroutine.
type asyncPromptResult struct {
	resp acp.PromptResponse
	err  error
}

// acpPromptAsync fires session/prompt without blocking the caller — the
// busy-prompt tests need the test goroutine free to wait on queue state
// (acpRequest's t.Fatalf must not run off-goroutine, so this variant
// reports errors through the channel instead).
func acpPromptAsync(server *acpServer, id acp.SessionID, text string) <-chan asyncPromptResult {
	out := make(chan asyncPromptResult, 1)
	raw, err := json.Marshal(acp.PromptRequest{
		SessionID: id,
		Prompt:    []acp.ContentBlock{acp.TextBlock(text)},
	})
	if err != nil {
		out <- asyncPromptResult{err: err}
		return out
	}
	go func() {
		result, err := server.HandleRequest(context.Background(), "session/prompt", raw)
		if err != nil {
			out <- asyncPromptResult{err: err}
			return
		}
		resp, ok := result.(acp.PromptResponse)
		if !ok {
			out <- asyncPromptResult{err: fmt.Errorf("session/prompt returned %T, want acp.PromptResponse", result)}
			return
		}
		out <- asyncPromptResult{resp: resp}
	}()
	return out
}

// writeSSE answers one model request with a single content chunk — the
// shape the stub backends in acp_server_test.go use (no usage line needed).
func writeSSE(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", content)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// acpUserUpserts returns every user_message upsert sent on the conn, in
// order — the accept-echoes of prompts.
func acpUserUpserts(conn *fakeACPConn) []acp.MessageUpsert {
	var out []acp.MessageUpsert
	for _, n := range conn.sent() {
		w, ok := n.Params.(acp.UpdateSessionNotification)
		if !ok {
			continue
		}
		if u, ok := w.Update.(acp.MessageUpsert); ok && u.SessionUpdate == "user_message" {
			out = append(out, u)
		}
	}
	return out
}

// acpIdleCount counts idle state_updates — one per completed turn, so it
// tells "rode the running turn" (1) from "ran its own turn" (2).
func acpIdleCount(conn *fakeACPConn) int {
	n := 0
	for _, notif := range conn.sent() {
		w, ok := notif.Params.(acp.UpdateSessionNotification)
		if !ok {
			continue
		}
		if s, ok := w.Update.(acp.StateUpdate); ok && s.State == acp.SessionStateIdle {
			n++
		}
	}
	return n
}

// steerQueueLen reads the session's steering queue length.
func steerQueueLen(as *acpSession) int {
	as.steerMu.Lock()
	defer as.steerMu.Unlock()
	return len(as.steerQueue)
}

// TestACPSteer_QueuedPromptDeliversIntoRunningTurn: prompt A runs, prompt B
// arrives mid-turn and is queued with a receipt; the terminal drain of A's
// single-response turn pulls B as a continuation — B's text reaches the
// model inside A's request cycle, B's response carries the delivery echo's
// messageId, and no second turn ever runs (one idle state, two model
// requests total).
func TestACPSteer_QueuedPromptDeliversIntoRunningTurn(t *testing.T) {
	gate := make(chan struct{})
	var gateOnce sync.Once
	releaseGate := func() { gateOnce.Do(func() { close(gate) }) }
	t.Cleanup(releaseGate) // a failed wait must not strand the handler (hung httptest.Close)
	req1 := make(chan struct{}, 1)
	var mu sync.Mutex
	var bodies []string
	server, conn := acpTestServerWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		n := len(bodies)
		mu.Unlock()
		if n == 1 {
			req1 <- struct{}{} // the loop's pre-request drain has already run
			<-gate             // hold turn A open until prompt B is queued
		}
		if n == 1 {
			writeSSE(w, "first answer")
			return
		}
		writeSSE(w, "second answer")
	})
	id := acpNewSession(t, server)
	as := server.session(id)

	doneA := acpPromptAsync(server, id, "original prompt")
	select {
	case <-req1:
	case <-time.After(2 * time.Second):
		t.Fatal("turn A never reached the model")
	}

	// B enqueues after request 1 is already in flight — the loop's
	// first-iteration drain has run, so nothing can pop B until the
	// gated response hands the turn its terminal boundary.
	doneB := acpPromptAsync(server, id, "steer me mid-turn")
	waitFor(t, "prompt B queued with its receipt", func() bool {
		if steerQueueLen(as) != 1 {
			return false
		}
		chunks := strings.Join(acpChunks(conn), "\n")
		return strings.Contains(chunks, "queued") && strings.Contains(chunks, "next step")
	})
	releaseGate()

	if ra := <-doneA; ra.err != nil {
		t.Fatalf("prompt A: %v", ra.err)
	}
	rb := <-doneB
	if rb.err != nil {
		t.Fatalf("prompt B: %v", rb.err)
	}

	mu.Lock()
	got := append([]string(nil), bodies...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("model requests = %d, want 2 (B steered as a continuation of A's turn, no turn of its own)", len(got))
	}
	if !strings.Contains(got[1], "first answer") || !strings.Contains(got[1], "steer me mid-turn") {
		t.Errorf("second request must carry A's answer-so-far plus the steered prompt; got %q", got[1])
	}

	// B's response id is the echo the drain sent mid-turn (the schema's
	// messageId identity rule: the client saw its message under that id).
	var echoID acp.MessageID
	for _, u := range acpUserUpserts(conn) {
		if len(u.Content) > 0 && strings.Contains(u.Content[0].Text, "steer me mid-turn") {
			echoID = u.MessageID
		}
	}
	if echoID == "" {
		t.Fatal("steered prompt was never echoed to the client")
	}
	if rb.resp.MessageID != echoID {
		t.Errorf("prompt B response messageId = %q, want the delivery echo %q", rb.resp.MessageID, echoID)
	}
	if n := acpIdleCount(conn); n != 1 {
		t.Errorf("idle states = %d, want 1 — B rode A's turn instead of running one", n)
	}
	if n := steerQueueLen(as); n != 0 {
		t.Errorf("queue not drained: %d entries left", n)
	}
}

func TestACPSteer_NonSteerablePromptKeepsFlushPath(t *testing.T) {
	gate := make(chan struct{})
	var gateOnce sync.Once
	releaseGate := func() { gateOnce.Do(func() { close(gate) }) }
	t.Cleanup(releaseGate) // a failed wait must not strand the handler (hung httptest.Close)
	req1 := make(chan struct{}, 1)
	var mu sync.Mutex
	var bodies []string
	server, conn := acpTestServerWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		n := len(bodies)
		mu.Unlock()
		if n == 1 {
			req1 <- struct{}{} // the loop's pre-request drain has already run
			<-gate             // hold turn A open until prompt B is queued
		}
		writeSSE(w, fmt.Sprintf("answer %d", n))
	})
	id := acpNewSession(t, server)
	as := server.session(id)

	doneA := acpPromptAsync(server, id, "original prompt")
	select {
	case <-req1:
	case <-time.After(2 * time.Second):
		t.Fatal("turn A never reached the model")
	}

	doneB := acpPromptAsync(server, id, "/tasks") // slash command — the flush path owns it
	waitFor(t, "prompt B queued", func() bool { return steerQueueLen(as) == 1 })
	releaseGate()

	if ra := <-doneA; ra.err != nil {
		t.Fatalf("prompt A: %v", ra.err)
	}
	rb := <-doneB
	if rb.err != nil {
		t.Fatalf("prompt B: %v", rb.err)
	}

	mu.Lock()
	got := append([]string(nil), bodies...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("model requests = %d, want 1 (a slash prompt must stop the drain, not ride it)", len(got))
	}
	if strings.Contains(got[0], "/tasks") {
		t.Error("slash prompt leaked into the running turn's request")
	}

	// B ran as its own turn after A finished: its echo comes after A's
	// answer, and its slash dispatch produced a second idle.
	answerIdx, echoIdx := -1, -1
	for i, n := range conn.sent() {
		w, ok := n.Params.(acp.UpdateSessionNotification)
		if !ok {
			continue
		}
		switch u := w.Update.(type) {
		case acp.ContentChunk:
			if answerIdx < 0 && strings.Contains(u.Content.Text, "answer 1") {
				answerIdx = i
			}
		case acp.MessageUpsert:
			if u.SessionUpdate == "user_message" && len(u.Content) > 0 && strings.Contains(u.Content[0].Text, "/tasks") {
				echoIdx = i
			}
		}
	}
	if answerIdx < 0 || echoIdx < 0 {
		t.Fatalf("missing A's answer or B's echo: answer=%d echo=%d", answerIdx, echoIdx)
	}
	if echoIdx < answerIdx {
		t.Error("B's echo arrived before A's turn finished — it should run as the next turn")
	}
	if n := acpIdleCount(conn); n != 2 {
		t.Errorf("idle states = %d, want 2 (A's turn + B's slash turn)", n)
	}
	if n := steerQueueLen(as); n != 0 {
		t.Errorf("queue not drained: %d entries left", n)
	}
}

func TestACPSteer_WizardArmedRefusesDrain(t *testing.T) {
	conn := &fakeACPConn{}
	as := &acpSession{conn: conn, runID: "test01"}
	as.pendingInit = &initWizardState{} // armed by a previous /config init turn

	e := as.enqueueSteer("the wizard's answer", nil)
	if e == nil {
		t.Fatal("enqueue failed")
	}
	if got := as.popSteer(); len(got) != 0 {
		t.Fatalf("popSteer delivered %q while a wizard is armed", got)
	}
	if n := steerQueueLen(as); n != 1 {
		t.Fatalf("queue = %d entries after the refusal, want the entry to survive", n)
	}

	as.pendingInit = nil
	got := as.popSteer()
	if len(got) != 1 || got[0] != "the wizard's answer" {
		t.Fatalf("popSteer = %q, want the refused entry once the wizard is gone", got)
	}
	if id, delivered := as.steerResult(e); !delivered || id == "" {
		t.Fatal("delivered entry must carry its echo id")
	}
	if n := steerQueueLen(as); n != 0 {
		t.Errorf("delivered entry still queued: %d", n)
	}
	if ups := acpUserUpserts(conn); len(ups) != 1 || !strings.Contains(ups[0].Content[0].Text, "the wizard's answer") {
		t.Fatalf("delivery must echo the user message exactly once, got %d upserts", len(ups))
	}
}

// TestACPSteer_QueueCapFallsBackToBlocking: past queuePromptCap the enqueue
// refuses (nil) — the caller falls back to the plain serialize-behind-
// turnMu path rather than dropping input, the ACP reading of the TUI's
// queue-full refusal.
func TestACPSteer_QueueCapFallsBackToBlocking(t *testing.T) {
	as := &acpSession{}
	for i := 0; i < queuePromptCap; i++ {
		if e := as.enqueueSteer(fmt.Sprintf("prompt %d", i), nil); e == nil {
			t.Fatalf("enqueue %d refused below the cap (%d)", i, queuePromptCap)
		}
	}
	if e := as.enqueueSteer("one too many", nil); e != nil {
		t.Fatal("queue accepted input past queuePromptCap — the caller must fall back to turnMu")
	}
	if n := steerQueueLen(as); n != queuePromptCap {
		t.Errorf("queue = %d entries, want the cap %d", n, queuePromptCap)
	}
}

// TestACPSteer_RemoteInputsDrainAfterPromptQueue: TUI order over ACP — a
// non-steerable prompt head blocks remote-oversight inputs from jumping it,
// and once the prompt queue is empty the drain takes the steerable remote
// prefix (echoing each) so Telegram input can act on the live turn too.
func TestACPSteer_RemoteInputsDrainAfterPromptQueue(t *testing.T) {
	conn := &fakeACPConn{}
	as := &acpSession{conn: conn, runID: "test01"}
	if e := as.enqueueSteer("/tasks", nil); e == nil {
		t.Fatal("enqueue failed")
	}
	as.mu.Lock()
	as.pendingRemoteInputs = []string{"telegram says hi"}
	as.mu.Unlock()

	if got := as.popSteer(); len(got) != 0 {
		t.Fatalf("remote input jumped the blocked prompt queue: %q", got)
	}
	as.steerMu.Lock()
	as.steerQueue = nil
	as.steerMu.Unlock()

	got := as.popSteer()
	if len(got) != 1 || got[0] != "telegram says hi" {
		t.Fatalf("popSteer = %q, want the remote input once the prompt queue is empty", got)
	}
	as.mu.Lock()
	remaining := len(as.pendingRemoteInputs)
	as.mu.Unlock()
	if remaining != 0 {
		t.Errorf("%d remote inputs left in the queue after the drain", remaining)
	}
	if ups := acpUserUpserts(conn); len(ups) != 1 || !strings.Contains(ups[0].Content[0].Text, "telegram says hi") {
		t.Fatalf("drained remote input must be echoed exactly once, got %d upserts", len(ups))
	}
}
