package local

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// summaryToolOutputMaxChars bounds one tool result fed to the summarizer
	// (OpenCode uses the same 2000): the summary needs what happened, not the
	// file contents.
	summaryToolOutputMaxChars = 2000
	// summaryInputMaxChars bounds the whole summarizer input so compacting a
	// huge span can't itself overflow the window.
	summaryInputMaxChars = 300000
)

// RenderForSummary renders messages as the text handed to Summarize. Tool
// calls appear as name(key argument) lines — the summarizer sees what was
// read, run and edited — and each tool result is cut to a short head.
func RenderForSummary(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			if m.Content != "" {
				fmt.Fprintf(&b, "[assistant]: %s\n", m.Content)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "[assistant called]: %s(%s)\n", tc.Function.Name, callArgSummary(tc.Function.Arguments))
			}
		case m.Content == "":
		case m.Role == "tool":
			c := m.Content
			if len(c) > summaryToolOutputMaxChars {
				c = runeSafeHead(c, summaryToolOutputMaxChars) + "…"
			}
			fmt.Fprintf(&b, "[tool result]: %s\n", c)
		default:
			fmt.Fprintf(&b, "[%s]: %s\n", m.Role, m.Content)
		}
	}
	return truncateHeadAndTail(b.String(), summaryInputMaxChars)
}

// callArgSummary is the one argument that identifies a tool call (path,
// command, pattern, …), shortened.
func callArgSummary(args string) string {
	var a map[string]any
	if json.Unmarshal([]byte(args), &a) != nil {
		return runeSafeHead(args, 120)
	}
	for _, k := range []string{"path", "file_path", "command", "pattern", "query", "url", "reason"} {
		if v, ok := a[k].(string); ok && v != "" {
			return runeSafeHead(v, 160)
		}
	}
	return ""
}

// FilesTouched lists the files the given messages' tool calls read, edited or
// wrote (most recent last, at most limit) for the compaction summary, so the
// model knows which file contents it no longer has. Empty when none.
func FilesTouched(msgs []Message, limit int) string {
	type entry struct {
		path, action string
	}
	var order []entry
	rank := map[string]int{"read": 1, "written": 2, "edited": 3}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			action := ""
			switch tc.Function.Name {
			case "read_file":
				action = "read"
			case "write_file":
				action = "written"
			case "edit_file", "apply_patch":
				action = "edited"
			default:
				continue
			}
			var a struct {
				Path string `json:"path"`
			}
			if json.Unmarshal([]byte(tc.Function.Arguments), &a) != nil || a.Path == "" {
				continue
			}
			prior := ""
			for i, e := range order {
				if e.path == a.Path {
					prior = e.action
					order = append(order[:i], order[i+1:]...)
					break
				}
			}
			if rank[prior] > rank[action] {
				action = prior
			}
			order = append(order, entry{a.Path, action})
		}
	}
	if len(order) == 0 {
		return ""
	}
	if limit > 0 && len(order) > limit {
		order = order[len(order)-limit:]
	}
	var b strings.Builder
	b.WriteString("<files-touched>\n")
	for _, e := range order {
		fmt.Fprintf(&b, "- %s (%s)\n", e.path, e.action)
	}
	b.WriteString("Their contents are no longer in context — re-read a file before editing it.\n</files-touched>")
	return b.String()
}
