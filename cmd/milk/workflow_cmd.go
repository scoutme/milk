package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
	"github.com/scoutme/milk/internal/workflow/interp"
)

// workflowWizardState tracks multi-step wizard input for /workflow.
// Every workflow launches through the generic (roles/roleValues/roleDefaults)
// fields via handleGenericWorkflowCmd and the interpreter.
type workflowWizardState struct {
	name          string // workflow name (e.g. "dev")
	task          string
	step          workflowWizardStep
	clearing      bool                  // true when this wizard is a /workflow clear confirmation
	resuming      bool                  // true when completing the wizard should resume rather than start fresh
	reconfiguring bool                  // true when completing the wizard should update state agent map only
	workflowID    int                   // resolved workflow ID (see workflow.CurrentWorkflowID/NextWorkflowID); used by clear/reconfigure/resume
	kind          workflow.WorkflowKind // used by the clear-confirmation path

	def          workflow.Definition // resolved definition
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
	wizardStepClearConfirm                           // ask user to type "clear" to confirm
	wizardStepGenericRole                            // ask for the role at roles[roleIdx]
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
		m.appendTranscript(milkTag() + " available workflows:\n  " + strings.Join(reg.Names(), ", ") + "\n\nUsage:\n  /workflow <name> [task] [--<role> <agent> ...]\n  /workflow status       — show the running workflow\n  /workflow cancel       — stop the running workflow (resumable)\n  /workflow resume       — resume workflow from last checkpoint\n  /workflow reconfigure  — reassign agent roles without losing saved state\n  /workflow clear        — delete saved state for this session\n")
		return m, nil
	}

	name := parts[0]
	if name == "status" {
		return m.handleWorkflowStatus()
	}
	if name == "cancel" {
		return m.handleWorkflowCancel()
	}
	if m.workflowRunning {
		// Everything below starts, resumes, reconfigures or clears a run; none
		// of it is safe against the one that is live.
		m.appendTranscript(milkTag() + " a workflow is running — /workflow status shows it, /workflow cancel stops it\n")
		return m, nil
	}
	if name == "resume" {
		return m.handleWorkflowResume()
	}
	if name == "clear" {
		return m.handleWorkflowClear()
	}
	if name == "reconfigure" {
		return m.handleWorkflowReconfigure()
	}
	// Every workflow launches through the registry + interpreter.
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
	}

	m.pendingWorkflowWizard = nil
	if w.reconfiguring {
		return m.applyGenericWorkflowReconfigure(w)
	}
	return m.launchGenericWorkflow(w)
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
	if _, err := clearSavedWorkflow(stateDir, sess.ID, workflowID, kind); err != nil {
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
	// Legacy dev-format checkpoint — cannot reconfigure without the hardcoded
	// dev workflow. Clear and start a fresh workflow instead.
	m.appendTranscript(milkTag() + " workflow reconfigure: legacy dev-format checkpoint detected — use /workflow clear then /workflow dev to start fresh\n")
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

// cancelRunningWorkflow cancels the live workflow, if any, and reports whether
// there was one. The done message that follows does the bookkeeping.
func (m *model) cancelRunningWorkflow() bool {
	if !m.workflowRunning || m.cancelWorkflow == nil {
		return false
	}
	m.workflowCancelled = true
	m.cancelWorkflow()
	m.cancelWorkflow = nil
	return true
}

func (m model) handleWorkflowCancel() (tea.Model, tea.Cmd) {
	if !m.cancelRunningWorkflow() {
		m.appendTranscript(milkTag() + " no workflow is running\n")
		return m, nil
	}
	m.appendTranscript(milkTag() + " cancelling workflow — /workflow resume continues it from the last checkpoint\n")
	return m, nil
}

func (m model) handleWorkflowStatus() (tea.Model, tea.Cmd) {
	if m.workflowRunning {
		m.appendTranscript(milkTag() + " " + workflowStatusLabel(m.workflowState) + "\n")
		return m, nil
	}
	saved, err := loadSavedWorkflow(m.st.sess)
	switch {
	case errors.Is(err, errNoSavedWorkflow):
		m.appendTranscript(milkTag() + " no workflow is running and none is saved for this session\n")
	case errors.Is(err, errWorkflowDone):
		m.appendTranscript(milkTag() + " no workflow is running — the last one is complete (/workflow clear removes its state)\n")
	case err != nil:
		m.appendTranscript(milkTag() + " workflow status error: " + err.Error() + "\n")
	default:
		m.appendTranscript(milkTag() + fmt.Sprintf(" no workflow is running — %s #%d is paused (/workflow resume continues it)\n  task: %s\n",
			saved.Def.Name, saved.ID, saved.Checkpoint.Task))
	}
	return m, nil
}

// busySafeWorkflowCmd reports whether a /workflow invocation may run while an
// agent turn is in progress: only the read-only status and the cancel, so a
// workflow can always be stopped without waiting for the turn.
func busySafeWorkflowCmd(cmd, rest string) bool {
	if cmd != cmdWorkflow {
		return false
	}
	switch strings.TrimSpace(rest) {
	case "status", "cancel":
		return true
	}
	return false
}

// workflowDisplaySend wraps send for the agents a workflow runs. buildTUIAgents
// wires each agent's display hooks (reasoning, CLI tool-use hints, retraction)
// to messages that write into the main transcript; for a workflow they belong
// in its live buffer instead. Retraction and reasoning-promotion refer to the
// main transcript's current turn and are dropped; everything else (permission
// asks, OAuth, open-file) passes through.
func workflowDisplaySend(send func(tea.Msg)) func(tea.Msg) {
	return func(msg tea.Msg) {
		switch v := msg.(type) {
		case thinkChunkMsg:
			send(workflowThinkChunkMsg{text: v.text})
		case chunkMsg:
			send(workflow.WorkflowChunkMsg{Text: v.text})
		case retractMsg, reasoningPromotedMsg:
		default:
			send(msg)
		}
	}
}

// launchGenericWorkflow resolves agents, builds runners, and starts the
// interpreter-driven workflow goroutine for any registered definition.
func (m model) launchGenericWorkflow(w *workflowWizardState) (tea.Model, tea.Cmd) {
	// Refuse any launch while a workflow is running: start_workflow's
	// tool-driven path (startWorkflowFromToolMsg) has no equivalent of the
	// /workflow slash commands' early check, and a second interp.Runner would
	// race the first over the single m.workflowState/m.cancelWorkflow fields.
	// A resume/extend of a finished run is fine: workflowRunning is already
	// false by then.
	if m.workflowRunning {
		running := "a workflow"
		if m.workflowState != nil && m.workflowState.WorkflowName != "" {
			running = fmt.Sprintf("workflow %q (role: %s)", m.workflowState.WorkflowName, m.workflowState.Role)
		}
		m.appendTranscript(milkTag() + fmt.Sprintf(
			" %s is already running — ignoring request to start %q; /workflow status shows it, /workflow cancel stops it\n",
			running, w.name,
		))
		m.refreshPrompt()
		return m, nil
	}

	cfg := m.st.cfg
	sess := m.st.sess
	send := func(msg tea.Msg) { m.st.program.Send(msg) }

	plan, err := planWorkflow(cfg, sess, w.def, w.task, w.roleValues, w.resuming, w.workflowID)
	if err != nil {
		m.appendTranscript(milkTag() + " workflow error: " + err.Error() + "\n")
		return m, nil
	}

	// Consume any staged attachments (/attach, clipboard paste): clear the
	// pending slot so they cannot silently leak into a later normal REPL turn,
	// and hand them to the workflow as on-disk file references appended to the
	// task (attachmentTaskBlock keeps the block short enough to survive interp's
	// variable-budget and prompt-render truncation caps). Role runners
	// additionally receive first-turn image injection (vision content parts for
	// local providers, @path lines for CLI providers).
	attachments := m.pendingAttachments
	m.pendingAttachments = nil
	task := workflowTaskWithAttachments(w.task, attachments)
	if len(attachments) > 0 {
		names := make([]string, 0, len(attachments))
		for _, a := range attachments {
			names = append(names, a.Name)
		}
		m.appendTranscript(milkTag() + fmt.Sprintf(
			" attached %d file(s) to the workflow task as file references (%s) — available to every role on disk\n",
			len(attachments), strings.Join(names, ", "),
		))
	}

	m.st.toolFutures = map[string]chan string{}
	ir0 := &tuiInputReader{send: send}
	tuiAgents, cliPC := m.buildTUIAgents(workflowDisplaySend(send), ir0)

	runners, err := buildWorkflowRunners(plan.AgentNames, cfg, sess, m.st.mem, &tuiAgents, cliPC, func() inputReader { return ir0 }, m.st.notifier, attachments)
	if err != nil {
		m.appendTranscript(milkTag() + " workflow error: " + err.Error() + "\n")
		return m, nil
	}

	answersCh := make(chan string, 1)

	r, runCfg := plan.newRunner(sess, task, runners, send, answersCh)
	if w.maxIterOverrideStageID != "" {
		r = r.WithMaxIterationsOverride(w.maxIterOverrideStageID, w.maxIterOverrideN)
	}

	m.autoOpenPanel(regionWorkflow)
	m.workflowRunning = true
	m.workflowCancelled = false
	m.workflowGen++
	gen := m.workflowGen
	m.lastWorkflowActivity = time.Now()
	m.workflowTimeoutWarned = false
	m.workflowState = plan.initialState()
	verb := "starting"
	if w.resuming {
		verb = "resuming"
	}
	m.appendTranscript(milkTag() + fmt.Sprintf(" %s workflow %s (%s)\n", verb, w.def.Name, formatGenericAgentNames(w.roles, plan.AgentNames)))
	m.syncLayout()

	ctx, cancel := context.WithCancel(m.ctx)
	m.cancelWorkflow = cancel
	return m, tea.Batch(
		workflowIdleCheck(gen),
		func() tea.Msg {
			defer cancel()
			err := r.Run(ctx, runCfg)
			return workflow.WorkflowDoneMsg{Err: err}
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

// handleWorkflowResume loads the session's saved interpreter-driven workflow
// and re-launches it from its checkpoint. Every registered workflow,
// including "dev", resumes through the interpreter; a checkpoint from an
// older build without an agent map falls back to the escalation agent for
// each role rather than launching a wizard, since a generic role list has no
// fixed shape to pre-populate wizard steps against.
func (m model) handleWorkflowResume() (tea.Model, tea.Cmd) {
	saved, err := loadSavedWorkflow(m.st.sess)
	switch {
	case errors.Is(err, errNoSavedWorkflow):
		m.appendTranscript(milkTag() + " no saved workflow state for this session\n")
		return m, nil
	case errors.Is(err, errLegacyWorkflow):
		// The hardcoded dev workflow has been removed; its old checkpoint can't resume.
		m.appendTranscript(milkTag() + " workflow resume: legacy dev-format checkpoint detected — use /workflow clear then /workflow dev to start fresh\n")
		return m, nil
	case errors.Is(err, errWorkflowDone):
		m.appendTranscript(milkTag() + " workflow already completed — use /workflow clear to remove\n")
		return m, nil
	case err != nil:
		m.appendTranscript(milkTag() + " workflow resume error: " + err.Error() + "\n")
		return m, nil
	}
	return m.launchGenericWorkflow(&workflowWizardState{
		name:       saved.Checkpoint.DefinitionName,
		task:       saved.Checkpoint.Task,
		def:        saved.Def,
		roles:      saved.Def.Roles,
		roleValues: saved.RoleValues(),
		resuming:   true,
		workflowID: saved.ID,
	})
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
