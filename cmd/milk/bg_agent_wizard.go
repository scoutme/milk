package main

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// bgAgentWizardState is the one-question "which agent runs this background
// job?" prompt, the background-job counterpart of the /workflow role wizard.
// It renders through the agent-choice flyover (picker.go, #335/#369): the
// candidates are listed with the default preselected, arrows move, typed text
// filters or overrides, Enter accepts — and untouched for
// bgAgentPickerTimeout the timed default answers blank itself.
type bgAgentWizardState struct {
	task     string
	names    []string  // agents a job can run on, for validation and the prompt
	def      string    // what a blank answer resolves to
	sel      int       // flyover cursor (within the currently filtered rows)
	deadline time.Time // timed default; fires only while untouched
	disarmed bool      // any key: a person owns the choice now
}

// startUserBackgroundAgent is the entry for user-started jobs (/bg start,
// Ctrl+Enter while busy). A non-blank spec (from --agent) spawns right away;
// otherwise, when there is a real choice between agents, it asks — blank
// keeps the default. Opening the prompt arms the timed default's tick.
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
	w.sel = pickerDefaultIndex(agentPickerEntries(names, w.def))
	w.deadline = time.Now().Add(bgAgentPickerTimeout)
	m.ta.Reset()
	m.leadingPasted = false
	m.busyHint = ""
	m.pendingBgAgent = w
	m.appendTranscript(milkTag() + bgAgentPrompt(w))
	m.refreshPrompt()
	m.syncLayout()
	return m, bgAgentPickerTick(w.deadline)
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

// bgAgentDisplay is the transcript record for an accepted answer: the answer
// itself, or the highlighted row's name when the answer is blank — the record
// should read "gemma", not an empty echo.
func (w *bgAgentWizardState) display(entries []paletteEntry, typed, answer string) string {
	if answer != "" {
		return answer
	}
	if h := agentPickerHighlight(entries, typed, w.sel); h != "" {
		return h
	}
	return w.def
}

func (m model) handleBgAgentWizardKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	w := m.pendingBgAgent
	// Any key at all = a person is here: the timed default never fires over
	// an answer in progress (esc/enter end the prompt outright anyway).
	w.disarmed = true
	switch msg.String() {
	case "ctrl+c", "esc":
		m.pendingBgAgent = nil
		m.ta.Reset()
		m.appendTranscript("\n" + milkTag() + " background job cancelled\n")
		m.refreshPrompt()
		m.syncLayout()
		return m, nil
	case "up", "ctrl+p", "shift+tab", "down", "ctrl+n", "tab":
		filtered := pickerFilter(agentPickerEntries(w.names, w.def), m.ta.Value())
		delta := 1
		if msg.String() == "up" || msg.String() == "ctrl+p" || msg.String() == "shift+tab" {
			delta = -1
		}
		w.sel = movePickerSel(w.sel, delta, len(filtered))
		return m, nil
	case "enter", "ctrl+m":
		entries := agentPickerEntries(w.names, w.def)
		typed := strings.TrimSpace(m.ta.Value())
		answer := agentPickerAnswer(entries, typed, w.sel)
		display := w.display(entries, typed, answer)
		m.ta.Reset()
		m.syncLayout()
		m.appendTranscript(promptLabel(m.st) + display + "\n")
		if !w.validChoice(answer) {
			m.appendTranscript(milkTag() + fmt.Sprintf(" unknown agent %q —", answer) + bgAgentPrompt(w))
			return m, nil
		}
		m.pendingBgAgent = nil
		m.refreshPrompt()
		return m.spawnUserBackgroundAgent(w.task, answer)
	}
	cmd := m.updateTA(msg)
	// Typed text narrows the flyover rows — palette semantics: the cursor
	// starts at the top of what it filtered.
	if msg.Text != "" || msg.String() == "backspace" {
		w.sel = 0
	}
	m.syncLayout()
	return m, cmd
}
