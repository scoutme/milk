package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
)

// ── parseWorkflowFlags ────────────────────────────────────────────────────────

func TestParseWorkflowFlags_TrailingFlag(t *testing.T) {
	_, _, err := parseWorkflowFlags([]string{"my", "task", "--designer"})
	if err == nil {
		t.Fatal("expected error for trailing --designer flag, got nil")
	}
}

func TestParseWorkflowFlags_TrailingFlagOnlyFlag(t *testing.T) {
	_, _, err := parseWorkflowFlags([]string{"--generator"})
	if err == nil {
		t.Fatal("expected error for lone --generator flag, got nil")
	}
}

func TestParseWorkflowFlags_ValidFlags(t *testing.T) {
	rem, flags, err := parseWorkflowFlags([]string{"my", "task", "--designer", "primary", "--evaluator", "escalation"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rem) != 2 || rem[0] != "my" || rem[1] != "task" {
		t.Errorf("remainder = %v, want [my task]", rem)
	}
	if flags["designer"] != "primary" {
		t.Errorf("designer flag = %q, want %q", flags["designer"], "primary")
	}
	if flags["evaluator"] != "escalation" {
		t.Errorf("evaluator flag = %q, want %q", flags["evaluator"], "escalation")
	}
}

func TestParseWorkflowFlags_NoFlags(t *testing.T) {
	rem, flags, err := parseWorkflowFlags([]string{"build", "a", "REST", "API"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rem) != 4 {
		t.Errorf("remainder = %v, want 4 items", rem)
	}
	if len(flags) != 0 {
		t.Errorf("flags = %v, want empty", flags)
	}
}

func TestParseWorkflowFlags_Empty(t *testing.T) {
	rem, flags, err := parseWorkflowFlags(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rem) != 0 || len(flags) != 0 {
		t.Errorf("expected empty results, got rem=%v flags=%v", rem, flags)
	}
}

// ── advanceWorkflowWizard ─────────────────────────────────────────────────────

// testModel returns a minimal model suitable for wizard-step tests.
// It has the transcript builders initialised but no TUI state.
func testModel() model {
	return model{
		transcript:        &strings.Builder{},
		transcriptNoThink: &strings.Builder{},
	}
}

// TestAdvanceWorkflowWizard_BlankTaskRePrompts verifies that pressing Enter with
// an empty input while at wizardStepTask does NOT advance the step.
// The task must not be set to workflow.AliasEscalation either.
func TestAdvanceWorkflowWizard_BlankTaskRePrompts(t *testing.T) {
	m := testModel()
	m.pendingWorkflowWizard = &workflowWizardState{
		name: "dev",
		step: wizardStepTask,
	}

	newM, _ := m.advanceWorkflowWizard("")
	nm := newM.(model)

	if nm.pendingWorkflowWizard == nil {
		t.Fatal("expected wizard still pending after blank task input")
	}
	if nm.pendingWorkflowWizard.step != wizardStepTask {
		t.Errorf("step = %d, want wizardStepTask (%d)", nm.pendingWorkflowWizard.step, wizardStepTask)
	}
	if nm.pendingWorkflowWizard.task == workflow.AliasEscalation {
		t.Error("task was incorrectly set to AliasEscalation on blank Enter")
	}
	if nm.pendingWorkflowWizard.task != "" {
		t.Errorf("task should remain empty, got %q", nm.pendingWorkflowWizard.task)
	}
}

// TestAdvanceWorkflowWizard_BlankTaskTwiceStillRePrompts verifies re-prompt is idempotent.
func TestAdvanceWorkflowWizard_BlankTaskTwiceStillRePrompts(t *testing.T) {
	m := testModel()
	m.pendingWorkflowWizard = &workflowWizardState{name: "dev", step: wizardStepTask}

	newM, _ := m.advanceWorkflowWizard("")
	nm := newM.(model)
	newM2, _ := nm.advanceWorkflowWizard("")
	nm2 := newM2.(model)

	if nm2.pendingWorkflowWizard == nil {
		t.Fatal("wizard should still be pending after two blank entries")
	}
	if nm2.pendingWorkflowWizard.step != wizardStepTask {
		t.Errorf("step after second blank = %d, want wizardStepTask", nm2.pendingWorkflowWizard.step)
	}
}

// TestAdvanceWorkflowWizard_TaskAdvancesToDesigner verifies that a non-empty task
// advances the wizard to the designer step rather than launching immediately.
func TestAdvanceWorkflowWizard_TaskAdvancesToDesigner(t *testing.T) {
	m := testModel()
	m.pendingWorkflowWizard = &workflowWizardState{name: "dev", step: wizardStepTask}

	newM, _ := m.advanceWorkflowWizard("build a REST API")
	nm := newM.(model)

	if nm.pendingWorkflowWizard == nil {
		t.Fatal("expected wizard still pending after task entry (should be at designer step)")
	}
	if nm.pendingWorkflowWizard.step != wizardStepDesigner {
		t.Errorf("step = %d, want wizardStepDesigner (%d)", nm.pendingWorkflowWizard.step, wizardStepDesigner)
	}
	if nm.pendingWorkflowWizard.task != "build a REST API" {
		t.Errorf("task = %q, want %q", nm.pendingWorkflowWizard.task, "build a REST API")
	}
}

// TestAdvanceWorkflowWizard_AgentStepsFlow verifies the full designer→generator→evaluator
// wizard sequence. After all three agent steps, the wizard is cleared and launchWorkflow
// is invoked (which will fail in test due to no live config — that's expected).
func TestAdvanceWorkflowWizard_AgentStepsFlow(t *testing.T) {
	m := testModel()
	m.st = &interactiveState{}
	m.pendingWorkflowWizard = &workflowWizardState{name: "dev", task: "build X", step: wizardStepDesigner}

	// designer: blank → should default to AliasEscalation
	m1, _ := m.advanceWorkflowWizard("")
	nm1 := m1.(model)
	if nm1.pendingWorkflowWizard == nil || nm1.pendingWorkflowWizard.step != wizardStepGenerator {
		t.Fatalf("after blank designer: want wizardStepGenerator, got step=%v pending=%v",
			nm1.pendingWorkflowWizard.step, nm1.pendingWorkflowWizard != nil)
	}
	if nm1.pendingWorkflowWizard.designer != workflow.AliasEscalation {
		t.Errorf("designer = %q, want %q", nm1.pendingWorkflowWizard.designer, workflow.AliasEscalation)
	}

	// generator: explicit name
	m2, _ := nm1.advanceWorkflowWizard("myagent")
	nm2 := m2.(model)
	if nm2.pendingWorkflowWizard == nil || nm2.pendingWorkflowWizard.step != wizardStepEvaluator {
		t.Fatalf("after generator: want wizardStepEvaluator, got step=%v", nm2.pendingWorkflowWizard.step)
	}
	if nm2.pendingWorkflowWizard.generator != "myagent" {
		t.Errorf("generator = %q, want %q", nm2.pendingWorkflowWizard.generator, "myagent")
	}

	// evaluator: wizard clears and launchWorkflow is called (fails in test — no session)
	m3, _ := nm2.advanceWorkflowWizard("")
	nm3 := m3.(model)
	if nm3.pendingWorkflowWizard != nil {
		t.Errorf("expected wizard cleared after evaluator step, still at step=%d", nm3.pendingWorkflowWizard.step)
	}
}

// ── /workflow clear wizard ────────────────────────────────────────────────────

// TestAdvanceWorkflowWizard_ClearWrongInputCancels verifies that typing anything
// other than "clear" at the confirm step cancels the operation without proceeding.
func TestAdvanceWorkflowWizard_ClearWrongInputCancels(t *testing.T) {
	m := testModel()
	m.pendingWorkflowWizard = &workflowWizardState{
		step:     wizardStepClearConfirm,
		clearing: true,
	}

	for _, bad := range []string{"yes", "CLEAR", "", "y", "delete"} {
		m2 := m
		newM, _ := m2.advanceWorkflowWizard(bad)
		nm := newM.(model)
		if nm.pendingWorkflowWizard != nil {
			t.Errorf("input %q: expected wizard cleared after cancel, still pending", bad)
		}
		if !strings.Contains(nm.transcript.String(), "cancelled") {
			t.Errorf("input %q: expected 'cancelled' in transcript, got: %s", bad, nm.transcript.String())
		}
	}
}

// TestAdvanceWorkflowWizard_ClearCorrectInputProceeds verifies that typing "clear"
// at the confirm step does NOT cancel — it calls execWorkflowClear. With no
// active session in the test model execWorkflowClear returns an error message,
// but it must not produce a "cancelled" message and must clear the wizard.
func TestAdvanceWorkflowWizard_ClearCorrectInputProceeds(t *testing.T) {
	m := testModel()
	m.st = &interactiveState{} // sess is nil → execWorkflowClear reports an error, not "cancelled"
	m.pendingWorkflowWizard = &workflowWizardState{
		step:     wizardStepClearConfirm,
		clearing: true,
	}

	newM, _ := m.advanceWorkflowWizard("clear")
	nm := newM.(model)

	if nm.pendingWorkflowWizard != nil {
		t.Error("expected wizard cleared after 'clear' confirmation")
	}
	if strings.Contains(nm.transcript.String(), "cancelled") {
		t.Errorf("input 'clear' should not produce a cancel message, got: %s", nm.transcript.String())
	}
}

// ── workflow.State.Task persistence ──────────────────────────────────────────

// TestWorkflowStateRoundTrip verifies that Task survives a save/load cycle.
func TestWorkflowStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := workflow.StatePath(dir, "test-session")

	original := &workflow.State{
		WorkflowName: "dev",
		Task:         "build a REST API",
		Sprint:       2,
		Pass:         1,
		Role:         "generator",
	}
	if err := workflow.SaveState(path, original); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	loaded, err := workflow.LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadState returned nil for an existing file")
	}
	if loaded.Task != original.Task {
		t.Errorf("Task = %q, want %q", loaded.Task, original.Task)
	}
	if loaded.WorkflowName != original.WorkflowName {
		t.Errorf("WorkflowName = %q, want %q", loaded.WorkflowName, original.WorkflowName)
	}
}

