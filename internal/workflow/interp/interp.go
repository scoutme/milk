// Package interp is the generic interpreter for workflow.Definition — the
// data-driven counterpart to internal/workflow/dev.DevWorkflow. It walks a
// Definition's Stage tree instead of following a hand-written Go control
// flow, so new workflow shapes (see workflow.Definition's four stage kinds)
// can be added as YAML/JSON rather than as a new Go package.
//
// Deliberately deferred in this first cut, pending a parity pass against
// dev.go (see the design discussion on GitHub issue #117):
//   - Checkpoint/resume. Runner.Run does not read or write workflow.State —
//     an interrupted run cannot be resumed. dev.go remains the
//     resume-capable implementation until this lands.
//   - On-disk plan/sprint/findings artifact files (workflow.PlanPath et
//     al.) — the interpreter keeps every stage's output in memory (Vars)
//     and never round-trips through disk, since nothing here reads them
//     back. Needed once resume is added.
//   - A CLI-supplied max-iterations override taking precedence over a
//     plan-declared value (dev.go's MaxPassesOverride).
//   - dev.go's git-diff fallback when a generator's tool calls produce no
//     closing text summary.
package interp

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"sync"

	"github.com/scoutme/milk/internal/workflow"
)

// ExhaustedError is returned when an iteration-bounded loop stage retries
// MaxIterations times without its body reporting "break". Mirrors dev.go's
// ErrPassesExhausted.
type ExhaustedError struct {
	StageID       string
	MaxIterations int
}

func (e *ExhaustedError) Error() string {
	return fmt.Sprintf("workflow: stage %q exceeded %d iterations without breaking", e.StageID, e.MaxIterations)
}

// ItemResult is one parallel_group body's outcome, aggregated under the
// stage's SaveAs entry.
type ItemResult struct {
	Index  int
	Label  string
	Status string // the body's loop outcome ("break"/""), or "error"
	Output string
	Err    string
}

// Runner adapts a workflow.Definition to the workflow.Workflow interface.
type Runner struct {
	Def  workflow.Definition
	Task string
}

// New constructs a Runner for def, seeded with the initial task/prompt.
func New(def workflow.Definition, task string) *Runner {
	return &Runner{Def: def, Task: task}
}

func (r *Runner) Name() string { return r.Def.Name }

// Run executes every top-level stage in order. See the package doc for what
// is intentionally not yet implemented (checkpoint/resume).
func (r *Runner) Run(ctx context.Context, cfg workflow.RunConfig) error {
	vars := make(map[string]any, len(r.Def.Vars)+1)
	for k, v := range r.Def.Vars {
		vars[k] = v
	}
	// "task" is reserved for the workflow's overall task description — a
	// loop/parallel_group stage with over: "Task" would overwrite it for the
	// duration of that iteration (label lowercased == "task"). Definitions
	// needing a "Task N" declared-section vocabulary should pick a
	// non-colliding label, e.g. "Item".
	vars["task"] = r.Task
	ec := &execContext{ctx: ctx, cfg: cfg, vars: vars}
	_, err := executeStages(ec, r.Def.Stages)
	return err
}

// execContext carries the state threaded through one stage-tree walk. vars
// is mutated in place by sequential stages; a parallel_group stage instead
// gives each concurrent body its own copy (see execParallelGroup) since
// concurrent writers to a shared map would race.
type execContext struct {
	ctx  context.Context
	cfg  workflow.RunConfig
	vars map[string]any
}

// executeStages runs stages in order, returning the last verdict-bearing
// stage's outcome ("break"/"retry"/"") — a loop stage above consumes that to
// decide whether to continue or stop; a non-loop caller ignores it.
func executeStages(ec *execContext, stages []workflow.Stage) (string, error) {
	outcome := ""
	for _, s := range stages {
		o, err := executeStage(ec, s)
		if err != nil {
			return "", err
		}
		if o != "" {
			outcome = o
		}
	}
	return outcome, nil
}

