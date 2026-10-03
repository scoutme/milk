package acp

import (
	"context"
	"encoding/json"
	"sync"
)

// fakeConn records the JSON-RPC traffic a mapper/host produces so tests can
// assert on the wire shape without a live stdio server.
type fakeConn struct {
	mu sync.Mutex

	notifications []Notification
	requests      []Request
	// respond generates the response body for a request (by method).
	respond func(method string, params any) (any, error)
}

func (f *fakeConn) Notify(method string, params any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifications = append(f.notifications, Notification{JSONRPC: "2.0", Method: method, Params: params})
	return nil
}

func (f *fakeConn) Request(ctx context.Context, method string, params any, result any) error {
	f.mu.Lock()
	f.requests = append(f.requests, Request{JSONRPC: "2.0", ID: len(f.requests) + 1, Method: method, Params: params})
	respond := f.respond
	f.mu.Unlock()
	if respond == nil {
		return nil
	}
	body, err := respond(method, params)
	if err != nil {
		return err
	}
	if result == nil || body == nil {
		return nil
	}
	// Round-trip through JSON so the unmarshalling path in the real client
	// is exercised too.
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, result)
}

// sent returns the recorded notifications (copy).
func (f *fakeConn) sent() []Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Notification(nil), f.notifications...)
}

// requested returns the recorded requests (copy).
func (f *fakeConn) requested() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// jsonBody marshals v to a generic map for field-level assertions.
func jsonBody(t interface{ Fatalf(string, ...any) }, v any) map[string]any {
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}
