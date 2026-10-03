package acp

import (
	"testing"
	"time"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/livebuf"
	"github.com/scoutme/milk/internal/loop"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/router"
)

// map.go parity: session_info_update._meta, background-agent tool trees +
// tool_call_content_chunk, and the matching Ext* extensions.

func TestSessionInfoUpdateMeta(t *testing.T) {
	meta := SessionInfoMeta{
		Route:            RoutePayload{Target: "escalation", Reason: "session state ESCALATION_WAITING", Conclusive: true},
		SessionState:     "idle",
		StickyEscalation: true,
		RouteHistory:     []RouteHistoryEntry{{Turn: 0, Target: "primary"}, {Turn: 2, Target: "escalation"}},
	}
	upd := SessionInfo("wire contract sprint", time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), meta)
	if upd.SessionUpdate != "session_info_update" {
		t.Fatalf("sessionUpdate = %q", upd.SessionUpdate)
	}
	if upd.UpdatedAt != "2026-10-02T12:00:00Z" {
		t.Fatalf("updatedAt = %q", upd.UpdatedAt)
	}
	body := jsonBody(t, upd)
	m, ok := body["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("_meta missing: %v", body)
	}
	// milk's route/session-state snapshot rides _meta with snake_case keys.
	if m["session_state"] != "idle" {
		t.Fatalf("session_state = %v", m["session_state"])
	}
	if m["sticky_escalation"] != true {
		t.Fatalf("sticky_escalation = %v", m["sticky_escalation"])
	}
	route := m["route"].(map[string]any)
	if route["target"] != "escalation" || route["conclusive"] != true {
		t.Fatalf("route = %v", route)
	}
	hist := m["route_history"].([]any)
	if len(hist) != 2 {
		t.Fatalf("route_history = %v", hist)
	}
	if _, ok := body["title"]; !ok {
		t.Fatalf("title missing: %v", body)
	}

	// And it goes out as a session/update notification.
	n := Mapper{Session: "sess-1"}.SessionInfoNotification("t", time.Time{}, meta)
	if n.Method != MethodSessionUpdate {
		t.Fatalf("method = %q", n.Method)
	}
}

func TestBackgroundJobTree(t *testing.T) {
	job := local.Job{
		ID:     "7",
		Label:  "survey ACP schema",
		Task:   "fetch and summarize schema/v2",
		Status: local.JobRunning,
		Role:   "primary",
		Model:  "qwen2.5-coder-32b",
		Live:   livebuf.New(0),
	}
	tree := BackgroundJobTree(job)
	if len(tree) != 2 {
		t.Fatalf("tree = %+v", tree)
	}
	root, child := tree[0], tree[1]
	if root.ToolCallID != ToolCallID("spawn:7") || root.Name != "spawn_background_agent" {
		t.Fatalf("root = %+v", root)
	}
	if root.Kind != ToolKindOther || root.Status != ToolCallInProgress {
		t.Fatalf("root kind/status = %v %v", root.Kind, root.Status)
	}
	// Parent linkage: _meta parent_tool_use_id + title/content breadcrumb
	// (ToolCallUpdate has no parent field).
	if child.Meta["parent_tool_use_id"] != "spawn:7" {
		t.Fatalf("child meta = %v", child.Meta)
	}
	if child.Title != "spawn_background_agent: survey ACP schema › background agent" {
		t.Fatalf("child title = %q", child.Title)
	}
	if len(child.Content) != 1 || child.Content[0].Content.Text != "parent: spawn:7" {
		t.Fatalf("child content = %+v", child.Content)
	}
	if child.ToolCallID != JobToolCallID("7") {
		t.Fatalf("child id = %q", child.ToolCallID)
	}

	// Completed job: result rides rawOutput.
	job.Status = local.JobCompleted
	job.Result = "done"
	tree = BackgroundJobTree(job)
	if tree[0].Status != ToolCallCompleted || tree[1].Status != ToolCallCompleted {
		t.Fatalf("completed tree = %+v", tree)
	}
	out := tree[1].RawOutput.(map[string]any)
	if out["result"] != "done" {
		t.Fatalf("rawOutput = %v", out)
	}

	// Failed job.
	job.Status = local.JobFailed
	tree = BackgroundJobTree(job)
	if tree[1].Status != ToolCallFailed {
		t.Fatalf("failed tree = %+v", tree)
	}

	// Wrapped as session/update notifications.
	ns := Mapper{Session: "s"}.BackgroundJobTreeNotifications(job)
	if len(ns) != 2 || ns[0].Method != MethodSessionUpdate {
		t.Fatalf("notifications = %+v", ns)
	}
}

