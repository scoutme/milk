package obs

import (
	"encoding/json"
	"strings"
)

// ToolSchemas returns the OpenAI function schemas for obs tools.
func ToolSchemas() []map[string]any {
	return []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "get_metrics",
				"description": "Return current milk observability metrics (memory stats, otel file sizes).",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
					"required":   []string{},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "search_signals",
				"description": "Search OTel signal files (logs, traces, metrics) for a pattern.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pattern":     map[string]any{"type": "string", "description": "Case-insensitive substring."},
						"signals":     map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"logs", "traces", "metrics"}}, "description": "Files to search; omit for all."},
						"max_results": map[string]any{"type": "integer", "description": "Max matches (default 20)."},
					},
					"required": []string{"pattern"},
				},
			},
		},
	}
}

// DispatchGetMetrics handles a get_metrics tool call.
func DispatchGetMetrics(otelDir string) string {
	result := FormatMetrics(otelDir)
	b, _ := json.Marshal(map[string]any{"output": result})
	return string(b)
}

// DispatchSearchSignals handles a search_signals tool call.
func DispatchSearchSignals(otelDir, argsJSON string) string {
	var args struct {
		Pattern    string   `json:"pattern"`
		Signals    []string `json:"signals"`
		MaxResults int      `json:"max_results"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		b, _ := json.Marshal(map[string]any{"error": "invalid arguments: " + err.Error()})
		return string(b)
	}
	if strings.TrimSpace(args.Pattern) == "" {
		b, _ := json.Marshal(map[string]any{"error": "pattern is required"})
		return string(b)
	}
	results := SearchSignals(otelDir, args.Pattern, args.Signals, args.MaxResults)
	output := FormatSearchResults(results, args.Pattern)
	b, _ := json.Marshal(map[string]any{"output": output})
	return string(b)
}
