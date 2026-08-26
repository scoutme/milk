package interp

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/workflow"
)

// loadDevDefinition loads the real embedded dev.yaml via the registry (not a
// hand-written test fixture), sandboxing $HOME so it doesn't pick up
// whatever happens to be in the machine's real ~/.milk/workflows.
func loadDevDefinition(t *testing.T) workflow.Definition {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	reg, errs := workflow.LoadRegistry()
	if len(errs) != 0 {
		t.Fatalf("LoadRegistry errors: %v", errs)
	}
	def, ok := reg.Lookup("dev")
	if !ok {
		t.Fatal("expected built-in \"dev\" definition to be registered")
	}
	return def
}

func TestDevYAML_EndToEnd_NoQuestionsSingleSprintGoodToGo(t *testing.T) {
	def := loadDevDefinition(t)

	designer := &fakeRunner{name: "designer", responses: []string{
		"NO_QUESTIONS\n## Spec\nBuild a thing.\n\n## Limits\nmax_passes: 2\nmax_sprints: 1\n\n" +
			"## Sprint Plan\n## Sprint 1\nBuild the thing end to end.\n",
	}}
	generator := &fakeRunner{name: "generator", responses: []string{"Implemented the thing in main.go."}}
	evaluator := &fakeRunner{name: "evaluator", responses: []string{"Looks good.\ngood_to_go"}}

	r := New(def, "build a thing")
	cfg := workflow.RunConfig{Runners: map[string]workflow.TurnRunner{
		"designer": designer, "generator": generator, "evaluator": evaluator,
	}}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if designer.callCount() != 1 {
		t.Errorf("designer calls = %d, want 1 (NO_QUESTIONS needs no finalize turn)", designer.callCount())
	}
	if generator.callCount() != 1 {
		t.Errorf("generator calls = %d, want 1 (good_to_go on first pass)", generator.callCount())
	}
	if evaluator.callCount() != 1 {
		t.Errorf("evaluator calls = %d, want 1", evaluator.callCount())
	}

	genPrompt := generator.lastPrompt()
	if !strings.Contains(genPrompt, "Sprint 1") || !strings.Contains(genPrompt, "Build the thing end to end") {
		t.Errorf("generator prompt missing current sprint section: %q", genPrompt)
	}
	evalPrompt := evaluator.lastPrompt()
	if !strings.Contains(evalPrompt, "Implemented the thing in main.go") {
		t.Errorf("evaluator prompt missing generator output: %q", evalPrompt)
	}
}

func TestDevYAML_EndToEnd_RetryThenGoodToGo(t *testing.T) {
	def := loadDevDefinition(t)

	designer := &fakeRunner{name: "designer", responses: []string{
		"NO_QUESTIONS\n## Limits\nmax_passes: 3\nmax_sprints: 1\n\n## Sprint 1\nDo it.\n",
	}}
	generator := &fakeRunner{name: "generator", responses: []string{"pass 1 attempt", "pass 2 attempt"}}
	evaluator := &fakeRunner{name: "evaluator", responses: []string{"not quite\nneeds_refinement", "much better\ngood_to_go"}}

	r := New(def, "task")
	cfg := workflow.RunConfig{Runners: map[string]workflow.TurnRunner{
		"designer": designer, "generator": generator, "evaluator": evaluator,
	}}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if generator.callCount() != 2 || evaluator.callCount() != 2 {
		t.Errorf("expected 2 passes, got generator=%d evaluator=%d", generator.callCount(), evaluator.callCount())
	}
	generator.mu.Lock()
	secondGenPrompt := generator.prompts[1]
	generator.mu.Unlock()
	if !strings.Contains(secondGenPrompt, "pass 2") {
		t.Errorf("second generator prompt = %q, expected a pass-2 marker", secondGenPrompt)
	}
	if !strings.Contains(secondGenPrompt, "not quite") {
		t.Errorf("second generator prompt = %q, expected pass 1's evaluator findings", secondGenPrompt)
	}
}

func TestDevYAML_EndToEnd_ExhaustsPassesReturnsResumableError(t *testing.T) {
	def := loadDevDefinition(t)

	designer := &fakeRunner{name: "designer", responses: []string{
		"NO_QUESTIONS\n## Limits\nmax_passes: 2\nmax_sprints: 1\n\n## Sprint 1\nDo it.\n",
	}}
	generator := &fakeRunner{name: "generator", responses: []string{"attempt"}}
	evaluator := &fakeRunner{name: "evaluator", responses: []string{"needs_refinement"}}

	r := New(def, "task")
	cfg := workflow.RunConfig{Runners: map[string]workflow.TurnRunner{
		"designer": designer, "generator": generator, "evaluator": evaluator,
	}}
	err := r.Run(context.Background(), cfg)
	var exhausted *ExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected *ExhaustedError, got %v", err)
	}
	if exhausted.MaxIterations != 2 || generator.callCount() != 2 {
		t.Errorf("exhausted = %+v, generator calls = %d", exhausted, generator.callCount())
	}
}

func TestDevYAML_EndToEnd_DesignerQuestionsAnsweredByUser(t *testing.T) {
	def := loadDevDefinition(t)

	designer := &fakeRunner{name: "designer", responses: []string{
		"## Questions\n- Q1: which language?\n\n## Defaults\n- Q1: Go",
		"## Spec\nUse Go.\n\n## Limits\nmax_passes: 1\nmax_sprints: 1\n\n## Sprint 1\nWrite it in Go.\n",
	}}
	generator := &fakeRunner{name: "generator", responses: []string{"done"}}
	evaluator := &fakeRunner{name: "evaluator", responses: []string{"good_to_go"}}

	answers := make(chan string, 1)
	var questionsMsg workflow.WorkflowQuestionsMsg
	answers <- "use Rust actually"
	r := New(def, "build something")
	cfg := workflow.RunConfig{
		Runners: map[string]workflow.TurnRunner{
			"designer": designer, "generator": generator, "evaluator": evaluator,
		},
		AnswersCh: answers,
		Send: func(m tea.Msg) {
			if qm, ok := m.(workflow.WorkflowQuestionsMsg); ok {
				questionsMsg = qm
			}
		},
	}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if questionsMsg.Questions == "" {
		t.Error("expected the designer's questions to be sent to the user")
	}
	if designer.callCount() != 2 {
		t.Fatalf("designer calls = %d, want 2 (questions, then finalize)", designer.callCount())
	}
	finalizePrompt := designer.lastPrompt()
	if !strings.Contains(finalizePrompt, "which language?") || !strings.Contains(finalizePrompt, "use Rust actually") {
		t.Errorf("finalize prompt = %q, expected the original question and the user's answer", finalizePrompt)
	}
}
