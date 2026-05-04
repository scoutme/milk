package local

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

func dispatchTool(ctx context.Context, name, argsJSON string) (string, bool) {
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
	case "escalate_to_claude":
		return "", true // caller checks the bool
	default:
		return toolResult{Error: "unknown tool: " + name}.String(), false
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
