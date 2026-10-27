package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/tasks"
	"github.com/scoutme/milk/internal/workflow"
)

// newTestModelForPanels builds a minimal model for panel auto-open tests,
// following the same pattern as background_ui_test.go's F-key panel-toggle tests.
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

// rawPanelOpen reads the panel's open state straight off the model field,
// bypassing panelOpen's workflow width gating — these tests run on an
// unsized model, so workflowPanelVisible would report "closed" for a panel
// that is logically open.
func rawPanelOpen(m *model, region panelRegion) bool {
	switch region {
	case regionMemory:
		return m.panelMemory
	case regionTasks:
		return m.panelTasks
	case regionBackground:
		return m.panelBackground
	case regionWorkflow:
		return m.workflowPanelOpen
	case regionProject:
		return m.panelProject
	case regionGit:
		return m.panelGit
	}
	return false
}

// spawnRunningBackgroundJob attaches a manager to the model with one job
// that stays JobRunning until the returned cancel func is called (the run
// body simply blocks on its context). Registers cancel as test cleanup so
// an early Fatal can't leak the goroutine.
func spawnRunningBackgroundJob(t *testing.T, m *model) (cancel context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr := local.NewManager(ctx, 1)
	mgr.Spawn("research X", "task", "primary", "model", func(ctx context.Context, jobID string, out io.Writer) (string, session.TokenUsage, error) {
		<-ctx.Done()
		return "", session.TokenUsage{}, ctx.Err()
	})
	m.agents.backgroundMgr = mgr
	return cancel
}

