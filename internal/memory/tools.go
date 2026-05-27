package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Schemas returns OpenAI function schemas for record_memory, get_memory, list_memory, and forget_memory.
func Schemas() []map[string]any {
	return []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "record_memory",
				"description": "Record a fact, preference, or decision worth remembering across sessions. Use when the user states a preference, makes a decision, or shares a fact that should persist. Set producer='user' when the fact was explicitly stated by the user; omit or use 'local'/'claude' for inferred facts.",
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
						"producer": map[string]any{
							"type":        "string",
							"enum":        []string{"user", "local", "claude"},
							"description": "Who is the source of this fact. Use 'user' when the user directly stated it; defaults to the calling agent.",
						},
						"consumer": map[string]any{
							"type":        "string",
							"enum":        []string{"local", "claude", ""},
							"description": "Which agent receives this at injection time. 'local' = only the local model; 'claude' = only Claude; omit or empty = both.",
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
		{
			"type": "function",
			"function": map[string]any{
				"name":        "forget_memory",
				"description": "Delete a stored Percept by its ID or 8-char prefix. Use when the user explicitly asks to forget or remove a specific memory.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id": map[string]any{
							"type":        "string",
							"description": "Full Percept ID or the 8-character prefix shown by list_memory.",
						},
					},
					"required": []string{"id"},
				},
			},
		},
	}
}

// DispatchForgetMemory handles a forget_memory tool call.
func DispatchForgetMemory(_ context.Context, store *Store, argsJSON string) string {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return errResult(invalidArgs + err.Error())
	}
	if strings.TrimSpace(args.ID) == "" {
		return errResult("id is required")
	}

	// Strip optional leading '#' so both "#abc123" and "abc123" work.
	id := strings.TrimPrefix(args.ID, "#")

	// Support prefix matching via FindByIDPrefix.
	candidates := store.FindByIDPrefix(id)
	switch len(candidates) {
	case 0:
		return okResult(fmt.Sprintf("percept %s not found", id))
	case 1:
		ok, err := store.Delete(candidates[0].ID)
		if err != nil {
			return errResult("failed to delete: " + err.Error())
		}
		if !ok {
			return okResult(fmt.Sprintf("percept %s not found", id))
		}
		return okResult(fmt.Sprintf("percept %s deleted", candidates[0].ID[:8]))
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "ambiguous prefix %q matches %d percepts — use a longer prefix:\n", id, len(candidates))
		for _, p := range candidates {
			fmt.Fprintf(&b, "  %s  %s\n", p.ID[:8], p.Content)
		}
		return errResult(b.String())
	}
}

// DispatchRecordMemory handles a record_memory tool call.
func DispatchRecordMemory(ctx context.Context, store *Store, argsJSON string) string {
	var args struct {
		Content  string `json:"content"`
		Subject  string `json:"subject"`
		Producer string `json:"producer"`
		Consumer string `json:"consumer"`
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
	producer := ProducerLocal
	switch args.Producer {
	case "user":
		producer = ProducerUser
	case "claude":
		producer = ProducerClaude
	}
	var consumer Consumer
	switch args.Consumer {
	case "local":
		consumer = ConsumerLocal
	case "claude":
		consumer = ConsumerClaude
	}
	id, err := store.Record(ctx, args.Content, producer, consumer, roles, false)
	if dup, ok := IsDuplicate(err); ok {
		return okResult(fmt.Sprintf("skipped — similar percept already exists: %s (%.0f%% overlap): %s",
			id[:8], dup.Similarity*100, dup.Existing.Content))
	}
	if err != nil {
		return errResult("failed to record: " + err.Error())
	}
	return okResult(fmt.Sprintf("recorded percept %s", id[:8]))
}

// DispatchGetMemory handles a get_memory tool call.
// caller restricts which percepts are visible (ConsumerAll = no restriction).
func DispatchGetMemory(ctx context.Context, store *Store, argsJSON string, caller Consumer) string {
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

	percepts := store.Query(ctx, args.Query, args.MinConfidence, args.MaxResults, caller)
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
	fmt.Fprintf(&b, "%-8s  %-6s  %-7s  %-5s  %-10s  %-6s  %s\n", "ID", "SCOPE", "W", "CORE", "PRODUCER", "FOR", "CONTENT")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 88))
	for _, p := range percepts {
		scope := "session"
		if p.Core {
			scope = "global"
		}
		core := ""
		if p.Core {
			core = "yes"
		}
		consumer := string(p.Consumer)
		if consumer == "" {
			consumer = "all"
		}
		content := p.Content
		if len(content) > 50 {
			content = content[:47] + "..."
		}
		fmt.Fprintf(&b, "%-8s  %-6s  %-7.2f  %-5s  %-10s  %-6s  %s\n",
			p.ID[:8], scope, p.W, core, string(p.Producer), consumer, content)
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
