package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
)

// toolUpdate routes one message through Update and returns the updated model
// (the Update value-receiver dance every case test needs).
func toolUpdate(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()
	next, _ := m.Update(msg)
	nm, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", next)
	}
	return nm
}

func startMsg(id, name, summary, argsJSON, diff string) toolCallStartMsg {
	return toolCallStartMsg{id: id, name: name, summary: summary,
		argsJSON: argsJSON, diff: diff, hintText: "\n\x1b[2m» " + name + "\x1b[0m\n"}
}

// TestToolBlock_LifecycleThroughUpdate pins the whole container life through
// the message handlers: open folded+running at the transcript end with the
// pretty input in the body, the file diff landing after the block (the
// position it had behind the old streamed hint), the outcome closing it, and
// an unknown id never misfiring.
func TestToolBlock_LifecycleThroughUpdate(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendTranscript("run:\n")

	m = toolUpdate(t, m, startMsg("t1", "bash", "git status", `{"command":"git status"}`, ""))
	if len(m.toolBlocks) != 1 {
		t.Fatalf("toolBlocks = %d, want 1", len(m.toolBlocks))
	}
	b := &m.toolBlocks[0]
	if b.closed || b.expanded || b.id != "t1" || b.name != "bash" {
		t.Fatalf("fresh block = %+v", b)
	}
	if m.activeToolUse != "bash" {
		t.Errorf("activeToolUse = %q, want bash", m.activeToolUse)
	}
	disp := m.transcriptDisplay()
	if !strings.Contains(disp, "▸ bash · git status — ") || !strings.Contains(disp, " · running") {
		t.Errorf("running header missing, display = %q", disp)
	}
	if strings.Contains(disp, "git status\"") {
		t.Errorf("folded body must not render, display = %q", disp)
	}
	// Expanding reveals the pretty input.
	m.toggleToolBlock(0)
	if d := m.transcriptDisplay(); !strings.Contains(d, "{\n  \"command\": \"git status\"\n}") {
		t.Errorf("expanded body must render the indented input, display = %q", d)
	}
	m.toggleToolBlock(0)

	// The outcome lands: closed, no running marker, result in the body.
	m = toolUpdate(t, m, toolCallEndMsg{id: "t1", result: "On branch main\n"})
	b = &m.toolBlocks[0]
	if !b.closed || !b.hasResult || b.result != "On branch main\n" {
		t.Fatalf("block after outcome = %+v", b)
	}
	disp = m.transcriptDisplay()
	if strings.Contains(disp, " · running") {
		t.Errorf("closed block still marked running: %q", disp)
	}

	// A second call with a diff: the block opens at the transcript end and
	// the diff appends after it — never before, never replacing the header.
	m = toolUpdate(t, m, startMsg("t2", "edit_file", "main.go", `{"path":"main.go"}`, "- old\n+ new\n"))
	disp = m.transcriptDisplay()
	hi, di := strings.Index(disp, "▸ edit_file"), strings.Index(disp, "+ new")
	if hi < 0 || di < 0 || hi > di {
		t.Errorf("header must render before the diff: header@%d diff@%d in %q", hi, di, disp)
	}
	if m.toolBlocks[1].closed {
		t.Errorf("t2 must still be running")
	}
	// An unknown id is a no-op — t2 stays running, t1 keeps its outcome.
	m = toolUpdate(t, m, toolCallEndMsg{id: "ghost", result: "x", isError: true})
	if m.toolBlocks[1].closed || m.toolBlocks[0].result != "On branch main\n" {
		t.Errorf("unknown id must not touch any block: %+v", m.toolBlocks)
	}
	// Turn end closes a pending call without an outcome (args-only truth).
	m.closeOpenToolBlock()
	if !m.toolBlocks[1].closed || m.toolBlocks[1].hasResult {
		t.Errorf("turn-end close must leave an outcome-less closed block: %+v", m.toolBlocks[1])
	}
}

