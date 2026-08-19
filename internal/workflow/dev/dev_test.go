package dev

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

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
	p := evaluatorPrompt("/tmp/plan.md", "/tmp/sprint1.md", 1, 1, 3)
	for _, kw := range []string{"good_to_go", "needs_refinement", "sprint_done"} {
		if !strings.Contains(p, kw) {
			t.Errorf("evaluatorPrompt missing verdict keyword %q", kw)
		}
	}
	if !strings.Contains(p, "Sprint 1") {
		t.Errorf("evaluatorPrompt missing sprint reference")
	}
}

func TestParseLimits(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantPasses  int
		wantSprints int
	}{
		{"empty", "", 0, 0},
		{"both present", "max_passes: 4\nmax_sprints: 2\n", 4, 2},
		{"only max_passes", "max_passes: 5\n", 5, 0},
		{"only max_sprints", "max_sprints: 3\n", 0, 3},
		{"inline in section", "## Limits\nmax_passes: 2\nmax_sprints: 1\n## Sprint 1\n", 2, 1},
		{"leading spaces", "  max_passes:  3\n  max_sprints: 2\n", 3, 2},
		{"ignores non-numeric", "max_passes: abc\n", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotP, gotS := parseLimits(tt.input)
			if gotP != tt.wantPasses {
				t.Errorf("parseLimits max_passes = %d, want %d", gotP, tt.wantPasses)
			}
			if gotS != tt.wantSprints {
				t.Errorf("parseLimits max_sprints = %d, want %d", gotS, tt.wantSprints)
			}
		})
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
		{"nested one level deeper than instructed", "## Sprint Plan\n### Sprint 1\ncontent\n### Sprint 2\nmore\n", 2},
		{"mixed heading levels", "## Sprint 1\ncontent\n### Sprint 2\nmore\n", 2},
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

func TestParseSprintSection(t *testing.T) {
	plan := `## Spec
Build a REST API.

## Sprint 1
Create user model and database schema.
- Create ` + "`models/user.go`" + `
- Create ` + "`migrations/001_create_users.sql`" + `

## Sprint 2
Implement CRUD endpoints.
- Create ` + "`handlers/user.go`" + `
- Create ` + "`routes/routes.go`" + `

## Sprint 3
Add authentication.
- Create ` + "`middleware/auth.go`" + `
`

	tests := []struct {
		name    string
		sprint  int
		want    string
		wantNil bool
	}{
		{
			name:   "sprint 1",
			sprint: 1,
			want:   "## Sprint 1\nCreate user model and database schema.\n- Create `models/user.go`\n- Create `migrations/001_create_users.sql`",
		},
		{
			name:   "sprint 2",
			sprint: 2,
			want:   "## Sprint 2\nImplement CRUD endpoints.\n- Create `handlers/user.go`\n- Create `routes/routes.go`",
		},
		{
			name:   "sprint 3",
			sprint: 3,
			want:   "## Sprint 3\nAdd authentication.\n- Create `middleware/auth.go`",
		},
		{
			name:    "sprint 4 does not exist",
			sprint:  4,
			wantNil: true,
		},
		{
			name:    "sprint 0 does not exist",
			sprint:  0,
			wantNil: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSprintSection(plan, tt.sprint)
			if tt.wantNil {
				if got != "" {
					t.Errorf("parseSprintSection(%d) = %q, want empty", tt.sprint, got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("parseSprintSection(%d) =\n%s\n\nwant:\n%s", tt.sprint, got, tt.want)
			}
		})
	}
}

func TestParseSprintSection_EmptyPlan(t *testing.T) {
	got := parseSprintSection("", 1)
	if got != "" {
		t.Errorf("parseSprintSection empty plan = %q, want empty", got)
	}
}

func TestParseSprintSection_CaseInsensitive(t *testing.T) {
	plan := "## sprint 1\nlowercase heading"
	got := parseSprintSection(plan, 1)
	if got == "" {
		t.Error("parseSprintSection should match case-insensitive sprint headings")
	}
}

func TestParseSprintSection_NestedHeadingLevel(t *testing.T) {
	plan := "## Sprint Plan\n### Sprint 1\nfirst\n### Sprint 2\nsecond\n"
	got := parseSprintSection(plan, 2)
	want := "### Sprint 2\nsecond"
	if got != want {
		t.Errorf("parseSprintSection(2) = %q, want %q", got, want)
	}
}

func TestHasQuestions(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     bool
	}{
		{"NO_QUESTIONS", "NO_QUESTIONS", false},
		{"no_questions lowercase", "no_questions", false},
		{"NO_QUESTIONS with spaces", "  NO_QUESTIONS  ", false},
		{"NO_QUESTIONS with plan", "NO_QUESTIONS\n\n## Spec\nBuild foo.", false},
		{"has questions", "## Questions\n- Q1: Which database?", true},
		{"empty string", "", true}, // empty is not "NO_QUESTIONS"
		{
			name:     "NO_QUESTIONS after reasoning preamble",
			response: "Let me consider whether this task has any ambiguities.\n\nIt's clear enough.\n\nNO_QUESTIONS\n\n## Spec\nBuild foo.",
			want:     false,
		},
		{
			name:     "NO_QUESTIONS mentioned inline, not on its own line",
			response: "Given the clarity of the task, NO_QUESTIONS needed here — asking would just waste time.",
			want:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasQuestions(tt.response)
			if got != tt.want {
				t.Errorf("hasQuestions(%q) = %v, want %v", tt.response, got, tt.want)
			}
		})
	}
}

