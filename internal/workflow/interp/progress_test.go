package interp

import (
	"context"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/scoutme/milk/internal/workflow"
)

// progressCollector accumulates every workflow.ProgressMsg sent via a
// workflow.RunConfig.Send func, safe for concurrent use (parallel_group items
// report from separate goroutines).
type progressCollector struct {
	mu       sync.Mutex
	messages []workflow.ProgressMsg
}

func (c *progressCollector) send() func(tea.Msg) {
	return func(m tea.Msg) {
		if pm, ok := m.(workflow.ProgressMsg); ok {
			c.mu.Lock()
			c.messages = append(c.messages, pm)
			c.mu.Unlock()
		}
	}
}

func (c *progressCollector) all() []workflow.ProgressMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]workflow.ProgressMsg{}, c.messages...)
}

// pathTreeContains reports whether any node in the PathSnapshot tree has a
// label containing substr. Returns false when snap is nil.
func pathTreeContains(snap *workflow.PathSnapshot, substr string) bool {
	if snap == nil || snap.Root == nil {
		return false
	}
	var walk func(n *workflow.StageNode) bool
	walk = func(n *workflow.StageNode) bool {
		if strings.Contains(n.Label, substr) {
			return true
		}
		for _, c := range n.Children {
			if walk(c) {
				return true
			}
		}
		return false
	}
	return walk(snap.Root)
}

func TestReportProgress_AgentTurnReportsRoleAndStagePath(t *testing.T) {
	collector := &progressCollector{}
	a := &fakeRunner{name: "a", responses: []string{"a-out"}}
	b := &fakeRunner{name: "b", responses: []string{"b-out"}}
	r := New(twoStageDef(), "the task")
	cfg := workflow.RunConfig{Runners: map[string]workflow.TurnRunner{"a": a, "b": b}, Send: collector.send()}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := collector.all()
	if len(msgs) != 2 {
		t.Fatalf("got %d progress messages, want 2 (one per stage), got %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "a" || msgs[1].Role != "b" {
		t.Errorf("roles = [%q, %q], want [a, b]", msgs[0].Role, msgs[1].Role)
	}
	for _, m := range msgs {
		if m.WorkflowName != "checkpointtest" || m.Task != "the task" {
			t.Errorf("message = %+v, want WorkflowName=checkpointtest Task=%q", m, "the task")
		}
	}
}

func TestReportProgress_LoopStagePathReflectsIteration(t *testing.T) {
	collector := &progressCollector{}
	def := workflow.Definition{
		Name: "loopprogress",
		Stages: []workflow.Stage{
			{
				ID: "pass_loop", Kind: workflow.StageKindLoop, MaxIterations: 2, IterationVar: "pass",
				Body: []workflow.Stage{
					{
						ID: "eval", Kind: workflow.StageKindAgentTurn, Role: "eval", Prompt: "go",
						Verdict: map[string]workflow.VerdictRule{
							"needs_refinement": {Action: "retry"},
							"good_to_go":       {Action: "break"},
						},
					},
				},
			},
		},
	}
	eval := &fakeRunner{name: "eval", responses: []string{"needs_refinement", "good_to_go"}}
	r := New(def, "task")
	cfg := workflow.RunConfig{Runners: map[string]workflow.TurnRunner{"eval": eval}, Send: collector.send()}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := collector.all()
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (one per pass)", len(msgs))
	}
	if !pathTreeContains(msgs[0].ActivePaths, "pass_loop[1]") {
		t.Errorf("first message ActivePaths missing pass_loop[1], got %+v", msgs[0].ActivePaths)
	}
	if !pathTreeContains(msgs[1].ActivePaths, "pass_loop[2]") {
		t.Errorf("second message ActivePaths missing pass_loop[2], got %+v", msgs[1].ActivePaths)
	}
}

func TestReportProgress_ParallelGroupAnnouncesItemCount(t *testing.T) {
	collector := &progressCollector{}
	def := workflow.Definition{
		Name: "swarmprogress",
		Stages: []workflow.Stage{
			{ID: "plan_stage", Kind: workflow.StageKindAgentTurn, Role: "planner", Prompt: "plan", SaveAs: "plan"},
			{
				ID: "fanout", Kind: workflow.StageKindParallelGroup, Over: "Item", From: "plan",
				Body: []workflow.Stage{{ID: "work", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "go"}},
			},
		},
	}
	planner := &fakeRunner{name: "planner", responses: []string{"## Item 1\na\n\n## Item 2\nb\n\n## Item 3\nc\n"}}
	worker := &fakeRunner{name: "w", responses: []string{"ok"}}
	r := New(def, "task")
	cfg := workflow.RunConfig{Runners: map[string]workflow.TurnRunner{"planner": planner, "w": worker}, Send: collector.send()}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := collector.all()
	var found bool
	for _, m := range msgs {
		if pathTreeContains(m.ActivePaths, "fanout (3 items)") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a progress message announcing the fanout's item count, got %+v", msgs)
	}
}
