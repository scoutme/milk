package main

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Agent-choice flyover (issues #335/#369): the "which agent runs this?"
// questions — the background-job picker (Ctrl+Enter, /bg start) and the
// workflow wizard's role-assignment step — were a transcript line easy to
// miss while output streams (issue #369's "barely visible"). They keep that
// line as the record and gain a palette-style floating face: options listed
// with the blank-equivalent preselected, arrows to move, typed text to
// filter or override, Enter to accept, Esc to cancel. The background-job
// picker additionally arms a timed default — untouched for
// bgAgentPickerTimeout it answers blank (the default agent) itself, so a
// fork asked for and then walked away from still runs; any key takes the
// picker over and disarms the deadline (the ADR-0052 timed-answer shape,
// applied to this one question). Every other wizard step stays typed on
// purpose: task descriptions and "type clear" confirmations must not be
// arrow-selectable (the #335 coherence audit lists the dispositions).

const (
	// bgAgentPickerTimeout is the background-job picker's timed default:
	// no key at all for this long and the default agent runs. One key of
	// any kind disarms it for good — the deadline only ever fires into
	// total silence, never over an answer in progress.
	bgAgentPickerTimeout = 60 * time.Second
)

// bgAgentPickerTickMsg re-checks the background-job picker's deadline.
type bgAgentPickerTickMsg struct{}

// bgAgentPickerTick arms the next deadline check.
func bgAgentPickerTick(deadline time.Time) tea.Cmd {
	d := time.Until(deadline)
	if d <= 0 {
		d = time.Millisecond
	}
	return tea.Tick(d, func(time.Time) tea.Msg { return bgAgentPickerTickMsg{} })
}

// agentPickerEntries builds the option rows for an agent-choice question: the
// configured candidates, the primary/escalation aliases, and the
// blank-equivalent answer (the default / keep-current value), which carries
// the "(default)" hint and the preselection even when it names an agent
// outside the candidate list — an external-process background_agent can only
// be chosen by blank, so it must be selectable anyway.
func agentPickerEntries(names []string, blankEquiv string) []paletteEntry {
	var entries []paletteEntry
	seen := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[strings.ToLower(name)] {
			return
		}
		seen[strings.ToLower(name)] = true
		entries = append(entries, paletteEntry{name: name})
	}
	for _, n := range names {
		add(n)
	}
	add("primary")
	add("escalation")
	add(blankEquiv)
	for i := range entries {
		if strings.EqualFold(entries[i].name, blankEquiv) {
			entries[i].hint = "(default)"
			break
		}
	}
	return entries
}

// pickerDefaultIndex is the preselection: the "(default)" row, else first.
func pickerDefaultIndex(entries []paletteEntry) int {
	for i, e := range entries {
		if e.hint != "" {
			return i
		}
	}
	return 0
}

// pickerFilter narrows rows to the typed text: prefix matches first, then
// substring — the palette's ranking, for agents.
func pickerFilter(entries []paletteEntry, typed string) []paletteEntry {
	needle := strings.ToLower(strings.TrimSpace(typed))
	if needle == "" {
		return entries
	}
	var prefix, contains []paletteEntry
	for _, e := range entries {
		name := strings.ToLower(e.name)
		switch {
		case strings.HasPrefix(name, needle):
			prefix = append(prefix, e)
		case strings.Contains(name, needle):
			contains = append(contains, e)
		}
	}
	return append(prefix, contains...)
}

// agentPickerHighlight is the row shown selected for the current typed text
// and cursor: the filtered list's row at sel, clamped. Empty when nothing
// matches the typed text.
func agentPickerHighlight(entries []paletteEntry, typed string, sel int) string {
	f := pickerFilter(entries, typed)
	if len(f) == 0 {
		return ""
	}
	return f[max(0, min(sel, len(f)-1))].name
}

// movePickerSel steps the cursor within n rows.
func movePickerSel(sel, delta, n int) int {
	if n <= 0 {
		return 0
	}
	return max(0, min(sel+delta, n-1))
}

// agentPickerAnswer resolves Enter for both agent-choice pickers. Typed text
// stands as given when it matches nothing: an exact option name answers that
// option, a narrowing prefix/substring answers the highlighted row of what it
// filtered (palette-style filtering), and text matching nothing stays raw for
// the wizard to validate. Blank input answers the highlighted row. In every
// case the blank-equivalent row — the "(default)" hint — answers "" so the
// wizard's own blank semantics (default agent / keep current / escalation)
// run unchanged.
func agentPickerAnswer(entries []paletteEntry, typed string, sel int) string {
	blank := ""
	for _, e := range entries {
		if e.hint != "" {
			blank = e.name
			break
		}
	}
	mapName := func(name string) string {
		if name == "" || strings.EqualFold(name, blank) {
			return ""
		}
		return name
	}
	typed = strings.TrimSpace(typed)
	if typed == "" {
		return mapName(agentPickerHighlight(entries, "", sel))
	}
	for _, e := range entries {
		if strings.EqualFold(e.name, typed) {
			return mapName(e.name)
		}
	}
	if f := pickerFilter(entries, typed); len(f) > 0 {
		return mapName(agentPickerHighlight(entries, typed, sel))
	}
	return typed
}

