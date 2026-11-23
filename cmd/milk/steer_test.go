package main

// Host side of mid-turn steering (issue #441): what may be pulled into the
// running turn at its next model-request boundary, and how a drained item is
// rendered back into the transcript.

import (
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/session"
)

// TestSteerablePrompt pins the predicate to submitInput's own dispatch rule:
// steer only what the flush path would send as a plain prompt — steering
// bypasses submitInput, so anything that must be executed or staged there
// (commands, bang exec, direct-bash shells, attachments) keeps its
// next-turn flush.
func TestSteerablePrompt(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		pasted     bool
		atts       int
		directBash bool
		want       bool
	}{
		{name: "plain prompt", text: "fix the retry logic", want: true},
		{name: "multi-line prompt", text: "first\nsecond", want: true},
		{name: "mid-input slash stays text", text: "see /theme dark output", want: true},
		{name: "known slash command", text: "/clear", want: false},
		{name: "slash with args", text: "/theme dark", want: false},
		{name: "config-init alias", text: "/config-init", want: false},
		{name: "unknown slash token is plain text", text: "/definitely-not-a-command x", want: true},
		{name: "bang direct exec", text: "!ls -la", want: false},
		{name: "staged attachments wait for the flush", text: "look at these", atts: 2, want: false},
		{name: "pasted slash is inert prompt text", text: "/clear", pasted: true, want: true},
		{name: "whitespace only", text: "   \n  ", want: false},
		{name: "direct-bash shell waits for the confirmation flow", text: "ls -la /tmp", directBash: true, want: false},
		{name: "shell-looking text with direct-bash off steers", text: "ls -la /tmp", want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Config{DirectBash: c.directBash}
			if got := steerablePrompt(c.text, c.pasted, c.atts, cfg); got != c.want {
				t.Errorf("steerablePrompt(%q, pasted=%v, atts=%d, directBash=%v) = %v, want %v",
					c.text, c.pasted, c.atts, c.directBash, got, c.want)
			}
		})
	}
}

// TestPopSteer_PromptPrefixStopsAtBlockedEntry: the drain takes the
// steerable prefix of the prompt queue and stops at the first flush-path
// entry — order cannot skip, so nothing behind it moves either, and remote
// input waits while the prompt queue still holds anything.
func TestPopSteer_PromptPrefixStopsAtBlockedEntry(t *testing.T) {
	st := &interactiveState{cfg: config.Config{}}
	st.queuedPrompts = []queuedPrompt{
		{text: "first plain"},
		{text: "/clear"},
		{text: "behind the command"},
	}
	st.pendingRemoteInputs = []string{"from the train"}

	items := st.popSteer()
	if len(items) != 1 || items[0].text != "first plain" {
		t.Fatalf("expected exactly the steerable prefix, got %#v", items)
	}
	if !strings.Contains(items[0].label, "[queued]") || items[0].recordHistory {
		t.Errorf("queued item must echo as [queued] without a history re-append: %#v", items[0])
	}
	if len(st.queuedPrompts) != 2 || st.queuedPrompts[0].text != "/clear" {
		t.Errorf("queue must keep the blocked entry and what follows: %#v", st.queuedPrompts)
	}
	if len(st.pendingRemoteInputs) != 1 {
		t.Errorf("remote input must wait behind the blocked prompt queue: %#v", st.pendingRemoteInputs)
	}
}

// TestPopSteer_RemoteDeliversWhenQueueEmpty: once the prompt queue is empty,
// steerable remote (Telegram) input drains with its [telegram] label and
// records history at delivery (it never had a queue moment to record at).
// A slash-command remote entry stops its own queue like everywhere else.
func TestPopSteer_RemoteDeliversWhenQueueEmpty(t *testing.T) {
	st := &interactiveState{cfg: config.Config{}}
	st.pendingRemoteInputs = []string{"hello from the train", "/theme dark", "later"}

	items := st.popSteer()
	if len(items) != 1 || items[0].text != "hello from the train" {
		t.Fatalf("expected one steerable remote input, got %#v", items)
	}
	if !strings.Contains(items[0].label, "[telegram]") || !items[0].recordHistory {
		t.Errorf("remote item must echo as [telegram] and record history: %#v", items[0])
	}
	if len(st.pendingRemoteInputs) != 2 || st.pendingRemoteInputs[0] != "/theme dark" {
		t.Errorf("remote queue must stop at the command: %#v", st.pendingRemoteInputs)
	}
	// And an empty drain is empty.
	if more := st.popSteer(); len(more) != 0 {
		t.Errorf("second pop over a blocked queue must deliver nothing, got %#v", more)
	}
}

