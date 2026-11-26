package local

import "fmt"

// system_prompt_lean.go — the "lean" system-prompt tier (the system side of
// the issue #462 payload diet). Design v2, after the first A/B: keep the
// restraint mechanisms, cut the toolbox protocol prose.
//
// The v1 lean tier compressed the role directives to one-liners and dropped
// every MANDATORY block. The A/B (13 scenarios x 2 arms, mimo-v2.6-flash,
// pro judge) measured quality parity but behavioral loosening: +42% tool
// calls, +57% output tokens, ~3.5x clean-run cost, incl. one runaway design
// turn (15.6x). The role directives and discipline framing were doing
// restraint work — the prefix bytes were paying for something.
//
// v2 keeps the standard behavioral core verbatim (systemPromptRulesCore and
// the act-first rule) and the standard role tails verbatim (primaryRoleTail /
// escalation*RoleTail — shared consts, so no drift), and drops only the
// toolbox protocol prose (systemPromptToolboxMeta + systemPromptToolboxProtocols:
// current_need, git 5-step protocol, history-label rule, get_metrics, gh
// plumbing, table rules, config-write safety) plus the tool-family guidance
// blocks (includeFamilyGuidance). One line survives the git protocol: the
// commit-honesty rule. Default tier is unchanged ("standard") — lean is
// opt-in and A/B-measured before any default move.

// systemPromptSharedLean is the "lean" tier's rules block: the standard
// behavioral core verbatim (restraint rules), the act-first rule, the
// distilled commit-honesty rule, and pi-style conciseness — minus the
// toolbox protocol prose.
const systemPromptSharedLean = systemPromptRulesCore + systemPromptActFirst + `
- Before any git commit, push, or merge: verify what the session actually changed (git status/diff, get_session_context) and never invent a commit message for changes you cannot account for.
- Be concise.`

// includeFamilyGuidance reports whether the verbose tool-family guidance
// blocks (background/task/memory/board mandates) ride the system prompt at
// this tier. "lean" drops them: the tool schemas carry their own when-to-use
// text (issue #462, system side). projectMapGuidance is exempt — the one
// guidance block with measured outcome value (planning quality, 5.0 vs
// pi's 3.5).
func includeFamilyGuidance(tier string) bool { return tier != "lean" }

// buildLeanBase composes the "lean" tier's base prompt: the same role framings
// and role tails as the standard tier (shared consts — byte-identical role
// directives) around the lean rules block. The environment tail (date line,
// working directory, project instructions) is shared with the other tiers and
// appended by buildSystemPromptOpts.
func buildLeanBase(selfName, escalationName string, workflowRole, ownedFromStart bool) string {
	sharedBlock := "\n\n" + systemPromptSharedLean
	switch {
	case workflowRole:
		return "You are a coding and shell automation assistant." + sharedBlock
	case escalationName != "":
		if ownedFromStart {
			return fmt.Sprintf(escalationOwnedFraming, selfName) + sharedBlock + escalationOwnedRoleTail
		}
		return fmt.Sprintf(escalationHandoffFraming, selfName) + sharedBlock + escalationHandoffRoleTail
	default:
		return fmt.Sprintf(primaryFraming, selfName) + sharedBlock + primaryRoleTail
	}
}
