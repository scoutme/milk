// Package interp is the generic interpreter for workflow.Definition — the
// data-driven counterpart to internal/workflow/dev.DevWorkflow. It walks a
// Definition's Stage tree instead of following a hand-written Go control
// flow, so new workflow shapes (see workflow.Definition's four stage kinds)
// can be added as YAML/JSON rather than as a new Go package.
//
// Checkpoint/resume (see checkpoint.go) persists a flat, positionally-replayed
// trace of completed leaf stages — see TraceEntry's doc for why that's
// sufficient without recording loop iteration numbers. Runner.WithCheckpoint
// opts a run into it; without it, Run behaves exactly as before (nothing
// written, nothing read). cmd/milk's /workflow resume/reconfigure/clear all
// call it (see workflow.CurrentWorkflowID's WorkflowKind). A CLI-supplied
// max_iterations override (Runner.WithMaxIterationsOverride) beats a
// plan-declared value for the "continue with doubled passes?" recovery flow
// after an ExhaustedError — dev.go's MaxPassesOverride equivalent.
// Stage.EmptyOutputFallback == "git_diff" covers dev.go's git-diff fallback
// when a turn's tool calls produce no closing text summary.
//
// Still deliberately deferred, pending a parity pass against dev.go (see the
// design discussion on GitHub issue #117):
//   - On-disk plan/sprint/findings artifact files (workflow.PlanPath et
//     al.) in dev.go's specific per-file layout — this package's checkpoint
//     already persists every stage's output (via the trace), just as one
//     JSON file rather than separate inspectable markdown files.
//   - Live stage-by-stage progress reporting for the TUI panel (dev.go's
//     wfdev.WorkflowProgressMsg) — an interpreter-driven run's panel entry
//     shows a fixed "starting" state until the run finishes.
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
	Def              workflow.Definition
	Task             string
	checkpointPath   string            // "" = no checkpointing; see WithCheckpoint
	agentMap         map[string]string // see WithAgentMap
	maxIterOverrides map[string]int    // stage ID -> forced max_iterations; see WithMaxIterationsOverride
}

// New constructs a Runner for def, seeded with the initial task/prompt.
func New(def workflow.Definition, task string) *Runner {
	return &Runner{Def: def, Task: task}
}

// WithCheckpoint returns a copy of r that persists a checkpoint to path as
// the run progresses (see checkpoint.go), and resumes from it instead of
// starting fresh if path already holds an unfinished, matching checkpoint
// (same definition name and task). Safe to call unconditionally — a fresh
// run with no existing file at path just starts normally and begins
// checkpointing from its first completed stage.
func (r *Runner) WithCheckpoint(path string) *Runner {
	c := *r
	c.checkpointPath = path
	return &c
}

// WithAgentMap attaches role -> resolved agent name, persisted into every
// checkpoint written from this point on so a later resume can rebuild the
// same TurnRunners without re-asking a role-selection wizard. No effect
// unless WithCheckpoint is also used.
func (r *Runner) WithAgentMap(m map[string]string) *Runner {
	c := *r
	c.agentMap = m
	return &c
}

// WithMaxIterationsOverride forces the named "loop" stage's max_iterations
// bound to n for this run, beating both its static MaxIterations and any
// MaxIterationsFrom-resolved plan value. Intended for the "continue with
// doubled passes?" recovery flow after an ExhaustedError: resuming from the
// checkpoint that produced the error, with the override raised, replays every
// already-recorded pass and then — since the loop's bound is now higher —
// proceeds to a genuinely new one instead of immediately re-exhausting.
func (r *Runner) WithMaxIterationsOverride(stageID string, n int) *Runner {
	c := *r
	c.maxIterOverrides = make(map[string]int, len(r.maxIterOverrides)+1)
	maps.Copy(c.maxIterOverrides, r.maxIterOverrides)
	c.maxIterOverrides[stageID] = n
	return &c
}

func (r *Runner) Name() string { return r.Def.Name }

