package acp

import (
	"encoding/json"
	"testing"
)

// TestLifecycleWireShapes locks the exact camelCase field names verified
// against the upstream agentclientprotocol/agent-client-protocol
// schema/v2/schema.json — this package must not drift onto milk's own
// snake_case batch convention (design doc: "ACP side uses ACP's own
// camelCase shapes verbatim — never rename standard fields").
func TestLifecycleWireShapes(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want string
	}{
		{"InitializeRequest", InitializeRequest{ProtocolVersion: 2, Info: Implementation{Name: "zed"}},
			`{"protocolVersion":2,"info":{"name":"zed"},"capabilities":{}}`},
		{"InitializeResponse", InitializeResponse{ProtocolVersion: 2, Info: Implementation{Name: "milk"}},
			`{"protocolVersion":2,"info":{"name":"milk"},"capabilities":{}}`},
		{"InitializeResponse with session capability", InitializeResponse{ProtocolVersion: 2, Info: Implementation{Name: "milk"}, Capabilities: AgentCapabilities{Session: &SessionCapabilities{}}},
			`{"protocolVersion":2,"info":{"name":"milk"},"capabilities":{"session":{}}}`},
		{"NewSessionRequest", NewSessionRequest{CWD: "/repo"},
			`{"cwd":"/repo"}`},
		{"NewSessionResponse", NewSessionResponse{SessionID: "sess_1"},
			`{"sessionId":"sess_1"}`},
		{"PromptRequest", PromptRequest{SessionID: "sess_1", Prompt: []ContentBlock{TextBlock("hi")}},
			`{"sessionId":"sess_1","prompt":[{"type":"text","text":"hi"}]}`},
		{"PromptResponse", PromptResponse{MessageID: "msg_1"},
			`{"messageId":"msg_1"}`},
		{"CancelSessionNotification", CancelSessionNotification{SessionID: "sess_1"},
			`{"sessionId":"sess_1"}`},
		{"CancelRequestNotification", CancelRequestNotification{RequestID: "req_1"},
			`{"requestId":"req_1"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.v)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(b) != tt.want {
				t.Errorf("Marshal(%s) = %s, want %s", tt.name, b, tt.want)
			}
		})
	}
}

// TestPromptResponse_NoStopReason guards against reintroducing a stopReason
// field on PromptResponse — per the upstream schema this response only
// acknowledges the prompt was accepted; turn completion rides on a
// state_update session/update notification instead (see this file's doc
// comment on PromptResponse).
func TestPromptResponse_NoStopReason(t *testing.T) {
	b, err := json.Marshal(PromptResponse{MessageID: "msg_1"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := m["stopReason"]; ok {
		t.Error("PromptResponse must not carry stopReason — see its doc comment")
	}
}
