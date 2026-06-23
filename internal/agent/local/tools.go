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

	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/session"
)

const errMemUnavailable = "memory store not available"

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
func schemas(mem *memory.Store, otelDir string, sess *session.Session) []map[string]any {
	base := []map[string]any{
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
				"name":        "find_files",
				"description": "Find files by name pattern (glob) under a directory. Use this to locate files by filename, e.g. '*_test.go', '*.md'. For searching file contents use grep.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string", "description": "Root directory to search under"},
						"pattern": map[string]any{"type": "string", "description": "Glob filename pattern, e.g. '*_test.go'"},
					},
					"required": []string{"path", "pattern"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "grep",
				"description": "Search for a pattern inside file contents. Returns matching lines with line numbers. Use find_files to locate files by name instead.",
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
				"description": "Replace a string in a file. By default requires the string to be unique; set replace_all=true to replace every occurrence.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":        map[string]any{"type": "string", "description": "Absolute or relative file path"},
						"old_string":  map[string]any{"type": "string", "description": "Exact string to replace (must be unique unless replace_all is true)"},
						"new_string":  map[string]any{"type": "string", "description": "Replacement string"},
						"replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences instead of requiring uniqueness (default false)"},
					},
					"required": []string{"path", "old_string", "new_string"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "open_file",
				"description": "Open a file in the user's editor (interactive TUI mode only). Uses the same editor list as /config open: $EDITOR, $VISUAL, nano, vim, vi — or config_editors if set.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string", "description": "Absolute or relative file path to open"},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "delete_file",
				"description": "Delete a file from disk.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string", "description": "Absolute or relative file path to delete"},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "move_file",
				"description": "Move or rename a file. Creates destination parent directories as needed.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"source":      map[string]any{"type": "string", "description": "Source file path"},
						"destination": map[string]any{"type": "string", "description": "Destination file path"},
					},
					"required": []string{"source", "destination"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "http_request",
				"description": "Make an HTTP request with any method. Use for POST/PUT/DELETE or when custom headers or a request body are needed.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"method":    map[string]any{"type": "string", "description": "HTTP method (GET, POST, PUT, PATCH, DELETE, …); default GET"},
						"url":       map[string]any{"type": "string", "description": "URL to request"},
						"headers":   map[string]any{"type": "object", "description": "Optional request headers as key-value pairs"},
						"body":      map[string]any{"type": "string", "description": "Optional request body"},
						"max_bytes": map[string]any{"type": "integer", "description": "Maximum response bytes to return (default 8192)"},
					},
					"required": []string{"url"},
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
				"description": "Return shared session history for both agents. Filter with last_n, pattern, or agent.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"last_n":  map[string]any{"type": "integer", "description": "Last N turns only."},
						"pattern": map[string]any{"type": "string", "description": "Substring filter."},
						"agent":   map[string]any{"type": "string", "enum": []string{"primary", "escalation"}, "description": "Filter by agent role."},
					},
					"required": []string{},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "escalate",
				"description": "Signal that this task exceeds local model capabilities and should be handed off to the escalation agent.",
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
	if mem != nil {
		base = append(base, memory.Schemas()...)
	}
	if otelDir != "" {
		base = append(base, obs.ToolSchemas()...)
	}
	if sess != nil {
		base = append(base, exportSessionSchema())
		base = append(base, currentNeedSchema())
		base = append(base, getContextStatsSchema())
	}
	return base
}

func currentNeedSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "current_need",
			"description": "Update the current user goal for this session.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"goal": map[string]any{"type": "string", "description": "One-sentence description of the current goal."},
				},
				"required": []string{"goal"},
			},
		},
	}
}

func exportSessionSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "export_session",
			"description": "Export the current session transcript. Optionally write to a file.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"format":      map[string]any{"type": "string", "enum": []string{"text", "json"}, "description": "'text' (default) or 'json'."},
					"output_path": map[string]any{"type": "string", "description": "File path to write to; omit to return inline."},
				},
				"required": []string{},
			},
		},
	}
}

