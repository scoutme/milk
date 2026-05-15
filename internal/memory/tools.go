package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Schemas returns OpenAI function schemas for record_memory, get_memory, and list_memory.
func Schemas() []map[string]any {
	return []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "record_memory",
				"description": "Record a fact, preference, or decision worth remembering across sessions. Use when the user states a preference, makes a decision, or shares a fact that should persist.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content": map[string]any{
							"type":        "string",
							"description": "Natural-language assertion to remember (e.g. 'User prefers flat file output over JSON').",
						},
						"subject": map[string]any{
							"type":        "string",
							"description": "Short subject label for grouping (e.g. 'user preferences', 'project setup').",
						},
					},
					"required": []string{"content"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "get_memory",
				"description": "Retrieve Percepts (remembered facts) relevant to a query. Call before answering questions that reference past context or stated preferences.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"description": "Keywords or phrase describing what to recall.",
						},
						"min_confidence": map[string]any{
							"type":        "number",
							"description": "Minimum confidence weight [0,1] (default 0.4).",
						},
						"max_results": map[string]any{
							"type":        "integer",
							"description": "Maximum number of results to return (default 5).",
						},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "list_memory",
				"description": "List Percepts stored in memory, optionally filtered by scope, producer, confidence, or keyword. Use this to inspect what the memory store currently contains.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"scope": map[string]any{
							"type":        "string",
							"enum":        []string{"global", "session", ""},
							"description": "Restrict to global or session scope. Omit for both.",
						},
						"producer": map[string]any{
							"type":        "string",
							"enum":        []string{"user", "local", "claude", "system", ""},
							"description": "Filter by who created the Percept. Omit for all producers.",
						},
						"min_w": map[string]any{
							"type":        "number",
							"description": "Minimum confidence weight [0,1]. Omit for no floor.",
						},
						"pattern": map[string]any{
							"type":        "string",
							"description": "Case-insensitive substring to match against content.",
						},
					},
					"required": []string{},
				},
			},
		},
	}
}

// DispatchRecordMemory handles a record_memory tool call.
func DispatchRecordMemory(ctx context.Context, store *Store, argsJSON string) string {
	var args struct {
		Content string `json:"content"`
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return errResult(invalidArgs + err.Error())
	}
	if strings.TrimSpace(args.Content) == "" {
		return errResult("content is required")
	}
	roles := Roles{}
	if args.Subject != "" {
		roles.Theme = args.Subject
	}
	id, err := store.Record(ctx, args.Content, ProducerLocal, roles, false)
	if err != nil {
		return errResult("failed to record: " + err.Error())
	}
	return okResult(fmt.Sprintf("recorded percept %s", id[:8]))
}

// DispatchGetMemory handles a get_memory tool call.
func DispatchGetMemory(ctx context.Context, store *Store, argsJSON string) string {
	var args struct {
		Query         string  `json:"query"`
		MinConfidence float64 `json:"min_confidence"`
		MaxResults    int     `json:"max_results"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return errResult(invalidArgs + err.Error())
	}
	if args.MinConfidence == 0 {
		args.MinConfidence = 0.4
	}
	if args.MaxResults == 0 {
		args.MaxResults = 5
	}

	percepts := store.Query(ctx, args.Query, args.MinConfidence, args.MaxResults)
	if len(percepts) == 0 {
		return okResult("(no relevant memories found)")
	}

	var b strings.Builder
	for _, p := range percepts {
		fmt.Fprintf(&b, "[%.2f] %s\n", p.W, p.Content)
	}
	return okResult(b.String())
}

// DispatchListMemory handles a list_memory tool call.
func DispatchListMemory(_ context.Context, store *Store, argsJSON string) string {
	var args struct {
		Scope    string  `json:"scope"`
		Producer string  `json:"producer"`
		MinW     float64 `json:"min_w"`
		Pattern  string  `json:"pattern"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return errResult(invalidArgs + err.Error())
	}
	percepts := store.List(ListOpts{
		Scope:    args.Scope,
		Producer: args.Producer,
		MinW:     args.MinW,
		Pattern:  args.Pattern,
	})
	if len(percepts) == 0 {
		return okResult("(no percepts found)")
	}
	return okResult(FormatList(percepts))
}

// FormatList renders a human-readable table of Percepts.
func FormatList(percepts []Percept) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-8s  %-6s  %-7s  %-5s  %-10s  %s\n", "ID", "SCOPE", "W", "CORE", "PRODUCER", "CONTENT")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 80))
	for _, p := range percepts {
		scope := "session"
		if p.Core {
			scope = "global"
		}
		core := ""
		if p.Core {
			core = "yes"
		}
		content := p.Content
		if len(content) > 50 {
			content = content[:47] + "..."
		}
		fmt.Fprintf(&b, "%-8s  %-6s  %-7.2f  %-5s  %-10s  %s\n",
			p.ID[:8], scope, p.W, core, string(p.Producer), content)
	}
	return b.String()
}

// FormatListVerbose renders a full multi-line listing with all Percept fields.
func FormatListVerbose(percepts []Percept) string {
	var b strings.Builder
	for i, p := range percepts {
		if i > 0 {
			fmt.Fprintln(&b)
		}
		scope := "session"
		if p.Core {
			scope = "global"
		}
		fmt.Fprintf(&b, "[%d] %s  scope=%s  W=%.2f  producer=%s  core=%v\n",
			i+1, p.ID[:8], scope, p.W, p.Producer, p.Core)
		fmt.Fprintf(&b, "    content: %s\n", p.Content)
		fmt.Fprintf(&b, "    created: %s\n", p.CreatedAt.Format(time.DateTime))
		if p.Roles.Theme != "" {
			fmt.Fprintf(&b, "    theme:   %s\n", p.Roles.Theme)
		}
	}
	return b.String()
}

const invalidArgs = "invalid arguments: "

func okResult(output string) string {
	b, _ := json.Marshal(map[string]any{"output": output})
	return string(b)
}

func errResult(msg string) string {
	b, _ := json.Marshal(map[string]any{"error": msg})
	return string(b)
}