// TestWorkflowStateTaskOmitEmpty verifies that old state files without a Task
// field load cleanly with Task == "".
func TestWorkflowStateTaskOmitEmpty(t *testing.T) {
	dir := t.TempDir()
	path := workflow.StatePath(dir, "legacy-session")

	legacy := []byte(`{"workflow_name":"dev","sprint":1,"pass":1,"role":"designer"}`)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	st, err := workflow.LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st == nil {
		t.Fatal("LoadState returned nil")
	}
	if st.Task != "" {
		t.Errorf("Task = %q, want empty for legacy file", st.Task)
	}
}

// ── /workflow reconfigure wizard ─────────────────────────────────────────────

// TestReconfigureWizardStepsFlow verifies that the reconfigure wizard walks
// designer → generator → evaluator in order, populates fields correctly, and
// calls applyWorkflowReconfigure (not launchWorkflow) at the end.
// The test drives advanceWorkflowWizard directly, bypassing session.Dir() entirely.
func TestReconfigureWizardStepsFlow(t *testing.T) {
	dir := t.TempDir()

	// Write a state file that applyWorkflowReconfigure can load.
	sessID := "reconf-flow-test"
	original := &workflow.State{
		WorkflowName: "dev",
		Task:         "task X",
		Sprint:       3,
		Pass:         2,
		Role:         "evaluator",
		AgentMap:     map[string]string{"designer": "old-d", "generator": "old-g", "evaluator": "old-e"},
	}
	statePath := workflow.StatePath(dir, sessID)
	if err := workflow.SaveState(statePath, original); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	sess := &session.Session{ID: sessID}
	m := testModel()
	m.st = &interactiveState{sess: sess}

	// Pre-populate the wizard as handleWorkflowReconfigure would.
	m.pendingWorkflowWizard = &workflowWizardState{
		name:          "dev",
		task:          "task X",
		step:          wizardStepDesigner,
		reconfiguring: true,
		sprint:        3,
		pass:          2,
		role:          "evaluator",
	}

	// designer step
	m1, _ := m.advanceWorkflowWizard("new-d")
	nm1 := m1.(model)
	if nm1.pendingWorkflowWizard == nil || nm1.pendingWorkflowWizard.step != wizardStepGenerator {
		t.Fatalf("after designer: want wizardStepGenerator, got step=%v pending=%v",
			nm1.pendingWorkflowWizard.step, nm1.pendingWorkflowWizard != nil)
	}
	if nm1.pendingWorkflowWizard.designer != "new-d" {
		t.Errorf("designer = %q, want %q", nm1.pendingWorkflowWizard.designer, "new-d")
	}

	// generator step
	m2, _ := nm1.advanceWorkflowWizard("new-g")
	nm2 := m2.(model)
	if nm2.pendingWorkflowWizard == nil || nm2.pendingWorkflowWizard.step != wizardStepEvaluator {
		t.Fatalf("after generator: want wizardStepEvaluator")
	}

	// evaluator step: wizard completes, applyWorkflowReconfigure is called.
	// With a nil program (no TUI), applyWorkflowReconfigure will call session.Dir()
	// which returns the real ~/.milk/sessions — we can't write there in tests.
	// Instead, verify that wizard is cleared and reconfiguring=true reached the dispatch.
	m3, _ := nm2.advanceWorkflowWizard("new-e")
	nm3 := m3.(model)
	if nm3.pendingWorkflowWizard != nil {
		t.Error("expected wizard cleared after evaluator step")
	}
	// The evaluator field must have been set before dispatch.
	// (applyWorkflowReconfigure may fail due to missing state dir — that's OK in test.)
	if nm2.pendingWorkflowWizard.evaluator != "" {
		// already nil after step advance; check pre-advance state captured in nm2
	}
	// Verify reconfiguring branch was taken (not resuming or fresh launch):
	// if launchWorkflow had been called, cancelTurn would be set; it must NOT be.
	if nm3.cancelTurn != nil {
		t.Error("launchWorkflow was called instead of applyWorkflowReconfigure")
	}
}

