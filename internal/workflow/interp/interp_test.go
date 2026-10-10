package interp

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/workflow"
)

// fakeRunner returns scripted responses in order, one per call, and records
// every prompt it was given.
type fakeRunner struct {
	name      string
	responses []string
	errs      []error // parallel to responses; nil entries mean "no error"

	mu      sync.Mutex
	prompts []string
	calls   int
}

func (f *fakeRunner) Name() string { return f.name }

func (f *fakeRunner) Run(_ context.Context, prompt string, _ io.Writer) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, prompt)
	i := f.calls
	f.calls++
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	if i < len(f.responses) {
		return f.responses[i], err
	}
	return f.responses[len(f.responses)-1], err
}

func (f *fakeRunner) lastPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) == 0 {
		return ""
	}
	return f.prompts[len(f.prompts)-1]
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func runCfg(runners map[string]workflow.TurnRunner) workflow.RunConfig {
	return workflow.RunConfig{Runners: runners}
}

func TestExecAgentTurn_RendersPromptAndSavesOutput(t *testing.T) {
	worker := &fakeRunner{name: "w", responses: []string{"worker output"}}
	next := &fakeRunner{name: "n", responses: []string{"ok"}}
	def := workflow.Definition{
		Name: "t",
		Stages: []workflow.Stage{
			{ID: "first", Kind: workflow.StageKindAgentTurn, Role: "worker", Prompt: "task is {{.task}}", SaveAs: "first_out"},
			{ID: "second", Kind: workflow.StageKindAgentTurn, Role: "next", Prompt: "prior said: {{.first_out}}"},
		},
	}
	r := New(def, "do the thing")
	cfg := runCfg(map[string]workflow.TurnRunner{"worker": worker, "next": next})
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := worker.lastPrompt(); got != "task is do the thing" {
		t.Errorf("worker prompt = %q", got)
	}
	if got := next.lastPrompt(); got != "prior said: worker output" {
		t.Errorf("next prompt = %q, expected it to include first stage's saved output", got)
	}
}

func TestExecAgentTurn_MissingRunner(t *testing.T) {
	def := workflow.Definition{Name: "t", Stages: []workflow.Stage{
		{ID: "a", Kind: workflow.StageKindAgentTurn, Role: "ghost", Prompt: "hi"},
	}}
	r := New(def, "task")
	err := r.Run(context.Background(), runCfg(nil))
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("expected error naming missing role, got %v", err)
	}
}

func TestExecAgentTurn_RunUnlessMarkerSkipsStage(t *testing.T) {
	gated := &fakeRunner{name: "g", responses: []string{"NO_ISSUES", "should not be called"}}
	def := workflow.Definition{Name: "t", Stages: []workflow.Stage{
		{ID: "check", Kind: workflow.StageKindAgentTurn, Role: "g", Prompt: "check", SaveAs: "check_out"},
		{
			ID: "maybe", Kind: workflow.StageKindAgentTurn, Role: "g", Prompt: "fix it",
			RunUnlessMarkerIn: "check_out", RunUnlessContains: "NO_ISSUES",
		},
	}}
	r := New(def, "task")
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"g": gated})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gated.callCount() != 1 {
		t.Errorf("expected the gated stage to be skipped, got %d calls", gated.callCount())
	}
}

func TestExecAgentTurn_RunUnlessMarkerRunsWhenAbsent(t *testing.T) {
	gated := &fakeRunner{name: "g", responses: []string{"some issues found", "fixed"}}
	def := workflow.Definition{Name: "t", Stages: []workflow.Stage{
		{ID: "check", Kind: workflow.StageKindAgentTurn, Role: "g", Prompt: "check", SaveAs: "check_out"},
		{
			ID: "maybe", Kind: workflow.StageKindAgentTurn, Role: "g", Prompt: "fix it",
			RunUnlessMarkerIn: "check_out", RunUnlessContains: "NO_ISSUES",
		},
	}}
	r := New(def, "task")
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"g": gated})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gated.callCount() != 2 {
		t.Errorf("expected the gated stage to run, got %d calls", gated.callCount())
	}
}