// TestToolBlock_ErrorAndHeaderShapes pins the header vocabulary — running
// and error suffixes, the no-summary form — and the pattern disjointness the
// ordinal hit-tests of both families rely on.
func TestToolBlock_ErrorAndHeaderShapes(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendTranscript("x\n")
	m = toolUpdate(t, m, startMsg("e1", "edit_file", "", `not json`, ""))
	m = toolUpdate(t, m, toolCallEndMsg{id: "e1", result: "boom", isError: true})
	got := m.transcriptDisplay()
	if !strings.Contains(got, "▸ edit_file — ") || !strings.Contains(got, " · error") {
		t.Errorf("error header shape wrong: %q", got)
	}
	if strings.Contains(got, " · running") {
		t.Errorf("error suffix must not coexist with running: %q", got)
	}
	// Non-JSON input renders raw (no pretty-print crash) — expanded, since
	// the folded header is all a closed block shows by default.
	m.toggleToolBlock(0)
	if !strings.Contains(m.transcriptDisplay(), "not json") {
		t.Errorf("raw args missing: %q", m.transcriptDisplay())
	}
	m.toggleToolBlock(0)
	// Disjointness both ways: neither pattern matches the other family's
	// complete header.
	toolHeader := "▸ bash · git status — 3 lines"
	thinkHeader := "▸ thinking · 3 lines"
	if len(thinkHeaderRe.FindAllString(toolHeader, -1)) != 0 {
		t.Errorf("think pattern must not match a tool header: %q", toolHeader)
	}
	if len(toolHeaderRe.FindAllString(thinkHeader, -1)) != 0 {
		t.Errorf("tool pattern must not match a think header: %q", thinkHeader)
	}
	runningTool := "▸ bash · x — 1 line · running"
	if n := toolHeaderCount(runningTool); n != 1 {
		t.Errorf("running tool header count = %d, want 1", n)
	}
}

// TestToolBlock_ClickHitTest_InterleavedWithThink maps clicks with both
// families on screen: each ordinal space sees only its own headers, so a
// click on a tool header never toggles a thinking block and vice versa.
func TestToolBlock_ClickHitTest_InterleavedWithThink(t *testing.T) {
	m := layoutTestModel(t, 200, 50)
	m.colorizeMode = ColorizeOff
	m.appendTranscript("aaa\n")
	m.appendThinkingStreamed("thought\n")
	m.appendTranscript("mid\n") // closes the thinking block
	m = toolUpdate(t, m, startMsg("t1", "bash", "ls", `{"command":"ls"}`, ""))
	m.appendTranscript("after\n") // content after the (still-running) tool block
	m = toolUpdate(t, m, startMsg("t2", "grep", "pat", `{"pattern":"pat"}`, ""))
	m.appendTranscript("tail\n")
	m.closeOpenToolBlock()

	disp := m.transcriptDisplay()
	lines := strings.Split(disp, "\n")
	thinkLine, toolLine0, toolLine1 := -1, -1, -1
	for i, l := range lines {
		if isThinkHeaderLine(l) && thinkLine < 0 {
			thinkLine = i
		}
		if toolHeaderCount(l) > 0 {
			if toolLine0 < 0 {
				toolLine0 = i
			} else {
				toolLine1 = i
			}
		}
	}
	if thinkLine < 0 || toolLine0 < 0 || toolLine1 < 0 {
		t.Fatalf("expected 1 think + 2 tool headers, display = %q", disp)
	}
	if idx := m.thinkBlockAtLine(thinkLine); idx != 0 {
		t.Errorf("think header maps to %d, want 0", idx)
	}
	if idx := m.thinkBlockAtLine(toolLine0); idx != -1 {
		t.Errorf("tool header must be invisible to the think hit-test, got %d", idx)
	}
	if idx := m.toolBlockAtLine(toolLine0); idx != 0 {
		t.Errorf("first tool header maps to %d, want 0", idx)
	}
	if idx := m.toolBlockAtLine(toolLine1); idx != 1 {
		t.Errorf("second tool header maps to %d, want 1", idx)
	}
	if idx := m.toolBlockAtLine(thinkLine); idx != -1 {
		t.Errorf("think header must be invisible to the tool hit-test, got %d", idx)
	}
	// Click semantics: expand + pin on the tool block only.
	m.toggleToolBlock(1)
	if !m.toolBlocks[1].expanded || !m.toolBlocks[1].userSet {
		t.Errorf("toggle must expand+pin tool block 1: %+v", m.toolBlocks[1])
	}
	if m.thinkBlocks[0].expanded {
		t.Errorf("think block must be untouched: %+v", m.thinkBlocks[0])
	}
}

