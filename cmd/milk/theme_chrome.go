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

// handleThemeCmd handles /theme [name|list|reload|view|carousel]: bare
// /theme (and /theme carousel) opens the live-preview browse flyover,
// <name> switches the theme live and persists the choice, `list` renders the
// registry with each theme's source, `reload` rescans the theme dirs
// (~/.milk/themes and <project>/.milk/themes) so a drop-in JSON theme
// appears without a restart, and `view` prints a theme's JSON — the
// copy-paste starting point for theme authors. The reserved words win over
// a theme of the same name.
func (m model) handleThemeCmd(arg string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(arg)
	switch {
	case len(fields) == 0, len(fields) == 1 && fields[0] == "carousel":
		return m.openThemeCarousel()
	case len(fields) == 1 && strings.EqualFold(fields[0], "new"):
		return m.openThemeWizard()
	case len(fields) == 1 && fields[0] == "list":
		m.appendTranscript(themeListText())
		return m, nil
	case len(fields) == 1 && fields[0] == "reload":
		loadThemeRegistry(m.themeProjectDir())
		note := fmt.Sprintf("%s theme dirs rescanned: %d themes", milkTag(), len(themeRegistry.Names()))
		if w := themeRegistry.Warnings(); len(w) > 0 {
			note += fmt.Sprintf(" (%d skipped)", len(w))
		}
		m.appendTranscript(note + "\n")
		for _, warn := range themeRegistry.Warnings() {
			m.appendTranscript(milkTag() + " warning: " + warn + "\n")
		}
		return m, nil
	case len(fields) >= 1 && fields[0] == "view":
		name := th.name
		if len(fields) == 2 {
			name = fields[1]
		} else if len(fields) > 2 {
			break // fall through to usage
		}
		body, ok := themeViewText(name)
		if !ok {
			m.appendTranscript(milkTag() + " usage: /theme view [name] — unknown theme\n")
			return m, nil
		}
		m.appendTranscript(milkTag() + " theme " + name + ":\n" + body)
		if !strings.HasSuffix(body, "\n") {
			m.appendTranscript("\n")
		}
		return m, nil
	}
	if len(fields) != 1 {
		m.appendTranscript(milkTag() + " usage: /theme [name|list|reload|view|carousel]\n")
		return m, nil
	}
	arg = fields[0]
	if _, ok := themeDefFor(arg); !ok {
		m.appendTranscript(milkTag() + " usage: /theme [name|list|reload|view|carousel]\n")
		return m, nil
	}
	setTheme(arg)
	// Paint protocol (theme.go): re-anchor first — restore the terminal's
	// own palette and stage the theme's paint for the answer. Painting here
	// (pre-answer) would make the query echo milk's own colour and latch it
	// — the loop that kept off/space light after a supermilk visit.
	restoreTerminalColors()
	m.pendingTerminalApply = true
	m.st.cfg.Theme = th.name
	m.themeChanged()
	if err := saveLocalOrGlobal(m.st.cfg, activeConfigDir(m.st)); err != nil {
		m.appendTranscript(fmt.Sprintf("%s theme: %s (config save failed: %v)\n", milkTag(), bold(th.name), err))
	} else {
		m.notify("theme: "+th.name, "/theme")
	}
	m.syncLayout()
	// The staged paint lands here: the query below is written after the
	// restore above, so the terminal answers with its own ground and the
	// variant pick can never follow milk's previous paint (theme.go).
	return m, tea.Batch(tea.ClearScreen, tea.RequestBackgroundColor, terminalPaintTimeout())
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
