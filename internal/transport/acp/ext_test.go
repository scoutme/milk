package acp

import (
	"encoding/json"
	"testing"
)

// ExtNotification mapping (ext.go): milk/notification|warning|memory|route.
// Each carries its kind payload under the sanctioned Ext* envelope.

func TestExtNotificationKinds(t *testing.T) {
	cases := []struct {
		name   string
		ext    ExtNotification
		method string
	}{
		{"notification", NotificationExt(NotificationPayload{ID: "t1", Severity: "info", CommandHint: "/notifications", Body: "connected"}), ExtMethodNotification},
		{"warning", WarningExt(WarningPayload{Category: "token_velocity", Consumption: true, Count: 9, Limit: 10, Message: "burning fast"}), ExtMethodWarning},
		{"memory", MemoryExt(MemoryPayload{Op: "record", PerceptID: "p1", Subject: "wire contract"}), ExtMethodMemory},
		{"route", RouteExt(RoutePayload{Target: "escalation", Reason: "router", Conclusive: true}), ExtMethodRoute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ext.Method != tc.method {
				t.Fatalf("method = %q, want %q", tc.ext.Method, tc.method)
			}
			body := jsonBody(t, tc.ext)
			if body["method"] != tc.method {
				t.Fatalf("wire method = %v, want %q", body["method"], tc.method)
			}
			params, ok := body["params"].(map[string]any)
			if !ok {
				t.Fatalf("params missing: %v", body)
			}
			// snake_case payload keys per §8.3 (milk's own keys).
			switch tc.name {
			case "notification":
				for _, key := range []string{"id", "severity", "command_hint", "body"} {
					if _, ok := params[key]; !ok {
						t.Errorf("notification payload missing %q: %v", key, params)
					}
				}
			case "warning":
				for _, key := range []string{"category", "consumption", "count", "limit", "message"} {
					if _, ok := params[key]; !ok {
						t.Errorf("warning payload missing %q: %v", key, params)
					}
				}
			case "memory":
				for _, key := range []string{"op", "percept_id", "subject"} {
					if _, ok := params[key]; !ok {
						t.Errorf("memory payload missing %q: %v", key, params)
					}
				}
			case "route":
				for _, key := range []string{"target", "reason", "conclusive"} {
					if _, ok := params[key]; !ok {
						t.Errorf("route payload missing %q: %v", key, params)
					}
				}
			}
		})
	}
}

func TestMapperExtEnvelope(t *testing.T) {
	m := Mapper{Session: "sess-1"}
	n := m.Toast("t1", "info", "/notifications", "hello")
	if n.Method != ExtMethodNotification {
		t.Fatalf("method = %q", n.Method)
	}
	if n.JSONRPC != "2.0" {
		t.Fatalf("jsonrpc = %q", n.JSONRPC)
	}
	raw, err := json.Marshal(n.Params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	var p NotificationPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("params not a NotificationPayload: %v", err)
	}
	if p.ID != "t1" || p.CommandHint != "/notifications" {
		t.Fatalf("payload = %+v", p)
	}
}
