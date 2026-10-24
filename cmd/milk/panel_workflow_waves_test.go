package main

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/workflow"
	"github.com/scoutme/milk/internal/workflow/interp"
)

// stagedRunner answers every call with resp, except that calls from the
// blockFrom-th on park until release is closed (signalling reached first).
type stagedRunner struct {
	resp      string
	blockFrom int
	release   chan struct{}
	reached   chan struct{}

	mu    sync.Mutex
	calls int
}

func (r *stagedRunner) Name() string { return "staged" }

func (r *stagedRunner) Run(_ context.Context, _ string, _ io.Writer) (string, error) {
	r.mu.Lock()
	r.calls++
	n := r.calls
	r.mu.Unlock()
	if r.blockFrom > 0 && n >= r.blockFrom {
		select {
		case r.reached <- struct{}{}:
		default:
		}
		<-r.release
	}
	return r.resp, nil
}

// While a later fan-out wave is still running, the workflow panel shows the
// run as in progress: the root and its ancestors are active and the in-flight
// items are listed alongside the finished ones. The active and completed
// path snapshots must be rooted the same way as the static stage tree for the
// panel to match them — a completed tree holding both the designer and the
// pass loop is rooted at the synthetic "workflow" container, an active tree
// holding only the pass loop used not to be, and the panel then drew the whole
// run as done and dropped the running items.
func TestWorkflowPanel_ShowsRunningItemsOfLaterWave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	reg, errs := workflow.LoadRegistry()
	if len(errs) != 0 {
		t.Fatalf("LoadRegistry: %v", errs)
	}
	def, ok := reg.Lookup("team")
	if !ok {
		t.Fatal(`built-in "team" workflow missing`)
	}

	plan := "NO_QUESTIONS\n## Spec\nx\n\n## Item Plan\n" +
		"## Item 1\na\n\n## Item 2\nb\n\n" +
		"## Item 3 (depends_on: 1, 2)\nc\n\n## Item 4 (depends_on: 1, 2)\nd\n"
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	runners := map[string]workflow.TurnRunner{
		"designer": &stagedRunner{resp: plan},
		// Item 1 and 2 (wave 1) pass through; the third worker call, the
		// first of wave 2, parks.
		"worker":      &stagedRunner{resp: "did", blockFrom: 3, release: release, reached: reached},
		"evaluator":   &stagedRunner{resp: "good_to_go"},
		"coordinator": &stagedRunner{resp: "all_done"},
		"implementer": &stagedRunner{resp: "done"},
	}

	var mu sync.Mutex
	st := &workflow.State{StageTree: definitionStageTree(def.Stages)}
	send := func(msg tea.Msg) {
		if pm, ok := msg.(workflow.ProgressMsg); ok {
			mu.Lock()
			st.ApplyProgress(pm)
			mu.Unlock()
		}
	}
	done := make(chan error, 1)
	go func() {
		done <- interp.New(def, "task").Run(context.Background(), workflow.RunConfig{Runners: runners, Send: send})
	}()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("wave 2 never started")
	}
	time.Sleep(100 * time.Millisecond) // let the workers' progress reports land

	mu.Lock()
	rows := buildGenericWorkflowPanelLines(st, 40)
	mu.Unlock()
	for i := range rows {
		rows[i] = ansi.Strip(rows[i])
	}
	panel := strings.Join(rows, "\n")

	for _, want := range []string{"▸ workflow", "▸   pass 1", "▸     fanout", "▸       item 3", "▸       item 4", "✓       item 1", "✓       item 2"} {
		if !strings.Contains(panel, want) {
			t.Errorf("panel missing %q:\n%s", want, panel)
		}
	}
	if strings.Contains(panel, "✓ workflow") {
		t.Errorf("run is still going, but the root is marked done:\n%s", panel)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// Loop stages are labelled from their own ids ("<noun>_loop[N]" → "<noun> N"),
// so a definition's inner retry loop and its outer pass loop read differently.
func TestDisplayStageLabel_LoopsUseTheirOwnNoun(t *testing.T) {
	tests := map[string]string{
		"team_pass_loop[2]":       "pass 2",
		"worker_attempt_loop[1]":  "attempt 1",
		"sprint_loop[3]":          "sprint 3",
		"pass_loop[1]":            "pass 1",
		"worker_attempt_loop":     "attempt loop",
		"worker_fanout item[4]":   "item 4",
		"worker_fanout (5 items)": "fanout",
	}
	for in, want := range tests {
		if got := displayStageLabel(in); got != want {
			t.Errorf("displayStageLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every built-in workflow shows a run in progress as in progress: while a
// stage is mid-turn the root and the path down to it are active (▸), not
// drawn as already done. Guards the shared snapshot-root rule across dev,
// pair, swarm and team rather than one definition.
func TestWorkflowPanel_BuiltinsShowRunInProgress(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	reg, errs := workflow.LoadRegistry()
	if len(errs) != 0 {
		t.Fatalf("LoadRegistry: %v", errs)
	}
	const sprintPlan = "NO_QUESTIONS\n## Spec\nx\n\n## Sprint 1\nbuild a\n\n## Sprint 2\nbuild b\n\nmax_passes: 2\n"
	const itemPlan = "NO_QUESTIONS\n## Spec\nx\n\n## Item Plan\n## Item 1\na\n\n## Item 2\nb\n"
	tests := []struct {
		name  string
		plan  string
		block string // the role whose first turn parks
	}{
		{"dev", sprintPlan, "generator"},
		{"pair", sprintPlan, "generator"},
		{"swarm", itemPlan, "worker"},
		{"team", itemPlan, "worker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def, ok := reg.Lookup(tt.name)
			if !ok {
				t.Fatalf("built-in %q missing", tt.name)
			}
			release := make(chan struct{})
			reached := make(chan struct{}, 1)
			runners := map[string]workflow.TurnRunner{}
			for _, role := range def.Roles {
				switch role {
				case "designer":
					runners[role] = &stagedRunner{resp: tt.plan}
				case tt.block:
					runners[role] = &stagedRunner{resp: "did", blockFrom: 1, release: release, reached: reached}
				default:
					runners[role] = &stagedRunner{resp: "good_to_go"}
				}
			}
			var mu sync.Mutex
			st := &workflow.State{StageTree: definitionStageTree(def.Stages)}
			send := func(msg tea.Msg) {
				if pm, ok := msg.(workflow.ProgressMsg); ok {
					mu.Lock()
					st.ApplyProgress(pm)
					mu.Unlock()
				}
			}
			done := make(chan error, 1)
			go func() {
				done <- interp.New(def, "task").Run(context.Background(), workflow.RunConfig{Runners: runners, Send: send})
			}()
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			select {
			case <-reached:
			case <-time.After(10 * time.Second):
				t.Fatalf("%s: the %s stage never started", tt.name, tt.block)
			}
			time.Sleep(100 * time.Millisecond)

			mu.Lock()
			rows := buildGenericWorkflowPanelLines(st, 40)
			mu.Unlock()
			for i := range rows {
				rows[i] = ansi.Strip(rows[i])
			}
			panel := strings.Join(rows, "\n")
			if !strings.Contains(panel, "▸ workflow") {
				t.Errorf("root not shown active mid-run:\n%s", panel)
			}
			if strings.Contains(panel, "✓ workflow") {
				t.Errorf("run still going, but the root is drawn done:\n%s", panel)
			}
			if strings.Count(panel, "▸") < 3 {
				t.Errorf("path down to the running stage not shown active:\n%s", panel)
			}
			close(release)
			<-done
		})
	}
}