func TestExecAgentTurn_EmptyOutputFallbackGitDiff(t *testing.T) {
	worker := &fakeRunner{name: "w", responses: []string{""}}
	checker := &fakeRunner{name: "c", responses: []string{"ok"}}
	def := workflow.Definition{Name: "t", Stages: []workflow.Stage{
		{ID: "a", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "go", SaveAs: "out", EmptyOutputFallback: "git_diff"},
		{ID: "b", Kind: workflow.StageKindAgentTurn, Role: "c", Prompt: "{{.out}}"},
	}}
	r := New(def, "task")
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"w": worker, "c": checker})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := checker.lastPrompt(); got == "" {
		t.Error("expected empty_output_fallback to substitute a non-empty git-diff summary")
	}
}

func TestExecAgentTurn_EmptyOutputFallbackNotTriggeredWhenOutputPresent(t *testing.T) {
	worker := &fakeRunner{name: "w", responses: []string{"real output"}}
	checker := &fakeRunner{name: "c", responses: []string{"ok"}}
	def := workflow.Definition{Name: "t", Stages: []workflow.Stage{
		{ID: "a", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "go", SaveAs: "out", EmptyOutputFallback: "git_diff"},
		{ID: "b", Kind: workflow.StageKindAgentTurn, Role: "c", Prompt: "{{.out}}"},
	}}
	r := New(def, "task")
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"w": worker, "c": checker})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := checker.lastPrompt(); got != "real output" {
		t.Errorf("prompt = %q, want the original output preserved (%q)", got, "real output")
	}
}

func TestExecLoop_BoundedRetryThenBreak(t *testing.T) {
	evaluator := &fakeRunner{name: "eval", responses: []string{
		"findings\nneeds_refinement",
		"findings\ngood_to_go",
	}}
	def := workflow.Definition{
		Name: "t",
		Stages: []workflow.Stage{
			{
				ID: "loop", Kind: workflow.StageKindLoop, MaxIterations: 5,
				Body: []workflow.Stage{
					{
						ID: "eval", Kind: workflow.StageKindAgentTurn, Role: "eval", Prompt: "go (pass {{.pass}})",
						Verdict: map[string]workflow.VerdictRule{
							"good_to_go":       {Action: "break"},
							"needs_refinement": {Action: "retry"},
						},
					},
				},
			},
		},
	}
	r := New(def, "task")
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"eval": evaluator})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if evaluator.callCount() != 2 {
		t.Errorf("expected 2 calls (retry then break), got %d", evaluator.callCount())
	}
}

func TestExecLoop_ExhaustedReturnsError(t *testing.T) {
	evaluator := &fakeRunner{name: "eval", responses: []string{"needs_refinement"}}
	def := workflow.Definition{
		Name: "t",
		Stages: []workflow.Stage{
			{
				ID: "loop", Kind: workflow.StageKindLoop, MaxIterations: 3,
				Body: []workflow.Stage{
					{
						ID: "eval", Kind: workflow.StageKindAgentTurn, Role: "eval", Prompt: "go",
						Verdict: map[string]workflow.VerdictRule{"needs_refinement": {Action: "retry"}},
					},
				},
			},
		},
	}
	r := New(def, "task")
	err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"eval": evaluator}))
	var exhausted *ExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected *ExhaustedError, got %v", err)
	}
	if exhausted.MaxIterations != 3 || evaluator.callCount() != 3 {
		t.Errorf("exhausted = %+v, callCount = %d", exhausted, evaluator.callCount())
	}
}

