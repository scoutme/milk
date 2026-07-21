package dev

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/workflow"
)

const defaultMaxPasses = 5

// WorkflowProgressMsg is sent to the TUI after each state transition.
// cmd/milk handles this type in its Update loop.
type WorkflowProgressMsg struct {
	State workflow.State
}

// WorkflowDoneMsg is sent to the TUI when the workflow ends (success or error).
type WorkflowDoneMsg struct {
	Err error
}

// ErrPassesExhausted is returned (and wrapped in WorkflowDoneMsg.Err) when a
// sprint exhausts its pass limit without reaching good_to_go. The TUI detects
// this sentinel to offer the user a chance to continue with a doubled limit.
type ErrPassesExhausted struct {
	Sprint       int
	MaxPasses    int
	FindingsPath string
}

func (e *ErrPassesExhausted) Error() string {
	return fmt.Sprintf("workflow: sprint %d exceeded %d passes without good_to_go; last findings: %s",
		e.Sprint, e.MaxPasses, e.FindingsPath)
}

// DevWorkflow implements the designer→generator→evaluator loop.
type DevWorkflow struct {
	MaxPasses         int  // maximum generator passes per sprint; overridden by plan declaration unless MaxPassesOverride is set
	MaxPassesOverride bool // when true, MaxPasses wins over the plan-declared max_passes
	Task              string
	ResumeSprint      int // if > 0, skip designer and start from this sprint
	ResumePass        int // if > 0, start from this pass within ResumeSprint
}

func New(task string, maxPasses int) *DevWorkflow {
	if maxPasses <= 0 {
		maxPasses = defaultMaxPasses
	}
	return &DevWorkflow{Task: task, MaxPasses: maxPasses}
}

// NewResume creates a DevWorkflow that skips the designer and restarts from
// the given sprint/pass checkpoint. The plan file must already exist.
// When maxPasses > 0 it is used as a hard override, taking precedence over any
// max_passes directive in the plan (used when the user extends after exhaustion).
func NewResume(task string, maxPasses, sprint, pass int) *DevWorkflow {
	wf := New(task, maxPasses)
	wf.ResumeSprint = sprint
	wf.ResumePass = pass
	if maxPasses > 0 {
		wf.MaxPassesOverride = true
	}
	return wf
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

	var designPlan string

	// ── Designer (skip when resuming with an existing plan) ───────────────────
	if w.ResumeSprint > 0 {
		// Resuming: read the existing plan file, skip the designer.
		data, err := os.ReadFile(planPath)
		if err != nil {
			return fmt.Errorf("workflow: resume: cannot read plan file %s: %w", planPath, err)
		}
		designPlan = string(data)
		emitPrefix(send, "resumed from sprint "+fmt.Sprintf("%d", w.ResumeSprint))
	} else {
		st.Role = "designer"
		sendProgress()
		designerRunner, ok := cfg.Runners["designer"]
		if !ok {
			return fmt.Errorf("workflow: no runner for role 'designer'")
		}
		emitPrefix(send, "designer")
		var err error
		designPlan, err = workflow.Turn(ctx, designerRunner, designerPrompt(w.Task), send)
		if err != nil {
			return fmt.Errorf("workflow: designer: %w", err)
		}
		if err := os.WriteFile(planPath, []byte(designPlan), 0o600); err != nil {
			return fmt.Errorf("workflow: writing plan file: %w", err)
		}
		// Checkpoint after designer so /workflow resume works even if the
		// workflow is interrupted before the first evaluator call.
		st.Sprint = 1
		st.Pass = 1
		st.Role = "generator"
		_ = workflow.SaveState(statePath, st)
	}

	// Derive limits from the plan. Designer-declared values take precedence unless
	// MaxPassesOverride is set (used when the user explicitly extends the limit).
	planMaxPasses, planMaxSprints := parseLimits(designPlan)
	maxPasses := w.MaxPasses
	if planMaxPasses > 0 && !w.MaxPassesOverride {
		maxPasses = planMaxPasses
	}

	sprintCount := countSprints(designPlan)
	if sprintCount == 0 {
		sprintCount = 1
	}
	// planMaxSprints is a safety cap; if designer declared one, honour it.
	if planMaxSprints > 0 && sprintCount > planMaxSprints {
		sprintCount = planMaxSprints
	}

	startSprint := 1
	if w.ResumeSprint > 0 {
		startSprint = w.ResumeSprint
	}

	// ── Sprint loop ───────────────────────────────────────────────────────────
	for sprint := startSprint; sprint <= sprintCount; sprint++ {
		st.Sprint = sprint
		st.Pass = 1
		if sprint == startSprint && w.ResumePass > 1 {
			st.Pass = w.ResumePass
		}

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
			// When the generator completes via tool calls and produces no closing text,
			// substitute a git diff summary so the evaluator can see what changed.
			if strings.TrimSpace(genOutput) == "" {
				genOutput = gitDiffSummary()
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
			evalOutput, err := workflow.Turn(ctx, evaluatorRunner, evaluatorPrompt(planPath, sprintPath, sprint, st.Pass, maxPasses), send)
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

			case workflow.VerdictSprintDone:
				if sprint == sprintCount {
					_ = os.Remove(statePath)
					return nil
				}
				goto nextSprint

			case workflow.VerdictNeedsRefinement:
				if st.Pass >= maxPasses {
					return &ErrPassesExhausted{Sprint: sprint, MaxPasses: maxPasses, FindingsPath: findingsPath}
				}
				st.Pass++

			default:
				// VerdictUnknown — treat as needs_refinement with a warning.
				if st.Pass >= maxPasses {
					return &ErrPassesExhausted{Sprint: sprint, MaxPasses: maxPasses, FindingsPath: findingsPath}
				}
				st.Pass++
			}
		}
	nextSprint:
	}

	_ = os.Remove(statePath)
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

