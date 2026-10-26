package local

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/selfdocs"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
)

// agentToolNameRE matches non-alphanumeric-lowercase characters for tool name sanitisation.
var agentToolNameRE = regexp.MustCompile(`[^a-z0-9]+`)

const errMemUnavailable = "memory store not available"
const errTaskUnavailable = "task store not available"

// maxReadLines is the default cap for read_file when no limit is given.
// Sized similarly to MiMo-Code's DEFAULT_READ_LIMIT (2000) — large enough
// that a model reads a whole file (or a substantial window of one) in a
// single call instead of paging through it in small, iteration-burning
// slices.
const maxReadLines = 2000

// TaskStore is the subset of the tasks.Store interface used by the local agent.
// Defined here to avoid an import cycle between internal/agent/local and internal/tasks.
type TaskStore interface {
	Create(title string, tags []string) (TaskEntry, error)
	Update(id, status, title string) error
	Complete(id string) error
	List(includeGlobal bool) ([]TaskEntry, error)
}

// TaskEntry is a simplified view of tasks.Task used in tool results.
type TaskEntry struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Status string   `json:"status"`
	Tags   []string `json:"tags,omitempty"`
}

type toolResult struct {
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

func (r toolResult) String() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// isToolError returns true when the tool result JSON contains a non-empty error field.
func isToolError(result string) bool {
	var r toolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return false
	}
	return r.Error != ""
}

// toolName extracts the function name from a tool schema map.
// Returns "" when the schema is malformed.
func toolName(s map[string]any) string {
	fn, _ := s["function"].(map[string]any)
	if fn == nil {
		return ""
	}
	name, _ := fn["name"].(string)
	return name
}

// filterTools applies IncludedTools / ExcludedTools from limits to the given
// tool list and returns the filtered result.
//
//  1. If limits is nil (or both lists empty) the original slice is returned
//     unchanged.
//  2. If IncludedTools is non-empty, only entries whose name appears in that
//     list are kept.
//  3. Names in ExcludedTools are removed from the result of step 2 (or from
//     the original list when IncludedTools is empty).
//
// The filtering is applied to the base built-in tools only. Dynamically
// appended tools (memory, otel, agent tools) are excluded from filtering so
// they always appear when the store / directory is available.
func filterTools(tools []map[string]any, limits *config.AgentLimits) []map[string]any {
	if limits == nil || (len(limits.IncludedTools) == 0 && len(limits.ExcludedTools) == 0) {
		return tools
	}
	var result []map[string]any
	if len(limits.IncludedTools) > 0 {
		inc := make(map[string]bool, len(limits.IncludedTools))
		for _, n := range limits.IncludedTools {
			inc[n] = true
		}
		for _, s := range tools {
			if inc[toolName(s)] {
				result = append(result, s)
			}
		}
	} else {
		result = make([]map[string]any, len(tools))
		copy(result, tools)
	}
	if len(limits.ExcludedTools) > 0 {
		exc := make(map[string]bool, len(limits.ExcludedTools))
		for _, n := range limits.ExcludedTools {
			exc[n] = true
		}
		filtered := result[:0:0]
		for _, s := range result {
			if !exc[toolName(s)] {
				filtered = append(filtered, s)
			}
		}
		result = filtered
	}
	return result
}

// workflowRegistryNames returns every currently registered /workflow name
// (built-in plus any ~/.milk/workflows/*.yaml overrides), for start_workflow's
// "name" enum. Computed fresh on every schemas() call rather than hardcoded —
// workflows are user-configurable, so a static list would drift the moment
// someone adds or renames one.
func workflowRegistryNames() []string {
	reg, _ := workflow.LoadRegistry()
	return reg.Names()
}

// workflowToolDescription builds start_workflow's description with the
// current registry names inlined, for the same reason workflowRegistryNames
// computes the enum fresh rather than hardcoding it.
func workflowToolDescription() string {
	names := workflowRegistryNames()
	base := "Start one of milk's native /workflow pipelines — a multi-stage pipeline with structured roles and a typed completion contract, run by internal/workflow/interp, distinct from spawn_background_agent's single self-contained tool loop."
	if len(names) == 0 {
		return base + " No workflows are currently registered."
	}
	return base + " Available: " + strings.Join(names, ", ") + ". Runs independently in the background: its progress and result appear in the transcript for the user, but — unlike spawn_background_agent — are not automatically added to your own context, so don't wait on or assume its outcome in this turn."
}

