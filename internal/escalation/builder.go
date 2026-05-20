package escalation

import (
	"fmt"
	"strings"

	"github.com/scoutme/milk/internal/session"
)

// BuildContext formats the local session history into a system prompt context
// block for Claude. Claude receives this via --append-system-prompt and
// orients itself without a separate reformulation step.
// nonce is passed through to MemoryInstruction so the tag format is session-specific.
// percepts contains content strings of remembered facts to inject for Claude; may be nil.
func BuildContext(sess *session.Session, nonce string, percepts []string) string {
	if len(sess.History) == 0 {
		return MemoryInstruction(nonce) + formatPercepts(percepts)
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
		"When you identify an atomic fact that is worth persisting across sessions — a\n" +
		"decision made, a constraint learned, a preference stated, a key finding — emit\n" +
		"it as a self-closing tag anywhere in your response:\n\n" +
		"  " + openTag + "concise, standalone fact in one sentence" + closeTag + "\n\n" +
		"Rules:\n" +
		"- One fact per tag; split compound facts into separate tags.\n" +
		"- Only emit facts that are non-obvious, session-independent, and reusable.\n" +
		"- Do NOT emit facts about transient state, current file content, or the current task.\n" +
		"- These tags are intercepted by milk and never shown to the user.\n" +
		"- To target a specific agent, prefix the fact: @local: <fact> (local model only) or @claude: <fact> (Claude only).\n" +
		"  Example: " + openTag + "@local: escalate to Claude for architecture questions" + closeTag + "\n"
}
