package acp

import "context"

// Host presents milk's interactive surfaces to the turn loop (design §8.1):
// one interface, implemented by three hosts — the TUI host
// (cmd/milk/host_tui.go), the ACP host (this package: ElicitSession for
// elicitation/create + elicitation/complete, the serve loop for
// session/request_permission), and the headless batch host (flags decide
// without round-trips). Nothing above this interface may assume which host is
// attached: permission prompts, user input and notifications are host
// concerns — exactly the Phase 3 elicitation wiring's "behind Host.Elicit".
type Host interface {
	// Notify surfaces a turn-unrelated notification: an ExtNotification
	// (milk/notification|warning|memory|route) rendered by the host as
	// toasts/status-bar badges/panel feeds.
	Notify(ExtNotification)
	// RequestPermission runs a structured permission prompt
	// (session/request_permission; ADR-0013/0015).
	RequestPermission(ctx context.Context, req PermissionRequest) (PermissionOutcome, error)
	// Elicit runs a structured user-input round trip
	// (elicitation/create → elicitation/complete): form/select prompts.
	Elicit(ctx context.Context, req ElicitationRequest) (ElicitationResult, error)
	// State reports session state changes (state_update:
	// running|idle|requires_action).
	State(StateUpdate)
}

// PermissionOptionKind is the ACP PermissionOptionKind enum (open-set).
type PermissionOptionKind string

const (
	PermissionAllowOnce    PermissionOptionKind = "allow_once"
	PermissionAllowAlways  PermissionOptionKind = "allow_always"
	PermissionRejectOnce   PermissionOptionKind = "reject_once"
	PermissionRejectAlways PermissionOptionKind = "reject_always"
)

// PermissionOption is one choice offered by a permission prompt.
type PermissionOption struct {
	OptionID string               `json:"optionId"`
	Name     string               `json:"name"`
	Kind     PermissionOptionKind `json:"kind"`
}

// PermissionRequest is the host-level permission prompt
// (session/request_permission): maps field-for-field onto milk's structured
// permission records.
type PermissionRequest struct {
	SessionID SessionID          `json:"sessionId,omitempty"`
	Title     string             `json:"title"`
	Subject   string             `json:"subject,omitempty"` // tool call or command name
	Options   []PermissionOption `json:"options"`
}

// PermissionOutcome is the user's answer: the selected option ID, or
// Cancelled when the prompt was dismissed.
type PermissionOutcome struct {
	Cancelled bool
	OptionID  string
}

// Allow reports whether the selected option's kind is an allow kind.
func (o PermissionOutcome) Allow(options []PermissionOption) bool {
	for _, opt := range options {
		if opt.OptionID == o.OptionID {
			return opt.Kind == PermissionAllowOnce || opt.Kind == PermissionAllowAlways
		}
	}
	return false
}

// RequestPermissionRequest is the session/request_permission params
// (agent→client).
type RequestPermissionRequest struct {
	SessionID SessionID          `json:"sessionId"`
	Title     string             `json:"title"`
	Subject   string             `json:"subject,omitempty"`
	Options   []PermissionOption `json:"options"`
	Meta      map[string]any     `json:"_meta,omitempty"`
}

// RequestPermissionOutcome is the outcome variant of
// RequestPermissionResponse (open-set: "selected" | "cancelled").
type RequestPermissionOutcome struct {
	Outcome  string `json:"outcome"` // "selected" | "cancelled" (open-set)
	OptionID string `json:"optionId,omitempty"`
}

// RequestPermissionResponse is the session/request_permission result.
type RequestPermissionResponse struct {
	Outcome RequestPermissionOutcome `json:"outcome"`
	Meta    map[string]any           `json:"_meta,omitempty"`
}

// ACPHost implements Host over an ACP connection: notifications ride their
// Ext* methods (or session/update for state), permission prompts round-trip
// session/request_permission, and elicitation runs through ElicitSession
// (elicitation/create + elicitation/complete behind Host.Elicit).
type ACPHost struct {
	Conn    Conn
	Session SessionID
	elicit  *ElicitSession
}

// NewACPHost returns the Host implementation the serve loop hands to the turn
// loop when an ACP client is attached.
func NewACPHost(conn Conn, session SessionID) *ACPHost {
	return &ACPHost{Conn: conn, Session: session, elicit: NewElicitSession(conn, session)}
}

var _ Host = (*ACPHost)(nil)

// Notify implements Host.
func (h *ACPHost) Notify(n ExtNotification) {
	_ = h.Conn.Notify(n.Method, n.Params)
}

// RequestPermission implements Host over session/request_permission.
func (h *ACPHost) RequestPermission(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
	params := RequestPermissionRequest{
		SessionID: h.Session,
		Title:     req.Title,
		Subject:   req.Subject,
		Options:   req.Options,
	}
	if req.SessionID != "" {
		params.SessionID = req.SessionID
	}
	var resp RequestPermissionResponse
	if err := h.Conn.Request(ctx, MethodRequestPermission, params, &resp); err != nil {
		return PermissionOutcome{Cancelled: true}, err
	}
	if resp.Outcome.Outcome == "selected" {
		return PermissionOutcome{OptionID: resp.Outcome.OptionID}, nil
	}
	return PermissionOutcome{Cancelled: true}, nil
}

// Elicit implements Host (structured user-input form/select prompts).
func (h *ACPHost) Elicit(ctx context.Context, req ElicitationRequest) (ElicitationResult, error) {
	return h.elicit.Elicit(ctx, req)
}

// State implements Host (state_update session notification).
func (h *ACPHost) State(u StateUpdate) {
	_ = h.Conn.Notify(MethodSessionUpdate, UpdateSessionNotification{SessionID: h.Session, Update: u})
}