func TestExecLoop_OverIteratesDeclaredSections(t *testing.T) {
	worker := &fakeRunner{name: "w", responses: []string{"ok"}}
	def := workflow.Definition{
		Name: "t",
		Stages: []workflow.Stage{
			{ID: "plan_stage", Kind: workflow.StageKindAgentTurn, Role: "planner",
				Prompt: "plan", SaveAs: "plan"},
			{
				ID: "sprint_loop", Kind: workflow.StageKindLoop, Over: "Sprint", From: "plan",
				Body: []workflow.Stage{
					{ID: "work", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "sprint {{.sprint}}: {{.sprint_section}}"},
				},
			},
		},
	}
	planner := &fakeRunner{name: "planner", responses: []string{"## Sprint 1\nfirst\n\n## Sprint 2\nsecond\n"}}
	r := New(def, "task")
	cfg := runCfg(map[string]workflow.TurnRunner{"planner": planner, "w": worker})
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if worker.callCount() != 2 {
		t.Fatalf("expected 2 sprint iterations, got %d", worker.callCount())
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if !strings.Contains(worker.prompts[0], "sprint 1") || !strings.Contains(worker.prompts[0], "first") {
		t.Errorf("first call prompt = %q", worker.prompts[0])
	}
	if !strings.Contains(worker.prompts[1], "sprint 2") || !strings.Contains(worker.prompts[1], "second") {
		t.Errorf("second call prompt = %q", worker.prompts[1])
	}
}

func TestExecLoop_OverFallsBackToOneIterationWhenNoSectionsDeclared(t *testing.T) {
	worker := &fakeRunner{name: "w", responses: []string{"ok"}}
	def := workflow.Definition{
		Name: "t",
		Stages: []workflow.Stage{
			{ID: "plan_stage", Kind: workflow.StageKindAgentTurn, Role: "planner", Prompt: "plan", SaveAs: "plan"},
			{
				ID: "sprint_loop", Kind: workflow.StageKindLoop, Over: "Sprint", From: "plan",
				Body: []workflow.Stage{{ID: "work", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "go"}},
			},
		},
	}
	planner := &fakeRunner{name: "planner", responses: []string{"no headings here"}}
	r := New(def, "task")
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"planner": planner, "w": worker})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if worker.callCount() != 1 {
		t.Errorf("expected exactly 1 fallback iteration, got %d", worker.callCount())
	}
}

func TestExecAgentTurn_SkipUserCheckpointMarkerFound(t *testing.T) {
	designer := &fakeRunner{name: "d", responses: []string{"NO_QUESTIONS\nthe actual plan"}}
	def := workflow.Definition{Name: "t", Stages: []workflow.Stage{
		{ID: "designer", Kind: workflow.StageKindAgentTurn, Role: "d", Prompt: "design",
			SaveAs: "plan", SkipUserCheckpointMarker: "NO_QUESTIONS"},
		{ID: "use", Kind: workflow.StageKindAgentTurn, Role: "d", Prompt: "{{.plan}}"},
	}}
	r := New(def, "task")
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"d": designer})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := designer.lastPrompt(); strings.TrimSpace(got) != "the actual plan" {
		t.Errorf("second stage saw %q, want %q", got, "the actual plan")
	}
}

func TestExecAgentTurn_SkipUserCheckpointMarkerAbsent_PausesAndFinalizes(t *testing.T) {
	designer := &fakeRunner{name: "d", responses: []string{"## Questions\n- Q1: what?", "finalized plan"}}
	answers := make(chan string, 1)
	def := workflow.Definition{Name: "t", Stages: []workflow.Stage{
		{
			ID: "designer", Kind: workflow.StageKindAgentTurn, Role: "d",
			Prompt: "design", SaveAs: "plan", SkipUserCheckpointMarker: "NO_QUESTIONS",
			OnAnswerPrompt: "questions were: {{.designer_questions}} answer: {{.designer_answer}}",
		},
	}}
	r := New(def, "task")
	var got workflow.WorkflowQuestionsMsg
	cfg := workflow.RunConfig{
		Runners:   map[string]workflow.TurnRunner{"d": designer},
		AnswersCh: answers,
		Send: func(m tea.Msg) {
			if qm, ok := m.(workflow.WorkflowQuestionsMsg); ok {
				got = qm
			}
		},
	}
	answers <- "42"
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Questions == "" {
		t.Error("expected WorkflowQuestionsMsg to be sent")
	}
	if designer.callCount() != 2 {
		t.Fatalf("expected 2 calls (questions + finalize), got %d", designer.callCount())
	}
	final := designer.lastPrompt()
	if !strings.Contains(final, "what?") || !strings.Contains(final, "42") {
		t.Errorf("finalize prompt = %q, expected the original question and the answer", final)
	}
}

