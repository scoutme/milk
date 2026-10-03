package acp

import (
	"time"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/livebuf"
	"github.com/scoutme/milk/internal/loop"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/workflow"
)

// map.go — the single place where every milk-side signal is matched to its
// ACP wire form ("matching extensions" included): ExtNotification channels
// (ext.go), workflow plans (plan.go), livebuf terminals (terminal.go),
// elicitation (elicitation.go), command/config surfaces (commands.go),
// session_info_update._meta, and background-agent tool trees with streamed
// tool_call_content_chunk content. cmd/milk/host_tui.go holds the TUI-side
// parity checks that keep this map honest.

// Mapper maps milk-side signals onto ACP messages for one session.
type Mapper struct {
	Session SessionID
}

// --- envelope helpers ------------------------------------------------------

// Update wraps a SessionUpdate into a session/update notification.
func (m Mapper) Update(u SessionUpdate) Notification {
	return Notification{
		JSONRPC: "2.0",
		Method:  MethodSessionUpdate,
		Params:  UpdateSessionNotification{SessionID: m.Session, Update: u},
	}
}

// Updates wraps each SessionUpdate into its own session/update notification.
func (m Mapper) Updates(us ...SessionUpdate) []Notification {
	out := make([]Notification, 0, len(us))
	for _, u := range us {
		out = append(out, m.Update(u))
	}
	return out
}

// Ext wraps an ExtNotification into its JSON-RPC notification (method =
// milk/*, params = the kind payload).
func (m Mapper) Ext(n ExtNotification) Notification {
	return Notification{JSONRPC: "2.0", Method: n.Method, Params: n.Params}
}

// --- matching extensions: ExtNotification channels -------------------------

// Toast maps one ADR-0048 toast onto milk/notification (id, severity,
// command_hint, body).
func (m Mapper) Toast(id, severity, commandHint, body string) Notification {
	return m.Ext(NotificationExt(NotificationPayload{
		ID: id, Severity: severity, CommandHint: commandHint, Body: body,
	}))
}

// LoopWarning maps a loop-detection verdict (internal/loop) onto milk/warning
// — carrying Signal.Category and IsConsumption() plus the count/limit facts
// so hosts render "[⚠ loop detected: …]" vs "[⚠ consumption: …]" correctly
// (issue #173).
func (m Mapper) LoopWarning(v loop.Verdict, count, limit int) Notification {
	return m.Ext(WarningExt(WarningPayload{
		Category:    v.Signal.String(),
		Consumption: v.Signal.IsConsumption(),
		Count:       count,
		Limit:       limit,
		Message:     v.Message,
	}))
}

// MemoryOp maps one memory-panel operation (record|get|list|forget) onto
// milk/memory.
func (m Mapper) MemoryOp(op, perceptID, subject string) Notification {
	return m.Ext(MemoryExt(MemoryPayload{Op: op, PerceptID: perceptID, Subject: subject}))
}

// MemoryRecord maps a recorded percept (internal/memory) onto milk/memory,
// with the percept's subject (role theme) for panel grouping.
func (m Mapper) MemoryRecord(p memory.Percept) Notification {
	return m.MemoryOp("record", p.ID, p.Roles.Theme)
}

// Route maps a routing decision (internal/router.Decision) onto milk/route.
func (m Mapper) Route(d router.Decision) Notification {
	return m.Ext(RouteExt(RoutePayload{
		Target:     string(d.Target),
		Reason:     d.Reason,
		Conclusive: d.Conclusive,
	}))
}

// AgentSwitch maps a self-escalation hand-off onto milk/route (from/to).
func (m Mapper) AgentSwitch(from, to string) Notification {
	return m.Ext(RouteExt(RoutePayload{From: from, To: to}))
}

// --- matching extensions: plan, terminal, commands -------------------------

// WorkflowPlan maps a workflow run's state onto its plan_update (plan.go).
func (m Mapper) WorkflowPlan(st *workflow.State, cancelled bool) Notification {
	return m.Update(PlanForWorkflow(st, cancelled))
}

