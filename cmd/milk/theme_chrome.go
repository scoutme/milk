package main

import (
	"fmt"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// panelTabs is the short-label table for side panels, left to right (f1–f6
// order): the name a panel's tab carries everywhere tabs are rendered —
// the width-budget overflow strip (panel_tabs.go, #373, both themes per
// ADR-0098). The retired supermilk six-tab bar (ADR-0079) walked this same
// table.
var panelTabs = []struct {
	region panelRegion
	name   string
}{
	{regionMemory, "memory"}, {regionTasks, "tasks"}, {regionBackground, "bg"},
	{regionWorkflow, "flow"}, {regionProject, "files"}, {regionGit, "git"},
}

// panelTabName is a panel's short tab label from that one table — the
// width-budget overflow strip's render and its click hit-test both label
// panels through it, so draw and click can never disagree on a name.
func panelTabName(region panelRegion) string {
	for _, t := range panelTabs {
		if t.region == region {
			return t.name
		}
	}
	return ""
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

// modernToolRows restyles tool-activity rows for a skin: the marker stays
// the shared tool glyph » (ADR-0075 — the font-safe double closing brackets
// the default theme shows in the transcript) and only takes the accent, the
// tool name is bold muted ink, the summary muted. One marker across skins,
// so a transcript reads the same whichever skin paints it.
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
		out := colorize("»", tkAccent.sgrFg()) + " " + colorize(m[1], ansiBold+tkMuted.sgrFg())
		if m[2] != "" {
			out += "  " + colorize(m[2], tkMuted.sgrFg())
		}
		rows[i] = out
	}
	return strings.Join(rows, "\n")
}
