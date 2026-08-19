package main

import (
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/workflow"
)

// TestWorkflowIdleCheck_WarnsAfterTimeoutAndReschedules verifies that the
// workflow idle watchdog warns once the current role's agent turn timeout has
// elapsed since the last streamed activity, and keeps rescheduling itself
// while the workflow is still busy.
func TestWorkflowIdleCheck_WarnsAfterTimeoutAndReschedules(t *testing.T) {
	m := testModel()
	sec := 1
	m.st = &interactiveState{
		cfg: config.Config{Agents: []config.AgentConfig{
			{Name: "local-designer", Limits: &config.AgentLimits{TurnTimeoutSecs: &sec}},
		}},
	}
	m.busy = true
	m.ready = false
	m.workflowState = &workflow.State{
		WorkflowName: "dev",
		Role:         "designer",
		AgentMap:     map[string]string{"designer": "local-designer"},
	}
	m.lastWorkflowActivity = time.Now().Add(-time.Hour) // long past the 1s timeout

	updated, cmd := m.Update(workflowIdleCheckMsg{})
	mm := updated.(model)

	if !mm.workflowTimeoutWarned {
		t.Fatalf("expected workflowTimeoutWarned=true after idle check past timeout")
	}
	if !strings.Contains(mm.transcript.String(), "taking longer than expected") {
		t.Fatalf("expected timeout warning in transcript, got: %s", mm.transcript.String())
	}
	if cmd == nil {
		t.Fatalf("expected idle check to reschedule itself while busy")
	}

	// A second tick before any new activity must not warn again.
	before := mm.transcript.String()
	updated2, _ := mm.Update(workflowIdleCheckMsg{})
	mm2 := updated2.(model)
	if mm2.transcript.String() != before {
		t.Fatalf("expected no duplicate warning, transcript grew: %s", mm2.transcript.String())
	}

	// Once the workflow finishes (busy=false), the watchdog must stop rescheduling.
	mm2.busy = false
	_, cmd3 := mm2.Update(workflowIdleCheckMsg{})
	if cmd3 != nil {
		t.Fatalf("expected idle check to stop rescheduling once busy=false")
	}
}
