package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
	wfdev "github.com/scoutme/milk/internal/workflow/dev"
	"github.com/scoutme/milk/internal/workflow/interp"
)

// workflowWizardState tracks multi-step wizard input for /workflow. Every
// fresh launch — "dev" included — goes through the generic (roles/
// roleValues/roleDefaults) fields below via handleGenericWorkflowCmd. The
// designer/generator/evaluator fields and their fixed wizard steps
// (wizardStepDesigner/Generator/Evaluator) survive only to resume or
// reconfigure a pre-existing dev.go-format checkpoint (workflow.State,
// predating this migration, or one missing AgentMap) — see
// handleWorkflowResume/handleWorkflowReconfigure's dev-specific branches.
//
// There is deliberately no inline behaviour-prompt-override step (dev.go's
// wizard used to have one, applied as a session-local AgentConfig.Prompt
// override) — a task description, and the plan the designer produces from
// it, can already carry any specific behaviour request into the generator/
// evaluator prompts naturally; a separate runtime override mechanism was
// redundant with that, and didn't fit the generic (arbitrary-role)
// definitions at all. A persistent AgentConfig.Prompt in ~/.milk/config.json
// remains the way to give an agent a standing behaviour override.
type workflowWizardState struct {
	name          string // workflow name (e.g. "dev")
	task          string
	designer      string
	generator     string
	evaluator     string
	step          workflowWizardStep
	clearing      bool                  // true when this wizard is a /workflow clear confirmation
	resuming      bool                  // true when completing the wizard should resume rather than start fresh
	reconfiguring bool                  // true when completing the wizard should update state agent map only
	sprint        int                   // checkpoint sprint (used when resuming/reconfiguring == true)
	pass          int                   // checkpoint pass (used when resuming/reconfiguring == true)
	role          string                // checkpoint role (used when resuming/reconfiguring == true)
	workflowID    int                   // resolved workflow ID (see workflow.CurrentWorkflowID/NextWorkflowID); used by clear/reconfigure/resume
	kind          workflow.WorkflowKind // dev vs. interp-driven; used by the clear-confirmation path

	// Generic (registry-driven) workflow fields — every definition, including
	// the built-in "dev", launches through these via internal/workflow/interp.
	// /workflow resume, clear, and reconfigure all work for this path (see
	// handleGenericWorkflowResume/Reconfigure, interp.Runner.WithCheckpoint).
	def          workflow.Definition // resolved definition; zero value unused when name == "dev"
	roles        []string            // def.Roles, in wizard-ask order
	roleValues   map[string]string   // answers collected so far this wizard pass, role -> agent specifier
	roleDefaults map[string]string   // current agent per role when reconfiguring (shown as the "blank = keep X" default); nil for a fresh launch
	roleIdx      int                 // index into roles for the role currently being asked
	// maxIterOverrideStageID/N: when set, launchGenericWorkflow applies
	// interp.Runner.WithMaxIterationsOverride — used by the "continue with
	// doubled passes?" recovery flow after a genericWorkflowExtendState.
	maxIterOverrideStageID string
	maxIterOverrideN       int
}

type workflowWizardStep int

const (
	wizardStepTask         workflowWizardStep = iota // ask for task description
	wizardStepDesigner                               // ask for designer agent
	wizardStepGenerator                              // ask for generator agent
	wizardStepEvaluator                              // ask for evaluator agent
	wizardStepClearConfirm                           // ask user to type "clear" to confirm
	wizardStepGenericRole                            // ask for the role at roles[roleIdx] (non-"dev" workflows)
	wizardStepDone
)

// handleWorkflowCmd dispatches /workflow [name] [args...].
func (m model) handleWorkflowCmd(args string) (tea.Model, tea.Cmd) {
	parts := strings.Fields(args)

	reg, regErrs := workflow.LoadRegistry()
	for _, e := range regErrs {
		obs.Info("workflow.registry.load_error", "error", e.Error())
	}

	if len(parts) == 0 {
		// List available workflows.
		m.appendTranscript(milkTag() + " available workflows:\n  " + strings.Join(reg.Names(), ", ") + "\n\nUsage:\n  /workflow <name> [task] [--<role> <agent> ...]\n  /workflow resume       — resume workflow from last checkpoint\n  /workflow reconfigure  — reassign agent roles without losing saved state\n  /workflow clear        — delete saved state for this session\n")
		return m, nil
	}

	name := parts[0]
	if name == "resume" {
		return m.handleWorkflowResume()
	}
	if name == "clear" {
		return m.handleWorkflowClear()
	}
	if name == "reconfigure" {
		return m.handleWorkflowReconfigure()
	}
	// Every workflow, including the built-in "dev", launches through the
	// registry + interpreter now — there is no more dev-specific fresh-launch
	// path. The fixed designer/generator/evaluator wizard steps below remain
	// only for resuming a pre-existing dev.go-format checkpoint (an old state
	// file predating AgentMap support) and for /workflow reconfigure against
	// one — both read an on-disk file that already fixes dev's shape, so
	// there's nothing to generalize there.
	def, ok := reg.Lookup(name)
	if !ok {
		m.appendTranscript(milkTag() + fmt.Sprintf(" unknown workflow %q — available: %s, resume, reconfigure, clear\n", name, strings.Join(reg.Names(), ", ")))
		return m, nil
	}
	return m.handleGenericWorkflowCmd(name, def, parts[1:])
}

