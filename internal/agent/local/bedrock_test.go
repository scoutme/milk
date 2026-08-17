package local

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- convertMessagesToConverse ---

func TestConvertMessages_SystemExtracted(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "hello"},
	}
	conv, sys := convertMessagesToConverse(msgs)
	if len(sys) != 1 || sys[0].Text != "You are helpful." {
		t.Errorf("system message not extracted: %+v", sys)
	}
	if len(conv) != 1 || conv[0].Role != "user" {
		t.Errorf("user message missing: %+v", conv)
	}
}

func TestConvertMessages_ToolResultsMerged(t *testing.T) {
	// Two consecutive tool results must be merged into one user message.
	msgs := []Message{
		{Role: "user", Content: "run bash"},
		{Role: "assistant", Content: "", ToolCalls: []toolCall{
			{ID: "tc1", Type: "function", Function: toolCallFunction{Name: "bash", Arguments: `{"command":"ls"}`}},
		}},
		{Role: "tool", Content: "file.go", ToolCallID: "tc1"},
		{Role: "tool", Content: "main.go", ToolCallID: "tc2"},
	}
	conv, _ := convertMessagesToConverse(msgs)
	// Should be: user("run bash"), assistant(tool-use), user(two tool-results merged)
	if len(conv) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(conv), conv)
	}
	last := conv[2]
	if last.Role != "user" {
		t.Errorf("merged tool results must be role=user, got %q", last.Role)
	}
	if len(last.Content) != 2 {
		t.Errorf("want 2 tool-result blocks merged, got %d", len(last.Content))
	}
}

func TestConvertMessages_AssistantToolCallsConverted(t *testing.T) {
	msgs := []Message{
		{
			Role: "assistant",
			ToolCalls: []toolCall{
				{ID: "tc1", Type: "function", Function: toolCallFunction{Name: "read_file", Arguments: `{"path":"/tmp/x"}`}},
			},
		},
	}
	conv, _ := convertMessagesToConverse(msgs)
	if len(conv) != 1 {
		t.Fatalf("want 1 message, got %d", len(conv))
	}
	blk := conv[0].Content[0]
	if blk.ToolUse == nil {
		t.Fatal("expected ToolUse block")
	}
	if blk.ToolUse.Name != "read_file" || blk.ToolUse.ToolUseID != "tc1" {
		t.Errorf("unexpected ToolUse: %+v", blk.ToolUse)
	}
}

func TestConvertMessages_EmptySystemIgnored(t *testing.T) {
	msgs := []Message{{Role: "system", Content: ""}}
	_, sys := convertMessagesToConverse(msgs)
	if len(sys) != 0 {
		t.Errorf("empty system message must be ignored, got %+v", sys)
	}
}

// --- convertToolsToConverse ---

func TestConvertTools_BasicShape(t *testing.T) {
	tools := []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "bash",
				"description": "Run a command",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
	result := convertToolsToConverse(tools)
	if len(result) != 1 {
		t.Fatalf("want 1 tool, got %d", len(result))
	}
	spec := result[0].ToolSpec
	if spec.Name != "bash" || spec.Description != "Run a command" {
		t.Errorf("unexpected spec: %+v", spec)
	}
	if spec.InputSchema.JSON == nil {
		t.Error("InputSchema.JSON must not be nil")
	}
}

func TestConvertTools_MissingParametersGetsDefault(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "noop", "description": ""}},
	}
	result := convertToolsToConverse(tools)
	if len(result) != 1 {
		t.Fatalf("want 1 tool, got %d", len(result))
	}
	if result[0].ToolSpec.InputSchema.JSON == nil {
		t.Error("missing parameters must produce a default schema")
	}
}

func TestConvertTools_NonFunctionEntrySkipped(t *testing.T) {
	tools := []map[string]any{
		{"type": "not-a-function"},
	}
	result := convertToolsToConverse(tools)
	if len(result) != 0 {
		t.Errorf("non-function entry must be skipped, got %+v", result)
	}
}

// --- parseBedrockHeader ---

func TestParseBedrockHeader_EventType(t *testing.T) {
	// Build a minimal header block: one string header named ":event-type" = "contentBlockDelta"
	name := ":event-type"
	val := "contentBlockDelta"
	data := buildStringHeader(name, val)
	got := parseBedrockHeader(data, ":event-type")
	if got != val {
		t.Errorf("want %q, got %q", val, got)
	}
}

func TestParseBedrockHeader_MissingKey(t *testing.T) {
	data := buildStringHeader(":event-type", "x")
	if got := parseBedrockHeader(data, ":other"); got != "" {
		t.Errorf("missing key must return empty, got %q", got)
	}
}

