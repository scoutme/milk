// Package acp implements milk's ACP v2 parity surface (Phase 3 of the
// machine-readable output design, ADR-0049): the session/update kinds and
// agent->client messages an external host needs to reach TUI parity —
// workflow plans (plan_update), agent-owned terminals (terminal_update +
// terminal_output_chunk), structured user input (elicitation/create +
// elicitation/complete behind Host.Elicit), slash-command and session-config
// surfaces (available_commands_update, session/set_config_option),
// session_info_update._meta route/state snapshots, background-agent tool
// trees with streamed tool_call_content_chunk content, and milk's Ext*
// notification channels (milk/notification|warning|memory|route).
//
// Wire shapes follow the upstream ACP v2 schema
// (agentclientprotocol/agent-client-protocol schema/v2) verbatim: standard
// fields keep ACP's camelCase names, enums are open-set ("ignore values you
// don't recognize"), and `_meta` is reserved for implementation metadata.
// Milk-specific payloads ride the sanctioned Ext*/_meta mechanisms and use
// snake_case keys per the locked §8.3 conventions. The mapping from milk-side
// signals onto these shapes lives in map.go ("one canonical model, mapped in
// exactly one place"); the TUI-side parity checks live in
// cmd/milk/host_tui.go.
package acp

import (
	"context"
)

// ProtocolVersion is the ACP protocol version this surface targets (v2 is the
// documented forward path; a v1 bridge is a translation layer only if v1-only
// hosts matter — see the design doc §3 delta table).
const ProtocolVersion = 2

// JSON-RPC method names touched by the parity surface.
const (
	// MethodSessionUpdate is the agent->client streaming notification
	// carrying every SessionUpdate variant.
	MethodSessionUpdate = "session/update"
	// MethodSessionSetConfigOption is the client->agent request that applies
	// a SessionConfigOption change (/think, /agent switch, /model).
	MethodSessionSetConfigOption = "session/set_config_option"
	// MethodElicitationCreate is the agent->client structured-input request
	// (form/select prompts).
	MethodElicitationCreate = "elicitation/create"
	// MethodElicitationComplete is the agent->client notification closing an
	// elicitation once its result has been consumed.
	MethodElicitationComplete = "elicitation/complete"
	// MethodRequestPermission is the agent->client structured permission
	// prompt (ADR-0013/0015); Phase 2 owns the flow, declared here so the
	// Host interface and parity checks have one vocabulary.
	MethodRequestPermission = "session/request_permission"
	// MethodCancelRequest cancels an in-flight request in either direction.
	MethodCancelRequest = "$/cancel_request"
)

// Wire identifier types. ACP models these as distinct strings; keeping them
// distinct in Go prevents cross-wiring a plan ID into a terminal field.
type (
	SessionID            string
	PlanID               string
	ToolCallID           string
	TerminalID           string
	MessageID            string
	ElicitationID        string
	SessionConfigID      string
	SessionConfigValueID string
)

// JSON-RPC 2.0 envelopes. One message per write, flushable per event — the
// framing contract for SSH/websocket relays (design §2 requirement 3).
type (
	// Request is a JSON-RPC request (either direction).
	Request struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}

	// Notification is a JSON-RPC notification (either direction). Ext*
	// extension messages are notifications or requests whose Method is
	// outside the standard method map (see ext.go).
	Notification struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}

	// Response is a JSON-RPC response.
	Response struct {
		JSONRPC string    `json:"jsonrpc"`
		ID      any       `json:"id"`
		Result  any       `json:"result,omitempty"`
		Error   *RPCError `json:"error,omitempty"`
	}

	// RPCError is a JSON-RPC error object.
	RPCError struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data,omitempty"`
	}
)

// Conn is the write side of an ACP connection: one JSON-RPC message per call.
// The stdio serve loop implements it over os.Stdin/os.Stdout; tests implement
// it with an in-memory recorder.
type Conn interface {
	// Notify sends a JSON-RPC notification.
	Notify(method string, params any) error
	// Request sends a JSON-RPC request and unmarshals the response into
	// result (which may be nil to ignore the body).
	Request(ctx context.Context, method string, params any, result any) error
}

