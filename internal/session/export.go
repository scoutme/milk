package session

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ExportJSON serialises the session as indented JSON.
func ExportJSON(s *Session) ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

// ExportText renders the session as a human-readable transcript.
func ExportText(s *Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "session: %s\n", s.ID)
	if s.Name != "" {
		fmt.Fprintf(&b, "name:    %s\n", s.Name)
	}
	fmt.Fprintf(&b, "cwd:     %s\n", s.CWD)
	fmt.Fprintf(&b, "created: %s\n", s.CreatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "state:   %s\n", s.State)
	fmt.Fprintf(&b, "turns:   %d\n", len(s.History))
	fmt.Fprintln(&b, strings.Repeat("-", 60))

	for _, t := range s.History {
		ts := t.Timestamp.Format("15:04:05")
		switch t.Role {
		case RoleUser:
			fmt.Fprintf(&b, "[%s] user: %s\n", ts, t.Content)
		case RoleAssistant:
			agent := string(t.Agent)
			if len(t.ToolCalls) > 0 {
				for _, tc := range t.ToolCalls {
					fmt.Fprintf(&b, "[%s] %s → tool call: %s(%s)\n", ts, agent, tc.Name, tc.Arguments)
				}
			} else {
				fmt.Fprintf(&b, "[%s] %s: %s\n", ts, agent, t.Content)
			}
		case RoleToolResult:
			content := t.Content
			if len(content) > 300 {
				content = content[:297] + "..."
			}
			fmt.Fprintf(&b, "[%s] tool result: %s\n", ts, content)
		}
	}
	return b.String()
}
