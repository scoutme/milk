package dev

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
)

func TestDesignerPromptContainsTask(t *testing.T) {
	task := "build a REST API for user management"
	p := designerPrompt(task)
	if !strings.Contains(p, task) {
		t.Errorf("designerPrompt missing task text")
	}
	if !strings.Contains(p, "## Sprint") {
		t.Errorf("designerPrompt missing Sprint heading instruction")
	}
	if !strings.Contains(p, "## Spec") {
		t.Errorf("designerPrompt missing Spec heading instruction")
	}
}

func TestGeneratorPromptContainsSprint(t *testing.T) {
	p := generatorPrompt("/tmp/plan.md", 2, 1, "")
	if !strings.Contains(p, "Sprint 2") {
		t.Errorf("generatorPrompt missing sprint number")
	}
}

func TestGeneratorPromptRefinementPass(t *testing.T) {
	p := generatorPrompt("/tmp/plan.md", 1, 3, "/tmp/findings.md")
	if !strings.Contains(p, "pass 3") {
		t.Errorf("generatorPrompt missing pass count on refinement")
	}
	if !strings.Contains(p, "refine") {
		t.Errorf("generatorPrompt missing refinement note")
	}
}

func TestEvaluatorPromptContainsVerdictInstruction(t *testing.T) {
	p := evaluatorPrompt("/tmp/plan.md", "/tmp/sprint1.md", 1, 1)
	for _, kw := range []string{"good_to_go", "needs_refinement", "next_sprint"} {
		if !strings.Contains(p, kw) {
			t.Errorf("evaluatorPrompt missing verdict keyword %q", kw)
		}
	}
	if !strings.Contains(p, "Sprint 1") {
		t.Errorf("evaluatorPrompt missing sprint reference")
	}
}

func TestCountSprints(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"zero sprints", "no headings here", 0},
		{"one sprint", "## Sprint 1\nsome content", 1},
		{"two sprints", "## Sprint 1\ncontent\n## Sprint 2\nmore", 2},
		{"three sprints", "## Sprint 1\n## Sprint 2\n## Sprint 3\n", 3},
		{"ignores other headings", "## Overview\n## Sprint 1\n## Notes\n## Sprint 2\n", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countSprints(tt.input)
			if got != tt.want {
				t.Errorf("countSprints(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// stubTurnRunner is a minimal TurnRunner for unit tests.
type stubTurnRunner struct {
	name   string
	output string
}

func (s *stubTurnRunner) Name() string { return s.name }
func (s *stubTurnRunner) Run(_ context.Context, _ string, out io.Writer) (string, error) {
	_, _ = io.WriteString(out, s.output)
	return s.output, nil
}

// TestDevWorkflow_NoDoneMessageViaSend verifies that Run never calls send with a
// WorkflowDoneMsg — the goroutine wrapper in cmd/milk is the sole source of that
// message. The test drives the workflow with a stub runner that returns
// "good_to_go" as the evaluator verdict so a single sprint completes cleanly.
func TestDevWorkflow_NoDoneMessageViaSend(t *testing.T) {
	dir := t.TempDir()

	// Stub designer: returns a plan with one sprint heading.
	designerOut := "## Spec\nBuild foo.\n\n## Sprint 1\nImplement foo."
	// Stub generator: returns some output.
	generatorOut := "func main() {}"
	// Stub evaluator: returns good_to_go verdict.
	evaluatorOut := "verdict: good_to_go"

	runners := map[string]workflow.TurnRunner{
		"designer":  &stubTurnRunner{name: "designer", output: designerOut},
		"generator": &stubTurnRunner{name: "generator", output: generatorOut},
		"evaluator": &stubTurnRunner{name: "evaluator", output: evaluatorOut},
	}

	var received []tea.Msg
	send := func(msg tea.Msg) { received = append(received, msg) }

	sess := &session.Session{ID: "test-" + t.Name()}
	cfg := workflow.RunConfig{
		Session:  sess,
		Runners:  runners,
		Send:     send,
		StateDir: dir,
	}

	wf := New("build foo", 3)
	if err := wf.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// Clean up artefact files so os.WriteFile doesn't leave them around.
	_ = os.RemoveAll(dir)

	for _, msg := range received {
		if _, ok := msg.(WorkflowDoneMsg); ok {
			t.Errorf("Run sent a WorkflowDoneMsg via send — it must not; the goroutine wrapper is the sole source")
		}
	}
}
