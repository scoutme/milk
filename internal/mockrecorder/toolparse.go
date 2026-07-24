package mockrecorder

import (
	"encoding/json"
	"regexp"
	"strings"
)

// toolCallRE matches the [tool:<name>,<json_args>] syntax used by the mock.
// The args portion is everything after the first comma through the closing bracket.
var toolCallRE = regexp.MustCompile(`\[tool:([a-zA-Z_][a-zA-Z0-9_]*),([^\]]*)\]`)

// ParseToolCalls extracts all [tool:<name>,<args>] directives from input.
// The args portion is decoded as JSON when it parses successfully; otherwise
// it is stored as {"raw": <args>} so callers always get a map[string]any.
func ParseToolCalls(input string) []ToolCallRecord {
	var out []ToolCallRecord
	matches := toolCallRE.FindAllStringSubmatch(input, -1)
	for _, m := range matches {
		name := m[1]
		raw := strings.TrimSpace(m[2])
		args := decodeArgs(raw)
		out = append(out, ToolCallRecord{
			Name: name,
			Args: args,
		})
	}
	return out
}

// decodeArgs attempts to JSON-decode raw as an object. Falls back to
// {"raw": raw} when the value is not valid JSON or not an object.
func decodeArgs(raw string) map[string]any {
	if raw != "" && raw[0] == '{' {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err == nil {
			return m
		}
	}
	return map[string]any{"raw": raw}
}

// HasToolCall reports whether input contains at least one [tool:...] directive.
func HasToolCall(input string) bool {
	return toolCallRE.MatchString(input)
}

// ExtractContextSummary returns a short summary of the messages/system context
// passed to the mock. It looks for the first 120 characters of the system message
// or user message text and returns a truncated version for recording.
func ExtractContextSummary(messages []map[string]any) string {
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		if role == "system" {
			content, _ := msg["content"].(string)
			return truncate(content, 120)
		}
	}
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		if role == "user" {
			content, _ := msg["content"].(string)
			return truncate(content, 120)
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
