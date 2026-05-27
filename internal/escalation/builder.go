package escalation

import (
	"fmt"
	"strings"

	"github.com/scoutme/milk/internal/session"
)

// identityBlock is injected at the top of every system context passed to Claude
// so it understands it is operating as a milk-hosted agent, not a standalone session.
const identityBlock = "[Milk agent context]\n" +
	"You are Claude Code hosted by milk (multi-agent router). " +
	"You share session history and memory with the local LLM agent. " +
	"Expect mid-conversation hand-offs and multi-turn resumes.\n\n"

// BuildContext formats the local session history into a system prompt context
// block for Claude. Claude receives this via --append-system-prompt and
// orients itself without a separate reformulation step.
// nonce is passed through to MemoryInstruction so the tag format is session-specific.
// percepts contains content strings of remembered facts to inject for Claude; may be nil.
func BuildContext(sess *session.Session, nonce string, percepts []string) string {
	if len(sess.History) == 0 {
		return identityBlock + MemoryInstruction(nonce) + formatPercepts(percepts)
	}

	var b strings.Builder
	b.WriteString(identityBlock)
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
	b.WriteString("\n")
	b.WriteString(MemoryInstruction(nonce))
	b.WriteString(formatPercepts(percepts))
	return b.String()
}

// formatPercepts renders a [Remembered facts] block when percepts is non-empty.
func formatPercepts(percepts []string) string {
	if len(percepts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n[Remembered facts]\n")
	for _, p := range percepts {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	return b.String()
}

// MemoryInstruction returns a system-prompt fragment that instructs Claude to
// emit atomic facts worth persisting using session-specific nonce tags.
// nonce is a short alphanumeric string generated fresh each session so that
// only live responses — not explanations or code examples about the tag format —
// are captured by the stream layer.
// Milk's stream layer intercepts and strips these tags, recording each fact into
// the shared memory store with ProducerClaude — they never appear in the output.
func MemoryInstruction(nonce string) string {
	openTag := "<milk:percept:" + nonce + ">"
	closeTag := "</milk:percept:" + nonce + ">"
	return "[Milk shared memory]\n" +
		"Emit reusable, non-obvious, session-independent facts as:\n" +
		"  " + openTag + "one fact" + closeTag + "\n" +
		"One fact per tag. Skip transient state or current-task details. " +
		"Tags are intercepted by milk, never shown. " +
		"Prefix @local: or @claude: to target a specific agent.\n"
}
