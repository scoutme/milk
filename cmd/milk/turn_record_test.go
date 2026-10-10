package main

import (
	"bytes"
	"context"
	"errors"
	tea "github.com/charmbracelet/bubbletea"
	"io"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
	"github.com/scoutme/milk/internal/workflow/interp"
)

func spawnTrail() []session.TrailStep {
	return []session.TrailStep{{Calls: []session.TrailCall{{
		ID: "c1", Name: "spawn_background_agent", Args: `{"label":"research"}`, Result: `{"output":"Spawned background agent job-1"}`,
	}}}}
}

func readTrail() []session.TrailStep {
	return []session.TrailStep{{Calls: []session.TrailCall{
		{ID: "r1", Name: "read_file", Args: `{"path":"a.go"}`, Result: "x"},
		{ID: "r2", Name: "read_file", Args: `{"path":"b.go"}`, Result: "y"},
	}}}
}

func TestRecordSilentTurn(t *testing.T) {
	t.Run("spawned job gets its hook note and the trail", func(t *testing.T) {
		sess := &session.Session{}
		if !recordSilentTurn(sess, session.AgentLocal, "p", TurnResult{Trail: spawnTrail()}, true) {
			t.Fatal("expected a turn to be recorded")
		}
		got := sess.History[0]
		if got.Role != session.RoleAssistant || len(got.Trail) != 1 || !strings.Contains(got.Content, `Spawned background agent "research"`) {
			t.Errorf("unexpected turn: %+v", got)
		}
	})
	t.Run("tools without a hook are summarized generically", func(t *testing.T) {
		sess := &session.Session{}
		recordSilentTurn(sess, session.AgentLocal, "p", TurnResult{Trail: readTrail()}, true)
		if len(sess.History) != 1 || !strings.Contains(sess.History[0].Content, "read_file ×2") {
			t.Errorf("unexpected history: %+v", sess.History)
		}
	})
	t.Run("a turn with closing text never gets a second assistant turn", func(t *testing.T) {
		sess := &session.Session{}
		if recordSilentTurn(sess, session.AgentLocal, "p", TurnResult{Text: "done", Trail: spawnTrail()}, true) || len(sess.History) != 0 {
			t.Errorf("must not add a turn, got %+v", sess.History)
		}
	})
	t.Run("self-escalation is left to the escalation turn", func(t *testing.T) {
		sess := &session.Session{}
		if recordSilentTurn(sess, session.AgentLocal, "p", TurnResult{EscalationReason: "why", Trail: readTrail()}, true) {
			t.Error("must not record a turn for self-escalation")
		}
	})
	t.Run("no activity records nothing", func(t *testing.T) {
		if recordSilentTurn(&session.Session{}, session.AgentLocal, "p", TurnResult{}, true) {
			t.Error("empty turn must not be recorded")
		}
	})
}

// interruptRunner fails with err but still reports the trail the turn built up.
type interruptRunner struct {
	*flakyExecRunner
	trail []session.TrailStep
	err   error
}

func (r *interruptRunner) Execute(_ context.Context, _ config.Config, _ *session.Session, _ *memory.Store, _ AgentRole,
	_ escalation.ContextMode, _, _ string, _ []string, _ bool, _ string, _ TurnCallbacks, _ io.Writer) (TurnResult, error) {
	return TurnResult{Trail: r.trail}, r.err
}

func TestRunPrimary_InterruptedTurn(t *testing.T) {
	cfg := config.Config{Agents: []config.AgentConfig{{Name: "p", Provider: "local"}}}
	run := func(trail []session.TrailStep, err error) *session.Session {
		sess := &session.Session{ID: "s"}
		r := &interruptRunner{flakyExecRunner: &flakyExecRunner{name: "p"}, trail: trail, err: err}
		var out bytes.Buffer
		if got := runPrimaryWithSession(context.Background(), cfg, sess, r, nil, nil, "do it", "do it", &out, nil, nil, nil, nil); !errors.Is(got, err) {
			t.Fatalf("want the runner's error back, got %v", got)
		}
		return sess
	}

	t.Run("lasting action is recorded", func(t *testing.T) {
		sess := run(spawnTrail(), context.Canceled)
		if len(sess.History) != 2 || sess.History[0].Role != session.RoleUser || sess.History[1].Role != session.RoleAssistant {
			t.Fatalf("want [user, assistant], got %+v", sess.History)
		}
		if c := sess.History[1].Content; !strings.Contains(c, "interrupted by the user") || !strings.Contains(c, "research") {
			t.Errorf("unexpected note: %q", c)
		}
	})
	t.Run("no lasting action leaves history untouched so a retry is clean", func(t *testing.T) {
		if sess := run(readTrail(), errors.New("boom")); len(sess.History) != 0 {
			t.Errorf("want empty history, got %+v", sess.History)
		}
	})
}

