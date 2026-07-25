package local

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMessage_MarshalJSON_PlainString(t *testing.T) {
	m := Message{Role: "user", Content: "hello"}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Should be: {"role":"user","content":"hello"}
	s := string(data)
	if !strings.Contains(s, `"content":"hello"`) {
		t.Errorf("expected string content, got: %s", s)
	}
	// Content must be a string, not an array.
	if strings.Contains(s, `"content":[`) {
		t.Errorf("expected string content, got array: %s", s)
	}
}

func TestMessage_MarshalJSON_Multipart(t *testing.T) {
	m := Message{
		Role:    "user",
		Content: "describe this image",
		ContentParts: []ContentPart{
			{Type: "text", Text: "describe this image"},
			{Type: "image_url", ImageURL: &ImageURLPart{URL: "data:image/png;base64,abc"}},
		},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Should be: {"role":"user","content":[...]}
	s := string(data)
	if !strings.Contains(s, `"content":[`) {
		t.Errorf("expected array content, got: %s", s)
	}
	if !strings.Contains(s, `"type":"text"`) {
		t.Errorf("missing text part: %s", s)
	}
	if !strings.Contains(s, `"type":"image_url"`) {
		t.Errorf("missing image_url part: %s", s)
	}
	if !strings.Contains(s, `data:image/png;base64,abc`) {
		t.Errorf("missing data URI: %s", s)
	}
}

func TestMessage_MarshalJSON_EmptyContent(t *testing.T) {
	m := Message{Role: "assistant"}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// omitempty: content field absent when empty and no ContentParts.
	s := string(data)
	if strings.Contains(s, `"content"`) {
		t.Errorf("expected omitted content field, got: %s", s)
	}
}

func TestMessage_MarshalJSON_ToolCalls(t *testing.T) {
	tc := toolCall{
		ID:   "call_1",
		Type: "function",
		Function: toolCallFunction{
			Name:      "my_tool",
			Arguments: `{"x":1}`,
		},
	}
	m := Message{Role: "assistant", ToolCalls: []toolCall{tc}}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, `"tool_calls"`) {
		t.Errorf("expected tool_calls field: %s", s)
	}
	if !strings.Contains(s, `"my_tool"`) {
		t.Errorf("expected tool name: %s", s)
	}
}

func TestContentPart_ImageURL(t *testing.T) {
	part := ContentPart{
		Type:     "image_url",
		ImageURL: &ImageURLPart{URL: "data:image/jpeg;base64,xyz"},
	}
	data, err := json.Marshal(part)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, `"image_url"`) {
		t.Errorf("missing image_url key: %s", s)
	}
	if !strings.Contains(s, `"url":"data:image/jpeg;base64,xyz"`) {
		t.Errorf("missing url value: %s", s)
	}
}
