package main

import (
	"context"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
)

// busyQueueModel returns a busy model with the queue knob set as asked.
func busyQueueModel(t *testing.T, queueWhileBusy bool) model {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	knob := queueWhileBusy
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}, cfg: config.Config{QueueWhileBusy: &knob}}
	mgr := local.NewManager(context.Background(), 3)
	escAgent := local.New("http://127.0.0.1:1", "esc-model")
	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr, escalationLocal: escAgent}, nil)
	m.busy = true
	return m
}

// TestQueuePrompt_CapturesAndClears: queueing takes the text, the paste
// provenance and the staged attachments, clears the input, and leaves a
// receipt line naming the /queue review surface.
func TestQueuePrompt_CapturesAndClears(t *testing.T) {
	m := busyQueueModel(t, true)
	m.leadingPasted = true
	m.pendingAttachments = []PendingAttachment{{Name: "img.png", MIMEType: "image/png"}}
	m.ta.SetValue("fix the retry logic")

	m.queuePrompt(strings.TrimSpace(m.ta.Value()))
	m.ta.Reset()

	if len(m.st.queuedPrompts) != 1 {
		t.Fatalf("expected 1 queued prompt, got %d", len(m.st.queuedPrompts))
	}
	q := m.st.queuedPrompts[0]
	if q.text != "fix the retry logic" {
		t.Errorf("queued text = %q", q.text)
	}
	if !q.leadingPasted {
		t.Errorf("paste provenance not captured")
	}
	if len(q.attachments) != 1 {
		t.Errorf("attachments not captured: %#v", q.attachments)
	}
	if len(m.pendingAttachments) != 0 {
		t.Errorf("staged attachments not consumed at queue time")
	}
}

// TestQueuePrompt_ReceiptLine: the receipt proves the input was not dropped
// and shows the preview, the line count and the attachment count.
func TestQueuePrompt_ReceiptLine(t *testing.T) {
	m := busyQueueModel(t, true)
	m.queuePrompt("first line\nsecond line")

	if !strings.Contains(m.transcript.String(), "queued for next turn") {
		t.Errorf("missing receipt line:\n%s", m.transcript.String())
	}
	if !strings.Contains(m.transcript.String(), "/queue to review") {
		t.Errorf("receipt does not name /queue:\n%s", m.transcript.String())
	}
	if !strings.Contains(m.transcript.String(), "⋯ 2 lines") {
		t.Errorf("receipt missing the line-count variant:\n%s", m.transcript.String())
	}
}

// TestQueuePrompt_Cap: the 21st prompt is refused with a hint and the typed
// text stays in the input box.
func TestQueuePrompt_Cap(t *testing.T) {
	m := busyQueueModel(t, true)
	for i := 0; i < queuePromptCap; i++ {
		m.queuePrompt("filler")
	}
	m.ta.SetValue("one too many")
	m.queuePrompt(strings.TrimSpace(m.ta.Value()))

	if len(m.st.queuedPrompts) != queuePromptCap {
		t.Fatalf("queue grew past the cap: %d", len(m.st.queuedPrompts))
	}
	if !strings.Contains(m.busyHint, "queue is full") {
		t.Errorf("expected the full-queue hint, got %q", m.busyHint)
	}
	if m.ta.Value() != "one too many" {
		t.Errorf("refused prompt lost its text: %q", m.ta.Value())
	}
}

// TestDrainQueuedPrompt_FIFOOnePerTurn: two queued prompts flush in order,
// one per drain (i.e. one per turn).
func TestDrainQueuedPrompt_FIFOOnePerTurn(t *testing.T) {
	m := busyQueueModel(t, true)
	m.queuePrompt("first")
	m.queuePrompt("second")

	next, _, ok := m.drainQueuedPrompt()
	if !ok {
		t.Fatal("expected a drain for the first queued prompt")
	}
	out := next.(model)
	if len(out.st.queuedPrompts) != 1 || out.st.queuedPrompts[0].text != "second" {
		t.Fatalf("expected FIFO order, queue now %#v", out.st.queuedPrompts)
	}

	next2, _, ok2 := out.drainQueuedPrompt()
	if !ok2 {
		t.Fatal("expected a drain for the second queued prompt")
	}
	out2 := next2.(model)
	if len(out2.st.queuedPrompts) != 0 {
		t.Fatalf("expected an empty queue after two drains, got %#v", out2.st.queuedPrompts)
	}
	if _, _, ok3 := out2.drainQueuedPrompt(); ok3 {
		t.Error("drain must no-op on an empty queue")
	}
}

