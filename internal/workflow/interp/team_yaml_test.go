package interp

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/scoutme/milk/internal/testutil"
	"github.com/scoutme/milk/internal/workflow"
	"github.com/scoutme/milk/internal/workflow/board"
)

func loadTeamDefinition(t *testing.T) workflow.Definition {
	t.Helper()
	home := testutil.SandboxHome(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	reg, errs := workflow.LoadRegistry()
	if len(errs) != 0 {
		t.Fatalf("LoadRegistry errors: %v", errs)
	}
	def, ok := reg.Lookup("team")
	if !ok {
		t.Fatal(`expected built-in "team" definition to be registered`)
	}
	return def
}

// End-to-end over the real team.yaml: designer plan → board_seed → pass
// loop (fan-out + coordinator standup) → final evaluation with the
// NO_ISSUES short-circuit. Asserts the coordination surfaces actually
// render: the seeded board reaches the worker/evaluator/coordinator/final
// prompts as {{.board}}, and the coordinator's verdict ends the loop.
func TestTeamYAML_EndToEnd_BoardRendersThroughStages(t *testing.T) {
	def := loadTeamDefinition(t)

	designer := &fakeRunner{name: "designer", responses: []string{
		"NO_QUESTIONS\n## Spec\nBuild two things.\n\n## Item Plan\n" +
			"## Item 1\nBuild the first thing.\n\n## Item 2\nBuild the second thing.\n",
	}}
	// Identical worker responses keep the assertion order-independent —
	// fan-out items run concurrently.
	worker := &fakeRunner{name: "worker", responses: []string{"did the work", "did the work", "did the work", "did the work"}}
	evaluator := &fakeRunner{name: "evaluator", responses: []string{
		"good_to_go", // worker_eval item A
		"good_to_go", // worker_eval item B
		"NO_ISSUES",  // final_evaluation
	}}
	coordinator := &fakeRunner{name: "coordinator", responses: []string{"all_done"}}
	// final_implementation is skipped (NO_ISSUES) — never called.

	cfg := runCfg(map[string]workflow.TurnRunner{
		"designer": designer, "worker": worker, "evaluator": evaluator,
		"coordinator": coordinator,
	})
	if err := New(def, "build two things").Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The seeded board rendered into the worker, evaluator, coordinator and
	// final-evaluation prompts (worker prompts carry the item sections too).
	worker.mu.Lock()
	workerPrompts := append([]string(nil), worker.prompts...)
	worker.mu.Unlock()
	if len(workerPrompts) == 0 {
		t.Fatal("worker never ran")
	}
	for _, p := range workerPrompts {
		if !strings.Contains(p, "shared board") || !strings.Contains(p, "Item 1") {
			t.Errorf("worker prompt missing the board snapshot/item:\n%s", p)
		}
		if !strings.Contains(p, "board_claim item") {
			t.Errorf("worker prompt missing the claim-first convention:\n%s", p)
		}
	}

	// lastPrompt takes f.mu itself — locking here would self-deadlock a
	// non-reentrant mutex (sync.Mutex, held by this very goroutine).
	coPrompt := coordinator.lastPrompt()
	for _, want := range []string{"Board (live)", "all_done", "more_work"} {
		if !strings.Contains(coPrompt, want) {
			t.Errorf("coordinator prompt missing %q:\n%s", want, coPrompt)
		}
	}
	// The coordinator's verdict ("all_done") ended the pass loop after one
	// round: exactly two worker evals + one final eval ran, and the
	// implementer stage was skipped via NO_ISSUES (not in the runner map —
	// an attempted lookup would have failed the run).
	evaluator.mu.Lock()
	if got := len(evaluator.prompts); got != 3 {
		t.Errorf("evaluator ran %d times, want 3 (2 item evals + final)", got)
	}
	evaluator.mu.Unlock()
}

// board_seed without the definition flag fails validation before any run —
// the pairing lives in Definition.Validate, not in name-specific code.
func TestTeamYAML_DeclaresBoardAndValidates(t *testing.T) {
	def := loadTeamDefinition(t)
	if err := def.Validate(); err != nil {
		t.Errorf("team definition must validate: %v", err)
	}
	stripped := def
	stripped.Board = false
	if err := stripped.Validate(); err == nil {
		t.Error("dropping board: true must invalidate the definition (board_seed pairing)")
	}
	_ = io.Discard
}

// The shipped team.yaml wires the generic engine features together: a
// refinement retry reopens the item (so the worker's claim is accepted
// again), and a later pass re-runs only the items the stand-up reopened.
// Item 1 is accepted first time; item 2 needs one refinement and is then
// reopened by the stand-up for a second pass.
func TestTeamYAML_RefinementRetryAndSecondPassRunOnlyReopenedItems(t *testing.T) {
	def := loadTeamDefinition(t)

	var mu sync.Mutex
	workerCalls := map[int]int{}
	claimRefused := 0
	evalCalls := map[int]int{}
	standups := 0

	designer := &funcRunner{name: "designer", fn: func(context.Context, string) (string, error) {
		return "NO_QUESTIONS\n## Spec\nx\n\n## Item Plan\n## Item 1\na\n\n## Item 2\nb\n", nil
	}}
	worker := &funcRunner{name: "worker", fn: func(ctx context.Context, _ string) (string, error) {
		idx := itemOf(t, ctx)
		b := board.FromContext(ctx)
		mu.Lock()
		defer mu.Unlock()
		workerCalls[idx]++
		if _, ok := b.Claim(idx, board.IdentityFrom(ctx)); !ok {
			claimRefused++
			return "SKIPPED: claim refused", nil
		}
		b.Update(idx, board.StatusDone, "") // done before evaluation, as the live run did
		return fmt.Sprintf("worked item %d", idx), nil
	}}
	evaluator := &funcRunner{name: "evaluator", fn: func(ctx context.Context, _ string) (string, error) {
		if board.IdentityFrom(ctx) == "evaluator" { // final_evaluation: no item
			return "NO_ISSUES", nil
		}
		idx := itemOf(t, ctx)
		mu.Lock()
		defer mu.Unlock()
		evalCalls[idx]++
		if idx == 2 && evalCalls[idx] == 1 {
			return "needs_refinement", nil
		}
		return "good_to_go", nil
	}}
	coordinator := &funcRunner{name: "coordinator", fn: func(ctx context.Context, _ string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		standups++
		if standups == 1 {
			board.FromContext(ctx).Update(2, board.StatusTodo, "")
			return "more_work", nil
		}
		return "all_done", nil
	}}
	cfg := runCfg(map[string]workflow.TurnRunner{
		"designer": designer, "worker": worker, "evaluator": evaluator, "coordinator": coordinator,
		"implementer": &funcRunner{name: "implementer", fn: func(context.Context, string) (string, error) { return "unused", nil }},
	})
	if err := New(def, "build two things").Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if claimRefused != 0 {
		t.Errorf("%d worker claim(s) refused — a refinement retry or a reopened item could not re-claim", claimRefused)
	}
	if workerCalls[1] != 1 {
		t.Errorf("item 1 worked %d times, want exactly once (settled items are not re-run in pass 2)", workerCalls[1])
	}
	if workerCalls[2] != 3 {
		t.Errorf("item 2 worked %d times, want 3 (attempt 1, refinement, pass 2)", workerCalls[2])
	}
	if evalCalls[1] != 1 || evalCalls[2] != 3 {
		t.Errorf("evaluator calls = %v, want item 1 once and item 2 three times (no due-diligence reviews of skipped items)", evalCalls)
	}
	if standups != 2 {
		t.Errorf("standups = %d, want 2", standups)
	}
}
