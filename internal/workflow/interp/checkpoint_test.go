package interp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/workflow"
)

func twoStageDef() workflow.Definition {
	return workflow.Definition{
		Name: "checkpointtest",
		Stages: []workflow.Stage{
			{ID: "a", Kind: workflow.StageKindAgentTurn, Role: "a", Prompt: "do a", SaveAs: "a_out"},
			{ID: "b", Kind: workflow.StageKindAgentTurn, Role: "b", Prompt: "b sees: {{.a_out}}", SaveAs: "b_out"},
		},
	}
}

func TestCheckpoint_FreshRunPersistsTraceAndMarksDone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.json")
	a := &fakeRunner{name: "a", responses: []string{"a-result"}}
	b := &fakeRunner{name: "b", responses: []string{"b-result"}}

	r := New(twoStageDef(), "task").WithCheckpoint(path)
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"a": a, "b": b})); err != nil {
		t.Fatalf("Run: %v", err)
	}

	cp, err := LoadCheckpoint(path)
	if err != nil {
		t.Fatalf("LoadCheckpoint: %v", err)
	}
	if cp == nil || !cp.Done {
		t.Fatalf("checkpoint = %+v, want Done", cp)
	}
	if len(cp.Trace) != 2 || cp.Trace[0].StageID != "a" || cp.Trace[1].StageID != "b" {
		t.Errorf("trace = %+v, want entries for a then b", cp.Trace)
	}
}

func TestCheckpoint_ResumeSkipsCompletedLeavesAndRestoresVars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.json")

	// First attempt: "a" succeeds, "b" fails (simulating an interruption
	// after "a" has already been checkpointed).
	a1 := &fakeRunner{name: "a", responses: []string{"a-result"}}
	b1 := &fakeRunner{name: "b", responses: []string{"unused"}, errs: []error{errors.New("network blip")}}
	r1 := New(twoStageDef(), "task").WithCheckpoint(path)
	err := r1.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"a": a1, "b": b1}))
	if err == nil {
		t.Fatal("expected the first attempt to fail at stage b")
	}

	cp, _ := LoadCheckpoint(path)
	if cp == nil || cp.Done || len(cp.Trace) != 1 || cp.Trace[0].StageID != "a" {
		t.Fatalf("checkpoint after failed attempt = %+v, want one entry for stage a, not done", cp)
	}

	// Resume: fresh runners. "a" must NOT be re-invoked; "b" must see "a"'s
	// restored output.
	a2 := &fakeRunner{name: "a", responses: []string{"should not be called"}}
	b2 := &fakeRunner{name: "b", responses: []string{"b-result"}}
	r2 := New(twoStageDef(), "task").WithCheckpoint(path)
	if err := r2.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"a": a2, "b": b2})); err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if a2.callCount() != 0 {
		t.Errorf("stage a's runner was called %d times on resume, want 0 (should be replayed)", a2.callCount())
	}
	if got := b2.lastPrompt(); got != "b sees: a-result" {
		t.Errorf("stage b's prompt = %q, want it to see stage a's replayed output", got)
	}

	cp2, _ := LoadCheckpoint(path)
	if cp2 == nil || !cp2.Done || len(cp2.Trace) != 2 {
		t.Errorf("final checkpoint = %+v, want Done with 2 entries", cp2)
	}
}

func TestCheckpoint_DoneCheckpointStartsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.json")
	a1 := &fakeRunner{name: "a", responses: []string{"a-result"}}
	b1 := &fakeRunner{name: "b", responses: []string{"b-result"}}
	r1 := New(twoStageDef(), "task").WithCheckpoint(path)
	if err := r1.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"a": a1, "b": b1})); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Running again against the same (now Done) checkpoint path starts a
	// fresh run rather than replaying a completed one as a no-op.
	a2 := &fakeRunner{name: "a", responses: []string{"a-result-2"}}
	b2 := &fakeRunner{name: "b", responses: []string{"b-result-2"}}
	r2 := New(twoStageDef(), "task").WithCheckpoint(path)
	if err := r2.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"a": a2, "b": b2})); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if a2.callCount() != 1 || b2.callCount() != 1 {
		t.Errorf("expected a fresh full run (1 call each), got a=%d b=%d", a2.callCount(), b2.callCount())
	}
}

