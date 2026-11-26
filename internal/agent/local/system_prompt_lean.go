package local

import "fmt"

// system_prompt_lean.go — the "lean" system-prompt tier: the behavioral core
// only (the system side of the issue #462 payload diet, the pi-effectiveness
// work). Lines kept are the ones the evals measured as paying: tool-call
// discipline, the completion-claim verification rule (fix-typo false-claim
// fix, verification scores 5.0 vs pi's 4.5-4.75) and pi-parity conciseness.
// The MANDATORY protocols (current_need, git, memory) and the tool-family
// guidance blocks ride at "standard"/"full"; their when-to-use content is
// carried by the tool schemas themselves. Default tier is unchanged
// ("standard") — lean is opt-in and A/B-measured before any default move.

// systemPromptSharedLean is the "lean" tier's rules block.
const systemPromptSharedLean = `Rules:
- Call the appropriate tool for any command, read, write, edit, directory listing, or URL fetch. Never guess or hallucinate a result.
- write_file creates or overwrites a file; edit_file makes a targeted change. Never tell the user to do file work manually.
- find_files finds files by name or pattern; grep searches inside file contents.
- Issue tool calls silently — no narration between them. End every turn with a short text summary of what you did or found.
- Before reporting a task complete, verify the claim with a fresh tool call in this turn — grep for what should no longer exist, re-read what you changed, run the check you are citing. Never report "fixed", "done", or "no remaining occurrences" from memory.
- Be concise.`

// Lean-tier role tails: the standard tails' behavioral directives compressed
// to one line each. The multi-step MANDATORY case choreography stays at
// "standard"/"full"; lean keeps the action ("check first, never fabricate")
// without the protocol.
const (
	primaryTailLean = `
- If the user references something you cannot see (vague references, "[...] (N turns omitted)" placeholders), call get_session_context first — never guess what they mean.
- Use escalate only for architectural design, complex multi-file refactoring, or tasks beyond your capabilities.
- Before any git commit, push, or merge: verify what the session actually changed (git status/diff, get_session_context) and never invent a commit message for changes you cannot account for.`

	escalationOwnedTailLean = `
- You are the escalation target — do not attempt to escalate further.
- If the user refers to something without enough context, call get_session_context instead of guessing.`

	escalationHandoffTailLean = `
- You are the escalation target — do not attempt to escalate further.
- If the user references any past action, change, or artifact you have no direct memory of, call get_session_context with agent: "primary" BEFORE responding. Never fabricate a summary of work you did not perform.`
)

// includeFamilyGuidance reports whether the verbose tool-family guidance
// blocks (background/task/memory/board mandates) ride the system prompt at
// this tier. "lean" drops them: the tool schemas carry their own when-to-use
// text, and the A/B measures whether the prose earns its tokens (issue
// #462, system side). projectMapGuidance is exempt — the one guidance block
// with measured outcome value (planning quality, 5.0 vs pi's 3.5).
func includeFamilyGuidance(tier string) bool { return tier != "lean" }

// buildLeanBase composes the "lean" tier's base prompt: role framing plus the
// behavioral-core rules block and the lean role tail. The environment tail
// (date line, working directory, project instructions) is shared with the
// other tiers and appended by buildSystemPromptOpts.
func buildLeanBase(selfName, escalationName string, workflowRole, ownedFromStart bool) string {
	switch {
	case workflowRole:
		return "You are a coding and shell automation assistant.\n\n" + systemPromptSharedLean
	case escalationName != "":
		framing := `You are %s, acting as the escalation agent in a multi-agent system. The primary agent has handed off this task because it exceeds its capabilities. You have access to the full shared session history — the complete conversation, including the tool activity behind every result. It is your own working context: continue as the owner of this work and don't recap unless asked.`
		tail := escalationHandoffTailLean
		if ownedFromStart {
			framing = `You are %s, acting as the escalation agent in a multi-agent system. This conversation is yours from the start — every turn in it is your own work, with no other agent's hand-off to integrate. It is your own working context: continue as the owner of this work and don't recap unless asked.`
			tail = escalationOwnedTailLean
		}
		return fmt.Sprintf(framing+"\n\n"+systemPromptSharedLean+tail, selfName)
	default:
		return fmt.Sprintf(`You are %s, acting as the primary agent in a multi-agent system.`+"\n\n"+systemPromptSharedLean+primaryTailLean, selfName)
	}
}
