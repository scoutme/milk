package main

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
)

func runningWorkflowModel(t *testing.T) (model, *int) {
	t.Helper()
	sandboxMilkHome(t)
	m := testModel()
	m.st = &interactiveState{sess: &session.Session{ID: "s"}}
	m.workflowRunning = true
	m.workflowGen = 1
	m.workflowState = &workflow.State{WorkflowName: "swarm", Role: "designer"}
	cancels := 0
	m.cancelWorkflow = func() { cancels++ }
	return m, &cancels
}

func TestLaunchGenericWorkflow_RunsInBackground(t *testing.T) {
	sandboxMilkHome(t)
	reg, _ := workflow.LoadRegistry()
	def, _ := reg.Lookup("swarm")
	m := testModel()
	m.ctx = context.Background()
	m.st = &interactiveState{sess: &session.Session{ID: "bg-launch"}}
	roles := map[string]string{}
	for _, r := range def.Roles {
		roles[r] = workflow.AliasPrimary
	}
	newM, _ := m.launchGenericWorkflow(&workflowWizardState{name: "swarm", task: "t", def: def, roles: def.Roles, roleValues: roles})
	nm := newM.(model)
	if !nm.workflowRunning || nm.cancelWorkflow == nil {
		t.Fatal("workflow should be running with its own cancel handle")
	}
	if nm.busy || nm.cancelTurn != nil || nm.inputLocked() {
		t.Errorf("a background workflow must not occupy the turn state: busy=%v cancelTurn set=%v", nm.busy, nm.cancelTurn != nil)
	}
}

func TestWorkflowDone_LeavesConcurrentTurnAlone(t *testing.T) {
	m, _ := runningWorkflowModel(t)
	turnCancelled := false
	m.busy = true
	m.cancelTurn = func() { turnCancelled = true }
	m.currentTurnChars = 42

	newM, _ := m.Update(workflow.WorkflowDoneMsg{})
	nm := newM.(model)

	if nm.workflowRunning || nm.cancelWorkflow != nil {
		t.Error("workflow state should be cleared")
	}
	if !nm.busy || nm.cancelTurn == nil || turnCancelled || nm.currentTurnChars != 42 {
		t.Errorf("the in-flight turn's state was clobbered: busy=%v cancelTurn=%v chars=%d", nm.busy, nm.cancelTurn != nil, nm.currentTurnChars)
	}
	if n := nm.st.sess.PendingNotices; len(n) != 1 || !strings.Contains(n[0], "swarm") {
		t.Errorf("completion notice should be queued, got %v", n)
	}
}

func TestCtrlC_WhileTurnBusy_CancelsTurnNotWorkflow(t *testing.T) {
	m, cancels := runningWorkflowModel(t)
	turnCancelled := false
	m.busy = true
	m.cancelTurn = func() { turnCancelled = true }

	newM, _ := m.handleBusyKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	nm := newM.(model)
	if !turnCancelled || *cancels != 0 || !nm.workflowRunning {
		t.Errorf("Ctrl+C mid-turn must cancel only the turn: turn=%v workflowCancels=%d running=%v", turnCancelled, *cancels, nm.workflowRunning)
	}
}

func TestCtrlC_Idle_WithWorkflow_NeedsConfirmationToExit(t *testing.T) {
	m, cancels := runningWorkflowModel(t)
	newM, cmd := m.handleCtrlC()
	nm := newM.(model)
	if !nm.quitPending || *cancels != 0 || cmd == nil {
		t.Fatalf("first Ctrl+C must only arm the quit confirmation: quitPending=%v cancels=%d", nm.quitPending, *cancels)
	}
	nm.width = 400
	if !strings.Contains(nm.statusBar(), "workflow running") {
		t.Errorf("the confirmation should say a workflow is running, got %q", nm.statusBar())
	}
}

func TestWorkflowCancel_WorksWhileTurnBusy(t *testing.T) {
	m, cancels := runningWorkflowModel(t)
	m.busy = true
	m.cancelTurn = func() { t.Error("/workflow cancel must not cancel the turn") }
	if !busySafeWorkflowCmd(cmdWorkflow, " cancel ") || busySafeWorkflowCmd(cmdWorkflow, "resume") || busySafeWorkflowCmd("/new", "cancel") {
		t.Fatal("only /workflow status and /workflow cancel are busy-safe")
	}
	newM, _ := m.handleWorkflowCmd("cancel")
	nm := newM.(model)
	if *cancels != 1 || !nm.workflowCancelled || !nm.busy {
		t.Errorf("cancels=%d workflowCancelled=%v busy=%v", *cancels, nm.workflowCancelled, nm.busy)
	}

	done, _ := nm.Update(workflow.WorkflowDoneMsg{Err: context.Canceled})
	if !strings.Contains(done.(model).transcript.String(), "workflow interrupted") {
		t.Errorf("done should report the interruption, got %q", done.(model).transcript.String())
	}
}

