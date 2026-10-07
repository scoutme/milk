package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/transport/acp"
)

// acpServer dispatches incoming ACP JSON-RPC requests/notifications
// (acp.Handler) to per-session state. One per `milk serve --acp` process;
// conn is shared across every session, since ACP's wire framing has no
// per-session transport, only a sessionId field inside each method's
// params.
type acpServer struct {
	cfg  config.Config
	conn acp.Conn

	mu       sync.Mutex
	sessions map[acp.SessionID]*acpSession
	// clientProtocol is the protocolVersion the client sent in initialize
	// (0 if it never did). milk always answers with its own version; this
	// only selects the few wire shapes that differ for a v1 client (plans).
	clientProtocol int
	// clientCaps is what the client sent in initialize — read today for
	// elicitation.form, which decides whether /config init may drive its
	// wizard through form dialogs (acp_initwizard.go).
	clientCaps acp.ClientCapabilities
}

func newACPServer(cfg config.Config, conn acp.Conn) *acpServer {
	return &acpServer{cfg: cfg, conn: conn, sessions: map[acp.SessionID]*acpSession{}}
}

var _ acp.Handler = (*acpServer)(nil)

// HandleRequest implements acp.Handler. Only initialize/session/new/
// session/prompt are wired this round — see
// docs/machine-readable-output-design.md's ACP status note for the full
// deferred list (session/list|resume|delete|close, auth/*,
// session/set_config_option). Everything else gets the standard JSON-RPC
// "method not found" error, the correct way to express "not implemented
// yet" here (see MethodNotFoundError's doc comment). (elicitation is now
// wired the other way — the setup wizard sends elicitation/create to the
// client; see acp_initwizard.go.)
func (s *acpServer) HandleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return s.handleInitialize(params)
	case "session/new":
		return s.handleSessionNew(params)
	case "session/prompt":
		return s.handlePrompt(ctx, params)
	default:
		return nil, &acp.MethodNotFoundError{Method: method}
	}
}

// HandleNotification implements acp.Handler. Unrecognized notifications are
// silently ignored — JSON-RPC notifications have no response to carry an
// error back on, and ACP's own spec treats unknown notifications as safe to
// drop.
func (s *acpServer) HandleNotification(method string, params json.RawMessage) {
	switch method {
	case "session/cancel":
		s.handleCancel(params)
	}
}

// AfterResponse implements acp.PostResponder: once session/new's response is
// on the wire, advertise milk's slash commands for the new session.
func (s *acpServer) AfterResponse(method string, result any) {
	if method != "session/new" {
		return
	}
	resp, ok := result.(acp.NewSessionResponse)
	if !ok {
		return
	}
	if as := s.session(resp.SessionID); as != nil {
		as.notify(acp.NewAvailableCommandsUpdate(acpAdvertisedCommands()))
	}
}

func (s *acpServer) handleInitialize(params json.RawMessage) (any, error) {
	var req acp.InitializeRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	s.mu.Lock()
	s.clientProtocol = req.ProtocolVersion
	s.clientCaps = req.Capabilities
	s.mu.Unlock()
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersion,
		Info:            acp.Implementation{Name: "milk", Version: version},
		// Session capability is a monolithic baseline per the upstream
		// schema (new|list|resume|close|prompt|cancel|update) — see
		// lifecycle.go's package doc for why advertising it despite only
		// implementing a subset is correct, not a capability lie.
		Capabilities: acp.AgentCapabilities{
			Session: &acp.SessionCapabilities{},
			Meta: map[string]any{"milk": map[string]any{
				"notifications": []string{acp.ExtMethodRoute, acp.ExtMethodWarning},
			}},
		},
	}, nil
}

func (s *acpServer) handleSessionNew(params json.RawMessage) (any, error) {
	var req acp.NewSessionRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("session/new: %w", err)
	}
	if req.CWD == "" {
		return nil, fmt.Errorf("session/new: cwd is required")
	}

	sess, err := session.New(req.CWD, "")
	if err != nil {
		return nil, fmt.Errorf("session/new: %w", err)
	}
	id := acp.SessionID(sess.ID)

	// Re-read the config from disk: a serve process outlives any single
	// session, and /config init (or an external editor) may have rewritten
	// the config since startup — a session created afterwards must see it,
	// not the startup snapshot. Fall back to the startup config when the
	// file can't be read.
	cfg := s.cfg
	if fresh, err := config.LoadMerged(); err == nil {
		cfg = fresh
	}

	as, err := newACPSession(cfg, sess, s.conn, id)
	if err != nil {
		return nil, fmt.Errorf("session/new: %w", err)
	}

	s.mu.Lock()
	as.v1Client = s.clientProtocol == 1
	as.host.host.V1 = as.v1Client
	as.formElicit = s.clientCaps.FormElicitation()
	s.sessions[id] = as
	s.mu.Unlock()

	return acp.NewSessionResponse{SessionID: id}, nil
}

func (s *acpServer) handlePrompt(ctx context.Context, params json.RawMessage) (any, error) {
	var req acp.PromptRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("session/prompt: %w", err)
	}
	as := s.session(req.SessionID)
	if as == nil {
		return nil, fmt.Errorf("session/prompt: unknown session %q", req.SessionID)
	}
	return as.prompt(ctx, req)
}

func (s *acpServer) handleCancel(params json.RawMessage) {
	var n acp.CancelSessionNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	if as := s.session(n.SessionID); as != nil {
		as.mu.Lock()
		cancel := as.cancel
		as.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
}

func (s *acpServer) session(id acp.SessionID) *acpSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}