// TestDrainQueuedPrompt_ClearsFollowupFlagsAndReplaysProvenance: the flushed
// turn is the next turn — follow-up flags are cleared so finished job
// results ride along inside it, and the entry's paste provenance and
// attachments are replayed.
func TestDrainQueuedPrompt_ClearsFollowupFlagsAndReplaysProvenance(t *testing.T) {
	m := busyQueueModel(t, true)
	m.leadingPasted = true
	m.pendingAttachments = []PendingAttachment{{Name: "notes.md", MIMEType: "text/markdown"}}
	m.queuePrompt("continue here")
	m.pendingBackgroundFollowup = true
	m.pendingUserBackgroundFollowup = true

	next, cmd, ok := m.drainQueuedPrompt()
	if !ok {
		t.Fatal("expected a drain")
	}
	out := next.(model)
	if out.pendingBackgroundFollowup || out.pendingUserBackgroundFollowup {
		t.Errorf("follow-up flags not cleared at flush: %v %v",
			out.pendingBackgroundFollowup, out.pendingUserBackgroundFollowup)
	}
	// Provenance and attachments are consumed *into* the flushed turn
	// (submitInput snapshots leadingPasted; the dispatch takes the staged
	// attachments) — what the entry owned is pinned by
	// TestQueuePrompt_CapturesAndClears.
	if out.leadingPasted {
		t.Errorf("paste provenance left set after the flush consumed it")
	}
	if len(out.pendingAttachments) != 0 {
		t.Errorf("attachments left staged after the flush consumed them: %#v", out.pendingAttachments)
	}
	if cmd == nil {
		t.Error("expected the flush to start a turn")
	}
}

// TestQueuePrompt_Knob: "queue_while_busy": false disables the queue path
// (handleBusyKey keeps the hint — pinned in completion_while_busy_test),
// and the knob is read live from the config.
func TestQueuePrompt_Knob(t *testing.T) {
	m := busyQueueModel(t, false)
	if m.st.cfg.QueueWhileBusyEnabled() {
		t.Error("expected the knob to read false")
	}
	m2 := busyQueueModel(t, true)
	if !m2.st.cfg.QueueWhileBusyEnabled() {
		t.Error("expected the knob to read true")
	}
	var unset config.Config
	if !unset.QueueWhileBusyEnabled() {
		t.Error("unset knob must default to true")
	}
}

// TestQueueDroppedOnSessionSwitch: queued prompts do not leak across a
// session switch (like staged attachments) and say so.
func TestQueueDroppedOnSessionSwitch(t *testing.T) {
	m := busyQueueModel(t, true)
	m.queuePrompt("for this conversation only")
	m.queuePrompt("also for this one")

	next, _ := m.handleSlashInput(cmdNew, "")
	out := next.(model)
	if len(out.st.queuedPrompts) != 0 {
		t.Errorf("queued prompts survived a session switch: %#v", out.st.queuedPrompts)
	}
	if !strings.Contains(out.transcript.String(), "dropped 2 queued prompt(s)") {
		t.Errorf("missing the drop notice:\n%s", out.transcript.String())
	}
}

