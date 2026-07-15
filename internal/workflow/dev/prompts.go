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

## Limits
Declare the execution limits for this workflow on two lines, exactly as shown:
  max_passes: <N>
  max_sprints: <N>

max_passes is the maximum number of generator→evaluator iterations allowed per sprint
before the workflow halts with an error. Set it based on task complexity: 1 for trivial
tasks, 2–3 for normal tasks, up to 5 for tasks where convergence may take several rounds.

max_sprints is a safety cap on the total number of sprints. It must equal or exceed the
number of ## Sprint N sections you define below. Use it to prevent runaway loops.

## Sprint Plan
Use one section per sprint, each headed exactly as "## Sprint N" (e.g. "## Sprint 1").
For each sprint list the concrete deliverables: files to create or modify, tests to write,
testing instructions for a human reviewer.

Keep the number of sprints to the minimum needed. If the task fits in one sprint, use one.
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
Use your tools (read_file, write_file, edit_file, bash, etc.) to read, create, and modify files.
After completing all tool use, write a plain-text summary of what you did: which files were
created or modified, what each change does, and which acceptance criteria it satisfies.
This written summary is mandatory — the evaluator reads it to assess the sprint.
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
Use "needs_refinement" when there are fixable issues in the same sprint.
Use "sprint_done" when the sprint deliverables are complete (whether or not more sprints follow).
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