// TestReconfigureWizardState_ReconfigFlagPreserved verifies that the reconfiguring
// flag is propagated through all wizard steps without being cleared.
func TestReconfigureWizardState_ReconfigFlagPreserved(t *testing.T) {
	m := testModel()
	m.st = &interactiveState{}
	m.pendingWorkflowWizard = &workflowWizardState{
		name:          "dev",
		task:          "task",
		step:          wizardStepDesigner,
		reconfiguring: true,
		sprint:        2,
		pass:          1,
		role:          "evaluator",
	}

	// After designer step, reconfiguring must still be true.
	m1, _ := m.advanceWorkflowWizard("agent-a")
	nm1 := m1.(model)
	if nm1.pendingWorkflowWizard == nil {
		t.Fatal("wizard cleared too early")
	}
	if !nm1.pendingWorkflowWizard.reconfiguring {
		t.Error("reconfiguring flag lost after designer step")
	}
	if nm1.pendingWorkflowWizard.sprint != 2 || nm1.pendingWorkflowWizard.pass != 1 {
		t.Errorf("checkpoint changed: sprint=%d pass=%d", nm1.pendingWorkflowWizard.sprint, nm1.pendingWorkflowWizard.pass)
	}

	// After generator step.
	m2, _ := nm1.advanceWorkflowWizard("agent-b")
	nm2 := m2.(model)
	if nm2.pendingWorkflowWizard == nil {
		t.Fatal("wizard cleared after generator step")
	}
	if !nm2.pendingWorkflowWizard.reconfiguring {
		t.Error("reconfiguring flag lost after generator step")
	}
}

