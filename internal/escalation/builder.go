package escalation

import (
	"strings"

	"github.com/scoutme/milk/internal/session"
)

// identityBlock is injected at the top of every system context passed to Claude
// so it understands it is operating as a milk-hosted agent, not a standalone session.
const identityBlock = "[Milk agent context]\n" +
	"You are Claude Code hosted by milk (multi-agent router). " +
	"You share session history and memory with the local LLM agent. " +
	"Expect mid-conversation hand-offs and multi-turn resumes.\n\n"

// BuildContext assembles the system-prompt context block for a Claude escalation.
//
// On the first escalation (not a resume), it injects:
//   - identity block
//   - escalation brief (if agent-triggered via escalate_to_claude)
//   - current need (what the user is working towards)
//   - last local summary (what the local model did since the last Claude session)
//   - memory instruction (percept tag format)
//   - remembered facts
//
// On a resume, only identity + current need + last local summary + memory instruction
// are included — Claude already has its own conversation history.
func BuildContext(sess *session.Session, nonce string, percepts []string, resuming bool) string {
	var b strings.Builder
	b.WriteString(identityBlock)

	if !resuming && sess.EscalationBrief != "" {
		b.WriteString("[Escalation brief from local agent]\n")
		b.WriteString(sess.EscalationBrief)
		b.WriteString("\n\n")
	}

	if sess.CurrentNeed != "" {
		b.WriteString("[Current user goal]\n")
		b.WriteString(sess.CurrentNeed)
		b.WriteString("\n\n")
	}

	if sess.LastLocalSummary != "" {
		b.WriteString("[Recent local agent activity]\n")
		b.WriteString(sess.LastLocalSummary)
		b.WriteString("\n")
	}

	b.WriteString(NeedInstruction(nonce))
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
		b.WriteString("- ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	return b.String()
}

// NeedInstruction returns the system-prompt fragment that instructs both agents
// to emit a <milk:need:NONCE> tag when the user switches context, so milk can
// keep CurrentNeed up to date across the session.
func NeedInstruction(nonce string) string {
	openTag := "<milk:need:" + nonce + ">"
	closeTag := "</milk:need:" + nonce + ">"
	return "[Milk current-need tracking]\n" +
		"When the user switches to a new topic or goal, emit:\n" +
		"  " + openTag + "one-sentence description of what the user is now trying to accomplish" + closeTag + "\n" +
		"Also emit this tag for the very first user message if no goal has been set yet. " +
		"Tags are intercepted by milk, never shown to the user.\n\n"
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
