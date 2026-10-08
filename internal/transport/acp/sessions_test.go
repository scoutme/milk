package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestSessionsWireShapes locks the exact camelCase field names verified
// against the upstream agentclientprotocol/agent-client-protocol
// schema/v2/schema.json (same rule as TestLifecycleWireShapes: ACP side uses
// ACP's own camelCase shapes verbatim — never rename standard fields).
func TestSessionsWireShapes(t *testing.T) {
	cursor := SessionListCursor("o:50")
	tests := []struct {
		name string
		v    any
		want string
	}{
		{"ListSessionsRequest", ListSessionsRequest{CWD: "/repo"},
			`{"cwd":"/repo"}`},
		{"ListSessionsRequest with cursor", ListSessionsRequest{CWD: "/repo", Cursor: &cursor},
			`{"cwd":"/repo","cursor":"o:50"}`},
		{"ListSessionsResponse", ListSessionsResponse{Sessions: []SessionInfo{
			{SessionID: "sess_1", CWD: "/repo", Title: "fix the thing", UpdatedAt: "2026-09-29T12:00:00Z"}}},
			`{"sessions":[{"sessionId":"sess_1","cwd":"/repo","title":"fix the thing","updatedAt":"2026-09-29T12:00:00Z"}]}`},
		{"ListSessionsResponse with nextCursor", ListSessionsResponse{Sessions: []SessionInfo{}, NextCursor: &cursor},
			`{"sessions":[],"nextCursor":"o:50"}`},
		{"SessionInfo", SessionInfo{SessionID: "sess_1", CWD: "/repo"},
			`{"sessionId":"sess_1","cwd":"/repo"}`},
		{"ResumeSessionRequest", ResumeSessionRequest{SessionID: "sess_1", CWD: "/repo"},
			`{"sessionId":"sess_1","cwd":"/repo"}`},
		{"ResumeSessionRequest with replayFrom", ResumeSessionRequest{SessionID: "sess_1", CWD: "/repo",
			ReplayFrom: json.RawMessage(`{"type":"start"}`)},
			`{"sessionId":"sess_1","cwd":"/repo","replayFrom":{"type":"start"}}`},
		{"ResumeSessionResponse", ResumeSessionResponse{AvailableCommands: []AvailableCommand{Command("help", "list commands", "")}},
			`{"availableCommands":[{"name":"help","description":"list commands"}]}`},
		{"ReplayFrom start", ReplayStart(), `{"type":"start"}`},
		{"CloseSessionRequest", CloseSessionRequest{SessionID: "sess_1"}, `{"sessionId":"sess_1"}`},
		{"CloseSessionResponse", CloseSessionResponse{}, `{}`},
		{"DeleteSessionRequest", DeleteSessionRequest{SessionID: "sess_1"}, `{"sessionId":"sess_1"}`},
		{"DeleteSessionResponse", DeleteSessionResponse{}, `{}`},
		{"SessionCapabilities with delete", SessionCapabilities{Delete: &SessionDeleteCapabilities{}}, `{"delete":{}}`},
		{"UserMessageUpsert", UserMessageUpsert("msg_1", "hi"),
			`{"sessionUpdate":"user_message","messageId":"msg_1","content":[{"type":"text","text":"hi"}]}`},
		{"AgentMessageUpsert", AgentMessageUpsert("msg_2", "hello"),
			`{"sessionUpdate":"agent_message","messageId":"msg_2","content":[{"type":"text","text":"hello"}]}`},
		{"AgentThoughtUpsert", AgentThoughtUpsert("msg_3", "hmm"),
			`{"sessionUpdate":"agent_thought","messageId":"msg_3","content":[{"type":"text","text":"hmm"}]}`},
		{"UserMessageChunk", UserMessageChunk("msg_4", "hi"),
			`{"sessionUpdate":"user_message_chunk","messageId":"msg_4","content":{"type":"text","text":"hi"}}`},
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

// TestParseReplayFrom pins the schema's replay cursor rule: omitted/null
// resumes without replay, {"type":"start"} replays everything, and any other
// cursor type is rejected rather than guessed at.
func TestParseReplayFrom(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantNil bool
		wantErr bool
	}{
		{"omitted", ``, true, false},
		{"null", `null`, true, false},
		{"start", `{"type":"start"}`, false, false},
		{"underscore extension", `{"type":"_x"}`, false, true},
		{"unknown cursor", `{"type":"later"}`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseReplayFrom(json.RawMessage(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseReplayFrom(%s) = %v, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseReplayFrom(%s): %v", tc.raw, err)
			}
			if tc.wantNil != (got == nil) {
				t.Errorf("ParseReplayFrom(%s) = %v, wantNil=%v", tc.raw, got, tc.wantNil)
			}
			if got != nil && got.Type != "start" {
				t.Errorf("ParseReplayFrom(%s).Type = %q, want start", tc.raw, got.Type)
			}
		})
	}
}

// TestCodedError_JSONRPCCode drives handleRequest directly and pins the
// error code that lands on the wire: CodedError carries its deliberate code
// (even wrapped), MethodNotFoundError maps to -32601, anything else to the
// generic -32000.
func TestCodedError_JSONRPCCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"coded", &CodedError{Code: CodeResourceNotFound, Message: "session not found: nope"}, CodeResourceNotFound},
		{"coded invalid params", &CodedError{Code: CodeInvalidParams, Message: "bad params"}, CodeInvalidParams},
		{"wrapped coded", fmt.Errorf("session/resume: %w", &CodedError{Code: CodeInvalidParams, Message: "cwd mismatch"}), CodeInvalidParams},
		{"method not found", &MethodNotFoundError{Method: "session/load"}, -32601},
		{"generic", fmt.Errorf("boom"), -32000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeHandler{responder: func(string, json.RawMessage) (any, error) { return nil, tc.err }}
			var out bytes.Buffer
			c := NewStdioConn(&out, h)
			c.handleRequest(context.Background(), wireMessage{
				JSONRPC: "2.0", ID: json.RawMessage(`7`), Method: "test/fail", Params: json.RawMessage(`{}`),
			})
			line := strings.TrimSpace(out.String())
			var resp struct {
				Error *RPCError `json:"error"`
			}
			if err := json.Unmarshal([]byte(line), &resp); err != nil {
				t.Fatalf("Unmarshal(%s): %v", line, err)
			}
			if resp.Error == nil {
				t.Fatalf("no error object on the wire: %s", line)
			}
			if resp.Error.Code != tc.want {
				t.Errorf("error code = %d, want %d (wire: %s)", resp.Error.Code, tc.want, line)
			}
		})
	}
}