// schemas returns the OpenAI function schemas for all built-in tools,
// filtered by limits when non-nil.
func schemas(mem *memory.Store, otelDir string, sess *session.Session, toolAgents []config.AgentToolEntry, ts TaskStore, limits *config.AgentLimits) []map[string]any {
	base := []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "bash",
				"description": "Execute a shell command and return stdout, stderr, and exit code. Runs in the session working directory (or workdir). Set run_in_background=true for long-running processes (dev servers, watchers) — it returns a shell_id immediately; read output with bash_output, stop with kill_bash.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command":           map[string]any{"type": "string", "description": "Shell command to execute"},
						"workdir":           map[string]any{"type": "string", "description": "Working directory for the command (default: session cwd)"},
						"run_in_background": map[string]any{"type": "boolean", "description": "Start the command in the background and return a shell_id (default false)"},
						"timeout_seconds":   map[string]any{"type": "integer", "description": "Timeout in seconds (default 120, max 600); ignored with run_in_background"},
					},
					"required": []string{"command"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "bash_output",
				"description": "Read the current output of a background shell started with bash(run_in_background). Shows a status line (running/exited/killed) plus output.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"shell_id":   map[string]any{"type": "string", "description": "The shell ID returned by bash run_in_background"},
						"tail_lines": map[string]any{"type": "integer", "description": "Return only the last N lines (default: all retained output)"},
					},
					"required": []string{"shell_id"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "kill_bash",
				"description": "Stop a background shell started with bash(run_in_background), by its shell ID. Its output stays readable via bash_output until reaped.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"shell_id": map[string]any{"type": "string", "description": "The shell ID returned by bash run_in_background"},
					},
					"required": []string{"shell_id"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "find_files",
				"description": "Find files by name pattern (glob) under a directory. Use this to locate files by filename, e.g. '*_test.go', '*.md', 'src/**/index.ts'. Honors .gitignore (via git) where possible; results are capped and sorted newest-first. For searching file contents use grep.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string", "description": "Root directory to search under"},
						"pattern": map[string]any{"type": "string", "description": "Glob filename pattern: '*' and '?' within a segment, '**' for any depth (e.g. '*_test.go', 'src/**/*.ts'). A pattern with '/' matches the path relative to the root; otherwise it matches the basename."},
					},
					"required": []string{"path", "pattern"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "grep",
				"description": "Search for a pattern inside file contents. Returns matching lines with line numbers (extended regex, e.g. 'foo|bar'). Uses ripgrep when available, else git grep inside a git work tree (both honor .gitignore), else a built-in search (Go regex; skips hidden, vendored and binary files). Very long lines are truncated. Use find_files to locate files by name instead.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pattern":     map[string]any{"type": "string", "description": "Extended regex pattern to search for"},
						"path":        map[string]any{"type": "string", "description": "File or directory path to search"},
						"recursive":   map[string]any{"type": "boolean", "description": "Search recursively in directories (directories are searched recursively regardless)"},
						"ignore_case": map[string]any{"type": "boolean", "description": "Case-insensitive match (default false)"},
						"context":     map[string]any{"type": "integer", "description": "Include N lines of context around each match (default 0)"},
						"include":     map[string]any{"type": "string", "description": "Only search files matching this glob, e.g. '*.go'"},
						"max_results": map[string]any{"type": "integer", "description": "Cap on returned matching lines (default 200); the tail says how many more matched"},
					},
					"required": []string{"pattern", "path"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "read_file",
				"description": fmt.Sprintf("Read the contents of a file. By default returns up to %d lines from the start (or from offset, if given). Avoid tiny repeated slices (e.g. 20-30 line chunks) to explore a file — read a larger window, or the whole file, up front; use offset/limit only to page through a file that exceeds the default.", maxReadLines),
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":   map[string]any{"type": "string", "description": "Absolute or relative file path"},
						"offset": map[string]any{"type": "integer", "description": "Line number to start reading from (1-based, matching the output numbering; default 1)"},
						"limit":  map[string]any{"type": "integer", "description": fmt.Sprintf("Maximum number of lines to return (default %d)", maxReadLines)},
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
				"description": "Replace a string in a file. By default requires the string to be unique; set replace_all=true to replace every occurrence. If the exact string is not found, milk falls back to whitespace/indentation-tolerant matching (reported as 'fuzzy match'); when nothing matches, the error returns the closest matching block as a hint — never a silent fuzzy edit.",
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
				"name":        "multiedit",
				"description": "Apply several edits to one file in a single call, all-or-nothing: each edit is an old_string→new_string replacement with exactly edit_file's matching rules (exact first, whitespace/indentation-tolerant fallback reported as 'fuzzy match', uniqueness required unless replace_all), applied in order so each edit sees the previous edit's result. If any edit fails to match, nothing is written at all — the error names the failing edit and its closest matching block (never applied silently). Prefer this over repeated edit_file calls when one file needs several changes: one round trip instead of many.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string", "description": "Absolute or relative file path"},
						"edits": map[string]any{
							"type":        "array",
							"description": "Edits to apply in order.",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"old_string":  map[string]any{"type": "string", "description": "Text to replace (must be unique unless replace_all is true)"},
									"new_string":  map[string]any{"type": "string", "description": "Replacement string"},
									"replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences instead of requiring uniqueness (default false)"},
								},
								"required": []string{"old_string", "new_string"},
							},
						},
					},
					"required": []string{"path", "edits"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "open_file",
				"description": "Open a file for the user to view. In the TUI this uses the same editor list as /config open: $EDITOR, $VISUAL, nano, vim, vi — or config_editors if set; over ACP the file is handed to the platform opener instead. Refused where no opener is wired (single-prompt mode, background jobs).",
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
				"name":        "project_map",
				"description": "Token-cheap structural map of a codebase (or a subtree): a short framing header (project identity, docs and build/config landmarks) plus files grouped by directory with their symbols and signatures (Go: go/ast; JS/TS, Python, Java, Rust: heuristic scanners) — outlines, never bodies. Other files are listed by name. Use it to orient in an unfamiliar project instead of reading files just to learn what they define. Output degrades gracefully (signatures → names → reference-ranked names → file tree) to stay within max_chars; per-file results are cached in-process and on disk, re-parsed only on change.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":      map[string]any{"type": "string", "description": "Subtree to map (default: current directory)"},
						"max_files": map[string]any{"type": "integer", "description": "Max files to include (default 200)"},
						"max_chars": map[string]any{"type": "integer", "description": "Output budget in characters (default 6000); the outline degrades to fit"},
					},
					"required": []string{},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "web_fetch",
				"description": "Fetch a web page or API response. HTML is converted to Markdown by default (format: auto|markdown|text|html|raw). Upgrades http:// to https:// for non-local hosts. For web *search*, use an MCP search server instead.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url":       map[string]any{"type": "string", "description": "URL to fetch"},
						"format":    map[string]any{"type": "string", "enum": []string{"auto", "markdown", "text", "html", "raw"}, "description": "Output format: 'auto' (default) converts HTML to Markdown and leaves other content raw; 'text' strips markup without Markdown syntax"},
						"max_bytes": map[string]any{"type": "integer", "description": "Maximum body bytes to return (default 262144, max 5242880); oversized bodies spill to a file like other tool output"},
					},
					"required": []string{"url"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "get_session_context",
				"description": "Return shared session history for both agents. Recent turns are returned verbatim; older turns are shown as compact one-liners with turn indices (e.g. [3]). Use turn_from/turn_to to fetch specific older turns verbatim (max 5 turns per request). Filter with last_n, pattern, or agent.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"last_n":    map[string]any{"type": "integer", "description": "Last N turns only. Ignored when turn_from is set."},
						"pattern":   map[string]any{"type": "string", "description": "Substring filter."},
						"agent":     map[string]any{"type": "string", "enum": []string{"primary", "escalation"}, "description": "Filter by agent role."},
						"turn_from": map[string]any{"type": "integer", "description": "1-based start index from a previous compact older history listing. Returns turns verbatim (max 5)."},
						"turn_to":   map[string]any{"type": "integer", "description": "1-based end index (inclusive). Defaults to turn_from when omitted."},
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
		{
			"type": "function",
			"function": map[string]any{
				"name":        "ask_user",
				"description": "Ask the user a clarifying question and wait for their answer. Use it when the request is ambiguous in a way that changes what you should do — missing constraints, conflicting goals, or a choice between real alternatives — and the answer is not derivable from the code, the conversation, or a quick search. One question per call, phrased so a person who just walked up can answer it without context; include `options` when there are realistic choices. The answer comes back as the tool result. The user may be remote or away — in unattended contexts the question is bounded by a timed answer and may come back unanswered, in which case decide and continue instead of asking again.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question": map[string]any{"type": "string", "description": "The question to ask."},
						"options":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional realistic choices; free-text answers are still accepted."},
					},
					"required": []string{"question"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "start_workflow",
				"description": workflowToolDescription(),
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{
							"type":        "string",
							"enum":        workflowRegistryNames(),
							"description": "Registered workflow name.",
						},
						"task": map[string]any{"type": "string", "description": "The task/goal to run through the workflow."},
						"roles": map[string]any{
							"type":                 "object",
							"additionalProperties": map[string]any{"type": "string"},
							"description":          `Optional per-role agent overrides (e.g. {"generator": "gemma-local"}) — "primary"/"escalation" are aliases for the currently configured primary/escalation agent, or use a concrete agent name. Any role left unspecified defaults to "escalation".`,
						},
					},
					"required": []string{"name", "task"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "milk_config_help",
				"description": "Look up how to manage milk's own configuration — agents, MCP servers, memory, routing, etc. — from milk's own docs, instead of guessing the config.json schema. Omit topic to list what's available.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"topic": map[string]any{"type": "string", "description": `e.g. "mcp add", "mcp remove", "mcp assign", "agent add", "agent remove", "mcp auth". Omit to list all topics.`},
					},
					"required": []string{},
				},
			},
		},
	}
	// Apply whitelist/blacklist filtering to the base built-in tool set and
	// the memory tools. Memory tools used to be appended after filterTools,
	// bypassing limits.included_tools/excluded_tools entirely — they are now
	// routed through the same filter so they can be scoped away from small
	// models (issue #172 gap 4). Dynamic tools (otel, session, task, agent)
	// are appended after filtering so they are always available when the
	// backing store is present.
	if mem != nil {
		base = append(base, memory.Schemas()...)
	}
	base = filterTools(base, limits)

	if otelDir != "" {
		base = append(base, obs.ToolSchemas()...)
	}
	if sess != nil {
		base = append(base, exportSessionSchema())
		base = append(base, currentNeedSchema())
		base = append(base, getContextStatsSchema())
	}
	if ts != nil {
		base = append(base, taskSchemas()...)
	}
	base = append(base, AgentToolSchemas(toolAgents)...)
	return base
}