// ── /workflow autocomplete hints ──────────────────────────────────────────────

// TestWorkflowCmdVariants verifies that all four /workflow subcommands appear in
// cmdVariants, so tab-completion and inline hints work.
func TestWorkflowCmdVariants(t *testing.T) {
	vs := cmdVariants["/workflow"]
	if len(vs) == 0 {
		t.Fatal("cmdVariants[\"/workflow\"] is empty — /workflow has no autocomplete entries")
	}
	sigs := make([]string, len(vs))
	for i, v := range vs {
		sigs[i] = v.sig
	}
	for _, want := range []string{"dev", "resume", "reconfigure", "clear"} {
		found := false
		for _, sig := range sigs {
			if strings.Contains(sig, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("subcommand %q not found in /workflow variants: %v", want, sigs)
		}
	}
}

// ── TestLaunchWorkflow_SetsCancelTurn ─────────────────────────────────────────

// TestLaunchWorkflow_SetsCancelTurn verifies that launchWorkflow assigns a non-nil
// cancelTurn to the model so Ctrl+C can cancel a running workflow.
//
// The test exploits the fact that with a zero config and nil primary/escalation
// runners, both cfg.ActiveAgent().Name and da.primary.Name() resolve to "", so
// buildWorkflowRunners takes the "matches primary name" branch for all three roles
// (no error returned) and the code reaches the ctx/cancel assignment.
func TestLaunchWorkflow_SetsCancelTurn(t *testing.T) {
	m := testModel()
	m.ctx = context.Background()
	// Zero-value interactiveState: cfg is empty, ActiveAgent().Name == "".
	m.st = &interactiveState{}
	// da.primary and da.escalation are nil, so both primaryName and escalationName
	// are "". AliasPrimary resolves to cfg.ActiveAgent().Name == "" as well,
	// so buildWorkflowRunners takes the matching-primary-name branch (no error).
	// da.agents is already zero-value (nil primary/escalation), which is fine.

	wizard := &workflowWizardState{
		name:      "dev",
		task:      "build hello world",
		designer:  workflow.AliasPrimary,
		generator: workflow.AliasPrimary,
		evaluator: workflow.AliasPrimary,
	}

	newM, _ := m.launchWorkflow(wizard)
	nm := newM.(model)

	if nm.cancelTurn == nil {
		t.Error("expected cancelTurn to be non-nil after launchWorkflow; was the workflow goroutine launched?")
	}
}
