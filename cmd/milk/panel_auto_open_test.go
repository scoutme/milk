package main

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
)

// newTestModelForPanels builds a minimal model for panel auto-open tests,
// following the same pattern as background_ui_test.go's F1-F4 tests.
func newTestModelForPanels(t *testing.T) model {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	return newModel(context.Background(), st, nil, dispatchAgents{}, nil)
}

// TestAutoOpenPanel_OpensWhenNotOverridden verifies a panel whose content
// just became active opens automatically when the user hasn't touched it.
func TestAutoOpenPanel_OpensWhenNotOverridden(t *testing.T) {
	m := newTestModelForPanels(t)
	if m.panelBackground {
		t.Fatal("expected background panel closed by default")
	}
	m.autoOpenPanel(regionBackground)
	if !m.panelBackground {
		t.Error("expected autoOpenPanel to open the background panel")
	}
}

// TestAutoOpenPanel_RespectsExplicitClose verifies that once the user
// explicitly closes a panel (F1-F4 or /panel), automatic "content became
// active" opens no longer override that choice.
func TestAutoOpenPanel_RespectsExplicitClose(t *testing.T) {
	m := newTestModelForPanels(t)

	// Open it automatically once — should work, nothing manual yet.
	m.autoOpenPanel(regionBackground)
	if !m.panelBackground {
		t.Fatal("expected the panel to open automatically")
	}

	// User explicitly closes it via F3.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyF3})
	m2 := updated.(model)
	if m2.panelBackground {
		t.Fatal("expected F3 to close the background panel")
	}

	// Further automatic activity must not reopen it.
	m2.autoOpenPanel(regionBackground)
	if m2.panelBackground {
		t.Error("expected the explicit close to stick over automatic re-open")
	}
}

// TestAutoOpenPanel_RespectsExplicitOpenToo verifies the sticky rule cuts
// both ways: once the user has explicitly toggled a panel, automatic
// management should not be allowed to close it either (autoOpenPanel never
// closes, but this documents that an explicit open is also "manual" and
// future automatic opens are then no-ops, not double-opens that could mask
// a bug where the override map isn't set).
func TestAutoOpenPanel_RespectsExplicitOpenToo(t *testing.T) {
	m := newTestModelForPanels(t)

	// User explicitly opens tasks via F2.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyF2})
	m2 := updated.(model)
	if !m2.panelTasks {
		t.Fatal("expected F2 to open the tasks panel")
	}
	if !m2.panelManualOverride[regionTasks] {
		t.Fatal("expected F2 to record a manual override for the tasks panel")
	}
}

// TestTaskStoreChangedMsg_OpensTasksPanel verifies that a task-store mutation
// auto-opens the tasks panel when the user hasn't manually closed it.
func TestTaskStoreChangedMsg_OpensTasksPanel(t *testing.T) {
	m := newTestModelForPanels(t)
	updated, _ := m.Update(taskStoreChangedMsg{})
	m2 := updated.(model)
	if !m2.panelTasks {
		t.Error("expected taskStoreChangedMsg to auto-open the tasks panel")
	}
}

// TestTaskStoreChangedMsg_RespectsManualClose verifies a user-closed tasks
// panel stays closed even when tasks keep changing.
func TestTaskStoreChangedMsg_RespectsManualClose(t *testing.T) {
	m := newTestModelForPanels(t)

	// Open then explicitly close via F2, same as a real session would.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyF2})
	m2 := updated.(model)
	updated2, _ := m2.Update(tea.KeyMsg{Type: tea.KeyF2})
	m3 := updated2.(model)
	if m3.panelTasks {
		t.Fatal("expected the second F2 to close the tasks panel")
	}

	updated3, _ := m3.Update(taskStoreChangedMsg{})
	m4 := updated3.(model)
	if m4.panelTasks {
		t.Error("expected the manual close to stick despite task activity")
	}
}

// TestBackgroundJobStartedMsg_OpensBackgroundPanel verifies a newly spawned
// background job auto-opens the background-agents panel.
func TestBackgroundJobStartedMsg_OpensBackgroundPanel(t *testing.T) {
	m := newTestModelForPanels(t)
	updated, _ := m.Update(backgroundJobStartedMsg{})
	m2 := updated.(model)
	if !m2.panelBackground {
		t.Error("expected backgroundJobStartedMsg to auto-open the background panel")
	}
}

// TestWorkflowProgressMsg_RespectsManualClose verifies that once the user
// closes the workflow panel via F4, ongoing workflow progress does not force
// it back open.
func TestWorkflowProgressMsg_RespectsManualClose(t *testing.T) {
	m := newTestModelForPanels(t)

	// Open then explicitly close via F4.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyF4})
	m2 := updated.(model)
	updated2, _ := m2.Update(tea.KeyMsg{Type: tea.KeyF4})
	m3 := updated2.(model)
	if m3.workflowPanelOpen {
		t.Fatal("expected the second F4 to close the workflow panel")
	}

	updated3, _ := m3.Update(workflow.ProgressMsg{WorkflowName: "test", Role: "worker"})
	m4 := updated3.(model)
	if m4.workflowPanelOpen {
		t.Error("expected the manual close to stick despite workflow progress")
	}
}