func taskSchemas() []map[string]any {
	return []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "create_task",
				"description": "Create a new task for tracking. Returns the task id.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title": map[string]any{"type": "string", "description": "Short task title"},
						"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional labels"},
					},
					"required": []string{"title"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "update_task",
				"description": "Update the status (and optionally title) of a task.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":     map[string]any{"type": "string", "description": "Task id (or unique prefix)"},
						"status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "done", "blocked"}, "description": "New status"},
						"title":  map[string]any{"type": "string", "description": "Optional new title"},
					},
					"required": []string{"id", "status"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "list_tasks",
				"description": "List current tasks. Returns session tasks; optionally include global tasks.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"include_global": map[string]any{"type": "boolean", "description": "Include cross-session global tasks"},
					},
					"required": []string{},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "complete_task",
				"description": "Mark a task as done.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id": map[string]any{"type": "string", "description": "Task id (or unique prefix)"},
					},
					"required": []string{"id"},
				},
			},
		},
	}
}

// sanitiseAgentToolName converts an agent name to a safe OpenAI tool function name.
// It lowercases the name, replaces non-alphanumeric runs with underscores, and
// prefixes with "agent_". Example: "my-agent" → "agent_my_agent".
func sanitiseAgentToolName(name string) string {
	lower := strings.ToLower(name)
	safe := agentToolNameRE.ReplaceAllString(lower, "_")
	safe = strings.Trim(safe, "_")
	return "agent_" + safe
}

// ResolveAgentToolName maps a dispatched tool-call name (e.g. "agent_mimo_local")
// back to the original config agent name (e.g. "mimo-local") by matching against
// entries' own sanitised names — the reverse of sanitiseAgentToolName isn't a pure
// string operation (it's lossy: hyphens, spaces, and other separators all collapse
// to "_"), so a naive prefix-strip of "agent_" recovers the sanitised form, not the
// original name. Returns ("", false) when toolName doesn't match any entry.
func ResolveAgentToolName(entries []config.AgentToolEntry, toolName string) (string, bool) {
	for _, e := range entries {
		if sanitiseAgentToolName(e.Agent) == toolName {
			return e.Agent, true
		}
	}
	return "", false
}

// AgentToolSchemas produces OpenAI function-call schema entries for the given
// agent tool entries. Returns an empty (non-nil) slice when entries is empty.
func AgentToolSchemas(entries []config.AgentToolEntry) []map[string]any {
	result := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		result = append(result, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        sanitiseAgentToolName(e.Agent),
				"description": e.Description,
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"request": map[string]any{
							"type":        "string",
							"description": "The request to send to the agent.",
						},
					},
					"required": []string{"request"},
				},
			},
		})
	}
	return result
}

// spawnBackgroundAgentSchema is the schema for the spawn_background_agent
// tool (ADR-0043). Only appended at Run's call site when a.backgroundManager
// is set — never when building a background job's own tool list (Phase 3),
// which enforces the depth-1 fork cap structurally rather than by filtering.
func spawnBackgroundAgentSchema(peers []string) map[string]any {
	schema := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "spawn_background_agent",
			"description": "Fork an independent copy of yourself to research a narrow, self-contained question in the background — reading files, grepping, running commands — without using up your own context. You are notified with a summary when it completes; you do not block on it and must not fabricate a result before that.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task":         map[string]any{"type": "string", "description": "The self-contained question or task for the background agent. Include everything it needs — it does not see your conversation."},
					"label":        map[string]any{"type": "string", "description": `Short human-readable label for status display, e.g. "analyze level-gen code".`},
					"full_context": map[string]any{"type": "boolean", "description": "Rarely needed — default false. When true, the job also receives a summary of your own recent activity in this conversation, for a task that genuinely depends on what you've been doing (not just a self-contained question). This makes the job more expensive and defeats most of the point of forking (keeping your context small doesn't help if the fork re-pays that cost) — prefer putting everything the task needs directly in the task text instead."},
				},
				"required": []string{"task", "label"},
			},
		},
	}
	if len(peers) > 0 {
		props := schema["function"].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)
		props["agent"] = map[string]any{
			"type":        "string",
			"enum":        peers,
			"description": "Optional. Run the job on this peer agent instead of a copy of yourself — its model, tools and MCP servers. Omit to fork yourself (the default). Use for work that needs a stronger or differently-equipped peer; it runs without waiting for you, unlike calling the agent_* tool.",
		}
	}
	return schema
}

// cancelBackgroundAgentSchema is the schema for the cancel_background_agent
// tool (docs/internal/analysis/prompt-context-management-review.md §8 rec #12) — the
// model-facing counterpart to the human-only `/bg stop` command, letting the
// spawning agent itself cancel a job it decided is no longer needed. Gated
// identically to spawnBackgroundAgentSchema (only appended when
// a.backgroundManager is set), and for the same reason: never offered to a
// background job's own tool list.
func cancelBackgroundAgentSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "cancel_background_agent",
			"description": "Cancel a background agent job you previously spawned via spawn_background_agent, by its job ID. Use this if you decide the answer is no longer needed (e.g. the user's request changed) — otherwise let it run to completion.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{"type": "string", "description": "The job ID returned by spawn_background_agent."},
				},
				"required": []string{"job_id"},
			},
		},
	}
}

// statusBackgroundAgentSchema is the schema for status_background_agent
// (issue #250) — checking on jobs spawned via spawn_background_agent.
// Gated identically to spawnBackgroundAgentSchema (only appended when
// a.backgroundManager is set, never in a background job's own tool list).
func statusBackgroundAgentSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "status_background_agent",
			"description": "Check on background agent job(s) you spawned via spawn_background_agent. With job_id: that job's state, task and result (the result is complete once finished). Without: every job as a one-line summary. Cheap, but you do not need to poll — you are notified when jobs complete; use this when you want facts now.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{"type": "string", "description": "Optional. The job ID returned by spawn_background_agent; omit to list all jobs."},
				},
				"required": []string{},
			},
		},
	}
}