// TestToolBlock_ViewCallbacks_AreOutGated pins the ownership test: only the
// main turn's stream (sendWriter, optionally under touchWriter) yields a
// message — any other writer (a job live buffer, a workflow buffer) keeps
// its streamed hint and delivers nothing.
func TestToolBlock_ViewCallbacks_AreOutGated(t *testing.T) {
	var got []tea.Msg
	capture := func(msg tea.Msg) { got = append(got, msg) }
	start := mainToolCallView(capture)
	done := mainToolCallDone(capture)

	if !start(&sendWriter{send: capture}, "id1", "bash", "ls", "{}", "", "hint") {
		t.Fatal("main sendWriter must be taken over")
	}
	if !start(&touchWriter{w: &sendWriter{send: capture}, touch: func() {}}, "id2", "bash", "ls", "{}", "", "hint") {
		t.Fatal("touchWriter over the main stream must be taken over")
	}
	if start(&bytes.Buffer{}, "id3", "bash", "ls", "{}", "", "hint") {
		t.Error("a plain buffer (job/workflow out) must NOT be taken over")
	}
	if done(&bytes.Buffer{}, "id3", "result", false) {
		t.Error("done must be gated the same way")
	}
	if len(got) != 2 {
		t.Fatalf("captured %d messages, want 2: %#v", len(got), got)
	}
	s0, ok := got[0].(toolCallStartMsg)
	if !ok || s0.id != "id1" || s0.hintText != "hint" || s0.summary != "ls" {
		t.Errorf("start message = %#v", got[0])
	}
	if s1, ok := got[1].(toolCallStartMsg); !ok || s1.id != "id2" {
		t.Errorf("second message = %#v", got[1])
	}
	if !done(&sendWriter{send: capture}, "id9", "result", true) {
		t.Fatal("done on the main stream must be taken over")
	}
	last := got[len(got)-1]
	if e, ok := last.(toolCallEndMsg); !ok || e.id != "id9" || e.result != "result" || !e.isError {
		t.Errorf("end message = %#v", last)
	}
	// The agents they attach to compile through the helper (construction
	// smoke — the callbacks themselves are wired in local's tests).
	_ = withMainToolCallView(local.New("http://127.0.0.1:1", "m"), capture)
}

// TestWorkflowDisplaySend_TranslatesToolCalls pins the workflow-side
// translation: a start reaches the live buffer as the plain hint chunk the
// stream used to carry; an end is dropped (outcome delivery is main-only).
func TestWorkflowDisplaySend_TranslatesToolCalls(t *testing.T) {
	var got []tea.Msg
	ds := workflowDisplaySend(func(msg tea.Msg) { got = append(got, msg) })
	ds(toolCallStartMsg{id: "w1", name: "bash", summary: "ls", hintText: "\nhint-line\n", diff: "ignored"})
	if len(got) != 1 {
		t.Fatalf("captured %#v", got)
	}
	chunk, ok := got[0].(workflow.WorkflowChunkMsg)
	if !ok || chunk.Text != "\nhint-line\n" {
		t.Errorf("translated chunk = %#v", got[0])
	}
	ds(toolCallEndMsg{id: "w1", result: "r"})
	if len(got) != 1 {
		t.Errorf("toolCallEndMsg must be dropped, captured %#v", got)
	}
	// The ordinary chunk path is untouched.
	ds(chunkMsg{text: "plain"})
	if len(got) != 2 {
		t.Errorf("chunkMsg must still translate, captured %#v", got)
	}
}

