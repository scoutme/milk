package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/tasks"
)

// Keyboard interaction of the focused panel (issue #412's follow-up):
// arrows move a row cursor, Enter runs the cursor row's double-click
// action (rows with items only), typing hands focus back to the main chat.

// cursorTestModel wires a full model with the tasks panel open and focused,
// carrying count tasks.
func cursorTestModel(t *testing.T, count int) (model, *tasks.Store) {
	t.Helper()
	m := newTestModelForPanels(t)
	m.height = 40
	ts, err := tasks.New(t.TempDir(), "cursor")
	if err != nil {
		t.Fatalf("tasks.New: %v", err)
	}
	for i := range count {
		if _, err := ts.Create("task "+string(rune('a'+i)), nil); err != nil {
			t.Fatalf("task create: %v", err)
		}
	}
	m.taskStore = ts
	m.panelTasks = true
	m.panelFocus = regionTasks
	return m, ts
}

func TestPanelCursor_ArrowsMoveAndClamp(t *testing.T) {
	m, _ := cursorTestModel(t, 3)
	rows, _ := m.sidePanelLines(regionTasks)
	if len(rows) < 2 {
		t.Fatalf("fixture: want several rows, got %d", len(rows))
	}

	// Up at the top clamps.
	m.panelCursorLine = 0
	m.movePanelCursor(regionTasks, -1)
	if m.panelCursorLine != 0 {
		t.Errorf("up at top must clamp to 0, got %d", m.panelCursorLine)
	}

	// Down walks one row.
	m.movePanelCursor(regionTasks, 1)
	if m.panelCursorLine != 1 {
		t.Errorf("down must move to row 1, got %d", m.panelCursorLine)
	}

	// Down past the last row clamps.
	m.panelCursorLine = len(rows) - 1
	m.movePanelCursor(regionTasks, 1)
	if m.panelCursorLine != len(rows)-1 {
		t.Errorf("down at bottom must clamp to %d, got %d", len(rows)-1, m.panelCursorLine)
	}
}

