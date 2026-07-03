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

// ExportText renders the session as a human-readable transcript (plain text, no ANSI).
func ExportText(s *Session) string {
	return exportText(s, false)
}

// ExportTextColorized renders the session as a human-readable transcript with ANSI color
// highlighting on turn headers. Use this when writing to a terminal; use ExportText for
// file output.
func ExportTextColorized(s *Session) string {
	return exportText(s, true)
}

// exportText is the shared implementation for ExportText and ExportTextColorized.
// When colorize is true, ANSI escape codes are applied to the rule and header line of
// each turn; body content is always emitted as-is.
func exportText(s *Session, colorize bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "session: %s\n", s.ID)
	if s.Name != "" {
		fmt.Fprintf(&b, "name:    %s\n", s.Name)
	}
	fmt.Fprintf(&b, "cwd:     %s\n", s.CWD)
	fmt.Fprintf(&b, "created: %s\n", s.CreatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "state:   %s\n", s.State)
	fmt.Fprintf(&b, "turns:   %d\n", len(s.History))

	const rule = "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

	// ansi wraps text in an ANSI escape sequence when colorize is true.
	ansi := func(s, code string) string {
		if !colorize {
			return s
		}
		return code + s + "\033[0m"
	}

	for i, t := range s.History {
		ts := t.Timestamp.Format("2006-01-02 15:04:05")
		if i > 0 {
			fmt.Fprintln(&b)
		}

		// Role-specific ANSI codes: headerCode is bold+color for the role label;
		// ruleCode is the same hue without bold for the horizontal rule.
		// Colors match the live chat colors exactly:
		//   user       → bold white  (neutral, not confused with any agent)
		//   primary    → bold green  (matches live primary agent label)
		//   escalation → bold blue   (matches live escalation agent label)
		//   tool result→ dim yellow
		//   thinking   → bold magenta
		var headerCode, ruleCode string
		switch t.Role {
		case RoleUser:
			headerCode, ruleCode = "\033[1;37m", "\033[37m"
		case RoleAssistant:
			switch t.Agent {
			case AgentLocal: // primary
				headerCode, ruleCode = "\033[1;32m", "\033[32m"
			case AgentEscalation:
				headerCode, ruleCode = "\033[1;34m", "\033[34m"
			default:
				headerCode, ruleCode = "\033[1;36m", "\033[36m"
			}
		case RoleToolResult:
			headerCode, ruleCode = "\033[2;33m", "\033[33m"
		default: // thinking or unknown roles
			headerCode, ruleCode = "\033[1;35m", "\033[35m"
		}

		fmt.Fprintln(&b, ansi(rule, ruleCode))

		dimTS := ansi(ts, "\033[2m")
		sep := ansi("  ·  ", "\033[2m")

		switch t.Role {
		case RoleUser:
			fmt.Fprintf(&b, "%s%s%s\n%s\n", dimTS, sep, ansi("user", headerCode), t.Content)
		case RoleAssistant:
			agent := string(t.Agent)
			fmt.Fprintf(&b, "%s%s%s\n", dimTS, sep, ansi(agent, headerCode))
			if len(t.ToolCalls) > 0 {
				for _, tc := range t.ToolCalls {
					fmt.Fprintf(&b, "  ⚙ %s: %s\n", tc.Name, tc.Arguments)
				}
			} else {
				fmt.Fprintf(&b, "%s\n", t.Content)
			}
		case RoleToolResult:
			content := t.Content
			if len(content) > 300 {
				content = content[:297] + "..."
			}
			fmt.Fprintf(&b, "%s%s%s\n  → %s\n", dimTS, sep, ansi("tool result", headerCode), content)
		}
	}
	return b.String()
}