// SessionUpdate is one variant of the ACP SessionUpdate union — the payload
// of a session/update notification. Every variant marshals with its own
// `sessionUpdate` discriminator ("open-set enums: ignore values you don't
// recognize").
type SessionUpdate interface {
	isSessionUpdate()
}

// UpdateSessionNotification is the `session/update` params shape
// (UpdateSessionNotification in the upstream schema).
type UpdateSessionNotification struct {
	SessionID SessionID      `json:"sessionId"`
	Update    SessionUpdate  `json:"update"`
	Meta      map[string]any `json:"_meta,omitempty"`
}

// --- shared content shapes -------------------------------------------------

// ContentBlock is a single content block. The parity surface streams text;
// other block kinds pass through as open-set types.
type ContentBlock struct {
	Type string `json:"type"` // "text" (open-set)
	Text string `json:"text,omitempty"`
}

// TextBlock is the common ContentBlock{text}.
func TextBlock(text string) ContentBlock { return ContentBlock{Type: "text", Text: text} }

// ToolCallContent is one item of tool-call content (ToolCallContent union:
// "content" wrapping a ContentBlock, "terminal" referencing an agent-owned
// terminal, or an open-set extension type).
type ToolCallContent struct {
	Type       string        `json:"type"`
	Content    *ContentBlock `json:"content,omitempty"`
	TerminalID *TerminalID   `json:"terminalId,omitempty"`
}

// ContentItem wraps a ContentBlock as ToolCallContent{type:"content"}.
func ContentItem(text string) ToolCallContent {
	blk := TextBlock(text)
	return ToolCallContent{Type: "content", Content: &blk}
}

// TerminalItem links a tool call to an agent-owned terminal
// (ToolCallContent{type:"terminal"}).
func TerminalItem(id TerminalID) ToolCallContent {
	return ToolCallContent{Type: "terminal", TerminalID: &id}
}

// ToolCallLocation is a source location a tool call touched.
type ToolCallLocation struct {
	Path string `json:"path"`
	Line *int   `json:"line,omitempty"`
}

// --- tool lifecycle (v2 create-or-update) ----------------------------------

// ToolKind is the ACP ToolKind enum (open-set).
type ToolKind string

const (
	ToolKindRead       ToolKind = "read"
	ToolKindEdit       ToolKind = "edit"
	ToolKindDelete     ToolKind = "delete"
	ToolKindMove       ToolKind = "move"
	ToolKindSearch     ToolKind = "search"
	ToolKindExecute    ToolKind = "execute"
	ToolKindThink      ToolKind = "think"
	ToolKindFetch      ToolKind = "fetch"
	ToolKindSwitchMode ToolKind = "switch_mode"
	ToolKindOther      ToolKind = "other"
)

// ToolCallStatus is the ACP ToolCallStatus enum (open-set).
type ToolCallStatus string

const (
	ToolCallPending    ToolCallStatus = "pending"
	ToolCallInProgress ToolCallStatus = "in_progress"
	ToolCallCompleted  ToolCallStatus = "completed"
	ToolCallFailed     ToolCallStatus = "failed"
	ToolCallCancelled  ToolCallStatus = "cancelled"
)

// ToolCallUpdate is the v2 create-or-update tool lifecycle message: repeated
// updates with the same toolCallId patch the same call (progressive rawInput
// fills, status transitions, streamed content).
type ToolCallUpdate struct {
	SessionUpdate string             `json:"sessionUpdate"` // "tool_call_update"
	ToolCallID    ToolCallID         `json:"toolCallId"`
	Name          string             `json:"name,omitempty"`
	Title         string             `json:"title,omitempty"`
	Kind          ToolKind           `json:"kind,omitempty"`
	Status        ToolCallStatus     `json:"status,omitempty"`
	Content       []ToolCallContent  `json:"content,omitempty"`
	Locations     []ToolCallLocation `json:"locations,omitempty"`
	RawInput      any                `json:"rawInput,omitempty"`
	RawOutput     any                `json:"rawOutput,omitempty"`
	Meta          map[string]any     `json:"_meta,omitempty"`
}