// TestHasQuestions_ReproducesRealWorldHalt is the exact shape of response
// that halted a real workflow run for user input despite the designer
// clearly deciding there were no questions: a short reasoning preamble,
// then NO_QUESTIONS on its own line, then the plan. hasQuestions used to
// only check whether the *entire trimmed response* started with
// "NO_QUESTIONS", so any preamble caused a false positive.
func TestHasQuestions_ReproducesRealWorldHalt(t *testing.T) {
	response := "The task description is detailed and unambiguous — domain model, exchange " +
		"mechanics, and limits are all spelled out.\n\nNO_QUESTIONS\n\n## Spec\nBuild the marketplace."
	if hasQuestions(response) {
		t.Error("hasQuestions incorrectly reported questions present — this is the bug that halted a live workflow for no reason")
	}
}

func TestStripNoQuestionsPreamble_DiscardsTextBeforeMarker(t *testing.T) {
	response := "Thinking about it, no ambiguity here.\n\nNO_QUESTIONS\n\n## Spec\nBuild foo.\n\n## Sprint 1\nDo it."
	got := stripNoQuestionsPreamble(response)
	want := "## Spec\nBuild foo.\n\n## Sprint 1\nDo it."
	if got != want {
		t.Errorf("stripNoQuestionsPreamble(%q) = %q, want %q", response, got, want)
	}
}

func TestStripNoQuestionsPreamble_NoMarkerFallsBackToTrimmedInput(t *testing.T) {
	response := "  some text with no marker at all  "
	got := stripNoQuestionsPreamble(response)
	want := "some text with no marker at all"
	if got != want {
		t.Errorf("stripNoQuestionsPreamble(%q) = %q, want %q", response, got, want)
	}
}

func TestDesignerQuestionsPrompt(t *testing.T) {
	task := "build a web app"
	p := designerQuestionsPrompt(task)
	if !strings.Contains(p, task) {
		t.Error("designerQuestionsPrompt missing task text")
	}
	if !strings.Contains(p, "NO_QUESTIONS") {
		t.Error("designerQuestionsPrompt missing NO_QUESTIONS instruction")
	}
	if !strings.Contains(p, "## Questions") {
		t.Error("designerQuestionsPrompt missing Questions heading instruction")
	}
	// Regression: dev.go's NO_QUESTIONS branch (Run, in the designer step) treats
	// everything after "NO_QUESTIONS" as the plan with no follow-up call — so the
	// prompt must itself instruct the designer to include the full plan right
	// there. Without this, a model that follows instructions literally replies
	// with just "NO_QUESTIONS" and the workflow ends up with an empty plan,
	// collapsing sprintCount to 1 and finishing after the first sprint.
	if !strings.Contains(p, "## Sprint") {
		t.Error("designerQuestionsPrompt must instruct the designer to include the full plan after NO_QUESTIONS")
	}
}