func TestExecAgentTurn_SkipUserCheckpointMarkerAbsent_NoChannelErrors(t *testing.T) {
	designer := &fakeRunner{name: "d", responses: []string{"## Questions\n- Q1: what?"}}
	def := workflow.Definition{Name: "t", Stages: []workflow.Stage{
		{ID: "designer", Kind: workflow.StageKindAgentTurn, Role: "d", Prompt: "design",
			SkipUserCheckpointMarker: "NO_QUESTIONS"},
	}}
	r := New(def, "task")
	err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"d": designer}))
	if err == nil {
		t.Fatal("expected an error when no interactive channel is available to answer questions")
	}
}

func TestExecUserCheckpoint_WaitsForAnswer(t *testing.T) {
	answers := make(chan string, 1)
	worker := &fakeRunner{name: "w", responses: []string{"ok"}}
	def := workflow.Definition{Name: "t", Stages: []workflow.Stage{
		{ID: "ask", Kind: workflow.StageKindUserCheckpoint, Prompt: "please respond", SaveAs: "reply"},
		{ID: "use", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "got: {{.reply}}"},
	}}
	r := New(def, "task")
	cfg := workflow.RunConfig{
		Runners:   map[string]workflow.TurnRunner{"w": worker},
		AnswersCh: answers,
		Send:      func(tea.Msg) {},
	}
	answers <- "the reply"
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := worker.lastPrompt(); got != "got: the reply" {
		t.Errorf("prompt = %q", got)
	}
}

func TestExecUserCheckpoint_CancelledContext(t *testing.T) {
	answers := make(chan string)
	def := workflow.Definition{Name: "t", Stages: []workflow.Stage{
		{ID: "ask", Kind: workflow.StageKindUserCheckpoint, Prompt: "please respond"},
	}}
	r := New(def, "task")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := workflow.RunConfig{AnswersCh: answers, Send: func(tea.Msg) {}}
	if err := r.Run(ctx, cfg); !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestExecParallelGroup_FanOutAndAggregate(t *testing.T) {
	worker := &fakeRunner{name: "w", responses: []string{"item output"}}
	def := workflow.Definition{
		Name: "t",
		Stages: []workflow.Stage{
			{ID: "plan_stage", Kind: workflow.StageKindAgentTurn, Role: "planner", Prompt: "plan", SaveAs: "plan"},
			{
				ID: "fanout", Kind: workflow.StageKindParallelGroup, Over: "Item", From: "plan",
				MaxConcurrency: 2, SaveAs: "results",
				Body: []workflow.Stage{
					{ID: "work", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "do item {{.item}}", SaveAs: "work_out"},
				},
			},
		},
	}
	planner := &fakeRunner{name: "planner", responses: []string{"## Item 1\na\n\n## Item 2\nb\n\n## Item 3\nc\n"}}
	r := New(def, "task")
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"planner": planner, "w": worker})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if worker.callCount() != 3 {
		t.Errorf("expected 3 concurrent invocations, got %d", worker.callCount())
	}
}

// TestExecParallelGroup_ItemResultOutputAndStatusPopulated verifies each
// fanned-out item's ItemResult carries real Status/Output, not the zero
// value, when its body is a single flat agent_turn stage (the simple case
// TestExecParallelGroup_FanOutAndAggregate exercises but never actually
// inspects). A later stage ranging over the SaveAs'd []ItemResult (exactly
// swarm.yaml's final_evaluation does over worker_results) is the only way to
// observe this from outside the package, so this renders that stage's
// prompt and checks it.
func TestExecParallelGroup_ItemResultOutputAndStatusPopulated(t *testing.T) {
	worker := &fakeRunner{name: "w", responses: []string{"item output"}}
	summary := &fakeRunner{name: "s", responses: []string{"noted"}}
	def := workflow.Definition{
		Name: "t",
		Stages: []workflow.Stage{
			{ID: "plan_stage", Kind: workflow.StageKindAgentTurn, Role: "planner", Prompt: "plan", SaveAs: "plan"},
			{
				ID: "fanout", Kind: workflow.StageKindParallelGroup, Over: "Item", From: "plan",
				SaveAs: "results",
				Body: []workflow.Stage{
					{
						ID: "work", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "do item {{.item}}",
						SaveAs: "work_out", Verdict: map[string]workflow.VerdictRule{"good_to_go": {Action: "break"}},
					},
				},
			},
			{
				ID: "summarize", Kind: workflow.StageKindAgentTurn, Role: "s",
				Prompt: "{{range .results}}[{{.Index}} {{.Status}}] {{.Output}}\n{{end}}",
			},
		},
	}
	planner := &fakeRunner{name: "planner", responses: []string{"## Item 1\na\n"}}
	worker.responses = []string{"item output\ngood_to_go"}
	r := New(def, "task")
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"planner": planner, "w": worker, "s": summary})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := summary.lastPrompt()
	if !strings.Contains(got, "[1 break]") {
		t.Errorf("summarize prompt = %q, want Status %q populated as %q", got, "Status", "[1 break]")
	}
	if !strings.Contains(got, "item output") {
		t.Errorf("summarize prompt = %q, want Output populated with the worker's saved text", got)
	}
}

