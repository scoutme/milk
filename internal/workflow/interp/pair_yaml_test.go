package interp

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/workflow"
)

func loadPairDefinition(t *testing.T) workflow.Definition {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	reg, errs := workflow.LoadRegistry()
	if len(errs) != 0 {
		t.Fatalf("LoadRegistry errors: %v", errs)
	}
	def, ok := reg.Lookup("pair")
	if !ok {
		t.Fatal("expected built-in \"pair\" definition to be registered")
	}
	return def
}

func TestPairYAML_EndToEnd_UserAddsContextBeforeDecision(t *testing.T) {
	def := loadPairDefinition(t)

	designer := &fakeRunner{name: "designer", responses: []string{
		"NO_QUESTIONS\n## Limits\nmax_passes: 2\nmax_sprints: 1\n\n## Sprint 1\nBuild it.\n",
	}}
	generator := &fakeRunner{name: "generator", responses: []string{"Implemented it."}}
	evaluator := &fakeRunner{name: "evaluator", responses: []string{
		"Mostly done, one edge case untested.",   // findings turn -- no verdict yet
		"Addressed per user's note.\ngood_to_go", // decision turn, after user input
	}}

	answers := make(chan string, 1)
	answers <- "please also check the empty-input case"
	var questionsMsg workflow.WorkflowQuestionsMsg

	r := New(def, "build a thing")
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

	if evaluator.callCount() != 2 {
		t.Fatalf("evaluator calls = %d, want 2 (findings, then decision)", evaluator.callCount())
	}
	if !strings.Contains(questionsMsg.Questions, "Mostly done, one edge case untested") {
		t.Errorf("expected the user checkpoint to present the evaluator's findings, got %q", questionsMsg.Questions)
	}
	decisionPrompt := evaluator.lastPrompt()
	if !strings.Contains(decisionPrompt, "please also check the empty-input case") {
		t.Errorf("decision prompt = %q, expected the user's input to be included", decisionPrompt)
	}
	if !strings.Contains(decisionPrompt, "Mostly done, one edge case untested") {
		t.Errorf("decision prompt = %q, expected the original findings to be included", decisionPrompt)
	}
}

func TestPairYAML_EndToEnd_UserRequestsRefinement(t *testing.T) {
	def := loadPairDefinition(t)

	designer := &fakeRunner{name: "designer", responses: []string{
		"NO_QUESTIONS\n## Limits\nmax_passes: 3\nmax_sprints: 1\n\n## Sprint 1\nBuild it.\n",
	}}
	generator := &fakeRunner{name: "generator", responses: []string{"attempt 1", "attempt 2"}}
	evaluator := &fakeRunner{name: "evaluator", responses: []string{
		"looks okay",
		"deferring to user\nneeds_refinement", // pass 1 decision: user wasn't happy
		"looks better now",
		"user is satisfied\ngood_to_go", // pass 2 decision
	}}
	answers := make(chan string, 2)
	answers <- "not good enough, please redo the error handling"
	answers <- "looks good now"

	r := New(def, "task")
	cfg := workflow.RunConfig{
		Runners: map[string]workflow.TurnRunner{
			"designer": designer, "generator": generator, "evaluator": evaluator,
		},
		AnswersCh: answers,
		Send:      func(tea.Msg) {},
	}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if generator.callCount() != 2 {
		t.Errorf("generator calls = %d, want 2 (retry after needs_refinement)", generator.callCount())
	}
	if evaluator.callCount() != 4 {
		t.Errorf("evaluator calls = %d, want 4 (findings+decision per pass, 2 passes)", evaluator.callCount())
	}
}
