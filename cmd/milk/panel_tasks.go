package main

import (
	"fmt"
	"unicode/utf8"

	"github.com/scoutme/milk/internal/tasks"
)

const tasksPanelWidth = 33 // 32 inner + 1 right scrollbar column
const tasksPanelInner = 32

// renderTasksPanel returns a vertical panel string of exactly h lines and
// tasksPanelInner columns (scrollbar is rendered separately by
// renderTasksPanelScrollbar). Body shared with every other side panel — see
// renderSidePanel.
func (m *model) renderTasksPanel(h int) string {
	return m.renderSidePanel(regionTasks, h)
}

// renderTasksPanelScrollbar returns a 1-column string of h lines: a dim │
// track with a ▌ thumb when the panel content overflows, or a blank column
// otherwise. Shared with every other side panel — see renderSidePanelScrollbar.
func (m *model) renderTasksPanelScrollbar(h int) string {
	return m.renderSidePanelScrollbar(regionTasks, h)
}

func buildTasksPanelLines(ts *tasks.Store, inner int) []string {
	var lines []string
	addLine := func(s string) { lines = append(lines, s) }

	addLine(stylePanelTitle.Render("tasks"))
	addLine("")

	if ts == nil {
		addLine(dim("(unavailable)"))
		return lines
	}

	sessionTasks, _ := ts.List(tasks.ListOpts{})
	globalTasks, _ := ts.List(tasks.ListOpts{IncludeGlobal: true})

	// Separate global-only tasks (those not in session).
	sessionIDs := map[string]bool{}
	for _, t := range sessionTasks {
		sessionIDs[t.ID] = true
	}
	var globalOnly []tasks.Task
	for _, t := range globalTasks {
		if !sessionIDs[t.ID] {
			globalOnly = append(globalOnly, t)
		}
	}

	addLine(stylePanelSection.Render("SESSION"))
	if len(sessionTasks) == 0 {
		addLine(dim("  (none)"))
	}
	for _, t := range sessionTasks {
		addLine(renderTaskLine(t, inner))
	}

	addLine("")
	addLine(stylePanelSection.Render("GLOBAL"))
	if len(globalOnly) == 0 {
		addLine(dim("  (none)"))
	}
	for _, t := range globalOnly {
		addLine(renderTaskLine(t, inner))
	}

	return lines
}

func renderTaskLine(t tasks.Task, inner int) string {
	badge := taskStatusBadge(t.Status)
	title := t.Title
	prefix := fmt.Sprintf("  %s ", badge)
	prefixW := utf8.RuneCountInString(stripANSI(prefix))
	maxTitleW := inner - prefixW
	if maxTitleW < 4 {
		maxTitleW = 4
	}
	titleRunes := []rune(title)
	if len(titleRunes) > maxTitleW {
		title = string(titleRunes[:maxTitleW-1]) + "…"
	}
	return prefix + title
}

func taskStatusBadge(status string) string {
	switch status {
	case tasks.StatusDone:
		return dim("✓")
	case tasks.StatusInProgress:
		return green("▶")
	case tasks.StatusBlocked:
		return red("✗")
	default: // pending
		return dim("○")
	}
}
