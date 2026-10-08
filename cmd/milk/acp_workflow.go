package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/livebuf"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/transport/acp"
	"github.com/scoutme/milk/internal/workflow"
)

// Workflows over ACP run synchronously inside the session/prompt that started
// them (via /workflow or the model's start_workflow tool): the prompt stays
// open for the whole run, which is what ACP expects of a long turn, and
// session/cancel cancels the run like any other turn. Progress goes out as
// plan updates; the engine itself (internal/workflow/interp) has no TUI
// dependency beyond the tea.Msg type its Send callback carries.

// acpDenyInput answers any interactive confirmation a CLI-agent workflow role
// asks for with "no": there is no human on the other end of a workflow step.
type acpDenyInput struct{}

func (acpDenyInput) readLine(string) (string, error) { return "n", nil }

func acpWorkflowCmd(t *acpTurn, rest string) (string, string) {
	as := t.as
	parts := strings.Fields(rest)
	reg, _ := workflow.LoadRegistry() //nolint:errcheck // load errors are per-file; usable definitions are still returned
	if len(parts) == 0 {
		return "available workflows: " + strings.Join(reg.Names(), ", ") +
			"\nusage: /workflow <name> <task> [--<role> <agent> ...] | resume | status | clear", ""
	}
	switch parts[0] {
	case "resume":
		return as.resumeWorkflow(t), ""
	case "status":
		return as.workflowStatus(), ""
	case "clear":
		return as.clearWorkflow(), ""
	}
	def, ok := reg.Lookup(parts[0])
	if !ok {
		return fmt.Sprintf("unknown workflow %q — available: %s, resume, status, clear", parts[0], strings.Join(reg.Names(), ", ")), ""
	}
	remaining, flags, err := parseWorkflowFlags(parts[1:])
	if err != nil {
		return "workflow error: " + err.Error(), ""
	}
	task := strings.Join(remaining, " ")
	if task == "" {
		return fmt.Sprintf("usage: /workflow %s <task> [--<role> <agent> ...]  (roles: %s)", def.Name, strings.Join(def.Roles, ", ")), ""
	}
	return as.runWorkflow(t, def, task, flags, false, 0), ""
}

// startWorkflowFromTool handles the model's start_workflow tool call, which
// the local agent surfaces as a signal once its turn is over.
func (as *acpSession) startWorkflowFromTool(t *acpTurn, ws *local.WorkflowStartSignal) {
	reg, _ := workflow.LoadRegistry() //nolint:errcheck // see acpWorkflowCmd
	def, ok := reg.Lookup(ws.Name)
	if !ok {
		t.say(fmt.Sprintf("Workflow %q is no longer registered — not starting.", ws.Name))
		return
	}
	t.say(as.runWorkflow(t, def, ws.Task, ws.Roles, false, 0))
}

