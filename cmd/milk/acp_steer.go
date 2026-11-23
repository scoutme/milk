package main

// ACP parity for mid-turn steering (issue #441, ADR-0108): a session/prompt
// arriving while a turn runs is queued — visible to the running turn's
// steerDrain at its next model-request boundary — instead of only
// serializing behind turnMu. The waiter still blocks on turnMu exactly as
// before, which is what keeps this a small delta:
//
//   - delivered (drained mid-turn): the drain echoed the message to the
//     client under its own messageId; once the consuming turn releases
//     turnMu the waiter returns that id — one echo, no second turn.
//   - not delivered (the turn ended first, or the entry is not steerable):
//     the waiter dequeues itself and runs its own turn through runTurn,
//     exactly as the blocking path did.
//
// Order rules mirror the TUI's popSteer (steer.go): the steerable prefix of
// the prompt queue drains first and stops at the first entry the flush path
// must own (slash command, media, DirectBash shell text), so nothing jumps
// ahead of it; remote-oversight inputs drain only once the prompt queue is
// empty; and a pending setup wizard refuses delivery outright — a wizard
// answer must reach runTurn's consumption, never the model.
//
// Lock order: turnMu → steerMu → as.mu (never reversed).

import (
	"github.com/scoutme/milk/internal/transport/acp"
)

// acpQueuedPrompt is one session/prompt captured while a turn runs.
type acpQueuedPrompt struct {
	text  string
	media *acpPromptMedia
	// delivered is set by popSteer when the running turn's boundary took
	// the entry; echoID is the messageId that delivery echoed it under —
	// the PromptResponse the waiter returns.
	delivered bool
	echoID    acp.MessageID
}

// enqueueSteer appends one prompt to the steering queue. Returns nil at
// queuePromptCap — the caller then falls back to the plain
// serialize-behind-turnMu path (never drops input; the TUI refuses with a
// hint because it has a text box to keep the text in, ACP does not).
func (as *acpSession) enqueueSteer(text string, media *acpPromptMedia) *acpQueuedPrompt {
	as.steerMu.Lock()
	defer as.steerMu.Unlock()
	if len(as.steerQueue) >= queuePromptCap {
		return nil
	}
	e := &acpQueuedPrompt{text: text, media: media}
	as.steerQueue = append(as.steerQueue, e)
	return e
}

// dropSteer removes the waiter's own undelivered entry after it acquired
// turnMu (it is about to run as the next turn). A delivered entry is
// already gone; dropping it again is a no-op.
func (as *acpSession) dropSteer(e *acpQueuedPrompt) {
	as.steerMu.Lock()
	defer as.steerMu.Unlock()
	for i, q := range as.steerQueue {
		if q == e {
			as.steerQueue = append(as.steerQueue[:i], as.steerQueue[i+1:]...)
			return
		}
	}
}

// steerResult is the waiter's read of its entry: the drain wrote
// delivered/echoID on the turn goroutine, and the steerMu critical section
// hands those writes across to this reader.
func (as *acpSession) steerResult(e *acpQueuedPrompt) (acp.MessageID, bool) {
	as.steerMu.Lock()
	defer as.steerMu.Unlock()
	return e.echoID, e.delivered
}

// mediaAttachments maps ACP prompt media onto steerablePrompt's attachment
// count: any non-text block (image, audio, open-set) makes the prompt
// non-steerable — image parts ride the turn's context at turn start and
// notes only surface through runTurn, so neither can be injected at a
// model-request boundary.
func mediaAttachments(m *acpPromptMedia) int {
	if m == nil {
		return 0
	}
	return 1
}

// popSteer drains the running turn's share of the queue (issue #441): the
// steerable prompt prefix first — stopping at the first entry the flush
// path must own — then remote-oversight inputs once the prompt queue is
// empty. Each delivered prompt is echoed to the client as the user message
// it is (the waiter returns this id and runs no second turn). Runs on the
// turn's own goroutine with turnMu held, which is also what makes the
// wizard-state reads safe: every writer of pendingInit/pendingTelegram
// runs under turnMu.
func (as *acpSession) popSteer() []string {
	// A pending setup wizard owns the next prompt it sees (runTurn
	// consumes it as its answer): refuse delivery outright so an answer
	// queued behind a background/remote turn reaches the wizard instead of
	// the model. The queue survives the refusal — the waiter's own turn
	// runs it after this one ends.
	if as.pendingInit != nil || as.pendingTelegram != nil {
		return nil
	}

	as.steerMu.Lock()
	var batch []*acpQueuedPrompt
	for _, e := range as.steerQueue {
		if !steerablePrompt(e.text, false, mediaAttachments(e.media), as.cfg) {
			break // order cannot skip: this entry keeps the flush path
		}
		e.delivered = true
		e.echoID = as.liveID("msg")
		batch = append(batch, e)
	}
	if len(batch) > 0 {
		as.steerQueue = as.steerQueue[len(batch):]
	}
	promptQueueEmpty := len(as.steerQueue) == 0

	// Remote-oversight inputs (TUI parity): only while the prompt queue is
	// fully drained — the flush chain runs queued prompts before remote
	// inputs, so a remote entry must never jump a waiting prompt. steerMu
	// nests over as.mu here; nothing takes them in the reverse order.
	var remote []string
	if promptQueueEmpty && len(as.pendingRemoteInputs) > 0 {
		as.mu.Lock()
		m := 0
		for _, text := range as.pendingRemoteInputs {
			if !steerablePrompt(text, false, 0, as.cfg) {
				break
			}
			m++
		}
		if m > 0 {
			remote = append(remote, as.pendingRemoteInputs[:m]...)
			as.pendingRemoteInputs = append([]string(nil), as.pendingRemoteInputs[m:]...)
		}
		as.mu.Unlock()
	}
	as.steerMu.Unlock()

	texts := make([]string, 0, len(batch)+len(remote))
	for _, e := range batch {
		as.echoUserPrompt(e.echoID, e.text, nil)
		texts = append(texts, e.text)
	}
	for _, text := range remote {
		as.echoUserPrompt(as.liveID("msg"), text, nil)
		texts = append(texts, text)
	}
	return texts
}
