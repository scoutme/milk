package acp

import (
	"strconv"
	"strings"

	"github.com/scoutme/milk/internal/workflow"
)

// plan_update — the ACP v2 agent plan surface. A workflow run maps onto one
// plan (identified by PlanId) whose PlanItems are the workflow's stage nodes,
// so an editor's plan UI renders exactly the TUI's F4 workflow panel:
// "workflow start_workflow call owns a PlanItems (planId, entries per stage
// node)" (design §7.1).

// PlanEntryStatus is the ACP PlanEntryStatus enum (open-set).
type PlanEntryStatus string

const (
	PlanEntryPending    PlanEntryStatus = "pending"
	PlanEntryInProgress PlanEntryStatus = "in_progress"
	PlanEntryCompleted  PlanEntryStatus = "completed"
	PlanEntryCancelled  PlanEntryStatus = "cancelled"
)

// PlanEntryPriority is the ACP PlanEntryPriority enum (open-set).
type PlanEntryPriority string

const (
	PlanPriorityHigh   PlanEntryPriority = "high"
	PlanPriorityMedium PlanEntryPriority = "medium"
	PlanPriorityLow    PlanEntryPriority = "low"
)

// PlanEntry is one stage node in the plan.
type PlanEntry struct {
	Content  string            `json:"content"`
	Priority PlanEntryPriority `json:"priority"`
	Status   PlanEntryStatus   `json:"status"`
	Meta     map[string]any    `json:"_meta,omitempty"`
}

// PlanItems is the structured plan content ("items" variant of the ACP
// PlanUpdateContent union). The client replaces the whole plan with each
// update — so every emission carries the complete entry list with current
// statuses.
type PlanItems struct {
	Type    string         `json:"type"` // "items"
	PlanID  PlanID         `json:"planId"`
	Entries []PlanEntry    `json:"entries"`
	Meta    map[string]any `json:"_meta,omitempty"`
}

// PlanUpdate is the plan_update session update. Plan carries PlanItems (the
// PlanUpdateContent "items" variant).
type PlanUpdate struct {
	SessionUpdate string         `json:"sessionUpdate"` // "plan_update"
	Plan          PlanItems      `json:"plan"`
	Meta          map[string]any `json:"_meta,omitempty"`
}

func (PlanUpdate) isSessionUpdate() {}

// NewPlanUpdate wraps entries into a plan_update for the given plan.
func NewPlanUpdate(id PlanID, entries []PlanEntry) PlanUpdate {
	return PlanUpdate{
		SessionUpdate: "plan_update",
		Plan: PlanItems{
			Type:    "items",
			PlanID:  id,
			Entries: entries,
		},
	}
}

// PlanIDForWorkflow derives a session-stable plan ID from a workflow run's
// identity (workflow IDs are per-session unique; see workflow.NextWorkflowID).
func PlanIDForWorkflow(st *workflow.State) PlanID {
	return PlanID("workflow-" + strconv.Itoa(st.WorkflowID))
}

// PlanForWorkflow maps a workflow run's state onto a plan_update: workflows →
// PlanItems, stage nodes as pending|in_progress|completed|cancelled. cancelled
// marks every unfinished node cancelled (the run was aborted before the node
// completed).
//
// Both state shapes are covered:
//   - interpreter-driven (Generic) runs report the stage tree explicitly via
//     State.StageTree with State.ActiveStageTree / State.CompletedStageTree
//     marking the executing and finished paths (ProgressMsg.ActivePaths /
//     CompletedPaths);
//   - dev-style runs report Sprint/TotalSprints + VerdictHistory, so each
//     sprint is one entry (verdict recorded → completed, current Sprint →
//     in_progress, later sprints → pending).
func PlanForWorkflow(st *workflow.State, cancelled bool) PlanUpdate {
	return NewPlanUpdate(PlanIDForWorkflow(st), PlanEntriesForState(st, cancelled))
}

// PlanEntriesForState flattens the workflow state into plan entries, in
// stage order (depth-first, the F4 panel's indented-tree order).
func PlanEntriesForState(st *workflow.State, cancelled bool) []PlanEntry {
	if st == nil {
		return nil
	}
	if st.Generic || st.StageTree != nil {
		return planEntriesFromStageTree(st, cancelled)
	}
	return planEntriesFromSprints(st, cancelled)
}

