package escalation

import (
	"strings"

	"github.com/scoutme/milk/internal/session"
)

// identityBlock is injected at the top of every system context passed to the CLI escalation agent
// so it understands it is operating as a milk-hosted agent, not a standalone session.
const identityBlock = "[Milk agent context]\n" +
	"You are Claude Code hosted by milk (multi-agent router). " +
	"You share session history and memory with the local LLM agent. " +
	"Expect mid-conversation hand-offs and multi-turn resumes.\n\n"

// BuildContext assembles the system-prompt context block for a CLI escalation.
//
// primaryName and escalationName are the configured names of the two agents;
// they are embedded in the MemoryInstruction so the escalation agent knows
// which @<name>: prefix to use for consumer-targeted percepts.
//
// injectInstructions controls whether the need/memory instruction blocks are
// included. Pass true on the first escalation and on re-injection turns; pass
// false on subsequent resume turns where the instructions are already in context.
//
// On the first escalation (not a resume), it injects:
//   - identity block
//   - escalation brief (if agent-triggered via escalate)
//   - current need (what the user is working towards)
//   - last local summary (what the local model did since the last Claude session)
//   - memory instruction (percept tag format)
//   - remembered facts
//
// On a resume, only identity + current need + last local summary (+ optionally
// memory instruction) are included — the escalation agent already has its own
// conversation history.
func BuildContext(sess *session.Session, nonce string, percepts []string, resuming bool, injectInstructions bool, primaryName, escalationName string) string {
	var b strings.Builder
	b.WriteString(identityBlock)

	if !resuming && sess.EscalationBrief != "" {
		b.WriteString("[Escalation brief from local agent]\n")
		b.WriteString(sess.EscalationBrief)
		b.WriteString("\n\n")
	}

	if sess.CurrentNeed != "" {
		// CurrentNeedSetAt is 1-based: 0 = never set (treat as fresh), ≥1 = len(History)+1 at write time.
		var turnsAgo int
		if sess.CurrentNeedSetAt > 0 {
			turnsAgo = len(sess.History) - (sess.CurrentNeedSetAt - 1)
		}
		if turnsAgo >= 4 {
			b.WriteString("[Last known user goal — may already be fulfilled; verify from conversation history before acting]\n")
		} else {
			b.WriteString("[Current user goal]\n")
		}
		b.WriteString(sess.CurrentNeed)
		b.WriteString("\n\n")
	}

	if sess.LastLocalSummary != "" {
		b.WriteString("[Recent local agent activity]\n")
		b.WriteString(sess.LastLocalSummary)
		b.WriteString("\n")
	}

	if injectInstructions {
		b.WriteString(NeedInstruction(nonce))
		b.WriteString(MemoryInstruction(nonce, primaryName, escalationName))
		b.WriteString(formatPercepts(percepts))
	}
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

// MemoryInstruction returns a system-prompt fragment that instructs an agent to
// emit atomic facts worth persisting using session-specific nonce tags.
// primaryName and escalationName are the configured agent names used in the
// @<name>: consumer-hint prefix so the model knows the actual agent identifiers.
// nonce is a short alphanumeric string generated fresh each session so that
// only live responses — not explanations or code examples about the tag format —
// are captured by the stream layer.
// Milk's stream layer intercepts and strips these tags — they never appear in the output.
func MemoryInstruction(nonce, primaryName, escalationName string) string {
	openTag := "<milk:percept:" + nonce + ">"
	closeTag := "</milk:percept:" + nonce + ">"
	return "[Milk shared memory]\n" +
		"Emit reusable, non-obvious, session-independent facts as:\n" +
		"  " + openTag + "one fact" + closeTag + "\n" +
		"One fact per tag. Skip transient state or current-task details. " +
		"Tags are intercepted by milk, never shown. " +
		"Prefix @" + primaryName + ": or @" + escalationName + ": to target a specific agent.\n"
}