// waitBackgroundAgentSchema is the schema for wait_background_agent (issue
// #250) — blocking on a job's result. Gated like spawnBackgroundAgentSchema.
func waitBackgroundAgentSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "wait_background_agent",
			"description": "Block until a background agent job finishes and return its result. Use only when your current work genuinely depends on the answer and you have nothing else to do meanwhile — normally you are notified on completion and should keep working. If the job is still running after timeout_secs (default 60), you get a 'still running' note instead: do not immediately wait again.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id":       map[string]any{"type": "string", "description": "The job ID returned by spawn_background_agent."},
					"timeout_secs": map[string]any{"type": "number", "description": "Optional. How long to wait (default 60, max 600) before returning a 'still running' note."},
				},
				"required": []string{"job_id"},
			},
		},
	}
}

// sendBackgroundAgentSchema is the schema for send_background_agent (issue
// #250) — follow-up input to a running job. Gated like
// spawnBackgroundAgentSchema.
func sendBackgroundAgentSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "send_background_agent",
			"description": "Deliver follow-up input to a *running* background agent job — a correction, a new constraint, or a piece of context it lacks (e.g. which test is failing). The input reaches the job as an extra user message at its next step. Fails for finished jobs (spawn a new job instead) and does not interrupt the job's current step.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id":  map[string]any{"type": "string", "description": "The job ID returned by spawn_background_agent."},
					"message": map[string]any{"type": "string", "description": "The follow-up input for the job. Self-contained: the job sees this plus its original task, not your conversation."},
				},
				"required": []string{"job_id", "message"},
			},
		},
	}
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

func dispatchTool(ctx context.Context, name, argsJSON string, sess *session.Session, mem *memory.Store, memCaller memory.Consumer, otelDir string, ts TaskStore) (string, bool) {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return toolResult{Error: "invalid arguments: " + err.Error()}.String(), false
	}

	cwd := ""
	if sess != nil {
		cwd = sess.CWD
	}

	switch name {
	case "bash":
		return runBash(ctx, args, cwd)
	case "bash_output":
		return runBashOutput(args)
	case "kill_bash":
		return runKillBash(ctx, args)
	case "find_files":
		return runFindFiles(ctx, args, cwd)
	case "project_map":
		return runProjectMap(ctx, args, cwd)
	case "grep":
		return runGrep(ctx, args, cwd)
	case "read_file":
		return runReadFile(args, cwd)
	case "write_file":
		out, esc := runWriteFile(args, cwd)
		traceFileChanges(sess, cwd, out, fileDelta{path: strArg(args, "path"), kind: session.ChangeCreated})
		return out, esc
	case "edit_file":
		out, esc := runEditFile(args, cwd)
		traceFileChanges(sess, cwd, out, fileDelta{path: strArg(args, "path"), kind: session.ChangeModified})
		return out, esc
	case "multiedit":
		out, esc := runMultiEdit(args, cwd)
		traceFileChanges(sess, cwd, out, fileDelta{path: strArg(args, "path"), kind: session.ChangeModified})
		return out, esc
	case "delete_file":
		out, esc := runDeleteFile(args, cwd)
		traceFileChanges(sess, cwd, out, fileDelta{path: strArg(args, "path"), kind: session.ChangeDeleted})
		return out, esc
	case "move_file":
		out, esc := runMoveFile(args, cwd)
		traceFileMove(sess, cwd, out, strArg(args, "source"), strArg(args, "destination"))
		return out, esc
	case "http_request":
		return runHTTPRequest(ctx, args)
	case "list_dir":
		return runListDir(args, cwd)
	case "web_fetch":
		return runWebFetch(ctx, args)
	case "http_get": // deprecated alias of web_fetch (kept for stale tool lists)
		return runWebFetch(ctx, args)
	case "get_session_context":
		return runGetSessionContext(sess, args), false
	case "record_memory":
		if mem != nil {
			return memory.DispatchRecordMemory(ctx, mem, argsJSON), false
		}
		return toolResult{Error: errMemUnavailable}.String(), false
	case "get_memory":
		if mem != nil {
			return memory.DispatchGetMemory(ctx, mem, argsJSON, memCaller), false
		}
		return toolResult{Error: errMemUnavailable}.String(), false
	case "list_memory":
		if mem != nil {
			return memory.DispatchListMemory(ctx, mem, argsJSON, memCaller), false
		}
		return toolResult{Error: errMemUnavailable}.String(), false
	case "forget_memory":
		if mem != nil {
			return memory.DispatchForgetMemory(ctx, mem, argsJSON), false
		}
		return toolResult{Error: errMemUnavailable}.String(), false
	case "board_get":
		return dispatchBoardGet(ctx), false
	case "board_claim":
		return dispatchBoardClaim(ctx, args), false
	case "board_update":
		return dispatchBoardUpdate(ctx, args), false
	case "board_post":
		return dispatchBoardPost(ctx, args), false
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
	case "milk_config_help":
		return runMilkConfigHelp(args)
	case "create_task", "update_task", "list_tasks", "complete_task":
		if ts == nil {
			return toolResult{Error: errTaskUnavailable}.String(), false
		}
		return dispatchTaskTool(name, argsJSON, ts), false
	default:
		return toolResult{Error: "unknown tool: " + name}.String(), false
	}
}

func dispatchTaskTool(name, argsJSON string, ts TaskStore) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return toolResult{Error: "invalid arguments: " + err.Error()}.String()
	}
	switch name {
	case "create_task":
		title, _ := args["title"].(string)
		if title == "" {
			return toolResult{Error: "title is required"}.String()
		}
		var tags []string
		if raw, ok := args["tags"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok {
					tags = append(tags, s)
				}
			}
		}
		t, err := ts.Create(title, tags)
		if err != nil {
			return toolResult{Error: err.Error()}.String()
		}
		out, _ := json.Marshal(map[string]string{"id": t.ID})
		return toolResult{Output: string(out)}.String()

	case "update_task":
		id, _ := args["id"].(string)
		status, _ := args["status"].(string)
		title, _ := args["title"].(string)
		if id == "" || status == "" {
			return toolResult{Error: "id and status are required"}.String()
		}
		if err := ts.Update(id, status, title); err != nil {
			return toolResult{Error: err.Error()}.String()
		}
		return toolResult{Output: "ok"}.String()

	case "list_tasks":
		includeGlobal, _ := args["include_global"].(bool)
		tasks, err := ts.List(includeGlobal)
		if err != nil {
			return toolResult{Error: err.Error()}.String()
		}
		out, _ := json.Marshal(tasks)
		return toolResult{Output: string(out)}.String()

	case "complete_task":
		id, _ := args["id"].(string)
		if id == "" {
			return toolResult{Error: "id is required"}.String()
		}
		if err := ts.Complete(id); err != nil {
			return toolResult{Error: err.Error()}.String()
		}
		return toolResult{Output: "ok"}.String()
	}
	return toolResult{Error: "unknown task tool: " + name}.String()
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

// runGetSessionContext formats session history with optional filters.
// Normal mode: recent contextRecentTurns are verbatim; older turns are compact
// one-liners with 1-based indices. If the older portion is ≤ contextOlderThreshold
// it is also returned verbatim (no indices needed for tiny histories).
// Range mode (turn_from set): returns a specific slice of at most
// contextRangeMaxTurns turns fully verbatim, bypassing the split logic.
const (
	contextRecentTurns    = 10 // tail returned verbatim in normal mode
	contextOlderThreshold = 5  // older portion: verbatim when ≤ this, else compact
	contextSummaryChars   = 80 // max content chars per line in compact summary
	contextRangeMaxTurns  = 5  // max turns fetchable via turn_from/turn_to
)

