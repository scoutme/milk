package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/scoutme/milk/internal/events"
	"github.com/scoutme/milk/internal/transport/acp"
)

// fakeACPConn mirrors internal/transport/acp's own conn_test.go fakeConn —
// duplicated locally since that type is unexported and this is its only
// caller in cmd/milk, not worth promoting to an exported test helper.
type fakeACPConn struct {
	mu            sync.Mutex
	notifications []acp.Notification
	respond       func(method string, params any) (any, error)
}

func (f *fakeACPConn) Notify(method string, params any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifications = append(f.notifications, acp.Notification{Method: method, Params: params})
	return nil
}

func (f *fakeACPConn) Request(ctx context.Context, method string, params any, result any) error {
	if f.respond == nil {
		return nil
	}
	body, err := f.respond(method, params)
	if err != nil {
		return err
	}
	if result == nil || body == nil {
		return nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, result)
}

func (f *fakeACPConn) sent() []acp.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]acp.Notification(nil), f.notifications...)
}

func TestACPHost_RequestPermission_Allow(t *testing.T) {
	conn := &fakeACPConn{respond: func(method string, params any) (any, error) {
		req := params.(acp.RequestPermissionRequest)
		if req.Title != "allow me?" {
			t.Errorf("Title = %q, want %q", req.Title, "allow me?")
		}
		if len(req.Options) != 2 {
			t.Fatalf("Options = %v, want 2 entries", req.Options)
		}
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Outcome: "selected", OptionID: acpPermAllowOptionID}}, nil
	}}
	host := newACPHost(conn, "sess-1")

	outcome, err := host.RequestPermission(context.Background(), events.PermissionRequest{Prompt: "allow me?"})
	if err != nil {
		t.Fatalf("RequestPermission: %v", err)
	}
	if !outcome.Allow {
		t.Error("Allow = false, want true")
	}
}

func TestACPHost_RequestPermission_Deny(t *testing.T) {
	conn := &fakeACPConn{respond: func(method string, params any) (any, error) {
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Outcome: "selected", OptionID: acpPermDenyOptionID}}, nil
	}}
	host := newACPHost(conn, "sess-1")

	outcome, err := host.RequestPermission(context.Background(), events.PermissionRequest{Prompt: "allow me?"})
	if err != nil {
		t.Fatalf("RequestPermission: %v", err)
	}
	if outcome.Allow {
		t.Error("Allow = true, want false")
	}
}

func TestACPHost_RequestPermission_Cancelled(t *testing.T) {
	conn := &fakeACPConn{respond: func(method string, params any) (any, error) {
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Outcome: "cancelled"}}, nil
	}}
	host := newACPHost(conn, "sess-1")

	outcome, err := host.RequestPermission(context.Background(), events.PermissionRequest{Prompt: "allow me?"})
	if err != nil {
		t.Fatalf("RequestPermission: %v", err)
	}
	if !outcome.Cancelled || outcome.Allow {
		t.Errorf("outcome = %+v, want Cancelled=true Allow=false", outcome)
	}
}

func TestACPHost_Notify(t *testing.T) {
	conn := &fakeACPConn{}
	host := newACPHost(conn, "sess-1")

	host.Notify(events.Notification{Text: "hello", CommandHint: "/foo"})

	sent := conn.sent()
	if len(sent) != 1 || sent[0].Method != acp.ExtMethodNotification {
		t.Fatalf("sent = %+v, want one milk/notification", sent)
	}
	payload, ok := sent[0].Params.(acp.NotificationPayload)
	if !ok || payload.Body != "hello" || payload.CommandHint != "/foo" {
		t.Errorf("payload = %+v, want Body=hello CommandHint=/foo", sent[0].Params)
	}
}

func TestACPHost_ElicitAndState_DoNotPanic(t *testing.T) {
	host := newACPHost(&fakeACPConn{}, "sess-1")

	result, err := host.Elicit(context.Background(), events.ElicitationRequest{Prompt: "pick one"})
	if err != nil {
		t.Fatalf("Elicit: %v", err)
	}
	if !result.Cancelled {
		t.Error("Elicit should report Cancelled until wired — see its doc comment")
	}

	host.State(events.StateUpdate{State: events.StateRunning}) // must be a safe no-op
}
