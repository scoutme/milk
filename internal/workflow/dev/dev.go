package dev

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/workflow"
)

const defaultMaxPasses = 3

// WorkflowProgressMsg is sent to the TUI after each state transition.
// cmd/milk handles this type in its Update loop.
type WorkflowProgressMsg struct {
	State workflow.State
}

// WorkflowDoneMsg is sent to the TUI when the workflow ends (success or error).
type WorkflowDoneMsg struct {
	Err error
}

// DevWorkflow implements the designer→generator→evaluator loop.
type DevWorkflow struct {
	MaxPasses int // maximum generator passes per sprint before halting; default 3
	Task      string
}

func New(task string, maxPasses int) *DevWorkflow {
	if maxPasses <= 0 {
		maxPasses = defaultMaxPasses
	}
	return &DevWorkflow{Task: task, MaxPasses: maxPasses}
}

func (w *DevWorkflow) Name() string { return "dev" }

func (w *DevWorkflow) Run(ctx context.Context, cfg workflow.RunConfig) error {
	sess := cfg.Session
	send := cfg.Send
	stateDir := cfg.StateDir

	planPath := filepath.Join(stateDir, sess.ID+".workflow.plan.md")
	statePath := workflow.StatePath(stateDir, sess.ID)

	st := &workflow.State{
		WorkflowName: "dev",
		Task:         w.Task,
		Sprint:       1,
		Pass:         1,
		Role:         "designer",
		AgentMap:     agentMapFromRunners(cfg.Runners),
	}

	sendProgress := func() {
		if send != nil {
			send(WorkflowProgressMsg{State: *st})
		}
	}

	// ── Designer ──────────────────────────────────────────────────────────────
	st.Role = "designer"
	sendProgress()
	designerRunner, ok := cfg.Runners["designer"]
	if !ok {
		return fmt.Errorf("workflow: no runner for role 'designer'")
	}
	emitPrefix(send, "designer")
	designPlan, err := workflow.Turn(ctx, designerRunner, designerPrompt(w.Task), send)
	if err != nil {
		return fmt.Errorf("workflow: designer: %w", err)
	}
	if err := os.WriteFile(planPath, []byte(designPlan), 0o600); err != nil {
		return fmt.Errorf("workflow: writing plan file: %w", err)
	}

	// Derive sprint count from plan headings.
	sprintCount := countSprints(designPlan)
	if sprintCount == 0 {
		sprintCount = 1
	}

	// ── Sprint loop ───────────────────────────────────────────────────────────
	for sprint := 1; sprint <= sprintCount; sprint++ {
		st.Sprint = sprint
		st.Pass = 1

		for {
			sprintPath := filepath.Join(stateDir, fmt.Sprintf("%s.workflow.sprint%d.md", sess.ID, sprint))
			findingsPath := filepath.Join(stateDir, fmt.Sprintf("%s.workflow.findings%d.md", sess.ID, sprint))

			// Generator
			st.Role = "generator"
			sendProgress()
			generatorRunner, ok := cfg.Runners["generator"]
			if !ok {
				return fmt.Errorf("workflow: no runner for role 'generator'")
			}
			emitPrefix(send, fmt.Sprintf("generator sprint %d pass %d", sprint, st.Pass))
			genOutput, err := workflow.Turn(ctx, generatorRunner, generatorPrompt(planPath, sprint, st.Pass, findingsPath), send)
			if err != nil {
				return fmt.Errorf("workflow: generator sprint %d pass %d: %w", sprint, st.Pass, err)
			}
			if err := os.WriteFile(sprintPath, []byte(genOutput), 0o600); err != nil {
				return fmt.Errorf("workflow: writing sprint file: %w", err)
			}

			// Evaluator
			st.Role = "evaluator"
			sendProgress()
			evaluatorRunner, ok := cfg.Runners["evaluator"]
			if !ok {
				return fmt.Errorf("workflow: no runner for role 'evaluator'")
			}
			emitPrefix(send, fmt.Sprintf("evaluator sprint %d pass %d", sprint, st.Pass))
			evalOutput, err := workflow.Turn(ctx, evaluatorRunner, evaluatorPrompt(planPath, sprintPath, sprint, st.Pass), send)
			if err != nil {
				return fmt.Errorf("workflow: evaluator sprint %d pass %d: %w", sprint, st.Pass, err)
			}
			if err := os.WriteFile(findingsPath, []byte(evalOutput), 0o600); err != nil {
				return fmt.Errorf("workflow: writing findings file: %w", err)
			}

			verdict := workflow.ParseVerdict(evalOutput)
			st.VerdictHistory = append(st.VerdictHistory, workflow.VerdictEntry{
				Sprint:  sprint,
				Pass:    st.Pass,
				Verdict: verdict.String(),
			})
			_ = workflow.SaveState(statePath, st)
			sendProgress()

			switch verdict {
			case workflow.VerdictGoodToGo:
				if sprint == sprintCount {
					_ = os.Remove(statePath)
					return nil
				}
				// Advance to next sprint.
				goto nextSprint

			case workflow.VerdictNextSprint:
				goto nextSprint

			case workflow.VerdictNeedsRefinement:
				if st.Pass >= w.MaxPasses {
					return fmt.Errorf("workflow: sprint %d exceeded %d passes without good_to_go; last findings: %s", sprint, w.MaxPasses, findingsPath)
				}
				st.Pass++

			default:
				// VerdictUnknown — treat as needs_refinement with a warning.
				if st.Pass >= w.MaxPasses {
					return fmt.Errorf("workflow: sprint %d: evaluator did not return a recognised verdict after %d passes; last findings: %s", sprint, w.MaxPasses, findingsPath)
				}
				st.Pass++
			}
		}
	nextSprint:
	}

	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

var sprintHeadingRE = regexp.MustCompile(`(?mi)^##\s+Sprint\s+\d+`)

func countSprints(plan string) int {
	return len(sprintHeadingRE.FindAllString(plan, -1))
}

func agentMapFromRunners(runners map[string]workflow.TurnRunner) map[string]string {
	m := make(map[string]string, len(runners))
	for role, r := range runners {
		m[role] = r.Name()
	}
	return m
}

// emitPrefix writes a dim role label to the transcript (mirrors dispatch.go behaviour).
func emitPrefix(send func(tea.Msg), label string) {
	if send == nil {
		return
	}
	send(workflow.WorkflowChunkMsg{Text: "\n" + strings.ToLower(label) + ": "})
}
