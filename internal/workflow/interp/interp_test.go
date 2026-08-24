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
