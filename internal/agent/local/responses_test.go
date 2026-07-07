package local

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// --- messagesToResponses ---

func TestMessagesToResponses_RoleMessages(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	items := messagesToResponses(msgs)
	if len(items) != 3 {
		t.Fatalf("want 3 items, got %d: %+v", len(items), items)
	}
	for i, want := range []struct{ role, content string }{
		{"system", "You are helpful."},
		{"user", "hello"},
		{"assistant", "hi"},
	} {
		if items[i].Role != want.role || items[i].Content != want.content {
			t.Errorf("item[%d]: want role=%q content=%q, got role=%q content=%q",
				i, want.role, want.content, items[i].Role, items[i].Content)
		}
	}
}

func TestMessagesToResponses_ToolCall(t *testing.T) {
	msgs := []Message{
		{
			Role: "assistant",
			ToolCalls: []toolCall{
				{ID: "call1", Type: "function", Function: toolCallFunction{Name: "bash", Arguments: `{"command":"ls"}`}},
			},
		},
	}
	items := messagesToResponses(msgs)
	if len(items) != 1 {
		t.Fatalf("want 1 item (function_call), got %d: %+v", len(items), items)
	}
	got := items[0]
	if got.Type != "function_call" {
		t.Errorf("want type=function_call, got %q", got.Type)
	}
	if got.CallID != "call1" || got.Name != "bash" || got.Arguments != `{"command":"ls"}` {
		t.Errorf("unexpected function_call item: %+v", got)
	}
}

func TestMessagesToResponses_ToolResult(t *testing.T) {
	msgs := []Message{
		{Role: "tool", Content: "ok", ToolCallID: "call1"},
	}
	items := messagesToResponses(msgs)
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	got := items[0]
	if got.Type != "function_call_output" {
		t.Errorf("want type=function_call_output, got %q", got.Type)
	}
	if got.CallID != "call1" || got.Output != "ok" {
		t.Errorf("unexpected function_call_output item: %+v", got)
	}
}

func TestMessagesToResponses_AssistantTextAndToolCallBothEmitted(t *testing.T) {
	// An assistant turn can have both text content and tool calls; both must appear.
	msgs := []Message{
		{
			Role:    "assistant",
			Content: "thinking out loud",
			ToolCalls: []toolCall{
				{ID: "c2", Type: "function", Function: toolCallFunction{Name: "read_file", Arguments: `{}`}},
			},
		},
	}
	items := messagesToResponses(msgs)
	if len(items) != 2 {
		t.Fatalf("want 2 items (function_call + message), got %d: %+v", len(items), items)
	}
	if items[0].Type != "function_call" {
		t.Errorf("first item must be function_call, got %q", items[0].Type)
	}
	if items[1].Role != "assistant" || items[1].Content != "thinking out loud" {
		t.Errorf("second item must be assistant text, got %+v", items[1])
	}
}

// --- convertToolsToResponsesFormat ---

func TestConvertToolsToResponsesFormat_FlattensFunction(t *testing.T) {
	tools := []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "bash",
				"description": "Run a shell command",
				"parameters":  map[string]any{"type": "object"},
			},
		},
	}
	result := convertToolsToResponsesFormat(tools)
	if len(result) != 1 {
		t.Fatalf("want 1 tool, got %d", len(result))
	}
	got := result[0]
	if got["type"] != "function" {
		t.Errorf("want type=function, got %q", got["type"])
	}
	if got["name"] != "bash" {
		t.Errorf("want name=bash, got %q", got["name"])
	}
	if got["description"] != "Run a shell command" {
		t.Errorf("want description set, got %q", got["description"])
	}
	// The "function" wrapper key must not appear in the flat result.
	if _, ok := got["function"]; ok {
		t.Error("flat result must not contain a nested 'function' key")
	}
}