func TestCheckpoint_MismatchOnEditedDefinition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.json")
	if err := SaveCheckpoint(path, &Checkpoint{
		DefinitionName: "checkpointtest",
		Task:           "task",
		Trace:          []TraceEntry{{StageID: "renamed_stage", SaveAs: "a_out", Value: "a-result"}},
	}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	a := &fakeRunner{name: "a", responses: []string{"a-result"}}
	b := &fakeRunner{name: "b", responses: []string{"b-result"}}
	r := New(twoStageDef(), "task").WithCheckpoint(path)
	err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"a": a, "b": b}))
	if err == nil || !strings.Contains(err.Error(), "checkpoint mismatch") {
		t.Fatalf("expected a checkpoint mismatch error, got %v", err)
	}
}

func TestCheckpoint_MismatchOnDifferentTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.json")
	if err := SaveCheckpoint(path, &Checkpoint{
		DefinitionName: "checkpointtest",
		Task:           "a different task",
		Trace:          []TraceEntry{{StageID: "a", SaveAs: "a_out", Value: "a-result"}},
	}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	a := &fakeRunner{name: "a", responses: []string{"a-result"}}
	b := &fakeRunner{name: "b", responses: []string{"b-result"}}
	r := New(twoStageDef(), "task").WithCheckpoint(path)
	if err := r.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"a": a, "b": b})); err == nil {
		t.Fatal("expected an error resuming a checkpoint for a different task")
	}
}

// TestCheckpoint_ResumeMidPassLoopRetry covers the scenario the whole
// mechanism exists for: a bounded retry loop interrupted partway through,
// resuming exactly at the interrupted pass without re-running earlier ones.
func TestCheckpoint_ResumeMidPassLoopRetry(t *testing.T) {
	def := workflow.Definition{
		Name: "loopcheckpoint",
		Stages: []workflow.Stage{
			{
				ID: "pass_loop", Kind: workflow.StageKindLoop, MaxIterations: 5, IterationVar: "pass",
				Body: []workflow.Stage{
					{ID: "gen", Kind: workflow.StageKindAgentTurn, Role: "gen", Prompt: "gen pass {{.pass}}", SaveAs: "gen_out"},
					{
						ID: "eval", Kind: workflow.StageKindAgentTurn, Role: "eval", Prompt: "eval {{.gen_out}} pass {{.pass}}", SaveAs: "eval_out",
						Verdict: map[string]workflow.VerdictRule{
							"good_to_go":       {Action: "break"},
							"needs_refinement": {Action: "retry"},
						},
					},
				},
			},
		},
	}
	path := filepath.Join(t.TempDir(), "cp.json")

	// Pass 1: gen succeeds, eval says needs_refinement (recorded). Pass 2:
	// gen succeeds (recorded), eval fails outright (simulated interruption).
	gen1 := &fakeRunner{name: "gen", responses: []string{"gen-pass-1", "gen-pass-2"}}
	eval1 := &fakeRunner{name: "eval", responses: []string{"needs_refinement", "unused"}, errs: []error{nil, errors.New("blip")}}
	r1 := New(def, "task").WithCheckpoint(path)
	if err := r1.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"gen": gen1, "eval": eval1})); err == nil {
		t.Fatal("expected the first attempt to fail during pass 2's eval")
	}

	cp, _ := LoadCheckpoint(path)
	if cp == nil || len(cp.Trace) != 3 {
		t.Fatalf("checkpoint = %+v, want 3 entries (gen1, eval1, gen2)", cp)
	}

	// Resume: gen must not be re-invoked at all (both passes already
	// recorded); eval only needs its pass-2 call.
	gen2 := &fakeRunner{name: "gen", responses: []string{"should not be called"}}
	eval2 := &fakeRunner{name: "eval", responses: []string{"good_to_go"}}
	r2 := New(def, "task").WithCheckpoint(path)
	if err := r2.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{"gen": gen2, "eval": eval2})); err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if gen2.callCount() != 0 {
		t.Errorf("gen was called %d times on resume, want 0", gen2.callCount())
	}
	if eval2.callCount() != 1 {
		t.Errorf("eval was called %d times on resume, want 1 (just pass 2's re-attempt)", eval2.callCount())
	}
	if got := eval2.lastPrompt(); !strings.Contains(got, "gen-pass-2") || !strings.Contains(got, "pass 2") {
		t.Errorf("eval's resumed prompt = %q, expected pass 2's replayed generator output", got)
	}
}