// Run executes every top-level stage in order, resuming from r.checkpointPath
// when WithCheckpoint was used and a matching unfinished checkpoint exists
// there. See the package doc for what checkpoint/resume does not yet cover.
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

	var trace []TraceEntry
	if r.checkpointPath != "" {
		var err error
		trace, err = loadResumableTrace(r.checkpointPath, r.Def.Name, r.Task)
		if err != nil {
			return err
		}
	}

	activeMu := &sync.Mutex{}
	ec := &execContext{ctx: ctx, cfg: cfg, vars: vars, replay: trace, maxIterOverrides: r.maxIterOverrides, defName: r.Def.Name, activeMu: activeMu}
	if r.checkpointPath != "" {
		liveTrace := append([]TraceEntry{}, trace...)
		ec.onLeafComplete = func(e TraceEntry) {
			liveTrace = append(liveTrace, e)
			// Best-effort: a failed write here surfaces as a lost checkpoint
			// on the next crash, not a failed turn — the run itself already
			// succeeded and should not be undone by a disk-write hiccup.
			_ = SaveCheckpoint(r.checkpointPath, &Checkpoint{
				DefinitionName: r.Def.Name, Task: r.Task, Trace: liveTrace, AgentMap: r.agentMap,
			})
		}
	}

	_, err := executeStages(ec, r.Def.Stages)
	if err == nil && r.checkpointPath != "" {
		final, loadErr := LoadCheckpoint(r.checkpointPath)
		if loadErr != nil || final == nil {
			final = &Checkpoint{DefinitionName: r.Def.Name, Task: r.Task, AgentMap: r.agentMap}
		}
		final.Done = true
		_ = SaveCheckpoint(r.checkpointPath, final)
	}
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

	// replay holds trace entries loaded from an existing checkpoint, still
	// to be consumed positionally (see TraceEntry's doc); nil/exhausted once
	// live execution takes over. onLeafComplete, when checkpointing is
	// enabled, is called after each newly-completed leaf stage (not each
	// replayed one — those are already on disk).
	replay         []TraceEntry
	replayIdx      int
	onLeafComplete func(TraceEntry)

	// maxIterOverrides forces a named "loop" stage's max_iterations bound;
	// see Runner.WithMaxIterationsOverride.
	maxIterOverrides map[string]int

	// defName and activePaths back workflow.ProgressMsg reporting (see
	// reportProgress): defName is the Definition's own name (set once, in
	// Run); activePaths is a shared, mutex-protected list of currently-active
	// path segments from root to the deepest executing stage. Every
	// goroutine (sequential or parallel) pushes/pops its own segment via
	// pushPath/popPath; reportProgress builds a tree snapshot from the list.
	defName     string
	activePaths []string    // shared across goroutines — protect with activeMu
	activeMu    *sync.Mutex // shared across goroutines; nil only in test stubs
}

// pushPath appends a segment to the shared activePaths list.
func (ec *execContext) pushPath(seg string) {
	ec.activeMu.Lock()
	ec.activePaths = append(ec.activePaths, seg)
	ec.activeMu.Unlock()
}

// popPath removes the last segment from the shared activePaths list.
func (ec *execContext) popPath() {
	ec.activeMu.Lock()
	ec.activePaths = ec.activePaths[:len(ec.activePaths)-1]
	ec.activeMu.Unlock()
}

// buildPathTree constructs a PathSnapshot from the current activePaths.
// The deepest path (last segment at each level) is highlighted by being
// the last child. Concurrent siblings under a parallel group each form
// their own branch.
func (ec *execContext) buildPathTree() *workflow.PathSnapshot {
	ec.activeMu.Lock()
	snapshot := make([]string, len(ec.activePaths))
	copy(snapshot, ec.activePaths)
	ec.activeMu.Unlock()

	if len(snapshot) == 0 {
		return nil
	}
	root := &workflow.StageNode{Label: snapshot[0]}
	cur := root
	for _, seg := range snapshot[1:] {
		child := &workflow.StageNode{Label: seg}
		cur.Children = append(cur.Children, child)
		cur = child
	}
	return &workflow.PathSnapshot{Root: root}
}

// reportProgress sends a workflow.ProgressMsg reflecting the current
// activePaths tree and the role about to run (or "" if none — e.g. a
// user_checkpoint pause, or a parallel_group announcing its own start).
// No-op if the caller supplied no Send func.
func (ec *execContext) reportProgress(role string) {
	if ec.cfg.Send == nil {
		return
	}
	task, _ := ec.vars["task"].(string)
	ec.cfg.Send(workflow.ProgressMsg{
		WorkflowName: ec.defName,
		Task:         task,
		WorkflowID:   ec.cfg.WorkflowID,
		ActivePaths:  ec.buildPathTree(),
		Role:         role,
	})
}

// nextReplay returns the next unconsumed trace entry, if any.
func (ec *execContext) nextReplay() (TraceEntry, bool) {
	if ec.replayIdx >= len(ec.replay) {
		return TraceEntry{}, false
	}
	e := ec.replay[ec.replayIdx]
	ec.replayIdx++
	return e, true
}