func (ToolCallUpdate) isSessionUpdate() {}

// ToolCallContentChunk streams one item of tool-call content (live-attach
// parity: F3/attach views become streamed tool output).
type ToolCallContentChunk struct {
	SessionUpdate string          `json:"sessionUpdate"` // "tool_call_content_chunk"
	ToolCallID    ToolCallID      `json:"toolCallId"`
	Content       ToolCallContent `json:"content"`
	Meta          map[string]any  `json:"_meta,omitempty"`
}

func (ToolCallContentChunk) isSessionUpdate() {}

// --- messaging, state, usage ----------------------------------------------

// AgentMessageChunk / AgentThoughtChunk stream assistant text and reasoning
// (ADR-0042: reasoning preserved verbatim). Declared here so map.go and the
// parity checks can cover the whole session/update surface.
type ContentChunk struct {
	SessionUpdate string         `json:"sessionUpdate"` // discriminator, see constructors
	MessageID     MessageID      `json:"messageId"`
	Content       ContentBlock   `json:"content"`
	Meta          map[string]any `json:"_meta,omitempty"`
}

func (ContentChunk) isSessionUpdate() {}

// AgentMessageChunk streams one assistant text delta.
func AgentMessageChunk(id MessageID, text string) ContentChunk {
	return ContentChunk{SessionUpdate: "agent_message_chunk", MessageID: id, Content: TextBlock(text)}
}

// AgentThoughtChunk streams one reasoning delta (/think panel feed).
func AgentThoughtChunk(id MessageID, text string) ContentChunk {
	return ContentChunk{SessionUpdate: "agent_thought_chunk", MessageID: id, Content: TextBlock(text)}
}

// SessionState is the ACP state_update state enum (open-set).
type SessionState string

const (
	SessionStateRunning        SessionState = "running"
	SessionStateIdle           SessionState = "idle"
	SessionStateRequiresAction SessionState = "requires_action"
)

// Stop reasons reported on an idle state_update (open-set).
const (
	StopReasonEndTurn   = "end_turn"
	StopReasonMaxTokens = "max_tokens"
	StopReasonMaxTurns  = "max_turn_requests"
	StopReasonRefusal   = "refusal"
	StopReasonCancelled = "cancelled"
	StopReasonError     = "error"
)

// StateUpdate reports foreground-work state (running/idle/requires_action).
// requires_action fires while a permission or elicitation is outstanding —
// the TUI's "needs input" state.
type StateUpdate struct {
	SessionUpdate string         `json:"sessionUpdate"` // "state_update"
	State         SessionState   `json:"state"`
	StopReason    string         `json:"stopReason,omitempty"` // idle only
	Meta          map[string]any `json:"_meta,omitempty"`
}

func (StateUpdate) isSessionUpdate() {}

// RunningState reports foreground work in progress.
func RunningState() StateUpdate {
	return StateUpdate{SessionUpdate: "state_update", State: SessionStateRunning}
}

// IdleState reports the turn end with its stop reason.
func IdleState(stopReason string) StateUpdate {
	return StateUpdate{SessionUpdate: "state_update", State: SessionStateIdle, StopReason: stopReason}
}

// RequiresActionState reports the session is blocked on user action.
func RequiresActionState() StateUpdate {
	return StateUpdate{SessionUpdate: "state_update", State: SessionStateRequiresAction}
}

// UsageUpdate reports context-window use (used/size in tokens) and optional
// cost. milk maps cache_read into used-context accounting and emits cost only
// when a pricing table exists (design §7.1).
type UsageUpdate struct {
	SessionUpdate string         `json:"sessionUpdate"` // "usage_update"
	Used          int64          `json:"used"`
	Size          int64          `json:"size"`
	Cost          *Cost          `json:"cost,omitempty"`
	Meta          map[string]any `json:"_meta,omitempty"`
}

func (UsageUpdate) isSessionUpdate() {}

// Cost is the ACP cost shape ({amount, currency}).
type Cost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}
