package main

import (
	"errors"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
	"github.com/scoutme/milk/internal/workflow/interp"
)

// The host-independent half of launching, resuming and clearing a workflow,
// shared by the TUI (workflow_cmd.go) and the ACP server (acp_workflow.go).
// The hosts differ in how they gather role choices, where they show progress
// and whether the run is a tea.Cmd or a blocking call — not in any of this.

// workflowPlan is a resolved launch: which definition, which agent plays each
// role, and which state ID the run checkpoints under.
type workflowPlan struct {
	Def        workflow.Definition
	Task       string // as the user gave it (shown in state); the interpreter may get an augmented task
	AgentNames map[string]string
	StateDir   string
	ID         int
	Resuming   bool
}

// planWorkflow resolves roleValues (role -> agent name or alias) against cfg
// and allocates a workflow ID, or reuses resumeID when resuming. Errors carry
// no "workflow error" prefix; callers add their own.
func planWorkflow(cfg config.Config, sess *session.Session, def workflow.Definition, task string, roleValues map[string]string, resuming bool, resumeID int) (workflowPlan, error) {
	agentNames, err := workflow.ResolveAgentNames(roleValues, cfg)
	if err != nil {
		return workflowPlan{}, err
	}
	stateDir, err := session.Dir()
	if err != nil {
		return workflowPlan{}, fmt.Errorf("cannot determine state dir: %w", err)
	}
	id := resumeID
	if !resuming {
		if id, err = workflow.NextWorkflowID(stateDir, sess.ID); err != nil {
			return workflowPlan{}, fmt.Errorf("cannot determine workflow ID: %w", err)
		}
	}
	return workflowPlan{Def: def, Task: task, AgentNames: agentNames, StateDir: stateDir, ID: id, Resuming: resuming}, nil
}

// defaultRoleValues fills every role of def that given leaves unset with the
// escalation alias.
func defaultRoleValues(def workflow.Definition, given map[string]string) map[string]string {
	out := make(map[string]string, len(def.Roles))
	for _, role := range def.Roles {
		if v := given[role]; v != "" {
			out[role] = v
		} else {
			out[role] = workflow.AliasEscalation
		}
	}
	return out
}

// initialState is the workflow's state at launch: the full stage tree, no
// progress yet.
func (p workflowPlan) initialState() *workflow.State {
	return &workflow.State{
		WorkflowName: p.Def.Name,
		Task:         p.Task,
		Role:         "starting",
		AgentMap:     p.AgentNames,
		WorkflowID:   p.ID,
		StageTree:    definitionStageTree(p.Def.Stages),
		Generic:      true,
		ParallelExec: p.Def.HasParallelGroup(),
	}
}

// newRunner builds the interpreter and its RunConfig. runTask is the task text
// handed to the interpreter (the TUI appends attachment references to it).
// The caller may chain further options on the returned Runner.
func (p workflowPlan) newRunner(sess *session.Session, runTask string, runners map[string]workflow.TurnRunner, send func(tea.Msg), answersCh chan string) (*interp.Runner, workflow.RunConfig) {
	r := interp.New(p.Def, runTask).
		WithCheckpoint(workflow.InterpCheckpointPath(p.StateDir, sess.ID, p.ID)).
		WithAgentMap(p.AgentNames)
	return r, workflow.RunConfig{
		Session:    sess,
		Runners:    runners,
		Send:       send,
		StateDir:   p.StateDir,
		AnswersCh:  answersCh,
		WorkflowID: p.ID,
	}
}

// Sentinel outcomes of loadSavedWorkflow that hosts word in their own voice.
var (
	errNoSavedWorkflow = errors.New("no saved workflow state for this session")
	errLegacyWorkflow  = errors.New("legacy dev-format checkpoint")
	errWorkflowDone    = errors.New("workflow already completed")
)

// savedWorkflow is a session's resumable (interpreter-driven, not yet done)
// workflow.
type savedWorkflow struct {
	ID         int
	StateDir   string
	Def        workflow.Definition
	Checkpoint *interp.Checkpoint
}

// RoleValues are the agent choices to resume with: the checkpoint's own map,
// or the escalation agent for every role when an older checkpoint has none.
func (s savedWorkflow) RoleValues() map[string]string {
	if len(s.Checkpoint.AgentMap) > 0 {
		return s.Checkpoint.AgentMap
	}
	return defaultRoleValues(s.Def, nil)
}

// loadSavedWorkflow finds the session's saved workflow. It returns
// errNoSavedWorkflow, errLegacyWorkflow or errWorkflowDone for the three
// expected non-resumable cases, and a descriptive error otherwise.
func loadSavedWorkflow(sess *session.Session) (savedWorkflow, error) {
	stateDir, err := session.Dir()
	if err != nil {
		return savedWorkflow{}, fmt.Errorf("cannot determine state dir: %w", err)
	}
	id, kind, err := workflow.CurrentWorkflowID(stateDir, sess.ID)
	if err != nil {
		return savedWorkflow{}, err
	}
	switch kind {
	case workflow.WorkflowKindNone:
		return savedWorkflow{}, errNoSavedWorkflow
	case workflow.WorkflowKindInterp:
	default:
		return savedWorkflow{}, errLegacyWorkflow
	}
	cp, err := interp.LoadCheckpoint(workflow.InterpCheckpointPath(stateDir, sess.ID, id))
	if err != nil {
		return savedWorkflow{}, err
	}
	if cp == nil || cp.Done {
		return savedWorkflow{}, errWorkflowDone
	}
	reg, regErrs := workflow.LoadRegistry()
	for _, e := range regErrs {
		obs.Info("workflow.registry.load_error", "error", e.Error())
	}
	def, ok := reg.Lookup(cp.DefinitionName)
	if !ok {
		return savedWorkflow{}, fmt.Errorf("workflow %q is no longer registered", cp.DefinitionName)
	}
	return savedWorkflow{ID: id, StateDir: stateDir, Def: def, Checkpoint: cp}, nil
}

// clearSavedWorkflow renames the workflow's state file out of the way (adding
// ".cleared") instead of deleting it, so a checkpoint stays inspectable and
// the ID is never reused. It returns the original path.
func clearSavedWorkflow(stateDir, sessionID string, id int, kind workflow.WorkflowKind) (string, error) {
	path := workflow.StatePath(stateDir, sessionID, id)
	if kind == workflow.WorkflowKindInterp {
		path = workflow.InterpCheckpointPath(stateDir, sessionID, id)
	}
	if err := os.Rename(path, path+".cleared"); err != nil && !os.IsNotExist(err) {
		return path, err
	}
	return path, nil
}
