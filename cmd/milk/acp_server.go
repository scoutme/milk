package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/transport/acp"
	"github.com/scoutme/milk/internal/updater"
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
	// closedIDs remembers the sessions session/close or session/delete tore
	// down this process (D2.4): a thread the user explicitly closed must not
	// silently resurrect on the next session/new. Guarded by mu.
	closedIDs map[acp.SessionID]bool
	// clientProtocol is the protocolVersion the client sent in initialize
	// (0 if it never did). milk always answers with its own version; this
	// only selects the few wire shapes that differ for a v1 client (plans).
	clientProtocol int
	// clientCaps is what the client sent in initialize — read today for
	// elicitation.form, which decides whether /config init may drive its
	// wizard through form dialogs (acp_initwizard.go).
	clientCaps acp.ClientCapabilities

	// Shared self-update state (/update …, the startup check): one release
	// cache and one in-flight guard per serve process, not per session.
	// Guarded by mu.
	updateRelease    *updater.Release // last known available release (nil: none known)
	updateInstalled  string           // tag applied this process — pending restart
	updateInstalling bool             // /update install is running

	// Remote oversight (Telegram): one notifier per serve process — created
	// by startOversight at serve start, rebuilt by /setup telegram on|off.
	// notifierStop cancels the current notifier's polling loop on rebuild.
	// remoteQueue holds messages that arrived with no live session yet.
	// All guarded by mu. See acp_oversight.go.
	notifier     oversight.Notifier
	notifierStop context.CancelFunc
	serveCtx     context.Context
	remoteQueue  []string
}

func newACPServer(cfg config.Config, conn acp.Conn) *acpServer {
	return &acpServer{cfg: cfg, conn: conn, sessions: map[acp.SessionID]*acpSession{}, closedIDs: map[acp.SessionID]bool{}}
}

var _ acp.Handler = (*acpServer)(nil)

// HandleRequest implements acp.Handler. Wired: initialize, session/new,
// session/list, session/resume, session/close, session/delete,
// session/prompt. Everything else gets the standard JSON-RPC "method not
// found" error — the correct way to express "not implemented yet" here (see
// MethodNotFoundError's doc comment); the remaining deferred surface
// (auth/*, the legacy v1 session/load, session/set_config_option) is listed
// in docs/acp-integration.md's known gaps. (elicitation is wired the other
// way — the setup wizard sends elicitation/create to the client; see
// acp_initwizard.go.)
func (s *acpServer) HandleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return s.handleInitialize(params)
	case "session/new":
		return s.handleSessionNew(params)
	case "session/list":
		return s.handleSessionList(params)
	case "session/resume":
		return s.handleSessionResume(params)
	case "session/close":
		return s.handleSessionClose(params)
	case "session/delete":
		return s.handleSessionDelete(params)
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

