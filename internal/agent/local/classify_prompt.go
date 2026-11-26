package local

// classifyPromptShared is the common classifier instruction used by the
// chat-completions and Responses paths. The single-file line (2026-11-25,
// flip-rate study: the classifier flipped 1 of 5 on iterative-refactor's
// single-file extract-method prompt at temperature 0) pins the intent the
// escalation criteria already imply — a mechanical transformation confined
// to one file is coding assistance, not "complex multi-file refactoring" —
// so prompt noise tips it to escalate less often.
const classifyPromptShared = `Respond with exactly one word: "primary" or "escalate".
Use "escalate" only when the task clearly requires complex multi-file refactoring, architectural design decisions, or deep reasoning beyond coding assistance.
Use "primary" for shell commands, file reading, grep, simple code questions, debugging, and writing small functions.
Single-file mechanical transformations (extract methods, add fields or constructors, split a function into methods, rename) are "primary" even when the prompt calls them a refactor.

Task: `

// classifyPrompt is the chat-completions/Responses classifier prompt for one
// task text.
func classifyPrompt(task string) string { return classifyPromptShared + task }

// classifyPromptBedrock is the Bedrock Converse variant (kept worded in its
// own imperative style; same criteria and the same single-file rule).
const classifyPromptBedrockShared = `You are a routing classifier. Respond with exactly one word: "primary" or "escalate".
Respond "escalate" only if the task clearly requires: complex multi-file refactoring, architectural design decisions, or tasks that require deep reasoning beyond coding assistance.
Respond "primary" for: shell commands, file reading, grep, simple code questions, debugging, writing small functions.
Single-file mechanical transformations (extract methods, add fields or constructors, split a function into methods, rename) respond "primary" even when the task calls them a refactor.

Task: `

// classifyPromptBedrock is the Bedrock classifier prompt for one task text.
func classifyPromptBedrock(task string) string { return classifyPromptBedrockShared + task }
