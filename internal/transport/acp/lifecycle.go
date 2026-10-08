// Session-lifecycle wire shapes (initialize, session/new, session/prompt,
// session/cancel, $/cancel_request) — the inbound (client->agent) structs
// acp.go/host.go/map.go never needed, since nothing wired a server loop
// until milk serve --acp. Field names and shapes are verified against the
// upstream agentclientprotocol/agent-client-protocol schema/v2/schema.json,
// not guessed from this design's own prose tables.
//
// Scope note: SessionCapabilities{} is a monolithic baseline per the
// upstream schema itself ("supplying {} means the agent supports... session/
// new, session/list, session/resume, session/close, session/prompt,
// session/cancel, and session/update") — there is no finer-grained capability
// flag to advertise only the subset milk serve --acp actually implements
// this round (new/prompt/cancel/update). Advertising the baseline is required
// to turn those four on at all; session/list|resume|close calls from a real
// client get the standard JSON-RPC "method not found" error, which is the
// correct way to express "not implemented yet" here, not a capability lie.
package acp

import "encoding/json"

// Implementation describes the name/version of a client or agent (initialize
// request/response).
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

// AgentCapabilities is what InitializeResponse advertises. Session nil means
// no session/* support at all; see the package doc for why milk sets it to a
// non-nil empty SessionCapabilities despite only implementing a subset of
// its monolithic baseline.
type AgentCapabilities struct {
	Session *SessionCapabilities `json:"session,omitempty"`
	// Meta advertises milk's custom notification methods (ACP's sanctioned
	// way to negotiate custom features: _meta in capability objects).
	Meta map[string]any `json:"_meta,omitempty"`
}

// SessionCapabilities carries the schema's optional capability flags. Zero
// value = the monolithic baseline per the upstream schema itself ("supplying
// {} means the agent supports... session/new, session/list, session/resume,
// session/close, session/prompt, session/cancel, and session/update") — there
// is no finer-grained capability flag to advertise only a subset of that
// baseline. Delete is the one add-on flag: set to &SessionDeleteCapabilities{}
// to advertise session/delete ("only available if the Agent supports the
// session.delete capability").
type SessionCapabilities struct {
	Delete *SessionDeleteCapabilities `json:"delete,omitempty"`
}

// ClientCapabilities is what InitializeRequest.Capabilities carries. milk
// now acts on elicitation (the /config init wizard's form dialogs — see
// FormElicitation and cmd/milk/acp_initwizard.go) and on nothing else yet;
// the maps are kept as typed fields so a real client's request still
// decodes cleanly instead of erroring on an unrecognized shape.
type ClientCapabilities struct {
	Auth        map[string]any `json:"auth,omitempty"`
	Elicitation map[string]any `json:"elicitation,omitempty"`
}

// FormElicitation reports whether the client advertised form-mode
// elicitation support. Per the spec this is a presence check, not truthiness:
// an omitted or null `elicitation.form` means no support, while an explicitly
// supplied capability object — even the empty one — advertises it.
func (c ClientCapabilities) FormElicitation() bool {
	v, ok := c.Elicitation["form"]
	return ok && v != nil
}

// InitializeRequest is the client->agent initialize method's params. Field
// spellings vary across the protocol's versions: the released schema names
// the client fields `clientInfo`/`clientCapabilities`, while v2-draft builds
// use `info`/`capabilities` (the spellings Marshal emits, pinned by
// TestLifecycleWireShapes). UnmarshalJSON accepts either — released names
// win when both are present — since reading clientCapabilities is
// load-bearing now: the setup wizard must know whether the client supports
// elicitation before it may request a form.
type InitializeRequest struct {
	ProtocolVersion int                `json:"protocolVersion"`
	Info            Implementation     `json:"info"`
	Capabilities    ClientCapabilities `json:"capabilities"`
}

// UnmarshalJSON decodes both initialize field-name variants — see
// InitializeRequest's doc comment.
func (r *InitializeRequest) UnmarshalJSON(b []byte) error {
	type wire struct {
		ProtocolVersion int                `json:"protocolVersion"`
		Info            Implementation     `json:"clientInfo"`
		InfoAlt         Implementation     `json:"info"`
		Capabilities    ClientCapabilities `json:"clientCapabilities"`
		CapabilitiesAlt ClientCapabilities `json:"capabilities"`
	}
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	r.ProtocolVersion = w.ProtocolVersion
	r.Info = w.Info
	if r.Info == (Implementation{}) {
		r.Info = w.InfoAlt
	}
	r.Capabilities = w.Capabilities
	if r.Capabilities.Auth == nil && r.Capabilities.Elicitation == nil {
		r.Capabilities = w.CapabilitiesAlt
	}
	return nil
}

// InitializeResponse is the initialize method's result.
type InitializeResponse struct {
	ProtocolVersion int               `json:"protocolVersion"`
	Info            Implementation    `json:"info"`
	Capabilities    AgentCapabilities `json:"capabilities"`
}

// McpServerConfig is accepted-and-ignored this round (see package doc on
// NewSessionRequest) — milk does not yet merge client-supplied MCP servers
// with ~/.milk/config.json's. Kept as json.RawMessage so an arbitrary
// transport-shaped entry (http|stdio|other, per the upstream discriminated
// union) still decodes without error.
type McpServerConfig = map[string]any

// NewSessionRequest is session/new's params.
type NewSessionRequest struct {
	CWD                   string            `json:"cwd"`
	AdditionalDirectories []string          `json:"additionalDirectories,omitempty"`
	MCPServers            []McpServerConfig `json:"mcpServers,omitempty"`
}

// NewSessionResponse is session/new's result.
type NewSessionResponse struct {
	SessionID         SessionID             `json:"sessionId"`
	ConfigOptions     []SessionConfigOption `json:"configOptions,omitempty"`
	AvailableCommands []AvailableCommand    `json:"availableCommands,omitempty"`
}

// PromptRequest is session/prompt's params.
type PromptRequest struct {
	SessionID SessionID      `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// PromptResponse is session/prompt's result. It does NOT carry a stop
// reason — per the upstream schema, this response only acknowledges the
// prompt was accepted into the conversation; turn completion is reported via
// a state_update session/update notification (StateUpdate/IdleState in
// acp.go), which may arrive before or after this response.
type PromptResponse struct {
	MessageID MessageID `json:"messageId"`
}

// CancelSessionNotification is session/cancel's params (a notification, not
// a request — no response is expected for an interrupt signal).
type CancelSessionNotification struct {
	SessionID SessionID `json:"sessionId"`
}

// CancelRequestNotification is $/cancel_request's params — cancels an
// in-flight request (either direction) by its original id.
type CancelRequestNotification struct {
	RequestID any `json:"requestId"`
}