// AfterResponse implements acp.PostResponder: once a session/new or
// session/resume response is on the wire, tell that session about anything it
// may have missed while the request was in flight — milk's slash commands
// (session/new carries them out-of-band here; session/resume already returned
// them in-band in ResumeSessionResponse) and an available update release
// (once per session — announceUpdateTo).
func (s *acpServer) AfterResponse(method string, result any) {
	switch method {
	case "session/new":
		resp, ok := result.(acp.NewSessionResponse)
		if !ok {
			return
		}
		if as := s.session(resp.SessionID); as != nil {
			as.notify(acp.NewAvailableCommandsUpdate(acpAdvertisedCommands()))
			// A session created after the startup check completed hears about
			// the release now — once per session (announceUpdateTo).
			s.announceUpdateTo(as)
			// Telegram messages that arrived before any session existed run
			// on this one (acp_oversight.go).
			s.drainRemoteQueue(as)
		}
	case "session/resume":
		resp, ok := result.(resumeResult)
		if !ok {
			return
		}
		if as := s.session(resp.sessionID); as != nil {
			s.announceUpdateTo(as)
			s.drainRemoteQueue(as)
		}
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
		// schema (new|list|resume|close|prompt|cancel|update) — now fully
		// implemented (see lifecycle.go's package doc) — plus the add-on
		// `delete` flag advertising session/delete.
		Capabilities: acp.AgentCapabilities{
			Session: &acp.SessionCapabilities{Delete: &acp.SessionDeleteCapabilities{}},
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

	// Resume-by-default adoption (docs/acp-session-resume-plan.md D2): when
	// the request is clearly the continuation case, session/new continues
	// the cwd's most recent conversation instead of starting empty — the
	// TUI's --continue default mapped onto the entry point clients actually
	// call. See adoptCandidate for the full five-condition rule.
	if sess := s.adoptCandidate(req); sess != nil {
		as, err := s.registerACPSession(sess)
		if err != nil {
			return nil, fmt.Errorf("session/new: %w", err)
		}
		// Adoption never hides state: mandatory bounded replay (the client's
		// panel is empty) plus a one-line notice pointing at /export.
		as.replayHistory()
		as.notify(acp.AgentMessageChunk(as.liveID("resume"), fmt.Sprintf(
			"resumed session %.8s — %d earlier turns; /export prints the full transcript",
			sess.ID, len(sess.History))))
		return acp.NewSessionResponse{SessionID: as.id}, nil
	}

	sess, err := session.New(req.CWD, "")
	if err != nil {
		return nil, fmt.Errorf("session/new: %w", err)
	}
	as, err := s.registerACPSession(sess)
	if err != nil {
		return nil, fmt.Errorf("session/new: %w", err)
	}
	return acp.NewSessionResponse{SessionID: as.id}, nil
}

// adoptCandidate returns the stored session session/new should adopt, or nil
// to start fresh. Full adoption rule (docs/acp-session-resume-plan.md D2):
// adopt the most-recent stored session for req.cwd iff
//  1. config acp_resume is on (default on),
//  2. the request's _meta.milk.fresh is not true,
//  3. this process has no live session whose cwd is req.cwd,
//  4. the candidate was not closed earlier in this process, and
//  5. the store holds at least one session for req.cwd.
//
// Anything else behaves exactly as session/new always did — conditions 3+4
// preserve "new thread" inside a running client and stop an explicitly closed
// thread from resurrecting; only the first open of a workspace adopts.
func (s *acpServer) adoptCandidate(req acp.NewSessionRequest) *session.Session {
	if !s.currentConfig().ACPResumeEnabled() { // 1
		return nil
	}
	if milkMeta, _ := req.Meta["milk"].(map[string]any); milkMeta["fresh"] == true { // 2
		return nil
	}
	s.mu.Lock()
	for _, as := range s.sessions {
		if as.sess.CWD == req.CWD { // 3
			s.mu.Unlock()
			return nil
		}
	}
	s.mu.Unlock()

	byCWD, err := session.List(req.CWD) // 5
	if err != nil {
		return nil // store trouble: fresh beats failing session/new
	}
	entries := byCWD[req.CWD]
	if len(entries) == 0 {
		return nil
	}
	cand := entries[0] // sorted LastUsed desc — the most recent

	s.mu.Lock()
	closed := s.closedIDs[acp.SessionID(cand.ID)] // 4
	s.mu.Unlock()
	if closed {
		return nil
	}
	sess, err := session.Load(cand.ID)
	if err != nil {
		return nil
	}
	return sess
}

// currentConfig returns the freshest usable config: re-read from disk — a
// serve process outlives any single session, and /config init (or an external
// editor) may have rewritten the config since startup, so a decision or
// session created afterwards must see it, not the startup snapshot. Falls
// back to the startup config when the file can't be read.
func (s *acpServer) currentConfig() config.Config {
	if fresh, err := config.LoadMerged(); err == nil {
		return fresh
	}
	return s.cfg
}

// registerACPSession builds an acpSession around sess (fresh or loaded) and
// wires it into the server — the shared tail of session/new, session/resume
// and resume-by-default adoption (docs/acp-session-resume-plan.md §3.2).
func (s *acpServer) registerACPSession(sess *session.Session) (*acpSession, error) {
	cfg := s.currentConfig()

	as, err := newACPSession(cfg, sess, s.conn, acp.SessionID(sess.ID))
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	as.v1Client = s.clientProtocol == 1
	as.host.host.V1 = as.v1Client
	as.formElicit = s.clientCaps.FormElicitation()
	as.srv = s
	s.sessions[as.id] = as
	s.mu.Unlock()

	return as, nil
}

// resumeResult pairs session/resume's wire response with the session it
// resumed, so AfterResponse can correlate (ResumeSessionResponse carries no
// sessionId — the client already knows it). Marshals exactly as
// ResumeSessionResponse: the embedded exported struct's fields promote and
// the unexported field is invisible to encoding/json.
type resumeResult struct {
	acp.ResumeSessionResponse
	sessionID acp.SessionID
}

// sessionListPageSize is the session/list page size (docs/acp-session-resume-plan.md D4).
const sessionListPageSize = 50

// handleSessionList implements session/list (D4): every stored session,
// flattened and sorted by last use descending, filtered by cwd when the
// request names one, paged through an opaque base64("o:<n>") offset cursor.
// Title is the session name, else its first user turn truncated, else empty.
func (s *acpServer) handleSessionList(params json.RawMessage) (any, error) {
	var req acp.ListSessionsRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, invalidParams("session/list", err)
	}
	offset := 0
	if req.Cursor != nil {
		n, err := decodeSessionCursor(*req.Cursor)
		if err != nil {
			return nil, err
		}
		offset = n
	}

	byCWD, err := session.List("")
	if err != nil {
		return nil, fmt.Errorf("session/list: %w", err)
	}
	type flat struct {
		id    string
		cwd   string
		entry session.IndexEntry
	}
	var all []flat
	for cwd, entries := range byCWD {
		if req.CWD != "" && cwd != req.CWD {
			continue
		}
		for _, e := range entries {
			all = append(all, flat{id: e.ID, cwd: cwd, entry: e})
		}
	}
	// Per-cwd index order is LastUsed desc already; re-sort the flattened
	// slice so mixed-cwd listings keep the same order.
	sort.SliceStable(all, func(a, b int) bool {
		return all[a].entry.LastUsed.After(all[b].entry.LastUsed)
	})
	if offset > len(all) {
		offset = len(all)
	}

	end := offset + sessionListPageSize
	if end > len(all) {
		end = len(all)
	}
	page := all[offset:end]

	out := acp.ListSessionsResponse{Sessions: make([]acp.SessionInfo, 0, len(page))}
	for _, f := range page {
		info := acp.SessionInfo{
			SessionID: acp.SessionID(f.id),
			CWD:       f.cwd,
			Title:     f.entry.Name,
			UpdatedAt: f.entry.LastUsed.Format(time.RFC3339),
		}
		if info.Title == "" {
			if sess, err := session.Load(f.id); err == nil {
				info.Title = sessionTitle(sess)
			}
		}
		out.Sessions = append(out.Sessions, info)
	}
	if end < len(all) {
		out.NextCursor = encodeSessionCursor(end)
	}
	return out, nil
}

// sessionTitle is the fallback SessionInfo title: the first user turn,
// whitespace-collapsed and truncated (~60 chars), else empty.
func sessionTitle(sess *session.Session) string {
	for _, t := range sess.History {
		if t.Role != session.RoleUser {
			continue
		}
		title := strings.Join(strings.Fields(t.Content), " ")
		r := []rune(title)
		if len(r) > 60 {
			title = string(r[:60]) + "…"
		}
		return title
	}
	return ""
}

// encodeSessionCursor packs a page offset into the opaque wire cursor.
func encodeSessionCursor(offset int) *acp.SessionListCursor {
	c := acp.SessionListCursor(base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("o:%d", offset))))
	return &c
}

