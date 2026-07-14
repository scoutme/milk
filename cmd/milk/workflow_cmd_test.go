package main

import (
	"context"
	"os"
	"strings"
	"testing"

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

// TestAdvanceWorkflowWizard_NonBlankTaskClearsWizard verifies that a non-empty
// task causes the wizard to be cleared (launchWorkflow is invoked). We expect
// launchWorkflow to return an error in the transcript (no live config), but not
// to leave the wizard pending.
func TestAdvanceWorkflowWizard_NonBlankTaskClearsWizard(t *testing.T) {
	m := testModel()
	m.pendingWorkflowWizard = &workflowWizardState{name: "dev", step: wizardStepTask}
	// Provide a minimal st so launchWorkflow doesn't nil-pointer on cfg access.
	m.st = &interactiveState{}

	newM, _ := m.advanceWorkflowWizard("build a REST API")
	nm := newM.(model)

	// Wizard must be cleared regardless of whether launchWorkflow succeeded.
	if nm.pendingWorkflowWizard != nil {
		t.Errorf("expected wizard cleared after non-blank task, step=%d task=%q",
			nm.pendingWorkflowWizard.step, nm.pendingWorkflowWizard.task)
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
