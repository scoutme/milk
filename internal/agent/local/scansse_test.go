package local

import (
	"bufio"
	"bytes"
	"fmt"
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

// repeatingReasoningSSE builds an SSE stream of n identical reasoning_content
// deltas — enough to trip the n-gram monitor's consecutive-repeat detector —
// followed by a clean finish.
func repeatingReasoningSSE(cycle string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `data: {"choices":[{"delta":{"reasoning_content":%q}}]}`+"\n", cycle)
	}
	b.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n")
	b.WriteString("data: [DONE]\n")
	return b.String()
}

// TestScanSSE_NgramNotFedForWorkflowRole verifies the fix for a real bug:
// the reasoning n-gram monitor's Feed call must be gated by workflowRole,
// not just its recovery handling in Run(). Before this fix, a workflow-role
// agent's stream was still cut mid-reasoning by the monitor (Run() just
// never consumed the trigger to recover), which meant every subsequent
// stream in the turn would be truncated too, since the monitor's window was
// never reset for that role.
func TestScanSSE_NgramNotFedForWorkflowRole(t *testing.T) {
	cycle := "OK I'm done let me write up my findings actually wait let me check one more thing "
	sse := repeatingReasoningSSE(cycle, 10) // well past consecutiveThreshold=5

	a := &Agent{workflowRole: true}
	a.reasoningNgram = newReasoningNgramMonitor()
	scanner := bufio.NewScanner(strings.NewReader(sse))
	det := NewStreamDetector(ToolFormatUnknown)
	var textBuf strings.Builder

	_, _, _, _, reasoningText, finishReason, err := a.scanSSE(scanner, det, map[int]*toolCall{}, &textBuf, io.Discard)
	if err != nil {
		t.Fatalf("scanSSE returned error: %v", err)
	}
	if a.reasoningNgramTriggered {
		t.Error("expected reasoningNgramTriggered to stay false for a workflow-role agent")
	}
	if finishReason != "stop" {
		t.Errorf("expected the stream to run to completion (finish_reason=stop), got %q — it was cut short", finishReason)
	}
	wantLen := len(strings.Repeat(cycle, 10))
	if len(reasoningText) != wantLen {
		t.Errorf("expected full reasoning text (%d chars) to be streamed, got %d chars — stream was cut", wantLen, len(reasoningText))
	}
}

// TestScanSSE_NgramCutsStreamForNonWorkflowRole is the control for the test
// above: the same repeating pattern IS cut short when workflowRole is false.
func TestScanSSE_NgramCutsStreamForNonWorkflowRole(t *testing.T) {
	cycle := "OK I'm done let me write up my findings actually wait let me check one more thing "
	sse := repeatingReasoningSSE(cycle, 10)

	a := &Agent{}
	a.reasoningNgram = newReasoningNgramMonitor()
	scanner := bufio.NewScanner(strings.NewReader(sse))
	det := NewStreamDetector(ToolFormatUnknown)
	var textBuf strings.Builder

	_, _, _, _, reasoningText, _, err := a.scanSSE(scanner, det, map[int]*toolCall{}, &textBuf, io.Discard)
	if err != nil {
		t.Fatalf("scanSSE returned error: %v", err)
	}
	if !a.reasoningNgramTriggered {
		t.Error("expected reasoningNgramTriggered to be set for a non-workflow-role agent")
	}
	wantLen := len(strings.Repeat(cycle, 10))
	if len(reasoningText) >= wantLen {
		t.Errorf("expected the stream to be cut short before all %d chars arrived, got %d chars", wantLen, len(reasoningText))
	}
}