func executeStage(ec *execContext, s workflow.Stage) (string, error) {
	switch s.Kind {
	case workflow.StageKindAgentTurn:
		return execAgentTurn(ec, s)
	case workflow.StageKindLoop:
		return "", execLoop(ec, s)
	case workflow.StageKindUserCheckpoint:
		return "", execUserCheckpoint(ec, s)
	case workflow.StageKindParallelGroup:
		return "", execParallelGroup(ec, s)
	default:
		return "", fmt.Errorf("workflow: stage %q: unknown kind %q", s.ID, s.Kind)
	}
}

var markerLineRE = func(marker string) *regexp.Regexp {
	return regexp.MustCompile(`(?mi)^[ \t]*` + regexp.QuoteMeta(marker) + `[ \t]*$`)
}

func execAgentTurn(ec *execContext, s workflow.Stage) (string, error) {
	if s.RunUnlessContains != "" {
		gate, _ := ec.vars[s.RunUnlessMarkerIn].(string)
		if markerLineRE(s.RunUnlessContains).FindStringIndex(gate) != nil {
			return "", nil
		}
	}

	runner, ok := ec.cfg.Runners[s.Role]
	if !ok {
		return "", fmt.Errorf("workflow: no runner for role %q (stage %q)", s.Role, s.ID)
	}

	prompt, err := renderTemplate(s.ID+".prompt", s.Prompt, ec.vars)
	if err != nil {
		return "", fmt.Errorf("workflow: stage %q: %w", s.ID, err)
	}
	out, err := workflow.Turn(ec.ctx, runner, prompt, ec.cfg.Send)
	if err != nil {
		return "", fmt.Errorf("workflow: stage %q: %w", s.ID, err)
	}

	if s.SkipUserCheckpointMarker != "" {
		out, err = resolveUserCheckpointDance(ec, s, runner, out)
		if err != nil {
			return "", err
		}
	}

	if s.SaveAs != "" {
		ec.vars[s.SaveAs] = out
	}

	if len(s.Verdict) == 0 {
		return "", nil
	}
	v := workflow.ParseVerdict(out)
	rule, ok := s.Verdict[v.String()]
	if !ok {
		return "", fmt.Errorf("workflow: stage %q: no verdict rule for %q", s.ID, v.String())
	}
	if rule.Warn && ec.cfg.Send != nil {
		ec.cfg.Send(workflow.WorkflowChunkMsg{Text: fmt.Sprintf(
			"\n[warning: unexpected verdict %q at stage %q, treating as %q]\n", v.String(), s.ID, rule.Action)})
	}
	return rule.Action, nil
}

// resolveUserCheckpointDance implements the SkipUserCheckpointMarker /
// OnAnswerPrompt behavior documented on workflow.Stage: if out contains a
// standalone marker line, everything after it is the final output; otherwise
// out is treated as disambiguating questions, paused for a user answer via
// cfg.AnswersCh, then re-run as a second turn on the same role.
func resolveUserCheckpointDance(ec *execContext, s workflow.Stage, runner workflow.TurnRunner, out string) (string, error) {
	re := markerLineRE(s.SkipUserCheckpointMarker)
	if loc := re.FindStringIndex(out); loc != nil {
		return strings.TrimSpace(out[loc[1]:]), nil
	}

	if ec.cfg.Send == nil || ec.cfg.AnswersCh == nil {
		return "", fmt.Errorf("workflow: stage %q: produced disambiguation questions but no interactive channel is available to answer them", s.ID)
	}

	ec.vars[s.ID+"_questions"] = out
	ec.cfg.Send(workflow.WorkflowQuestionsMsg{Questions: out, AnswersCh: ec.cfg.AnswersCh})
	select {
	case answer := <-ec.cfg.AnswersCh:
		ec.vars[s.ID+"_answer"] = answer
	case <-ec.ctx.Done():
		return "", ec.ctx.Err()
	}

	finalPrompt, err := renderTemplate(s.ID+".on_answer_prompt", s.OnAnswerPrompt, ec.vars)
	if err != nil {
		return "", fmt.Errorf("workflow: stage %q: %w", s.ID, err)
	}
	final, err := workflow.Turn(ec.ctx, runner, finalPrompt, ec.cfg.Send)
	if err != nil {
		return "", fmt.Errorf("workflow: stage %q (finalize): %w", s.ID, err)
	}
	return final, nil
}

