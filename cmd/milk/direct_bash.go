package main

import (
	tea "github.com/charmbracelet/bubbletea"
)

// handleDirectBashKey handles y/N keypresses while pendingDirectBash is set.
// 'y' or Enter launches the command in an embedded PTY pane; anything else
// (including 'n', Esc, Ctrl+C) routes the original input to the agent.
func (m model) handleDirectBashKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	cmd := *m.pendingDirectBash
	m.pendingDirectBash = nil

	switch msg.String() {
	case "y", "Y", "enter":
		m.appendTranscript("y\n")
		return m.launchPTYPane(cmd)
	default:
		// User declined — route to agent as normal input.
		m.appendTranscript("n\n")
		m.refreshPrompt()
		return m.dispatchAgent(cmd)
	}
}