// decodeSessionCursor unpacks the opaque wire cursor; anything milk didn't
// write is an invalid-params error, never a guess.
func decodeSessionCursor(c acp.SessionListCursor) (int, error) {
	invalid := &acp.CodedError{Code: acp.CodeInvalidParams, Message: "session/list: invalid cursor"}
	raw, err := base64.StdEncoding.DecodeString(string(c))
	if err != nil {
		return 0, invalid
	}
	n, ok := strings.CutPrefix(string(raw), "o:")
	if !ok {
		return 0, invalid
	}
	offset, err := strconv.Atoi(n)
	if err != nil || offset < 0 {
		return 0, invalid
	}
	return offset, nil
}

// handleSessionResume implements session/resume (D1): attach the stored
// session (idempotent reattach when it is already open in this process),
// honoring the replayFrom cursor. Unknown session → -32002; cwd mismatch or
// an unknown replay cursor → -32602, the latter rejected before anything is
// emitted ("reject the request rather than guessing where to replay from").
func (s *acpServer) handleSessionResume(params json.RawMessage) (any, error) {
	var req acp.ResumeSessionRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, invalidParams("session/resume", err)
	}
	cursor, err := acp.ParseReplayFrom(req.ReplayFrom)
	if err != nil {
		return nil, invalidParams("session/resume", err)
	}

	sess, err := session.Load(string(req.SessionID))
	if err != nil {
		return nil, &acp.CodedError{Code: acp.CodeResourceNotFound,
			Message: fmt.Sprintf("session/resume: unknown session %q", req.SessionID)}
	}
	if req.CWD != sess.CWD {
		return nil, &acp.CodedError{Code: acp.CodeInvalidParams,
			Message: fmt.Sprintf("session/resume: cwd %q does not match the session's cwd %q", req.CWD, sess.CWD)}
	}

	// Already attached in this process (the client re-issued resume):
	// idempotent reattach — never rebuild, that would clobber a possibly
	// running turn. replayFrom is still honored (replay reads sess.History).
	as := s.session(req.SessionID)
	if as == nil {
		if as, err = s.registerACPSession(sess); err != nil {
			return nil, fmt.Errorf("session/resume: %w", err)
		}
	}
	if cursor != nil { // {"type":"start"} — the only cursor ParseReplayFrom accepts
		as.replayHistory()
	}
	return resumeResult{ResumeSessionResponse: acp.ResumeSessionResponse{
		AvailableCommands: acpAdvertisedCommands(),
	}, sessionID: as.id}, nil
}