func execUserCheckpoint(ec *execContext, s workflow.Stage) error {
	if ec.cfg.Send == nil || ec.cfg.AnswersCh == nil {
		return fmt.Errorf("workflow: stage %q (user_checkpoint): no interactive channel available", s.ID)
	}
	prompt, err := renderTemplate(s.ID+".prompt", s.Prompt, ec.vars)
	if err != nil {
		return fmt.Errorf("workflow: stage %q: %w", s.ID, err)
	}
	ec.cfg.Send(workflow.WorkflowQuestionsMsg{Questions: prompt, AnswersCh: ec.cfg.AnswersCh})
	select {
	case answer := <-ec.cfg.AnswersCh:
		if s.SaveAs != "" {
			ec.vars[s.SaveAs] = answer
		}
		return nil
	case <-ec.ctx.Done():
		return ec.ctx.Err()
	}
}

func execLoop(ec *execContext, s workflow.Stage) error {
	if s.Over != "" {
		return execOverLoop(ec, s)
	}
	return execBoundedLoop(ec, s)
}

// execOverLoop runs Body once per declared section named by Over (parsed out
// of Vars[From]), exposing the current section as "<label>" (its 1-based
// declared index) and "<label>_section" (its body text). Falls back to a
// single empty-body iteration when no matching sections are found, mirroring
// dev.go's "assume one sprint" fallback when the designer's plan doesn't
// declare any.
func execOverLoop(ec *execContext, s workflow.Stage) error {
	doc, _ := ec.vars[s.From].(string)
	decls := ParseDeclarations(doc)
	sections := decls.SectionsFor(s.Over)
	if len(sections) == 0 {
		sections = []Section{{Label: s.Over, Index: 1}}
	}
	label := strings.ToLower(s.Over)
	for _, sec := range sections {
		ec.vars[label] = sec.Index
		ec.vars[label+"_section"] = sec.Body
		if _, err := executeStages(ec, s.Body); err != nil {
			return err
		}
	}
	return nil
}

// execBoundedLoop runs Body up to MaxIterations times (or the value named by
// MaxIterationsFrom, resolved from Vars[From]), exposing the current 1-based
// iteration as IterationVar. Body's reported outcome after each iteration —
// "break" (or no verdict-bearing stage at all) stops the loop successfully;
// "retry" runs it again, subject to the bound.
func execBoundedLoop(ec *execContext, s workflow.Stage) error {
	maxIter := s.MaxIterations
	if s.MaxIterationsFrom != "" {
		doc, _ := ec.vars[s.From].(string)
		decls := ParseDeclarations(doc)
		if n, ok := decls.Scalars[strings.ToLower(s.MaxIterationsFrom)]; ok && n > 0 {
			maxIter = n
		}
		ec.vars[strings.ToLower(s.MaxIterationsFrom)] = maxIter
	}
	if maxIter <= 0 {
		maxIter = 1
	}
	for iter := 1; iter <= maxIter; iter++ {
		if s.IterationVar != "" {
			ec.vars[s.IterationVar] = iter
		}
		outcome, err := executeStages(ec, s.Body)
		if err != nil {
			return err
		}
		switch outcome {
		case "break", "":
			return nil
		case "retry":
			if iter == maxIter {
				return &ExhaustedError{StageID: s.ID, MaxIterations: maxIter}
			}
		default:
			return fmt.Errorf("workflow: stage %q: unknown loop outcome %q", s.ID, outcome)
		}
	}
	return nil
}

