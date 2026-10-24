package main

import (
	"fmt"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// panelTabStrip is the themed title row of every side panel: the panel tabs,
// the open one in the accent colour and underlined.
func panelTabStrip(region panelRegion, inner int) string {
	tabs := []struct {
		region panelRegion
		name   string
	}{
		{regionMemory, "memory"}, {regionTasks, "tasks"}, {regionBackground, "jobs"},
		{regionWorkflow, "flow"}, {regionProject, "files"}, {regionGit, "git"},
	}
	var b strings.Builder
	for i, t := range tabs {
		if i > 0 {
			b.WriteString(" ")
		}
		if t.region == region {
			b.WriteString(colorize(t.name, ansiBold+"\033[4m"+tkAccent.sgrFg()))
		} else {
			b.WriteString(colorize(t.name, tkMuted.sgrFg()))
		}
	}
	return ansi.Truncate(b.String(), inner, "")
}

// handleThemeCmd handles /theme [off|supermilk]: switches the theme live,
// pushes its colours into the terminal and persists the choice.
func (m model) handleThemeCmd(arg string) (tea.Model, tea.Cmd) {
	if arg == "" {
		m.appendTranscript(fmt.Sprintf("%s theme: %s  (use /theme off|supermilk)\n", milkTag(), bold(th.name)))
		return m, nil
	}
	if _, ok := themes[strings.ToLower(arg)]; !ok {
		m.appendTranscript(milkTag() + " usage: /theme [off|supermilk]\n")
		return m, nil
	}
	if th.modern {
		restoreTerminalColors()
	}
	setTheme(arg)
	applyThemeTerminal()
	m.st.cfg.Theme = th.name
	m.colorizeForce = true
	m.viewportDirty = true
	if err := saveLocalOrGlobal(m.st.cfg, activeConfigDir(m.st)); err != nil {
		m.appendTranscript(fmt.Sprintf("%s theme: %s (config save failed: %v)\n", milkTag(), bold(th.name), err))
	} else {
		m.notify("theme: "+th.name, "/theme")
	}
	m.syncLayout()
	// The skin paints the terminal background, so the detected light/dark
	// follows the theme: ask again once the colors changed (switching back to
	// "off" must not keep the cream skin's "light" verdict on a dark terminal).
	return m, tea.Batch(tea.ClearScreen, tea.RequestBackgroundColor)
}

// toolRowRe matches a tool-activity row as the agents emit it: "» name: summary"
// inside whatever dim/colour codes wrap it. "⚙" still matches rows seeded from
// sessions written before ADR-0075.
var toolRowRe = regexp.MustCompile(`^(?:\x1b\[[0-9;]*m)*[⚙»] ([^:\x1b]+?)(?:: (.*?))?(?:\x1b\[[0-9;]*m)*$`)

// modernToolRows restyles tool-activity rows for a skin: the marker becomes a
// small accent glyph, the tool name is bold muted ink, the summary muted.
// Render-only; the transcript buffer is untouched.
func modernToolRows(content string) string {
	if !strings.Contains(content, "⚙ ") && !strings.Contains(content, "» ") {
		return content
	}
	rows := strings.Split(content, "\n")
	for i, row := range rows {
		m := toolRowRe.FindStringSubmatch(row)
		if m == nil {
			continue
		}
		out := colorize("▸", tkAccent.sgrFg()) + " " + colorize(m[1], ansiBold+tkMuted.sgrFg())
		if m[2] != "" {
			out += "  " + colorize(m[2], tkMuted.sgrFg())
		}
		rows[i] = out
	}
	return strings.Join(rows, "\n")
}
