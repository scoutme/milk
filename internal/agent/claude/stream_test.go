package claude

import (
	"strings"
	"testing"
)

func ndjson(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func TestStream_ExtractsSessionID(t *testing.T) {
	input := ndjson(
		`{"type":"system","subtype":"init","session_id":"sess-abc123"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-abc123","result":"hi"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "sess-abc123" {
		t.Errorf("want session_id sess-abc123, got %q", res.SessionID)
	}
}

func TestStream_WritesTextToOut(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello, world"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Hello, world") {
		t.Errorf("output missing text, got %q", out.String())
	}
	if res.Text != "Hello, world" {
		t.Errorf("want text %q, got %q", "Hello, world", res.Text)
	}
}

func TestStream_SeparatesConsecutiveAssistantEvents(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"First turn"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Second turn"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "First turn\nSecond turn" {
		t.Errorf("want newline between assistant events, got %q", res.Text)
	}
}

func TestStream_EndsWithQuestion(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"What do you mean?"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if !res.EndsWithQ {
		t.Error("expected EndsWithQ=true for response ending with '?'")
	}
}

func TestStream_NotEndsWithQuestion(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Done."}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if res.EndsWithQ {
		t.Error("expected EndsWithQ=false for response not ending with '?'")
	}
}

func TestStream_ErrorResult(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"result","is_error":true,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if !res.IsError {
		t.Error("expected IsError=true")
	}
}

func TestStream_IgnoresMalformedLines(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`not json at all`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "ok" {
		t.Errorf("want text ok, got %q", res.Text)
	}
}

func TestStream_SkipsNonTextContentBlocks(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","text":"internal"},{"type":"text","text":"visible"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if strings.Contains(res.Text, "internal") {
		t.Error("thinking block should not appear in text output")
	}
	if res.Text != "visible" {
		t.Errorf("want text visible, got %q", res.Text)
	}
}

// --- stream_event (--include-partial-messages) tests ---

func wrapStreamEvent(inner string) string {
	return `{"type":"stream_event","event":` + inner + `}`
}

func TestStream_TextDeltaStreamsText(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hel"}}`),
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}`),
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hello" {
		t.Errorf("want text hello, got %q", res.Text)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Errorf("output missing streamed text, got %q", out.String())
	}
}

func TestStream_TextDeltaSkipsAssistantEvent(t *testing.T) {
	// When text_delta events are received the final assistant event must be skipped.
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"streamed"}}`),
		`{"type":"assistant","message":{"content":[{"type":"text","text":"streamed"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if res.Text != "streamed" {
		t.Errorf("want streamed once, got %q", res.Text)
	}
	if strings.Count(out.String(), "streamed") != 1 {
		t.Errorf("text printed more than once: %q", out.String())
	}
}

func TestStream_ThinkingNewlineBeforeText(t *testing.T) {
	// First text_delta after thinking tokens must be preceded by a newline.
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"thought"}}`),
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"answer"}}`),
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	_, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "\n") {
		t.Errorf("want leading newline before first text after thinking, got %q", out.String())
	}
}

func TestStream_OnThinkingCallback(t *testing.T) {
	var got []string
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"deep thought"}}`),
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	Stream(strings.NewReader(input), &out, nil, StreamOpts{ //nolint:errcheck
		OnThinking: func(t string) { got = append(got, t) },
	})
	if len(got) != 1 || got[0] != "deep thought" {
		t.Errorf("OnThinking: want [deep thought], got %v", got)
	}
}

func TestStream_OnToolUseCallback(t *testing.T) {
	var got []string
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		wrapStreamEvent(`{"type":"content_block_start","content_block":{"type":"tool_use","name":"Bash"}}`),
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	Stream(strings.NewReader(input), &out, nil, StreamOpts{ //nolint:errcheck
		OnToolUse: func(name string) { got = append(got, name) },
	})
	if len(got) != 1 || got[0] != "Bash" {
		t.Errorf("OnToolUse: want [Bash], got %v", got)
	}
}

func TestStream_MessageStartSeparatesMessages(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"first"}}`),
		wrapStreamEvent(`{"type":"message_start"}`),
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"second"}}`),
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if res.Text != "first\nsecond" {
		t.Errorf("want newline between messages, got %q", res.Text)
	}
}

func TestStream_PermissionDenialsInResult(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"result","is_error":false,"session_id":"s1","permission_denials":[{"tool_name":"Write","tool_use_id":"u1","tool_input":{"file_path":"/tmp/x"}}]}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if len(res.PermissionDenials) != 1 || res.PermissionDenials[0].ToolName != "Write" {
		t.Errorf("want Write denial, got %v", res.PermissionDenials)
	}
}