// TestExecParallelGroup_ItemResultPopulatedThroughNestedLoop is the
// regression test for a real bug: when an item's body is a retry loop
// (exactly swarm.yaml's worker_pass_loop shape — an agent_turn, then a
// second verdict-bearing agent_turn) rather than a single flat agent_turn,
// ItemResult.Status and .Output silently stayed "" — a loop-kind stage
// always reported an empty outcome regardless of what happened inside it
// (executeStage's dispatch discarded it), and lastSaveAsOf never looked
// inside a nested loop body for the last SaveAs. In production this meant
// swarm's final_evaluation stage reviewed cross-item integration blind to
// every item's actual findings, for every run.
func TestExecParallelGroup_ItemResultPopulatedThroughNestedLoop(t *testing.T) {
	worker := &fakeRunner{name: "w", responses: []string{"did the work"}}
	evaluator := &fakeRunner{name: "e", responses: []string{"findings: looks good\ngood_to_go"}}
	summary := &fakeRunner{name: "s", responses: []string{"noted"}}
	def := workflow.Definition{
		Name: "t",
		Stages: []workflow.Stage{
			{ID: "plan_stage", Kind: workflow.StageKindAgentTurn, Role: "planner", Prompt: "plan", SaveAs: "plan"},
			{
				ID: "fanout", Kind: workflow.StageKindParallelGroup, Over: "Item", From: "plan",
				SaveAs: "results",
				Body: []workflow.Stage{
					{
						ID: "pass_loop", Kind: workflow.StageKindLoop, MaxIterations: 3, IterationVar: "pass",
						Body: []workflow.Stage{
							{ID: "work", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "implement", SaveAs: "work_out"},
							{
								ID: "eval", Kind: workflow.StageKindAgentTurn, Role: "e", Prompt: "review",
								SaveAs: "eval_findings",
								Verdict: map[string]workflow.VerdictRule{
									"good_to_go":       {Action: "break"},
									"needs_refinement": {Action: "retry"},
								},
							},
						},
					},
				},
			},
			{
				ID: "summarize", Kind: workflow.StageKindAgentTurn, Role: "s",
				Prompt: "{{range .results}}[{{.Index}} {{.Status}}] {{.Output}}\n{{end}}",
			},
		},
	}
	planner := &fakeRunner{name: "planner", responses: []string{"## Item 1\na\n"}}
	r := New(def, "task")
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{
		"planner": planner, "w": worker, "e": evaluator, "s": summary,
	})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := summary.lastPrompt()
	if !strings.Contains(got, "[1 break]") {
		t.Errorf("summarize prompt = %q, want Status %q populated as %q even though the item's body is a nested loop", got, "Status", "[1 break]")
	}
	if !strings.Contains(got, "looks good") {
		t.Errorf("summarize prompt = %q, want Output populated with the evaluator's findings text (eval_findings, the last SaveAs inside the nested loop)", got)
	}
}