// TestQueuePrompt_RecordsHistoryAtQueueTime (issue #368): queueing enters
// both histories immediately — the typed text must be retrievable (Up /
// Ctrl+R) even if the queued request is cancelled before it ever runs.
func TestQueuePrompt_RecordsHistoryAtQueueTime(t *testing.T) {
	m := busyQueueModel(t, true)
	m.queuePrompt("dig into the physics module")

	if got := m.sessionHistory; len(got) != 1 || got[0] != "dig into the physics module" {
		t.Errorf("session history after queue = %#v, want the queued text", got)
	}
	if got := m.globalHistory; len(got) != 1 || got[0] != "dig into the physics module" {
		t.Errorf("global history after queue = %#v, want the queued text", got)
	}
	// Consecutive duplicates still dedupe, like submitInput's own append.
	m.queuePrompt("dig into the physics module")
	if len(m.sessionHistory) != 1 {
		t.Errorf("identical back-to-back queues = %#v, want one entry", m.sessionHistory)
	}
}

// TestQueuePrompt_CancelKeepsHistory (issue #368): the cancel paths drop the
// queue but must leave the text in history — the exact loss the bug reported.
func TestQueuePrompt_CancelKeepsHistory(t *testing.T) {
	m := busyQueueModel(t, true)
	m.queuePrompt("recover me from history")
	m.queuePrompt("second queued text")

	// Path 1: Ctrl+C stage 0 (undo the most recent queueing).
	next, _ := m.handleCtrlC()
	out := next.(model)
	if len(out.st.queuedPrompts) != 1 {
		t.Fatalf("press must drop the last entry, queue = %#v", out.st.queuedPrompts)
	}
	// Path 2: /queue clear (the remaining entry).
	out = out.handleQueueCmd("clear")
	if len(out.st.queuedPrompts) != 0 {
		t.Fatalf("clear must empty the queue, got %#v", out.st.queuedPrompts)
	}
	for _, want := range []string{"recover me from history", "second queued text"} {
		found := false
		for _, h := range out.sessionHistory {
			if h == want {
				found = true
			}
		}
		if !found {
			t.Errorf("cancelled queued text %q missing from session history: %#v", want, out.sessionHistory)
		}
	}
}

// TestDrainQueuedPrompt_NoDoubleHistoryAppend (issue #368): the flush must
// not append what queue time already recorded — consecutive-dedup alone is
// not enough (queue A, queue B, flush A would re-append A after B).
func TestDrainQueuedPrompt_NoDoubleHistoryAppend(t *testing.T) {
	m := busyQueueModel(t, true)
	m.queuePrompt("first")
	m.queuePrompt("second")

	next, _, ok := m.drainQueuedPrompt()
	if !ok {
		t.Fatal("expected a drain")
	}
	out := next.(model)
	if out.queuedHistoryRecorded {
		t.Error("queued flush marker must be consumed by submitInput")
	}
	next2, _, ok2 := out.drainQueuedPrompt()
	if !ok2 {
		t.Fatal("expected a second drain")
	}
	out2 := next2.(model)

	want := []string{"first", "second"}
	if len(out2.sessionHistory) != len(want) || out2.sessionHistory[0] != want[0] || out2.sessionHistory[1] != want[1] {
		t.Errorf("session history after both flushes = %#v, want exactly %#v (once each, in order)", out2.sessionHistory, want)
	}
	if len(out2.globalHistory) != len(want) {
		t.Errorf("global history after both flushes = %#v, want exactly %#v", out2.globalHistory, want)
	}
}

// TestSubmitInput_ConsumesQueuedFlushMarkerOnEmpty: the marker is consumed
// at submitInput's top even when the input is empty, so it can never leak
// forward and suppress a later, unrelated history append.
func TestSubmitInput_ConsumesQueuedFlushMarkerOnEmpty(t *testing.T) {
	m := busyQueueModel(t, true)
	m.queuedHistoryRecorded = true

	next, _ := m.submitInput("", "")
	out := next.(model)
	if out.queuedHistoryRecorded {
		t.Error("marker survived an empty submit — a later append would be wrongly skipped")
	}
	// And a subsequent normal submit still appends.
	next2, _ := out.submitInput("typed after the flush", "")
	out2 := next2.(model)
	if got := out2.sessionHistory; len(got) != 1 || got[0] != "typed after the flush" {
		t.Errorf("normal submit history = %#v, want one entry", got)
	}
}