// handleGenericWorkflowCmd dispatches /workflow <name> [args...] for any
// registered definition, including the built-in "dev" (e.g. "pair", "swarm",
// or a user-defined workflow in ~/.milk/workflows/). Drives an agent wizard
// over def.Roles (an arbitrary list — dev's designer/generator/evaluator
// triple is just what dev.yaml happens to declare, not special-cased here)
// before launching via internal/workflow/interp.
func (m model) handleGenericWorkflowCmd(name string, def workflow.Definition, rest []string) (tea.Model, tea.Cmd) {
	remaining, flags, flagErr := parseWorkflowFlags(rest)
	if flagErr != nil {
		m.appendTranscript(milkTag() + " workflow error: " + flagErr.Error() + "\n")
		return m, nil
	}
	task := strings.Join(remaining, " ")

	wizard := &workflowWizardState{
		name:       name,
		task:       task,
		def:        def,
		roles:      def.Roles,
		roleValues: map[string]string{},
	}
	for _, role := range def.Roles {
		if v, ok := flags[role]; ok {
			wizard.roleValues[role] = v
		}
	}

	if wizard.task == "" {
		wizard.step = wizardStepTask
		m.pendingWorkflowWizard = wizard
		m.appendTranscript(milkTag() + fmt.Sprintf(" workflow %s — enter task description:\n", name))
		m.refreshPrompt()
		return m, nil
	}
	return m.advanceToNextGenericRoleOrLaunch(wizard)
}

// advanceToNextGenericRoleOrLaunch finds the first role in w.roles not yet in
// w.roleValues and prompts for it, or launches the workflow once every role
// has a value.
func (m model) advanceToNextGenericRoleOrLaunch(w *workflowWizardState) (tea.Model, tea.Cmd) {
	for i, role := range w.roles {
		if _, ok := w.roleValues[role]; ok {
			continue
		}
		w.roleIdx = i
		w.step = wizardStepGenericRole
		m.pendingWorkflowWizard = w
		if w.reconfiguring {
			m.appendTranscript(milkTag() + genericWorkflowAgentReconfigurePrompt(w.name, role, w.roleDefaults[role]))
		} else {
			m.appendTranscript(milkTag() + genericWorkflowAgentPrompt(w.name, role))
		}
		m.refreshPrompt()
		return m, nil
	}
	m.pendingWorkflowWizard = nil
	if w.reconfiguring {
		return m.applyGenericWorkflowReconfigure(w)
	}
	return m.launchGenericWorkflow(w)
}

// genericWorkflowAgentPrompt returns the wizard prompt line for a role in a
// non-"dev" workflow.
func genericWorkflowAgentPrompt(workflowName, role string) string {
	return fmt.Sprintf(" workflow %s — %s agent (blank = escalation):\n", workflowName, role)
}

// genericWorkflowAgentReconfigurePrompt is genericWorkflowAgentPrompt's
// reconfigure-mode counterpart, showing the role's current agent as the
// default kept on blank input — mirrors workflowAgentReconfigurePrompt (dev's
// fixed-triple equivalent).
func genericWorkflowAgentReconfigurePrompt(workflowName, role, current string) string {
	if current == "" {
		return fmt.Sprintf(" workflow %s reconfigure — %s agent (blank = escalation):\n", workflowName, role)
	}
	return fmt.Sprintf(" workflow %s reconfigure — %s agent (blank = keep %q):\n", workflowName, role, current)
}

