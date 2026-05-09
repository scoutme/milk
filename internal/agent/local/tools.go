package local

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/scoutme/milk/internal/session"
)

type toolResult struct {
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

func (r toolResult) String() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// schemas returns the OpenAI function schemas for all built-in tools.
func schemas() []map[string]any {
	return []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "bash",
				"description": "Execute a shell command and return stdout, stderr, and exit code.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string", "description": "Shell command to execute"},
					},
					"required": []string{"command"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "grep",
				"description": "Search for a pattern in files. Returns matching lines.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pattern":   map[string]any{"type": "string", "description": "Regex pattern to search for"},
						"path":      map[string]any{"type": "string", "description": "File or directory path to search"},
						"recursive": map[string]any{"type": "boolean", "description": "Search recursively in directories"},
					},
					"required": []string{"pattern", "path"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "read_file",
				"description": "Read the contents of a file, optionally with offset and limit.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":   map[string]any{"type": "string", "description": "Absolute or relative file path"},
						"offset": map[string]any{"type": "integer", "description": "Line offset to start reading from (0-based)"},
						"limit":  map[string]any{"type": "integer", "description": "Maximum number of lines to return"},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "write_file",
				"description": "Write content to a file, creating it or overwriting it. Use this to save code, configs, or any text to disk.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string", "description": "Absolute or relative file path"},
						"content": map[string]any{"type": "string", "description": "Full file content to write"},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "edit_file",
				"description": "Replace an exact string in a file with new content. Fails if old_string is not found or is ambiguous (appears more than once).",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":       map[string]any{"type": "string", "description": "Absolute or relative file path"},
						"old_string": map[string]any{"type": "string", "description": "Exact string to replace (must be unique in the file)"},
						"new_string": map[string]any{"type": "string", "description": "Replacement string"},
					},
					"required": []string{"path", "old_string", "new_string"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "list_dir",
				"description": "List the contents of a directory with file types and sizes.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string", "description": "Directory path to list (default: current directory)"},
					},
					"required": []string{},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "http_get",
				"description": "Fetch the body of a URL via HTTP GET. Useful for checking APIs or downloading text content.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url":       map[string]any{"type": "string", "description": "URL to fetch"},
						"max_bytes": map[string]any{"type": "integer", "description": "Maximum response bytes to return (default 8192)"},
					},
					"required": []string{"url"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "get_session_context",
				"description": "Return shared conversation history for this session (both agents). Use filters to avoid retrieving more than needed. Prefer last_n: 5 for recent context, pattern for a specific fact, agent to scope to one agent's turns.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"last_n":  map[string]any{"type": "integer", "description": "Return only the last N turns. Omit to return all."},
						"pattern": map[string]any{"type": "string", "description": "Case-insensitive substring filter — only turns whose content contains this string are returned."},
						"agent":   map[string]any{"type": "string", "enum": []string{"local", "claude"}, "description": "Restrict to turns from a specific agent. Omit for both."},
					},
					"required": []string{},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "escalate_to_claude",
				"description": "Signal that this task exceeds local model capabilities and should be handled by Claude.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"reason": map[string]any{"type": "string", "description": "Why escalation is needed"},
					},
					"required": []string{"reason"},
				},
			},
		},
	}
}

func dispatchTool(ctx context.Context, name, argsJSON string, sess *session.Session) (string, bool) {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return toolResult{Error: "invalid arguments: " + err.Error()}.String(), false
	}

	switch name {
	case "bash":
		return runBash(ctx, args)
	case "grep":
		return runGrep(ctx, args)
	case "read_file":
		return runReadFile(args)
	case "write_file":
		return runWriteFile(args)
	case "edit_file":
		return runEditFile(args)
	case "list_dir":
		return runListDir(args)
	case "http_get":
		return runHTTPGet(ctx, args)
	case "get_session_context":
		return runGetSessionContext(sess, args), false
	case "escalate_to_claude":
		return "", true // caller checks the bool
	default:
		return toolResult{Error: "unknown tool: " + name}.String(), false
	}
}

// runGetSessionContext formats session history with optional filters:
// last_n, pattern (substring), agent ("local"|"claude").
func runGetSessionContext(sess *session.Session, args map[string]any) string {
	if sess == nil || len(sess.History) == 0 {
		return toolResult{Output: "(no session history yet)"}.String()
	}
	turns := applySessionFilters(sess.History, args)
	if len(turns) == 0 {
		return toolResult{Output: "(no matching turns)"}.String()
	}
	var b strings.Builder
	for _, t := range turns {
		appendTurn(&b, t)
	}
	return toolResult{Output: b.String()}.String()
}

func applySessionFilters(turns []session.Turn, args map[string]any) []session.Turn {
	if agent, _ := args["agent"].(string); agent != "" {
		turns = filterByAgent(turns, agent)
	}
	if lastN := intArg(args, "last_n"); lastN > 0 && lastN < len(turns) {
		turns = turns[len(turns)-lastN:]
	}
	if pattern, _ := args["pattern"].(string); pattern != "" {
		turns = filterByPattern(turns, strings.ToLower(pattern))
	}
	return turns
}

