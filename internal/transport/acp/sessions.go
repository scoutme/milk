// Session-management wire shapes (session/list, session/resume,
// session/close, session/delete) plus the message-upsert session/update
// variants ACP v2 designates for chat-history replay (user_message,
// agent_message, agent_thought). Field names and shapes are verified against
// the upstream agentclientprotocol/agent-client-protocol schema/v2/schema.json
// like lifecycle.go's — camelCase standard fields verbatim, `_meta` reserved
// for implementation metadata. See docs/acp-session-resume-plan.md.
package acp

import (
	"encoding/json"
	"fmt"
)

// JSON-RPC error codes milk emits deliberately. JSON-RPC reserves -32768..-32000;
// these are the ones the ACP session-management surface needs beyond the
// reserved -32601 "method not found" (MethodNotFoundError) and the generic
// -32000 server error everything else falls back to.
const (
	// CodeInvalidParams is "invalid params": a well-formed request whose
	// params fail validation (e.g. session/resume cwd mismatch, an unknown
	// replayFrom cursor type).
	CodeInvalidParams = -32602
	// CodeResourceNotFound is the schema's session-not-found error
	// ("If the requested session does not exist, return a resource not found
	// error").
	CodeResourceNotFound = -32002
)

// ListSessionsRequest is session/list's params. cwd filters to one working
// directory when set (schema: relative paths resolve against the client's
// working directory — milk only matches absolute paths, see the handler);
// omitted or null lists sessions for every cwd. cursor continues a previous
// page.
type ListSessionsRequest struct {
	CWD    string             `json:"cwd,omitempty"`
	Cursor *SessionListCursor `json:"cursor,omitempty"`
}

// SessionListCursor is the opaque pagination cursor ("opaque" per the
// schema — clients must echo it back unmodified). milk encodes a decimal
// offset as base64("o:<n>").
type SessionListCursor string

// SessionInfo is one entry of ListSessionsResponse. additionalDirectories is
// omitted: milk sessions have none.
type SessionInfo struct {
	SessionID SessionID `json:"sessionId"`
	CWD       string    `json:"cwd"`
	Title     string    `json:"title,omitempty"`
	UpdatedAt string    `json:"updatedAt,omitempty"` // RFC 3339
}

// ListSessionsResponse is session/list's result, one page of sessions.
type ListSessionsResponse struct {
	Sessions   []SessionInfo      `json:"sessions"`
	NextCursor *SessionListCursor `json:"nextCursor,omitempty"`
}

// ResumeSessionRequest is session/resume's params. The schema conditions the
// whole method on cwd matching the session's cwd. replayFrom is the history
// replay cursor: omitted/null resumes without replaying, {"type":"start"}
// replays all retained history before the response; ParseReplayFrom rejects
// any other cursor type rather than guessing.
type ResumeSessionRequest struct {
	SessionID  SessionID       `json:"sessionId"`
	CWD        string          `json:"cwd"`
	ReplayFrom json.RawMessage `json:"replayFrom,omitempty"`
}

// ResumeSessionResponse is session/resume's result.
type ResumeSessionResponse struct {
	ConfigOptions     []SessionConfigOption `json:"configOptions,omitempty"`
	AvailableCommands []AvailableCommand    `json:"availableCommands,omitempty"`
}

// ReplayFrom is the replay cursor union. Only {"type":"start"} is defined
// today; future/custom cursor types are rejected at parse time (see
// ParseReplayFrom), never guessed at.
type ReplayFrom struct {
	Type string `json:"type"`
}

// ReplayStart returns the {"type":"start"} cursor — "replay all retained
// conversation history before responding".
func ReplayStart() ReplayFrom { return ReplayFrom{Type: "start"} }

// ParseReplayFrom decodes a replayFrom cursor: omitted or null returns nil
// (resume without replaying), {"type":"start"} returns ReplayStart(), and any
// other cursor type returns an error ("reject the request rather than
// guessing where to replay from").
func ParseReplayFrom(raw json.RawMessage) (*ReplayFrom, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var c ReplayFrom
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("replayFrom: %w", err)
	}
	if c.Type != "start" {
		return nil, fmt.Errorf("replayFrom: unsupported cursor type %q (only \"start\" is defined)", c.Type)
	}
	return &c, nil
}

// CloseSessionRequest is session/close's params.
type CloseSessionRequest struct {
	SessionID SessionID `json:"sessionId"`
}

// CloseSessionResponse is session/close's result ({} — closing an unknown or
// already-closed session is idempotent, not an error).
type CloseSessionResponse struct{}

// DeleteSessionRequest is session/delete's params.
type DeleteSessionRequest struct {
	SessionID SessionID `json:"sessionId"`
}

// DeleteSessionResponse is session/delete's result.
type DeleteSessionResponse struct{}

// SessionDeleteCapabilities is the `session.delete` capability object. Per
// the schema, "supplying {} means the agent supports deleting sessions from
// session/list" — it has no fields.
type SessionDeleteCapabilities struct{}

// MessageUpsert is one of the v2 message-upsert session/update variants —
// user_message / agent_message / agent_thought. Upsert semantics: repeated
// updates with the same messageId patch that message ("Agents can send this
// when they accept or replay a user message"), which is what makes history
// replay idempotent across repeated resumes.
type MessageUpsert struct {
	SessionUpdate string         `json:"sessionUpdate"` // "user_message" | "agent_message" | "agent_thought"
	MessageID     MessageID      `json:"messageId"`
	Content       []ContentBlock `json:"content"`
	Meta          map[string]any `json:"_meta,omitempty"`
}

func (MessageUpsert) isSessionUpdate() {}

// UserMessageUpsert creates a user_message upsert (accept/replay of a user
// message).
func UserMessageUpsert(id MessageID, text string) MessageUpsert {
	return MessageUpsert{SessionUpdate: "user_message", MessageID: id, Content: []ContentBlock{TextBlock(text)}}
}

// AgentMessageUpsert creates an agent_message upsert (replayed assistant
// message).
func AgentMessageUpsert(id MessageID, text string) MessageUpsert {
	return MessageUpsert{SessionUpdate: "agent_message", MessageID: id, Content: []ContentBlock{TextBlock(text)}}
}

// AgentThoughtUpsert creates an agent_thought upsert (replayed reasoning).
func AgentThoughtUpsert(id MessageID, text string) MessageUpsert {
	return MessageUpsert{SessionUpdate: "agent_thought", MessageID: id, Content: []ContentBlock{TextBlock(text)}}
}

// UserMessageChunk streams one user message delta — the v1-client fallback
// shape for replay/accept (v1 has no message upserts; chunks with the same
// messageId append into one message).
func UserMessageChunk(id MessageID, text string) ContentChunk {
	return ContentChunk{SessionUpdate: "user_message_chunk", MessageID: id, Content: TextBlock(text)}
}