// advanceWorkflowWizard handles a user input while a workflow wizard is active.
// Each call records the answer for the current step, then either asks the next
// question or launches the workflow when all fields are collected.
func (m model) advanceWorkflowWizard(input string) (tea.Model, tea.Cmd) {
	w := m.pendingWorkflowWizard

	switch w.step {
	case wizardStepClearConfirm:
		if input != "clear" {
			m.pendingWorkflowWizard = nil
			m.appendTranscript(milkTag() + " workflow clear cancelled\n")
			m.refreshPrompt()
			return m, nil
		}
		m.pendingWorkflowWizard = nil
		return m.execWorkflowClear(w.workflowID, w.kind)

	case wizardStepTask:
		if input == "" {
			m.appendTranscript(milkTag() + " task description cannot be empty — enter task description:\n")
			m.refreshPrompt()
			return m, nil
		}
		w.task = input
		return m.advanceToNextGenericRoleOrLaunch(w)

	case wizardStepGenericRole:
		role := w.roles[w.roleIdx]
		w.roleValues[role] = workflowAgentInputWithDefault(input, w.roleDefaults[role], w.reconfiguring)
		return m.advanceToNextGenericRoleOrLaunch(w)

	case wizardStepDesigner:
		w.designer = workflowAgentInputWithDefault(input, w.designer, w.reconfiguring)
		w.step = wizardStepGenerator
		m.pendingWorkflowWizard = w
		if w.reconfiguring {
			m.appendTranscript(milkTag() + workflowAgentReconfigurePrompt("generator", w.generator))
		} else {
			m.appendTranscript(milkTag() + workflowAgentPrompt("generator"))
		}
		m.refreshPrompt()
		return m, nil

	case wizardStepGenerator:
		w.generator = workflowAgentInputWithDefault(input, w.generator, w.reconfiguring)
		w.step = wizardStepEvaluator
		m.pendingWorkflowWizard = w
		if w.reconfiguring {
			m.appendTranscript(milkTag() + workflowAgentReconfigurePrompt("evaluator", w.evaluator))
		} else {
			m.appendTranscript(milkTag() + workflowAgentPrompt("evaluator"))
		}
		m.refreshPrompt()
		return m, nil

	case wizardStepEvaluator:
		w.evaluator = workflowAgentInputWithDefault(input, w.evaluator, w.reconfiguring)
		w.step = wizardStepDone

		// Record the fully-assembled command in history so the user can
		// recall and re-run it without stepping through the wizard again.
		full := "/workflow dev " + w.task
		m.sessionHistory = appendDeduped(m.sessionHistory, full, maxPersistedHistory)
		m.globalHistory = appendDeduped(m.globalHistory, full, maxPersistedHistory)
	}

	// Reaching here means completing the fixed designer/generator/evaluator
	// steps for dev.go's own wizard shape — only reachable via
	// handleWorkflowResume's pre-AgentMap fallback (resuming) or
	// handleWorkflowReconfigure (reconfiguring); every fresh launch, dev
	// included, goes through the generic role wizard above instead.
	m.pendingWorkflowWizard = nil
	if w.reconfiguring {
		return m.applyWorkflowReconfigure(w)
	}
	return m.launchWorkflowResume(w, w.sprint, w.pass, 0, w.role)
}

// handleWorkflowClear starts the confirmation wizard for /workflow clear.
func (m model) handleWorkflowClear() (tea.Model, tea.Cmd) {
	sess := m.st.sess
	stateDir, err := session.Dir()
	if err != nil {
		m.appendTranscript(milkTag() + " workflow clear error: cannot determine state dir: " + err.Error() + "\n")
		return m, nil
	}
	id, kind, err := workflow.CurrentWorkflowID(stateDir, sess.ID)
	if err != nil {
		m.appendTranscript(milkTag() + " workflow clear error: " + err.Error() + "\n")
		return m, nil
	}
	if kind == workflow.WorkflowKindNone {
		m.workflowState = nil
		m.workflowPanelOpen = false
		m.syncLayout()
		m.appendTranscript(milkTag() + " no saved workflow state for this session\n")
		return m, nil
	}
	path := workflow.StatePath(stateDir, sess.ID, id)
	if kind == workflow.WorkflowKindInterp {
		path = workflow.InterpCheckpointPath(stateDir, sess.ID, id)
	}
	m.pendingWorkflowWizard = &workflowWizardState{
		step:       wizardStepClearConfirm,
		clearing:   true,
		workflowID: id,
		kind:       kind,
	}
	m.appendTranscript(milkTag() + fmt.Sprintf(
		" workflow clear — type \"clear\" to rename the state file (won't delete plan/findings/sprint files), anything else to cancel:\n  %s\n",
		path,
	))
	m.refreshPrompt()
	return m, nil
}

// execWorkflowClear renames the workflow state file for the current session
// out of the way (adding a ".cleared" suffix) so /workflow clear does not
// destroy a checkpoint the user might still want to inspect, and so the
// workflow's ID stays reserved and is never reused by a later run.
func (m model) execWorkflowClear(workflowID int, kind workflow.WorkflowKind) (tea.Model, tea.Cmd) {
	sess := m.st.sess
	if sess == nil {
		m.appendTranscript(milkTag() + " workflow clear error: no active session\n")
		return m, nil
	}
	stateDir, err := session.Dir()
	if err != nil {
		m.appendTranscript(milkTag() + " workflow clear error: cannot determine state dir: " + err.Error() + "\n")
		return m, nil
	}
	path := workflow.StatePath(stateDir, sess.ID, workflowID)
	if kind == workflow.WorkflowKindInterp {
		path = workflow.InterpCheckpointPath(stateDir, sess.ID, workflowID)
	}
	if err := os.Rename(path, path+".cleared"); err != nil && !os.IsNotExist(err) {
		m.appendTranscript(milkTag() + " workflow clear error: " + err.Error() + "\n")
		return m, nil
	}
	m.workflowState = nil
	m.workflowPanelOpen = false
	m.syncLayout()
	m.appendTranscript(milkTag() + " workflow state cleared\n")
	return m, nil
}

