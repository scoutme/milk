package interp

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/workflow"
)

func loadSwarmDefinition(t *testing.T) workflow.Definition {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	reg, errs := workflow.LoadRegistry()
	if len(errs) != 0 {
		t.Fatalf("LoadRegistry errors: %v", errs)
	}
	def, ok := reg.Lookup("swarm")
	if !ok {
		t.Fatal("expected built-in \"swarm\" definition to be registered")
	}
	return def
}

func TestSwarmYAML_EndToEnd_NoIssuesSkipsFinalImplementation(t *testing.T) {
	def := loadSwarmDefinition(t)

	designer := &fakeRunner{name: "designer", responses: []string{
		"NO_QUESTIONS\n## Spec\nBuild two things.\n\n## Item Plan\n" +
			"## Item 1\nBuild the first thing.\n\n## Item 2\nBuild the second thing.\n",
	}}
	worker := &fakeRunner{name: "worker", responses: []string{"did item 1", "did item 2"}}
	evaluator := &fakeRunner{name: "evaluator", responses: []string{
		"good_to_go", // worker_eval for item 1
		"good_to_go", // worker_eval for item 2
		"NO_ISSUES",  // final_evaluation
	}}
	implementer := &fakeRunner{name: "implementer", responses: []string{"should not be called"}}

	r := New(def, "build two things")
	cfg := workflow.RunConfig{Runners: map[string]workflow.TurnRunner{
		"designer": designer, "worker": worker, "evaluator": evaluator, "implementer": implementer,
	}}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if worker.callCount() != 2 {
		t.Errorf("worker calls = %d, want 2 (one per item, both good_to_go on first pass)", worker.callCount())
	}
	if evaluator.callCount() != 3 {
		t.Errorf("evaluator calls = %d, want 3 (2 per-item + 1 final)", evaluator.callCount())
	}
	if implementer.callCount() != 0 {
		t.Errorf("implementer calls = %d, want 0 (final evaluation reported no issues)", implementer.callCount())
	}
}

func TestSwarmYAML_EndToEnd_IssuesFoundTriggersFinalImplementation(t *testing.T) {
	def := loadSwarmDefinition(t)

	designer := &fakeRunner{name: "designer", responses: []string{
		"NO_QUESTIONS\n## Item Plan\n## Item 1\nBuild it.\n",
	}}
	worker := &fakeRunner{name: "worker", responses: []string{"did it"}}
	evaluator := &fakeRunner{name: "evaluator", responses: []string{
		"good_to_go", // worker_eval
		"item 1 and item 2 use conflicting API shapes", // final_evaluation -- no NO_ISSUES marker
	}}
	implementer := &fakeRunner{name: "implementer", responses: []string{"fixed the conflict"}}

	r := New(def, "build a thing")
	cfg := workflow.RunConfig{Runners: map[string]workflow.TurnRunner{
		"designer": designer, "worker": worker, "evaluator": evaluator, "implementer": implementer,
	}}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if implementer.callCount() != 1 {
		t.Fatalf("implementer calls = %d, want 1", implementer.callCount())
	}
	prompt := implementer.lastPrompt()
	if !strings.Contains(prompt, "conflicting API shapes") {
		t.Errorf("implementer prompt = %q, expected the final evaluation's findings", prompt)
	}
}

func TestSwarmYAML_EndToEnd_PerItemRetryThenGoodToGo(t *testing.T) {
	def := loadSwarmDefinition(t)

	designer := &fakeRunner{name: "designer", responses: []string{
		"NO_QUESTIONS\n## Item Plan\n## Item 1\nBuild it well.\n",
	}}
	worker := &fakeRunner{name: "worker", responses: []string{"attempt 1", "attempt 2"}}
	evaluator := &fakeRunner{name: "evaluator", responses: []string{
		"not quite\nneeds_refinement",
		"better\ngood_to_go",
		"NO_ISSUES",
	}}
	implementer := &fakeRunner{name: "implementer", responses: []string{"n/a"}}

	r := New(def, "build a thing well")
	cfg := workflow.RunConfig{Runners: map[string]workflow.TurnRunner{
		"designer": designer, "worker": worker, "evaluator": evaluator, "implementer": implementer,
	}}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if worker.callCount() != 2 {
		t.Errorf("worker calls = %d, want 2 (retry then good_to_go)", worker.callCount())
	}
	secondPrompt := worker.lastPrompt()
	if !strings.Contains(secondPrompt, "pass 2") || !strings.Contains(secondPrompt, "not quite") {
		t.Errorf("second worker prompt = %q, expected pass-2 marker and pass-1 findings", secondPrompt)
	}
}

func TestSwarmYAML_EndToEnd_DependentItemsRunInDependencyOrder(t *testing.T) {
	def := loadSwarmDefinition(t)

	designer := &fakeRunner{name: "designer", responses: []string{
		"NO_QUESTIONS\n## Item Plan\n## Item 1\nfoundation\n\n## Item 2 (depends_on: 1)\nbuilds on item 1\n",
	}}
	var order []string
	worker := &orderRecordingRunner{order: &order}
	evaluator := &fakeRunner{name: "evaluator", responses: []string{"good_to_go", "good_to_go", "NO_ISSUES"}}
	implementer := &fakeRunner{name: "implementer", responses: []string{"n/a"}}

	r := New(def, "task")
	cfg := workflow.RunConfig{Runners: map[string]workflow.TurnRunner{
		"designer": designer, "worker": worker, "evaluator": evaluator, "implementer": implementer,
	}}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(order) != 2 || order[0] != "1" || order[1] != "2" {
		t.Errorf("execution order = %v, want [1 2] (item 2 depends on item 1)", order)
	}
}

// orderRecordingRunner appends the item index it was called for (detected
// from the "Implement Item N" line the swarm worker prompt always includes)
// to order as it's called -- used to assert dependency-respecting
// scheduling without timing-based flakiness.
type orderRecordingRunner struct {
	order *[]string
}

func (r *orderRecordingRunner) Name() string { return "worker" }
func (r *orderRecordingRunner) Run(_ context.Context, prompt string, _ io.Writer) (string, error) {
	item := "?"
	switch {
	case strings.Contains(prompt, "Implement Item 1"):
		item = "1"
	case strings.Contains(prompt, "Implement Item 2"):
		item = "2"
	}
	*r.order = append(*r.order, item)
	return "done", nil
}