// TestSteerDeliveredMsg_EchoesAndRecords: the Update half — a drained item
// renders like its flush would (label + tokens), and only remote input
// appends the input histories (queue-time recording owns the rest, #368).
func TestSteerDeliveredMsg_EchoesAndRecords(t *testing.T) {
	m := busyQueueModel(t, true)
	m.loopDetector = nil // delivery bookkeeping without a detector must not panic

	next, _ := m.Update(steerDeliveredMsg{item: steerItem{
		text:  "steer this into the turn",
		label: dim("[queued]") + " ",
	}})
	out := next.(model)
	if !strings.Contains(out.transcript.String(), "steer this into the turn") {
		t.Errorf("missing the delivery echo:\n%s", out.transcript.String())
	}
	if !strings.Contains(out.transcript.String(), "[queued]") {
		t.Errorf("echo must carry the queue-of-origin label:\n%s", out.transcript.String())
	}
	if len(out.sessionHistory) != 0 || len(out.globalHistory) != 0 {
		t.Errorf("queued items were recorded at queue time (#368) — delivery must not append: %#v", out.sessionHistory)
	}

	next2, _ := out.Update(steerDeliveredMsg{item: steerItem{
		text:          "from the telegram",
		label:         dim("[telegram]") + " ",
		recordHistory: true,
	}})
	out2 := next2.(model)
	if !strings.Contains(out2.transcript.String(), "from the telegram") {
		t.Errorf("missing the remote delivery echo:\n%s", out2.transcript.String())
	}
	if got := out2.sessionHistory; len(got) != 1 || got[0] != "from the telegram" {
		t.Errorf("remote delivery must record the input history: %#v", got)
	}
	if got := out2.globalHistory; len(got) != 1 || got[0] != "from the telegram" {
		t.Errorf("remote delivery must record the global history: %#v", got)
	}
}

// TestSteerQueuePopAppendRace pins the lock discipline the feature's
// cross-goroutine contract rests on: the agent goroutine pops (popSteer)
// while the UI goroutine queues, drops and reviews — run under -race.
func TestSteerQueuePopAppendRace(t *testing.T) {
	st := &interactiveState{cfg: config.Config{}}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = st.popSteer()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			st.steerMu.Lock()
			st.queuedPrompts = append(st.queuedPrompts, queuedPrompt{text: "typed while busy"})
			st.steerMu.Unlock()

			st.steerMu.Lock()
			if len(st.queuedPrompts) > 0 {
				st.queuedPrompts = st.queuedPrompts[1:] // the undo/drop shape
			}
			st.steerMu.Unlock()

			st.steerMu.Lock()
			st.pendingRemoteInputs = append(st.pendingRemoteInputs, "from telegram")
			st.steerMu.Unlock()
		}
	}()
	wg.Wait()
}

// TestSteerDrain_QueuesSessionRecord: the wiring drain hands every delivered
// item to Update (transcript + input histories) AND queues its session-store
// record — delivered steering must reach the conversation record like any
// other user message (ADR-0108 persistence), with nothing left behind for a
// later turn to re-append.
func TestSteerDrain_QueuesSessionRecord(t *testing.T) {
	st := &interactiveState{cfg: config.Config{}, sess: &session.Session{}}
	st.queuedPrompts = []queuedPrompt{{text: "steer this"}}

	var echoed []steerDeliveredMsg
	send := func(m tea.Msg) {
		if d, ok := m.(steerDeliveredMsg); ok {
			echoed = append(echoed, d)
		}
	}
	drain := newSteerDrain(st, send)

	texts := drain()
	if len(texts) != 1 || texts[0] != "steer this" {
		t.Fatalf("drain = %#v, want the queued text once", texts)
	}
	if len(echoed) != 1 {
		t.Errorf("each delivery must reach Update for the transcript echo, got %d", len(echoed))
	}
	if got := st.sess.SteeredPending; len(got) != 1 || got[0] != "steer this" {
		t.Errorf("delivered input must queue its session-history record, got %#v", got)
	}

	// Nothing left to drain: empty pop, pending untouched (recordUserTurn
	// owns the flush, not the next drain).
	if more := drain(); len(more) != 0 {
		t.Errorf("second drain over an empty queue = %#v, want none", more)
	}
	if len(st.sess.SteeredPending) != 1 {
		t.Errorf("pending must survive until the turn's record, got %#v", st.sess.SteeredPending)
	}
}