var sprintHeadingRE = regexp.MustCompile(`(?mi)^##\s+Sprint\s+\d+`)

func countSprints(plan string) int {
	return len(sprintHeadingRE.FindAllString(plan, -1))
}

var (
	maxPassesRE  = regexp.MustCompile(`(?mi)^\s*max_passes\s*:\s*(\d+)`)
	maxSprintsRE = regexp.MustCompile(`(?mi)^\s*max_sprints\s*:\s*(\d+)`)
)

// parseLimits extracts max_passes and max_sprints from the plan text.
// Returns 0 for any value not found.
func parseLimits(plan string) (maxPasses, maxSprints int) {
	if m := maxPassesRE.FindStringSubmatch(plan); len(m) == 2 {
		fmt.Sscanf(m[1], "%d", &maxPasses) //nolint:errcheck
	}
	if m := maxSprintsRE.FindStringSubmatch(plan); len(m) == 2 {
		fmt.Sscanf(m[1], "%d", &maxSprints) //nolint:errcheck
	}
	return
}

func agentMapFromRunners(runners map[string]workflow.TurnRunner) map[string]string {
	m := make(map[string]string, len(runners))
	for role, r := range runners {
		m[role] = r.Name()
	}
	return m
}

// gitDiffSummary returns a human-readable summary of uncommitted changes via
// `git diff HEAD` (falls back to `git diff` if HEAD doesn't exist). Used when
// the generator completes its work entirely via tool calls without producing
// a closing text summary.
func gitDiffSummary() string {
	stat, err := exec.Command("git", "diff", "--stat", "HEAD").Output()
	if err != nil {
		stat, err = exec.Command("git", "diff", "--stat").Output()
		if err != nil {
			return "(generator used tools to make changes; no git diff available)"
		}
	}
	if strings.TrimSpace(string(stat)) == "" {
		return "(generator completed via tool calls; no uncommitted changes detected)"
	}
	diff, err := exec.Command("git", "diff", "HEAD").Output()
	if err != nil {
		diff, _ = exec.Command("git", "diff").Output()
	}
	const maxDiffBytes = 32 * 1024
	diffStr := string(diff)
	if len(diffStr) > maxDiffBytes {
		diffStr = diffStr[:maxDiffBytes] + "\n... (diff truncated)"
	}
	return "Generator completed via tool calls. Changes made:\n\n```diff\n" + strings.TrimSpace(string(stat)) + "\n```\n\n" + diffStr
}

// emitPrefix writes a dim role label to the transcript (mirrors dispatch.go behaviour).
func emitPrefix(send func(tea.Msg), label string) {
	if send == nil {
		return
	}
	send(workflow.WorkflowChunkMsg{Text: "\n" + strings.ToLower(label) + ": "})
}