// handleWorkflowReconfigure starts the agent-roles wizard for /workflow reconfigure.
// It loads the saved state to get the task and checkpoint position, then runs the
// designer/generator/evaluator wizard steps. On completion, applyWorkflowReconfigure
// writes the new agent names back into the state file without touching sprint/pass/role,
// so a subsequent /workflow resume picks up where it left off with the new agents.
func (m model) handleWorkflowReconfigure() (tea.Model, tea.Cmd) {
	sess := m.st.sess
	stateDir, err := session.Dir()
	if err != nil {
		m.appendTranscript(milkTag() + " workflow reconfigure error: cannot determine state dir: " + err.Error() + "\n")
		return m, nil
	}
	id, kind, err := workflow.CurrentWorkflowID(stateDir, sess.ID)
	if err != nil {
		m.appendTranscript(milkTag() + " workflow reconfigure error: " + err.Error() + "\n")
		return m, nil
	}
	if kind == workflow.WorkflowKindNone {
		m.appendTranscript(milkTag() + " no saved workflow state for this session — start a workflow first\n")
		return m, nil
	}
	if kind == workflow.WorkflowKindInterp {
		return m.handleGenericWorkflowReconfigure(stateDir, id)
	}
	st, err := workflow.LoadState(workflow.StatePath(stateDir, sess.ID, id))
	if err != nil {
		m.appendTranscript(milkTag() + " workflow reconfigure error: " + err.Error() + "\n")
		return m, nil
	}
	if st == nil {
		m.appendTranscript(milkTag() + " no saved workflow state for this session — start a workflow first\n")
		return m, nil
	}

	w := &workflowWizardState{
		name: st.WorkflowName,
		task: st.Task,
		// Pre-populate current agent names so the wizard can show them as defaults
		// and accept blank input to keep the existing value.
		designer:      st.AgentMap["designer"],
		generator:     st.AgentMap["generator"],
		evaluator:     st.AgentMap["evaluator"],
		step:          wizardStepDesigner,
		reconfiguring: true,
		sprint:        st.Sprint,
		pass:          st.Pass,
		role:          st.Role,
		workflowID:    id,
	}
	m.pendingWorkflowWizard = w
	m.appendTranscript(milkTag() + fmt.Sprintf(
		" workflow reconfigure — reassign agents for sprint %d pass %d (task: %s)\n",
		st.Sprint, st.Pass, st.Task,
	))
	m.appendTranscript(milkTag() + workflowAgentReconfigurePrompt("designer", w.designer))
	m.refreshPrompt()
	return m, nil
}

// handleGenericWorkflowReconfigure starts the role-reassignment wizard for an
// interpreter-driven (non-"dev") checkpoint at stateDir/id — the generic
// counterpart to handleWorkflowReconfigure's dev-specific path above. Every
// role is re-asked (unlike dev's fixed triple, a generic definition's role
// list isn't known until the definition is looked up), each defaulting to
// its current agent on blank input.
func (m model) handleGenericWorkflowReconfigure(stateDir string, id int) (tea.Model, tea.Cmd) {
	sess := m.st.sess
	path := workflow.InterpCheckpointPath(stateDir, sess.ID, id)
	cp, err := interp.LoadCheckpoint(path)
	if err != nil {
		m.appendTranscript(milkTag() + " workflow reconfigure error: " + err.Error() + "\n")
		return m, nil
	}
	if cp == nil {
		m.appendTranscript(milkTag() + " no saved workflow state for this session — start a workflow first\n")
		return m, nil
	}

	reg, regErrs := workflow.LoadRegistry()
	for _, e := range regErrs {
		obs.Info("workflow.registry.load_error", "error", e.Error())
	}
	def, ok := reg.Lookup(cp.DefinitionName)
	if !ok {
		m.appendTranscript(milkTag() + fmt.Sprintf(" workflow reconfigure error: workflow %q is no longer registered\n", cp.DefinitionName))
		return m, nil
	}

	w := &workflowWizardState{
		name:          cp.DefinitionName,
		task:          cp.Task,
		def:           def,
		roles:         def.Roles,
		roleValues:    map[string]string{},
		roleDefaults:  cp.AgentMap,
		reconfiguring: true,
		workflowID:    id,
	}
	m.appendTranscript(milkTag() + fmt.Sprintf(
		" workflow reconfigure — reassign agents for %s (task: %s)\n", cp.DefinitionName, cp.Task,
	))
	return m.advanceToNextGenericRoleOrLaunch(w)
}