func TestExecParallelGroup_MaxConcurrencyBound(t *testing.T) {
	var mu sync.Mutex
	current, peak := 0, 0
	slow := &blockingRunner{
		before: func() {
			mu.Lock()
			current++
			if current > peak {
				peak = current
			}
			mu.Unlock()
		},
		after: func() {
			mu.Lock()
			current--
			mu.Unlock()
		},
		delay: 20 * time.Millisecond,
	}
	def := workflow.Definition{
		Name: "t",
		Stages: []workflow.Stage{
			{ID: "plan_stage", Kind: workflow.StageKindAgentTurn, Role: "planner", Prompt: "plan", SaveAs: "plan"},
			{
				ID: "fanout", Kind: workflow.StageKindParallelGroup, Over: "Item", From: "plan", MaxConcurrency: 2,
				Body: []workflow.Stage{{ID: "work", Kind: workflow.StageKindAgentTurn, Role: "slow", Prompt: "go"}},
			},
		},
	}
	planner := &fakeRunner{name: "planner", responses: []string{
		"## Item 1\na\n\n## Item 2\nb\n\n## Item 3\nc\n\n## Item 4\nd\n",
	}}
	r := New(def, "task")
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"planner": planner, "slow": slow})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > 2 {
		t.Errorf("peak concurrency = %d, want <= 2 (MaxConcurrency)", peak)
	}
	if peak < 2 {
		t.Errorf("peak concurrency = %d, want == 2 (should actually run in parallel)", peak)
	}
}

func TestComputeWaves_LinearChain(t *testing.T) {
	sections := []Section{
		{Index: 1},
		{Index: 2, DependsOn: []int{1}},
		{Index: 3, DependsOn: []int{2}},
	}
	waves, err := computeWaves(sections)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}
	if len(waves) != 3 {
		t.Fatalf("got %d waves, want 3 (strict chain)", len(waves))
	}
	for i, wave := range waves {
		if len(wave) != 1 || wave[0].Index != i+1 {
			t.Errorf("wave %d = %v, want [{Index: %d}]", i, wave, i+1)
		}
	}
}

func TestComputeWaves_IndependentItemsShareAWave(t *testing.T) {
	sections := []Section{
		{Index: 1},
		{Index: 2},
		{Index: 3, DependsOn: []int{1, 2}},
	}
	waves, err := computeWaves(sections)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}
	if len(waves) != 2 {
		t.Fatalf("got %d waves, want 2", len(waves))
	}
	if len(waves[0]) != 2 {
		t.Errorf("wave 0 = %v, want both independent items", waves[0])
	}
	if len(waves[1]) != 1 || waves[1][0].Index != 3 {
		t.Errorf("wave 1 = %v, want just item 3", waves[1])
	}
}

func TestComputeWaves_CycleReturnsError(t *testing.T) {
	sections := []Section{
		{Index: 1, DependsOn: []int{2}},
		{Index: 2, DependsOn: []int{1}},
	}
	if _, err := computeWaves(sections); err == nil {
		t.Error("expected an error for a dependency cycle")
	}
}

func TestExecParallelGroup_DependentItemWaitsForItsDependency(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(evt string) {
		mu.Lock()
		events = append(events, evt)
		mu.Unlock()
	}
	worker := &recordingRunner{record: record, delay: 10 * time.Millisecond}
	def := workflow.Definition{
		Name: "t",
		Stages: []workflow.Stage{
			{ID: "plan_stage", Kind: workflow.StageKindAgentTurn, Role: "planner", Prompt: "plan", SaveAs: "plan"},
			{
				ID: "fanout", Kind: workflow.StageKindParallelGroup, Over: "Item", From: "plan", MaxConcurrency: 4,
				Body: []workflow.Stage{{ID: "work", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "do {{.item}}"}},
			},
		},
	}
	planner := &fakeRunner{name: "planner", responses: []string{
		"## Item 1\nfirst\n\n## Item 2 (depends_on: 1)\nsecond, needs first\n",
	}}
	r := New(def, "task")
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"planner": planner, "w": worker})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 4 {
		t.Fatalf("events = %v, want 4 start/end markers", events)
	}
	// Item 1 must fully finish (start AND end) before item 2 starts.
	item1EndIdx, item2StartIdx := -1, -1
	for i, e := range events {
		if e == "end:1" {
			item1EndIdx = i
		}
		if e == "start:2" {
			item2StartIdx = i
		}
	}
	if item1EndIdx == -1 || item2StartIdx == -1 || item2StartIdx < item1EndIdx {
		t.Errorf("events = %v, want item 1 to fully finish before item 2 starts", events)
	}
}

// recordingRunner records "start:<prompt-derived id>" / "end:<id>" via
// record, sleeping delay in between so tests can observe overlap (or its
// absence, for a dependency-respecting wave).
type recordingRunner struct {
	record func(string)
	delay  time.Duration
}

