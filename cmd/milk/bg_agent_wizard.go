package main

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// bgAgentWizardState is the one-question "which agent runs this background
// job?" prompt, the background-job counterpart of the /workflow role wizard.
type bgAgentWizardState struct {
	task  string
	names []string // agents a job can run on, for validation and the prompt
	def   string   // what a blank answer resolves to
}

// startUserBackgroundAgent is the entry for user-started jobs (/bg start,
// Ctrl+Enter while busy). A non-blank spec (from --agent) spawns right away;
// otherwise, when there is a real choice between agents, it asks — blank
// keeps the default.
func (m model) startUserBackgroundAgent(task, spec string) (tea.Model, tea.Cmd) {
	if m.pendingBgAgent != nil {
		return m, nil
	}
	names := bgAgentCandidates(m.st.cfg)
	if strings.TrimSpace(spec) != "" || len(names) < 2 {
		return m.spawnUserBackgroundAgent(task, spec)
	}
	escAvail := m.agents.escalationLocal != nil
	w := &bgAgentWizardState{
		task:  task,
		names: names,
		def:   userJobDefaultName(m.st.cfg, escAvail),
	}
	m.ta.Reset()
	m.leadingPasted = false
	m.busyHint = ""
	m.pendingBgAgent = w
	m.appendTranscript(milkTag() + bgAgentPrompt(w))
	m.refreshPrompt()
	m.syncLayout()
	return m, nil
}

func bgAgentPrompt(w *bgAgentWizardState) string {
	return fmt.Sprintf(" background agent [%s, or primary/escalation] (blank = %s):\n", strings.Join(w.names, ", "), w.def)
}

// validBgAgentChoice reports whether answer names a usable agent (blank is
// always fine — it means the default).
func (w *bgAgentWizardState) validChoice(answer string) bool {
	if answer == "" || strings.EqualFold(answer, "primary") || strings.EqualFold(answer, "escalation") {
		return true
	}
	for _, n := range w.names {
		if strings.EqualFold(n, answer) {
			return true
		}
	}
	return false
}

func (m model) handleBgAgentWizardKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		m.pendingBgAgent = nil
		m.ta.Reset()
		m.appendTranscript("\n" + milkTag() + " background job cancelled\n")
		m.refreshPrompt()
		m.syncLayout()
		return m, nil
	case "enter", "ctrl+m":
		w := m.pendingBgAgent
		input := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()
		m.syncLayout()
		m.appendTranscript(promptLabel(m.st) + input + "\n")
		if !w.validChoice(input) {
			m.appendTranscript(milkTag() + fmt.Sprintf(" unknown agent %q —", input) + bgAgentPrompt(w))
			return m, nil
		}
		m.pendingBgAgent = nil
		m.refreshPrompt()
		return m.spawnUserBackgroundAgent(w.task, input)
	}
	cmd := m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}
