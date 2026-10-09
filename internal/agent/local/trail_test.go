package local

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

func tc(id, name, args string) toolCall {
	return toolCall{ID: id, Type: "function", Function: toolCallFunction{Name: name, Arguments: args}}
}

func TestTrailFromMessages_PairsResultsAndSkipsNoise(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "earlier turn"},
		{Role: "assistant", Content: "earlier answer"},
		{Role: "user", Content: "this turn"},
		{Role: "assistant", Content: "looking", ToolCalls: []toolCall{tc("a", "read_file", `{"path":"x.go"}`), tc("b", "bash", `{"command":"ls"}`)}},
		{Role: "tool", ToolCallID: "b", Content: `{"output":"files"}`},
		{Role: "tool", ToolCallID: "a", Content: `{"output":"package x"}`},
		{Role: "user", Content: "<system-reminder>nudge</system-reminder>"},
		{Role: "assistant", Content: "final answer"},
	}
	steps := TrailFromMessages(msgs, 3)
	if len(steps) != 1 || len(steps[0].Calls) != 2 {
		t.Fatalf("want one step with two calls, got %+v", steps)
	}
	if steps[0].Text != "looking" || steps[0].Calls[0].Result != `{"output":"package x"}` || steps[0].Calls[1].Result != `{"output":"files"}` {
		t.Errorf("results must pair by call id, not position: %+v", steps[0])
	}
	if TrailFromMessages(msgs, 99) != nil {
		t.Error("bad index must yield no trail")
	}
}

func TestTrailFromMessages_BoundsStoredSize(t *testing.T) {
	msgs := []Message{{Role: "user", Content: "go"}}
	big := toolResult{Output: strings.Repeat("y", 50000)}.String()
	for i := 0; i < 80; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			Message{Role: "assistant", ToolCalls: []toolCall{tc(id, "bash", `{"command":"cat big"}`)}},
			Message{Role: "tool", ToolCallID: id, Content: big})
	}
	steps := TrailFromMessages(msgs, 0)
	total, cleared := 0, 0
	for _, s := range steps {
		for _, c := range s.Calls {
			total += len(c.Args) + len(c.Result)
			if c.Cleared {
				cleared++
				if c.Result != "" {
					t.Error("a cleared call must not keep its result")
				}
			}
		}
	}
	if total > trailStoreBudget+trailResultMaxBytes*2 {
		t.Errorf("stored trail %d bytes exceeds the budget", total)
	}
	if cleared == 0 || steps[len(steps)-1].Calls[0].Cleared {
		t.Errorf("oldest calls are cleared first, newest keeps its result (cleared=%d)", cleared)
	}
}

func TestCompactArgs_StaysValidJSON(t *testing.T) {
	in, _ := json.Marshal(map[string]string{"path": "a.go", "new_string": strings.Repeat("z", 5000)})
	out := compactArgs(string(in), 300)
	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("compacted arguments must stay valid JSON: %v (%s)", err, out)
	}
	if m["path"] != "a.go" || len(out) > 600 {
		t.Errorf("path kept, big value cut: len=%d %v", len(out), m)
	}
}

func steps(n int, resultBytes int) []session.TrailStep {
	var out []session.TrailStep
	for i := 0; i < n; i++ {
		out = append(out, session.TrailStep{Calls: []session.TrailCall{{
			ID: fmt.Sprintf("c%d", i), Name: "read_file", Args: fmt.Sprintf(`{"path":"f%d.go"}`, i),
			Result: strings.Repeat("r", resultBytes),
		}}})
	}
	return out
}

func TestReplayTrails_RecentExpandedOlderBecomeManifest(t *testing.T) {
	trails := [][]session.TrailStep{steps(2, 100), nil, steps(2, 100), steps(2, 100)}
	got := ReplayTrails(trails)
	if got[0].Messages != nil || !strings.Contains(got[0].Note, "read_file ×2") || !strings.Contains(got[0].Note, "f0.go (read)") {
		t.Errorf("oldest tool turn must shrink to a manifest, got %+v", got[0])
	}
	if got[1].Messages != nil || got[1].Note != "" {
		t.Errorf("a turn without a trail replays nothing: %+v", got[1])
	}
	for _, i := range []int{2, 3} {
		if len(got[i].Messages) != 4 || got[i].Note != "" {
			t.Fatalf("recent turn %d must expand to 2 call/result pairs, got %d messages", i, len(got[i].Messages))
		}
	}
}

func TestReplayTrails_PairsAreValidAndOldResultsClearedByBudget(t *testing.T) {
	// Two recent turns of 100 KB results each: only the newest fits the
	// 160 KB protect window, so the older turn's results become placeholders.
	trails := [][]session.TrailStep{steps(1, 100<<10), steps(1, 100<<10)}
	got := ReplayTrails(trails)
	if !strings.Contains(got[0].Messages[1].Content, clearedResultPlaceholder) {
		t.Errorf("older result must be cleared, got %.80q", got[0].Messages[1].Content)
	}
	if strings.Contains(got[1].Messages[1].Content, clearedResultPlaceholder) {
		t.Error("newest result must stay verbatim")
	}
	for _, r := range got {
		for i := 0; i+1 < len(r.Messages); i += 2 {
			a, tm := r.Messages[i], r.Messages[i+1]
			if a.Role != "assistant" || tm.Role != "tool" || tm.ToolCallID != a.ToolCalls[0].ID {
				t.Fatalf("tool message must follow its assistant call: %+v / %+v", a, tm)
			}
			if !json.Valid([]byte(a.ToolCalls[0].Function.Arguments)) {
				t.Errorf("replayed arguments must be valid JSON: %q", a.ToolCalls[0].Function.Arguments)
			}
		}
	}
}

func TestRun_CapturesTurnTrail(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&n, 1) == 1 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"unknown-tool","arguments":"{\"path\":\"p.go\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	agent := New(srv.URL, "m")
	var out strings.Builder
	if _, err := agent.Run(context.Background(), nil, "x", &out, &session.Session{}, nil); err != nil {
		t.Fatal(err)
	}
	trail := agent.TurnTrail()
	if len(trail) != 1 || len(trail[0].Calls) != 1 || trail[0].Calls[0].Name != "unknown-tool" || trail[0].Calls[0].Result == "" {
		t.Fatalf("trail not captured: %+v", trail)
	}
}