func TestParseBedrockHeader_EmptyData(t *testing.T) {
	if got := parseBedrockHeader(nil, ":event-type"); got != "" {
		t.Errorf("empty data must return empty, got %q", got)
	}
}

// --- readBedrockEvent ---

func TestReadBedrockEvent_RoundTrip(t *testing.T) {
	// Build a minimal valid event frame manually.
	eventType := "contentBlockDelta"
	payload := []byte(`{"contentBlockIndex":0,"delta":{"text":"hello"}}`)

	headerData := buildStringHeader(":event-type", eventType)
	frame := buildBedrockEventFrame(headerData, payload)

	reader := newByteReader(frame)
	gotType, gotPayload, err := readBedrockEvent(reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotType != eventType {
		t.Errorf("want eventType %q, got %q", eventType, gotType)
	}

	var ev bedrockContentBlockDeltaEvent
	if err := json.Unmarshal(gotPayload, &ev); err != nil {
		t.Fatalf("payload not valid JSON: %v", err)
	}
	if ev.Delta.Text != "hello" {
		t.Errorf("want delta.text=hello, got %q", ev.Delta.Text)
	}
}

// --- test helpers ---

// buildStringHeader encodes a single AWS Event Stream string header.
// Format: nameLen(1) + name + type(1, value=7) + valueLen(2BE) + value
func buildStringHeader(name, value string) []byte {
	var b []byte
	b = append(b, byte(len(name)))
	b = append(b, []byte(name)...)
	b = append(b, 7) // string type
	vl := make([]byte, 2)
	binary.BigEndian.PutUint16(vl, uint16(len(value)))
	b = append(b, vl...)
	b = append(b, []byte(value)...)
	return b
}

// buildBedrockEventFrame builds a complete AWS Event Stream frame.
// Does not compute real CRCs (zeros are fine for unit-test parsing since
// readBedrockEvent skips CRC bytes without verifying them).
func buildBedrockEventFrame(headers, payload []byte) []byte {
	// total = 4(total) + 4(headersLen) + 4(preludeCRC) + len(headers) + len(payload) + 4(msgCRC)
	total := uint32(16 + len(headers) + len(payload))
	frame := make([]byte, total)
	binary.BigEndian.PutUint32(frame[0:4], total)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	// [8:12] prelude CRC — zeroed
	copy(frame[12:12+len(headers)], headers)
	copy(frame[12+len(headers):], payload)
	// [total-4:total] message CRC — zeroed
	return frame
}

type byteReader struct {
	data []byte
	pos  int
}

func newByteReader(data []byte) *byteReader { return &byteReader{data: data} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, errorEOF()
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func errorEOF() error {
	return ioEOF{}
}

type ioEOF struct{}

func (ioEOF) Error() string { return "EOF" }

// Make ioEOF satisfy the io.EOF check used inside readBedrockEvent.
// readBedrockEvent compares err == io.EOF, so we need to return the real io.EOF.
// Replace byteReader.Read to return the real io.EOF instead.
func init() {
	// No-op: byteReader.Read already returns ioEOF which won't match io.EOF,
	// but readBedrockEvent's first read is io.ReadFull which wraps EOF as
	// io.ErrUnexpectedEOF when partial, or returns io.EOF when zero bytes are read.
	// Our test frame is complete so no EOF is hit during parsing.
	_ = struct{}{}
}

// --- inferenceURL ---

func TestInferenceURL_Default(t *testing.T) {
	a := &Agent{baseURL: "http://localhost:8080"}
	want := "http://localhost:8080/v1/chat/completions"
	if got := a.inferenceURL(); got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestInferenceURL_CustomChatPath(t *testing.T) {
	a := &Agent{baseURL: "https://proxy.example.com", chatPath: "/chat/completions"}
	want := "https://proxy.example.com/chat/completions"
	if got := a.inferenceURL(); got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

// --- bedrockStreamCompletion metadata usage / cache tokens ---

// bedrockConverseStreamServer returns a test server that serves a converse-stream
// response consisting of the given AWS Event Stream frames concatenated, using
// the same frame/header encoding as readBedrockEvent's own round-trip test.
func bedrockConverseStreamServer(t *testing.T, frames ...[]byte) *httptest.Server {
	t.Helper()
	var body []byte
	for _, f := range frames {
		body = append(body, f...)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body) //nolint:errcheck
	}))
}

func bedrockMetadataFrame(usageJSON string) []byte {
	headers := buildStringHeader(":event-type", "metadata")
	return buildBedrockEventFrame(headers, []byte(`{"usage":`+usageJSON+`}`))
}

func bedrockMessageStopFrame() []byte {
	headers := buildStringHeader(":event-type", "messageStop")
	return buildBedrockEventFrame(headers, []byte(`{"stopReason":"end_turn"}`))
}

// TestBedrockStreamCompletion_CacheTokensReported verifies that when the
// converse-stream metadata event's usage object includes cacheReadInputTokens
// and cacheWriteInputTokens (as documented by AWS's Converse API TokenUsage
// shape: https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_TokenUsage.html,
// retrieved 2026-08-17), those values reach onTokens as cacheRead/cacheCreation.
func TestBedrockStreamCompletion_CacheTokensReported(t *testing.T) {
	srv := bedrockConverseStreamServer(t,
		bedrockMetadataFrame(`{"inputTokens":100,"outputTokens":50,"cacheReadInputTokens":80,"cacheWriteInputTokens":20}`),
		bedrockMessageStopFrame(),
	)
	defer srv.Close()

	var gotPrompt, gotCompletion, gotCacheRead, gotCacheCreation int64
	var calls int
	a := &Agent{
		baseURL: srv.URL,
		model:   "test-model",
		client:  srv.Client(),
	}
	a.onTokens = func(model, role string, prompt, completion, cacheRead, cacheCreation int64) {
		calls++
		gotPrompt, gotCompletion, gotCacheRead, gotCacheCreation = prompt, completion, cacheRead, cacheCreation
	}

	if _, _, _, err := a.bedrockStreamCompletion(context.Background(), nil, nil, nil); err != nil {
		t.Fatalf("bedrockStreamCompletion returned error: %v", err)
	}

	if calls != 1 {
		t.Fatalf("want onTokens called once, got %d", calls)
	}
	if gotPrompt != 100 || gotCompletion != 50 {
		t.Errorf("want prompt=100 completion=50, got prompt=%d completion=%d", gotPrompt, gotCompletion)
	}
	if gotCacheRead != 80 {
		t.Errorf("want cacheRead=80, got %d", gotCacheRead)
	}
	if gotCacheCreation != 20 {
		t.Errorf("want cacheCreation=20, got %d", gotCacheCreation)
	}
}

// TestBedrockStreamCompletion_NoCacheFields_RegressionGuard verifies that
// when the metadata event's usage object has no cache fields at all (today's
// real-world case, since milk never sends an explicit cachePoint block yet),
// cacheRead and cacheCreation are both reported as 0 with no parse error.
func TestBedrockStreamCompletion_NoCacheFields_RegressionGuard(t *testing.T) {
	srv := bedrockConverseStreamServer(t,
		bedrockMetadataFrame(`{"inputTokens":100,"outputTokens":50}`),
		bedrockMessageStopFrame(),
	)
	defer srv.Close()

	var gotCacheRead, gotCacheCreation int64
	var calls int
	a := &Agent{
		baseURL: srv.URL,
		model:   "test-model",
		client:  srv.Client(),
	}
	a.onTokens = func(model, role string, prompt, completion, cacheRead, cacheCreation int64) {
		calls++
		gotCacheRead, gotCacheCreation = cacheRead, cacheCreation
	}

	if _, _, _, err := a.bedrockStreamCompletion(context.Background(), nil, nil, nil); err != nil {
		t.Fatalf("bedrockStreamCompletion returned error: %v", err)
	}

	if calls != 1 {
		t.Fatalf("want onTokens called once, got %d", calls)
	}
	if gotCacheRead != 0 || gotCacheCreation != 0 {
		t.Errorf("want cacheRead=0 cacheCreation=0 when cache fields are absent, got cacheRead=%d cacheCreation=%d", gotCacheRead, gotCacheCreation)
	}
}

// --- WarmToken ---

func TestWarmToken_NilTransport(t *testing.T) {
	a := &Agent{} // no tokenCmd set
	if err := a.WarmToken(); err != nil {
		t.Errorf("expected nil error with no token_cmd, got %v", err)
	}
}

func TestWarmToken_FetchesEagerly(t *testing.T) {
	tr := &tokenCmdTransport{inner: noopRoundTripper{}, cmd: "echo warmtoken"}
	a := &Agent{tokenCmd: tr}
	if err := a.WarmToken(); err != nil {
		t.Fatalf("WarmToken failed: %v", err)
	}
	tr.mu.Lock()
	cached := tr.token
	tr.mu.Unlock()
	if cached != "warmtoken" {
		t.Errorf("expected token 'warmtoken' after WarmToken, got %q", cached)
	}
}

type noopRoundTripper struct{}

func (noopRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(nil))}, nil
}
