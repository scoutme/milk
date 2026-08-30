package workflow

import "fmt"

// Definition is a workflow described as data (YAML/JSON) rather than a
// hand-written Go type. The interpreter (internal/workflow/interp) walks a
// Definition's Stages to execute a run.
type Definition struct {
	// Name is the identifier used by /workflow <name> and as the registry key.
	Name string `yaml:"name" json:"name"`
	// Roles lists the role names this definition expects RunConfig.Runners to
	// provide (e.g. "designer", "generator", "evaluator"). Informational —
	// resolution against actual agents happens via ResolveAgentNames before a
	// run starts; this is not itself consulted by the interpreter.
	Roles []string `yaml:"roles" json:"roles"`
	// Vars seeds the template context every stage renders its Prompt against,
	// letting a definition share fixed text (e.g. a common instructions
	// block) across multiple prompts without duplicating it. Stage outputs
	// (via SaveAs) and loop variables are layered on top at render time and
	// take precedence over a same-named entry here.
	Vars map[string]string `yaml:"vars,omitempty" json:"vars,omitempty"`
	// Stages are executed in order. See Stage.Kind for the supported shapes.
	Stages []Stage `yaml:"stages" json:"stages"`
}

// Stage is one node in a Definition's stage tree. Kind selects which fields
// apply:
//
//   - "agent_turn": Role, Prompt, SaveAs, and optionally SkipUserCheckpointMarker +
//     OnAnswerPrompt (pause for user input unless the turn's output contains
//     a standalone line matching SkipUserCheckpointMarker — e.g. dev's
//     "NO_QUESTIONS"), and Verdict (parse the turn's output as a workflow
//     verdict and report "break"/"retry" to an enclosing "loop" stage).
//   - "loop": either Over (iterate a declared collection parsed out of the
//     document named by From — e.g. dev's sprint plan) or MaxIterations
//     (bounded retry loop; the last Body stage must set Verdict). Body is run
//     once per iteration.
//   - "user_checkpoint": Prompt (rendered from Vars), pauses for a user
//     answer via RunConfig.AnswersCh, stores the answer under SaveAs.
//   - "parallel_group": like "loop" with Over, but Body runs concurrently
//     per item (bounded by MaxConcurrency) instead of sequentially; per-item
//     failures do not abort siblings. Results aggregate into SaveAs.
type Stage struct {
	ID   string `yaml:"id" json:"id"`
	Kind string `yaml:"kind" json:"kind"`

	// agent_turn / user_checkpoint
	Role   string `yaml:"role,omitempty" json:"role,omitempty"`
	Prompt string `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	SaveAs string `yaml:"save_as,omitempty" json:"save_as,omitempty"`
	// SkipUserCheckpointMarker: if set, the turn's output is scanned for a
	// standalone line equal to this marker (case-insensitive). Found → no
	// disambiguation needed, SaveAs gets everything after the marker line.
	// Not found → the whole output is treated as questions for the user:
	// paused via RunConfig.AnswersCh, then OnAnswerPrompt is rendered (with
	// the original output available as Vars["<id>_questions"] and the user's
	// reply as Vars["<id>_answer"]) and run as a second turn on the same
	// role; that turn's output becomes SaveAs.
	SkipUserCheckpointMarker string                 `yaml:"skip_user_checkpoint_marker,omitempty" json:"skip_user_checkpoint_marker,omitempty"`
	OnAnswerPrompt           string                 `yaml:"on_answer_prompt,omitempty" json:"on_answer_prompt,omitempty"`
	Verdict                  map[string]VerdictRule `yaml:"verdict,omitempty" json:"verdict,omitempty"`
	// EmptyOutputFallback substitutes a canned value for this turn's output
	// when it comes back empty/whitespace-only — e.g. a generator that
	// completed entirely via tool calls without writing a closing summary.
	// Only "git_diff" is currently supported (a `git diff --stat`/`git diff`
	// summary of uncommitted changes since HEAD).
	EmptyOutputFallback string `yaml:"empty_output_fallback,omitempty" json:"empty_output_fallback,omitempty"`
	// RunUnlessMarkerIn/RunUnlessContains: an agent_turn stage this pair is
	// set on is skipped entirely (no runner call, no SaveAs) when
	// Vars[RunUnlessMarkerIn] contains a standalone line equal to
	// RunUnlessContains. Used for an optional stage gated on a prior stage's
	// verdict-like output — e.g. swarm's final implementation pass, skipped
	// when the final evaluation reports no issues.
	RunUnlessMarkerIn string `yaml:"run_unless_marker_in,omitempty" json:"run_unless_marker_in,omitempty"`
	RunUnlessContains string `yaml:"run_unless_contains,omitempty" json:"run_unless_contains,omitempty"`

	// loop / parallel_group
	// Over names the declared section label (e.g. "Sprint", parsed out of
	// "## Sprint N" headings) to iterate; From names the Vars entry holding
	// the source document to parse it out of (e.g. "plan"). Mutually
	// exclusive with MaxIterations/MaxIterationsFrom on "loop" — a loop is
	// either bounded by a declared collection (Over) or by an iteration cap
	// (MaxIterations*), not both.
	Over string `yaml:"over,omitempty" json:"over,omitempty"`
	From string `yaml:"from,omitempty" json:"from,omitempty"`
	// MaxIterations is a static bound for a "loop" stage's retry count.
	// MaxIterationsFrom instead names a declared scalar (e.g. "max_passes",
	// parsed out of the From document's "key: N" lines) resolved at runtime;
	// when both are unset the loop falls back to MaxIterations's zero value,
	// which Validate rejects unless Over is set.
	MaxIterations     int    `yaml:"max_iterations,omitempty" json:"max_iterations,omitempty"`
	MaxIterationsFrom string `yaml:"max_iterations_from,omitempty" json:"max_iterations_from,omitempty"`
	// IterationVar names the Vars entry a MaxIterations(From)-bounded loop
	// exposes its current 1-based iteration count as (e.g. "pass"). Unused
	// by Over-bounded loops, which instead derive both an index and a
	// "<label>_section" body var from Over itself (e.g. "sprint"/"sprint_section").
	IterationVar   string  `yaml:"iteration_var,omitempty" json:"iteration_var,omitempty"`
	MaxConcurrency int     `yaml:"max_concurrency,omitempty" json:"max_concurrency,omitempty"`
	Body           []Stage `yaml:"body,omitempty" json:"body,omitempty"`
}

// VerdictRule maps one ParseVerdict outcome (its map key: "good_to_go",
// "sprint_done", "needs_refinement", or "unknown") to an action.
type VerdictRule struct {
	// Action is "break" (exit the enclosing loop successfully) or "retry"
	// (run the loop body again, subject to the loop's MaxIterations).
	Action string `yaml:"action" json:"action"`
	// Warn marks this outcome as unexpected — the interpreter surfaces a
	// warning to the transcript (mirrors dev.go's VerdictUnknown handling)
	// but still applies Action.
	Warn bool `yaml:"warn,omitempty" json:"warn,omitempty"`
}

const (
	StageKindAgentTurn      = "agent_turn"
	StageKindLoop           = "loop"
	StageKindUserCheckpoint = "user_checkpoint"
	StageKindParallelGroup  = "parallel_group"
)

// Validate reports structural problems that would otherwise surface as a
// confusing runtime error partway through a live run (e.g. an unknown Kind,
// or a bounded loop whose body never declares a Verdict to break/retry on).
func (d Definition) Validate() error {
	if d.Name == "" {
		return fmt.Errorf("workflow definition: name is required")
	}
	if len(d.Stages) == 0 {
		return fmt.Errorf("workflow definition %q: at least one stage is required", d.Name)
	}
	return validateStages(d.Name, d.Stages)
}

func validateStages(defName string, stages []Stage) error {
	for _, s := range stages {
		if err := s.validate(defName); err != nil {
			return err
		}
	}
	return nil
}

func (s Stage) validate(defName string) error {
	if s.ID == "" {
		return fmt.Errorf("workflow definition %q: stage with kind %q missing id", defName, s.Kind)
	}
	switch s.Kind {
	case StageKindAgentTurn:
		if s.Role == "" {
			return fmt.Errorf("workflow definition %q: stage %q (agent_turn) missing role", defName, s.ID)
		}
		if s.Prompt == "" {
			return fmt.Errorf("workflow definition %q: stage %q (agent_turn) missing prompt", defName, s.ID)
		}
		if (s.RunUnlessMarkerIn == "") != (s.RunUnlessContains == "") {
			return fmt.Errorf("workflow definition %q: stage %q (agent_turn): run_unless_marker_in and run_unless_contains must be set together", defName, s.ID)
		}
		if s.EmptyOutputFallback != "" && s.EmptyOutputFallback != "git_diff" {
			return fmt.Errorf("workflow definition %q: stage %q (agent_turn): unknown empty_output_fallback %q (only \"git_diff\" is supported)", defName, s.ID, s.EmptyOutputFallback)
		}
	case StageKindUserCheckpoint:
		if s.Prompt == "" {
			return fmt.Errorf("workflow definition %q: stage %q (user_checkpoint) missing prompt", defName, s.ID)
		}
	case StageKindLoop:
		if len(s.Body) == 0 {
			return fmt.Errorf("workflow definition %q: stage %q (loop) has no body", defName, s.ID)
		}
		bounded := s.MaxIterations > 0 || s.MaxIterationsFrom != ""
		if s.Over == "" && !bounded {
			return fmt.Errorf("workflow definition %q: stage %q (loop) needs either over or max_iterations/max_iterations_from", defName, s.ID)
		}
		if s.Over == "" && bounded {
			last := s.Body[len(s.Body)-1]
			if len(last.Verdict) == 0 {
				return fmt.Errorf("workflow definition %q: stage %q (loop, iteration-bounded) requires its last body stage to declare verdict rules", defName, s.ID)
			}
		}
		if err := validateStages(defName, s.Body); err != nil {
			return err
		}
	case StageKindParallelGroup:
		if len(s.Body) == 0 {
			return fmt.Errorf("workflow definition %q: stage %q (parallel_group) has no body", defName, s.ID)
		}
		if s.Over == "" {
			return fmt.Errorf("workflow definition %q: stage %q (parallel_group) missing over", defName, s.ID)
		}
		if err := validateStages(defName, s.Body); err != nil {
			return err
		}
	default:
		return fmt.Errorf("workflow definition %q: stage %q has unknown kind %q", defName, s.ID, s.Kind)
	}
	return nil
}
