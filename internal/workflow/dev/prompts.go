package dev

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// planInstructions is the shared spec/limits/sprint-plan format instructions,
// embedded by every designer prompt variant that must produce (or may need to
// produce, in the NO_QUESTIONS case) the actual plan the rest of the workflow parses.
const planInstructions = `## Spec
A precise description of what needs to be built, including acceptance criteria.

## Limits
Declare the execution limits for this workflow on two lines, exactly as shown:
  max_passes: <N>
  max_sprints: <N>

max_passes is the maximum number of generator→evaluator iterations allowed per sprint
before the workflow halts with an error. Set it based on task complexity:
  1–2 for trivial tasks (a single small file or function)
  3–5 for moderate tasks (a feature, a small module)
  6–10 for complex tasks (a full game, a multi-file system, many interacting components)
Err on the side of more passes — running out of passes halts the workflow with an error.

max_sprints is a safety cap on the total number of sprints. It must equal or exceed the
number of ## Sprint N sections you define below. Use it to prevent runaway loops.

## Sprint Plan
Use one section per sprint, each headed exactly as "## Sprint N" (e.g. "## Sprint 1").
For each sprint list the concrete deliverables: files to create or modify, tests to write,
testing instructions for a human reviewer.

Keep the number of sprints to the minimum needed. If the task fits in one sprint, use one.
`

func designerPrompt(task string) string {
	return fmt.Sprintf(`You are the designer for this development task.

Task: %s

Produce a detailed spec and sprint plan. Structure your response as follows:

%s`, task, planInstructions)
}

// designerQuestionsPrompt asks the designer to identify ambiguities in the task
// before producing the full plan. When there are no ambiguities worth asking
// about, the designer must skip straight to the full plan in the same response
// (there is no separate finalize call in that case — dev.go parses everything
// after the "NO_QUESTIONS" line as the plan).
func designerQuestionsPrompt(task string) string {
	return fmt.Sprintf(`You are the designer for this development task.

Task: %s

Before producing a full plan, identify any ambiguities or missing information that would
affect the design. Focus on:
- Technology choices (language, framework, libraries)
- Architecture decisions (monolith vs modules, API design, data model)
- Scope boundaries (what's in vs out)
- Non-functional requirements (performance, security, deployment)

If the task has ambiguities worth asking about, respond with ONLY the questions, structured as:

## Questions
- Q1: <question>
- Q2: <question>
...

## Defaults
If the user doesn't answer, use these defaults:
- Q1: <default answer>
- Q2: <default answer>
...

Keep the number of questions to the minimum needed. Prefer reasonable defaults over asking.

Otherwise — if the task is already clear enough to design without asking — skip the questions
entirely. Respond with "NO_QUESTIONS" on its own line, immediately followed by the full spec
and sprint plan in this format:

%s`, task, planInstructions)
}

// designerFinalizePrompt asks the designer to produce the full plan incorporating
// the user's answers to disambiguation questions.
func designerFinalizePrompt(task, questions, answers string) string {
	return fmt.Sprintf(`You are the designer for this development task.

Task: %s

You previously identified these ambiguities:
%s

The user provided these answers:
%s

Now produce the full detailed spec and sprint plan incorporating the user's choices.

%s`, task, questions, answers, planInstructions)
}

// noQuestionsLineRE matches a standalone "NO_QUESTIONS" marker line,
// case-insensitive, ignoring leading/trailing spaces/tabs on that line. It is
// anchored to a whole line rather than the very start of the response
// because designers sometimes prepend a short reasoning preamble before their
// concluding marker and plan, despite being asked to lead with it — treating
// that as "has questions" incorrectly halts the workflow for user input.
var noQuestionsLineRE = regexp.MustCompile(`(?mi)^[ \t]*NO_QUESTIONS[ \t]*$`)

// hasQuestions returns true if the designer response contains structured questions
// (i.e., no standalone "NO_QUESTIONS" marker line found anywhere in the response).
func hasQuestions(response string) bool {
	return !noQuestionsLineRE.MatchString(response)
}