// Terminal wraps a terminal surface update (terminal.go).
func (m Mapper) Terminal(u SessionUpdate) Notification { return m.Update(u) }

// Commands maps the advertised slash commands onto available_commands_update
// (commands.go); pass MilkCommands() for milk's own surface.
func (m Mapper) Commands(cmds []AvailableCommand) Notification {
	return m.Update(NewAvailableCommandsUpdate(cmds))
}

// ConfigOptions maps the current session-config set onto config_option_update
// (commands.go).
func (m Mapper) ConfigOptions(opts []SessionConfigOption) Notification {
	return m.Update(ConfigOptionUpdate{SessionUpdate: "config_option_update", ConfigOptions: opts})
}

// --- session_info_update._meta ---------------------------------------------

// RouteHistoryEntry is one turn's routing outcome (§6.4 result.route_history
// shape, reused in the _meta snapshot).
type RouteHistoryEntry struct {
	Turn   int    `json:"turn"`
	Target string `json:"target"`
}

// SessionInfoMeta is milk's route/session-state snapshot carried in
// session_info_update._meta: "sticky-escalation state and route history are
// per-session and ride in session_info_update._meta" (design §7.2). Field
// keys are snake_case per the §8.3 conventions (they are milk's payload,
// inside ACP's reserved _meta).
type SessionInfoMeta struct {
	Route            RoutePayload        `json:"route"`
	SessionState     string              `json:"session_state"` // running|idle|requires_action|… session state machine
	StickyEscalation bool                `json:"sticky_escalation,omitempty"`
	RouteHistory     []RouteHistoryEntry `json:"route_history,omitempty"`
}

// MetaMap renders the snapshot as the session_info_update _meta object.
func (s SessionInfoMeta) MetaMap() map[string]any {
	out := map[string]any{
		"route":         s.Route,
		"session_state": s.SessionState,
	}
	if s.StickyEscalation {
		out["sticky_escalation"] = true
	}
	if len(s.RouteHistory) > 0 {
		out["route_history"] = s.RouteHistory
	}
	return out
}

// SessionInfoUpdate is the session_info_update session update (title,
// updatedAt, _meta).
type SessionInfoUpdate struct {
	SessionUpdate string         `json:"sessionUpdate"` // "session_info_update"
	Title         string         `json:"title,omitempty"`
	UpdatedAt     string         `json:"updatedAt,omitempty"` // RFC 3339
	Meta          map[string]any `json:"_meta,omitempty"`
}

func (SessionInfoUpdate) isSessionUpdate() {}

// SessionInfo builds the session_info_update with milk's _meta snapshot.
func SessionInfo(title string, updatedAt time.Time, meta SessionInfoMeta) SessionInfoUpdate {
	upd := SessionInfoUpdate{SessionUpdate: "session_info_update", Title: title}
	if !updatedAt.IsZero() {
		upd.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	}
	upd.Meta = meta.MetaMap()
	return upd
}

// SessionInfoNotification maps a session-info change (with the route/state
// snapshot) onto its session/update notification.
func (m Mapper) SessionInfoNotification(title string, updatedAt time.Time, meta SessionInfoMeta) Notification {
	return m.Update(SessionInfo(title, updatedAt, meta))
}

// --- background-agent tool trees + tool_call_content_chunk -----------------

// ToolKindFor maps a milk tool name onto the ACP ToolKind vocabulary
// (design §7.1 mapping table). Unknown tools map to "other" — its children
// (background agents, workflow stages) appear as separate calls.
func ToolKindFor(name string) ToolKind {
	switch name {
	case "read_file", "open_file":
		return ToolKindRead
	case "edit_file", "write_file":
		return ToolKindEdit
	case "delete_file":
		return ToolKindDelete
	case "move_file":
		return ToolKindMove
	case "find_files", "grep":
		return ToolKindSearch
	case "bash":
		return ToolKindExecute
	case "think", "think_tool":
		return ToolKindThink
	case "http_get", "http_request":
		return ToolKindFetch
	case "spawn_background_agent", "start_workflow":
		return ToolKindOther
	}
	return ToolKindOther
}