// agentPickerOpen reports whether a choice step needs the flyover: the
// agent questions or the bare /plugins marketplace browse.
func (m *model) agentPickerOpen() bool {
	if m.pendingBgAgent != nil || m.pendingPluginBrowse != nil {
		return true
	}
	w := m.pendingWorkflowWizard
	return w != nil && w.step == wizardStepGenericRole && w.roleIdx < len(w.roles)
}

// rolePickerEntries is agentPickerEntries for the workflow wizard's current
// role step: candidates plus aliases, blank-equivalent preselected.
func (m model) rolePickerEntries(w *workflowWizardState) []paletteEntry {
	// st is nil in hand-built model fixtures (workflow_cmd_test's advance
	// tests) — an absent config just means no candidates beyond the aliases.
	if m.st == nil || w.roleIdx >= len(w.roles) {
		return nil
	}
	return agentPickerEntries(bgAgentCandidates(m.st.cfg), w.roleBlankEquiv())
}

// overlayAgentPicker draws the open agent-choice flyover over the main area,
// centered near the top — the same placement as the command palette.
func (m *model) overlayAgentPicker(block string) string {
	if !m.agentPickerOpen() {
		return block
	}
	width := m.mainWidth()
	boxWidth := min(paletteMaxWidth, width-4)
	if boxWidth < paletteMinWidth {
		return block
	}
	box := m.renderAgentPicker(boxWidth)
	return composeOverlays(block, overlayLayer{content: box, x: centeredX(width, boxWidth), y: paletteTopRow})
}

// renderAgentPicker renders the flyover box for the open agent-choice step,
// or "" when none is open. The typed line mirrors the palette's query row so
// filtering is visible; the timed default (background job only) shows as a
// footer line while armed.
func (m *model) renderAgentPicker(boxWidth int) string {
	inner := boxWidth - 4 // border + one column of padding per side
	var title string
	var entries []paletteEntry
	sel := 0
	typed := m.ta.Value()

	switch {
	case m.pendingBgAgent != nil:
		w := m.pendingBgAgent
		entries = agentPickerEntries(w.names, w.def)
		sel = w.sel
		title = lipgloss.NewStyle().Bold(true).Render("background agent") +
			dim("  ↑↓ choose · enter accept · type to filter · esc cancel")
	case m.pendingPluginBrowse != nil:
		// Before the workflow case below: agentPickerOpen() covers this state
		// too, and that case dereferences the wizard state.
		w := m.pendingPluginBrowse
		entries = browseEntries(w.choices)
		sel = w.sel
		title = lipgloss.NewStyle().Bold(true).Render("install from marketplaces") +
			dim("  ↑↓ choose · enter install · type to filter · esc cancel")
	case m.agentPickerOpen():
		w := m.pendingWorkflowWizard
		role := w.roles[w.roleIdx]
		entries = m.rolePickerEntries(w)
		sel = w.roleSel
		title = lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("workflow %s — %s agent", w.name, role)) +
			dim("  ↑↓ choose · enter accept · type to filter · esc cancel")
	default:
		return ""
	}

	filtered := pickerFilter(entries, typed)
	sel = movePickerSel(sel, 0, len(filtered))

	lines := []string{title, "› " + typed + "▏"}
	start := 0
	if sel >= paletteMaxRows {
		start = sel - paletteMaxRows + 1
	}
	end := min(start+paletteMaxRows, len(filtered))
	for i := start; i < end; i++ {
		lines = append(lines, paletteRow(filtered[i], inner, i == sel))
	}
	if len(filtered) == 0 {
		if m.pendingPluginBrowse != nil {
			lines = append(lines, dim("no marketplace matches"))
		} else {
			lines = append(lines, dim("no agent matches"))
		}
	}
	if w := m.pendingBgAgent; w != nil && !w.disarmed && !w.deadline.IsZero() {
		secs := int(time.Until(w.deadline).Seconds() + 0.999)
		if secs < 1 {
			secs = 1
		}
		lines = append(lines, dim(fmt.Sprintf("auto-picks %s in %ds — any key takes over", w.def, secs)))
	}

	body := strings.Join(lines, "\n")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(tkAccent.adaptive()).
		Padding(0, 1).
		Width(boxWidth).
		Render(body)
}
