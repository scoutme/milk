package acp

import (
	"context"
	"fmt"
	"sync"
)

// elicitation/create + elicitation/complete — ACP's structured user-input
// round trip, wired behind Host.Elicit: a form/select prompt with a typed
// result, no custom dialog protocol (design §7.1). No production caller
// exists yet (see Host.Elicit's doc comment and cmd/milk/host_acp.go).

// Elicitation actions reported by elicitation/create (open-set).
const (
	ElicitationAccept  = "accept"
	ElicitationDecline = "decline"
	ElicitationCancel  = "cancel"
)

// EnumOption is one titled choice in a single- or multi-select schema.
type EnumOption struct {
	Const string `json:"const"`
	Title string `json:"title"`
}

// MultiSelectItems describes a multi-select property's allowed values
// (MultiSelectItems union: plain string values or titled options).
type MultiSelectItems struct {
	Type  string       `json:"type"` // "string" (open-set)
	Enum  []string     `json:"enum,omitempty"`
	AnyOf []EnumOption `json:"anyOf,omitempty"`
}

// StringMultiSelect builds untitled multi-select items.
func StringMultiSelect(values []string) MultiSelectItems {
	return MultiSelectItems{Type: "string", Enum: values}
}

// TitledMultiSelect builds titled multi-select items.
func TitledMultiSelect(opts []EnumOption) MultiSelectItems {
	return MultiSelectItems{Type: "string", AnyOf: opts}
}

// ElicitationProperty is one form field (ElicitationPropertySchema union:
// string / number / integer / boolean / array-multi-select). Enum and OneOf
// are mutually exclusive single-select forms of the string type; Items turns
// the array type into a multi-select.
type ElicitationProperty struct {
	Type     string            `json:"type"` // "string" | "number" | "integer" | "boolean" | "array" (open-set)
	Title    string            `json:"title,omitempty"`
	Enum     []string          `json:"enum,omitempty"`
	OneOf    []EnumOption      `json:"oneOf,omitempty"`
	Items    *MultiSelectItems `json:"items,omitempty"`
	MinItems *int              `json:"minItems,omitempty"`
	MaxItems *int              `json:"maxItems,omitempty"`
	Default  any               `json:"default,omitempty"`
}

// SelectProperty builds a titled single-select string property.
func SelectProperty(title string, opts ...EnumOption) ElicitationProperty {
	return ElicitationProperty{Type: "string", Title: title, OneOf: opts}
}

// MultiSelectProperty builds a multi-select array property.
func MultiSelectProperty(title string, items MultiSelectItems) ElicitationProperty {
	return ElicitationProperty{Type: "array", Title: title, Items: &items}
}

// ElicitationSchema is the form schema the client renders
// (ElicitationSchema: type "object" with primitive property definitions).
type ElicitationSchema struct {
	Type        string                         `json:"type"` // "object"
	Title       string                         `json:"title,omitempty"`
	Description string                         `json:"description,omitempty"`
	Properties  map[string]ElicitationProperty `json:"properties"`
	Required    []string                       `json:"required,omitempty"`
}

// ElicitationIDText is the key under which ElicitSession carries milk's
// elicitation ID in CreateElicitationRequest._meta. Form-mode creates have no
// standard elicitationId field (url-mode does), and elicitation/complete
// needs one — `_meta` is the sanctioned place (ADR-0049 rule 1).
const ElicitationIDText = "milk/elicitation_id"

// CreateElicitationRequest is the elicitation/create params (form mode,
// session scope).
type CreateElicitationRequest struct {
	Message         string             `json:"message"`
	Mode            string             `json:"mode"` // "form"
	RequestedSchema *ElicitationSchema `json:"requestedSchema"`
	SessionID       SessionID          `json:"sessionId"`
	ToolCallID      *ToolCallID        `json:"toolCallId,omitempty"`
	Meta            map[string]any     `json:"_meta,omitempty"`
}

// CreateElicitationResponse is the elicitation/create result.
type CreateElicitationResponse struct {
	Action  string         `json:"action"` // "accept" | "decline" | "cancel" (open-set)
	Content map[string]any `json:"content,omitempty"`
}

// CompleteElicitationNotification is the elicitation/complete params: the
// agent telling the client an elicitation is over and its UI can go.
type CompleteElicitationNotification struct {
	ElicitationID ElicitationID  `json:"elicitationId"`
	Meta          map[string]any `json:"_meta,omitempty"`
}

// ElicitationRequest is the host-level structured-input prompt (design §8.1
// Host.Elicit input).
type ElicitationRequest struct {
	Message    string
	Schema     ElicitationSchema
	ToolCallID *ToolCallID // optional: scope the elicitation to one tool call
}

// ElicitationResult is the host-level structured-input answer.
type ElicitationResult struct {
	Action  string         // "accept" | "decline" | "cancel" (open-set)
	Content map[string]any // form values when accepted (ElicitationContentValue kinds)
}

// ElicitSession wires Host.Elicit over elicitation/create + elicitation/complete.
type ElicitSession struct {
	conn    Conn
	session SessionID

	mu   sync.Mutex
	next int
}

// NewElicitSession returns a Host.Elicit implementation that round-trips
// every request over conn for the given session.
func NewElicitSession(conn Conn, session SessionID) *ElicitSession {
	return &ElicitSession{conn: conn, session: session}
}

// Elicit sends elicitation/create (form mode, session scope), converts the
// answer, then sends elicitation/complete for the elicitation ID it allocated
// into the request's _meta.
func (e *ElicitSession) Elicit(ctx context.Context, req ElicitationRequest) (ElicitationResult, error) {
	if e.conn == nil {
		return ElicitationResult{Action: ElicitationCancel}, fmt.Errorf("acp: elicitation: no connection")
	}
	id := e.allocateID()
	schema := req.Schema
	params := CreateElicitationRequest{
		Message:         req.Message,
		Mode:            "form",
		RequestedSchema: &schema,
		SessionID:       e.session,
		ToolCallID:      req.ToolCallID,
		Meta:            map[string]any{ElicitationIDText: string(id)},
	}
	var resp CreateElicitationResponse
	if err := e.conn.Request(ctx, MethodElicitationCreate, params, &resp); err != nil {
		return ElicitationResult{Action: ElicitationCancel}, err
	}
	result := ElicitationResult{Action: resp.Action, Content: resp.Content}
	// The result has been handed to the caller: the elicitation is complete.
	// Fire-and-forget — a host that already tore the prompt down treats this
	// as a no-op (unknown Ext*/notification content must be ignored).
	_ = e.conn.Notify(MethodElicitationComplete, CompleteElicitationNotification{ElicitationID: id})
	return result, nil
}

func (e *ElicitSession) allocateID() ElicitationID {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.next++
	return ElicitationID(fmt.Sprintf("elicit-%d", e.next))
}
