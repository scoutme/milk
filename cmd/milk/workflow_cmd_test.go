package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
	"github.com/scoutme/milk/internal/workflow/interp"
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
// TestAdvanceWorkflowWizard_TaskAdvancesToFirstRole verifies that entering a
// task at wizardStepTask advances to wizardStepGenericRole for the
// definition's first declared role — every fresh launch, "dev" included,
// goes through the generic role wizard now (see
// TestHandleWorkflowCmd_DevLaunchesThroughGenericPath for "dev" specifically).
func TestAdvanceWorkflowWizard_TaskAdvancesToFirstRole(t *testing.T) {
	m := testModel()
	m.pendingWorkflowWizard = &workflowWizardState{
		name: "dev", step: wizardStepTask,
		roles: []string{"designer", "generator", "evaluator"}, roleValues: map[string]string{},
	}

	newM, _ := m.advanceWorkflowWizard("build a REST API")
	nm := newM.(model)

	if nm.pendingWorkflowWizard == nil {
		t.Fatal("expected wizard still pending after task entry (should be at the first role)")
	}
	if nm.pendingWorkflowWizard.step != wizardStepGenericRole {
		t.Errorf("step = %d, want wizardStepGenericRole (%d)", nm.pendingWorkflowWizard.step, wizardStepGenericRole)
	}
	if nm.pendingWorkflowWizard.roleIdx != 0 {
		t.Errorf("roleIdx = %d, want 0 (designer, the first role)", nm.pendingWorkflowWizard.roleIdx)
	}
	if nm.pendingWorkflowWizard.task != "build a REST API" {
		t.Errorf("task = %q, want %q", nm.pendingWorkflowWizard.task, "build a REST API")
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
	path := workflow.StatePath(dir, "test-session", 0)

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
	path := workflow.StatePath(dir, "legacy-session", 0)

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
	// dev/pair/swarm are registered workflows, not hardcoded subcommands —
	// the generic "<name>" form covers them (and any custom workflow in
	// ~/.milk/workflows/) in one variant, alongside the three fixed
	// subcommands that remain reserved words (see #123's design discussion).
	for _, want := range []string{"<name>", "resume", "reconfigure", "clear"} {
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
// TestHandleWorkflowCmd_DevLaunchesThroughGenericPath verifies that /workflow
// dev — like every other name now — routes through handleGenericWorkflowCmd
// and launchGenericWorkflow rather than any dev-specific fresh-launch code
// (that path, launchWorkflow, no longer exists).
func TestHandleWorkflowCmd_DevLaunchesThroughGenericPath(t *testing.T) {
	sandboxMilkHome(t)
	m := testModel()
	m.ctx = context.Background()
	// Zero-value interactiveState (cfg is empty, ActiveAgent().Name == "") but
	// with a real session, since launchGenericWorkflow resolves a workflow ID
	// from sess.ID before ever reaching the async goroutine.
	m.st = &interactiveState{sess: &session.Session{ID: "test-dev-generic-launch-session"}}
	// da.primary and da.escalation are nil, so both primaryName and escalationName
	// are "". AliasPrimary resolves to cfg.ActiveAgent().Name == "" as well,
	// so buildWorkflowRunners takes the matching-primary-name branch (no error).

	m1, _ := m.handleWorkflowCmd("dev build hello world")
	nm1 := m1.(model)
	w := nm1.pendingWorkflowWizard
	if w == nil || w.def.Name != "dev" || len(w.roles) == 0 {
		t.Fatalf("expected a pending generic wizard for the \"dev\" definition, got %+v", w)
	}

	cur := nm1
	for range w.roles {
		mN, _ := cur.advanceWorkflowWizard(workflow.AliasPrimary)
		cur = mN.(model)
	}
	if cur.pendingWorkflowWizard != nil {
		t.Fatalf("expected the wizard to clear after all roles answered, still at %+v", cur.pendingWorkflowWizard)
	}
	if cur.cancelTurn == nil {
		t.Error("expected cancelTurn to be non-nil — was the workflow goroutine launched?")
	}
	if cur.workflowState == nil || cur.workflowState.WorkflowName != "dev" {
		t.Errorf("workflowState = %+v, want WorkflowName %q", cur.workflowState, "dev")
	}
}

// ── Generic (registry-driven, non-"dev") workflows ────────────────────────────

// sandboxMilkHome points config.Dir() (and so workflow.LoadRegistry's user
// override lookup) at a temp dir, so these tests only ever see the built-in
// dev/pair/swarm definitions regardless of what's in the real machine's
// ~/.milk/workflows.
func sandboxMilkHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func TestHandleWorkflowCmd_NoArgsListsAllRegisteredNames(t *testing.T) {
	sandboxMilkHome(t)
	m := testModel()
	newM, _ := m.handleWorkflowCmd("")
	nm := newM.(model)
	out := nm.transcript.String()
	for _, want := range []string{"dev", "pair", "swarm"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q listed in %q", want, out)
		}
	}
}

func TestHandleWorkflowCmd_UnknownWorkflowName(t *testing.T) {
	sandboxMilkHome(t)
	m := testModel()
	newM, _ := m.handleWorkflowCmd("bogus some task")
	nm := newM.(model)
	if !strings.Contains(nm.transcript.String(), "unknown workflow") {
		t.Errorf("expected an unknown-workflow error, got %q", nm.transcript.String())
	}
}

// TestHandleGenericWorkflowCmd_TaskThenRoleWizardFlow verifies /workflow pair
// (no task, no role flags) drives the task step then one wizardStepGenericRole
// step per role in pair's declared order, and clears the wizard once every
// role has an answer (launchGenericWorkflow is invoked, which will error in
// this test due to no live config — same pattern as the dev wizard test).
func TestHandleGenericWorkflowCmd_TaskThenRoleWizardFlow(t *testing.T) {
	sandboxMilkHome(t)
	m := testModel()
	m.ctx = context.Background()
	// A real session is required: unlike the dev-wizard test's "myagent" (an
	// unresolvable name that makes launchWorkflow fail before touching
	// sess.ID), every role here answers blank, which resolves successfully
	// to the built-in claude-cli default — so launchGenericWorkflow proceeds
	// far enough to need a real session.
	m.st = &interactiveState{sess: &session.Session{ID: "test-generic-wizard-session"}}

	m1, _ := m.handleWorkflowCmd("pair")
	nm1 := m1.(model)
	if nm1.pendingWorkflowWizard == nil || nm1.pendingWorkflowWizard.step != wizardStepTask {
		t.Fatalf("expected wizardStepTask pending, got %+v", nm1.pendingWorkflowWizard)
	}
	if nm1.pendingWorkflowWizard.def.Name != "pair" {
		t.Errorf("def.Name = %q, want %q", nm1.pendingWorkflowWizard.def.Name, "pair")
	}

	m2, _ := nm1.advanceWorkflowWizard("build a thing")
	nm2 := m2.(model)
	w2 := nm2.pendingWorkflowWizard
	if w2 == nil || w2.step != wizardStepGenericRole {
		t.Fatalf("after task: want wizardStepGenericRole, got %+v", w2)
	}
	if len(w2.roles) == 0 {
		t.Fatal("expected roles to be populated from the definition")
	}
	if w2.roleIdx != 0 {
		t.Errorf("roleIdx = %d, want 0 (first role)", w2.roleIdx)
	}

	// Answer every declared role in turn; blank defaults to AliasEscalation.
	cur := nm2
	for range w2.roles {
		mN, _ := cur.advanceWorkflowWizard("")
		cur = mN.(model)
	}
	if cur.pendingWorkflowWizard != nil {
		t.Errorf("expected wizard cleared after all %d roles answered, still pending at step=%d",
			len(w2.roles), cur.pendingWorkflowWizard.step)
	}
}

func TestLaunchGenericWorkflow_SetsCancelTurn(t *testing.T) {
	sandboxMilkHome(t)
	reg, errs := workflow.LoadRegistry()
	if len(errs) != 0 {
		t.Fatalf("LoadRegistry errors: %v", errs)
	}
	def, ok := reg.Lookup("swarm")
	if !ok {
		t.Fatal("expected built-in \"swarm\" definition")
	}

	m := testModel()
	m.ctx = context.Background()
	m.st = &interactiveState{sess: &session.Session{ID: "test-launch-generic-workflow-session"}}

	roleValues := make(map[string]string, len(def.Roles))
	for _, role := range def.Roles {
		roleValues[role] = workflow.AliasPrimary
	}
	wizard := &workflowWizardState{
		name:       "swarm",
		task:       "build several things",
		def:        def,
		roles:      def.Roles,
		roleValues: roleValues,
	}

	newM, _ := m.launchGenericWorkflow(wizard)
	nm := newM.(model)

	if nm.cancelTurn == nil {
		t.Error("expected cancelTurn to be non-nil after launchGenericWorkflow; was the workflow goroutine launched?")
	}
	if nm.workflowState == nil || nm.workflowState.WorkflowName != "swarm" {
		t.Errorf("workflowState = %+v, want WorkflowName %q", nm.workflowState, "swarm")
	}
}

// ── Generic (interpreter-driven) resume/clear/reconfigure ────────────────────

func stateDirForTest(t *testing.T) string {
	t.Helper()
	dir, err := session.Dir()
	if err != nil {
		t.Fatalf("session.Dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return dir
}

func TestHandleWorkflowResume_InterpKind_LoadsCheckpointAndLaunches(t *testing.T) {
	sandboxMilkHome(t)
	sessID := "test-resume-session"
	stateDir := stateDirForTest(t)
	path := workflow.InterpCheckpointPath(stateDir, sessID, 0)
	if err := interp.SaveCheckpoint(path, &interp.Checkpoint{
		DefinitionName: "pair",
		Task:           "build a thing",
		AgentMap:       map[string]string{"designer": "primary", "generator": "primary", "evaluator": "primary"},
		Trace:          []interp.TraceEntry{{StageID: "designer", SaveAs: "plan", Value: "the plan"}},
	}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	m := testModel()
	m.ctx = context.Background()
	m.st = &interactiveState{sess: &session.Session{ID: sessID}}

	newM, _ := m.handleWorkflowResume()
	nm := newM.(model)
	if nm.cancelTurn == nil {
		t.Fatal("expected cancelTurn to be set — was the resumed workflow launched?")
	}
	if !strings.Contains(nm.transcript.String(), "resuming workflow pair") {
		t.Errorf("transcript = %q, expected a \"resuming workflow pair\" line", nm.transcript.String())
	}
	if nm.workflowState == nil || nm.workflowState.WorkflowName != "pair" || nm.workflowState.Task != "build a thing" {
		t.Errorf("workflowState = %+v, want WorkflowName=pair Task=%q", nm.workflowState, "build a thing")
	}
}

func TestHandleWorkflowResume_InterpKind_DoneCheckpointReportsCompleted(t *testing.T) {
	sandboxMilkHome(t)
	sessID := "test-resume-done-session"
	stateDir := stateDirForTest(t)
	path := workflow.InterpCheckpointPath(stateDir, sessID, 0)
	if err := interp.SaveCheckpoint(path, &interp.Checkpoint{DefinitionName: "pair", Task: "x", Done: true}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	m := testModel()
	m.st = &interactiveState{sess: &session.Session{ID: sessID}}
	newM, _ := m.handleWorkflowResume()
	nm := newM.(model)
	if nm.cancelTurn != nil {
		t.Error("expected no launch for an already-completed checkpoint")
	}
	if !strings.Contains(nm.transcript.String(), "already completed") {
		t.Errorf("transcript = %q, expected an already-completed message", nm.transcript.String())
	}
}

func TestHandleWorkflowResume_InterpKind_UnregisteredDefinitionErrors(t *testing.T) {
	sandboxMilkHome(t)
	sessID := "test-resume-unregistered-session"
	stateDir := stateDirForTest(t)
	path := workflow.InterpCheckpointPath(stateDir, sessID, 0)
	if err := interp.SaveCheckpoint(path, &interp.Checkpoint{DefinitionName: "no-such-workflow", Task: "x"}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	m := testModel()
	m.st = &interactiveState{sess: &session.Session{ID: sessID}}
	newM, _ := m.handleWorkflowResume()
	nm := newM.(model)
	if nm.cancelTurn != nil {
		t.Error("expected no launch when the checkpoint's definition is no longer registered")
	}
	if !strings.Contains(nm.transcript.String(), "no longer registered") {
		t.Errorf("transcript = %q, expected a \"no longer registered\" error", nm.transcript.String())
	}
}

func TestHandleWorkflowClear_InterpKind_RenamesCheckpointFile(t *testing.T) {
	sandboxMilkHome(t)
	sessID := "test-clear-session"
	stateDir := stateDirForTest(t)
	path := workflow.InterpCheckpointPath(stateDir, sessID, 0)
	if err := interp.SaveCheckpoint(path, &interp.Checkpoint{DefinitionName: "swarm", Task: "x"}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	m := testModel()
	m.st = &interactiveState{sess: &session.Session{ID: sessID}}
	m1, _ := m.handleWorkflowClear()
	nm1 := m1.(model)
	if nm1.pendingWorkflowWizard == nil || nm1.pendingWorkflowWizard.kind != workflow.WorkflowKindInterp {
		t.Fatalf("expected a pending clear-confirm wizard with kind=Interp, got %+v", nm1.pendingWorkflowWizard)
	}

	m2, _ := nm1.advanceWorkflowWizard("clear")
	nm2 := m2.(model)
	if !strings.Contains(nm2.transcript.String(), "workflow state cleared") {
		t.Errorf("transcript = %q, expected confirmation of clearing", nm2.transcript.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected the original checkpoint file to be gone (renamed), stat err = %v", err)
	}
	if _, err := os.Stat(path + ".cleared"); err != nil {
		t.Errorf("expected a %s.cleared file, stat err = %v", path, err)
	}
}

func TestHandleWorkflowReconfigure_InterpKind_WalksRolesAndApplies(t *testing.T) {
	sandboxMilkHome(t)
	sessID := "test-reconfigure-session"
	stateDir := stateDirForTest(t)
	path := workflow.InterpCheckpointPath(stateDir, sessID, 0)
	// "claude" is always resolvable (the built-in claude-cli entry), used here
	// as the pre-existing value every role should keep on blank input.
	if err := interp.SaveCheckpoint(path, &interp.Checkpoint{
		DefinitionName: "swarm",
		Task:           "x",
		AgentMap: map[string]string{
			"designer": "claude", "worker": "claude",
			"evaluator": "claude", "implementer": "claude",
		},
	}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	m := testModel()
	m.st = &interactiveState{
		sess: &session.Session{ID: sessID},
		// A real configured agent so reassigning "worker" to it resolves
		// successfully (an arbitrary made-up name would fail ResolveAgentNames,
		// same as it would for a real user typo).
		cfg: config.Config{Agents: []config.AgentConfig{{Name: "new-worker", Provider: "claude-cli"}}},
	}
	m1, _ := m.handleWorkflowReconfigure()
	nm1 := m1.(model)
	w := nm1.pendingWorkflowWizard
	if w == nil || w.step != wizardStepGenericRole || !w.reconfiguring {
		t.Fatalf("expected a pending reconfigure wizard at the first role, got %+v", w)
	}
	if !strings.Contains(nm1.transcript.String(), `keep "claude"`) {
		t.Errorf("transcript = %q, expected the current agent shown as the default", nm1.transcript.String())
	}

	// Blank on every role keeps its current value except "worker", which we
	// explicitly reassign.
	cur := nm1
	for _, role := range w.roles {
		answer := ""
		if role == "worker" {
			answer = "new-worker"
		}
		mN, _ := cur.advanceWorkflowWizard(answer)
		cur = mN.(model)
	}
	if cur.pendingWorkflowWizard != nil {
		t.Fatalf("expected the wizard to clear after all roles answered, still at %+v", cur.pendingWorkflowWizard)
	}
	if !strings.Contains(cur.transcript.String(), "workflow reconfigured") {
		t.Errorf("transcript = %q, expected a reconfigured confirmation", cur.transcript.String())
	}

	cp, err := interp.LoadCheckpoint(path)
	if err != nil {
		t.Fatalf("LoadCheckpoint: %v", err)
	}
	if cp.AgentMap["designer"] != "claude" {
		t.Errorf("designer = %q, want kept as %q", cp.AgentMap["designer"], "claude")
	}
	if cp.AgentMap["worker"] != "new-worker" {
		t.Errorf("worker = %q, want reassigned to %q", cp.AgentMap["worker"], "new-worker")
	}
}

// ── Generic "continue with doubled passes?" exhaustion flow ──────────────────

func TestWorkflowDoneMsg_GenericExhaustion_OffersContinue(t *testing.T) {
	sandboxMilkHome(t)
	m := testModel()
	m.workflowState = &workflow.State{
		WorkflowName: "pair",
		Task:         "build a thing",
		AgentMap:     map[string]string{"designer": "claude", "generator": "claude", "evaluator": "claude"},
		WorkflowID:   0,
	}
	newM, _ := m.Update(workflow.WorkflowDoneMsg{Err: &interp.ExhaustedError{StageID: "pass_loop", MaxIterations: 3}})
	nm := newM.(model)

	if nm.pendingGenericWorkflowExtend == nil {
		t.Fatal("expected pendingGenericWorkflowExtend to be set")
	}
	if nm.pendingGenericWorkflowExtend.stageID != "pass_loop" || nm.pendingGenericWorkflowExtend.maxIterations != 3 {
		t.Errorf("extend state = %+v, want stageID=pass_loop maxIterations=3", nm.pendingGenericWorkflowExtend)
	}
	if !strings.Contains(nm.transcript.String(), "continue with 6?") {
		t.Errorf("transcript = %q, expected a doubled-iterations prompt", nm.transcript.String())
	}
	if nm.pendingGenericWorkflowExtend.wizard.def.Name != "pair" {
		t.Errorf("wizard.def.Name = %q, want %q", nm.pendingGenericWorkflowExtend.wizard.def.Name, "pair")
	}
}

func TestHandleGenericWorkflowExtendKey_Yes_RelaunchesWithOverride(t *testing.T) {
	sandboxMilkHome(t)
	reg, errs := workflow.LoadRegistry()
	if len(errs) != 0 {
		t.Fatalf("LoadRegistry errors: %v", errs)
	}
	def, _ := reg.Lookup("pair")

	m := testModel()
	m.ctx = context.Background()
	m.ta = textarea.New()
	m.st = &interactiveState{sess: &session.Session{ID: "test-extend-session"}}
	m.pendingGenericWorkflowExtend = &genericWorkflowExtendState{
		wizard: &workflowWizardState{
			name: "pair", task: "x", def: def, roles: def.Roles,
			roleValues: map[string]string{"designer": workflow.AliasPrimary, "generator": workflow.AliasPrimary, "evaluator": workflow.AliasPrimary},
			resuming:   true,
			workflowID: 0,
		},
		stageID:       "pass_loop",
		maxIterations: 3,
	}

	newM, _ := m.handleGenericWorkflowExtendKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	nm := newM.(model)
	if nm.pendingGenericWorkflowExtend != nil {
		t.Error("expected pendingGenericWorkflowExtend to be cleared")
	}
	if nm.cancelTurn == nil {
		t.Error("expected cancelTurn to be set — was the workflow relaunched?")
	}
}

func TestHandleGenericWorkflowExtendKey_No_Halts(t *testing.T) {
	m := testModel()
	m.ta = textarea.New()
	m.pendingGenericWorkflowExtend = &genericWorkflowExtendState{
		wizard:        &workflowWizardState{name: "pair"},
		stageID:       "pass_loop",
		maxIterations: 3,
	}
	newM, _ := m.handleGenericWorkflowExtendKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	nm := newM.(model)
	if nm.pendingGenericWorkflowExtend != nil {
		t.Error("expected pendingGenericWorkflowExtend to be cleared")
	}
	if !strings.Contains(nm.transcript.String(), "halted after 3 iterations") {
		t.Errorf("transcript = %q, expected a halted message", nm.transcript.String())
	}
}