// applyGenericWorkflowReconfigure writes new agent names from the wizard into
// the checkpoint's AgentMap, leaving Trace/Done untouched so a subsequent
// /workflow resume continues exactly where it left off with the new agents.
func (m model) applyGenericWorkflowReconfigure(w *workflowWizardState) (tea.Model, tea.Cmd) {
	sess := m.st.sess
	stateDir, err := session.Dir()
	if err != nil {
		m.appendTranscript(milkTag() + " workflow reconfigure error: cannot determine state dir: " + err.Error() + "\n")
		return m, nil
	}
	path := workflow.InterpCheckpointPath(stateDir, sess.ID, w.workflowID)
	cp, err := interp.LoadCheckpoint(path)
	if err != nil {
		m.appendTranscript(milkTag() + " workflow reconfigure error: " + err.Error() + "\n")
		return m, nil
	}
	if cp == nil {
		m.appendTranscript(milkTag() + " workflow reconfigure error: checkpoint disappeared during wizard\n")
		return m, nil
	}

	agentNames, err := workflow.ResolveAgentNames(w.roleValues, m.st.cfg)
	if err != nil {
		m.appendTranscript(milkTag() + " workflow reconfigure error: " + err.Error() + "\n")
		return m, nil
	}

	cp.AgentMap = agentNames
	if err := interp.SaveCheckpoint(path, cp); err != nil {
		m.appendTranscript(milkTag() + " workflow reconfigure error: cannot save checkpoint: " + err.Error() + "\n")
		return m, nil
	}

	if m.workflowState != nil {
		m.workflowState.AgentMap = agentNames
	}
	m.appendTranscript(milkTag() + fmt.Sprintf(
		" workflow reconfigured — %s\n  use /workflow resume to continue\n", formatGenericAgentNames(w.roles, agentNames),
	))
	m.refreshPrompt()
	return m, nil
}

// applyWorkflowReconfigure writes new agent names from the wizard into the saved
// state file, preserving sprint/pass/role so /workflow resume works unchanged.
func (m model) applyWorkflowReconfigure(w *workflowWizardState) (tea.Model, tea.Cmd) {
	sess := m.st.sess
	stateDir, err := session.Dir()
	if err != nil {
		m.appendTranscript(milkTag() + " workflow reconfigure error: cannot determine state dir: " + err.Error() + "\n")
		return m, nil
	}
	path := workflow.StatePath(stateDir, sess.ID, w.workflowID)
	st, err := workflow.LoadState(path)
	if err != nil {
		m.appendTranscript(milkTag() + " workflow reconfigure error: " + err.Error() + "\n")
		return m, nil
	}
	if st == nil {
		m.appendTranscript(milkTag() + " workflow reconfigure error: state file disappeared during wizard\n")
		return m, nil
	}

	roles := map[string]string{
		"designer":  w.designer,
		"generator": w.generator,
		"evaluator": w.evaluator,
	}
	agentNames, err := workflow.ResolveAgentNames(roles, m.st.cfg)
	if err != nil {
		m.appendTranscript(milkTag() + " workflow reconfigure error: " + err.Error() + "\n")
		return m, nil
	}

	st.AgentMap = agentNames
	if err := workflow.SaveState(path, st); err != nil {
		m.appendTranscript(milkTag() + " workflow reconfigure error: cannot save state: " + err.Error() + "\n")
		return m, nil
	}

	// Update the in-memory panel display to reflect the new agent map.
	if m.workflowState != nil {
		m.workflowState.AgentMap = agentNames
	}

	m.appendTranscript(milkTag() + fmt.Sprintf(
		" workflow reconfigured — designer: %s  generator: %s  evaluator: %s\n  use /workflow resume to continue from sprint %d pass %d\n",
		agentNames["designer"], agentNames["generator"], agentNames["evaluator"],
		st.Sprint, st.Pass,
	))
	m.refreshPrompt()
	return m, nil
}

// launchGenericWorkflow resolves agents, builds runners, and starts the
// interpreter-driven workflow goroutine for any registered definition,
// including the built-in "dev" — every fresh launch goes through here now.
// w.workflowState is shown once at start and not updated stage-by-stage the
// way dev.go's wfdev.WorkflowProgressMsg did; see internal/workflow/interp's
// package doc for that and other still-open gaps against the retired
// hand-written dev.go launch path (dev.go itself remains in place — it's
// still needed to resume a pre-existing, dev.go-format checkpoint).
func (m model) launchGenericWorkflow(w *workflowWizardState) (tea.Model, tea.Cmd) {
	cfg := m.st.cfg
	sess := m.st.sess
	send := func(msg tea.Msg) { m.st.program.Send(msg) }

	agentNames, err := workflow.ResolveAgentNames(w.roleValues, cfg)
	if err != nil {
		m.appendTranscript(milkTag() + " workflow error: " + err.Error() + "\n")
		return m, nil
	}

	stateDir, err := session.Dir()
	if err != nil {
		m.appendTranscript(milkTag() + " workflow error: cannot determine state dir: " + err.Error() + "\n")
		return m, nil
	}
	workflowID := w.workflowID
	if !w.resuming {
		var err error
		workflowID, err = workflow.NextWorkflowID(stateDir, sess.ID)
		if err != nil {
			m.appendTranscript(milkTag() + " workflow error: cannot determine workflow ID: " + err.Error() + "\n")
			return m, nil
		}
	}

	m.st.toolFutures = map[string]chan string{}
	ir0 := &tuiInputReader{send: send}
	tuiAgents, cliPC := m.buildTUIAgents(send, ir0)

	runners, err := buildWorkflowRunners(agentNames, cfg, sess, m.st.mem, &tuiAgents, cliPC, func() inputReader { return ir0 })
	if err != nil {
		m.appendTranscript(milkTag() + " workflow error: " + err.Error() + "\n")
		return m, nil
	}

	answersCh := make(chan string, 1)

	checkpointPath := workflow.InterpCheckpointPath(stateDir, sess.ID, workflowID)
	r := interp.New(w.def, w.task).WithCheckpoint(checkpointPath).WithAgentMap(agentNames)
	if w.maxIterOverrideStageID != "" {
		r = r.WithMaxIterationsOverride(w.maxIterOverrideStageID, w.maxIterOverrideN)
	}
	runCfg := workflow.RunConfig{
		Session:    sess,
		Runners:    runners,
		Send:       send,
		StateDir:   stateDir,
		AnswersCh:  answersCh,
		WorkflowID: workflowID,
	}

	m.workflowPanelOpen = true
	m.busy = true
	m.spinnerFrame = 0
	m.lastWorkflowActivity = time.Now()
	m.workflowTimeoutWarned = false
	m.workflowState = &workflow.State{
		WorkflowName: w.def.Name,
		Task:         w.task,
		Role:         "starting",
		AgentMap:     agentNames,
		WorkflowID:   workflowID,
		Generic:      true,
	}
	verb := "starting"
	if w.resuming {
		verb = "resuming"
	}
	m.appendTranscript(milkTag() + fmt.Sprintf(" %s workflow %s (%s)\n", verb, w.def.Name, formatGenericAgentNames(w.roles, agentNames)))
	m.syncLayout()

	ctx, cancel := context.WithCancel(m.ctx)
	m.cancelTurn = cancel
	return m, tea.Batch(
		spinnerTick(),
		workflowIdleCheck(),
		func() tea.Msg {
			defer cancel()
			err := r.Run(ctx, runCfg)
			return wfdev.WorkflowDoneMsg{Err: err}
		},
	)
}

