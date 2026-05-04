package escalation

import (
	"fmt"
	"strings"

	"github.com/scoutme/milk/internal/session"
)

// BuildContext formats the local session history into a system prompt context
// block for Claude. Claude receives this via --append-system-prompt and
// orients itself without a separate reformulation step.
func BuildContext(sess *session.Session) string {
	if len(sess.History) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("[Context from local agent session]\n")

	for _, turn := range sess.History {
		switch turn.Role {
		case session.RoleUser:
			fmt.Fprintf(&b, "User: %s\n", turn.Content)

		case session.RoleAssistant:
			if len(turn.ToolCalls) > 0 {
				for _, tc := range turn.ToolCalls {
					fmt.Fprintf(&b, "[Tool call: %s] %s\n", tc.Name, tc.Arguments)
				}
			} else {
				fmt.Fprintf(&b, "Assistant (%s): %s\n", turn.Agent, turn.Content)
			}

		case session.RoleToolResult:
			content := turn.Content
			if len(content) > 500 {
				content = content[:500] + "\n... (truncated)"
			}
			fmt.Fprintf(&b, "[Tool result] %s\n", content)
		}
	}

	b.WriteString("[End of local context — continue from here]\n")
	return b.String()
}
