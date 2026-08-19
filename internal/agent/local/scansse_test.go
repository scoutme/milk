package local

import (
	"bufio"
	"bytes"
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