// ToolCallStatusForJob maps a background job's status (ADR-0043) onto
// ToolCallStatus 1:1 where the vocabularies overlap.
func ToolCallStatusForJob(s local.JobStatus) ToolCallStatus {
	switch s {
	case local.JobRunning:
		return ToolCallInProgress
	case local.JobCompleted:
		return ToolCallCompleted
	case local.JobFailed:
		return ToolCallFailed
	}
	return ToolCallPending
}

// BackgroundJobTree builds the nested tool-call tree for one
// spawn_background_agent job: a root call for the spawn itself and the job as
// a child call. ToolCallUpdate has no parent field, so parent linkage rides
// _meta["parent_tool_use_id"] (the §6 parent_tool_use_id convention) plus the
// title/content breadcrumb — the nested-actor tree the F3 panel renders.
// The child call's streamed output arrives separately via
// ToolCallContentPoll's tool_call_content_chunk updates.
func BackgroundJobTree(job local.Job) []ToolCallUpdate {
	rootID := ToolCallID("spawn:" + job.ID)
	childID := ToolCallID("job:" + job.ID)
	status := ToolCallStatusForJob(job.Status)
	root := ToolCallUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    rootID,
		Name:          "spawn_background_agent",
		Title:         "spawn_background_agent: " + job.Label,
		Kind:          ToolKindOther,
		Status:        status,
		RawInput: map[string]any{
			"label": job.Label,
			"task":  job.Task,
			"role":  job.Role,
		},
		Meta: map[string]any{"milk/job_id": job.ID},
	}
	child := ToolCallUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    childID,
		Name:          "background_agent",
		Title:         "spawn_background_agent: " + job.Label + " › background agent",
		Kind:          ToolKindOther,
		Status:        status,
		Content:       []ToolCallContent{ContentItem("parent: " + string(rootID))},
		RawInput:      map[string]any{"task": job.Task, "model": job.Model},
		Meta: map[string]any{
			"parent_tool_use_id": string(rootID),
			"milk/job_id":        job.ID,
		},
	}
	if job.Status == local.JobCompleted {
		child.RawOutput = map[string]any{"result": job.Result}
	}
	if job.Err != nil {
		child.RawOutput = map[string]any{"error": job.Err.Error()}
	}
	return []ToolCallUpdate{root, child}
}

// BackgroundJobTreeNotifications wraps BackgroundJobTree's calls as
// session/update notifications.
func (m Mapper) BackgroundJobTreeNotifications(job local.Job) []Notification {
	tree := BackgroundJobTree(job)
	out := make([]Notification, 0, len(tree))
	for _, u := range tree {
		out = append(out, m.Update(u))
	}
	return out
}

// ToolCallContentPoll streams new content from a livebuf-backed live buffer
// (a background job's Job.Live, ADR-0047) as tool_call_content_chunk
// updates. Like TerminalStream.Poll it re-anchors with a full-content
// tool_call_update when the buffer trimmed content the cursor had not seen
// (chunk items are append-only and cannot express a drop).
func ToolCallContentPoll(toolCallID ToolCallID, buf *livebuf.Buffer, cursor *BufCursor) []SessionUpdate {
	if buf == nil || cursor == nil {
		return nil
	}
	delta, reset := cursor.Next(buf)
	if reset && delta != "" {
		return []SessionUpdate{ToolCallUpdate{
			SessionUpdate: "tool_call_update",
			ToolCallID:    toolCallID,
			Content:       []ToolCallContent{ContentItem(cursor.Sent())},
		}}
	}
	if delta == "" {
		return nil
	}
	return []SessionUpdate{ToolCallContentChunk{
		SessionUpdate: "tool_call_content_chunk",
		ToolCallID:    toolCallID,
		Content:       ContentItem(delta),
	}}
}

// JobToolCallID is the child tool-call ID for a background job.
func JobToolCallID(jobID string) ToolCallID { return ToolCallID("job:" + jobID) }