func (r *recordingRunner) Name() string { return "w" }
func (r *recordingRunner) Run(_ context.Context, prompt string, _ io.Writer) (string, error) {
	id := prompt[len(prompt)-1:] // prompts end in the item index digit
	r.record("start:" + id)
	time.Sleep(r.delay)
	r.record("end:" + id)
	return "ok", nil
}

func TestExecParallelGroup_OneItemErrorDoesNotAbortSiblings(t *testing.T) {
	failing := &fakeRunner{name: "f", responses: []string{"never used"}, errs: []error{errors.New("boom")}}
	def := workflow.Definition{
		Name: "t",
		Stages: []workflow.Stage{
			{ID: "plan_stage", Kind: workflow.StageKindAgentTurn, Role: "planner", Prompt: "plan", SaveAs: "plan"},
			{
				ID: "fanout", Kind: workflow.StageKindParallelGroup, Over: "Item", From: "plan", SaveAs: "results",
				Body: []workflow.Stage{{ID: "work", Kind: workflow.StageKindAgentTurn, Role: "f", Prompt: "go"}},
			},
		},
	}
	planner := &fakeRunner{name: "planner", responses: []string{"## Item 1\na\n\n## Item 2\nb\n"}}
	r := New(def, "task")
	// Run should not return an error even though every item's worker fails --
	// per-item failures are isolated, not fatal to the group.
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"planner": planner, "f": failing})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if failing.callCount() != 2 {
		t.Errorf("expected both items to run despite errors, got %d calls", failing.callCount())
	}
}

// TestExecAgentTurn_BudgetExhaustedIsDistinctFromLoopTermination verifies
// that a stage output carrying the iteration-budget-exhausted marker (what
// internal/agent/local produces when runToolLoop exhausts maxIter without a
// final response) advances the workflow via the new distinct
// isBudgetExhaustedTurn path -- not the loop-detector "terminated by loop
// detection" message isTerminatedTurn's callers report.
func TestExecAgentTurn_BudgetExhaustedIsDistinctFromLoopTermination(t *testing.T) {
	budgetExhaustedOutput := "[budget exhausted: exceeded the maximum tool-call iteration limit without a final response — this may reflect a task that needed more iterations, not necessarily a stuck loop]\n\n" +
		"[turn ended without a final summary — tool activity this turn:]\n- read_file(a.go)\n"
	worker := &fakeRunner{name: "w", responses: []string{budgetExhaustedOutput}}
	def := workflow.Definition{
		Name: "t",
		Stages: []workflow.Stage{
			{ID: "work", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "go", SaveAs: "out"},
		},
	}
	r := New(def, "task")

	var sentTexts []string
	var mu sync.Mutex
	cfg := workflow.RunConfig{
		Runners: map[string]workflow.TurnRunner{"w": worker},
		Send: func(msg tea.Msg) {
			if m, ok := msg.(workflow.WorkflowChunkMsg); ok {
				mu.Lock()
				sentTexts = append(sentTexts, m.Text)
				mu.Unlock()
			}
		},
	}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(sentTexts, "\n")
	if !strings.Contains(joined, "ran out of tool-call iteration budget") {
		t.Errorf("expected the budget-exhausted message, got sent texts: %v", sentTexts)
	}
	if strings.Contains(joined, "terminated by loop detection") {
		t.Errorf("expected the loop-detection message NOT to fire for a budget-exhausted turn, got sent texts: %v", sentTexts)
	}
}

