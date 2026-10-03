package acp

import (
	"context"
	"strings"
	"time"

	"github.com/scoutme/milk/internal/livebuf"
)

// terminal_update + terminal_output_chunk — ACP v2's agent-owned terminal
// model (v2 replaced v1's client-owned terminal/create|output|kill surface).
// milk already runs exactly this shape: process output streams into
// internal/livebuf buffers that cmd/milk/attach.go attaches to on demand
// (ADR-0047 live-attach views over background jobs and workflows). Each
// attach view is mirrored here as one agent-owned terminal, so an editor's
// terminal pane shows the same stream the TUI's attach view shows.

// TerminalOutput is the snapshot of a terminal's output carried on
// terminal_update (TerminalOutput in the upstream schema).
type TerminalOutput struct {
	Data string         `json:"data"`
	Meta map[string]any `json:"_meta,omitempty"`
}

// TerminalExitStatus reports how a terminal exited (exitCode and signal are
// each nullable — exactly one is expected).
type TerminalExitStatus struct {
	ExitCode *int           `json:"exitCode"`
	Signal   *string        `json:"signal,omitempty"`
	Meta     map[string]any `json:"_meta,omitempty"`
}

// TerminalUpdate is the terminal_update session update: created-or-updated
// terminal state (command, cwd, full output snapshot, exit status).
type TerminalUpdate struct {
	SessionUpdate string              `json:"sessionUpdate"` // "terminal_update"
	TerminalID    TerminalID          `json:"terminalId"`
	Command       string              `json:"command,omitempty"`
	Cwd           string              `json:"cwd,omitempty"`
	Output        *TerminalOutput     `json:"output,omitempty"`
	ExitStatus    *TerminalExitStatus `json:"exitStatus,omitempty"`
	Meta          map[string]any      `json:"_meta,omitempty"`
}

func (TerminalUpdate) isSessionUpdate() {}

// TerminalOutputChunk is the terminal_output_chunk session update: bytes
// appended to a terminal's output since the previous update.
type TerminalOutputChunk struct {
	SessionUpdate string         `json:"sessionUpdate"` // "terminal_output_chunk"
	TerminalID    TerminalID     `json:"terminalId"`
	Data          string         `json:"data"`
	Meta          map[string]any `json:"_meta,omitempty"`
}

func (TerminalOutputChunk) isSessionUpdate() {}

// ExitStatus builds a numeric exit status (int pointer semantics of the
// schema preserved: nil field marshals as null).
func ExitStatus(code int) *TerminalExitStatus {
	return &TerminalExitStatus{ExitCode: &code}
}

// SignalStatus builds a signal-based exit status.
func SignalStatus(sig string) *TerminalExitStatus {
	return &TerminalExitStatus{Signal: &sig}
}

// AttachKind identifies what a livebuf-backed terminal is mirroring — the
// same distinction cmd/milk/attach.go's attachKind draws (its String()
// values are reused verbatim so the two surfaces name the same views).
type AttachKind string

const (
	AttachBackground AttachKind = "background" // a background-agent job's Live buffer (ADR-0043/0047)
	AttachWorkflow   AttachKind = "workflow"   // a workflow run's Live buffer (ADR-0047)
)

// DefaultTerminalPollInterval mirrors cmd/milk/attach.go's attachPollInterval
// — how often a livebuf-backed stream is checked for new content. A job's
// Live buffer is written directly by its own goroutine with no accompanying
// message, so the stream polls exactly like the attach view does.
const DefaultTerminalPollInterval = 500 * time.Millisecond

// BufCursor tracks how much of a livebuf.Buffer has been streamed so
// consumers emit only newly appended content. livebuf drops the oldest bytes
// once over its cap, so a cursor can fall behind the retained window; Next
// reports that case as reset and returns the complete retained snapshot for
// the caller to re-anchor with.
type BufCursor struct {
	sent string
}

// Next returns the content appended since the previous call. reset reports
// that the buffer trimmed content the cursor had not yet seen (delta is then
// the complete retained snapshot and must be delivered as a full-output
// update, not an append). A partial trim whose retained tail still overlaps
// the already-streamed suffix yields just the new bytes as delta: the tail
// the client already holds is not re-sent, and the best-effort byte stream
// tolerates the dropped head (livebuf is lossy by design).
func (c *BufCursor) Next(buf *livebuf.Buffer) (delta string, reset bool) {
	if buf == nil {
		return "", false
	}
	snap := buf.Snapshot()
	if strings.HasPrefix(snap, c.sent) {
		delta = snap[len(c.sent):]
		c.sent = snap
		return delta, false
	}
	// Partial trim: the longest suffix of what we already streamed that is
	// a prefix of the retained snapshot is the overlap; only the remainder
	// is new. No overlap at all => the stream must re-anchor in full.
	for k := 1; k < len(c.sent) && k <= len(snap); k++ {
		if strings.HasSuffix(c.sent, snap[:k]) {
			delta = snap[k:]
			c.sent = snap
			return delta, false
		}
	}
	c.sent = snap
	return snap, true
}