// formatGenericAgentNames renders "role1: name1  role2: name2  ..." in roles
// order, for the "starting workflow" transcript line.
func formatGenericAgentNames(roles []string, agentNames map[string]string) string {
	parts := make([]string, len(roles))
	for i, role := range roles {
		parts[i] = fmt.Sprintf("%s: %s", role, agentNames[role])
	}
	return strings.Join(parts, "  ")
}

// handleWorkflowWizardKey handles keypresses while the workflow wizard is active.
// Ctrl+C and Esc cancel; Enter advances to the next step.
func (m model) handleWorkflowWizardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		m.pendingWorkflowWizard = nil
		m.appendTranscript("\n" + milkTag() + " workflow wizard cancelled\n")
		m.refreshPrompt()
		return m, nil
	case "enter", "ctrl+m":
		input := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()
		m.syncLayout()
		m.appendTranscript(promptLabel(m.st) + input + "\n")
		return m.advanceWorkflowWizard(input)
	}
	// All other keys: normal textarea editing.
	cmd := m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

// handleWorkflowResume loads saved workflow state and re-launches the workflow
// from the checkpointed sprint/pass, using the agent names recorded in state.
func (m model) handleWorkflowResume() (tea.Model, tea.Cmd) {
	sess := m.st.sess
	stateDir, err := session.Dir()
	if err != nil {
		m.appendTranscript(milkTag() + " workflow resume error: cannot determine state dir: " + err.Error() + "\n")
		return m, nil
	}
	id, kind, err := workflow.CurrentWorkflowID(stateDir, sess.ID)
	if err != nil {
		m.appendTranscript(milkTag() + " workflow resume error: " + err.Error() + "\n")
		return m, nil
	}
	if kind == workflow.WorkflowKindNone {
		m.appendTranscript(milkTag() + " no saved workflow state for this session\n")
		return m, nil
	}
	if kind == workflow.WorkflowKindInterp {
		return m.handleGenericWorkflowResume(stateDir, id)
	}
	st, err := workflow.LoadState(workflow.StatePath(stateDir, sess.ID, id))
	if err != nil {
		m.appendTranscript(milkTag() + " workflow resume error: " + err.Error() + "\n")
		return m, nil
	}
	if st == nil {
		m.appendTranscript(milkTag() + " no saved workflow state for this session\n")
		return m, nil
	}
	if st.Role == "done" {
		m.appendTranscript(milkTag() + " workflow already completed — use /workflow clear to remove\n")
		return m, nil
	}

	// Rebuild wizard state from the saved checkpoint.
	// If AgentMap is absent (state file predates AgentMap support), enter the
	// agent wizard so the user can supply the roles before resuming.
	if len(st.AgentMap) == 0 {
		w := &workflowWizardState{
			name:       st.WorkflowName,
			task:       st.Task,
			step:       wizardStepDesigner,
			resuming:   true,
			sprint:     st.Sprint,
			pass:       st.Pass,
			workflowID: id,
		}
		m.pendingWorkflowWizard = w
		m.appendTranscript(milkTag() + " workflow resume: agent map missing — please specify agents (blank = escalation):\n")
		m.appendTranscript(milkTag() + workflowAgentPrompt("designer"))
		m.refreshPrompt()
		return m, nil
	}
	w := &workflowWizardState{
		name:       st.WorkflowName,
		task:       st.Task,
		designer:   st.AgentMap["designer"],
		generator:  st.AgentMap["generator"],
		evaluator:  st.AgentMap["evaluator"],
		workflowID: id,
	}
	if w.designer == "" {
		w.designer = workflow.AliasEscalation
	}
	if w.generator == "" {
		w.generator = workflow.AliasEscalation
	}
	if w.evaluator == "" {
		w.evaluator = workflow.AliasEscalation
	}

	return m.launchWorkflowResume(w, st.Sprint, st.Pass, 0, st.Role)
}

