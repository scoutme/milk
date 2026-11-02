package main

import (
	"fmt"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// panelTabs is the tab bar of a tabbed theme (ADR-0079), left to right:
// one tab per side panel — the strip is both the panel's title row and the
// only navigation between the tabs of the shared panel slot, so the render
// (panelTabStrip) and the click hit-test (panelTabAt) share this one table.
var panelTabs = []struct {
	region panelRegion
	name   string
}{
	{regionMemory, "memory"}, {regionTasks, "tasks"}, {regionBackground, "bg"},
	{regionWorkflow, "flow"}, {regionProject, "files"}, {regionGit, "git"},
}

// panelTabOrder is the panelTabs sequence as a region list: the fixed
// f1–f6 order every strip renders in — labels never reorder on selection
// (tabbed bar and budget strip alike; selection is the active label's
// styling). panelTabStrip/panelTabAt walk it directly.
var panelTabOrder = func() []panelRegion {
	order := make([]panelRegion, 0, len(panelTabs))
	for _, t := range panelTabs {
		order = append(order, t.region)
	}
	return order
}()

// panelTabStripIn renders the tab strip for an explicit label order: the
// active one in the accent colour and underlined, the rest muted, one space
// apart. Order-agnostic by construction — render and hit-test pass the same
// order (the fixed table for the tabbed bar, ADR-0079; the budget strip's
// group, issue #373), so what is drawn and what is clickable can never
// disagree.
func panelTabStripIn(order []panelRegion, active panelRegion, inner int) string {
	var b strings.Builder
	for i, r := range order {
		if i > 0 {
			b.WriteString(" ")
		}
		if r == active {
			b.WriteString(colorize(panelTabName(r), ansiBold+"\033[4m"+tkAccent.sgrFg()))
		} else {
			b.WriteString(colorize(panelTabName(r), tkMuted.sgrFg()))
		}
	}
	return ansi.Truncate(b.String(), inner, "")
}

// panelTabStrip is the themed title row of the side panel: the panel tabs,
// the open one in the accent colour and underlined. On a tabbed theme the
// panels share one slot, so this strip is the single tab bar over it —
// always the fixed f1–f6 table order: labels never reorder on selection,
// the live render uses this exact row.
func panelTabStrip(region panelRegion, inner int) string {
	return panelTabStripIn(panelTabOrder, region, inner)
}

// panelTabAtIn maps a column offset within the strip (0-based from the tab
// bar's left edge) to the tab whose label covers it — regionNone on a gap or
// past the strip. Walks the same single-space join panelTabStripIn renders
// with, so the click target can never drift from what is drawn. Callers pass
// the order that was rendered (the fixed table, or the budget strip's
// group).
func panelTabAtIn(order []panelRegion, col int) panelRegion {
	x := 0
	for _, r := range order {
		w := len([]rune(panelTabName(r)))
		if col >= x && col < x+w {
			return r
		}
		x += w + 1
	}
	return regionNone
}

// panelTabAt is the table-order hit-test wrapper (see panelTabAtIn).
func panelTabAt(col int) panelRegion {
	return panelTabAtIn(panelTabOrder, col)
}

// panelTabName is a panel's short tab label from the same single table — the
// tabbed bar's render (panelTabStrip), its hit-test (panelTabAt) and the
// width-budget overflow strip (panel_tabs.go, issue #373) all label panels
// through it, so the three tab surfaces can never disagree on a name.
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
	// Tabs are exclusive (ADR-0079): arriving from a sibling (theme "off")
	// layout, only the front panel survives — the rest would be hidden-open
	// siblings under a tab bar that cannot show them side by side.
	m.enforceTabbedPanelExclusivity()
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
