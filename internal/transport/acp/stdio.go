package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// Handler dispatches incoming client->agent JSON-RPC requests and
// notifications. Implementations live in cmd/milk (acpServer) — this
// package stays generic, with no milk-turn-loop knowledge.
type Handler interface {
	// HandleRequest handles an incoming request and returns its result (or
	// an error, mapped to a JSON-RPC error response). Called on its own
	// goroutine per request — a long-running request (session/prompt, held
	// open for the whole turn per the ACP spec) never blocks the read loop
	// or any other session's requests.
	HandleRequest(ctx context.Context, method string, params json.RawMessage) (result any, err error)
	// HandleNotification handles an incoming notification (no response).
	// Also called on its own goroutine, for the same reason.
	HandleNotification(method string, params json.RawMessage)
}

// PostResponder is an optional Handler extension: AfterResponse is called
// after a successful request's response has been written, so a handler can
// send notifications the client can only correlate once it has the response
// (e.g. session/update for a sessionId returned by session/new).
type PostResponder interface {
	AfterResponse(method string, result any)
}

// wireMessage is the superset shape of every JSON-RPC message read off the
// wire: a request/notification (Method set, ID set or unset) or a response
// to one of our own outbound Request calls (Method unset, ID set, Result or
// Error set). ID stays raw bytes rather than a decoded Go value so it can
// always be echoed back byte-for-byte (JSON-RPC permits string or number
// ids; decoding then re-encoding a number can silently reformat it) and
// cheaply compared for pending-request correlation.
type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// StdioConn is a JSON-RPC 2.0 Conn plus the incoming-message dispatch loop,
// over any io.Writer (named for its production use over os.Stdout, but
// transport-agnostic — Serve takes its own io.Reader so tests can wire two
// instances together over an io.Pipe instead of real stdio). One per server
// process; every ACP session shares it, since ACP's wire framing has no
// per-session transport, only a sessionId field inside each method's params.
type StdioConn struct {
	w       io.Writer
	writeMu sync.Mutex

	handler Handler

	pendingMu sync.Mutex
	pending   map[string]chan wireMessage
	nextID    atomic.Int64
}

// NewStdioConn returns a connection writing to w and dispatching incoming
// requests/notifications to h. Call Serve to start reading.
func NewStdioConn(w io.Writer, h Handler) *StdioConn {
	return &StdioConn{w: w, handler: h, pending: map[string]chan wireMessage{}}
}

// Serve reads newline-framed JSON-RPC messages from r until r is exhausted,
// ctx is cancelled, or a scan error occurs. Mirrors
// internal/agent/claude/stream.go's scanLines pattern: the scanner runs in
// its own goroutine piping lines into a buffered channel, so the pipe is
// always drained even while a handler blocks — critical here, since a
// session/prompt handler blocks for an entire turn and every other
// session's traffic must keep flowing on the same reader meanwhile.
func (c *StdioConn) Serve(ctx context.Context, r io.Reader) error {
	lines := make(chan []byte, 64)
	scanErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // large tool-call payloads need headroom past the default 64KB token limit
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...) // Scanner reuses its internal buffer
			lines <- line
		}
		scanErr <- sc.Err()
		close(lines)
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				return <-scanErr
			}
			if len(line) == 0 {
				continue
			}
			c.handleLine(ctx, line)
		}
	}
}

func (c *StdioConn) handleLine(ctx context.Context, line []byte) {
	var msg wireMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return // malformed line: not meaningfully recoverable, drop it
	}
	switch {
	case msg.Method != "" && len(msg.ID) > 0:
		go c.handleRequest(ctx, msg)
	case msg.Method != "":
		go c.handler.HandleNotification(msg.Method, msg.Params)
	default:
		c.resolvePending(msg)
	}
}

func (c *StdioConn) handleRequest(ctx context.Context, msg wireMessage) {
	result, err := c.handler.HandleRequest(ctx, msg.Method, msg.Params)
	resp := Response{JSONRPC: "2.0", ID: json.RawMessage(msg.ID)}
	if err != nil {
		code := -32000
		var coder interface{ RPCCode() int }
		var mnfe *MethodNotFoundError
		switch {
		case errors.As(err, &coder):
			code = coder.RPCCode() // deliberate code chosen by the handler (see CodedError)
		case errors.As(err, &mnfe):
			code = -32601 // JSON-RPC 2.0 reserved "method not found"
		}
		resp.Error = &RPCError{Code: code, Message: err.Error()}
	} else {
		resp.Result = result
	}
	werr := c.write(resp)
	if err == nil && werr == nil {
		if pr, ok := c.handler.(PostResponder); ok {
			pr.AfterResponse(msg.Method, result)
		}
	}
}

// MethodNotFoundError signals a method a Handler doesn't implement — mapped
// to JSON-RPC's reserved -32601 code by handleRequest above. The standard way
// for milk serve --acp to express "not implemented yet" for a method nothing
// wires up (e.g. the legacy v1 session/load), as opposed to CodedError, which
// carries deliberate codes for methods that ARE wired but reject the request.
type MethodNotFoundError struct{ Method string }

func (e *MethodNotFoundError) Error() string { return "method not found: " + e.Method }

// CodedError is a handler error carrying its own deliberate JSON-RPC error
// code (e.g. CodeResourceNotFound / CodeInvalidParams from sessions.go).
// handleRequest honors RPCCode instead of falling back to the generic -32000.
type CodedError struct {
	Code    int
	Message string
}

func (e *CodedError) Error() string { return e.Message }
func (e *CodedError) RPCCode() int  { return e.Code }

func (c *StdioConn) resolvePending(msg wireMessage) {
	key := string(msg.ID)
	c.pendingMu.Lock()
	ch := c.pending[key]
	delete(c.pending, key)
	c.pendingMu.Unlock()
	if ch != nil {
		ch <- msg
	}
}

// Notify sends a JSON-RPC notification (no id, no response expected).
func (c *StdioConn) Notify(method string, params any) error {
	return c.write(Notification{JSONRPC: "2.0", Method: method, Params: params})
}

// Request sends a JSON-RPC request and blocks until the matching response
// arrives (or ctx is cancelled), unmarshaling its result into result (which
// may be nil to ignore the body). Satisfies the Conn interface ACPHost
// already depends on.
func (c *StdioConn) Request(ctx context.Context, method string, params any, result any) error {
	idJSON, _ := json.Marshal(fmt.Sprintf("%d", c.nextID.Add(1))) //nolint:errcheck // encoding a string never fails
	// key must match resolvePending's string(msg.ID) exactly — msg.ID holds
	// the raw wire bytes of the id (including the JSON quotes), not a
	// decoded Go string, so the map key here must be the same raw bytes.
	key := string(idJSON)
	ch := make(chan wireMessage, 1)
	c.pendingMu.Lock()
	c.pending[key] = ch
	c.pendingMu.Unlock()

	if err := c.write(Request{JSONRPC: "2.0", ID: json.RawMessage(idJSON), Method: method, Params: params}); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, key)
		c.pendingMu.Unlock()
		return err
	}

	select {
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, key)
		c.pendingMu.Unlock()
		return ctx.Err()
	case msg := <-ch:
		if msg.Error != nil {
			return fmt.Errorf("acp: %s: %s", method, msg.Error.Message)
		}
		if result == nil || len(msg.Result) == 0 {
			return nil
		}
		return json.Unmarshal(msg.Result, result)
	}
}

func (c *StdioConn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.w.Write(b)
	return err
}

var _ Conn = (*StdioConn)(nil)