// TestToolsCmd pins the command surface: counts, expand/collapse pinning,
// the usage line, and the empty-view toast (the non-empty view mirrors
// TestStartThinkView's attach assertions below).
func TestToolsCmd(t *testing.T) {
	m := newTestModelForPanels(t)
	m, _ = m.handleToolsCmd("") // empty: answer line, no blocks
	m.appendTranscript("x\n")
	m = toolUpdate(t, m, startMsg("t1", "bash", "ls", `{"command":"ls"}`, ""))
	m = toolUpdate(t, m, startMsg("t2", "grep", "p", `{"pattern":"p"}`, ""))

	m, _ = m.handleToolsCmd("expand")
	for i := range m.toolBlocks {
		if !m.toolBlocks[i].expanded || !m.toolBlocks[i].userSet {
			t.Errorf("block %d not pinned open: %+v", i, m.toolBlocks[i])
		}
	}
	if !m.toolFoldOpen {
		t.Error("expand must set the session fold status")
	}
	m, _ = m.handleToolsCmd("collapse")
	for i := range m.toolBlocks {
		if m.toolBlocks[i].expanded {
			t.Errorf("block %d still open after collapse", i)
		}
	}
	m, _ = m.handleToolsCmd("bogus")
	if !strings.Contains(m.transcript.String(), "usage: /tools") {
		t.Errorf("unknown subcommand must print usage, transcript = %q", m.transcript.String())
	}
	// Empty view: toast only, no attach.
	emptyModel := newTestModelForPanels(t)
	empty, cmd := emptyModel.startToolView()
	if cmd != nil || empty.attached != nil {
		t.Errorf("empty session must not attach, got cmd=%v attached=%v", cmd != nil, empty.attached)
	}
}

// TestToolBlocks_TidyAndFoldStatus pins the turn-boundary rules: the tidy
// folds the previous turn's blocks and pins them, the fold status opts out
// (/tools expand), and a user-opened block is folded by the tidy exactly
// like the think side — with the status itself untouched by the tidy.
func TestToolBlocks_TidyAndFoldStatus(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendTranscript("x\n")
	m = toolUpdate(t, m, startMsg("t1", "bash", "ls", `{"command":"ls"}`, ""))
	m = toolUpdate(t, m, toolCallEndMsg{id: "t1", result: "r"})
	m.toggleToolBlock(0) // user opened it

	m.collapseToolBlocksBefore(len(m.toolBlocks))
	if m.toolBlocks[0].expanded || !m.toolBlocks[0].userSet {
		t.Errorf("tidy must fold+pin: %+v", m.toolBlocks[0])
	}
	if m.toolFoldOpen {
		t.Error("tidy must not change the fold status")
	}
	// /tools expand opts the tidy out.
	m.setAllToolBlocks(true)
	m.collapseToolBlocksBefore(len(m.toolBlocks))
	if !m.toolBlocks[0].expanded {
		t.Error("tidy must not run while the fold status is open")
	}
	// closeOpenToolBlock respects the pin for running blocks.
	m.appendTranscript("y\n")
	m = toolUpdate(t, m, startMsg("t2", "grep", "p", `{}`, ""))
	m.toggleToolBlock(1) // user opened the running block
	m.closeOpenToolBlock()
	if !m.toolBlocks[1].closed || !m.toolBlocks[1].expanded {
		t.Errorf("turn-end close must fold unpinned blocks and keep the pin: %+v", m.toolBlocks[1])
	}
}

// TestReseedTranscript_ClearsToolBlocks: a /resume switch drops the fold
// containers with the transcript they anchor into (and resets the fold
// status), exactly like the reasoning blocks.
func TestReseedTranscript_ClearsToolBlocks(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendTranscript("x\n")
	m = toolUpdate(t, m, startMsg("t1", "bash", "ls", `{}`, ""))
	m.setAllToolBlocks(true)
	if len(m.toolBlocks) == 0 || !m.toolFoldOpen {
		t.Fatal("precondition failed")
	}
	m.reseedTranscriptFromHistory()
	if len(m.toolBlocks) != 0 || m.toolFoldOpen {
		t.Errorf("reseed must clear toolBlocks and the fold status: blocks=%d status=%v",
			len(m.toolBlocks), m.toolFoldOpen)
	}
}