func runGetSessionContext(sess *session.Session, args map[string]any) string {
	if sess == nil || len(sess.History) == 0 {
		return toolResult{Output: "(no session history yet)"}.String()
	}

	// Range mode: turn_from/turn_to fetch a specific verbatim slice.
	if from := intArg(args, "turn_from"); from > 0 {
		all := applySessionFilters(sess.History, args)
		to := intArg(args, "turn_to")
		if to < from {
			to = from
		}
		if to-from+1 > contextRangeMaxTurns {
			to = from + contextRangeMaxTurns - 1
		}
		// clamp to valid indices (1-based)
		if from > len(all) {
			return toolResult{Output: "(turn index out of range)"}.String()
		}
		if to > len(all) {
			to = len(all)
		}
		slice := all[from-1 : to]
		var b strings.Builder
		fmt.Fprintf(&b, "[turns %d–%d, verbatim]\n", from, to)
		for _, t := range slice {
			appendTurn(&b, t)
		}
		return toolResult{Output: b.String()}.String()
	}

	turns := applySessionFilters(sess.History, args)
	if len(turns) == 0 {
		return toolResult{Output: "(no matching turns)"}.String()
	}

	split := len(turns) - contextRecentTurns
	if split < 0 {
		split = 0
	}
	older, recent := turns[:split], turns[split:]

	var b strings.Builder
	if len(older) > 0 {
		if len(older) <= contextOlderThreshold {
			for i, t := range older {
				appendTurnCompact(&b, t, i+1)
			}
		} else {
			fmt.Fprintf(&b, "[older history — %d turns, compact; use turn_from/turn_to to fetch verbatim]\n", len(older))
			for i, t := range older {
				appendTurnCompact(&b, t, i+1)
			}
			b.WriteString("[end older history]\n")
		}
	}
	for _, t := range recent {
		appendTurn(&b, t)
	}
	return toolResult{Output: b.String()}.String()
}

