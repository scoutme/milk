package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Schemas returns OpenAI function schemas for record_memory, get_memory, list_memory, and forget_memory.
//
// The contract documented here must stay in sync with the memory-tool mandate
// in internal/agent/local's systemPromptShared / backgroundSystemPrompt
// (issue #172: prompt text and tool definitions describe the same contract).
func Schemas() []map[string]any {
	return []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "record_memory",
				"description": "Persist a fact, preference, or decision across sessions. Re-stating a similar fact re-affirms the existing percept (one corroboration per run; repetition within a run adds nothing) instead of duplicating it. Use relation/related_to to link the fact to an existing percept: extends/derives corroborate the related one, updates corroborates this fact and supersedes the related one (it fades faster and cannot be promoted), contradicts marks a conflict (both are weighed at memory consolidation).",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content":    map[string]any{"type": "string", "description": "Fact to remember."},
						"subject":    map[string]any{"type": "string", "description": "Short label for grouping."},
						"producer":   map[string]any{"type": "string", "enum": []string{"user", "primary", "escalation"}, "description": "Source: 'user' if stated by user."},
						"consumer":   map[string]any{"type": "string", "enum": []string{"primary", "escalation", ""}, "description": "Target agent; omit for both."},
						"relation":   map[string]any{"type": "string", "enum": []string{"extends", "updates", "derives", "contradicts"}, "description": "How this fact relates to related_to."},
						"related_to": map[string]any{"type": "string", "description": "ID (or prefix, or unique description) of an existing percept this one relates to."},
					},
					"required": []string{"content"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "get_memory",
				"description": "Retrieve remembered facts relevant to a query. Returns only percepts visible to the calling agent (its own consumer tag plus shared ones).",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query":          map[string]any{"type": "string", "description": "Keywords to recall."},
						"min_confidence": map[string]any{"type": "number", "description": "Min weight [0,1] (default 0.4)."},
						"max_results":    map[string]any{"type": "integer", "description": "Max results (default 5)."},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "list_memory",
				"description": "List stored memories visible to the calling agent (its own consumer tag plus shared ones), optionally filtered.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"scope":    map[string]any{"type": "string", "enum": []string{"global", "session", ""}, "description": "Scope filter."},
						"producer": map[string]any{"type": "string", "enum": []string{"user", "primary", "escalation", "system", ""}, "description": "Producer filter."},
						"consumer": map[string]any{"type": "string", "enum": []string{"primary", "escalation", ""}, "description": "Consumer-tag filter; empty lists everything visible to the calling agent."},
						"min_w":    map[string]any{"type": "number", "description": "Min weight [0,1]."},
						"pattern":  map[string]any{"type": "string", "description": "Substring filter."},
					},
					"required": []string{},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "forget_memory",
				"description": "Delete stored memories by ID, ID prefix, or description/pattern. id accepts a percept ID (leading '#' optional), an ID prefix, or a one-match description; ids deletes several such targets in one call; pattern deletes every percept whose content contains the substring. At least one of id/ids/pattern is required. Returns an error when a target doesn't resolve or is ambiguous — in that case nothing is deleted.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":      map[string]any{"type": "string", "description": "Percept ID (\"#\" optional), ID prefix, or a description matching exactly one percept."},
						"ids":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Several percept IDs/prefixes/descriptions in one call."},
						"pattern": map[string]any{"type": "string", "description": "Case-insensitive content substring; deletes every matching percept."},
					},
					"required": []string{},
				},
			},
		},
	}
}

