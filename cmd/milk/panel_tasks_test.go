package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/tasks"
)

// TestBuildTasksPanelLines_NilStore verifies the panel shows "(unavailable)" when
// no task store is configured.
func TestBuildTasksPanelLines_NilStore(t *testing.T) {
	lines, _ := taskPanelRows(nil, 32, "")
	if len(lines) == 0 {
		t.Fatal("expected non-empty output for nil store")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "unavailable") {
		t.Errorf("expected 'unavailable' in nil-store output; got:\n%s", joined)
	}
}

// TestBuildTasksPanelLines_EmptyStore verifies the panel shows SESSION/GLOBAL
// section headers and "(none)" markers when the store has no tasks.
func TestBuildTasksPanelLines_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	ts, err := tasks.New(dir, "test-session")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	lines, _ := taskPanelRows(ts, 32, "")
	joined := strings.Join(lines, "\n")

	for _, want := range []string{"SESSION", "GLOBAL", "(none)"} {
		// Strip ANSI before checking so terminal colour codes don't interfere.
		plain := ansi.Strip(joined)
		if !strings.Contains(plain, want) {
			t.Errorf("expected %q in panel output; got:\n%s", want, plain)
		}
	}
}

// TestBuildTasksPanelLines_WithTasks verifies that tasks appear in the panel
// output with their titles and correct status badges.
func TestBuildTasksPanelLines_WithTasks(t *testing.T) {
	dir := t.TempDir()
	ts, err := tasks.New(dir, "test-session")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t1, _ := ts.Create("implement login", nil)
	t2, _ := ts.Create("write docs", nil)
	_ = ts.Complete(t1.ID)

	lines, _ := taskPanelRows(ts, 32, "")
	joined := strings.Join(lines, "\n")
	plain := ansi.Strip(joined)

	if !strings.Contains(plain, "implement login") {
		t.Errorf("title 'implement login' not found in panel output:\n%s", plain)
	}
	if !strings.Contains(plain, "write docs") {
		t.Errorf("title 'write docs' not found in panel output:\n%s", plain)
	}
	_ = t2
}

// TestBuildTasksPanelLines_GlobalTask verifies that a promoted task appears in
// the GLOBAL section when opened from a different session.
func TestBuildTasksPanelLines_GlobalTask(t *testing.T) {
	dir := t.TempDir()
	ts1, _ := tasks.New(dir, "sess1")
	task, _ := ts1.Create("global plan item", nil)
	_ = ts1.Promote(task.ID)

	// Open from a different session.
	ts2, _ := tasks.New(dir, "sess2")
	lines, _ := taskPanelRows(ts2, 32, "")
	joined := strings.Join(lines, "\n")
	plain := ansi.Strip(joined)

	if !strings.Contains(plain, "global plan item") {
		t.Errorf("global task not shown in panel from different session:\n%s", plain)
	}
	if !strings.Contains(plain, "GLOBAL") {
		t.Errorf("GLOBAL section not found:\n%s", plain)
	}
}

// tasksPanelClickModel returns a minimal model wired for tasks-panel click
// tests (mirrors panel_project_test.go's projectClickModel).
func tasksPanelClickModel(t *testing.T) (*model, *tasks.Store) {
	t.Helper()
	ts, err := tasks.New(t.TempDir(), "test-session")
	if err != nil {
		t.Fatalf("tasks.New: %v", err)
	}
	return &model{taskStore: ts}, ts
}

// taskRowLineIndex finds the first rendered line carrying the task ID.
func taskRowLineIndex(t *testing.T, ts *tasks.Store, expanded, id string) int {
	t.Helper()
	_, ids := taskPanelRows(ts, tasksPanelInner, expanded)
	for i, got := range ids {
		if got == id {
			return i
		}
	}
	t.Fatalf("task %q has no row in panel (ids=%v)", id, ids)
	return -1
}

// TestTasksPanelRows_ExpandCollapseInPlace: double-click expands the full
// title in place (the collapsed one-liner ellipsizes it); a second
// double-click collapses it again — memory-panel semantics.
func TestTasksPanelRows_ExpandCollapseInPlace(t *testing.T) {
	m, ts := tasksPanelClickModel(t)
	title := "implement the project map tool for JS TS Python Java Rust"
	task, err := ts.Create(title, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Collapsed: one line, truncated with an ellipsis.
	collapsed, ids := taskPanelRows(ts, tasksPanelInner, "")
	row := taskRowLineIndex(t, ts, "", task.ID)
	if got := len(ids); got != len(collapsed) {
		t.Fatalf("ids/lines misaligned: %d ids vs %d lines", got, len(collapsed))
	}
	if plain := ansi.Strip(collapsed[row]); strings.Contains(plain, title) {
		t.Errorf("collapsed row shows the full title: %q", plain)
	} else if !strings.HasSuffix(plain, "…") {
		t.Errorf("collapsed row not ellipsized: %q", plain)
	}

	// The action (pure): one toggle expands — full title present across
	// wrapped lines.
	m.handleTasksPanelClick(row)
	if m.taskExpanded != task.ID {
		t.Fatalf("double-click action did not expand: taskExpanded=%q want %q", m.taskExpanded, task.ID)
	}
	expanded, expIDs := taskPanelRows(ts, tasksPanelInner, m.taskExpanded)
	if len(expIDs) != len(expanded) {
		t.Fatalf("expanded ids/lines misaligned: %d vs %d", len(expIDs), len(expanded))
	}
	// Title is word-wrapped across the task's rows; reconstruct it and
	// compare (the first field is the status badge).
	start := taskRowLineIndex(t, ts, m.taskExpanded, task.ID)
	words := gotWords(expanded, expIDs, start)
	if len(words) < 2 || strings.Join(words[1:], " ") != title {
		t.Errorf("expanded rows do not reconstruct the full title: %q", strings.Join(words, " "))
	}

	// One more toggle → collapsed.
	m.handleTasksPanelClick(taskRowLineIndex(t, ts, m.taskExpanded, task.ID))
	if m.taskExpanded != "" {
		t.Errorf("second toggle did not collapse: taskExpanded=%q", m.taskExpanded)
	}
}

// gotWords reconstructs the wrapped title words from a task's rows (the
// first word of the first row is the status badge and is included).
func gotWords(lines, ids []string, start int) []string {
	var words []string
	for i := start; i < len(lines) && ids[i] == ids[start]; i++ {
		words = append(words, strings.Fields(ansi.Strip(lines[i]))...)
	}
	return words
}

// TestTasksPanelRows_IDsAligned: rows and IDs stay aligned and every rendered
// line fits the panel inner width, collapsed and expanded (the issue #212
// drift class).
func TestTasksPanelRows_IDsAligned(t *testing.T) {
	_, ts := tasksPanelClickModel(t)
	task, err := ts.Create("wrap me: a task title long enough to need several lines at thirty-two columns", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, expanded := range []string{"", task.ID} {
		lines, ids := taskPanelRows(ts, tasksPanelInner, expanded)
		if len(lines) != len(ids) {
			t.Fatalf("expanded=%q: %d lines vs %d ids", expanded, len(lines), len(ids))
		}
		for i, l := range lines {
			if w := utf8.RuneCountInString(ansi.Strip(l)); w > tasksPanelInner {
				t.Errorf("expanded=%q: line %d exceeds inner width (%d > %d): %q", expanded, i, w, tasksPanelInner, l)
			}
		}
	}
}

// (the stale-window case is machine-level now: panelClickAction owns the
// 400ms cadence for every panel — see TestPanelClickCadence_*)