func TestDesignerFinalizePrompt(t *testing.T) {
	task := "build a web app"
	questions := "## Questions\n- Q1: Which database?"
	answers := "Q1: PostgreSQL"
	p := designerFinalizePrompt(task, questions, answers)
	if !strings.Contains(p, task) {
		t.Error("designerFinalizePrompt missing task text")
	}
	if !strings.Contains(p, questions) {
		t.Error("designerFinalizePrompt missing questions")
	}
	if !strings.Contains(p, answers) {
		t.Error("designerFinalizePrompt missing answers")
	}
	if !strings.Contains(p, "## Sprint") {
		t.Error("designerFinalizePrompt missing Sprint heading instruction")
	}
}

// TestDevWorkflow_DesignerDisambiguation verifies the two-step designer flow:
// 1. Designer asks questions
// 2. User provides answers via channel
// 3. Designer finalizes plan with answers
func TestDevWorkflow_DesignerDisambiguation(t *testing.T) {
	dir := t.TempDir()

	questionsOutput := "## Questions\n- Q1: Which database?\n- Q2: REST or GraphQL?\n\n## Defaults\n- Q1: PostgreSQL\n- Q2: REST"
	finalPlan := "## Spec\nBuild REST API with PostgreSQL.\n\n## Limits\nmax_passes: 2\nmax_sprints: 1\n\n## Sprint 1\nImplement API."
	generatorOut := "func main() {}"
	evaluatorOut := "verdict: good_to_go"

	designerCalls := 0
	var lastDesignerPrompt string

	runners := map[string]workflow.TurnRunner{
		"designer": &callCaptureRunner{
			name: "designer",
			handler: func(prompt string) string {
				designerCalls++
				lastDesignerPrompt = prompt
				if designerCalls == 1 {
					return questionsOutput // first call: return questions
				}
				return finalPlan // second call: finalize with answers
			},
		},
		"generator": &stubTurnRunner{name: "generator", output: generatorOut},
		"evaluator": &stubTurnRunner{name: "evaluator", output: evaluatorOut},
	}

	questionsCh := make(chan workflow.WorkflowQuestionsMsg, 1)
	answersCh := make(chan string, 1)

	sess := &session.Session{ID: "test-" + t.Name()}
	cfg := workflow.RunConfig{
		Session:  sess,
		Runners:  runners,
		StateDir: dir,
		Send: func(msg tea.Msg) {
			if qm, ok := msg.(workflow.WorkflowQuestionsMsg); ok {
				questionsCh <- qm
			}
		},
		AnswersCh: answersCh,
	}

	// Run workflow in background.
	errCh := make(chan error, 1)
	go func() {
		errCh <- New("build a web app", 3).Run(context.Background(), cfg)
	}()

	// Receive questions from channel.
	select {
	case msg := <-questionsCh:
		if !strings.Contains(msg.Questions, "Q1: Which database?") {
			t.Errorf("unexpected questions: %s", msg.Questions)
		}
	case err := <-errCh:
		t.Fatalf("workflow completed before sending questions: %v", err)
	}

	// Send answers.
	select {
	case answersCh <- "Q1: PostgreSQL\nQ2: REST":
	case err := <-errCh:
		t.Fatalf("workflow completed before we could send answers: %v", err)
	}

	// Wait for completion.
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("workflow failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("workflow timed out")
	}

	if designerCalls != 2 {
		t.Errorf("designer called %d times, want 2 (questions + finalize)", designerCalls)
	}
	if !strings.Contains(lastDesignerPrompt, "PostgreSQL") {
		t.Error("finalize prompt should contain user's answer 'PostgreSQL'")
	}
}

