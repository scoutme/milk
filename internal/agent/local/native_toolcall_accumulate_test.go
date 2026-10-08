package local

import "testing"

func chunk(idx int, id, name, args string) toolCall {
	return toolCall{Index: idx, ID: id, Function: toolCallFunction{Name: name, Arguments: args}}
}

func accumulateAll(chunks ...toolCall) []toolCall {
	partial := map[int]*toolCall{}
	for _, c := range chunks {
		accumulateNativeToolCalls([]toolCall{c}, partial)
	}
	return collectNativeToolCalls(partial)
}

// Shape of the 2026-09-13 mimo-v2.5-pro stream: call A is announced, loses its
// closing brace, then call B reuses index 0 with a new id.
func TestAccumulate_IndexReusedAfterDroppedBrace_SplitsAndRepairs(t *testing.T) {
	got := accumulateAll(
		chunk(0, "call_a", "search_signals", ""),
		chunk(0, "", "", `{"pattern": "denied", `),
		chunk(0, "", "", `"signals": `),
		chunk(0, "", "", `["logs"]`),
		chunk(0, "call_b", "search_signals", ""),
		chunk(0, "", "", `{"pattern": "permission", "signals": ["logs"]}`),
		chunk(1, "call_c", "search_signals", ""),
		chunk(1, "", "", `{"pattern": "timeout", "signals": ["logs"]}`),
	)
	if len(got) != 3 {
		t.Fatalf("want 3 calls, got %d: %+v", len(got), got)
	}
	want := []string{
		`{"pattern": "denied", "signals": ["logs"]}`,
		`{"pattern": "permission", "signals": ["logs"]}`,
		`{"pattern": "timeout", "signals": ["logs"]}`,
	}
	for i, w := range want {
		if got[i].Function.Name != "search_signals" || got[i].Function.Arguments != w {
			t.Errorf("call %d = %s(%s), want search_signals(%s)", i, got[i].Function.Name, got[i].Function.Arguments, w)
		}
	}
}

// Shape of the 2026-10-08 02:52 mimo-v2.6-pro stream: arguments of an
// unannounced call arrive before the header of the real call on the same index.
func TestAccumulate_NamelessOrphanArgsBeforeHeader_AreDropped(t *testing.T) {
	got := accumulateAll(
		chunk(0, "", "", `{"command": `),
		chunk(0, "", "", `"git status && git diff --stat HEAD`),
		chunk(0, "", "", `"`),
		chunk(0, "call_b", "bash", ""),
		chunk(0, "", "", `{"command": "ls -la"}`),
		chunk(1, "call_c", "get_session_context", ""),
		chunk(1, "", "", `{"last_n": 8}`),
	)
	if len(got) != 2 {
		t.Fatalf("want 2 calls, got %d: %+v", len(got), got)
	}
	if got[0].Function.Name != "bash" || got[0].Function.Arguments != `{"command": "ls -la"}` {
		t.Errorf("call 0 = %s(%s)", got[0].Function.Name, got[0].Function.Arguments)
	}
	if got[1].Function.Name != "get_session_context" {
		t.Errorf("call 1 = %s", got[1].Function.Name)
	}
}

// 2026-10-08 02:09 / 10-06 shape: nothing is ever announced, so nothing runs.
func TestAccumulate_NeverAnnounced_YieldsNoCalls(t *testing.T) {
	got := accumulateAll(
		chunk(0, "", "", `{"goal": "x"`),
		chunk(0, "", "", `{"command": "y"`),
	)
	if len(got) != 0 {
		t.Fatalf("want no calls, got %+v", got)
	}
}

func TestAccumulate_ConcatenatedCompleteObjects_KeepFirst(t *testing.T) {
	got := accumulateAll(
		chunk(0, "call_a", "bash", `{"command": "ls"}`),
		chunk(0, "", "", `{"command": "garbage"`),
	)
	if len(got) != 1 || got[0].Function.Arguments != `{"command": "ls"}` {
		t.Fatalf("got %+v", got)
	}
}

// Well-behaved servers must be untouched: same id repeated on every chunk,
// names streamed in pieces, parallel calls on distinct indices, no-arg calls.
func TestAccumulate_WellFormedStreamsUnchanged(t *testing.T) {
	got := accumulateAll(
		chunk(0, "id1", "ba", `{"command"`),
		chunk(0, "id1", "sh", `: "ls"}`),
		chunk(1, "id2", "list_tasks", ""),
	)
	if len(got) != 2 {
		t.Fatalf("want 2 calls, got %+v", got)
	}
	if got[0].Function.Name != "bash" || got[0].Function.Arguments != `{"command": "ls"}` {
		t.Errorf("call 0 = %+v", got[0])
	}
	if got[1].Function.Name != "list_tasks" || got[1].Function.Arguments != "" {
		t.Errorf("call 1 = %+v", got[1])
	}
}

// A cut stream must not have its truncated arguments "repaired" into a
// runnable call: brace repair is only for a call the server visibly moved past.
func TestAccumulate_TruncatedFinalCall_NotRepaired(t *testing.T) {
	got := accumulateAll(chunk(0, "id1", "write_file", `{"path": "a.go", "content": "abc"`))
	if len(got) != 1 || got[0].Function.Arguments != `{"path": "a.go", "content": "abc"` {
		t.Fatalf("got %+v", got)
	}
}
