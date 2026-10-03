package acp

import (
	"context"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/livebuf"
)

// terminal_update + terminal_output_chunk over internal/livebuf (terminal.go)
// — the ACP mirror of cmd/milk/attach.go's live-attach views.

func TestTerminalStreamLifecycle(t *testing.T) {
	buf := livebuf.New(0)
	ts := NewAttachTerminal("term-1", AttachBackground, "bg-1", "research (running)", buf)

	start := ts.Start()
	if start.SessionUpdate != "terminal_update" || start.TerminalID != "term-1" {
		t.Fatalf("start = %+v", start)
	}
	// The attach view's identity rides the command string (attach.go header).
	if start.Command != "background: research (running)" {
		t.Fatalf("command = %q", start.Command)
	}
	body := jsonBody(t, start)
	if body["sessionUpdate"] != "terminal_update" {
		t.Fatalf("sessionUpdate = %v", body["sessionUpdate"])
	}

	buf.Append([]byte("hello"))
	updates := ts.Poll()
	if len(updates) != 1 {
		t.Fatalf("poll 1 = %+v", updates)
	}
	chunk, ok := updates[0].(TerminalOutputChunk)
	if !ok {
		t.Fatalf("poll 1 type = %T", updates[0])
	}
	if chunk.SessionUpdate != "terminal_output_chunk" || chunk.Data != "hello" || chunk.TerminalID != "term-1" {
		t.Fatalf("chunk = %+v", chunk)
	}

	buf.Append([]byte(" world"))
	updates = ts.Poll()
	if len(updates) != 1 || updates[0].(TerminalOutputChunk).Data != " world" {
		t.Fatalf("poll 2 = %+v", updates)
	}
	if got := ts.Poll(); got != nil {
		t.Fatalf("poll 3 (no new content) = %+v", got)
	}

	fin := ts.Finish(ExitStatus(0))
	if fin.ExitStatus == nil || *fin.ExitStatus.ExitCode != 0 {
		t.Fatalf("finish = %+v", fin)
	}
	if fin.Output == nil || fin.Output.Data != "hello world" {
		t.Fatalf("finish output = %+v", fin.Output)
	}
	if !ts.Done() {
		t.Fatal("Done() = false after Finish")
	}
}

func TestTerminalStreamReanchorsOnTrim(t *testing.T) {
	// livebuf drops the oldest bytes over its cap; the stream re-anchors with
	// a full-output terminal_update instead of a bogus append chunk.
	buf := livebuf.New(8)
	ts := NewAttachTerminal("term-2", AttachWorkflow, "", "dev", buf)

	buf.Append([]byte("abc"))
	if got := ts.Poll(); len(got) != 1 {
		t.Fatalf("poll 1 = %+v", got)
	}
	buf.Append([]byte("defghijkl")) // 12 retained-8: trims to "efghijkl"
	updates := ts.Poll()
	if len(updates) != 1 {
		t.Fatalf("poll 2 = %+v", updates)
	}
	upd, ok := updates[0].(TerminalUpdate)
	if !ok {
		t.Fatalf("re-anchor type = %T", updates[0])
	}
	if upd.Output == nil || upd.Output.Data != "efghijkl" {
		t.Fatalf("re-anchor output = %+v", upd.Output)
	}
	// After re-anchoring, appends stream as chunks again.
	buf.Append([]byte("!"))
	updates = ts.Poll()
	if len(updates) != 1 || updates[0].(TerminalOutputChunk).Data != "!" {
		t.Fatalf("post re-anchor poll = %+v", updates)
	}
}

func TestTerminalStreamSnapshotAndRun(t *testing.T) {
	buf := livebuf.New(0)
	ts := NewAttachTerminal("term-3", AttachBackground, "bg-2", "job", buf)
	buf.Append([]byte("full content"))
	snap := ts.Snapshot()
	if snap.Output == nil || snap.Output.Data != "full content" {
		t.Fatalf("snapshot = %+v", snap)
	}

	// Run's poll loop emits appended content until the context is cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got []SessionUpdate
	done := make(chan struct{})
	go func() {
		defer close(done)
		ts.Run(ctx, 5*time.Millisecond, func(u SessionUpdate) {
			got = append(got, u)
			cancel() // one chunk is enough
		})
	}()
	buf.Append([]byte(" streamed"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not emit")
	}
	if len(got) != 1 || got[0].(TerminalOutputChunk).Data != " streamed" {
		t.Fatalf("run updates = %+v", got)
	}
}

func TestTerminalExitStatusShapes(t *testing.T) {
	num := jsonBody(t, ExitStatus(3))
	if num["exitCode"] != 3.0 {
		t.Fatalf("exitStatus = %v", num)
	}
	sig := jsonBody(t, SignalStatus("SIGKILL"))
	if sig["signal"] != "SIGKILL" {
		t.Fatalf("signal status = %v", sig)
	}
}
