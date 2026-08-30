package interp

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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
	if len(msgs) < 2 {
		t.Fatalf("got %d progress messages, want at least 2 (one per stage), got %+v", len(msgs), msgs)
	}
	var sawRoleA, sawRoleB bool
	for _, m := range msgs {
		if m.Role == "a" {
			sawRoleA = true
		}
		if m.Role == "b" {
			sawRoleB = true
		}
	}
	if !sawRoleA || !sawRoleB {
		t.Errorf("expected messages for roles a and b, got roles from %+v", msgs)
	}
	for _, m := range msgs {
		if m.WorkflowName != "checkpointtest" || m.Task != "the task" {
			t.Errorf("message = %+v, want WorkflowName=checkpointtest Task=%q", m, "the task")
		}
	}
	if !pathTreeContains(msgs[0].ActivePaths, "a") {
		t.Errorf("first message ActivePaths missing agent stage a, got %+v", msgs[0].ActivePaths)
	}
	var sawBActive bool
	for _, m := range msgs {
		if m.Role == "b" && pathTreeContains(m.ActivePaths, "b") {
			sawBActive = true
			break
		}
	}
	if !sawBActive {
		t.Errorf("expected a message with role b and ActivePaths containing b, got %+v", msgs)
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
	var sawPass1, sawPass2, sawCompletedPass1 bool
	for _, msg := range msgs {
		sawPass1 = sawPass1 || pathTreeContains(msg.ActivePaths, "pass_loop[1]")
		sawPass2 = sawPass2 || pathTreeContains(msg.ActivePaths, "pass_loop[2]")
		sawCompletedPass1 = sawCompletedPass1 || pathTreeContains(msg.CompletedPaths, "pass_loop[1]")
	}
	if !sawPass1 || !sawPass2 {
		t.Errorf("expected progress to show active pass_loop[1] and pass_loop[2], got %+v", msgs)
	}
	if !sawCompletedPass1 {
		t.Errorf("expected progress to retain completed pass_loop[1], got %+v", msgs)
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

func TestReportProgress_ParallelGroupShowsConcurrentBranches(t *testing.T) {
	collector := &progressCollector{}
	var mu sync.Mutex
	started := 0
	release := make(chan struct{})
	worker := &blockingRunner{
		before: func() {
			mu.Lock()
			started++
			if started == 2 {
				close(release)
			}
			mu.Unlock()
			<-release
		},
		after: func() {},
		delay: 10 * time.Millisecond,
	}
	def := workflow.Definition{
		Name: "swarmprogress",
		Stages: []workflow.Stage{
			{ID: "plan_stage", Kind: workflow.StageKindAgentTurn, Role: "planner", Prompt: "plan", SaveAs: "plan"},
			{
				ID: "fanout", Kind: workflow.StageKindParallelGroup, Over: "Item", From: "plan", MaxConcurrency: 2,
				Body: []workflow.Stage{{ID: "work", Kind: workflow.StageKindAgentTurn, Role: "slow", Prompt: "go"}},
			},
		},
	}
	planner := &fakeRunner{name: "planner", responses: []string{"## Item 1\na\n\n## Item 2\nb\n"}}
	r := New(def, "task")
	cfg := workflow.RunConfig{Runners: map[string]workflow.TurnRunner{"planner": planner, "slow": worker}, Send: collector.send()}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := collector.all()
	var completedBoth bool
	for _, m := range msgs {
		if pathTreeContains(m.ActivePaths, "fanout item[1]") && pathTreeContains(m.ActivePaths, "fanout item[2]") {
			completedBoth = true
			break
		}
	}
	if !completedBoth {
		t.Fatalf("expected one progress snapshot to include both active fanout item branches, got %+v", msgs)
	}
	for _, m := range msgs {
		if pathTreeContains(m.CompletedPaths, "fanout item[1]") && pathTreeContains(m.CompletedPaths, "fanout item[2]") {
			return
		}
	}
	t.Fatalf("expected completed fanout item branches to stay in progress snapshots, got %+v", msgs)
}

func TestReportProgress_OverLoopKeepsCompletedIterations(t *testing.T) {
	collector := &progressCollector{}
	def := workflow.Definition{
		Name: "devprogress",
		Stages: []workflow.Stage{
			{ID: "design", Kind: workflow.StageKindAgentTurn, Role: "designer", Prompt: "plan", SaveAs: "plan"},
			{
				ID: "sprint_loop", Kind: workflow.StageKindLoop, Over: "Sprint", From: "plan",
				Body: []workflow.Stage{
					{
						ID: "pass_loop", Kind: workflow.StageKindLoop, MaxIterations: 1, IterationVar: "pass",
						Body: []workflow.Stage{{ID: "eval", Kind: workflow.StageKindAgentTurn, Role: "evaluator", Prompt: "go", Verdict: map[string]workflow.VerdictRule{"good_to_go": {Action: "break"}}}},
					},
				},
			},
		},
	}
	designer := &fakeRunner{name: "designer", responses: []string{"## Sprint 1\none\n\n## Sprint 2\ntwo\n"}}
	evaluator := &fakeRunner{name: "evaluator", responses: []string{"good_to_go"}}
	r := New(def, "task")
	cfg := workflow.RunConfig{Runners: map[string]workflow.TurnRunner{"designer": designer, "evaluator": evaluator}, Send: collector.send()}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := collector.all()
	for _, m := range msgs {
		if pathTreeContains(m.CompletedPaths, "sprint_loop[1]") && pathTreeContains(m.CompletedPaths, "sprint_loop[2]") {
			return
		}
	}
	t.Fatalf("expected completed sprint iterations to stay in progress snapshots, got %+v", msgs)
}

func TestReportProgress_CompletedAgentTurnStaysWhileNextAgentActive(t *testing.T) {
	collector := &progressCollector{}
	def := workflow.Definition{
		Name: "devprogress",
		Stages: []workflow.Stage{
			{
				ID: "pass_loop", Kind: workflow.StageKindLoop, MaxIterations: 1, IterationVar: "pass",
				Body: []workflow.Stage{
					{ID: "generator", Kind: workflow.StageKindAgentTurn, Role: "generator", Prompt: "generate", SaveAs: "out"},
					{ID: "evaluator", Kind: workflow.StageKindAgentTurn, Role: "evaluator", Prompt: "eval", Verdict: map[string]workflow.VerdictRule{"good_to_go": {Action: "break"}}},
				},
			},
		},
	}
	generator := &fakeRunner{name: "generator", responses: []string{"generated"}}
	evaluator := &fakeRunner{name: "evaluator", responses: []string{"good_to_go"}}
	r := New(def, "task")
	cfg := workflow.RunConfig{Runners: map[string]workflow.TurnRunner{"generator": generator, "evaluator": evaluator}, Send: collector.send()}
	if err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, msg := range collector.all() {
		if pathTreeContains(msg.ActivePaths, "evaluator") && pathTreeContains(msg.CompletedPaths, "generator") {
			return
		}
	}
	t.Fatalf("expected completed generator to remain while evaluator is active, got %+v", collector.all())
}

func TestInsertPath_NoNestedWorkflowWrappers(t *testing.T) {
	// Simulate completed paths from a dev-like workflow: design completes
	// first, then sprint_loop[1]/pass_loop[1]/generator, then the pass and
	// sprint complete.  The merged tree must NOT nest redundant "workflow"
	// wrapper nodes — that was the root cause of wrong depths and duplicated
	// entries in the workflow panel.
	paths := [][]string{
		{"design"},
		{"sprint_loop[1]", "pass_loop[1]", "generator"},
		{"sprint_loop[1]", "pass_loop[1]"},
		{"sprint_loop[1]"},
	}
	var root *workflow.StageNode
	for _, p := range paths {
		root = insertPath(root, p)
	}
	// The root should be "workflow" (the synthetic container) with two
	// children: "design" and "sprint_loop[1]".  There must be exactly one
	// "workflow" level — no nesting.
	if root.Label != "workflow" {
		t.Fatalf("expected root label %q, got %q", "workflow", root.Label)
	}
	if len(root.Children) != 2 {
		t.Fatalf("expected 2 children, got %d: %v", len(root.Children), formatChildren(root))
	}
	// Verify no child is a "workflow" wrapper (the old bug created nested ones).
	for _, c := range root.Children {
		if c.Label == "workflow" {
			t.Fatalf("found nested workflow wrapper node — old bug reproduced: %s", formatTree(root, 0))
		}
	}
}

func TestInsertPath_MultipleSprintIterations(t *testing.T) {
	// Two sprint iterations completing sequentially must share the same
	// "workflow" container without wrapping.
	paths := [][]string{
		{"design"},
		{"sprint_loop[1]", "pass_loop[1]", "generator"},
		{"sprint_loop[1]", "pass_loop[1]"},
		{"sprint_loop[1]"},
		{"sprint_loop[2]", "pass_loop[1]", "generator"},
		{"sprint_loop[2]", "pass_loop[1]"},
		{"sprint_loop[2]"},
	}
	var root *workflow.StageNode
	for _, p := range paths {
		root = insertPath(root, p)
	}
	if root.Label != "workflow" {
		t.Fatalf("expected root label %q, got %q", "workflow", root.Label)
	}
	if len(root.Children) != 3 {
		t.Fatalf("expected 3 children (design, sprint_loop[1], sprint_loop[2]), got %d: %v", len(root.Children), formatChildren(root))
	}
	for _, c := range root.Children {
		if c.Label == "workflow" {
			t.Fatalf("found nested workflow wrapper: %s", formatTree(root, 0))
		}
	}
}

func formatChildren(n *workflow.StageNode) []string {
	var out []string
	for _, c := range n.Children {
		out = append(out, c.Label)
	}
	return out
}

func formatTree(n *workflow.StageNode, depth int) string {
	indent := strings.Repeat("  ", depth)
	s := indent + n.Label + "\n"
	for _, c := range n.Children {
		s += formatTree(c, depth+1)
	}
	return s
}