// TestIsBudgetExhaustedTurn verifies the marker-detection helper directly.
func TestIsBudgetExhaustedTurn(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{"empty", "", false},
		{"normal verdict", "good_to_go", false},
		{"loop terminated, no budget marker", "[turn terminated: stuck in a reasoning loop]", false},
		{"budget exhausted", "[budget exhausted: exceeded the maximum tool-call iteration limit without a final response]", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBudgetExhaustedTurn(tt.out); got != tt.want {
				t.Errorf("isBudgetExhaustedTurn(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

// memoryAwareFakeRunner additionally records SetUseMemoryForNextCall calls,
// satisfying workflow.MemoryAwareTurnRunner like cmd/milk's workflowTurnRunner.
type memoryAwareFakeRunner struct {
	*fakeRunner
	useMemoryCalls []bool
}

func (m *memoryAwareFakeRunner) SetUseMemoryForNextCall(v bool) {
	m.useMemoryCalls = append(m.useMemoryCalls, v)
}

// TestExecAgentTurn_SetsUseMemoryFromStage verifies that Stage.UseMemory is
// threaded down to a MemoryAwareTurnRunner via SetUseMemoryForNextCall
// before each call, and that a plain (non-memory-aware) runner is left
// alone — the type assertion must not panic on a runner that doesn't
// implement the optional interface.
func TestExecAgentTurn_SetsUseMemoryFromStage(t *testing.T) {
	inner := &memoryAwareFakeRunner{fakeRunner: &fakeRunner{name: "w", responses: []string{"ok", "ok"}}}
	def := workflow.Definition{
		Name: "t",
		Stages: []workflow.Stage{
			{ID: "no_memory", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "go"},
			{ID: "with_memory", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "go again", UseMemory: true},
		},
	}
	r := New(def, "task")
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"w": inner})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(inner.useMemoryCalls) != 2 {
		t.Fatalf("expected 2 SetUseMemoryForNextCall calls, got %d: %v", len(inner.useMemoryCalls), inner.useMemoryCalls)
	}
	if inner.useMemoryCalls[0] != false {
		t.Errorf("expected first stage (no UseMemory) to set false, got %v", inner.useMemoryCalls[0])
	}
	if inner.useMemoryCalls[1] != true {
		t.Errorf("expected second stage (UseMemory: true) to set true, got %v", inner.useMemoryCalls[1])
	}
}

// TestExecAgentTurn_PlainRunner_NoMemoryAwareInterface verifies a runner
// that doesn't implement MemoryAwareTurnRunner is simply skipped, not an error.
func TestExecAgentTurn_PlainRunner_NoMemoryAwareInterface(t *testing.T) {
	inner := &fakeRunner{name: "w", responses: []string{"ok"}}
	def := workflow.Definition{
		Name:   "t",
		Stages: []workflow.Stage{{ID: "s", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "go", UseMemory: true}},
	}
	r := New(def, "task")
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"w": inner})); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// blockingRunner sleeps for delay before returning, calling before/after
// around the sleep so tests can measure concurrency.
type blockingRunner struct {
	before, after func()
	delay         time.Duration
}

func (b *blockingRunner) Name() string { return "slow" }
func (b *blockingRunner) Run(_ context.Context, _ string, _ io.Writer) (string, error) {
	b.before()
	time.Sleep(b.delay)
	b.after()
	return "ok", nil
}

// A plan longer than sectionCharBudget must reach parallel_group intact. It was
// saved head+tail truncated, which dropped "## Item 1/2" from the middle and
// left only "## Item 3 (depends_on: 1, 2)" -- an unresolvable dependency.
func TestExecParallelGroup_LongPlanKeepsMiddleItems(t *testing.T) {
	filler := func(n int) string { return strings.Repeat("spec detail. ", n/13+1)[:n] + "\n\n" }
	plan := "## Spec\n" + filler(8500) +
		"## Item 1\na\n\n## Item 2\nb\n\n" + filler(2000) +
		"## Item 3 (depends_on: 1, 2)\nc\n\n" + filler(2500)
	if len(plan) <= sectionCharBudget {
		t.Fatalf("test plan must exceed the budget, got %d", len(plan))
	}
	worker := &fakeRunner{name: "w", responses: []string{"item output"}}
	def := workflow.Definition{
		Name: "t",
		Stages: []workflow.Stage{
			{ID: "plan_stage", Kind: workflow.StageKindAgentTurn, Role: "planner", Prompt: "plan", SaveAs: "plan"},
			{
				ID: "fanout", Kind: workflow.StageKindParallelGroup, Over: "Item", From: "plan",
				MaxConcurrency: 2, SaveAs: "results",
				Body: []workflow.Stage{
					{ID: "work", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "do item {{.item}}", SaveAs: "work_out"},
				},
			},
		},
	}
	planner := &fakeRunner{name: "planner", responses: []string{plan}}
	r := New(def, "task")
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"planner": planner, "w": worker})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if worker.callCount() != 3 {
		t.Errorf("want all 3 items run, got %d", worker.callCount())
	}
}
