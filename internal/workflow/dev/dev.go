package dev

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
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
	ResumeSprint      int    // if > 0, skip designer and start from this sprint
	ResumePass        int    // if > 0, start from this pass within ResumeSprint
	ResumeRole        string // if "evaluator", skip generator for the first resumed sprint/pass
}

func New(task string, maxPasses int) *DevWorkflow {
	if maxPasses <= 0 {
		maxPasses = defaultMaxPasses
	}
	return &DevWorkflow{Task: task, MaxPasses: maxPasses}
}

// NewResume creates a DevWorkflow that skips the designer and restarts from
// the given sprint/pass/role checkpoint. The plan file must already exist.
// role should be the saved State.Role value ("generator" or "evaluator").
// When maxPasses > 0 it is used as a hard override, taking precedence over any
// max_passes directive in the plan (used when the user extends after exhaustion).
func NewResume(task string, maxPasses, sprint, pass int, role string) *DevWorkflow {
	wf := New(task, maxPasses)
	wf.ResumeSprint = sprint
	wf.ResumePass = pass
	wf.ResumeRole = role
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

	wfID := cfg.WorkflowID
	planPath := workflow.PlanPath(stateDir, sess.ID, wfID)
	statePath := workflow.StatePath(stateDir, sess.ID, wfID)

	st := &workflow.State{
		WorkflowName: "dev",
		Task:         w.Task,
		Sprint:       1,
		Pass:         1,
		Role:         "designer",
		AgentMap:     agentMapFromRunners(cfg.Runners),
		WorkflowID:   wfID,
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
		// st starts as a fresh State above, which would otherwise drop the
		// verdict history for every sprint/pass completed before the resume
		// point — carry it forward so the workflow panel still shows them.
		if prev, err := workflow.LoadState(statePath); err == nil && prev != nil {
			st.VerdictHistory = prev.VerdictHistory
		}
		emitPrefix(send, "resumed from sprint "+fmt.Sprintf("%d", w.ResumeSprint))
	} else {
		st.Role = "designer"
		sendProgress()
		designerRunner, ok := cfg.Runners["designer"]
		if !ok {
			return fmt.Errorf("workflow: no runner for role 'designer'")
		}
		emitPrefix(send, "designer")

		// Two-step designer flow: first ask questions, then finalize with answers.
		if cfg.Send != nil && cfg.AnswersCh != nil {
			// Step 1: Ask designer to identify ambiguities.
			questionsOutput, err := workflow.Turn(ctx, designerRunner, designerQuestionsPrompt(w.Task), send)
			if err != nil {
				return fmt.Errorf("workflow: designer questions: %w", err)
			}

			if hasQuestions(questionsOutput) {
				// Step 2: Send questions to TUI and wait for user answers.
				cfg.Send(workflow.WorkflowQuestionsMsg{
					Questions: questionsOutput,
					AnswersCh: cfg.AnswersCh, // pass chan so TUI can send answers back
				})
				// Wait for user answers (or context cancellation).
				var answers string
				select {
				case answers = <-cfg.AnswersCh:
					// Continue with answers.
				case <-ctx.Done():
					return ctx.Err()
				}

				// Step 3: Finalize plan with user's answers.
				emitPrefix(send, "designer (finalizing)")
				designPlan, err = workflow.Turn(ctx, designerRunner, designerFinalizePrompt(w.Task, questionsOutput, answers), send)
				if err != nil {
					return fmt.Errorf("workflow: designer finalize: %w", err)
				}
			} else {
				// No questions — discard any preamble before the NO_QUESTIONS
				// marker line and use the rest as the plan.
				designPlan = stripNoQuestionsPreamble(questionsOutput)
			}
		} else {
			// Legacy path: no disambiguation channels, single designer call.
			var err error
			designPlan, err = workflow.Turn(ctx, designerRunner, designerPrompt(w.Task), send)
			if err != nil {
				return fmt.Errorf("workflow: designer: %w", err)
			}
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
	// Recorded so the TUI can show "sprint X/Y" instead of just "sprint X" —
	// lets the user judge overall progress, not just the current position.
	st.TotalSprints = sprintCount

	startSprint := 1
	if w.ResumeSprint > 0 {
		startSprint = w.ResumeSprint
	}
	// Backfill any sprint before the resume point that is missing from
	// VerdictHistory, using its findings file as ground truth. This closes
	// gaps left by a bug or crash that dropped history before it could be
	// checkpointed (e.g. a stale/incorrect sprint count that ended the run
	// early, or an interruption before a save) — the sprint history shown in
	// the workflow panel should reflect everything that actually happened,
	// not just what the last successful checkpoint recorded.
	st.VerdictHistory = backfillVerdictHistory(st.VerdictHistory, stateDir, sess.ID, wfID, startSprint)
	// resumeRole is the role to start from within the first resumed sprint/pass.
	// Empty string means start from generator (normal case). "evaluator" means
	// the generator already completed and we resume at the evaluator.
	resumeRole := ""
	if w.ResumeSprint > 0 {
		resumeRole = w.ResumeRole
	}
	// isResumeOpeningTurn marks the single inner-loop iteration that resumes
	// an existing run — skipGenerator below depends on the role/sprint/pass
	// exactly as they were persisted before the resume, so that one turn must
	// not be checkpointed over before it's evaluated. False for a fresh run
	// (there is nothing to preserve) and for every turn after the first.
	isResumeOpeningTurn := w.ResumeSprint > 0

	// ── Sprint loop ───────────────────────────────────────────────────────────
	for sprint := startSprint; sprint <= sprintCount; sprint++ {
		st.Sprint = sprint
		st.Pass = 1
		if sprint == startSprint && w.ResumePass > 1 {
			st.Pass = w.ResumePass
		}

		for {
			// Checkpoint before attempting this pass's generator (except the
			// resume-opening turn, see isResumeOpeningTurn) so that if the
			// generator fails outright — e.g. a transient network error that
			// exhausts retries — the on-disk state already reflects this
			// sprint/pass instead of whatever the previous sprint/pass last
			// left behind. Without this, resuming after such a failure would
			// re-target the last successfully-checkpointed (already-approved)
			// sprint/pass rather than the one that actually needs retrying.
			if !isResumeOpeningTurn {
				st.Role = "generator"
				_ = workflow.SaveState(statePath, st)
			}
			isResumeOpeningTurn = false

			sprintPath := workflow.SprintPath(stateDir, sess.ID, wfID, sprint)
			findingsPath := workflow.FindingsPath(stateDir, sess.ID, wfID, sprint)

			// Skip generator when resuming at the evaluator for this sprint/pass.
			skipGenerator := resumeRole == "evaluator" &&
				sprint == startSprint && st.Pass == w.ResumePass
			resumeRole = "" // only applies to the first resumed turn

			if skipGenerator {
				emitPrefix(send, fmt.Sprintf("resuming at evaluator sprint %d pass %d", sprint, st.Pass))
			} else {
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
				// Checkpoint with role="evaluator" so a resume after generator
				// completion skips straight to the evaluator.
				st.Role = "evaluator"
				_ = workflow.SaveState(statePath, st)
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
					st.Role = "done"
					_ = workflow.SaveState(statePath, st)
					return nil
				}
				// Advance to next sprint.
				goto nextSprint

			case workflow.VerdictSprintDone:
				if sprint == sprintCount {
					st.Role = "done"
					_ = workflow.SaveState(statePath, st)
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

	st.Role = "done"
	_ = workflow.SaveState(statePath, st)
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// backfillVerdictHistory ensures every sprint strictly before startSprint has
// at least one entry in history, deriving a best-effort entry from that
// sprint's findings file (ground truth for what the evaluator last decided)
// when history has none for it. The exact pass count for a backfilled entry
// is unrecoverable once its findings file has been overwritten by a later
// pass — findingsN.md holds only the latest pass's content — so it is
// recorded as pass 0 to mark it as reconstructed rather than recorded live.
func backfillVerdictHistory(history []workflow.VerdictEntry, stateDir, sessionID string, workflowID, startSprint int) []workflow.VerdictEntry {
	have := make(map[int]bool, len(history))
	for _, v := range history {
		have[v.Sprint] = true
	}
	for sprint := 1; sprint < startSprint; sprint++ {
		if have[sprint] {
			continue
		}
		findingsPath := workflow.FindingsPath(stateDir, sessionID, workflowID, sprint)
		data, err := os.ReadFile(findingsPath)
		if err != nil {
			continue
		}
		verdict := workflow.ParseVerdict(string(data))
		if verdict == workflow.VerdictUnknown {
			continue
		}
		history = append(history, workflow.VerdictEntry{Sprint: sprint, Pass: 0, Verdict: verdict.String()})
	}
	sort.Slice(history, func(i, j int) bool {
		if history[i].Sprint != history[j].Sprint {
			return history[i].Sprint < history[j].Sprint
		}
		return history[i].Pass < history[j].Pass
	})
	return history
}

// sprintHeadingRE matches "## Sprint N" headings (case-insensitive). The hash
// count is flexible (2-6 "#") because designers sometimes nest sprints one
// level deeper (e.g. "### Sprint N" under a "## Sprint Plan" heading) despite
// planInstructions asking for exactly "##" — the parser should not silently
// drop sprints just because the heading level didn't match instructions.
var sprintHeadingRE = regexp.MustCompile(`(?mi)^#{2,6}\s+Sprint\s+(\d+)`)

// countSprints returns the number of "## Sprint N" headings in the plan.
func countSprints(plan string) int {
	return len(sprintHeadingRE.FindAllString(plan, -1))
}

// parseSprintSection extracts the content under "## Sprint N" up to the next
// "## Sprint" heading or end of file. Returns empty string if the sprint is not found.
func parseSprintSection(plan string, sprint int) string {
	matches := sprintHeadingRE.FindAllStringIndex(plan, -1)
	target := fmt.Sprintf("Sprint %d", sprint)
	for i, loc := range matches {
		// Check if this heading matches the target sprint number.
		heading := plan[loc[0]:loc[1]]
		if !strings.Contains(strings.ToLower(heading), strings.ToLower(target)) {
			continue
		}
		end := len(plan)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		return strings.TrimSpace(plan[loc[0]:end])
	}
	return ""
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