func dispatchTool(ctx context.Context, name, argsJSON string, sess *session.Session, mem *memory.Store, otelDir string) (string, bool) {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return toolResult{Error: "invalid arguments: " + err.Error()}.String(), false
	}

	switch name {
	case "bash":
		return runBash(ctx, args)
	case "find_files":
		return runFindFiles(ctx, args)
	case "grep":
		return runGrep(ctx, args)
	case "read_file":
		return runReadFile(args)
	case "write_file":
		return runWriteFile(args)
	case "edit_file":
		return runEditFile(args)
	case "delete_file":
		return runDeleteFile(args)
	case "move_file":
		return runMoveFile(args)
	case "http_request":
		return runHTTPRequest(ctx, args)
	case "list_dir":
		return runListDir(args)
	case "http_get":
		return runHTTPGet(ctx, args)
	case "get_session_context":
		return runGetSessionContext(sess, args), false
	case "record_memory":
		if mem != nil {
			return memory.DispatchRecordMemory(ctx, mem, argsJSON), false
		}
		return toolResult{Error: errMemUnavailable}.String(), false
	case "get_memory":
		if mem != nil {
			return memory.DispatchGetMemory(ctx, mem, argsJSON, memory.ConsumerLocal), false
		}
		return toolResult{Error: errMemUnavailable}.String(), false
	case "list_memory":
		if mem != nil {
			return memory.DispatchListMemory(ctx, mem, argsJSON), false
		}
		return toolResult{Error: errMemUnavailable}.String(), false
	case "forget_memory":
		if mem != nil {
			return memory.DispatchForgetMemory(ctx, mem, argsJSON), false
		}
		return toolResult{Error: errMemUnavailable}.String(), false
	case "get_metrics":
		if otelDir != "" {
			return obs.DispatchGetMetrics(otelDir), false
		}
		return toolResult{Error: "observability not available"}.String(), false
	case "search_signals":
		if otelDir != "" {
			return obs.DispatchSearchSignals(otelDir, argsJSON), false
		}
		return toolResult{Error: "observability not available"}.String(), false
	case "export_session":
		return dispatchExportSession(sess, argsJSON), false
	case "current_need":
		return dispatchCurrentNeed(sess, argsJSON), false
	case "get_context_stats":
		return dispatchGetContextStats(sess, argsJSON), false
	case "escalate":
		return "", true // caller checks the bool
	default:
		return toolResult{Error: "unknown tool: " + name}.String(), false
	}
}

func dispatchExportSession(sess *session.Session, argsJSON string) string {
	if sess == nil {
		return toolResult{Error: "no active session"}.String()
	}
	var args struct {
		Format     string `json:"format"`
		OutputPath string `json:"output_path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return toolResult{Error: "invalid arguments: " + err.Error()}.String()
	}
	if args.Format == "" {
		args.Format = "text"
	}

	var content string
	switch args.Format {
	case "json":
		data, err := session.ExportJSON(sess)
		if err != nil {
			return toolResult{Error: "export failed: " + err.Error()}.String()
		}
		content = string(data)
	default:
		content = session.ExportText(sess)
	}

	if args.OutputPath != "" {
		path := filepath.Clean(args.OutputPath)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return toolResult{Error: "mkdir failed: " + err.Error()}.String()
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return toolResult{Error: "write failed: " + err.Error()}.String()
		}
		return toolResult{Output: fmt.Sprintf("session exported to %s (%d bytes)", path, len(content))}.String()
	}
	return toolResult{Output: content}.String()
}

func dispatchCurrentNeed(sess *session.Session, argsJSON string) string {
	if sess == nil {
		return toolResult{Error: "no active session"}.String()
	}
	var args struct {
		Goal string `json:"goal"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return toolResult{Error: "invalid arguments: " + err.Error()}.String()
	}
	if args.Goal == "" {
		return toolResult{Error: "goal must not be empty"}.String()
	}
	sess.RecordNeed(args.Goal)
	return toolResult{Output: "current need updated"}.String()
}

// runGetSessionContext formats session history with optional filters:
// last_n, pattern (substring), agent ("primary"|"escalation").
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

// expandTilde replaces a leading "~" with the user's home directory.
// Returns the path unchanged when it doesn't start with "~" or the home
// directory cannot be determined.
func expandTilde(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return home + path[1:]
}

func runFindFiles(ctx context.Context, args map[string]any) (string, bool) {
	path, _ := args["path"].(string)
	path = expandTilde(path)
	pattern, _ := args["pattern"].(string)
	cmd := exec.CommandContext(ctx, "find", path,
		"-not", "-path", "*/.git/*",
		"-not", "-path", "*/.claude/*",
		"-name", pattern,
		"-type", "f")
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
	return toolResult{Output: stdout.String(), Error: stderr.String(), ExitCode: code}.String(), false
}