func TestWorkflowCmd_RefusesMutatingSubcommandsWhileRunning(t *testing.T) {
	for _, arg := range []string{"resume", "clear", "reconfigure", "swarm do things"} {
		m, cancels := runningWorkflowModel(t)
		newM, _ := m.handleWorkflowCmd(arg)
		nm := newM.(model)
		if nm.pendingWorkflowWizard != nil || *cancels != 0 || !strings.Contains(nm.transcript.String(), "a workflow is running") {
			t.Errorf("/workflow %s must be refused while running; transcript=%q", arg, nm.transcript.String())
		}
	}
	m, _ := runningWorkflowModel(t)
	newM, _ := m.handleWorkflowCmd("status")
	if got := newM.(model).transcript.String(); !strings.Contains(got, "workflow swarm running") {
		t.Errorf("status should describe the running workflow, got %q", got)
	}
}

func TestSessionSwitching_RefusedWhileWorkflowRuns(t *testing.T) {
	for _, cmd := range []string{cmdNew, cmdClear, cmdDrop, cmdResume} {
		m, _ := runningWorkflowModel(t)
		newM, _ := m.handleSlashInput(cmd, "")
		nm := newM.(model)
		if nm.st.sess.ID != "s" || !strings.Contains(nm.transcript.String(), "a workflow is running") {
			t.Errorf("%s must be refused while a workflow runs; transcript=%q", cmd, nm.transcript.String())
		}
	}
}

func TestDesignerAnswer_RoutesEvenWhileTurnBusy_AndStaysBackground(t *testing.T) {
	m, _ := runningWorkflowModel(t)
	answers := make(chan string, 1)
	m.pendingWorkflowQuestions = "Q1?"
	m.workflowAnswersCh = answers
	m.busy = true
	m.cancelTurn = func() {}
	m.ta = buildTextarea()
	m.ta.SetValue("use defaults")

	newM, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	nm := newM.(model)

	select {
	case got := <-answers:
		if got != "use defaults" {
			t.Errorf("answer = %q", got)
		}
	default:
		t.Fatal("Enter with pending questions must reach the workflow even mid-turn")
	}
	if nm.pendingWorkflowQuestions != "" || !nm.busy || nm.cancelTurn == nil {
		t.Errorf("answering must neither re-lock input nor touch the turn: pending=%q busy=%v", nm.pendingWorkflowQuestions, nm.busy)
	}
}

func TestDesignerQuestions_DoNotUnlockOrLockInput(t *testing.T) {
	m, _ := runningWorkflowModel(t)
	m.busy = true // a turn is running
	newM, _ := m.Update(workflow.WorkflowQuestionsMsg{Questions: "Q?", AnswersCh: make(chan string, 1)})
	nm := newM.(model)
	if !nm.busy {
		t.Error("questions arriving must not clear a running turn's busy flag")
	}
	if nm.pendingWorkflowQuestions == "" || nm.workflowState.Role != "waiting for your answers" {
		t.Errorf("questions not registered: %+v", nm.workflowState)
	}
}

func TestAttachWorkflowView_NotesParallelExecution(t *testing.T) {
	for _, parallel := range []bool{true, false} {
		m := attachTestModel()
		m.workflowState = &workflow.State{WorkflowName: "swarm", ParallelExec: parallel}
		m.startAttach(attachWorkflow, "", "workflow swarm", m.workflowState.LiveBuffer())
		content := m.attached.vp.View()
		if got := strings.Contains(content, "parallel workflow"); got != parallel {
			t.Errorf("ParallelExec=%v: note shown=%v, view=%q", parallel, got, content)
		}
	}
}

func TestInitialState_FlagsParallelWorkflows(t *testing.T) {
	sandboxMilkHome(t)
	reg, _ := workflow.LoadRegistry()
	for name, want := range map[string]bool{"swarm": true, "pair": false} {
		def, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("missing built-in %q", name)
		}
		if got := (workflowPlan{Def: def}).initialState().ParallelExec; got != want {
			t.Errorf("%s: ParallelExec=%v, want %v", name, got, want)
		}
	}
}