// TestToolHeader_TruncatesToOneRow: the ordinal hit-test counts complete
// headers per row — a long summary must collapse to one row with the count
// suffix intact (never wrapped, never cut tail-first).
func TestToolHeader_TruncatesToOneRow(t *testing.T) {
	m := layoutTestModel(t, 100, 40)
	long := strings.Repeat("very-long-argument-summary ", 30)
	m = toolUpdate(t, m, startMsg("t1", "bash", long, `{"command":"x"}`, ""))
	hdr := m.toolHeader(&m.toolBlocks[0])
	if n := strings.Count(hdr, "\n"); n != 0 {
		t.Fatalf("header must be one row, got %d newlines: %q", n, hdr)
	}
	if w := ansiLen(hdr); w > 100 {
		t.Errorf("header width = %d, want <= 100 (viewport): %q", w, hdr)
	}
	// The count suffix survives truncation (the block is still running, so
	// the header also carries the live marker).
	if !strings.Contains(ansi.Strip(hdr), "— 3 lines") {
		t.Errorf("count suffix must survive truncation: %q", hdr)
	}
}

// ansiLen is the cell width of an ANSI-stripped string for these asserts.
func ansiLen(s string) int { return len([]rune(ansi.Strip(s))) }

// TestToolBlockView_RealProductionChain drives a REAL turn through the REAL
// writer chain a dispatched TUI turn builds: runTurn's touchWriter over the
// sendWriter, dispatch's activityWriter over that, and the tags writers
// local.Run adds on top when tag callbacks are set (always for a dispatched
// turn — WithTagCallbacks stores a non-nil onPercept closure unconditionally).
// The gate must see through ALL of it. The original isMainTurnOut only
// unwrapped touchWriter/sendWriter, so it returned false for every
// local-provider turn: no toolCallStartMsg was ever sent, NO fold container
// was ever created, and tools kept their streamed » hint — the live bug
// behind "tools are not expandable with mouse nor shortcut nor command":
// with zero blocks in m.toolBlocks, every expansion surface had nothing to
// act on. Hand-built sendWriter/touchWriter gate tests passed because they
// never reproduced this chain (fixture-reality gap — same class as the
// showcase sleep bug).
func TestToolBlockView_RealProductionChain(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&requests, 1) == 1 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"fc1","function":{"name":"grep","arguments":"{\"pattern\":\"needle\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"found"},"finish_reason":"stop"}]}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var got []tea.Msg
	capture := func(msg tea.Msg) { got = append(got, msg) }

	// The chain runTurn + dispatch build before Run even starts (sendWriter →
	// touchWriter → activityWriter) — production constructs it the same way.
	aw := newActivityWriter(&touchWriter{w: &sendWriter{send: capture}, touch: func() {}})

	agent := local.New(srv.URL, "test-model").
		WithMemConfig(local.MemConfig{MaxToolIterations: 5}).
		WithTagCallbacks("nonce-real", "primary", "escalation",
			func(string) {}, func(string, string) {}).
		WithOnToolCallView(mainToolCallView(capture)).
		WithOnToolCallDone(mainToolCallDone(capture))

	// Run adds the tags writers on top (tagNonce set + both callbacks
	// non-nil): the outermost type reaching executeToolCalls is
	// *tags.PerceptWriter — the exact type the old gate hit first and
	// bailed on.
	if _, err := agent.Run(context.Background(), nil, "find it", aw, &session.Session{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var start toolCallStartMsg
	var end toolCallEndMsg
	var sawStart bool
	for _, m := range got {
		switch v := m.(type) {
		case toolCallStartMsg:
			start, sawStart = v, true
		case toolCallEndMsg:
			end = v
		case chunkMsg:
			if strings.Contains(v.text, "» grep") {
				t.Errorf("streamed hint leaked despite takeover: %q", v.text)
			}
		}
	}
	if !sawStart || start.id != "fc1" || start.name != "grep" {
		t.Fatalf("gate never passed the production chain — no fold container would open: start=%#v (got %d msgs)", start, len(got))
	}
	if !strings.Contains(start.argsJSON, "needle") || start.hintText == "" {
		t.Errorf("start message carries incomplete payload: %#v", start)
	}
	if end.id != "fc1" || end.isError {
		t.Errorf("done message = %#v, want id=fc1 non-error", end)
	}
}

// TestToolBlock_MultiLineSummary_HeaderStaysOneRowAndCollapsible pins the
// reported bug: a bash command carrying a heredoc (newlines inside the
// summary) rendered a multi-line header — toolHeaderRe's `.` cannot cross
// \n, so the header row never matched, toolBlockAtLine found no target, and
// the block would not collapse on click. The constructor flattens: one row,
// matchable, clickable, in every state.
func TestToolBlock_MultiLineSummary_HeaderStaysOneRowAndCollapsible(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendTranscript("run:\n")
	heredoc := "cd /x && python3 - <<'EOF'\np='cmd/milk/keymap_test.go'\ns=open(p).read()\nold='for _, want'\nEOF"
	args, err := json.Marshal(map[string]any{"command": heredoc})
	if err != nil {
		t.Fatal(err)
	}
	m = toolUpdate(t, m, startMsg("m1", "bash", heredoc, string(args), ""))

	b := &m.toolBlocks[0]
	if strings.Contains(b.summary, "\n") || strings.Contains(b.summary, "\r") {
		t.Fatalf("stored summary still has newlines: %q", b.summary)
	}
	hdr := m.toolHeader(b)
	if strip := ansi.Strip(hdr); strings.Contains(strip, "\n") {
		t.Fatalf("header is not one row: %q", strip)
	}
	if n := toolHeaderCount(hdr); n != 1 {
		t.Fatalf("toolHeaderCount = %d, want 1 (click ordinal)", n)
	}
	if !toolHeaderRe.MatchString(ansi.Strip(hdr)) {
		t.Fatalf("header must match toolHeaderRe: %q", ansi.Strip(hdr))
	}

	// The collapse bug itself: the rendered header row must be a click
	// target that maps to this block.
	rows := strings.Split(m.transcriptDisplay(), "\n")
	line := -1
	for i, r := range rows {
		if strings.Contains(ansi.Strip(r), "▸ bash") {
			line = i
			break
		}
	}
	if line < 0 {
		t.Fatalf("no header row in display: %q", m.transcriptDisplay())
	}
	if idx := m.toolBlockAtLine(line); idx != 0 {
		t.Fatalf("toolBlockAtLine(%d) = %d, want 0 — block is not clickable", line, idx)
	}

	// Expand (body shows the pretty input) and collapse again: the header
	// stays one row and matchable on both sides of the toggle.
	m.toggleToolBlock(0)
	if !m.toolBlocks[0].expanded {
		t.Fatal("first toggle should expand")
	}
	m.toggleToolBlock(0)
	if m.toolBlocks[0].expanded {
		t.Fatal("second toggle should collapse")
	}
	if n := toolHeaderCount(m.toolHeader(&m.toolBlocks[0])); n != 1 {
		t.Fatalf("after toggle: toolHeaderCount = %d, want 1", n)
	}
}

// TestCliToolArgSummary_FlattensNewlines: the summary feeding the hint, the
// fold header and oversight notifications is one row by construction.
func TestCliToolArgSummary_FlattensNewlines(t *testing.T) {
	got := cliToolArgSummary(map[string]any{"command": "python3 -\np='f'\r\nEOF"})
	if got != "python3 - p='f' EOF" {
		t.Fatalf("cliToolArgSummary = %q, want %q", got, "python3 - p='f' EOF")
	}
}