func TestConvertToolsToResponsesFormat_NonFunctionPassthrough(t *testing.T) {
	// Entries without a "function" sub-map pass through unchanged.
	tool := map[string]any{"type": "computer", "display_width_px": 1920}
	result := convertToolsToResponsesFormat([]map[string]any{tool})
	if len(result) != 1 {
		t.Fatalf("want 1 tool, got %d", len(result))
	}
	if result[0]["type"] != "computer" {
		t.Errorf("non-function entry must pass through unchanged, got %+v", result[0])
	}
}

// --- scanResponsesSSE ---

func makeSSEScanner(lines ...string) *bufio.Scanner {
	return bufio.NewScanner(strings.NewReader(strings.Join(lines, "\n")))
}

func TestScanResponsesSSE_TextDelta(t *testing.T) {
	scanner := makeSSEScanner(
		`data: {"type":"response.output_text.delta","delta":"hello ","output_index":0}`,
		`data: {"type":"response.output_text.delta","delta":"world","output_index":0}`,
		`data: [DONE]`,
	)
	a := &Agent{}
	det := NewStreamDetector(ToolFormatUnknown)
	var textBuf strings.Builder
	var out strings.Builder
	tcs, prompt, completion, err := a.scanResponsesSSE(scanner, det, map[int]*toolCall{}, &textBuf, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tcs) != 0 {
		t.Errorf("want no tool calls, got %d", len(tcs))
	}
	if prompt != 0 || completion != 0 {
		t.Errorf("want zero token counts, got prompt=%d completion=%d", prompt, completion)
	}
	if textBuf.String() != "hello world" {
		t.Errorf("want textBuf=hello world, got %q", textBuf.String())
	}
}

func TestScanResponsesSSE_FunctionCallAssembled(t *testing.T) {
	scanner := makeSSEScanner(
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_abc","name":"bash"}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"command\":"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"ls\"}"}`,
		`data: [DONE]`,
	)
	a := &Agent{}
	det := NewStreamDetector(ToolFormatUnknown)
	tcs, _, _, err := a.scanResponsesSSE(scanner, det, map[int]*toolCall{}, &strings.Builder{}, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tcs) != 1 {
		t.Fatalf("want 1 tool call, got %d: %+v", len(tcs), tcs)
	}
	tc := tcs[0]
	if tc.ID != "call_abc" || tc.Function.Name != "bash" {
		t.Errorf("unexpected tool call: %+v", tc)
	}
	if tc.Function.Arguments != `{"command":"ls"}` {
		t.Errorf("unexpected arguments: %q", tc.Function.Arguments)
	}
}

func TestScanResponsesSSE_UsageFromResponseCompleted(t *testing.T) {
	scanner := makeSSEScanner(
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":42,"output_tokens":7}}}`,
		`data: [DONE]`,
	)
	a := &Agent{}
	det := NewStreamDetector(ToolFormatUnknown)
	_, prompt, completion, err := a.scanResponsesSSE(scanner, det, map[int]*toolCall{}, &strings.Builder{}, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prompt != 42 || completion != 7 {
		t.Errorf("want prompt=42 completion=7, got prompt=%d completion=%d", prompt, completion)
	}
}

func TestScanResponsesSSE_SkipsBadJSON(t *testing.T) {
	scanner := makeSSEScanner(
		`data: {"type":"response.output_text.delta","delta":"ok","output_index":0}`,
		`data: {not valid json`,
		`data: [DONE]`,
	)
	a := &Agent{}
	det := NewStreamDetector(ToolFormatUnknown)
	var textBuf strings.Builder
	_, _, _, err := a.scanResponsesSSE(scanner, det, map[int]*toolCall{}, &textBuf, io.Discard)
	if err != nil {
		t.Fatalf("bad JSON line must be skipped, got error: %v", err)
	}
	if textBuf.String() != "ok" {
		t.Errorf("want textBuf=ok after bad-JSON skip, got %q", textBuf.String())
	}
}

// --- inferenceURL with responses API ---

func TestInferenceURL_ResponsesAPIDefaultsToResponsesPath(t *testing.T) {
	a := &Agent{baseURL: "http://localhost:8080", chatPath: "/v1/responses"}
	want := "http://localhost:8080/v1/responses"
	if got := a.inferenceURL(); got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}
