package local

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// reToolCall matches <tool_call>...</tool_call> blocks, including when
// wrapped in markdown fences (```xml ... ``` or ``` ... ```).
var reToolCall = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)

// reFenced matches content inside markdown code fences.
var reFenced = regexp.MustCompile("(?s)```(?:xml|json)?\\s*(.*?)\\s*```")

// extractToolCalls parses tool calls from a raw content string emitted by the
// model when llama.cpp fails to translate them into the tool_calls field.
// Handles:
//   - <tool_call>{"name":..., "arguments":{...}}</tool_call>
//   - ```xml\n{"name":..., "arguments":{...}}\n```
//   - bare JSON: {"name":..., "arguments":{...}}
//
// The "arguments" value may be a JSON object (Qwen native) or a JSON string
// (OpenAI format); both are normalised to a JSON string for toolCall.Function.Arguments.
func extractToolCalls(content string) []toolCall {
	var candidates []string

	// 1. <tool_call> blocks (may themselves be inside fences)
	for _, m := range reToolCall.FindAllStringSubmatch(content, -1) {
		candidates = append(candidates, strings.TrimSpace(m[1]))
	}

	// 2. Fenced blocks without <tool_call> wrapper
	if len(candidates) == 0 {
		for _, m := range reFenced.FindAllStringSubmatch(content, -1) {
			candidates = append(candidates, strings.TrimSpace(m[1]))
		}
	}

	// 3. Bare content if it looks like a JSON object
	if len(candidates) == 0 {
		trimmed := strings.TrimSpace(content)
		if strings.HasPrefix(trimmed, "{") {
			candidates = append(candidates, trimmed)
		}
	}

	var calls []toolCall
	for _, raw := range candidates {
		tc, ok := parseToolCallJSON(raw)
		if ok {
			calls = append(calls, tc)
		}
	}
	return calls
}

// nativeToolCall represents Qwen's native format where arguments is an object.
type nativeToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func parseToolCallJSON(raw string) (toolCall, bool) {
	var native nativeToolCall
	if err := json.Unmarshal([]byte(raw), &native); err != nil || native.Name == "" {
		return toolCall{}, false
	}

	// Normalise arguments to a JSON string (OpenAI format)
	var argsStr string
	if len(native.Arguments) > 0 {
		// If arguments is already a string (OpenAI format), unwrap it
		var s string
		if err := json.Unmarshal(native.Arguments, &s); err == nil {
			argsStr = s
		} else {
			// arguments is an object — re-encode as string
			argsStr = string(native.Arguments)
		}
	} else {
		argsStr = "{}"
	}

	return toolCall{
		ID:   uuid.New().String(),
		Type: "function",
		Function: toolCallFunction{
			Name:      native.Name,
			Arguments: argsStr,
		},
	}, true
}