// TestHandleQueueCmd_Review: /queue list|show|drop|clear round-trips the
// queue, and /queue is busy-safe.
func TestHandleQueueCmd_Review(t *testing.T) {
	m := busyQueueModel(t, true)
	if !busySafeCommands[cmdQueue] {
		t.Fatal("/queue must be busy-safe")
	}
	m.queuePrompt("alpha")
	m.queuePrompt("beta")

	m = m.handleQueueCmd("")
	if !strings.Contains(m.transcript.String(), "2 queued prompt(s)") {
		t.Errorf("list does not show the count:\n%s", m.transcript.String())
	}

	m = m.handleQueueCmd("show 2")
	if !strings.Contains(m.transcript.String(), "beta") {
		t.Errorf("show does not print the entry:\n%s", m.transcript.String())
	}

	m = m.handleQueueCmd("drop last")
	if len(m.st.queuedPrompts) != 1 || m.st.queuedPrompts[0].text != "alpha" {
		t.Errorf("drop last removed the wrong entry: %#v", m.st.queuedPrompts)
	}

	m = m.handleQueueCmd("clear")
	if len(m.st.queuedPrompts) != 0 {
		t.Errorf("clear left entries behind: %#v", m.st.queuedPrompts)
	}
}

// TestQueuePreview: the receipt preview is the first line clipped to 60
// columns.
func TestQueuePreview(t *testing.T) {
	if got := queuePreview("short"); got != "short" {
		t.Errorf("short preview mangled: %q", got)
	}
	long := strings.Repeat("x", 80)
	got := queuePreview(long)
	if len([]rune(got)) != 60 {
		t.Errorf("preview not clipped to 60 columns: %d", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clipped preview must end with …: %q", got)
	}
	if got := queuePreview("one\ntwo"); got != "one" {
		t.Errorf("multi-line preview must be the first line: %q", got)
	}
}

// TestCtrlC_QueueUndoStack: the enriched Ctrl+C chain — drop the last
// queued prompt first (one press per entry), then the pre-existing stages
// (clear input → interrupt the turn).
func TestCtrlC_QueueUndoStack(t *testing.T) {
	m := busyQueueModel(t, true)
	m.queuePrompt("first queued")
	m.queuePrompt("second queued")
	m.ta.SetValue("draft in progress")
	interrupts := 0
	m.cancelTurn = func() { interrupts++ }

	// Press 1: drops the *last* entry (the queueing's undo), leaves input.
	next, _ := m.handleCtrlC()
	out := next.(model)
	if len(out.st.queuedPrompts) != 1 || out.st.queuedPrompts[0].text != "first queued" {
		t.Fatalf("press 1 must drop the last entry only, got %#v", out.st.queuedPrompts)
	}
	if out.ta.Value() != "draft in progress" {
		t.Errorf("press 1 touched the input: %q", out.ta.Value())
	}
	if !strings.Contains(out.transcript.String(), "removed the last queued prompt (1 left)") {
		t.Errorf("press 1 missing the undo notice:\n%s", out.transcript.String())
	}

	// Press 2: drops the remaining entry.
	next, _ = out.handleCtrlC()
	out = next.(model)
	if len(out.st.queuedPrompts) != 0 {
		t.Fatalf("press 2 must empty the queue, got %#v", out.st.queuedPrompts)
	}

	// Press 3: clears the input (the pre-existing clear stage).
	next, _ = out.handleCtrlC()
	out = next.(model)
	if out.ta.Value() != "" {
		t.Errorf("press 3 must clear the input, got %q", out.ta.Value())
	}
	if interrupts != 0 {
		t.Errorf("press 3 must not interrupt, got %d interrupts", interrupts)
	}

	// Press 4: interrupts the running turn.
	next, _ = out.handleCtrlC()
	out = next.(model)
	if interrupts != 1 || !out.interrupted {
		t.Errorf("press 4 must interrupt the turn: interrupts=%d interrupted=%v", interrupts, out.interrupted)
	}
}
