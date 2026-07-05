package escalation

import (
	"strings"

	"github.com/scoutme/milk/internal/session"
)

// identityBlock is injected at the top of every system context passed to the CLI escalation agent
// so it understands it is operating as a milk-hosted agent, not a standalone session.
const identityBlock = "[Milk agent context]\n" +
	"You are the escalation agent hosted by milk (multi-agent router). " +
	"You share session history and memory with the primary agent. " +
	"Expect mid-conversation hand-offs and multi-turn resumes.\n\n"

// ContextMode describes the relationship between this escalation turn and any prior escalation session.
type ContextMode int

const (
	// ContextModeFirst is the first escalation — no prior escalation session in this milk session.
	ContextModeFirst ContextMode = iota
	// ContextModeResume is a direct continuation of an active escalation session
	// (state == ESCALATION_WAITING). The escalation agent already has full conversation history via --resume.
	ContextModeResume
	// ContextModeReturning is a return to the escalation agent after the primary agent did some work.
	// The escalation agent has its own prior session but may not know what the primary agent did in the interim.
	ContextModeReturning
)

// BuildStaticContext assembles the stable part of the system-prompt context for a
// CLI escalation turn. It includes everything that does not change turn-to-turn:
// the identity block, need/memory instructions (with the session-stable nonce), and
// remembered percepts. Because the content is stable, the CLI can cache it as a
// long-lived prefix and only pay tokenisation once per session.
//
// Returns "" on ContextModeResume — instructions are already in Claude's cached context.
// On ContextModeFirst and ContextModeReturning the full static block is returned;
// injectInstructions gates whether the tag-instruction and percept blocks are included
// (false on re-injection turns that have not crossed the threshold).
func BuildStaticContext(nonce string, percepts []string, mode ContextMode, injectInstructions bool, primaryName, escalationName string) string {
	// On ContextModeResume the static block is suppressed unless the re-injection
	// threshold has been crossed (injectInstructions=true overrides the suppression).
	if !injectInstructions {
		return ""
	}
	var b strings.Builder
	b.WriteString(NeedInstruction(nonce))
	b.WriteString(MemoryInstruction(nonce, primaryName, escalationName))
	b.WriteString(formatPercepts(percepts))
	return b.String()
}

// BuildPrimaryStaticContext is like BuildStaticContext but prepends the escalation
// instruction for subprocess primary agents (aider, smolagents). It is not included
// for escalation agents, which should never self-escalate.
func BuildPrimaryStaticContext(nonce string, percepts []string, mode ContextMode, injectInstructions bool, primaryName, escalationName string) string {
	base := BuildStaticContext(nonce, percepts, mode, injectInstructions, primaryName, escalationName)
	if base == "" {
		return ""
	}
	return EscalateInstruction(nonce) + base
}

