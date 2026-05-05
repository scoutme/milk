package local

import (
	"testing"
)

func TestExtractToolCalls_ToolCallTag(t *testing.T) {
	content := "<tool_call>\n{\"name\": \"bash\", \"arguments\": {\"command\": \"ls *.go\"}}\n</tool_call>"
	calls := extractToolCalls(content)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "bash" {
		t.Errorf("expected name bash, got %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"command": "ls *.go"}` {
		t.Errorf("unexpected arguments: %q", calls[0].Function.Arguments)
	}
}

func TestExtractToolCalls_FencedXML(t *testing.T) {
	content := "```xml\n{\"name\": \"bash\", \"arguments\": {\"command\": \"ls *.go\"}}\n```"
	calls := extractToolCalls(content)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "bash" {
		t.Errorf("expected name bash, got %q", calls[0].Function.Name)
	}
}

func TestExtractToolCalls_FencedXMLUnclosed(t *testing.T) {
	// Qwen2.5 via llama.cpp omits the closing ``` at end-of-stream
	content := "```xml\n{\"name\": \"bash\", \"arguments\": {\"command\": \"find . -name '*.go'\"}}"
	calls := extractToolCalls(content)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call from unclosed fence, got %d", len(calls))
	}
	if calls[0].Function.Name != "bash" {
		t.Errorf("expected name bash, got %q", calls[0].Function.Name)
	}
}

func TestExtractToolCalls_FencedJSON(t *testing.T) {
	content := "```json\n{\"name\": \"grep\", \"arguments\": {\"pattern\": \"TODO\", \"path\": \".\", \"recursive\": true}}\n```"
	calls := extractToolCalls(content)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "grep" {
		t.Errorf("expected name grep, got %q", calls[0].Function.Name)
	}
}

func TestExtractToolCalls_ToolCallTagInsideFence(t *testing.T) {
	content := "```xml\n<tool_call>\n{\"name\": \"bash\", \"arguments\": {\"command\": \"pwd\"}}\n</tool_call>\n```"
	calls := extractToolCalls(content)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "bash" {
		t.Errorf("expected name bash, got %q", calls[0].Function.Name)
	}
}

func TestExtractToolCalls_MultipleToolCalls(t *testing.T) {
	content := `<tool_call>{"name":"bash","arguments":{"command":"ls"}}</tool_call>` +
		"\n" +
		`<tool_call>{"name":"grep","arguments":{"pattern":"TODO","path":"."}}</tool_call>`
	calls := extractToolCalls(content)
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(calls))
	}
}

func TestExtractToolCalls_OpenAIStringArguments(t *testing.T) {
	// Arguments already a JSON string (OpenAI native format)
	content := `<tool_call>{"name":"bash","arguments":"{\"command\":\"ls\"}"}</tool_call>`
	calls := extractToolCalls(content)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Arguments != `{"command":"ls"}` {
		t.Errorf("expected unwrapped arguments, got %q", calls[0].Function.Arguments)
	}
}

func TestExtractToolCalls_NoToolCall(t *testing.T) {
	content := "Here are the Go files: main.go, config.go"
	calls := extractToolCalls(content)
	if len(calls) != 0 {
		t.Errorf("expected no tool calls from plain text, got %d", len(calls))
	}
}

func TestExtractToolCalls_MalformedJSON(t *testing.T) {
	content := "<tool_call>not valid json</tool_call>"
	calls := extractToolCalls(content)
	if len(calls) != 0 {
		t.Errorf("expected no tool calls from malformed JSON, got %d", len(calls))
	}
}

func TestExtractToolCalls_IDIsSet(t *testing.T) {
	content := `<tool_call>{"name":"bash","arguments":{"command":"ls"}}</tool_call>`
	calls := extractToolCalls(content)
	if len(calls) == 0 {
		t.Fatal("expected 1 call")
	}
	if calls[0].ID == "" {
		t.Error("expected non-empty tool call ID")
	}
}