// TestDevWorkflow_DesignerNoQuestions verifies that when the designer returns
// NO_QUESTIONS, the workflow proceeds directly with the response as the plan.
func TestDevWorkflow_DesignerNoQuestions(t *testing.T) {
	dir := t.TempDir()

	plan := "## Spec\nBuild foo.\n\n## Limits\nmax_passes: 2\nmax_sprints: 1\n\n## Sprint 1\nImplement foo."
	generatorOut := "func main() {}"
	evaluatorOut := "verdict: good_to_go"

	designerCalls := 0

	runners := map[string]workflow.TurnRunner{
		"designer": &callCaptureRunner{
			name: "designer",
			handler: func(prompt string) string {
				designerCalls++
				return "NO_QUESTIONS\n\n" + plan
			},
		},
		"generator": &stubTurnRunner{name: "generator", output: generatorOut},
		"evaluator": &stubTurnRunner{name: "evaluator", output: evaluatorOut},
	}

	questionsCh := make(chan workflow.WorkflowQuestionsMsg, 1)
	answersCh := make(chan string, 1)

	sess := &session.Session{ID: "test-" + t.Name()}
	cfg := workflow.RunConfig{
		Session:  sess,
		Runners:  runners,
		StateDir: dir,
		Send: func(msg tea.Msg) {
			if qm, ok := msg.(workflow.WorkflowQuestionsMsg); ok {
				questionsCh <- qm
			}
		},
		AnswersCh: answersCh,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- New("build foo", 3).Run(context.Background(), cfg)
	}()

	// Should NOT receive questions on channel.
	select {
	case msg := <-questionsCh:
		t.Errorf("unexpected questions received: %s", msg.Questions)
	case err := <-errCh:
		if err != nil {
			t.Fatalf("workflow failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("workflow timed out")
	}

	if designerCalls != 1 {
		t.Errorf("designer called %d times, want 1 (no questions path)", designerCalls)
	}
}

// callCaptureRunner calls a handler function with the prompt and returns the result.
type callCaptureRunner struct {
	name    string
	handler func(prompt string) string
}

func (c *callCaptureRunner) Name() string { return c.name }
func (c *callCaptureRunner) Run(_ context.Context, prompt string, out io.Writer) (string, error) {
	result := c.handler(prompt)
	_, _ = io.WriteString(out, result)
	return result, nil
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

// TestDevWorkflow_PlanMaxPassesHonoured verifies that max_passes declared in the
// plan overrides the caller-supplied default.
func TestDevWorkflow_PlanMaxPassesHonoured(t *testing.T) {
	dir := t.TempDir()

	// Plan declares max_passes: 1 — workflow must halt after the first pass
	// instead of retrying when the evaluator returns needs_refinement.
	designerOut := "## Spec\nBuild foo.\n\n## Limits\nmax_passes: 1\nmax_sprints: 1\n\n## Sprint 1\nImplement foo."
	generatorOut := "func main() {}"
	evaluatorOut := "needs_refinement" // would normally trigger a retry

	runners := map[string]workflow.TurnRunner{
		"designer":  &stubTurnRunner{name: "designer", output: designerOut},
		"generator": &stubTurnRunner{name: "generator", output: generatorOut},
		"evaluator": &stubTurnRunner{name: "evaluator", output: evaluatorOut},
	}

	sess := &session.Session{ID: "test-" + t.Name()}
	cfg := workflow.RunConfig{
		Session:  sess,
		Runners:  runners,
		StateDir: dir,
	}

	wf := New("build foo", 3) // caller says 3 passes; plan says 1
	err := wf.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error due to max_passes:1 exceeded, got nil")
	}
	if !strings.Contains(err.Error(), "exceeded") && !strings.Contains(err.Error(), "pass") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestDevWorkflow_ResumeSkipsGenerator verifies that when the saved state has
// role="evaluator", resuming re-runs the evaluator without re-running the generator.
func TestDevWorkflow_ResumeSkipsGenerator(t *testing.T) {
	dir := t.TempDir()

	plan := "## Spec\nBuild foo.\n\n## Limits\nmax_passes: 3\nmax_sprints: 1\n\n## Sprint 1\nImplement foo."
	generatorOut := "func main() {}"
	evaluatorOut := "verdict: good_to_go"

	generatorCalls := 0
	evaluatorCalls := 0

	runners := map[string]workflow.TurnRunner{
		"designer": &stubTurnRunner{name: "designer", output: plan},
		"generator": &callCountRunner{
			stubTurnRunner: stubTurnRunner{name: "generator", output: generatorOut},
			count:          &generatorCalls,
		},
		"evaluator": &callCountRunner{
			stubTurnRunner: stubTurnRunner{name: "evaluator", output: evaluatorOut},
			count:          &evaluatorCalls,
		},
	}

	// Write the plan file (simulating a completed designer turn).
	sess := &session.Session{ID: "test-" + t.Name()}
	planPath := dir + "/" + sess.ID + ".workflow.plan.md"
	if err := os.WriteFile(planPath, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	// Write the sprint file (simulating a completed generator turn).
	sprintPath := dir + "/" + sess.ID + ".workflow.sprint1.md"
	if err := os.WriteFile(sprintPath, []byte(generatorOut), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := workflow.RunConfig{
		Session:  sess,
		Runners:  runners,
		StateDir: dir,
	}

	// Resume at sprint=1, pass=1, role="evaluator" — generator must not run.
	wf := NewResume("build foo", 0, 1, 1, "evaluator")
	if err := wf.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if generatorCalls != 0 {
		t.Errorf("generator was called %d time(s), want 0 on evaluator-role resume", generatorCalls)
	}
	if evaluatorCalls != 1 {
		t.Errorf("evaluator was called %d time(s), want 1", evaluatorCalls)
	}
}

// TestDevWorkflow_ResumePreservesVerdictHistory verifies that resuming from a
// later sprint does not drop the verdict history recorded for sprints
// completed before the resume point — Run builds a fresh workflow.State on
// every call, so the resume path must explicitly carry forward the prior
// state's VerdictHistory or the workflow panel loses sprint 1's status
// entirely once sprint 2 starts.
func TestDevWorkflow_ResumePreservesVerdictHistory(t *testing.T) {
	dir := t.TempDir()

	plan := "## Spec\nBuild foo.\n\n## Limits\nmax_passes: 3\nmax_sprints: 2\n\n" +
		"## Sprint 1\nFirst.\n\n## Sprint 2\nSecond."
	generatorOut := "func main() {}"
	evaluatorOut := "verdict: good_to_go"

	runners := map[string]workflow.TurnRunner{
		"designer":  &stubTurnRunner{name: "designer", output: plan},
		"generator": &stubTurnRunner{name: "generator", output: generatorOut},
		"evaluator": &stubTurnRunner{name: "evaluator", output: evaluatorOut},
	}

	sess := &session.Session{ID: "test-" + t.Name()}
	planPath := dir + "/" + sess.ID + ".workflow.plan.md"
	if err := os.WriteFile(planPath, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate a prior run that finished sprint 1 (one refinement pass, then
	// good_to_go) and was then resumed for sprint 2.
	statePath := workflow.StatePath(dir, sess.ID)
	priorState := &workflow.State{
		WorkflowName: "dev",
		Sprint:       1,
		Pass:         2,
		Role:         "done",
		VerdictHistory: []workflow.VerdictEntry{
			{Sprint: 1, Pass: 1, Verdict: "needs_refinement"},
			{Sprint: 1, Pass: 2, Verdict: "good_to_go"},
		},
	}
	if err := workflow.SaveState(statePath, priorState); err != nil {
		t.Fatal(err)
	}

	cfg := workflow.RunConfig{
		Session:  sess,
		Runners:  runners,
		StateDir: dir,
	}

	wf := NewResume("build foo", 0, 2, 1, "generator")
	if err := wf.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	final, err := workflow.LoadState(statePath)
	if err != nil || final == nil {
		t.Fatalf("failed to load final state: %v", err)
	}
	if len(final.VerdictHistory) != 3 {
		t.Fatalf("VerdictHistory = %+v, want 3 entries (2 carried over from sprint 1 + 1 new for sprint 2)",
			final.VerdictHistory)
	}
	if final.VerdictHistory[0].Sprint != 1 || final.VerdictHistory[1].Sprint != 1 {
		t.Errorf("expected sprint 1 history preserved at the front, got %+v", final.VerdictHistory)
	}
	if final.VerdictHistory[2].Sprint != 2 {
		t.Errorf("expected new sprint 2 verdict appended after the carried-over history, got %+v", final.VerdictHistory[2])
	}
}

// TestBackfillVerdictHistory_UsesFindingsFileForMissingSprint exercises the
// exact real-world gap this guards against: a saved state whose
// VerdictHistory is missing an earlier sprint entirely (e.g. dropped by the
// stale-sprint-count bug this workflow package used to have, or any future
// bug/crash that skips a checkpoint), while that sprint's findings file is
// still on disk. backfillVerdictHistory must reconstruct an entry from it
// rather than silently leaving the sprint absent from history.
func TestBackfillVerdictHistory_UsesFindingsFileForMissingSprint(t *testing.T) {
	dir := t.TempDir()
	sessionID := "test-" + t.Name()

	findings1 := dir + "/" + sessionID + ".workflow.findings1.md"
	if err := os.WriteFile(findings1, []byte("Sprint 1 looks complete.\n\ngood_to_go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	findings2 := dir + "/" + sessionID + ".workflow.findings2.md"
	if err := os.WriteFile(findings2, []byte("Sprint 2 has issues.\n\nneeds_refinement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// No findings file for sprint 3 — should be left alone (nothing to backfill from).

	history := []workflow.VerdictEntry{
		{Sprint: 2, Pass: 3, Verdict: "good_to_go"}, // sprint 2 already has an entry — must not be touched
	}

	got := backfillVerdictHistory(history, dir, sessionID, 4)

	if len(got) != 2 {
		t.Fatalf("VerdictHistory = %+v, want 2 entries (backfilled sprint 1 + existing sprint 2)", got)
	}
	if got[0].Sprint != 1 || got[0].Verdict != "good_to_go" {
		t.Errorf("expected a backfilled sprint 1 good_to_go entry at the front, got %+v", got[0])
	}
	if got[1].Sprint != 2 || got[1].Pass != 3 || got[1].Verdict != "good_to_go" {
		t.Errorf("existing sprint 2 entry must be preserved unchanged, got %+v", got[1])
	}
}

func TestBackfillVerdictHistory_NoOpWhenHistoryAlreadyComplete(t *testing.T) {
	dir := t.TempDir()
	history := []workflow.VerdictEntry{
		{Sprint: 1, Pass: 1, Verdict: "good_to_go"},
		{Sprint: 2, Pass: 1, Verdict: "good_to_go"},
	}
	got := backfillVerdictHistory(history, dir, "unused-session", 3)
	if len(got) != 2 {
		t.Errorf("expected no changes when every sprint already has an entry, got %+v", got)
	}
}

// TestDevWorkflow_ResumeBackfillsMissingSprintHistory verifies the fix end to
// end through Run(): a resumed run whose saved VerdictHistory is missing
// sprint 1 (but whose findings1.md still exists) ends up with sprint 1
// restored in the final history, without any manual reconstruction.
func TestDevWorkflow_ResumeBackfillsMissingSprintHistory(t *testing.T) {
	dir := t.TempDir()

	plan := "## Spec\nBuild foo.\n\n## Limits\nmax_passes: 3\nmax_sprints: 3\n\n" +
		"## Sprint 1\nFirst.\n\n## Sprint 2\nSecond.\n\n## Sprint 3\nThird."
	runners := map[string]workflow.TurnRunner{
		"designer":  &stubTurnRunner{name: "designer", output: plan},
		"generator": &stubTurnRunner{name: "generator", output: "func main() {}"},
		"evaluator": &stubTurnRunner{name: "evaluator", output: "verdict: good_to_go"},
	}

	sess := &session.Session{ID: "test-" + t.Name()}
	planPath := dir + "/" + sess.ID + ".workflow.plan.md"
	if err := os.WriteFile(planPath, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	// Ground truth for sprint 1, still on disk, but NOT reflected below in
	// the saved state's VerdictHistory — simulating a dropped checkpoint.
	findings1 := dir + "/" + sess.ID + ".workflow.findings1.md"
	if err := os.WriteFile(findings1, []byte("All good.\n\ngood_to_go\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	statePath := workflow.StatePath(dir, sess.ID)
	priorState := &workflow.State{
		WorkflowName: "dev",
		Sprint:       2,
		Pass:         1,
		Role:         "generator",
		VerdictHistory: []workflow.VerdictEntry{
			{Sprint: 2, Pass: 1, Verdict: "good_to_go"},
		},
	}
	if err := workflow.SaveState(statePath, priorState); err != nil {
		t.Fatal(err)
	}

	cfg := workflow.RunConfig{Session: sess, Runners: runners, StateDir: dir}
	wf := NewResume("build foo", 0, 3, 1, "generator")
	if err := wf.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	final, err := workflow.LoadState(statePath)
	if err != nil || final == nil {
		t.Fatalf("failed to load final state: %v", err)
	}
	foundSprint1 := false
	for _, v := range final.VerdictHistory {
		if v.Sprint == 1 && v.Verdict == "good_to_go" {
			foundSprint1 = true
		}
	}
	if !foundSprint1 {
		t.Errorf("expected sprint 1 backfilled into final VerdictHistory from findings1.md, got %+v", final.VerdictHistory)
	}
}

// failNthRunner fails with err on its Nth call (1-indexed) and otherwise
// returns output.
type failNthRunner struct {
	name   string
	output string
	failOn int
	err    error
	calls  int
}

func (r *failNthRunner) Name() string { return r.name }
func (r *failNthRunner) Run(_ context.Context, _ string, out io.Writer) (string, error) {
	r.calls++
	if r.calls == r.failOn {
		return "", r.err
	}
	_, _ = io.WriteString(out, r.output)
	return r.output, nil
}

// TestDevWorkflow_ChecksPointsSprintBeforeGeneratorRuns verifies that the
// on-disk state reflects the sprint/pass about to be attempted *before* the
// generator runs, not just after it succeeds. Without this, a generator
// failure on a new sprint's first pass — e.g. the transient network error
// this guards against in workflow.Turn — left the saved state pointing at
// the previous, already-approved sprint, so /workflow resume would re-run
// that sprint's evaluator instead of retrying the sprint that actually failed.
func TestDevWorkflow_ChecksPointsSprintBeforeGeneratorRuns(t *testing.T) {
	dir := t.TempDir()

	plan := "## Spec\nBuild foo.\n\n## Limits\nmax_passes: 3\nmax_sprints: 2\n\n" +
		"## Sprint 1\nFirst.\n\n## Sprint 2\nSecond."
	generatorFailure := errors.New("boom: sprint 2 generator failed")

	runners := map[string]workflow.TurnRunner{
		"designer":  &stubTurnRunner{name: "designer", output: plan},
		"generator": &failNthRunner{name: "generator", output: "func main() {}", failOn: 2, err: generatorFailure},
		"evaluator": &stubTurnRunner{name: "evaluator", output: "verdict: good_to_go"},
	}

	sess := &session.Session{ID: "test-" + t.Name()}
	cfg := workflow.RunConfig{
		Session:  sess,
		Runners:  runners,
		StateDir: dir,
	}

	err := New("build foo", 3).Run(context.Background(), cfg)
	if !errors.Is(err, generatorFailure) {
		t.Fatalf("Run error = %v, want to wrap %v", err, generatorFailure)
	}

	statePath := workflow.StatePath(dir, sess.ID)
	final, loadErr := workflow.LoadState(statePath)
	if loadErr != nil || final == nil {
		t.Fatalf("failed to load state after failure: %v", loadErr)
	}
	if final.Sprint != 2 || final.Pass != 1 || final.Role != "generator" {
		t.Errorf("saved state = {sprint:%d pass:%d role:%q}, want {sprint:2 pass:1 role:\"generator\"} "+
			"so /workflow resume retries sprint 2's generator instead of re-running sprint 1's evaluator",
			final.Sprint, final.Pass, final.Role)
	}
}

// callCountRunner wraps stubTurnRunner and increments a counter on each Run call.
type callCountRunner struct {
	stubTurnRunner
	count *int
}

func (c *callCountRunner) Run(ctx context.Context, prompt string, out io.Writer) (string, error) {
	*c.count++
	return c.stubTurnRunner.Run(ctx, prompt, out)
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