func appendTurnCompact(b *strings.Builder, t session.Turn, idx int) {
	prefix := fmt.Sprintf("[%d] ", idx)
	var role, content string
	switch t.Role {
	case session.RoleUser:
		role = "User"
		content = t.Content
	case session.RoleAssistant:
		if len(t.ToolCalls) > 0 {
			names := make([]string, 0, len(t.ToolCalls))
			for _, tc := range t.ToolCalls {
				names = append(names, tc.Name)
			}
			fmt.Fprintf(b, "%s%s: [tool calls: %s]\n", prefix, t.Agent, strings.Join(names, ", "))
			return
		}
		role = string(t.Agent)
		content = t.Content
	case session.RoleToolResult:
		content = t.Content
		if len(content) > contextSummaryChars {
			content = content[:contextSummaryChars] + "…"
		}
		fmt.Fprintf(b, "%s[tool result] %s\n", prefix, content)
		return
	default:
		return
	}
	if len(content) > contextSummaryChars {
		content = content[:contextSummaryChars] + "…"
	}
	fmt.Fprintf(b, "%s%s: %s\n", prefix, role, content)
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
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
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

const (
	bashDefaultTimeout = 120 * time.Second
	bashMaxTimeout     = 600 * time.Second
)

func runBash(ctx context.Context, args map[string]any, cwd string) (string, bool) {
	command, _ := args["command"].(string)
	dir := cwd
	if wd, _ := args["workdir"].(string); wd != "" {
		dir = resolvePath(wd, cwd)
	}

	// run_in_background: start a long-lived shell and return its ID
	// immediately (ADR-0063). timeout_seconds does not apply — the shell is
	// bounded by the registry's lifetime cap and kill_bash instead.
	if bg, _ := args["run_in_background"].(bool); bg {
		s, err := bgShells.start(command, dir, shellActor(ctx))
		if err != nil {
			return toolResult{Error: err.Error(), ExitCode: -1}.String(), false
		}
		return toolResult{Output: fmt.Sprintf(
			"started background shell %s — read its output with bash_output(shell_id: %q), stop it with kill_bash(shell_id: %q)",
			s.id, s.id, s.id)}.String(), false
	}

	timeout := bashDefaultTimeout
	if v, ok := args["timeout_seconds"]; ok {
		if n, ok := v.(float64); ok && n > 0 {
			// n * time.Second via float — time.Duration(n) truncates
			// fractions to 0, silently killing fractional timeouts (0.3s
			// became an already-expired deadline).
			d := time.Duration(n * float64(time.Second))
			if d > bashMaxTimeout {
				d = bashMaxTimeout
			}
			timeout = d
		}
	}

	// Issue #275: the kill message distinguishes who cut the run, so the
	// configured limit is never presented as the time it actually ran.
	// ourDeadline is what THIS call asked for — ctx.Deadline() can't be
	// used for that: context.WithDeadline hands back the parent's context
	// when the parent's deadline is earlier (the job hard-deadline shape),
	// which would report the parent's deadline as our own.
	start := time.Now()
	ourDeadline := start.Add(timeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	if dir != "" {
		cmd.Dir = dir
	}
	setProcGroup(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		// Launch failures, wait errors that are not an exit status, and
		// timeouts all report ExitCode -1: the exit code is the single
		// failure signal for bash (issue #271) — a result with Error set but
		// exit 0 is stderr output on a SUCCESSFUL run and fails nothing.
		// A ctx already dead at launch gets the uniform kill message too
		// (issue #275) instead of a bare "context deadline exceeded".
		if ctx.Err() != nil {
			return toolResult{Error: bashKilledMessage(ctx, start, ourDeadline, timeout), ExitCode: -1}.String(), false
		}
		return toolResult{Error: err.Error(), ExitCode: -1}.String(), false
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		// A ctx kill can win the race on either channel: Wait may return
		// before the select takes the ctx.Done arm (ExitError "signal:
		// killed", or the context error itself). A real finish still wins —
		// err==nil — but anything else cut by ctx reports the killed
		// message (issue #275), never "signal: killed" or a bare
		// "context deadline exceeded".
		if err == nil {
			return toolResult{
				Output:   stdout.String(),
				Error:    stderr.String(),
				ExitCode: 0,
			}.String(), false
		}
		if ctx.Err() != nil {
			killProcGroup(cmd)
			return toolResult{Error: bashKilledMessage(ctx, start, ourDeadline, timeout), ExitCode: -1}.String(), false
		}
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode() // -1 when a signal killed the process
		} else {
			return toolResult{Error: err.Error(), ExitCode: -1}.String(), false
		}
		return toolResult{
			Output:   stdout.String(),
			Error:    stderr.String(),
			ExitCode: code,
		}.String(), false
	case <-ctx.Done():
		killProcGroup(cmd)
		<-done
		return toolResult{Error: bashKilledMessage(ctx, start, ourDeadline, timeout), ExitCode: -1}.String(), false
	}
}

// bashKilledMessage explains a ctx-killed bash run (issue #275). A kill from
// outside — a cancelled turn, a background job's hard deadline, a parent
// deadline — reports the time the command actually ran alongside the
// configured limit ("killed by context after 1m59s (limit 5m0s)"), so a
// 2-minute job cut never claims the 5-minute bash limit as its duration. A
// run that consumed its own configured timeout keeps the plain wording —
// there the elapsed time IS the limit. ourDeadline is this call's own
// deadline (see runBash), not ctx's: a parent deadline earlier than ours
// makes WithDeadline return the parent, so ctx.Deadline() would misreport
// whose deadline fired.
func bashKilledMessage(ctx context.Context, start, ourDeadline time.Time, limit time.Duration) string {
	elapsed := time.Since(start).Round(time.Millisecond)
	external := ctx.Err() == context.Canceled || time.Now().Before(ourDeadline)
	if external {
		return fmt.Sprintf("bash: command killed by context after %s (limit %s)", elapsed, limit)
	}
	return fmt.Sprintf("bash: command timed out after %s", limit)
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

// resolvePath expands "~", anchors a relative path at the session cwd (not the
// process cwd — over ACP session/new carries its own cwd, so the two can
// differ), and cleans the result. Absolute paths pass through unchanged.
func resolvePath(path, cwd string) string {
	path = expandTilde(path)
	if path == "" {
		return path
	}
	if !filepath.IsAbs(path) && cwd != "" {
		path = filepath.Join(cwd, path)
	}
	return filepath.Clean(path)
}

// runMilkConfigHelp looks up a self-config doc section by topic, or lists
// available topics when none is given. Read-only, so it is not registered in
// toolNeedsPermission.
func runMilkConfigHelp(args map[string]any) (string, bool) {
	topic, _ := args["topic"].(string)
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return toolResult{Output: "topics: " + strings.Join(selfdocs.Topics(), ", ")}.String(), false
	}
	body, ok := selfdocs.Lookup(topic)
	if !ok {
		return toolResult{Error: fmt.Sprintf("no doc section for %q — topics: %s", topic, strings.Join(selfdocs.Topics(), ", "))}.String(), false
	}
	return toolResult{Output: body}.String(), false
}

// findFilesMaxResults is the default cap for find_files; a truncated result
// says how many matches were withheld.
const findFilesMaxResults = 500

// grepMaxResults is the default cap for grep (matching lines).
const grepMaxResults = 200

// walkSkipDirs are directory names never descended into by the find_files
// walk fallback (when git enumeration is unavailable). The git path honors
// .gitignore properly; this list is the crude substitute.
var walkSkipDirs = map[string]bool{".git": true, ".claude": true, "node_modules": true, "vendor": true}

// globToRegex converts a glob to an anchored regexp: `*` within a segment,
// `?` single char, and `**` for any depth (`**/` matching zero or more
// directories).
func globToRegex(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:[^/]+/)*")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			if strings.ContainsRune(`\.+()|[]{}^$`, rune(pattern[i])) {
				b.WriteString(`\`)
			}
			b.WriteByte(pattern[i])
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// matchGlob reports whether rel matches pattern. A pattern containing "/" is
// matched against the whole relative path; otherwise against the basename.
func matchGlob(pattern, rel string) bool {
	re := globToRegex(pattern)
	target := rel
	if !strings.Contains(pattern, "/") {
		target = filepath.Base(rel)
	}
	return re.MatchString(target)
}

// gitEnumerate lists files under root relative to it via git (tracked +
// untracked, honoring .gitignore). ok=false when git is missing or root is
// not inside a work tree — callers fall back to the directory walk.
func gitEnumerate(ctx context.Context, root string) ([]string, bool) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, false
	}
	if err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		return nil, false
	}
	cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, false
	}
	var out []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out, true
}

// walkEnumerate lists files under root relative to it via a plain walk,
// skipping walkSkipDirs.
func walkEnumerate(root string) []string {
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error { //nolint:errcheck
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if walkSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr == nil {
			out = append(out, rel)
		}
		return nil
	})
	return out
}

func runFindFiles(ctx context.Context, args map[string]any, cwd string) (string, bool) {
	path, _ := args["path"].(string)
	path = expandTilde(path)
	if path == "" {
		path = "."
	}
	if !filepath.IsAbs(path) && cwd != "" {
		path = filepath.Join(cwd, path)
	}
	root := filepath.Clean(path)
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return toolResult{Error: "pattern is required"}.String(), false
	}

	relFiles, ok := gitEnumerate(ctx, root)
	if !ok {
		relFiles = walkEnumerate(root)
	}

	type entry struct {
		rel   string
		mtime time.Time
	}
	var matches []entry
	for _, rel := range relFiles {
		if !matchGlob(pattern, filepath.ToSlash(rel)) {
			continue
		}
		var mtime time.Time
		if info, err := os.Stat(filepath.Join(root, rel)); err == nil {
			mtime = info.ModTime()
		}
		matches = append(matches, entry{rel, mtime})
	}
	// Newest first (MiMo-Code's ordering): recent files are what a model
	// almost always means by a filename search.
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].mtime.Equal(matches[j].mtime) {
			return matches[i].rel < matches[j].rel
		}
		return matches[i].mtime.After(matches[j].mtime)
	})

	truncated := 0
	if len(matches) > findFilesMaxResults {
		truncated = len(matches) - findFilesMaxResults
		matches = matches[:findFilesMaxResults]
	}

	var lines []string
	for _, m := range matches {
		lines = append(lines, filepath.Join(root, m.rel))
	}
	out := strings.Join(lines, "\n")
	if truncated > 0 {
		out += fmt.Sprintf("\n[truncated: %d more matches; refine the pattern or narrow path]", truncated)
	}
	return toolResult{Output: out}.String(), false
}

// grepMaxLineChars bounds one result line: a match inside minified JS/HTML or
// a generated file can be hundreds of KB on a single line.
const grepMaxLineChars = 2000

// capLongLines truncates every line longer than max bytes (on a rune
// boundary), marking how much was cut.
func capLongLines(out string, max int) string {
	if len(out) <= max {
		return out
	}
	lines := strings.Split(out, "\n")
	changed := false
	for i, l := range lines {
		if len(l) <= max {
			continue
		}
		cut := max
		for cut > 0 && !utf8.RuneStart(l[cut]) {
			cut--
		}
		lines[i] = l[:cut] + fmt.Sprintf("… [line truncated: %d more bytes]", len(l)-cut)
		changed = true
	}
	if !changed {
		return out
	}
	return strings.Join(lines, "\n")
}

// gitGrepCmd builds a `git grep` invocation equivalent to the rg/grep paths
// (extended regex, line numbers, binary files skipped) for path inside a git
// work tree, searching tracked plus untracked-but-not-ignored files. ok is
// false when git is missing or path is not in a work tree. root is the
// directory git runs in; its output paths are relative to it.
func gitGrepCmd(ctx context.Context, pattern, path string, ignoreCase bool, contextLines int, include string) (cmd *exec.Cmd, root string, ok bool) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, "", false
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", false
	}
	dir, spec := path, ""
	if !info.IsDir() {
		dir, spec = filepath.Dir(path), filepath.Base(path)
	}
	if err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		return nil, "", false
	}
	gargs := []string{"-C", dir, "grep", "--untracked", "--exclude-standard", "-I", "-n", "--heading", "--break", "-E", "--color=never"}
	if ignoreCase {
		gargs = append(gargs, "-i")
	}
	if contextLines > 0 {
		gargs = append(gargs, "-C", strconv.Itoa(contextLines))
	}
	gargs = append(gargs, "-e", pattern, "--")
	switch {
	case spec != "":
		gargs = append(gargs, spec)
	case include != "":
		gargs = append(gargs, ":(glob)**/"+include)
	}
	return exec.CommandContext(ctx, "git", gargs...), dir, true
}

// absolutizeGitGrep rewrites `git grep --heading --break` output (a filename
// line, then "123:text" match / "123-text" context lines, a blank line between
// files, "--" between context groups) into rg's one-line absolute form
// ("/root/path:123:text" / "/root/path-123-text"). Every content line carries
// a "<digits><sep>" prefix, so a blank line unambiguously starts a new file
// and filenames containing ':' or '-<digits>-' cannot confuse the parse.
func absolutizeGitGrep(out, root string) string {
	if out == "" {
		return out
	}
	var b strings.Builder
	var abs string
	expectFile := true
	for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		switch {
		case expectFile:
			abs = filepath.Join(root, l)
			expectFile = false
		case l == "":
			expectFile = true
		case l == "--":
			b.WriteString("--\n")
		default:
			j := 0
			for j < len(l) && l[j] >= '0' && l[j] <= '9' {
				j++
			}
			sep := ":"
			if j < len(l) && l[j] == '-' {
				sep = "-"
			}
			b.WriteString(abs + sep + l + "\n")
		}
	}
	return b.String()
}

func runGrep(ctx context.Context, args map[string]any, cwd string) (string, bool) {
	pattern, _ := args["pattern"].(string)
	path, _ := args["path"].(string)
	path = expandTilde(path)
	if path == "" {
		path = "."
	}
	if !filepath.IsAbs(path) && cwd != "" {
		path = filepath.Join(cwd, path)
	}
	ignoreCase, _ := args["ignore_case"].(bool)
	contextLines := intArg(args, "context")
	include, _ := args["include"].(string)
	maxResults := intArg(args, "max_results")
	if maxResults <= 0 {
		maxResults = grepMaxResults
	}

	var cmd *exec.Cmd
	var gitRoot string // set when git grep ran: it prints paths relative to this dir
	if _, err := exec.LookPath("rg"); err == nil {
		// ripgrep: extended regex by default, honors .gitignore/.rgignore,
		// skips binary files and hidden dirs.
		gargs := []string{"--line-number", "--color", "never", "-e", pattern}
		if ignoreCase {
			gargs = append(gargs, "-i")
		}
		if contextLines > 0 {
			gargs = append(gargs, "-C", strconv.Itoa(contextLines))
		}
		if include != "" {
			gargs = append(gargs, "--glob", include)
		}
		gargs = append(gargs, path)
		cmd = exec.CommandContext(ctx, "rg", gargs...)
	} else if gc, root, ok := gitGrepCmd(ctx, pattern, path, ignoreCase, contextLines, include); ok {
		// No ripgrep: git grep gives the same .gitignore-honoring search
		// inside a work tree, so a gitignored build/site directory never
		// floods the result.
		cmd = gc
		gitRoot = root
	} else {
		// Neither rg nor a git work tree: search natively (no external grep).
		out, code, errMsg := nativeGrep(ctx, pattern, path, ignoreCase, contextLines, include, maxResults)
		return toolResult{Output: capLongLines(out, grepMaxLineChars), Error: errMsg, ExitCode: code}.String(), false
	}

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

	out := stdout.String()
	if gitRoot != "" {
		out = absolutizeGitGrep(out, gitRoot)
	}
	out = capLongLines(out, grepMaxLineChars)
	// Cap matching lines (context lines ride along with their match).
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if out != "" && len(lines) > maxResults {
		omitted := len(lines) - maxResults
		out = strings.Join(lines[:maxResults], "\n") +
			fmt.Sprintf("\n[truncated: %d more matching lines; refine the pattern or raise max_results]", omitted)
	}
	return toolResult{
		Output:   out,
		Error:    stderr.String(),
		ExitCode: code,
	}.String(), false
}

func runWriteFile(args map[string]any, cwd string) (string, bool) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	path = resolvePath(path, cwd)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	if err := writeFileAtomic(path, []byte(content), 0o644); err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	return toolResult{Output: "wrote " + path}.String(), false
}

func runReadFile(args map[string]any, cwd string) (string, bool) {
	path, _ := args["path"].(string)
	path = resolvePath(path, cwd)

	data, err := os.ReadFile(path)
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}

	lines := strings.Split(string(data), "\n")

	// offset is a 1-based line number matching the output numbering, so a
	// model that sees line 120 and requests offset=120 gets line 120 (the
	// old 0-based offset silently skipped it).
	offset := 1
	if v, ok := args["offset"]; ok {
		switch n := v.(type) {
		case float64:
			offset = int(n)
		case string:
			offset, _ = strconv.Atoi(n)
		}
		if offset < 1 {
			offset = 1
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

	start := min(offset-1, len(lines))
	end := min(start+limit, len(lines))

	numbered := make([]string, 0, end-start+1)
	for i, l := range lines[start:end] {
		numbered = append(numbered, fmt.Sprintf("%d\t%s", start+i+1, l))
	}
	out := strings.Join(numbered, "\n")
	if !limitSet && end < len(lines) {
		out += fmt.Sprintf("\n\n[truncated: showed lines %d-%d of %d; use offset+limit to read more]", start+1, end, len(lines))
	}
	return toolResult{Output: out}.String(), false
}

func runEditFile(args map[string]any, cwd string) (string, bool) {
	path, _ := args["path"].(string)
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)
	replaceAll, _ := args["replace_all"].(bool)
	path = resolvePath(path, cwd)

	data, err := os.ReadFile(path)
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}

	updated, count, strategy, err := applyEdit(string(data), oldStr, newStr, path, replaceAll)
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	if err := writeFileAtomic(path, []byte(updated), 0o644); err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	fuzzy := ""
	if strategy != "exact" {
		fuzzy = " (fuzzy match: " + strategy + ")"
	}
	if replaceAll {
		return toolResult{Output: fmt.Sprintf("edited %s (%d replacements)%s", path, count, fuzzy)}.String(), false
	}
	return toolResult{Output: "edited " + path + fuzzy}.String(), false
}

// runMultiEdit applies a batch of edits to one file, all-or-nothing (issue
// #251): every edit is validated and applied in memory — in order, each
// seeing the previous edit's result, with exactly applyEdit's per-edit
// matching rules — and the file is written once at the end. A failing edit
// discards the whole batch (nothing is written) and reports itself plus the
// usual closest-match hint. One round trip replaces a burst of edit_file
// calls; false-positive risk is not compounded because each edit still
// requires a unique match (or an explicit replace_all).
func runMultiEdit(args map[string]any, cwd string) (string, bool) {
	path, _ := args["path"].(string)
	path = resolvePath(path, cwd)
	rawEdits, _ := args["edits"].([]any)
	if len(rawEdits) == 0 {
		return toolResult{Error: "edits must not be empty"}.String(), false
	}
	type editSpec struct {
		oldStr, newStr string
		replaceAll     bool
	}
	edits := make([]editSpec, 0, len(rawEdits))
	for i, raw := range rawEdits {
		m, ok := raw.(map[string]any)
		if !ok {
			return toolResult{Error: fmt.Sprintf("edits[%d] must be an object with old_string and new_string", i)}.String(), false
		}
		oldStr, _ := m["old_string"].(string)
		newStr, _ := m["new_string"].(string)
		replaceAll, _ := m["replace_all"].(bool)
		if oldStr == "" {
			return toolResult{Error: fmt.Sprintf("edits[%d]: old_string must not be empty", i)}.String(), false
		}
		edits = append(edits, editSpec{oldStr, newStr, replaceAll})
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	content := string(data)
	strategies := make([]string, len(edits))
	counts := make([]int, len(edits))
	for i, e := range edits {
		var strategy string
		var err error
		label := fmt.Sprintf("%s (multiedit edit %d/%d)", path, i+1, len(edits))
		content, counts[i], strategy, err = applyEdit(content, e.oldStr, e.newStr, label, e.replaceAll)
		if err != nil {
			return toolResult{Error: fmt.Sprintf("edit %d of %d not applied — no changes were written to %s.\n%v", i+1, len(edits), path, err)}.String(), false
		}
		strategies[i] = strategy
	}
	if err := writeFileAtomic(path, []byte(content), 0o644); err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}

	var notes []string
	for i := range edits {
		if strategies[i] != "exact" {
			notes = append(notes, fmt.Sprintf("edit %d fuzzy match: %s", i+1, strategies[i]))
		}
		if counts[i] > 1 {
			notes = append(notes, fmt.Sprintf("edit %d: %d replacements", i+1, counts[i]))
		}
	}
	out := fmt.Sprintf("edited %s (%d edits)", path, len(edits))
	if len(notes) > 0 {
		out += " — " + strings.Join(notes, "; ")
	}
	return toolResult{Output: out}.String(), false
}

func runListDir(args map[string]any, cwd string) (string, bool) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	path = resolvePath(path, cwd)

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

func runDeleteFile(args map[string]any, cwd string) (string, bool) {
	path, _ := args["path"].(string)
	path = resolvePath(path, cwd)
	if err := os.Remove(path); err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	return toolResult{Output: "deleted " + path}.String(), false
}

func runMoveFile(args map[string]any, cwd string) (string, bool) {
	src, _ := args["source"].(string)
	dst, _ := args["destination"].(string)
	src = resolvePath(src, cwd)
	dst = resolvePath(dst, cwd)
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

// web_fetch (toolset-review G5): the reading half of the old http_get, with
// HTTPS upgrade, HTML→Markdown and a body budget. http_get remains as a
// deprecated alias dispatching here.
const (
	webFetchDefaultMaxBytes = 262144
	webFetchHardMaxBytes    = 5 << 20
)

// upgradeHTTPS returns the https:// variant of a plain-http:// URL and
// whether an upgrade should be attempted — never for local hosts, where TLS
// is usually absent and the redirect dance just burns a request.
func upgradeHTTPS(rawURL string) (string, bool) {
	if !strings.HasPrefix(rawURL, "http://") {
		return rawURL, false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL, false
	}
	h := strings.ToLower(u.Hostname())
	if h == "localhost" || h == "::1" || h == "0.0.0.0" ||
		strings.HasPrefix(h, "127.") || h == "[::1]" {
		return rawURL, false
	}
	return "https://" + strings.TrimPrefix(rawURL, "http://"), true
}

// isHTMLContent sniffs whether a body is HTML (by Content-Type, falling back
// to the first bytes).
func isHTMLContent(contentType, body string) bool {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "text/html") {
		return true
	}
	if ct != "" && !strings.Contains(ct, "text/") {
		return false
	}
	head := strings.ToLower(body)
	if len(head) > 512 {
		head = head[:512]
	}
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")
}

func runWebFetch(ctx context.Context, args map[string]any) (string, bool) {
	url, _ := args["url"].(string)
	format, _ := args["format"].(string)
	maxBytes := webFetchDefaultMaxBytes
	if v := intArg(args, "max_bytes"); v > 0 {
		maxBytes = min(v, webFetchHardMaxBytes)
	}

	fetch := func(u string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "milk-web-fetch/1.0")
		req.Header.Set("Accept", "text/html, text/plain, text/markdown, application/json;q=0.9, */*;q=0.5")
		return http.DefaultClient.Do(req)
	}

	// HTTPS upgrade for non-local plain-http URLs, with a plain-HTTP fallback
	// when the upgrade fails (old docs servers, TLS-less hosts).
	target := url
	upgraded, tryUpgrade := upgradeHTTPS(url)
	resp, err := fetch(target)
	if tryUpgrade {
		if err != nil || resp.StatusCode >= 400 {
			if resp != nil {
				resp.Body.Close() //nolint:errcheck
			}
			target = upgraded
			resp, err = fetch(target)
		}
	}
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}
	truncated := len(body) > maxBytes
	if truncated {
		body = body[:maxBytes]
	}

	text := string(body)
	ct := resp.Header.Get("Content-Type")
	htmlish := isHTMLContent(ct, text)
	switch strings.ToLower(format) {
	case "html", "raw":
		// keep bytes as-is
	case "text":
		if htmlish {
			text = htmlToMarkdown(text) // same cleanup pass, no markdown syntax
			text = strings.ReplaceAll(text, "`", "")
			text = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`).ReplaceAllString(text, "$1")
		}
	default: // "auto", "markdown", "md", ""
		if htmlish {
			text = htmlToMarkdown(text)
		}
	}
	if truncated {
		text += fmt.Sprintf("\n[truncated at max_bytes=%d — raise max_bytes or fetch a narrower URL]", maxBytes)
	}
	return toolResult{Output: text, ExitCode: resp.StatusCode}.String(), false
}

const postEditOutputMaxBytes = 4000

// postEditCheck runs the configured post-edit command after a successful
// edit_file/write_file and appends its (capped) output to the tool result —
// the model learns about compile/format problems without a separate build
// call (toolset-review G2). Never runs for a failed edit.
func (a *Agent) postEditCheck(name, result, cwd string) string {
	if a.postEditCommand == "" || (name != "edit_file" && name != "multiedit" && name != "write_file") {
		return result
	}
	var r toolResult
	if json.Unmarshal([]byte(result), &r) != nil || r.Error != "" {
		return result
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", a.postEditCommand)
	if cwd != "" {
		cmd.Dir = cwd
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			code = -1
		}
	}

	out := stdout.String()
	if s := stderr.String(); s != "" {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += s
	}
	out = truncateHeadAndTail(out, postEditOutputMaxBytes)

	summary := "\n[post-edit check: " + a.postEditCommand
	if code == 0 {
		summary += " — ok"
	} else {
		summary += fmt.Sprintf(" — exit %d", code)
	}
	summary += "]"
	if out != "" {
		summary += "\n" + out
	}
	r.Output += summary
	b, err := json.Marshal(r)
	if err != nil {
		return result
	}
	return string(b)
}