// DispatchForgetMemory handles a forget_memory tool call.
//
// Accepts id (a single percept ID, ID prefix, or description), ids (several
// such values in one call), and/or pattern (a case-insensitive content
// substring matched against every percept). Lookup order mirrors the /forget
// slash command: "#"-prefixed and hex-looking values are ID/prefix lookups
// first (FindByIDPrefix), with a content-match fallback for plain values.
// id/ids entries must resolve to exactly one percept each; pattern deletes
// every match (that is the batch path).
//
// All targets are resolved before anything is deleted: if any target is
// unresolvable or ambiguous, the whole call returns an error result and the
// store is left untouched (issue #172 gap 5: "not found" is an error-shaped
// result, not an ok-shaped one).
func DispatchForgetMemory(_ context.Context, store *Store, argsJSON string) string {
	var args struct {
		ID      string   `json:"id"`
		IDs     []string `json:"ids"`
		Pattern string   `json:"pattern"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return errResult(invalidArgs + err.Error())
	}

	specs := make([]string, 0, 2+len(args.IDs))
	if s := strings.TrimSpace(args.ID); s != "" {
		specs = append(specs, s)
	}
	for _, raw := range args.IDs {
		if s := strings.TrimSpace(raw); s != "" {
			specs = append(specs, s)
		}
	}
	pattern := strings.TrimSpace(args.Pattern)
	if len(specs) == 0 && pattern == "" {
		return errResult("provide id, ids, or pattern — nothing to forget")
	}

	var targets []Percept
	seen := map[string]bool{}
	add := func(ps []Percept) {
		for _, p := range ps {
			if !seen[p.ID] {
				seen[p.ID] = true
				targets = append(targets, p)
			}
		}
	}

	var problems []string
	for _, spec := range specs {
		matches, err := resolveForgetTarget(store, spec)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		add(matches)
	}
	if pattern != "" {
		matches := store.List(ListOpts{Pattern: pattern})
		if len(matches) == 0 {
			problems = append(problems, fmt.Sprintf("no percepts match pattern %q", pattern))
		}
		add(matches)
	}
	if len(problems) > 0 {
		return errResult(strings.Join(problems, "\n"))
	}

	var b strings.Builder
	for _, p := range targets {
		ok, err := store.Delete(p.ID)
		if err != nil {
			return errResult("failed to delete: " + err.Error())
		}
		if !ok {
			return errResult(fmt.Sprintf("percept %s vanished before deletion", p.ID[:8]))
		}
		if len(targets) == 1 {
			return okResult(fmt.Sprintf("percept %s deleted", p.ID[:8]))
		}
		fmt.Fprintf(&b, "  %s  %s\n", p.ID[:8], p.Content)
	}
	return okResult(fmt.Sprintf("deleted %d percepts:\n%s", len(targets), b.String()))
}

// resolveForgetTarget resolves one id/ids entry to exactly one percept,
// mirroring /forget's lookup order:
//
//  1. "#"-prefixed input is an ID/prefix lookup only (FindByIDPrefix).
//  2. Hex-looking input (4-64 hex chars) is an ID/prefix lookup first, with a
//     content-match fallback.
//  3. Anything else is a description: a case-insensitive content substring
//     match, which must hit exactly one percept (several matches are
//     ambiguous — use a longer description, an ID, or pattern).
func resolveForgetTarget(store *Store, spec string) ([]Percept, error) {
	hashed := strings.HasPrefix(spec, "#")
	q := strings.TrimSpace(strings.TrimPrefix(spec, "#"))
	if q == "" {
		return nil, fmt.Errorf("percept %q not found — empty ID", spec)
	}
	if hashed || looksLikeIDPrefix(q) {
		candidates := store.FindByIDPrefix(q)
		switch {
		case len(candidates) == 1:
			return candidates, nil
		case len(candidates) > 1:
			var b strings.Builder
			fmt.Fprintf(&b, "ambiguous prefix %q matches %d percepts — use a longer prefix:\n", spec, len(candidates))
			for _, p := range candidates {
				fmt.Fprintf(&b, "  %s  %s\n", p.ID[:8], p.Content)
			}
			return nil, fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
		case hashed:
			// "#"-prefixed input is ID-only (parity with /forget).
			return nil, fmt.Errorf("percept %s not found", q)
		}
	}
	matches := store.List(ListOpts{Pattern: q})
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("percept %q not found (no ID prefix or content match)", spec)
	case 1:
		return matches, nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "description %q matches %d percepts — use a longer description, an ID, or pattern:\n", spec, len(matches))
		for _, p := range matches {
			fmt.Fprintf(&b, "  %s  %s\n", p.ID[:8], p.Content)
		}
		return nil, fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
	}
}

// looksLikeIDPrefix reports whether s looks like a percept ID prefix (4-64 hex
// chars) — the same heuristic as cmd/milk's isHexPrefix used by /forget.
func looksLikeIDPrefix(s string) bool {
	if len(s) < 4 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// DispatchRecordMemory handles a record_memory tool call. Optional
// relation + related_to declare a typed edge between the recorded (or
// reinforced) percept and an existing one (ADR-0053, relation semantics
// ADR-0091): extends/derives corroborate the target, updates corroborates the
// source (the newly recorded fact) and supersedes the target, contradicts
// weakens both — all weighed at consolidation.
func DispatchRecordMemory(ctx context.Context, store *Store, argsJSON string) string {
	var args struct {
		Content   string `json:"content"`
		Subject   string `json:"subject"`
		Producer  string `json:"producer"`
		Consumer  string `json:"consumer"`
		Relation  string `json:"relation"`
		RelatedTo string `json:"related_to"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return errResult(invalidArgs + err.Error())
	}
	if strings.TrimSpace(args.Content) == "" {
		return errResult("content is required")
	}
	rel := Relation(strings.TrimSpace(args.Relation))
	switch rel {
	case "", RelationExtends, RelationUpdates, RelationDerives, RelationContradicts:
	default:
		return errResult(invalidArgs + "relation must be one of extends, updates, derives, contradicts")
	}
	if rel != "" && strings.TrimSpace(args.RelatedTo) == "" {
		return errResult(invalidArgs + "related_to is required when relation is set")
	}

	roles := Roles{}
	if args.Subject != "" {
		roles.Theme = args.Subject
	}
	producer := ProducerLocal
	switch args.Producer {
	case "user":
		producer = ProducerUser
	case "escalation":
		producer = ProducerEscalation
	}
	var consumer Consumer
	switch args.Consumer {
	case "primary":
		consumer = ConsumerLocal
	case "escalation":
		consumer = ConsumerEscalation
	}
	out, err := store.Record(ctx, args.Content, producer, consumer, roles, false)
	if err != nil {
		return errResult("failed to record: " + err.Error())
	}

	var b strings.Builder
	if out.Reinforced {
		fmt.Fprintf(&b, "reinforced existing percept %s (w=%.2f, %.0f%% overlap): %s",
			out.ID[:8], out.W, out.Similarity*100, out.Existing)
	} else {
		fmt.Fprintf(&b, "recorded percept %s", out.ID[:8])
	}

	if rel != "" {
		targets, err := resolveForgetTarget(store, args.RelatedTo)
		if err != nil {
			return errResult("percept recorded, but related_to failed: " + err.Error())
		}
		target := targets[0]
		if err := store.AddEdge(out.ID, target.ID, rel); err != nil {
			return errResult("percept recorded, but edge failed: " + err.Error())
		}
		fmt.Fprintf(&b, "\nedge recorded: %s %s %s", out.ID[:8], rel, target.ID[:8])
	}
	return okResult(b.String())
}