func TestPanelCursor_EnterActivatesItemRowsOnly(t *testing.T) {
	m, ts := cursorTestModel(t, 2)
	task, err := ts.Create("expand me", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Item row: Enter runs the row's action (the double-click expand).
	m.panelCursorLine = taskRowLineIndex(t, ts, "", task.ID)
	next, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := next.(model)
	if mm.taskExpanded != task.ID {
		t.Errorf("enter must expand the cursor row's task, got %q", mm.taskExpanded)
	}

	// Non-item rows (a header/blank row, or out of range): Enter does
	// nothing — and never submits the input.
	_, ids := taskPanelRows(ts, tasksPanelInner, "")
	nonItems := []int{len(ids)} // out of range is always a non-item
	for i, id := range ids {
		if id == "" {
			nonItems = append(nonItems, i)
			break
		}
	}
	for _, line := range nonItems {
		m2, _ := cursorTestModel(t, 1)
		m2.taskStore = ts
		m2.ta.InsertString("hello")
		m2.panelCursorLine = line
		next2, _ := m2.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
		mm2 := next2.(model)
		if mm2.taskExpanded != "" {
			t.Errorf("enter on non-item row %d must do nothing, expanded %q", line, mm2.taskExpanded)
		}
		if mm2.ta.Value() != "hello" {
			t.Errorf("enter on non-item row %d must not submit the input, ta=%q", line, mm2.ta.Value())
		}
	}
}

func TestPanelTyping_RefocusesMainAndLandsInInput(t *testing.T) {
	m, _ := cursorTestModel(t, 1)
	next, _ := m.handleKey(tea.KeyPressMsg{Code: 'a', Text: "a"})
	mm := next.(model)
	if mm.focusedPanel() != regionNone {
		t.Error("typing must bring focus to the main chat")
	}
	if mm.ta.Value() != "a" {
		t.Errorf("the typed key must land in the input, got %q", mm.ta.Value())
	}

	// Space is typing too.
	m2, _ := cursorTestModel(t, 1)
	next2, _ := m2.handleKey(tea.KeyPressMsg{Code: tea.KeySpace})
	mm2 := next2.(model)
	if mm2.focusedPanel() != regionNone {
		t.Error("space must bring focus to the main chat")
	}

	// Editing keys stay panel-scoped: Enter does not refocus.
	m3, _ := cursorTestModel(t, 1)
	next3, _ := m3.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm3 := next3.(model)
	if mm3.focusedPanel() != regionTasks {
		t.Error("enter must stay panel-scoped (no refocus)")
	}
}

func TestPanelCursor_StaysVisibleOnPaging(t *testing.T) {
	m, _ := cursorTestModel(t, 40) // long content — the panel overflows
	h := m.viewportHeight()
	if m.panelMaxOffset(regionTasks, h) == 0 {
		t.Fatalf("fixture: tasks panel must overflow at height %d", h)
	}

	m.panelCursorLine = 0
	m.scrollFocusedPanel(1) // page down
	if m.panelCursorLine < m.panelScrollOffset(regionTasks) {
		t.Errorf("paging down must keep the cursor in view: cursor %d < offset %d",
			m.panelCursorLine, m.panelScrollOffset(regionTasks))
	}

	// Arrows scroll the window to follow the cursor off the bottom edge.
	m.panelCursorLine = m.panelScrollOffset(regionTasks) + h - 1
	m.movePanelCursor(regionTasks, 1)
	if m.panelCursorLine >= m.panelScrollOffset(regionTasks)+h {
		t.Errorf("arrow below the window must scroll it: cursor %d outside [%d, %d)",
			m.panelCursorLine, m.panelScrollOffset(regionTasks), m.panelScrollOffset(regionTasks)+h)
	}
}

func TestPanelCursor_RenderHighlight(t *testing.T) {
	oldTTY := isTTY
	isTTY = true // panels render blank rows without a TTY
	t.Cleanup(func() { isTTY = oldTTY })

	m, _ := cursorTestModel(t, 3)
	m.panelCursorLine = 1
	out := m.renderSidePanel(regionTasks, 5)
	rows := strings.Split(out, "\n")
	// One row wears the highlight (the primitive may re-emit the SGR after
	// resets inside the row — the pin is per row, not per escape).
	hi := 0
	for i, r := range rows {
		if strings.Contains(r, selectionOn) {
			hi++
			if i != 1 {
				t.Errorf("highlight at row %d, want the cursor row 1", i)
			}
		}
	}
	if hi != 1 {
		t.Errorf("exactly one row must wear the cursor highlight, got %d:\n%q", hi, out)
	}
}

// Busy-state twin of the Enter contract: while a turn runs, the busy
// disposition (ADR-0083 queue hint / double-Enter queue) must not eat the
// focused panel's Enter — it is the row's double-click action, a view
// operation like completion-accept. The queue disposition keeps its own
// Enter the moment focus is back on main (control below).
func TestBusyPanelEnter_RunsRowActionNotQueue(t *testing.T) {
	m, ts := cursorTestModel(t, 2)
	task, err := ts.Create("expand me while busy", nil)
	if err != nil {
		t.Fatal(err)
	}
	m.busy = true
	m.ta.SetValue("do not queue me")
	m.panelCursorLine = taskRowLineIndex(t, ts, "", task.ID)
	m.busyEnterArmed = true // an armed queue gesture must not survive the panel

	next, _ := m.handleBusyKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	nm := next.(model)

	if nm.taskExpanded != task.ID {
		t.Errorf("busy Enter must run the row action, expanded %q", nm.taskExpanded)
	}
	if len(nm.st.queuedPrompts) != 0 {
		t.Errorf("busy panel Enter must not queue, got %d", len(nm.st.queuedPrompts))
	}
	if nm.busyHint != "" {
		t.Errorf("busy panel Enter must not set the queue hint, got %q", nm.busyHint)
	}
	if nm.ta.Value() != "do not queue me" {
		t.Errorf("busy panel Enter must not touch the input, ta=%q", nm.ta.Value())
	}
	if nm.busyEnterArmed {
		t.Error("the panel consumed the Enter gesture — busyEnterArmed must clear")
	}
}

// Control: without a focused panel the busy disposition is untouched —
// first Enter arms the queue hint, second Enter queues (ADR-0083).
func TestBusyEnter_NoPanelFocus_KeepsQueueDisposition(t *testing.T) {
	m, _ := cursorTestModel(t, 1)
	m.busy = true
	m.panelFocus = regionNone
	m.ta.SetValue("queue me")

	next, _ := m.handleBusyKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	nm := next.(model)
	if !nm.busyEnterArmed || !strings.Contains(nm.busyHint, "Enter again") {
		t.Fatalf("first busy Enter must arm the queue hint, armed=%v hint=%q", nm.busyEnterArmed, nm.busyHint)
	}
	next2, _ := nm.handleBusyKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	nm2 := next2.(model)
	if len(nm2.st.queuedPrompts) != 1 || nm2.st.queuedPrompts[0].text != "queue me" {
		t.Errorf("second Enter must queue, got %#v", nm2.st.queuedPrompts)
	}
}