func TestRunPrimary_NoticesReachPromptAndHistoryOnce(t *testing.T) {
	cfg := config.Config{Agents: []config.AgentConfig{{Name: "p", Provider: "local"}}}
	sess := &session.Session{ID: "s"}
	sess.AddNotice(`Workflow "swarm" completed.`)
	var out bytes.Buffer

	r := &flakyExecRunner{name: "p", res: TurnResult{Text: "ok"}}
	if err := runPrimaryWithSession(context.Background(), cfg, sess, r, nil, nil, "next", "next", &out, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if c := sess.History[0].Content; !strings.HasPrefix(c, "[milk] Workflow \"swarm\" completed.") || !strings.HasSuffix(c, "next") {
		t.Errorf("history user turn should carry the notice then the prompt, got %q", c)
	}
	if len(sess.PendingNotices) != 0 {
		t.Errorf("notice must be consumed, got %v", sess.PendingNotices)
	}

	if err := runPrimaryWithSession(context.Background(), cfg, sess, r, nil, nil, "again", "again", &out, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sess.History[2].Content, "[milk]") {
		t.Errorf("notice must not repeat on later turns, got %q", sess.History[2].Content)
	}
}

func TestRunPrimary_NoticesKeptWhenTurnFails(t *testing.T) {
	cfg := config.Config{Agents: []config.AgentConfig{{Name: "p", Provider: "local"}}}
	sess := &session.Session{ID: "s"}
	sess.AddNotice("event")
	r := &flakyExecRunner{name: "p", errs: []error{errors.New("down")}}
	var out bytes.Buffer
	_ = runPrimaryWithSession(context.Background(), cfg, sess, r, nil, nil, "hi", "hi", &out, nil, nil, nil, nil)
	if len(sess.PendingNotices) != 1 {
		t.Errorf("a failed turn must keep the notice for the retry, got %v", sess.PendingNotices)
	}
}

func TestWorkflowCompletionNotice(t *testing.T) {
	cp := &interp.Checkpoint{Trace: []interp.TraceEntry{
		{StageID: "designer", Value: "the plan"},
		{StageID: "final_evaluation", Value: "NO_ISSUES", Outcome: "good_to_go"},
	}}
	got := workflowCompletionNotice("swarm", "build a page", nil, cp)
	for _, want := range []string{`"swarm" completed`, "build a page", "designer, final_evaluation (good_to_go)", "NO_ISSUES"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice missing %q: %s", want, got)
		}
	}
	if got := workflowCompletionNotice("swarm", "t", context.Canceled, nil); !strings.Contains(got, "cancelled") {
		t.Errorf("cancel not reported: %s", got)
	}
	if got := workflowCompletionNotice("swarm", "t", errors.New("stage x exploded"), nil); !strings.Contains(got, "stage x exploded") {
		t.Errorf("error not reported: %s", got)
	}
}

func TestWorkflowStatusLabel(t *testing.T) {
	cases := []struct {
		st   *workflow.State
		want string
	}{
		{nil, "workflow running"},
		{&workflow.State{WorkflowName: "swarm", Role: "starting"}, "workflow swarm running"},
		{&workflow.State{WorkflowName: "swarm", Role: "implementer"}, "workflow swarm running · implementer"},
	}
	for _, c := range cases {
		if got := workflowStatusLabel(c.st); got != c.want {
			t.Errorf("workflowStatusLabel(%+v) = %q, want %q", c.st, got, c.want)
		}
	}
}

func TestWorkflowDisplaySend_RoutesAwayFromMainTranscript(t *testing.T) {
	var got []tea.Msg
	send := workflowDisplaySend(func(msg tea.Msg) { got = append(got, msg) })

	send(thinkChunkMsg{text: "hmm"})
	send(chunkMsg{text: "⚙ Bash"})
	send(retractMsg{from: "a", to: "b"})
	send(reasoningPromotedMsg{})
	send(toolUseMsg{name: "x"})

	if len(got) != 3 {
		t.Fatalf("want 3 forwarded messages (retract/promotion dropped), got %d: %+v", len(got), got)
	}
	if v, ok := got[0].(workflowThinkChunkMsg); !ok || v.text != "hmm" {
		t.Errorf("reasoning should become a workflow think chunk, got %+v", got[0])
	}
	if v, ok := got[1].(workflow.WorkflowChunkMsg); !ok || v.Text != "⚙ Bash" {
		t.Errorf("tool hint should become a workflow chunk, got %+v", got[1])
	}
	if _, ok := got[2].(toolUseMsg); !ok {
		t.Errorf("other messages must pass through, got %+v", got[2])
	}
}

func TestWorkflowThinkChunk_StaysOutOfMainTranscript(t *testing.T) {
	for _, show := range []bool{true, false} {
		m := testModel()
		m.showThinking = show
		m.currentTurnThinking = &strings.Builder{}
		m.workflowState = &workflow.State{WorkflowName: "swarm"}
		newM, _ := m.Update(workflowThinkChunkMsg{text: "pondering"})
		nm := newM.(model)

		if nm.transcript.Len() != 0 || nm.transcriptNoThink.Len() != 0 || nm.currentTurnThinking.Len() != 0 {
			t.Errorf("showThinking=%v: workflow reasoning leaked into the main transcript", show)
		}
		if live := nm.workflowState.LiveBuffer().Snapshot(); show != strings.Contains(live, "pondering") {
			t.Errorf("showThinking=%v: live buffer = %q", show, live)
		}
		if nm.lastWorkflowActivity.IsZero() {
			t.Error("reasoning must count as workflow activity for the idle watchdog")
		}
	}
}
