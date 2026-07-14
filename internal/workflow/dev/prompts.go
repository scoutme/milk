package dev

import (
	"fmt"
	"os"
	"strings"
)

func designerPrompt(task string) string {
	return fmt.Sprintf(`You are the designer for this development task.

Task: %s

Produce a detailed spec and sprint plan. Structure your response as follows:

## Spec
A precise description of what needs to be built, including acceptance criteria.

## Sprint Plan
Use one section per sprint, each headed exactly as "## Sprint N" (e.g. "## Sprint 1").
For each sprint list the concrete deliverables: files to create or modify, tests to write,
testing instructions for a human reviewer.

Keep the number of sprints minimal (1–3). If the task fits in one sprint, use one sprint.
`, task)
}

func generatorPrompt(planPath string, sprint, pass int, findingsPath string) string {
	plan := readFileOrEmpty(planPath)

	var sb strings.Builder
	fmt.Fprintf(&sb, "You are the generator. Execute Sprint %d", sprint)
	if pass > 1 {
		fmt.Fprintf(&sb, " (pass %d — refine based on evaluator findings)", pass)
	}
	fmt.Fprintf(&sb, ".\n\n")

	if plan != "" {
		fmt.Fprintf(&sb, "## Plan\n%s\n\n", plan)
	}

	if pass > 1 && findingsPath != "" {
		findings := readFileOrEmpty(findingsPath)
		if findings != "" {
			fmt.Fprintf(&sb, "## Evaluator findings from previous pass\n%s\n\n", findings)
		}
	}

	fmt.Fprintf(&sb, `Implement all deliverables for Sprint %d as described in the plan.
Write the code, tests, and testing instructions.
Be thorough — the evaluator will review your output against the sprint acceptance criteria.
`, sprint)

	return sb.String()
}

func evaluatorPrompt(planPath string, sprintOutputPath string, sprint, pass int) string {
	plan := readFileOrEmpty(planPath)
	sprintOutput := readFileOrEmpty(sprintOutputPath)

	var sb strings.Builder
	fmt.Fprintf(&sb, "You are the evaluator. Review Sprint %d (pass %d).\n\n", sprint, pass)

	if plan != "" {
		fmt.Fprintf(&sb, "## Plan\n%s\n\n", plan)
	}
	if sprintOutput != "" {
		fmt.Fprintf(&sb, "## Generator output\n%s\n\n", sprintOutput)
	}

	fmt.Fprintf(&sb, `Review the generator's output against the Sprint %d acceptance criteria.

Write a findings section describing what is correct and what needs improvement.

End your response with exactly ONE of these verdict lines:
- good_to_go
- needs_refinement
- next_sprint

Use "good_to_go" when all acceptance criteria are met.
Use "needs_refinement" when there are fixable issues in the same sprint.
Use "next_sprint" when the sprint deliverables are complete but there is more work in the plan.
`, sprint)

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