// Sent returns the content already streamed (for tests and diagnostics).
func (c *BufCursor) Sent() string { return c.sent }

// TerminalStream mirrors one livebuf-backed attach view (cmd/milk/attach.go's
// attachState: kind + jobID + label + *livebuf.Buffer) as an agent-owned ACP
// terminal.
type TerminalStream struct {
	ID     TerminalID
	Kind   AttachKind
	JobID  string // empty for workflow views (the TUI shows one active workflow at a time)
	Label  string
	Cmd    string // command string for terminal_update (the attach view's subject, e.g. "background: <label>")
	Cwd    string
	buf    *livebuf.Buffer
	cursor BufCursor
	done   bool
}

// NewAttachTerminal mirrors an attach view: same identity inputs as
// cmd/milk/attach.go's startAttach(kind, jobID, label, buf).
func NewAttachTerminal(id TerminalID, kind AttachKind, jobID, label string, buf *livebuf.Buffer) *TerminalStream {
	return &TerminalStream{
		ID:    id,
		Kind:  kind,
		JobID: jobID,
		Label: label,
		Cmd:   string(kind) + ": " + label,
		buf:   buf,
	}
}

// Start is the terminal_update announcing the terminal at attach time (empty
// output — the stream fills in via Poll).
func (t *TerminalStream) Start() TerminalUpdate {
	out := ""
	return TerminalUpdate{
		SessionUpdate: "terminal_update",
		TerminalID:    t.ID,
		Command:       t.Cmd,
		Cwd:           t.Cwd,
		Output:        &TerminalOutput{Data: out},
	}
}

// Snapshot is a terminal_update carrying the full retained output — the
// message a freshly attached client uses to catch up. It anchors the stream
// cursor, so content streamed afterwards arrives as terminal_output_chunk
// appends rather than being repeated in full.
func (t *TerminalStream) Snapshot() TerminalUpdate {
	data := ""
	if t.buf != nil {
		_, _ = t.cursor.Next(t.buf) // anchor: the snapshot *is* what the client now holds
		data = t.buf.Snapshot()
	}
	return TerminalUpdate{
		SessionUpdate: "terminal_update",
		TerminalID:    t.ID,
		Command:       t.Cmd,
		Cwd:           t.Cwd,
		Output:        &TerminalOutput{Data: data},
	}
}

// Poll collects the updates for content appended since the last Poll: one
// terminal_output_chunk per new span, or a single full-output terminal_update
// when the live buffer trimmed content the cursor had not seen (append-only
// chunks cannot express the drop, so the update re-anchors the stream — the
// same content replacement terminal_update's output field is for).
func (t *TerminalStream) Poll() []SessionUpdate {
	if t.buf == nil {
		return nil
	}
	delta, reset := t.cursor.Next(t.buf)
	if reset && delta != "" {
		// The buffer trimmed unseen content: chunk deltas can't express the
		// drop, so re-anchor with the complete retained snapshot (the
		// replacement semantics of terminal_update's output field).
		return []SessionUpdate{TerminalUpdate{
			SessionUpdate: "terminal_update",
			TerminalID:    t.ID,
			Command:       t.Cmd,
			Cwd:           t.Cwd,
			Output:        &TerminalOutput{Data: t.cursor.Sent()},
		}}
	}
	if delta == "" {
		return nil
	}
	return []SessionUpdate{TerminalOutputChunk{
		SessionUpdate: "terminal_output_chunk",
		TerminalID:    t.ID,
		Data:          delta,
	}}
}

// Finish is the terminal_update closing the terminal with its exit status and
// the full retained output snapshot (terminal_update.output replaces the
// client's copy — same semantics the re-anchor path uses). Idempotent after
// the first call.
func (t *TerminalStream) Finish(exit *TerminalExitStatus) TerminalUpdate {
	t.done = true
	data := ""
	if t.buf != nil {
		_, _ = t.cursor.Next(t.buf) // drain any unseen tail into the cursor
		data = t.buf.Snapshot()
	}
	return TerminalUpdate{
		SessionUpdate: "terminal_update",
		TerminalID:    t.ID,
		Command:       t.Cmd,
		Cwd:           t.Cwd,
		Output:        &TerminalOutput{Data: data},
		ExitStatus:    exit,
	}
}

// Done reports whether Finish has been called.
func (t *TerminalStream) Done() bool { return t.done }

// Run polls the live buffer at interval (default DefaultTerminalPollInterval,
// mirroring cmd/milk/attach.go's refresh tick) and hands every update to
// until ctx is done or Finish is called. This is the feed loop the serve
// layer runs per attached terminal; keeping it here means the TUI attach view
// and the ACP terminal pane share one polling contract.
func (t *TerminalStream) Run(ctx context.Context, interval time.Duration, emit func(SessionUpdate)) {
	if interval <= 0 {
		interval = DefaultTerminalPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if t.done {
				return
			}
			for _, upd := range t.Poll() {
				emit(upd)
			}
		}
	}
}
