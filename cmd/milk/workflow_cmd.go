package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
	wfdev "github.com/scoutme/milk/internal/workflow/dev"
)

// workflowWizardState tracks multi-step wizard input for /workflow.
type workflowWizardState struct {
	name          string // workflow name (e.g. "dev")
	task          string
	designer      string
	generator     string
	evaluator     string
	step          workflowWizardStep
	clearing      bool   // true when this wizard is a /workflow clear confirmation
	resuming      bool   // true when completing the wizard should resume rather than start fresh
	reconfiguring bool   // true when completing the wizard should update state agent map only
	sprint        int    // checkpoint sprint (used when resuming/reconfiguring == true)
	pass          int    // checkpoint pass (used when resuming/reconfiguring == true)
	role          string // checkpoint role (used when resuming/reconfiguring == true)
}

type workflowWizardStep int

const (
	wizardStepTask         workflowWizardStep = iota // ask for task description
	wizardStepDesigner                               // ask for designer agent
	wizardStepGenerator                              // ask for generator agent
	wizardStepEvaluator                              // ask for evaluator agent
	wizardStepClearConfirm                           // ask user to type "clear" to confirm
	wizardStepDone
)

// handleWorkflowCmd dispatches /workflow [name] [args...].
func (m model) handleWorkflowCmd(args string) (tea.Model, tea.Cmd) {
	parts := strings.Fields(args)

	if len(parts) == 0 {
		// List available workflows.
		m.appendTranscript(milkTag() + " available workflows:\n  dev — designer → generator → evaluator loop\n\nUsage:\n  /workflow dev [task] [--designer <agent>] [--generator <agent>] [--evaluator <agent>]\n  /workflow resume       — resume workflow from last checkpoint\n  /workflow reconfigure  — reassign agent roles without losing saved state\n  /workflow clear        — delete saved state for this session\n")
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
	if name != "dev" {
		m.appendTranscript(milkTag() + fmt.Sprintf(" unknown workflow %q — available: dev, resume, reconfigure, clear\n", name))
		return m, nil
	}

	// Parse optional flags.
	remaining, flags, flagErr := parseWorkflowFlags(parts[1:])
	if flagErr != nil {
		m.appendTranscript(milkTag() + " workflow error: " + flagErr.Error() + "\n")
		return m, nil
	}
	task := strings.Join(remaining, " ")

	wizard := &workflowWizardState{
		name:      name,
		task:      task,
		designer:  flags["designer"],
		generator: flags["generator"],
		evaluator: flags["evaluator"],
	}

	// Enter wizard for any missing fields. Agent steps are skipped when all
	// three were supplied via flags; otherwise the wizard collects them in order.
	if wizard.task == "" {
		wizard.step = wizardStepTask
		m.pendingWorkflowWizard = wizard
		m.appendTranscript(milkTag() + " workflow dev — enter task description:\n")
		m.refreshPrompt()
		return m, nil
	}
	if wizard.designer == "" {
		wizard.step = wizardStepDesigner
		m.pendingWorkflowWizard = wizard
		m.appendTranscript(milkTag() + workflowAgentPrompt("designer"))
		m.refreshPrompt()
		return m, nil
	}
	if wizard.generator == "" {
		wizard.step = wizardStepGenerator
		m.pendingWorkflowWizard = wizard
		m.appendTranscript(milkTag() + workflowAgentPrompt("generator"))
		m.refreshPrompt()
		return m, nil
	}
	if wizard.evaluator == "" {
		wizard.step = wizardStepEvaluator
		m.pendingWorkflowWizard = wizard
		m.appendTranscript(milkTag() + workflowAgentPrompt("evaluator"))
		m.refreshPrompt()
		return m, nil
	}

	return m.launchWorkflow(wizard)
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
		return m.execWorkflowClear()

	case wizardStepTask:
		if input == "" {
			m.appendTranscript(milkTag() + " task description cannot be empty — enter task description:\n")
			m.refreshPrompt()
			return m, nil
		}
		w.task = input
		w.step = wizardStepDesigner
		m.pendingWorkflowWizard = w
		m.appendTranscript(milkTag() + workflowAgentPrompt("designer"))
		m.refreshPrompt()
		return m, nil

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

	m.pendingWorkflowWizard = nil
	if w.reconfiguring {
		return m.applyWorkflowReconfigure(w)
	}
	if w.resuming {
		return m.launchWorkflowResume(w, w.sprint, w.pass, 0, w.role)
	}
	return m.launchWorkflow(w)
}

// handleWorkflowClear starts the confirmation wizard for /workflow clear.
func (m model) handleWorkflowClear() (tea.Model, tea.Cmd) {
	sess := m.st.sess
	stateDir, err := session.Dir()
	if err != nil {
		m.appendTranscript(milkTag() + " workflow clear error: cannot determine state dir: " + err.Error() + "\n")
		return m, nil
	}
	path := workflow.StatePath(stateDir, sess.ID)
	st, err := workflow.LoadState(path)
	if err != nil {
		m.appendTranscript(milkTag() + " workflow clear error: " + err.Error() + "\n")
		return m, nil
	}
	if st == nil {
		m.workflowState = nil
		m.workflowPanelOpen = false
		m.syncLayout()
		m.appendTranscript(milkTag() + " no saved workflow state for this session\n")
		return m, nil
	}
	m.pendingWorkflowWizard = &workflowWizardState{
		step:     wizardStepClearConfirm,
		clearing: true,
	}
	m.appendTranscript(milkTag() + fmt.Sprintf(
		" workflow clear — type \"clear\" to delete state file, anything else to cancel:\n  %s\n",
		path,
	))
	m.refreshPrompt()
	return m, nil
}

// execWorkflowClear deletes the workflow state file for the current session.
func (m model) execWorkflowClear() (tea.Model, tea.Cmd) {
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
	path := workflow.StatePath(stateDir, sess.ID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
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
	path := workflow.StatePath(stateDir, sess.ID)
	st, err := workflow.LoadState(path)
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

// applyWorkflowReconfigure writes new agent names from the wizard into the saved
// state file, preserving sprint/pass/role so /workflow resume works unchanged.
func (m model) applyWorkflowReconfigure(w *workflowWizardState) (tea.Model, tea.Cmd) {
	sess := m.st.sess
	stateDir, err := session.Dir()
	if err != nil {
		m.appendTranscript(milkTag() + " workflow reconfigure error: cannot determine state dir: " + err.Error() + "\n")
		return m, nil
	}
	path := workflow.StatePath(stateDir, sess.ID)
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

// launchWorkflow resolves agents, builds runners, and starts the workflow goroutine.
func (m model) launchWorkflow(w *workflowWizardState) (tea.Model, tea.Cmd) {
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
		m.appendTranscript(milkTag() + " workflow error: " + err.Error() + "\n")
		return m, nil
	}

	stateDir, err := session.Dir()
	if err != nil {
		m.appendTranscript(milkTag() + " workflow error: cannot determine state dir: " + err.Error() + "\n")
		return m, nil
	}

	// Build TUI-wired agents (permission handlers, tool-use hints, skip-permissions)
	// so that workflow roles have the same tool access as normal turns.
	m.st.toolFutures = map[string]chan string{}
	ir0 := &tuiInputReader{send: send}
	tuiAgents, cliPC := m.buildTUIAgents(send, ir0)

	runners, err := buildWorkflowRunners(agentNames, cfg, sess, m.st.mem, &tuiAgents, cliPC, func() inputReader { return ir0 })
	if err != nil {
		m.appendTranscript(milkTag() + " workflow error: " + err.Error() + "\n")
		return m, nil
	}

	wf := wfdev.New(w.task, 0)
	runCfg := workflow.RunConfig{
		Session:  sess,
		Runners:  runners,
		Send:     send,
		StateDir: stateDir,
	}

	// Show panel immediately and mark busy so the TUI blocks normal input.
	m.workflowPanelOpen = true
	m.busy = true
	m.spinnerFrame = 0
	m.workflowState = &workflow.State{
		WorkflowName: "dev",
		Sprint:       1,
		Pass:         1,
		Role:         "starting",
		AgentMap:     agentNames,
	}
	m.appendTranscript(milkTag() + fmt.Sprintf(" starting workflow dev (designer: %s  generator: %s  evaluator: %s)\n",
		agentNames["designer"], agentNames["generator"], agentNames["evaluator"]))
	m.syncLayout()

	ctx, cancel := context.WithCancel(m.ctx)
	m.cancelTurn = cancel
	return m, tea.Batch(
		spinnerTick(),
		func() tea.Msg {
			defer cancel()
			err := wf.Run(ctx, runCfg)
			return wfdev.WorkflowDoneMsg{Err: err}
		},
	)
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
	path := workflow.StatePath(stateDir, sess.ID)
	st, err := workflow.LoadState(path)
	if err != nil {
		m.appendTranscript(milkTag() + " workflow resume error: " + err.Error() + "\n")
		return m, nil
	}
	if st == nil {
		m.appendTranscript(milkTag() + " no saved workflow state for this session\n")
		return m, nil
	}

	// Rebuild wizard state from the saved checkpoint.
	// If AgentMap is absent (state file predates AgentMap support), enter the
	// agent wizard so the user can supply the roles before resuming.
	if len(st.AgentMap) == 0 {
		w := &workflowWizardState{
			name:     st.WorkflowName,
			task:     st.Task,
			step:     wizardStepDesigner,
			resuming: true,
			sprint:   st.Sprint,
			pass:     st.Pass,
		}
		m.pendingWorkflowWizard = w
		m.appendTranscript(milkTag() + " workflow resume: agent map missing — please specify agents (blank = escalation):\n")
		m.appendTranscript(milkTag() + workflowAgentPrompt("designer"))
		m.refreshPrompt()
		return m, nil
	}
	w := &workflowWizardState{
		name:      st.WorkflowName,
		task:      st.Task,
		designer:  st.AgentMap["designer"],
		generator: st.AgentMap["generator"],
		evaluator: st.AgentMap["evaluator"],
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

	wf := wfdev.NewResume(w.task, maxPasses, sprint, pass, role)
	runCfg := workflow.RunConfig{
		Session:  sess,
		Runners:  runners,
		Send:     send,
		StateDir: stateDir,
	}

	m.workflowPanelOpen = true
	m.busy = true
	m.spinnerFrame = 0
	m.workflowState = &workflow.State{
		WorkflowName: "dev",
		Task:         w.task,
		Sprint:       sprint,
		Pass:         pass,
		Role:         "generator",
		AgentMap:     agentNames,
	}
	m.appendTranscript(milkTag() + fmt.Sprintf(" resuming workflow dev from sprint %d pass %d (designer: %s  generator: %s  evaluator: %s)\n",
		sprint, pass, agentNames["designer"], agentNames["generator"], agentNames["evaluator"]))
	m.syncLayout()

	ctx, cancel := context.WithCancel(m.ctx)
	m.cancelTurn = cancel
	return m, tea.Batch(
		spinnerTick(),
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