func filterByAgent(turns []session.Turn, agent string) []session.Turn {
	out := turns[:0:0]
	for _, t := range turns {
		if string(t.Agent) == agent || t.Role == session.RoleUser {
			out = append(out, t)
		}
	}
	return out
}

func filterByPattern(turns []session.Turn, patternLower string) []session.Turn {
	out := turns[:0:0]
	for _, t := range turns {
		if strings.Contains(strings.ToLower(t.Content), patternLower) {
			out = append(out, t)
		}
	}
	return out
}

func intArg(args map[string]any, key string) int {
	v, ok := args[key]
	if !ok {
		return 0
	}
	if n, ok := v.(float64); ok {
		return int(n)
	}
	return 0
}

func appendTurn(b *strings.Builder, t session.Turn) {
	switch t.Role {
	case session.RoleUser:
		fmt.Fprintf(b, "User: %s\n", t.Content)
	case session.RoleAssistant:
		appendAssistantTurn(b, t)
	case session.RoleToolResult:
		content := t.Content
		if len(content) > 300 {
			content = content[:300] + "... (truncated)"
		}
		fmt.Fprintf(b, "[tool result] %s\n", content)
	}
}

func appendAssistantTurn(b *strings.Builder, t session.Turn) {
	if len(t.ToolCalls) > 0 {
		for _, tc := range t.ToolCalls {
			fmt.Fprintf(b, "[%s tool call: %s] %s\n", t.Agent, tc.Name, tc.Arguments)
		}
	} else {
		fmt.Fprintf(b, "%s: %s\n", t.Agent, t.Content)
	}
}

func runBash(ctx context.Context, args map[string]any) (string, bool) {
	command, _ := args["command"].(string)
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			return toolResult{Error: err.Error()}.String(), false
		}
	}
	return toolResult{
		Output:   stdout.String(),
		Error:    stderr.String(),
		ExitCode: code,
	}.String(), false
}

func runGrep(ctx context.Context, args map[string]any) (string, bool) {
	pattern, _ := args["pattern"].(string)
	path, _ := args["path"].(string)
	recursive, _ := args["recursive"].(bool)

	gargs := []string{"-n", "--color=never"}
	if recursive {
		gargs = append(gargs, "-r")
	}
	gargs = append(gargs, pattern, path)

	cmd := exec.CommandContext(ctx, "grep", gargs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			return toolResult{Error: err.Error()}.String(), false
		}
	}
	return toolResult{
		Output:   stdout.String(),
		Error:    stderr.String(),
		ExitCode: code,
	}.String(), false
}

func runWriteFile(args map[string]any) (string, bool) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	path = filepath.Clean(path)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	return toolResult{Output: "wrote " + path}.String(), false
}

func runReadFile(args map[string]any) (string, bool) {
	path, _ := args["path"].(string)
	path = filepath.Clean(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}

	lines := strings.Split(string(data), "\n")

	offset := 0
	if v, ok := args["offset"]; ok {
		switch n := v.(type) {
		case float64:
			offset = int(n)
		case string:
			offset, _ = strconv.Atoi(n)
		}
	}
	limit := len(lines)
	if v, ok := args["limit"]; ok {
		switch n := v.(type) {
		case float64:
			limit = int(n)
		case string:
			limit, _ = strconv.Atoi(n)
		}
	}

	if offset > len(lines) {
		offset = len(lines)
	}
	end := offset + limit
	if end > len(lines) {
		end = len(lines)
	}

	numbered := make([]string, 0, end-offset)
	for i, l := range lines[offset:end] {
		numbered = append(numbered, fmt.Sprintf("%d\t%s", offset+i+1, l))
	}
	return toolResult{Output: strings.Join(numbered, "\n")}.String(), false
}

func runEditFile(args map[string]any) (string, bool) {
	path, _ := args["path"].(string)
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)
	path = filepath.Clean(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	content := string(data)

	count := strings.Count(content, oldStr)
	if count == 0 {
		return toolResult{Error: "old_string not found in " + path}.String(), false
	}
	if count > 1 {
		return toolResult{Error: fmt.Sprintf("old_string is ambiguous: found %d occurrences in %s", count, path)}.String(), false
	}

	updated := strings.Replace(content, oldStr, newStr, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	return toolResult{Output: "edited " + path}.String(), false
}

func runListDir(args map[string]any) (string, bool) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	path = filepath.Clean(path)

	entries, err := os.ReadDir(path)
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}

	var lines []string
	for _, e := range entries {
		var size int64
		kind := "dir"
		if !e.IsDir() {
			kind = "file"
			if info, err := e.Info(); err == nil {
				size = info.Size()
			}
		}
		lines = append(lines, fmt.Sprintf("%-6s  %8d  %s", kind, size, e.Name()))
	}
	return toolResult{Output: strings.Join(lines, "\n")}.String(), false
}

func runHTTPGet(ctx context.Context, args map[string]any) (string, bool) {
	url, _ := args["url"].(string)
	maxBytes := 8192
	if v, ok := args["max_bytes"]; ok {
		switch n := v.(type) {
		case float64:
			maxBytes = int(n)
		case string:
			maxBytes, _ = strconv.Atoi(n)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	return toolResult{
		Output:   string(body),
		ExitCode: resp.StatusCode,
	}.String(), false
}
