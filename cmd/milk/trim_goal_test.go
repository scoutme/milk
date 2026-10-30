package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/session"
)

// goalSwitchMsgs is a transcript with an orientation message, an old goal's
// two exchanges and a new goal's two exchanges (boundary 5, lead 1):
//
//	0 ORIENT | 1 OLD-ASK 2 old-reply 3 old2 4 old2-reply | 5 NEW-ASK 6 plan 7 step 8 ok
func goalSwitchMsgs() []local.Message {
	p := func(label string, n int) string { return label + strings.Repeat(".", n-len(label)) }
	return msgs(
		"system", p("ORIENT", 10),
		"user", p("OLD-ASK", 100),
		"assistant", p("old-reply", 100),
		"user", p("old2", 100),
		"assistant", p("old2-reply", 100),
		"user", p("NEW-ASK", 50),
		"assistant", p("plan", 50),
		"user", p("step", 50),
		"assistant", p("ok", 50),
	)
}

var goalHint = trimHint{lead: 1, boundary: 5}

func TestTrimPlan_NoHintKeepsHeadPin(t *testing.T) {
	// The behavior #362 is about: plain head retention pins the stale ask.
	got, trimmed := trimLocalMessagesHint(goalSwitchMsgs(), 450, trimHint{})
	if !trimmed || !strings.Contains(joined(got), "OLD-ASK") {
		t.Fatalf("baseline: expected the first request pinned in the head; trimmed=%v\n%s", trimmed, joined(got))
	}
}

func TestTrimPlan_DropsSupersededAskFirst(t *testing.T) {
	got, trimmed := trimLocalMessagesHint(goalSwitchMsgs(), 450, goalHint)
	if !trimmed {
		t.Fatal("expected trimming")
	}
	all := joined(got)
	if strings.Contains(all, "OLD-ASK") {
		t.Errorf("the superseded ask must go before anything newer; got:\n%s", all)
	}
	for _, keep := range []string{"ORIENT", "old2", "NEW-ASK", "plan", "step", "ok"} {
		if !strings.Contains(all, keep) {
			t.Errorf("%q should survive (oldest-first drop frees enough); got:\n%s", keep, all)
		}
	}
	if got[0].Content[:6] != "ORIENT" {
		t.Errorf("orientation must stay first, got %q", got[0].Content)
	}
	if !strings.Contains(got[1].Content, "superseded by a change of goal") {
		t.Errorf("marker should say why, got %q", got[1].Content)
	}
}

func TestTrimPlan_OversizedGoalKeepsSwitchTurnPinned(t *testing.T) {
	// Post-switch content alone exceeds the budget: every pre-switch message
	// goes, and the switch turn itself stays pinned (the old failure mode lost it).
	got, trimmed := trimLocalMessagesHint(goalSwitchMsgs(), 200, goalHint)
	if !trimmed {
		t.Fatal("expected trimming")
	}
	all := joined(got)
	if strings.Contains(all, "OLD-ASK") || strings.Contains(all, "old2") {
		t.Errorf("pre-switch content must be gone; got:\n%s", all)
	}
	for _, keep := range []string{"ORIENT", "NEW-ASK", "step", "ok"} {
		if !strings.Contains(all, keep) {
			t.Errorf("%q should survive; got:\n%s", keep, all)
		}
	}
}

func TestTrimPlan_WithinBudgetAndInvalidBoundaryAreUnchanged(t *testing.T) {
	in := goalSwitchMsgs()
	if got, trimmed := trimLocalMessagesHint(in, 10_000, goalHint); trimmed || len(got) != len(in) {
		t.Error("within budget: no-op expected")
	}
	for _, h := range []trimHint{{}, {lead: 1, boundary: 1}, {lead: 1, boundary: 0}, {lead: 1, boundary: 99}} {
		want, _ := trimLocalMessages(in, 450)
		got, _ := trimLocalMessagesHint(in, 450, h)
		if joined(got) != joined(want) {
			t.Errorf("hint %+v without a usable boundary must equal plain trimming", h)
		}
	}
}

func TestTrimPlan_CompactionSummarizesBothSpansOnce(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"condensed"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	agent := local.New(srv.URL, "test-model")

	got, trimmed := trimLocalMessagesWithCompactionHint(context.Background(), agent, &session.Session{}, goalSwitchMsgs(), 200, goalHint)
	if !trimmed || calls != 1 {
		t.Fatalf("trimmed=%v, summarize calls=%d (want one call for both spans)", trimmed, calls)
	}
	n := 0
	for _, m := range got {
		if strings.Contains(m.Content, "condensed") {
			n++
			if !strings.Contains(m.Content, "superseded") {
				t.Errorf("summary should flag the superseded requests, got %q", m.Content)
			}
		}
	}
	if n != 1 {
		t.Errorf("want exactly one summary message, got %d in:\n%s", n, joined(got))
	}
	if all := joined(got); !strings.Contains(all, "NEW-ASK") || strings.Contains(all, "OLD-ASK") {
		t.Errorf("switch turn kept, old ask summarized away; got:\n%s", all)
	}
}

func TestBuildAgentHistoryWithBoundary(t *testing.T) {
	turn := func(r session.Role, c string) session.Turn {
		return session.Turn{Role: r, Agent: session.AgentLocal, Content: c}
	}
	sess := &session.Session{History: []session.Turn{
		turn(session.RoleUser, "old ask"), turn(session.RoleAssistant, "old reply"),
		turn(session.RoleUser, "old2"), turn(session.RoleAssistant, "old2 reply"),
		turn(session.RoleUser, "switch"), turn(session.RoleAssistant, "ok"),
	}}
	sess.CurrentNeed = "new goal"

	sess.CurrentNeedSetAt = 5 // recorded while the switch turn (index 4) executed
	msgs, at := buildAgentHistoryWithBoundary(sess, 0, session.AgentLocal, "p", false)
	if at != 4 || !strings.Contains(msgs[at].Content, "switch") {
		t.Errorf("boundary = %d (%q), want the switch user message at 4", at, msgs[min(at, len(msgs)-1)].Content)
	}

	for _, setAt := range []int{0, 1} { // unset / need declared on the very first turn
		sess.CurrentNeedSetAt = setAt
		if _, at := buildAgentHistoryWithBoundary(sess, 0, session.AgentLocal, "p", false); at != -1 {
			t.Errorf("CurrentNeedSetAt=%d: boundary = %d, want -1", setAt, at)
		}
	}

	sess.CurrentNeedSetAt = 5
	sess.CurrentNeed = ""
	if _, at := buildAgentHistoryWithBoundary(sess, 0, session.AgentLocal, "p", false); at != -1 {
		t.Errorf("no need: boundary = %d, want -1", at)
	}
}