// handleGenericWorkflowResume resumes an interpreter-driven (non-"dev")
// workflow from its checkpoint at stateDir/id. Unlike dev's resume path,
// there is no separate wizard re-ask when the agent map is missing — every
// interp checkpoint written by this version of milk already carries one
// (see interp.Runner.WithAgentMap); a checkpoint from an older build without
// one falls back to the escalation alias for every role rather than
// launching a wizard, since a generic role list has no fixed shape to
// pre-populate wizard steps against the way dev's fixed triple does.
func (m model) handleGenericWorkflowResume(stateDir string, id int) (tea.Model, tea.Cmd) {
	sess := m.st.sess
	cp, err := interp.LoadCheckpoint(workflow.InterpCheckpointPath(stateDir, sess.ID, id))
	if err != nil {
		m.appendTranscript(milkTag() + " workflow resume error: " + err.Error() + "\n")
		return m, nil
	}
	if cp == nil || cp.Done {
		m.appendTranscript(milkTag() + " workflow already completed — use /workflow clear to remove\n")
		return m, nil
	}

	reg, regErrs := workflow.LoadRegistry()
	for _, e := range regErrs {
		obs.Info("workflow.registry.load_error", "error", e.Error())
	}
	def, ok := reg.Lookup(cp.DefinitionName)
	if !ok {
		m.appendTranscript(milkTag() + fmt.Sprintf(" workflow resume error: workflow %q is no longer registered\n", cp.DefinitionName))
		return m, nil
	}

	roleValues := cp.AgentMap
	if len(roleValues) == 0 {
		roleValues = make(map[string]string, len(def.Roles))
		for _, role := range def.Roles {
			roleValues[role] = workflow.AliasEscalation
		}
	}

	w := &workflowWizardState{
		name:       cp.DefinitionName,
		task:       cp.Task,
		def:        def,
		roles:      def.Roles,
		roleValues: roleValues,
		resuming:   true,
		workflowID: id,
	}
	return m.launchGenericWorkflow(w)
}

// launchWorkflowResume is like launchWorkflow but resumes from a sprint/pass/role checkpoint.
// role should be the saved State.Role value ("generator" or "evaluator"); use "generator" when
// the role is unknown (e.g. agent-wizard resume or extend-after-exhaustion).
// maxPasses overrides the plan-declared limit; pass 0 to use the plan value.
func (m model) launchWorkflowResume(w *workflowWizardState, sprint, pass, maxPasses int, role string) (tea.Model, tea.Cmd) {
	cfg := m.st.cfg
	sess := m.st.sess
	send := func(msg tea.Msg) { m.st.program.Send(msg) }

	roles := map[string]string{
		"designer":  w.designer,
		"generator": w.generator,
		"evaluator": w.evaluator,
	}

	agentNames, err := workflow.ResolveAgentNames(roles, cfg)
	if err != nil {
		m.appendTranscript(milkTag() + " workflow resume error: " + err.Error() + "\n")
		return m, nil
	}

	stateDir, err := session.Dir()
	if err != nil {
		m.appendTranscript(milkTag() + " workflow resume error: cannot determine state dir: " + err.Error() + "\n")
		return m, nil
	}

	m.st.toolFutures = map[string]chan string{}
	ir0 := &tuiInputReader{send: send}
	tuiAgents, cliPC := m.buildTUIAgents(send, ir0)

	runners, err := buildWorkflowRunners(agentNames, cfg, sess, m.st.mem, &tuiAgents, cliPC, func() inputReader { return ir0 })
	if err != nil {
		m.appendTranscript(milkTag() + " workflow resume error: " + err.Error() + "\n")
		return m, nil
	}

	answersCh := make(chan string, 1)

	wf := wfdev.NewResume(w.task, maxPasses, sprint, pass, role)
	runCfg := workflow.RunConfig{
		Session:    sess,
		Runners:    runners,
		Send:       send,
		StateDir:   stateDir,
		AnswersCh:  answersCh,
		WorkflowID: w.workflowID,
	}

	m.workflowPanelOpen = true
	m.busy = true
	m.spinnerFrame = 0
	m.lastWorkflowActivity = time.Now()
	m.workflowTimeoutWarned = false
	m.workflowState = &workflow.State{
		WorkflowName: "dev",
		Task:         w.task,
		Sprint:       sprint,
		Pass:         pass,
		Role:         "generator",
		AgentMap:     agentNames,
		WorkflowID:   w.workflowID,
	}
	m.appendTranscript(milkTag() + fmt.Sprintf(" resuming workflow dev from sprint %d pass %d (designer: %s  generator: %s  evaluator: %s)\n",
		sprint, pass, agentNames["designer"], agentNames["generator"], agentNames["evaluator"]))
	m.syncLayout()

	ctx, cancel := context.WithCancel(m.ctx)
	m.cancelTurn = cancel
	return m, tea.Batch(
		spinnerTick(),
		workflowIdleCheck(),
		func() tea.Msg {
			defer cancel()
			err := wf.Run(ctx, runCfg)
			return wfdev.WorkflowDoneMsg{Err: err}
		},
	)
}