func TestCheckpoint_ParallelGroupAtomicReplaySkipsRerun(t *testing.T) {
	def := workflow.Definition{
		Name: "swarmcheckpoint",
		Stages: []workflow.Stage{
			{ID: "plan_stage", Kind: workflow.StageKindAgentTurn, Role: "planner", Prompt: "plan", SaveAs: "plan"},
			{
				ID: "fanout", Kind: workflow.StageKindParallelGroup, Over: "Item", From: "plan", SaveAs: "results",
				Body: []workflow.Stage{{ID: "work", Kind: workflow.StageKindAgentTurn, Role: "w", Prompt: "do {{.item}}"}},
			},
			{ID: "after", Kind: workflow.StageKindAgentTurn, Role: "after", Prompt: "results: {{range .results}}{{.Status}} {{end}}"},
		},
	}
	path := filepath.Join(t.TempDir(), "cp.json")
	planner := &fakeRunner{name: "planner", responses: []string{"## Item 1\na\n\n## Item 2\nb\n"}}
	worker1 := &fakeRunner{name: "w", responses: []string{"ok"}}
	after1 := &fakeRunner{name: "after", responses: []string{"done"}}

	r1 := New(def, "task").WithCheckpoint(path)
	if err := r1.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{
		"planner": planner, "w": worker1, "after": after1,
	})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if worker1.callCount() != 2 {
		t.Fatalf("expected 2 worker calls on the fresh run, got %d", worker1.callCount())
	}

	// Second run against the completed checkpoint starts fresh (see
	// TestCheckpoint_DoneCheckpointStartsFresh) -- to exercise atomic replay
	// specifically, hand-author a not-done checkpoint whose trace covers the
	// plan and the fanout but not "after", simulating an interruption right
	// after the group finished.
	cp, _ := LoadCheckpoint(path)
	cp.Done = false
	cp.Trace = cp.Trace[:2] // plan_stage, fanout -- drop "after"'s entry
	if err := SaveCheckpoint(path, cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	plannerResume := &fakeRunner{name: "planner", responses: []string{"should not be called"}}
	workerResume := &fakeRunner{name: "w", responses: []string{"should not be called"}}
	afterResume := &fakeRunner{name: "after", responses: []string{"done-2"}}
	r2 := New(def, "task").WithCheckpoint(path)
	if err := r2.Run(context.Background(), runCfg(map[string]workflow.TurnRunner{
		"planner": plannerResume, "w": workerResume, "after": afterResume,
	})); err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if plannerResume.callCount() != 0 {
		t.Errorf("planner called %d times on resume, want 0", plannerResume.callCount())
	}
	if workerResume.callCount() != 0 {
		t.Errorf("worker called %d times on resume, want 0 (fanout should replay atomically)", workerResume.callCount())
	}
	if afterResume.callCount() != 1 {
		t.Errorf("after called %d times on resume, want 1", afterResume.callCount())
	}
	if got := afterResume.lastPrompt(); !strings.HasPrefix(got, "results:") {
		t.Errorf("after's prompt = %q, expected it to render against the replayed results slice without error", got)
	}
}

// Note: there is no test for "interrupted mid-flight through parallel_group
// redoes the whole group" (documented on Checkpoint) because that scenario
// is only reachable via a hard process crash — execParallelGroup isolates
// per-item errors (including a cancelled context) rather than treating them
// as fatal, so a graceful ctx cancellation still lets the group finish and
// checkpoint normally within a single process, same as a clean run. The
// atomic-replay behavior itself (skip vs. redo based on whether a trace
// entry exists yet) is exercised directly by
// TestCheckpoint_ParallelGroupAtomicReplaySkipsRerun and the fresh-run tests
// above.
