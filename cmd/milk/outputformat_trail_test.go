package main

import (
	"encoding/json"
	"testing"

	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/transport/streamjson"
)

func TestTrailToolInput(t *testing.T) {
	if got := string(trailToolInput("")); got != "{}" {
		t.Errorf(`trailToolInput("") = %s, want {}`, got)
	}
	if got := string(trailToolInput(`{"command":"ls"}`)); got != `{"command":"ls"}` {
		t.Errorf("valid JSON should pass through unchanged, got %s", got)
	}
	// Non-JSON arguments (plain-text tool args are cut as plain text at
	// capture) must come out as a JSON string so the stream stays parseable.
	got := string(trailToolInput("plain text args"))
	var s string
	if err := json.Unmarshal([]byte(got), &s); err != nil || s != "plain text args" {
		t.Errorf("trailToolInput(non-JSON) = %s, want a JSON string of the input", got)
	}
}

func TestTrailToolResult_Cleared(t *testing.T) {
	if r := trailToolResult("anything", true); r != nil {
		t.Errorf("cleared result should emit no payload, got %s", r)
	}
	r := trailToolResult("42 files", false)
	var s string
	if err := json.Unmarshal(r, &s); err != nil || s != "42 files" {
		t.Errorf("trailToolResult = %s, want JSON string %q", r, "42 files")
	}
}

func TestBuildToolTrailEvents_OneStep(t *testing.T) {
	trail := []session.TrailStep{{
		Text: "let me look",
		Calls: []session.TrailCall{
			{ID: "t1", Name: "bash", Args: `{"command":"ls"}`, Result: "a\nb"},
			{Name: "read_file", Args: "not json", Cleared: true},
		},
	}}
	events := buildToolTrailEvents("sess_1", trail)
	if len(events) != 2 {
		t.Fatalf("want 2 events (assistant + user), got %d", len(events))
	}

	asst, err := events[0].AsMessage()
	if err != nil || asst == nil {
		t.Fatalf("assistant event message: %v", err)
	}
	if len(asst.Content) != 3 {
		t.Fatalf("want text + 2 tool_use blocks, got %d", len(asst.Content))
	}
	if asst.Content[0].Type != "text" || asst.Content[0].Text != "let me look" {
		t.Errorf("first block = %+v, want narration text", asst.Content[0])
	}
	if asst.Content[1].Type != "tool_use" || asst.Content[1].Name != "bash" || asst.Content[1].ID != "t1" {
		t.Errorf("second block = %+v, want tool_use bash/t1", asst.Content[1])
	}
	if string(asst.Content[1].Input) != `{"command":"ls"}` {
		t.Errorf("tool_use input = %s", asst.Content[1].Input)
	}
	synthID := asst.Content[2].ID
	if asst.Content[2].Type != "tool_use" || asst.Content[2].Name != "read_file" || synthID == "" {
		t.Errorf("third block = %+v, want tool_use read_file with synthetic id", asst.Content[2])
	}

	user, err := events[1].AsMessage()
	if err != nil || user == nil {
		t.Fatalf("user event message: %v", err)
	}
	if len(user.Content) != 2 {
		t.Fatalf("want 2 tool_result blocks, got %d", len(user.Content))
	}
	if user.Content[0].Type != "tool_result" || user.Content[0].ToolUseID != "t1" {
		t.Errorf("first result block = %+v, want tool_result for t1", user.Content[0])
	}
	var res string
	if err := json.Unmarshal(user.Content[0].Content, &res); err != nil || res != "a\nb" {
		t.Errorf("result content = %s, want JSON string of the result", user.Content[0].Content)
	}
	if user.Content[1].ToolUseID != synthID {
		t.Errorf("second result block id = %s, want synthetic id %s", user.Content[1].ToolUseID, synthID)
	}
	if user.Content[1].Content != nil {
		t.Errorf("cleared result should have absent content, got %s", user.Content[1].Content)
	}
}

func TestBuildToolTrailEvents_MultiStepOrder(t *testing.T) {
	trail := []session.TrailStep{
		{Calls: []session.TrailCall{{ID: "a1", Name: "read_file"}}},
		{Calls: []session.TrailCall{{ID: "b1", Name: "edit_file"}}},
		{}, // step without calls — skipped entirely
	}
	events := buildToolTrailEvents("s", trail)
	if len(events) != 4 {
		t.Fatalf("want 4 events, got %d", len(events))
	}
	want := []struct {
		typ   string
		first string
	}{
		{streamjson.TypeAssistant, "a1"},
		{streamjson.TypeUser, "a1"},
		{streamjson.TypeAssistant, "b1"},
		{streamjson.TypeUser, "b1"},
	}
	for i, w := range want {
		if events[i].Type != w.typ {
			t.Errorf("event %d type = %s, want %s", i, events[i].Type, w.typ)
		}
		msg, _ := events[i].AsMessage()
		if msg == nil || len(msg.Content) == 0 {
			t.Fatalf("event %d has no content", i)
		}
		id := msg.Content[0].ID
		if id == "" {
			id = msg.Content[0].ToolUseID
		}
		if id != w.first {
			t.Errorf("event %d paired id = %s, want %s", i, id, w.first)
		}
	}
}

func TestNewTurnTrails(t *testing.T) {
	sess := &session.Session{History: []session.Turn{
		{Role: session.RoleUser, Content: "old"},
		{Role: session.RoleAssistant, Content: "old answer", Trail: []session.TrailStep{{Calls: []session.TrailCall{{Name: "stale"}}}}},
		{Role: session.RoleUser, Content: "new"},
		{Role: session.RoleAssistant, Content: "new answer", Trail: []session.TrailStep{{Calls: []session.TrailCall{{Name: "fresh"}}}}},
	}}
	trail := newTurnTrails(sess, 2)
	if len(trail) != 1 || trail[0].Calls[0].Name != "fresh" {
		t.Errorf("newTurnTrails = %+v, want only the post-start trail", trail)
	}
	if got := newTurnTrails(sess, 99); got != nil {
		t.Errorf("start beyond history should yield nil, got %+v", got)
	}
	if got := newTurnTrails(nil, 0); got != nil {
		t.Errorf("nil session should yield nil, got %+v", got)
	}
}
