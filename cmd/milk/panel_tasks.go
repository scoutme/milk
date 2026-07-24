package main

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/scoutme/milk/internal/tasks"
)

const tasksPanelWidth = 33 // 32 inner + 1 scrollbar column
const tasksPanelInner = 32

// renderTasksPanel returns a vertical panel string of exactly h lines and
// tasksPanelInner columns (32 chars; scrollbar is rendered separately).
func (m *model) renderTasksPanel(h int) string {
	inner := tasksPanelInner
	if !isTTY {
		return strings.Repeat("\n", h)
	}

	all := buildTasksPanelLines(m.taskStore, inner)
	total := len(all)

	// Clamp offset so we never scroll past the last screenful.
	maxOffset := max(total-h, 0)
	if m.tasksOffset > maxOffset {
		m.tasksOffset = maxOffset
	}

	lines := all[m.tasksOffset:]
	for len(lines) < h {
		lines = append(lines, "")
	}
	lines = lines[:h]

	var rows []string
	for _, line := range lines {
		lineW := utf8.RuneCountInString(stripANSI(line))
		if lineW < inner {
			line += strings.Repeat(" ", inner-lineW)
		}
		rows = append(rows, line)
	}
	return strings.Join(rows, "\n")
}

func buildTasksPanelLines(ts *tasks.Store, inner int) []string {
	var lines []string
	addLine := func(s string) { lines = append(lines, s) }
	hr := func() { addLine(stylePanelSection.Render(strings.Repeat("─", inner))) }

	addLine(stylePanelTitle.Render("tasks"))
	hr()

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

	hr()
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