// planEntriesFromStageTree flattens State.StageTree depth-first. A node's
// status derives from the path snapshots: in CompletedStageTree → completed,
// in ActiveStageTree → in_progress, otherwise pending (cancelled overrides
// pending/in_progress when the run is aborted). Priority follows the panel's
// active-path convention: the executing path is high, unfinished work
// medium, finished work low. Content carries the tree's indentation so the
// flat PlanItems still renders the same hierarchy the F4 panel shows.
//
// Matching across trees is by root-to-node label path: Active/Completed
// StageTrees are pruned snapshots (only the live/finished paths), so sibling
// indexes do not line up with StageTree's. Nodes whose labels repeat along a
// path share a status — the rare parallel-item collision the panel avoids by
// rendering the trees separately; the flat plan degrades gracefully.
func planEntriesFromStageTree(st *workflow.State, cancelled bool) []PlanEntry {
	active := pathSet(st.ActiveStageTree)
	completed := pathSet(st.CompletedStageTree)
	var entries []PlanEntry
	var walk func(n *workflow.StageNode, depth int, prefix string)
	walk = func(n *workflow.StageNode, depth int, prefix string) {
		if n == nil {
			return
		}
		path := prefix + n.Label + "\x00"
		content := strings.Repeat("  ", depth) + n.Label
		status := PlanEntryPending
		priority := PlanPriorityMedium
		switch {
		case completed[path]:
			status, priority = PlanEntryCompleted, PlanPriorityLow
		case active[path]:
			status, priority = PlanEntryInProgress, PlanPriorityHigh
		case cancelled:
			status, priority = PlanEntryCancelled, PlanPriorityLow
		}
		if cancelled && status == PlanEntryInProgress {
			status = PlanEntryCancelled
		}
		entries = append(entries, PlanEntry{Content: content, Priority: priority, Status: status})
		for _, child := range n.Children {
			walk(child, depth+1, path)
		}
	}
	walk(st.StageTree, 0, "")
	return entries
}

// planEntriesFromSprints renders a dev-style run's sprints as plan entries.
func planEntriesFromSprints(st *workflow.State, cancelled bool) []PlanEntry {
	total := st.TotalSprints
	if total < st.Sprint {
		total = st.Sprint
	}
	if total == 0 {
		return nil
	}
	done := map[int]bool{}
	for _, v := range st.VerdictHistory {
		if v.Verdict != "" {
			done[v.Sprint] = true
		}
	}
	entries := make([]PlanEntry, 0, total)
	for n := 1; n <= total; n++ {
		content := "Sprint " + strconv.Itoa(n)
		switch {
		case done[n]:
			entries = append(entries, PlanEntry{Content: content, Priority: PlanPriorityLow, Status: PlanEntryCompleted})
		case n == st.Sprint:
			status := PlanEntryStatus(PlanEntryInProgress)
			priority := PlanPriorityHigh
			if cancelled {
				status, priority = PlanEntryCancelled, PlanPriorityLow
			}
			entries = append(entries, PlanEntry{Content: content, Priority: priority, Status: status})
		case cancelled:
			entries = append(entries, PlanEntry{Content: content, Priority: PlanPriorityLow, Status: PlanEntryCancelled})
		default:
			entries = append(entries, PlanEntry{Content: content, Priority: PlanPriorityMedium, Status: PlanEntryPending})
		}
	}
	return entries
}

// pathSet indexes every root-to-node label path in a PathSnapshot-style tree.
// Keys are NUL-separated label sequences (matching planEntriesFromStageTree's
// walk exactly) so pruned active/completed snapshots still line up with the
// full stage tree.
func pathSet(root *workflow.StageNode) map[string]bool {
	set := map[string]bool{}
	var walk func(n *workflow.StageNode, prefix string)
	walk = func(n *workflow.StageNode, prefix string) {
		if n == nil {
			return
		}
		path := prefix + n.Label + "\x00"
		set[path] = true
		for _, child := range n.Children {
			walk(child, path)
		}
	}
	if root != nil {
		walk(root, "")
	}
	return set
}