// waitJobFinished polls the manager until its first job leaves JobRunning.
func waitJobFinished(t *testing.T, mgr *local.Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		js := mgr.Jobs()
		if len(js) > 0 && js[0].Status != local.JobRunning {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("background job did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
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
// explicitly closes a panel while its content is still active (an F-key
// or /panel), automatic "content became active" opens no longer override that
// choice — the sticky rule holds for as long as the content stays live.
func TestAutoOpenPanel_RespectsExplicitClose(t *testing.T) {
	m := newTestModelForPanels(t)
	cancel := spawnRunningBackgroundJob(t, &m)

	// Open it automatically once — should work, nothing manual yet.
	m.autoOpenPanel(regionBackground)
	if !m.panelBackground {
		t.Fatal("expected the panel to open automatically")
	}

	// User explicitly closes it via F3 while the job is still running.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyF3})
	m2 := updated.(model)
	if m2.panelBackground {
		t.Fatal("expected F3 to close the background panel")
	}
	if !m2.panelManualOverride[regionBackground] {
		t.Fatal("closing while a job is running must record a manual override")
	}

	// Further automatic activity must not reopen it while the job runs.
	m2.autoOpenPanel(regionBackground)
	if m2.panelBackground {
		t.Error("expected the explicit close to stick over automatic re-open")
	}
	cancel()
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
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	m2 := updated.(model)
	if !m2.panelTasks {
		t.Fatal("expected F2 to open the tasks panel")
	}
	if !m2.panelManualOverride[regionTasks] {
		t.Fatal("expected F2 to record a manual override for the tasks panel")
	}
}

// TestPanelClose_FinalizedContentDoesNotStick covers the ADR-0059 half of
// the contract at close time — the session-only semantics
// ("remember_panel_state": false): closing a panel whose content has
// *already* settled (emptied/completed/done) must not lock it out of its
// next new content — the override is dropped immediately, so the very next
// auto-open trigger still opens the panel. With the default durable panel
// state (ADR-0078) a close is a remembered preference instead, generalized
// to every panel — see TestPanelClose_RememberedPreferenceSticks.
func TestPanelClose_FinalizedContentDoesNotStick(t *testing.T) {
	// Fresh test models have finalized content in every region: nil mem,
	// nil/empty task store, no running jobs, no workflow state.
	cases := []struct {
		name   string
		region panelRegion
		key    rune
	}{
		{"memory", regionMemory, tea.KeyF1},
		{"tasks", regionTasks, tea.KeyF2},
		{"background", regionBackground, tea.KeyF3},
		{"workflow", regionWorkflow, tea.KeyF4},
		{"project", regionProject, tea.KeyF5},
		{"git", regionGit, tea.KeyF6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModelForPanels(t)
			m.rememberPanelState = false // ADR-0059 expiry semantics
			// Every panel starts closed (ADR-0078 for memory); the first
			// press opens, the second is the closing toggle.
			updated, _ := m.Update(tea.KeyPressMsg{Code: tc.key})
			m = updated.(model)
			if !rawPanelOpen(&m, tc.region) {
				t.Fatalf("expected first press to open the %s panel", tc.name)
			}
			updated, _ = m.Update(tea.KeyPressMsg{Code: tc.key})
			m = updated.(model)
			if rawPanelOpen(&m, tc.region) {
				t.Fatalf("expected second press to close the %s panel", tc.name)
			}
			if m.panelManualOverride[tc.region] {
				t.Errorf("closing the %s panel with finalized content must not record a sticky override", tc.name)
			}
			m.autoOpenPanel(tc.region)
			if !rawPanelOpen(&m, tc.region) {
				t.Errorf("expected auto-open to still work for the %s panel after closing finalized content", tc.name)
			}
		})
	}
}

// TestPanelClose_RememberedPreferenceSticks is the ADR-0078 default
// contract ("remember_panel_state" on): a manual toggle is a durable
// preference for *every* panel — memory included and not excepted. It
// survives content finalization and beats content-driven auto-open; the
// generalization of what the memory panel alone had when its state first
// became persistent.
func TestPanelClose_RememberedPreferenceSticks(t *testing.T) {
	cases := []struct {
		name   string
		region panelRegion
		key    rune
	}{
		{"memory", regionMemory, tea.KeyF1},
		{"tasks", regionTasks, tea.KeyF2},
		{"background", regionBackground, tea.KeyF3},
		{"workflow", regionWorkflow, tea.KeyF4},
		{"project", regionProject, tea.KeyF5},
		{"git", regionGit, tea.KeyF6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModelForPanels(t) // rememberPanelState on (default)
			updated, _ := m.Update(tea.KeyPressMsg{Code: tc.key})
			m = updated.(model)
			if !rawPanelOpen(&m, tc.region) {
				t.Fatalf("expected first press to open the %s panel", tc.name)
			}
			updated, _ = m.Update(tea.KeyPressMsg{Code: tc.key})
			m = updated.(model)
			if rawPanelOpen(&m, tc.region) {
				t.Fatalf("expected second press to close the %s panel", tc.name)
			}
			if !m.panelManualOverride[tc.region] {
				t.Errorf("a remembered close must stay recorded for the %s panel", tc.name)
			}
			m.autoOpenPanel(tc.region)
			if rawPanelOpen(&m, tc.region) {
				t.Errorf("expected the remembered close to beat auto-open for the %s panel", tc.name)
			}
		})
	}
}

// TestTaskStoreChangedMsg_OpensTasksPanel verifies a task-store mutation
// auto-opens the tasks panel when the user hasn't manually closed it.
func TestTaskStoreChangedMsg_OpensTasksPanel(t *testing.T) {
	m := newTestModelForPanels(t)
	updated, _ := m.Update(taskStoreChangedMsg{})
	m2 := updated.(model)
	if !m2.panelTasks {
		t.Error("expected taskStoreChangedMsg to auto-open the tasks panel")
	}
}

// TestTaskStoreChangedMsg_ManualCloseSticksUntilTasksFinalize walks the
// full ADR-0059 cycle (the session-only semantics, "remember_panel_state":
// false): a user close sticks while a task is still open, completing the
// last task finalizes the content (resetting the override without
// force-opening the panel), and the next created task auto-opens the panel
// again.
func TestTaskStoreChangedMsg_ManualCloseSticksUntilTasksFinalize(t *testing.T) {
	m := newTestModelForPanels(t)
	m.rememberPanelState = false // ADR-0059 expiry semantics
	ts, err := tasks.New(t.TempDir(), "sess")
	if err != nil {
		t.Fatal(err)
	}
	m.taskStore = ts
	task, err := ts.Create("active work", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Open then explicitly close via F2 while the task is still open.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	m2 := updated.(model)
	updated2, _ := m2.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	m3 := updated2.(model)
	if m3.panelTasks {
		t.Fatal("expected the second F2 to close the tasks panel")
	}
	if !m3.panelManualOverride[regionTasks] {
		t.Fatal("closing while a task is open must record a manual override")
	}

	// Task activity with content still live keeps the close sticky.
	updated3, _ := m3.Update(taskStoreChangedMsg{})
	m4 := updated3.(model)
	if m4.panelTasks {
		t.Error("expected the manual close to stick despite task activity")
	}

	// Completing the last task finalizes the content: the override is
	// dropped, but the finalizing event itself must not force the panel
	// open — only *new* content does.
	if err := ts.Complete(task.ID); err != nil {
		t.Fatal(err)
	}
	updated4, _ := m4.Update(taskStoreChangedMsg{})
	m5 := updated4.(model)
	if m5.panelTasks {
		t.Error("the finalizing task change must not force the panel open")
	}
	if m5.panelManualOverride[regionTasks] {
		t.Error("expected the manual override to reset once every task is done")
	}

	// The next new task auto-opens the panel again.
	if _, err := ts.Create("next task", nil); err != nil {
		t.Fatal(err)
	}
	updated5, _ := m5.Update(taskStoreChangedMsg{})
	m6 := updated5.(model)
	if !m6.panelTasks {
		t.Error("expected the tasks panel to auto-open again on new content after finalization")
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

// TestBackgroundJobDoneMsg_ResetsManualOverride (session-only semantics,
// "remember_panel_state": false): closing the background panel while a job
// runs sticks, the job finishing finalizes the content and re-arms
// auto-open (without force-opening), and the next spawned job opens the
// panel again.
func TestBackgroundJobDoneMsg_ResetsManualOverride(t *testing.T) {
	m := newTestModelForPanels(t)
	m.rememberPanelState = false // ADR-0059 expiry semantics
	cancel := spawnRunningBackgroundJob(t, &m)

	// Open then close via F3 while the job is running.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyF3})
	m2 := updated.(model)
	if !m2.panelBackground {
		t.Fatal("expected F3 to open the background panel")
	}
	updated2, _ := m2.Update(tea.KeyPressMsg{Code: tea.KeyF3})
	m3 := updated2.(model)
	if m3.panelBackground {
		t.Fatal("expected the second F3 to close the background panel")
	}
	if !m3.panelManualOverride[regionBackground] {
		t.Fatal("closing while a job runs must record a manual override")
	}

	// Job finishes (SetOnDone would deliver backgroundJobDoneMsg in the
	// real TUI; the tests send it explicitly once the manager's record has
	// left JobRunning).
	cancel()
	waitJobFinished(t, m3.agents.backgroundMgr)
	jobs := m3.agents.backgroundMgr.Jobs()
	updated3, _ := m3.Update(backgroundJobDoneMsg{job: &jobs[0]})
	m4 := updated3.(model)
	if m4.panelBackground {
		t.Error("job completion must not force the panel open")
	}
	if m4.panelManualOverride[regionBackground] {
		t.Error("expected the manual override to reset once no jobs are running")
	}

	// The next spawn auto-opens the panel again.
	updated4, _ := m4.Update(backgroundJobStartedMsg{})
	m5 := updated4.(model)
	if !m5.panelBackground {
		t.Error("expected the background panel to auto-open again on a new spawn")
	}
}

// TestWorkflowProgressMsg_RespectsManualClose (session-only semantics,
// "remember_panel_state": false): closing the workflow panel mid-run
// sticks over ongoing progress, WorkflowDoneMsg finalizes the content
// (resetting the override without force-opening), and the next auto-open
// trigger re-opens the panel.
func TestWorkflowProgressMsg_RespectsManualClose(t *testing.T) {
	m := newTestModelForPanels(t)
	m.rememberPanelState = false // ADR-0059 expiry semantics
	m.workflowRunning = true
	m.workflowState = &workflow.State{WorkflowName: "test", Task: "t", Role: "worker"}

	// Open then explicitly close via F4 while the run is in flight.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyF4})
	m2 := updated.(model)
	updated2, _ := m2.Update(tea.KeyPressMsg{Code: tea.KeyF4})
	m3 := updated2.(model)
	if m3.workflowPanelOpen {
		t.Fatal("expected the second F4 to close the workflow panel")
	}
	if !m3.panelManualOverride[regionWorkflow] {
		t.Fatal("closing while a workflow runs must record a manual override")
	}

	// Ongoing progress does not force it back open.
	updated3, _ := m3.Update(workflow.ProgressMsg{WorkflowName: "test", Role: "worker"})
	m4 := updated3.(model)
	if m4.workflowPanelOpen {
		t.Error("expected the manual close to stick despite workflow progress")
	}

	// The run finishing finalizes the content: override reset, panel
	// stays closed (completion is not "new content").
	updated4, _ := m4.Update(workflow.WorkflowDoneMsg{})
	m5 := updated4.(model)
	if m5.workflowPanelOpen {
		t.Error("workflow completion must not force the panel open")
	}
	if m5.panelManualOverride[regionWorkflow] {
		t.Error("expected the manual override to reset once the workflow is done")
	}

	// The next workflow activity auto-opens it again.
	m5.autoOpenPanel(regionWorkflow)
	if !m5.workflowPanelOpen {
		t.Error("expected auto-open re-armed after the workflow finalized")
	}
}

// TestForget_PreservesMemoryPreferenceWhenEmptied: with the durable panel
// state (ADR-0078, the default) closing the memory panel while percepts
// exist is a remembered preference — /forget deleting the last percept
// (the store's finalization event) must not reset it, and the choice must
// reach the state file for the next session. (The ADR-0059 expiry that
// emptying used to trigger is the "remember_panel_state": false fallback,
// pinned by TestPanelClose_FinalizedContentDoesNotStick.)
func TestForget_PreservesMemoryPreferenceWhenEmptied(t *testing.T) {
	m := newTestModelForPanels(t)
	m.panelStatePath = filepath.Join(t.TempDir(), "panel-state.json")
	store, err := memory.NewStoreParams(t.TempDir(), "sess", memory.Params{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Record(context.Background(), "a durable fact", memory.ProducerUser, memory.ConsumerAll, memory.Roles{}, false); err != nil {
		t.Fatal(err)
	}
	m.mem = store

	// The panel starts closed (content-driven default); open, then close
	// via F1 while the store still has content → remembered preference.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyF1})
	m2 := updated.(model)
	if !m2.panelMemory {
		t.Fatal("expected F1 to open the memory panel")
	}
	updated, _ = m2.Update(tea.KeyPressMsg{Code: tea.KeyF1})
	m2 = updated.(model)
	if m2.panelMemory {
		t.Fatal("expected the second F1 to close the memory panel")
	}
	if !m2.panelManualOverride[regionMemory] {
		t.Fatal("closing while memory has content must record a manual override")
	}
	if m2.panelContentFinalized(regionMemory) {
		t.Fatal("a store with a percept must not be finalized")
	}

	percepts := store.List(memory.ListOpts{Scope: "session"})
	if len(percepts) != 1 {
		t.Fatalf("expected exactly one session percept, got %d", len(percepts))
	}
	m2.pendingForget = &forgetState{candidates: percepts}
	m3 := m2.resolveForget("y")
	if len(m3.mem.List(memory.ListOpts{Scope: "session"})) != 0 {
		t.Fatal("expected the percept to be deleted")
	}
	if !m3.panelManualOverride[regionMemory] {
		t.Error("emptying memory must preserve the user's panel preference")
	}
	data, err := os.ReadFile(m.panelStatePath)
	if err != nil {
		t.Fatalf("the preference must reach the state file: %v", err)
	}
	if !strings.Contains(string(data), `"memory":false`) {
		t.Errorf("expected the closed memory preference in the state file, got %s", data)
	}
}

// TestPanelContentFinalized pins down the per-region finalization
// predicates that drive resetManualOverrideIfFinalized.
func TestPanelContentFinalized(t *testing.T) {
	t.Run("tasks", func(t *testing.T) {
		m := newTestModelForPanels(t)
		if !m.panelContentFinalized(regionTasks) {
			t.Error("nil task store counts as finalized")
		}
		ts, err := tasks.New(t.TempDir(), "sess")
		if err != nil {
			t.Fatal(err)
		}
		m.taskStore = ts
		if !m.panelContentFinalized(regionTasks) {
			t.Error("empty task store is finalized")
		}
		task, err := ts.Create("w", nil)
		if err != nil {
			t.Fatal(err)
		}
		if m.panelContentFinalized(regionTasks) {
			t.Error("an open task keeps the content active")
		}
		if err := ts.Complete(task.ID); err != nil {
			t.Fatal(err)
		}
		if !m.panelContentFinalized(regionTasks) {
			t.Error("all tasks done is finalized")
		}
	})

	t.Run("background", func(t *testing.T) {
		m := newTestModelForPanels(t)
		if !m.panelContentFinalized(regionBackground) {
			t.Error("nil manager counts as finalized")
		}
		cancel := spawnRunningBackgroundJob(t, &m)
		if m.panelContentFinalized(regionBackground) {
			t.Error("a running job keeps the content active")
		}
		cancel()
		waitJobFinished(t, m.agents.backgroundMgr)
		if !m.panelContentFinalized(regionBackground) {
			t.Error("no running jobs is finalized")
		}
	})

	t.Run("workflow", func(t *testing.T) {
		m := newTestModelForPanels(t)
		if !m.panelContentFinalized(regionWorkflow) {
			t.Error("no workflow state is finalized")
		}
		m.workflowRunning = true
		m.workflowState = &workflow.State{WorkflowName: "test", Task: "t", Role: "worker"}
		if m.panelContentFinalized(regionWorkflow) {
			t.Error("a running workflow keeps the content active")
		}
		m.workflowRunning = false
		if m.panelContentFinalized(regionWorkflow) {
			t.Error("an interrupted/unfinished state stays active")
		}
		m.workflowState.Role = "done"
		if !m.panelContentFinalized(regionWorkflow) {
			t.Error("Role done is finalized")
		}
	})

	t.Run("memory", func(t *testing.T) {
		m := newTestModelForPanels(t)
		if !m.panelContentFinalized(regionMemory) {
			t.Error("nil store counts as finalized")
		}
		store, err := memory.NewStoreParams(t.TempDir(), "sess", memory.Params{})
		if err != nil {
			t.Fatal(err)
		}
		m.mem = store
		if !m.panelContentFinalized(regionMemory) {
			t.Error("empty store is finalized")
		}
		p, err := store.Record(context.Background(), "fact", memory.ProducerUser, memory.ConsumerAll, memory.Roles{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if m.panelContentFinalized(regionMemory) {
			t.Error("a percept keeps the content active")
		}
		if _, err := store.Delete(p.ID); err != nil {
			t.Fatal(err)
		}
		if !m.panelContentFinalized(regionMemory) {
			t.Error("emptied store is finalized")
		}
	})
}

func TestAutoOpenPanel_MasterSwitchOff(t *testing.T) {
	m := newTestModelForPanels(t)
	m.panelAutoOpen = false
	m.autoOpenPanel(regionTasks)
	if rawPanelOpen(&m, regionTasks) {
		t.Error("autoOpenPanel opened the tasks panel with /panel autoopen off")
	}
	// The explicit toggle is unaffected: panels still open on request.
	mm, _ := m.handlePanelCmd("tasks")
	m = mm.(model)
	if !rawPanelOpen(&m, regionTasks) {
		t.Error("explicit /panel tasks did not open the panel with auto-open off")
	}
}

func TestApplyMemoryContentDefault_MasterSwitchOff(t *testing.T) {
	m := newTestModelForPanels(t)
	m.panelAutoOpen = false
	m.applyMemoryContentDefault()
	if m.panelMemory {
		t.Error("applyMemoryContentDefault opened the memory panel with auto-open off")
	}
}

func TestPanelAutoOpenCmd_TogglePersists(t *testing.T) {
	m := newTestModelForPanels(t)
	if !m.panelAutoOpen {
		t.Fatal("master switch should default on (pre-switch behaviour)")
	}
	mm, _ := m.handlePanelAutoOpenCmd("off")
	m = mm.(model)
	if m.panelAutoOpen {
		t.Error("panelAutoOpen still on after /panel autoopen off")
	}
	if m.st.cfg.PanelAutoOpen == nil || *m.st.cfg.PanelAutoOpen {
		t.Errorf("cfg.PanelAutoOpen = %v, want false", m.st.cfg.PanelAutoOpen)
	}
	// The state query reports the new state in the transcript.
	mm, _ = m.handlePanelAutoOpenCmd("")
	m = mm.(model)
	if got := m.transcript.String(); !strings.Contains(got, "panel auto-open: off") {
		t.Errorf("query transcript = %q, want state report", got)
	}
	// Re-enabling restores content-driven behaviour.
	mm, _ = m.handlePanelAutoOpenCmd("on")
	m = mm.(model)
	if !m.panelAutoOpen {
		t.Error("panelAutoOpen still off after /panel autoopen on")
	}
}

func TestPanelAutoOpenCmd_UsageOnBogusArg(t *testing.T) {
	m := newTestModelForPanels(t)
	mm, _ := m.handlePanelAutoOpenCmd("yes")
	m = mm.(model)
	if got := m.transcript.String(); !strings.Contains(got, "usage: /panel autoopen [on|off]") {
		t.Errorf("transcript = %q, want usage line", got)
	}
	if !m.panelAutoOpen {
		t.Error("a bogus argument must not change the state")
	}
}

func TestPanelAutoOpenCmd_RoutedFromPanelCmd(t *testing.T) {
	m := newTestModelForPanels(t)
	mm, _ := m.handlePanelCmd("autoopen off")
	m = mm.(model)
	if m.panelAutoOpen {
		t.Error("handlePanelCmd did not route autoopen to its handler")
	}
}