// runWorkflow runs def to completion (or cancellation) and returns a one-line
// outcome for the client. Unspecified roles default to the escalation agent.
func (as *acpSession) runWorkflow(t *acpTurn, def workflow.Definition, task string, roleValues map[string]string, resuming bool, resumeID int) string {
	if !as.workflowRunning.CompareAndSwap(false, true) {
		return "a workflow is already running in this session — wait for it, or cancel the current prompt"
	}
	defer as.workflowRunning.Store(false)

	plan, err := planWorkflow(as.cfg, as.sess, def, task, defaultRoleValues(def, roleValues), resuming, resumeID)
	if err != nil {
		return "workflow error: " + err.Error()
	}
	runners, err := buildWorkflowRunners(plan.AgentNames, as.cfg, as.sess, as.mem, as.da, as.cliPC,
		func() inputReader { return acpDenyInput{} }, as.notifier(), nil)
	if err != nil {
		return "workflow error: " + err.Error()
	}
	id, agentNames := plan.ID, plan.AgentNames

	state := plan.initialState()
	var stateMu sync.Mutex
	live := livebuf.New(0)
	lastRole := ""
	publishPlan := func(cancelled bool) {
		stateMu.Lock()
		entries := acp.PlanEntriesForState(state, cancelled)
		stateMu.Unlock()
		as.notifyPlan(acp.PlanIDForWorkflow(state), entries)
	}

	send := func(msg tea.Msg) {
		switch m := msg.(type) {
		case workflow.ProgressMsg:
			stateMu.Lock()
			state.ApplyProgress(m)
			if m.Role != "" && m.Role != lastRole {
				lastRole = m.Role
				sep := "\n\n"
				if live.Len() == 0 {
					sep = ""
				}
				live.Append([]byte(sep + "── " + m.Role + " ──\n"))
			}
			if m.ActivePaths != nil {
				state.ActiveStageTree = m.ActivePaths.Root
			}
			if m.CompletedPaths != nil {
				state.CompletedStageTree = m.CompletedPaths.Root
			}
			stateMu.Unlock()
			publishPlan(false)
		case workflow.WorkflowChunkMsg:
			live.Append([]byte(m.Text))
		case workflow.WorkflowQuestionsMsg:
			// A workflow step is asking the user something, but the user can't
			// answer while this prompt is open. Show the question and accept
			// the designer's defaults — "continue" is the TUI's own
			// bare-Enter answer. The send is async: the engine only starts
			// waiting on the channel after this callback returns.
			t.say("The workflow asked:\n\n" + strings.TrimSpace(m.Questions) +
				"\n\nContinuing with the defaults; put specifics in the task text to steer it.")
			go func() { m.AnswersCh <- "continue" }()
		}
	}

	callID := acp.ToolCallID(fmt.Sprintf("workflow:%d", id))
	roleSummary := make([]string, 0, len(def.Roles))
	for _, role := range def.Roles {
		roleSummary = append(roleSummary, role+"="+agentNames[role])
	}
	as.notify(acp.ToolCallUpdate{
		SessionUpdate: "tool_call_update", ToolCallID: callID, Name: "workflow",
		Title: "workflow " + def.Name, Kind: acp.ToolKindOther, Status: acp.ToolCallInProgress,
		RawInput: map[string]any{"workflow": def.Name, "task": task, "roles": agentNames},
	})
	stopStream := as.streamLive(callID, live)
	verb := "Starting"
	if resuming {
		verb = "Resuming"
	}
	t.say(fmt.Sprintf("%s workflow %s #%d (%s).", verb, def.Name, id, strings.Join(roleSummary, ", ")))
	publishPlan(false)

	r, runCfg := plan.newRunner(as.sess, task, runners, send, make(chan string, 1))
	runErr := r.Run(t.ctx, runCfg)

	stopStream() // flush stage output before the terminal status row
	finish := func(status acp.ToolCallStatus, out string) {
		as.notify(acp.ToolCallUpdate{SessionUpdate: "tool_call_update", ToolCallID: callID, Status: status, RawOutput: out})
	}
	switch {
	case runErr == nil:
		publishPlan(false)
		finish(acp.ToolCallCompleted, "complete")
		return fmt.Sprintf("Workflow %s #%d complete.", def.Name, id)
	case t.ctx.Err() != nil || errors.Is(runErr, context.Canceled):
		publishPlan(true)
		finish(acp.ToolCallFailed, "cancelled")
		return fmt.Sprintf("Workflow %s #%d cancelled. /workflow resume continues from the last checkpoint.", def.Name, id)
	default:
		publishPlan(true)
		finish(acp.ToolCallFailed, runErr.Error())
		return fmt.Sprintf("Workflow %s #%d stopped: %v\n/workflow resume retries from the last checkpoint.", def.Name, id, runErr)
	}
}

func (as *acpSession) resumeWorkflow(t *acpTurn) string {
	saved, err := loadSavedWorkflow(as.sess)
	switch {
	case errors.Is(err, errNoSavedWorkflow):
		return "no saved workflow state for this session"
	case errors.Is(err, errLegacyWorkflow):
		return "workflow resume: legacy dev-format checkpoint detected — /workflow clear, then start a fresh workflow"
	case errors.Is(err, errWorkflowDone):
		return "workflow already completed — /workflow clear removes it"
	case err != nil:
		return "workflow resume error: " + err.Error()
	}
	return as.runWorkflow(t, saved.Def, saved.Checkpoint.Task, saved.RoleValues(), true, saved.ID)
}

func (as *acpSession) workflowStatus() string {
	if as.workflowRunning.Load() {
		return "a workflow is running in this session"
	}
	saved, err := loadSavedWorkflow(as.sess)
	switch {
	case errors.Is(err, errNoSavedWorkflow):
		return "no saved workflow state for this session"
	case errors.Is(err, errLegacyWorkflow):
		return "legacy dev-format workflow checkpoint — /workflow clear to remove"
	case errors.Is(err, errWorkflowDone):
		return "the last workflow is complete — /workflow clear removes its state"
	case err != nil:
		return "workflow status error: " + err.Error()
	}
	return fmt.Sprintf("workflow %s #%d: paused — /workflow resume continues it\ntask: %s", saved.Def.Name, saved.ID, saved.Checkpoint.Task)
}

func (as *acpSession) clearWorkflow() string {
	if as.workflowRunning.Load() {
		return "a workflow is running — cancel the current prompt first"
	}
	stateDir, err := session.Dir()
	if err != nil {
		return "workflow clear error: cannot determine state dir: " + err.Error()
	}
	id, kind, err := workflow.CurrentWorkflowID(stateDir, as.sess.ID)
	if err != nil {
		return "workflow clear error: " + err.Error()
	}
	if kind == workflow.WorkflowKindNone {
		return "no saved workflow state for this session"
	}
	path, err := clearSavedWorkflow(stateDir, as.sess.ID, id, kind)
	if err != nil {
		return "workflow clear error: " + err.Error()
	}
	return "workflow state cleared (renamed to " + path + ".cleared; plan/findings files are untouched)"
}
