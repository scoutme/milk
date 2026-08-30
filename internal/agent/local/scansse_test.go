package local

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestScanSSE_ReasoningOnly verifies that a stream carrying only
// reasoning_content deltas (no content, no tool calls) is reported back with
// reasoningSeen=true and an empty accumulated text, so callers can
// distinguish a truncated/degenerate completion from a genuine empty
// response.
func TestScanSSE_ReasoningOnly(t *testing.T) {
	sse := "" +
		`data: {"choices":[{"delta":{"reasoning_content":"Now let me update the file."}}]}` + "\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n" +
		"data: [DONE]\n"

	a := &Agent{}
	var thinking strings.Builder
	a.onThinking = func(s string) { thinking.WriteString(s) }

	scanner := bufio.NewScanner(strings.NewReader(sse))
	det := NewStreamDetector(ToolFormatUnknown)
	var textBuf strings.Builder
	var out bytes.Buffer

	toolCalls, _, _, _, reasoningText, finishReason, err := a.scanSSE(scanner, det, map[int]*toolCall{}, &textBuf, &out)
	if err != nil {
		t.Fatalf("scanSSE returned error: %v", err)
	}
	if len(toolCalls) != 0 {
		t.Errorf("expected no tool calls, got %d", len(toolCalls))
	}
	if textBuf.Len() != 0 {
		t.Errorf("expected empty content buffer, got %q", textBuf.String())
	}
	if reasoningText != "Now let me update the file." {
		t.Errorf("expected reasoningText to capture the reasoning delta, got %q", reasoningText)
	}
	if finishReason != "stop" {
		t.Errorf("expected finishReason=%q, got %q", "stop", finishReason)
	}
	if thinking.String() != "Now let me update the file." {
		t.Errorf("onThinking did not receive reasoning text, got %q", thinking.String())
	}
}

func TestScanSSE_NativeToolCallWithNullContinuationFields(t *testing.T) {
	sse := "" +
		`data: {"choices":[{"delta":{"reasoning_content":"Hmm, that might not have worked. Let me try another approach."},"finish_reason":null}]}` + "\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"arguments":"","name":"bash"},"type":"function"}]},"finish_reason":null}]}` + "\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":null,"function":{"arguments":"{\"command\": ","name":null},"type":"function"}]},"finish_reason":null}]}` + "\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":null,"function":{"arguments":"\"cd /home/scoutme/altworkspace/milk && git status --short\"","name":null},"type":"function"}]},"finish_reason":null}]}` + "\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":null,"function":{"arguments":"}","name":null},"type":"function"}]},"finish_reason":null}]}` + "\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n" +
		"data: [DONE]\n"

	a := &Agent{}
	scanner := bufio.NewScanner(strings.NewReader(sse))
	det := NewStreamDetector(ToolFormatUnknown)
	var textBuf strings.Builder

	toolCalls, _, _, _, reasoningText, finishReason, err := a.scanSSE(scanner, det, map[int]*toolCall{}, &textBuf, io.Discard)
	if err != nil {
		t.Fatalf("scanSSE returned error: %v", err)
	}
	if finishReason != "tool_calls" {
		t.Fatalf("expected finishReason=tool_calls, got %q", finishReason)
	}
	if reasoningText == "" {
		t.Fatal("expected reasoning text to be preserved")
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected one native tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].ID != "call_1" || toolCalls[0].Function.Name != "bash" {
		t.Fatalf("unexpected tool call header: %+v", toolCalls[0])
	}
	if got := toolCalls[0].Function.Arguments; got != `{"command": "cd /home/scoutme/altworkspace/milk && git status --short"}` {
		t.Fatalf("expected assembled arguments, got %q", got)
	}
}