// BuildDynamicContext assembles the turn-specific part of the system-prompt context
// for a CLI escalation. It includes everything that may change each turn: the identity
// block, escalation brief, current need, and the rolling primary-agent summary.
// Because this content changes frequently it is sent as a separate file from the
// static instructions, so changes here do not invalidate Claude's cached prefix.
//
// ContextModeFirst:     identity · brief · need · primary summary
// ContextModeResume:    primary summary only (if changed since last injection)
// ContextModeReturning: identity · brief · need · primary summary
//
// (No escalation summary — --resume already gives Claude its full prior history.)
func BuildDynamicContext(sess *session.Session, mode ContextMode) string {
	var b strings.Builder

	if mode == ContextModeResume {
		if sess.LastLocalSummary != "" && sess.LastLocalSummary != sess.LastLocalSummaryInjected {
			b.WriteString("[Recent primary agent activity]\n")
			b.WriteString(sess.LastLocalSummary)
			b.WriteString("\n")
			sess.LastLocalSummaryInjected = sess.LastLocalSummary
		}
		return b.String()
	}

	// First and Returning: Claude needs full orientation.
	b.WriteString(identityBlock)

	if sess.EscalationBrief != "" {
		b.WriteString("[Escalation brief from primary agent]\n")
		b.WriteString(sess.EscalationBrief)
		b.WriteString("\n\n")
	}

	if sess.CurrentNeed != "" {
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

	if sess.LastLocalSummary != "" && sess.LastLocalSummary != sess.LastLocalSummaryInjected {
		b.WriteString("[Recent primary agent activity]\n")
		b.WriteString(sess.LastLocalSummary)
		b.WriteString("\n")
		sess.LastLocalSummaryInjected = sess.LastLocalSummary
	}

	return b.String()
}

// BuildPrimaryDynamicContext assembles the turn-specific context for a subprocess
// primary agent (subprocess, aider-cli). Unlike BuildDynamicContext it does NOT
// inject LastLocalSummary (the agent's own prior turns — redundant and large); instead
// it injects LastEscalationSummary so the primary knows what the escalation agent did.
//
// ContextModeFirst:     identity · current need
// ContextModeReturning: identity · current need · escalation summary (if changed)
// ContextModeResume:    "" (primary subprocess has no waiting state; never called)
func BuildPrimaryDynamicContext(sess *session.Session, mode ContextMode) string {
	if mode == ContextModeResume {
		return ""
	}
	var b strings.Builder
	b.WriteString(identityBlock)

	if sess.CurrentNeed != "" {
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

	if mode == ContextModeReturning && sess.LastEscalationSummary != "" {
		b.WriteString("[Recent escalation agent activity]\n")
		b.WriteString(sess.LastEscalationSummary)
		b.WriteString("\n")
	}

	return b.String()
}

// BuildContext is the legacy single-string builder kept for callers that have not
// yet been migrated to the split BuildStaticContext/BuildDynamicContext API.
// Deprecated: prefer BuildStaticContext + BuildDynamicContext to enable per-part
// caching via two --append-system-prompt-file flags.
func BuildContext(sess *session.Session, nonce string, percepts []string, mode ContextMode, injectInstructions bool, primaryName, escalationName string) string {
	dynamic := BuildDynamicContext(sess, mode)
	static := BuildStaticContext(nonce, percepts, mode, injectInstructions, primaryName, escalationName)
	if mode == ContextModeResume {
		// Resume: dynamic summary only (static already in Claude's cached context).
		return dynamic
	}
	// First/Returning: dynamic orientation first, then static instructions.
	return dynamic + static
}

// FormatPercepts renders a [Remembered facts] block when percepts is non-empty.
// Exported so non-CLI escalation paths can inject percepts as a standalone message.
func FormatPercepts(percepts []string) string {
	if len(percepts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[Remembered facts]\n")
	for _, p := range percepts {
		b.WriteString("- ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	return b.String()
}

// formatPercepts is the internal alias used by BuildStaticContext.
func formatPercepts(percepts []string) string { return FormatPercepts(percepts) }

// EscalateInstruction returns the system-prompt fragment that tells a subprocess
// primary agent how to request escalation to the escalation agent. It is only
// included in the system prompt sent to subprocess primaries (aider, smolagents),
// not to the escalation agent itself.
func EscalateInstruction(nonce string) string {
	openTag := "<milk:escalate:" + nonce + ">"
	closeTag := "</milk:escalate:" + nonce + ">"
	return "[Milk escalation]\n" +
		"You are a primary agent. If the user's request requires capabilities you do not have " +
		"(e.g. web search, GitHub API, external tools, complex multi-step reasoning beyond your scope), " +
		"emit the following tag and then stop — do not attempt the task yourself:\n" +
		"  " + openTag + "one sentence explaining why escalation is needed" + closeTag + "\n" +
		"The tag is intercepted by milk and triggers hand-off to the escalation agent. " +
		"It is never shown to the user. Only emit it when you genuinely cannot fulfil the request.\n\n"
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
		"Omit the prefix to share with both agents (default). " +
		"Prefix @" + primaryName + ": or @" + escalationName + ": only when the fact is relevant to just that agent.\n" +
		"Emit when you learn: who the user is or their role; a key preference or working style; " +
		"a project decision or constraint not derivable from the code; an external resource or tool they use.\n" +
		"IMPORTANT — memory precedence: when the user asks you to remember something during a milk session, " +
		"always emit a " + openTag + "..." + closeTag + " tag so milk can store it in its percept system. " +
		"Do NOT write to any file-based or external memory system (e.g. Claude Code project memory files) " +
		"for project- or session-scoped facts — those writes bypass milk's memory pipeline and are never " +
		"visible in the milk memory panel or injected into future sessions. " +
		"Reserve file-based memory only for facts that must persist across all sessions regardless of host.\n"
}
