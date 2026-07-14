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
	name      string // workflow name (e.g. "dev")
	task      string
	designer  string
	generator string
	evaluator string
	step      workflowWizardStep
	clearing  bool // true when this wizard is a /workflow clear confirmation
}

type workflowWizardStep int

const (
	wizardStepTask         workflowWizardStep = iota // ask for task description
	wizardStepClearConfirm                           // ask user to type "clear" to confirm
	wizardStepDone
)

// handleWorkflowCmd dispatches /workflow [name] [args...].
func (m model) handleWorkflowCmd(args string) (tea.Model, tea.Cmd) {
	parts := strings.Fields(args)

	if len(parts) == 0 {
		// List available workflows.
		m.appendTranscript(milkTag() + " available workflows:\n  dev — designer → generator → evaluator loop\n\nUsage:\n  /workflow dev [task] [--designer <agent>] [--generator <agent>] [--evaluator <agent>]\n  /workflow resume — restore saved state for this session\n  /workflow clear  — delete saved state for this session\n")
		return m, nil
	}

	name := parts[0]
	if name == "resume" {
		return m.handleWorkflowResume()
	}
	if name == "clear" {
		return m.handleWorkflowClear()
	}
	if name != "dev" {
		m.appendTranscript(milkTag() + fmt.Sprintf(" unknown workflow %q — available: dev, clear\n", name))
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

	// If task is missing, enter wizard for task only; agent defaults are applied after.
	if wizard.task == "" {
		wizard.step = wizardStepTask
		m.pendingWorkflowWizard = wizard
		m.appendTranscript(milkTag() + " workflow dev — enter task description:\n")
		m.refreshPrompt()
		return m, nil
	}

	// Default any unspecified agent roles to escalation and launch immediately.
	if wizard.designer == "" {
		wizard.designer = workflow.AliasEscalation
	}
	if wizard.generator == "" {
		wizard.generator = workflow.AliasEscalation
	}
	if wizard.evaluator == "" {
		wizard.evaluator = workflow.AliasEscalation
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
			// Blank task is not valid — re-prompt.
			m.appendTranscript(milkTag() + " task description cannot be empty — enter task description:\n")
			m.refreshPrompt()
			return m, nil
		}
		w.task = input
		// Default all three agent roles to escalation and launch immediately.
		w.designer = workflow.AliasEscalation
		w.generator = workflow.AliasEscalation
		w.evaluator = workflow.AliasEscalation
		w.step = wizardStepDone
	}

	m.pendingWorkflowWizard = nil
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

	runners, err := buildWorkflowRunners(agentNames, cfg, sess, m.st.mem, &m.agents)
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

// handleWorkflowResume loads the saved workflow state for the current session
// and restores panel state. It does not re-launch the workflow goroutine;
// re-launch requires the user to run /workflow dev <task> again. The state
// file path is printed so the user knows where the checkpoint lives.
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
	m.workflowState = st
	m.workflowPanelOpen = true
	m.syncLayout()
	rerunHint := "/workflow dev <task>"
	if st.Task != "" {
		rerunHint = "/workflow dev " + st.Task
	}
	m.appendTranscript(fmt.Sprintf(
		"%s workflow %s restored (sprint %d pass %d role %s)\n  state file: %s\n  to re-run: %s\n",
		milkTag(), st.WorkflowName, st.Sprint, st.Pass, st.Role, path, rerunHint,
	))
	return m, nil
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