// handleSessionClose implements session/close (D3): cancel any running turn,
// drop pending background follow-ups, persist the session file, and free the
// in-process resources. The file is kept — close ≠ delete. Idempotent:
// closing an unknown or already-closed session returns {} and records the ID
// in the closed-set (D2.4) all the same.
func (s *acpServer) handleSessionClose(params json.RawMessage) (any, error) {
	var req acp.CloseSessionRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, invalidParams("session/close", err)
	}
	s.closeSession(req.SessionID)
	return acp.CloseSessionResponse{}, nil
}

// closeSession is the teardown shared by session/close and session/delete.
func (s *acpServer) closeSession(id acp.SessionID) {
	s.mu.Lock()
	as := s.sessions[id]
	delete(s.sessions, id)
	s.closedIDs[id] = true
	s.mu.Unlock()
	if as != nil {
		as.close()
	}
}

// handleSessionDelete implements session/delete (D3): close the session if it
// is open (same teardown as session/close), then drop its file and index
// entry. Unknown session → -32002. Accepts an unambiguous ID prefix like
// /export session does (delete-by-prefix consistency).
func (s *acpServer) handleSessionDelete(params json.RawMessage) (any, error) {
	var req acp.DeleteSessionRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, invalidParams("session/delete", err)
	}
	sess, err := session.Lookup(string(req.SessionID))
	if err != nil {
		return nil, &acp.CodedError{Code: acp.CodeResourceNotFound, Message: "session/delete: " + err.Error()}
	}
	s.closeSession(acp.SessionID(sess.ID))
	if err := session.Drop(sess.ID, sess.CWD); err != nil {
		return nil, fmt.Errorf("session/delete: %w", err)
	}
	return acp.DeleteSessionResponse{}, nil
}

// invalidParams wraps a validation failure in the JSON-RPC invalid-params
// code (-32602).
func invalidParams(method string, err error) error {
	return &acp.CodedError{Code: acp.CodeInvalidParams, Message: method + ": " + err.Error()}
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
