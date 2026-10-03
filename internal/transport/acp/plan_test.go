package acp

import (
	"testing"

	"github.com/scoutme/milk/internal/workflow"
)

// plan_update mapping (plan.go): workflows → PlanItems, stage nodes as
// pending|in_progress|completed|cancelled.

func TestPlanUpdateEnvelopeShape(t *testing.T) {
	upd := NewPlanUpdate("workflow-3", []PlanEntry{
		{Content: "design", Priority: PlanPriorityHigh, Status: PlanEntryInProgress},
	})
	body := jsonBody(t, upd)
	if body["sessionUpdate"] != "plan_update" {
		t.Fatalf("sessionUpdate = %v", body["sessionUpdate"])
	}
	plan, ok := body["plan"].(map[string]any)
	if !ok {
		t.Fatalf("plan missing: %v", body)
	}
	if plan["type"] != "items" || plan["planId"] != "workflow-3" {
		t.Fatalf("plan = %v", plan)
	}
	entries, ok := plan["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("entries = %v", plan["entries"])
	}
	entry := entries[0].(map[string]any)
	for _, key := range []string{"content", "priority", "status"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("entry missing %q: %v", key, entry)
		}
	}
}

func TestPlanEntriesFromStageTree(t *testing.T) {
	// Stage tree: design → (build, test). build done; test in progress.
	st := &workflow.State{
		Generic:    true,
		WorkflowID: 1,
		StageTree: &workflow.StageNode{Label: "design", Children: []*workflow.StageNode{
			{Label: "build"},
			{Label: "test"},
		}},
		// Completed path: design → build (child index 0).
		CompletedStageTree: &workflow.StageNode{Label: "design", Children: []*workflow.StageNode{
			{Label: "build"},
		}},
		// Active path: design → test (child index 1).
		ActiveStageTree: &workflow.StageNode{Label: "design", Children: []*workflow.StageNode{
			{Label: "test"},
		}},
	}
	entries := PlanEntriesForState(st, false)
	if len(entries) != 3 {
		t.Fatalf("entries = %d (%+v)", len(entries), entries)
	}
	want := []struct {
		content  string
		status   PlanEntryStatus
		priority PlanEntryPriority
	}{
		{"design", PlanEntryCompleted, PlanPriorityLow},
		{"  build", PlanEntryCompleted, PlanPriorityLow},
		{"  test", PlanEntryInProgress, PlanPriorityHigh},
	}
	for i, w := range want {
		if entries[i].Content != w.content || entries[i].Status != w.status || entries[i].Priority != w.priority {
			t.Errorf("entry %d = %+v, want %+v", i, entries[i], w)
		}
	}
}

func TestPlanEntriesPendingAndCancelled(t *testing.T) {
	st := &workflow.State{
		Generic:    true,
		WorkflowID: 2,
		StageTree: &workflow.StageNode{Label: "sprint 1", Children: []*workflow.StageNode{
			{Label: "pass 2"},
			{Label: "pass 3"},
		}},
		ActiveStageTree: &workflow.StageNode{Label: "sprint 1", Children: []*workflow.StageNode{
			{Label: "pass 2"},
		}},
	}
	entries := PlanEntriesForState(st, false)
	if entries[1].Status != PlanEntryInProgress || entries[2].Status != PlanEntryPending {
		t.Fatalf("live entries = %+v", entries)
	}
	cancelled := PlanEntriesForState(st, true)
	if cancelled[0].Status != PlanEntryCancelled || cancelled[1].Status != PlanEntryCancelled || cancelled[2].Status != PlanEntryCancelled {
		t.Fatalf("cancelled entries = %+v", cancelled)
	}
	// Completed work stays completed under cancellation.
	st2 := &workflow.State{
		Generic:            true,
		StageTree:          &workflow.StageNode{Label: "a"},
		CompletedStageTree: &workflow.StageNode{Label: "a"},
	}
	if got := PlanEntriesForState(st2, true)[0].Status; got != PlanEntryCompleted {
		t.Fatalf("completed under cancel = %v", got)
	}
}

func TestPlanEntriesFromSprints(t *testing.T) {
	// Dev-style run: 4 sprints, sprint 1 verdicted, sprint 2 current.
	st := &workflow.State{
		WorkflowName: "dev",
		Sprint:       2,
		TotalSprints: 4,
		VerdictHistory: []workflow.VerdictEntry{
			{Sprint: 1, Pass: 1, Verdict: "advance"},
		},
	}
	upd := PlanForWorkflow(st, false)
	if upd.Plan.PlanID != PlanID("workflow-0") {
		t.Fatalf("planId = %q", upd.Plan.PlanID)
	}
	want := []PlanEntryStatus{PlanEntryCompleted, PlanEntryInProgress, PlanEntryPending, PlanEntryPending}
	if len(upd.Plan.Entries) != len(want) {
		t.Fatalf("entries = %+v", upd.Plan.Entries)
	}
	for i, w := range want {
		if upd.Plan.Entries[i].Status != w {
			t.Errorf("sprint %d status = %v, want %v", i+1, upd.Plan.Entries[i].Status, w)
		}
	}
	// Cancelled run: the current sprint and future sprints are cancelled.
	cancelled := PlanForWorkflow(st, true)
	if cancelled.Plan.Entries[0].Status != PlanEntryCompleted ||
		cancelled.Plan.Entries[1].Status != PlanEntryCancelled ||
		cancelled.Plan.Entries[3].Status != PlanEntryCancelled {
		t.Fatalf("cancelled = %+v", cancelled.Plan.Entries)
	}
}

func TestPlanEntriesNilState(t *testing.T) {
	if got := PlanEntriesForState(nil, false); got != nil {
		t.Fatalf("nil state entries = %+v", got)
	}
}