// execParallelGroup runs Body once per declared section named by Over,
// concurrently within each dependency wave (bounded by MaxConcurrency) and
// giving each a private copy of Vars — concurrent writers to a shared map
// would race, and each item's intermediate SaveAs values (e.g. a per-item
// "worker_output") are meaningless to its siblings. Items with a DependsOn
// declaration only start once every dependency's wave has finished (see
// computeWaves); a dependency cycle is reported as an error rather than
// deadlocking. One item's error does not cancel the others in its wave or
// abort later waves; results aggregate into Vars[SaveAs] as []ItemResult, in
// declaration order regardless of wave grouping.
func execParallelGroup(ec *execContext, s workflow.Stage) error {
	doc, _ := ec.vars[s.From].(string)
	decls := ParseDeclarations(doc)
	sections := decls.SectionsFor(s.Over)
	if len(sections) == 0 {
		return nil
	}
	waves, err := computeWaves(sections)
	if err != nil {
		return fmt.Errorf("workflow: stage %q: %w", s.ID, err)
	}
	maxConcurrency := s.MaxConcurrency
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	label := strings.ToLower(s.Over)
	results := make([]ItemResult, len(sections))
	posByIndex := make(map[int]int, len(sections))
	for i, sec := range sections {
		posByIndex[sec.Index] = i
	}

	for _, wave := range waves {
		sem := make(chan struct{}, maxConcurrency)
		var wg sync.WaitGroup
		for _, sec := range wave {
			wg.Add(1)
			sem <- struct{}{}
			go func(sec Section) {
				defer wg.Done()
				defer func() { <-sem }()

				itemVars := make(map[string]any, len(ec.vars)+2)
				maps.Copy(itemVars, ec.vars)
				itemVars[label] = sec.Index
				itemVars[label+"_section"] = sec.Body

				itemEC := &execContext{ctx: ec.ctx, cfg: ec.cfg, vars: itemVars}
				outcome, err := executeStages(itemEC, s.Body)
				r := ItemResult{Index: sec.Index, Label: sec.Label, Status: outcome}
				if lastSaveAs := lastSaveAsOf(s.Body); lastSaveAs != "" {
					if out, ok := itemVars[lastSaveAs].(string); ok {
						r.Output = out
					}
				}
				if err != nil {
					r.Status = "error"
					r.Err = err.Error()
				}
				results[posByIndex[sec.Index]] = r
			}(sec)
		}
		wg.Wait()
	}
	if s.SaveAs != "" {
		ec.vars[s.SaveAs] = results
	}
	return nil
}

// computeWaves groups sections into dependency waves: wave 0 holds every
// section with no DependsOn, wave 1 holds sections whose dependencies are
// all satisfied by wave 0, and so on. Sections within a wave have no
// dependency relationship to each other and can run concurrently; a wave
// only starts once every earlier wave has fully finished. Returns an error
// if a dependency cycle (or a reference to a nonexistent index) prevents any
// section from ever becoming ready.
func computeWaves(sections []Section) ([][]Section, error) {
	resolved := make(map[int]bool, len(sections))
	remaining := make([]Section, len(sections))
	copy(remaining, sections)

	var waves [][]Section
	for len(remaining) > 0 {
		var wave, next []Section
		for _, sec := range remaining {
			ready := true
			for _, dep := range sec.DependsOn {
				if !resolved[dep] {
					ready = false
					break
				}
			}
			if ready {
				wave = append(wave, sec)
			} else {
				next = append(next, sec)
			}
		}
		if len(wave) == 0 {
			stuck := make([]int, len(next))
			for i, sec := range next {
				stuck[i] = sec.Index
			}
			return nil, fmt.Errorf("unresolvable dependency cycle (or reference to a missing item) among items: %v", stuck)
		}
		for _, sec := range wave {
			resolved[sec.Index] = true
		}
		waves = append(waves, wave)
		remaining = next
	}
	return waves, nil
}

func lastSaveAsOf(body []workflow.Stage) string {
	for i := len(body) - 1; i >= 0; i-- {
		if body[i].SaveAs != "" {
			return body[i].SaveAs
		}
	}
	return ""
}