// stripNoQuestionsPreamble returns the plan text following a standalone
// NO_QUESTIONS marker line, discarding anything before it (e.g. a reasoning
// preamble the designer wrote before concluding there were no questions).
// Falls back to the trimmed response unchanged if no marker line is found.
func stripNoQuestionsPreamble(response string) string {
	loc := noQuestionsLineRE.FindStringIndex(response)
	if loc == nil {
		return strings.TrimSpace(response)
	}
	return strings.TrimSpace(response[loc[1]:])
}

func generatorPrompt(planPath string, sprint, pass int, findingsPath string) string {
	plan := readFileOrEmpty(planPath)

	var sb strings.Builder
	fmt.Fprintf(&sb, "You are the generator. Execute Sprint %d", sprint)
	if pass > 1 {
		fmt.Fprintf(&sb, " (pass %d — refine based on evaluator findings)", pass)
	}
	fmt.Fprintf(&sb, ".\n\n")

	// Send only the current sprint section, not the full plan.
	if plan != "" {
		sprintSection := parseSprintSection(plan, sprint)
		if sprintSection != "" {
			fmt.Fprintf(&sb, "## Current sprint\n%s\n\n", sprintSection)
		} else {
			// Fallback: sprint not found in plan, send the full plan.
			fmt.Fprintf(&sb, "## Plan\n%s\n\n", plan)
		}
	}

	if pass > 1 && findingsPath != "" {
		findings := readFileOrEmpty(findingsPath)
		if findings != "" {
			fmt.Fprintf(&sb, "## Evaluator findings from previous pass\n%s\n\n", findings)
		}
	}

	fmt.Fprintf(&sb, `Implement all deliverables for Sprint %d as described in the plan.
Use your tools (read_file, write_file, edit_file, bash, etc.) to read, create, and modify files.
After completing all tool use, write a plain-text summary of what you did: which files were
created or modified, what each change does, and which acceptance criteria it satisfies.
This written summary is mandatory — the evaluator reads it to assess the sprint.
`, sprint)

	return sb.String()
}

func evaluatorPrompt(planPath string, sprintOutputPath string, sprint, pass, maxPasses int) string {
	plan := readFileOrEmpty(planPath)
	sprintOutput := readFileOrEmpty(sprintOutputPath)

	var sb strings.Builder
	fmt.Fprintf(&sb, "You are the evaluator. Review Sprint %d (pass %d of %d).\n\n", sprint, pass, maxPasses)

	// Send only the current sprint section, not the full plan.
	if plan != "" {
		sprintSection := parseSprintSection(plan, sprint)
		if sprintSection != "" {
			fmt.Fprintf(&sb, "## Current sprint\n%s\n\n", sprintSection)
		} else {
			fmt.Fprintf(&sb, "## Plan\n%s\n\n", plan)
		}
	}
	if sprintOutput != "" {
		fmt.Fprintf(&sb, "## Generator output\n%s\n\n", sprintOutput)
	} else {
		fmt.Fprintf(&sb, "## Generator output\n(empty — the generator produced no written summary)\n\n")
		fmt.Fprintf(&sb, "NOTE: The generator may have used tools to modify files directly without "+
			"producing a written summary. Use your read_file and bash tools to inspect the codebase "+
			"and determine whether the sprint deliverables were actually implemented.\n\n")
	}

	fmt.Fprintf(&sb, `Review the sprint deliverables against the Sprint %d acceptance criteria.
Use your tools to read relevant source files and verify the implementation when needed.

Write a findings section describing what is correct and what needs improvement.

End your response with exactly ONE of these verdict lines:
- good_to_go
- needs_refinement
- sprint_done

Use "good_to_go" when all acceptance criteria are met.
Use "needs_refinement" when there are fixable issues AND passes remain (you are on pass %d of %d).
Use "sprint_done" when the sprint deliverables are substantially complete, OR when this is the last pass and remaining issues are minor or impractical to resolve in one more iteration.

IMPORTANT: if this is the final pass (pass %d = max %d), do NOT return "needs_refinement" — use "sprint_done" instead, since no further refinement will occur.
`, sprint, pass, maxPasses, pass, maxPasses)

	return sb.String()
}

func readFileOrEmpty(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