// DispatchGetMemory handles a get_memory tool call.
// caller restricts which percepts are visible (ConsumerAll = no restriction):
// the caller's own consumer tag plus shared percepts — derived from the calling
// agent's role by the dispatch layer (issue #172 gap 2).
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

// listArgs are the shared list_memory / filtered-list arguments.
type listArgs struct {
	Scope    string  `json:"scope"`
	Producer string  `json:"producer"`
	Consumer string  `json:"consumer"`
	MinW     float64 `json:"min_w"`
	Pattern  string  `json:"pattern"`
}

// listOpts resolves list_memory arguments into ListOpts. caller is the calling
// agent's visibility (own consumer tag plus shared percepts); the consumer
// argument, when set, narrows/overrides it to the named tag (issue #172 gap 3).
func (a listArgs) opts(caller Consumer) ListOpts {
	consumer := caller
	if a.Consumer != "" {
		consumer = Consumer(a.Consumer)
	}
	return ListOpts{
		Scope:    a.Scope,
		Producer: a.Producer,
		Consumer: consumer,
		MinW:     a.MinW,
		Pattern:  a.Pattern,
	}
}

// DispatchListMemory handles a list_memory tool call. caller restricts which
// percepts are visible (ConsumerAll = no restriction) unless the consumer
// argument names a tag explicitly.
func DispatchListMemory(_ context.Context, store *Store, argsJSON string, caller Consumer) string {
	var args listArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return errResult(invalidArgs + err.Error())
	}
	percepts := store.List(args.opts(caller))
	if len(percepts) == 0 {
		return okResult("(no percepts found)")
	}
	return okResult(FormatList(percepts))
}

// DispatchListMemoryFiltered is like DispatchListMemory but applies keyword
// relevance gating against prompt before formatting — percepts with zero token
// overlap with prompt are dropped. Used by the local agent when
// RelevanceGateEnabled is set.
func DispatchListMemoryFiltered(_ context.Context, store *Store, argsJSON, prompt string, caller Consumer) string {
	var args listArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return errResult(invalidArgs + err.Error())
	}
	percepts := store.List(args.opts(caller))
	percepts = FilterByRelevance(percepts, prompt)
	if len(percepts) == 0 {
		return okResult("(no relevant memories found)")
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