func TestToolCallContentChunkStreaming(t *testing.T) {
	buf := livebuf.New(0)
	var cursor BufCursor
	id := JobToolCallID("7")

	buf.Append([]byte("line 1\n"))
	updates := ToolCallContentPoll(id, buf, &cursor)
	if len(updates) != 1 {
		t.Fatalf("poll 1 = %+v", updates)
	}
	chunk := updates[0].(ToolCallContentChunk)
	if chunk.SessionUpdate != "tool_call_content_chunk" || chunk.ToolCallID != id {
		t.Fatalf("chunk = %+v", chunk)
	}
	if chunk.Content.Type != "content" || chunk.Content.Content.Text != "line 1\n" {
		t.Fatalf("chunk content = %+v", chunk.Content)
	}
	body := jsonBody(t, chunk)
	if body["sessionUpdate"] != "tool_call_content_chunk" {
		t.Fatalf("wire sessionUpdate = %v", body["sessionUpdate"])
	}

	// Trim → re-anchor as tool_call_update with the full retained content.
	small := livebuf.New(4)
	small.Append([]byte("ab"))
	if got := ToolCallContentPoll(id, small, &cursor); len(got) != 1 {
		t.Fatalf("small poll = %+v", got)
	}
	var smallCur BufCursor
	ToolCallContentPoll(id, small, &smallCur) // anchor "ab"
	small.Append([]byte("cdefgh"))            // trims to "efgh"
	updates = ToolCallContentPoll(id, small, &smallCur)
	if len(updates) != 1 {
		t.Fatalf("re-anchor poll = %+v", updates)
	}
	upd := updates[0].(ToolCallUpdate)
	if upd.Content[0].Content.Text != "efgh" {
		t.Fatalf("re-anchor content = %+v", upd.Content)
	}

	if got := ToolCallContentPoll(id, nil, &cursor); got != nil {
		t.Fatalf("nil buffer = %+v", got)
	}
}

func TestToolKindFor(t *testing.T) {
	cases := map[string]ToolKind{
		"read_file":              ToolKindRead,
		"edit_file":              ToolKindEdit,
		"bash":                   ToolKindExecute,
		"find_files":             ToolKindSearch,
		"grep":                   ToolKindSearch,
		"http_get":               ToolKindFetch,
		"http_request":           ToolKindFetch,
		"think_tool":             ToolKindThink,
		"spawn_background_agent": ToolKindOther,
		"start_workflow":         ToolKindOther,
		"unknown_tool":           ToolKindOther,
	}
	for name, want := range cases {
		if got := ToolKindFor(name); got != want {
			t.Errorf("ToolKindFor(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestMatchingExtMappings(t *testing.T) {
	m := Mapper{Session: "sess-1"}

	// milk/warning from a loop verdict (Signal.Category + IsConsumption).
	warn := m.LoopWarning(loop.Verdict{Signal: loop.SignalTokenVelocity, Message: "token burn"}, 41, 100)
	if warn.Method != ExtMethodWarning {
		t.Fatalf("warning method = %q", warn.Method)
	}
	wp := warn.Params.(WarningPayload)
	if wp.Category != "token_velocity" || !wp.Consumption || wp.Count != 41 || wp.Limit != 100 {
		t.Fatalf("warning payload = %+v", wp)
	}
	rep := m.LoopWarning(loop.Verdict{Signal: loop.SignalResponseRepetition}, 2, 3)
	if rep.Params.(WarningPayload).Consumption {
		t.Fatalf("repetition is not consumption: %+v", rep.Params)
	}

	// milk/route from a router decision.
	route := m.Route(router.Decision{Target: router.TargetEscalation, Reason: "model classifier", Conclusive: true})
	rp := route.Params.(RoutePayload)
	if rp.Target != "escalation" || rp.Reason != "model classifier" || !rp.Conclusive {
		t.Fatalf("route payload = %+v", rp)
	}

	// milk/route for an agent switch.
	sw := m.AgentSwitch("qwen-local", "claude")
	if sp := sw.Params.(RoutePayload); sp.From != "qwen-local" || sp.To != "claude" {
		t.Fatalf("switch payload = %+v", sp)
	}

	// milk/memory from a percept record.
	mem := m.MemoryRecord(memory.Percept{ID: "p1", Roles: memory.Roles{Theme: "wire contract"}})
	mp := mem.Params.(MemoryPayload)
	if mp.Op != "record" || mp.PerceptID != "p1" || mp.Subject != "wire contract" {
		t.Fatalf("memory payload = %+v", mp)
	}

	// milk/route + milk/notification wrap to their Ext* methods verbatim.
	if m.Ext(NotificationExt(NotificationPayload{ID: "x"})).Method != ExtMethodNotification {
		t.Fatal("notification ext method")
	}
}