// workflowExtendState holds context for the "passes exhausted — continue?" prompt.
type workflowExtendState struct {
	wizard    *workflowWizardState
	sprint    int
	maxPasses int // current (exhausted) limit; resume will double it
}

// handleWorkflowExtendKey handles keypresses while the extend prompt is pending.
// y/Enter doubles the pass limit and resumes; n/Ctrl+C/Esc dismisses with an error message.
func (m model) handleWorkflowExtendKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ext := m.pendingWorkflowExtend
	switch msg.String() {
	case "y", "Y", "enter", "ctrl+m":
		m.ta.Reset()
		m.syncLayout()
		m.appendTranscript("y\n")
		m.pendingWorkflowExtend = nil
		newMax := ext.maxPasses * 2
		return m.launchWorkflowResume(ext.wizard, ext.sprint, 1, newMax, "generator")
	case "n", "N", "ctrl+c", "esc":
		m.ta.Reset()
		m.syncLayout()
		m.appendTranscript("n\n" + milkTag() + fmt.Sprintf(" workflow halted after %d passes\n", ext.maxPasses))
		m.pendingWorkflowExtend = nil
		m.refreshPrompt()
		return m, nil
	}
	return m, nil
}

// genericWorkflowExtendState holds context for the "N iterations exhausted —
// continue?" prompt for an interpreter-driven (non-"dev") workflow — the
// generic counterpart to workflowExtendState above.
type genericWorkflowExtendState struct {
	wizard        *workflowWizardState
	stageID       string
	maxIterations int // current (exhausted) limit; resume will double it
}

// handleGenericWorkflowExtendKey handles keypresses while a generic extend
// prompt is pending. y/Enter doubles the exhausted stage's iteration limit
// and resumes; n/Ctrl+C/Esc dismisses with an error message.
func (m model) handleGenericWorkflowExtendKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ext := m.pendingGenericWorkflowExtend
	switch msg.String() {
	case "y", "Y", "enter", "ctrl+m":
		m.ta.Reset()
		m.syncLayout()
		m.appendTranscript("y\n")
		m.pendingGenericWorkflowExtend = nil
		ext.wizard.maxIterOverrideStageID = ext.stageID
		ext.wizard.maxIterOverrideN = ext.maxIterations * 2
		return m.launchGenericWorkflow(ext.wizard)
	case "n", "N", "ctrl+c", "esc":
		m.ta.Reset()
		m.syncLayout()
		m.appendTranscript("n\n" + milkTag() + fmt.Sprintf(" workflow halted after %d iterations\n", ext.maxIterations))
		m.pendingGenericWorkflowExtend = nil
		m.refreshPrompt()
		return m, nil
	}
	return m, nil
}

// workflowAgentPrompt returns the wizard prompt line for an agent role.
func workflowAgentPrompt(role string) string {
	return fmt.Sprintf(" workflow dev — %s agent (blank = escalation):\n", role)
}

// workflowAgentReconfigurePrompt returns the wizard prompt for a reconfigure step,
// showing the current agent name as the default.
func workflowAgentReconfigurePrompt(role, current string) string {
	if current == "" {
		return fmt.Sprintf(" workflow reconfigure — %s agent (blank = escalation):\n", role)
	}
	return fmt.Sprintf(" workflow reconfigure — %s agent (blank = keep %q):\n", role, current)
}

// applyWorkflowBehaviourOverrides returns a shallow copy of cfg with the
// generator and evaluator agent configs overridden with any behaviour prompts
// collected during the wizard. The override is session-local — it is NOT
// written back to ~/.milk/config.json. Empty prompts leave the config unchanged.
// workflowAgentInputWithDefault normalises a wizard agent answer.
// In reconfigure mode, blank keeps the existing value (current); in normal mode
// blank falls back to AliasEscalation.
func workflowAgentInputWithDefault(input, current string, reconfiguring bool) string {
	if input == "" {
		if reconfiguring && current != "" {
			return current
		}
		return workflow.AliasEscalation
	}
	return input
}

// parseWorkflowFlags splits args into positional remainder and --key value pairs.
// Returns an error when a --flag appears without a following value.
func parseWorkflowFlags(parts []string) (remainder []string, flags map[string]string, err error) {
	flags = map[string]string{}
	for i := 0; i < len(parts); i++ {
		if strings.HasPrefix(parts[i], "--") {
			if i+1 >= len(parts) {
				return nil, nil, errors.New("flag " + parts[i] + " requires a value")
			}
			key := strings.TrimPrefix(parts[i], "--")
			flags[key] = parts[i+1]
			i++
		} else {
			remainder = append(remainder, parts[i])
		}
	}
	return remainder, flags, nil
}