func runGrep(ctx context.Context, args map[string]any) (string, bool) {
	pattern, _ := args["pattern"].(string)
	path, _ := args["path"].(string)
	path = expandTilde(path)
	recursive, _ := args["recursive"].(bool)

	// Always exclude hidden/binary directories and skip binary files.
	// -r is forced when path is a directory, regardless of the recursive flag.
	gargs := []string{"-n", "--color=never", "--binary-files=without-match",
		"--exclude-dir=.git", "--exclude-dir=.claude"}
	if recursive {
		gargs = append(gargs, "-r")
	} else {
		// If path is a directory, force recursive to avoid grep opening binary index files.
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			gargs = append(gargs, "-r")
		}
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
	path = filepath.Clean(expandTilde(path))

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
	path = filepath.Clean(expandTilde(path))

	data, err := os.ReadFile(path)
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}

	lines := strings.Split(string(data), "\n")

	const maxReadLines = 500

	offset := 0
	if v, ok := args["offset"]; ok {
		switch n := v.(type) {
		case float64:
			offset = int(n)
		case string:
			offset, _ = strconv.Atoi(n)
		}
	}
	limitSet := false
	limit := len(lines)
	if v, ok := args["limit"]; ok {
		limitSet = true
		switch n := v.(type) {
		case float64:
			limit = int(n)
		case string:
			limit, _ = strconv.Atoi(n)
		}
	}
	if !limitSet {
		limit = min(limit, maxReadLines)
	}

	offset = min(offset, len(lines))
	end := min(offset+limit, len(lines))

	numbered := make([]string, 0, end-offset+1)
	for i, l := range lines[offset:end] {
		numbered = append(numbered, fmt.Sprintf("%d\t%s", offset+i+1, l))
	}
	out := strings.Join(numbered, "\n")
	if !limitSet && end < len(lines) {
		out += fmt.Sprintf("\n\n[truncated: showed lines %d-%d of %d; use offset+limit to read more]", offset+1, end, len(lines))
	}
	return toolResult{Output: out}.String(), false
}

func runEditFile(args map[string]any) (string, bool) {
	path, _ := args["path"].(string)
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)
	replaceAll, _ := args["replace_all"].(bool)
	path = filepath.Clean(expandTilde(path))

	data, err := os.ReadFile(path)
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	content := string(data)

	count := strings.Count(content, oldStr)
	if count == 0 {
		return toolResult{Error: "old_string not found in " + path}.String(), false
	}
	if !replaceAll && count > 1 {
		return toolResult{Error: fmt.Sprintf("old_string is ambiguous: found %d occurrences in %s; use replace_all=true to replace all", count, path)}.String(), false
	}

	n := 1
	if replaceAll {
		n = -1
	}
	updated := strings.Replace(content, oldStr, newStr, n)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	if replaceAll {
		return toolResult{Output: fmt.Sprintf("edited %s (%d replacements)", path, count)}.String(), false
	}
	return toolResult{Output: "edited " + path}.String(), false
}

func runListDir(args map[string]any) (string, bool) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	path = filepath.Clean(expandTilde(path))

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

func runDeleteFile(args map[string]any) (string, bool) {
	path, _ := args["path"].(string)
	path = filepath.Clean(expandTilde(path))
	if err := os.Remove(path); err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	return toolResult{Output: "deleted " + path}.String(), false
}

func runMoveFile(args map[string]any) (string, bool) {
	src, _ := args["source"].(string)
	dst, _ := args["destination"].(string)
	src = filepath.Clean(expandTilde(src))
	dst = filepath.Clean(expandTilde(dst))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	if err := os.Rename(src, dst); err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	return toolResult{Output: fmt.Sprintf("moved %s → %s", src, dst)}.String(), false
}

func runHTTPRequest(ctx context.Context, args map[string]any) (string, bool) {
	url, _ := args["url"].(string)
	method, _ := args["method"].(string)
	if method == "" {
		method = http.MethodGet
	}
	body, _ := args["body"].(string)
	maxBytes := 8192
	if v, ok := args["max_bytes"]; ok {
		if n, ok := v.(float64); ok {
			maxBytes = int(n)
		}
	}

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), url, bodyReader)
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	if hdrs, ok := args["headers"].(map[string]any); ok {
		for k, v := range hdrs {
			if sv, ok := v.(string); ok {
				req.Header.Set(k, sv)
			}
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	return toolResult{Output: string(respBody), ExitCode: resp.StatusCode}.String(), false
}

func getContextStatsSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "get_context_stats",
			"description": "Return the current session's context usage: history turn counts, total history character count, and the configured message budget. Useful for self-regulating before hitting context limits.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
				"required":   []string{},
			},
		},
	}
}

func dispatchGetContextStats(sess *session.Session, _ string) string {
	if sess == nil {
		return toolResult{Error: "no active session"}.String()
	}
	totalChars := 0
	for _, t := range sess.History {
		totalChars += len(t.Content)
	}
	out := fmt.Sprintf(
		"local_turns=%d escalation_turns=%d total_history_turns=%d total_history_chars=%d",
		sess.LocalTurnCount(),
		sess.EscalationTurnCount(),
		len(sess.History),
		totalChars,
	)
	return toolResult{Output: out}.String()
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