// tryReplay checks whether the next unconsumed trace entry belongs to s. If
// so, it restores the value it recorded (SaveAs) and returns (outcome, true,
// nil) without any live work. ok is false when there is nothing left to
// replay — the caller should proceed with live execution. A non-nil err
// means the next entry exists but names a different stage: the Definition
// was very likely edited since this checkpoint was written, so resuming
// positionally is no longer trustworthy.
func tryReplay(ec *execContext, s workflow.Stage) (outcome string, ok bool, err error) {
	entry, has := ec.nextReplay()
	if !has {
		return "", false, nil
	}
	if entry.StageID != s.ID {
		return "", true, fmt.Errorf(
			"workflow: checkpoint mismatch: expected stage %q to complete next, found %q — was the workflow definition edited since this checkpoint was saved?",
			entry.StageID, s.ID)
	}
	if entry.SaveAs != "" {
		ec.vars[entry.SaveAs] = entry.Value
	}
	return entry.Outcome, true, nil
}

// recordLeaf reports a newly (live-)completed leaf stage to the checkpoint,
// if one is configured. No-op when checkpointing is disabled.
func (ec *execContext) recordLeaf(e TraceEntry) {
	if ec.onLeafComplete != nil {
		ec.onLeafComplete(e)
	}
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
	if outcome, ok, err := tryReplay(ec, s); ok {
		return outcome, err
	}

	if s.RunUnlessContains != "" {
		gate, _ := ec.vars[s.RunUnlessMarkerIn].(string)
		if markerLineRE(s.RunUnlessContains).FindStringIndex(gate) != nil {
			ec.recordLeaf(TraceEntry{StageID: s.ID})
			return "", nil
		}
	}

	runner, ok := ec.cfg.Runners[s.Role]
	if !ok {
		return "", fmt.Errorf("workflow: no runner for role %q (stage %q)", s.Role, s.ID)
	}
	ec.reportProgress(s.Role)

	prompt, err := renderTemplate(s.ID+".prompt", s.Prompt, ec.vars)
	if err != nil {
		return "", fmt.Errorf("workflow: stage %q: %w", s.ID, err)
	}
	out, err := workflow.Turn(ec.ctx, runner, prompt, ec.cfg.Send)
	if err != nil {
		return "", fmt.Errorf("workflow: stage %q: %w", s.ID, err)
	}
	if s.EmptyOutputFallback == "git_diff" && strings.TrimSpace(out) == "" {
		out = gitDiffSummary()
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
		ec.recordLeaf(TraceEntry{StageID: s.ID, SaveAs: s.SaveAs, Value: out})
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
	ec.recordLeaf(TraceEntry{StageID: s.ID, SaveAs: s.SaveAs, Value: out, Outcome: rule.Action})
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
	if _, ok, err := tryReplay(ec, s); ok {
		return err
	}

	if ec.cfg.Send == nil || ec.cfg.AnswersCh == nil {
		return fmt.Errorf("workflow: stage %q (user_checkpoint): no interactive channel available", s.ID)
	}
	ec.reportProgress("") // WorkflowQuestionsMsg's handler sets its own "waiting" role text
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
		ec.recordLeaf(TraceEntry{StageID: s.ID, SaveAs: s.SaveAs, Value: answer})
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
		ec.pushPath(fmt.Sprintf("%s[%d]", s.ID, sec.Index))
		_, err := executeStages(ec, s.Body)
		ec.popPath()
		if err != nil {
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
	}
	if override, ok := ec.maxIterOverrides[s.ID]; ok && override > 0 {
		maxIter = override
	}
	if maxIter <= 0 {
		maxIter = 1
	}
	if s.MaxIterationsFrom != "" {
		ec.vars[strings.ToLower(s.MaxIterationsFrom)] = maxIter
	}
	for iter := 1; iter <= maxIter; iter++ {
		if s.IterationVar != "" {
			ec.vars[s.IterationVar] = iter
		}
		ec.pushPath(fmt.Sprintf("%s[%d]", s.ID, iter))
		outcome, err := executeStages(ec, s.Body)
		ec.popPath()
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
	if _, ok, err := tryReplay(ec, s); ok {
		return err
	}

	doc, _ := ec.vars[s.From].(string)
	decls := ParseDeclarations(doc)
	sections := decls.SectionsFor(s.Over)
	if len(sections) == 0 {
		ec.recordLeaf(TraceEntry{StageID: s.ID})
		return nil
	}
	waves, err := computeWaves(sections)
	if err != nil {
		return fmt.Errorf("workflow: stage %q: %w", s.ID, err)
	}
	ec.pushPath(fmt.Sprintf("%s (%d items)", s.ID, len(sections)))
	ec.reportProgress("")
	defer ec.popPath()
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

				itemEC := &execContext{
					ctx: ec.ctx, cfg: ec.cfg, vars: itemVars, defName: ec.defName,
					activePaths: append([]string{}, ec.activePaths...), activeMu: ec.activeMu,
				}
				itemEC.pushPath(fmt.Sprintf("%s %s[%d]", s.ID, label, sec.Index))
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
	ec.recordLeaf(TraceEntry{StageID: s.ID, SaveAs: s.SaveAs, Value: results})
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
