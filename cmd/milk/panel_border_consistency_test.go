package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/scoutme/milk/internal/session"
)

// TestSidePanels_AllHaveConsistentWidthAndScrollbar guards against the bug
// report that panels were inconsistent: only the workflow panel had a left
// border, and tasks/background had no scrollbar to show scroll position. The
// fix settled on no left border anywhere — just a right-hand scrollbar
// column (dim │ track / ▌ thumb) on every panel, matching what the memory
// panel already did. Every panel must render exactly its inner width with
// no left-border character, and every panel must expose a same-height
// scrollbar column.
func TestSidePanels_AllHaveConsistentWidthAndScrollbar(t *testing.T) {
	old := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = old })

	m := &model{st: &interactiveState{sess: &session.Session{}}}
	h := 5

	panels := map[string]struct {
		rendered string
		inner    int
	}{
		"memory":     {m.renderMemoryPanel(h), memoryPanelInner},
		"tasks":      {m.renderTasksPanel(h), tasksPanelInner},
		"background": {m.renderBackgroundPanel(h), backgroundPanelInner},
		"workflow":   {m.renderWorkflowPanel(h), workflowPanelInner},
	}
	for name, p := range panels {
		lines := strings.Split(p.rendered, "\n")
		if len(lines) != h {
			t.Errorf("%s panel: got %d lines, want %d", name, len(lines), h)
		}
		for i, line := range lines {
			if strings.HasPrefix(line, "│") {
				t.Errorf("%s panel line %d: unexpected left border, got %q", name, i, line)
			}
			if w := utf8.RuneCountInString(stripANSI(line)); w != p.inner {
				t.Errorf("%s panel line %d: width %d, want %d", name, i, w, p.inner)
			}
		}
	}

	widths := map[string]struct{ total, inner int }{
		"memory":     {memoryPanelWidth, memoryPanelInner},
		"tasks":      {tasksPanelWidth, tasksPanelInner},
		"background": {backgroundPanelWidth, backgroundPanelInner},
		"workflow":   {workflowPanelWidth, workflowPanelInner},
	}
	for name, w := range widths {
		if w.total != w.inner+1 {
			t.Errorf("%s panel: width %d should be inner (%d) + 1 scrollbar column", name, w.total, w.inner)
		}
	}

	scrollbars := map[string]string{
		"memory":     m.renderPanelScrollbar(h),
		"tasks":      m.renderTasksPanelScrollbar(h),
		"background": m.renderBackgroundPanelScrollbar(h),
		"workflow":   m.renderWorkflowPanelScrollbar(h),
	}
	for name, bar := range scrollbars {
		if got := strings.Count(bar, "\n") + 1; got != h {
			t.Errorf("%s scrollbar: got %d rows, want %d", name, got, h)
		}
	}
}
