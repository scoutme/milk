package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testSession() *Session {
	s := &Session{
		ID:        "test-session-id-1234",
		Name:      "test",
		CWD:       "/home/user/project",
		CreatedAt: time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC),
		LastUsed:  time.Date(2026, 5, 15, 10, 5, 0, 0, time.UTC),
		State:     StateRouting,
	}
	s.History = []Turn{
		{Role: RoleUser, Agent: AgentLocal, Content: "hello", Timestamp: time.Date(2026, 5, 15, 10, 1, 0, 0, time.UTC)},
		{Role: RoleAssistant, Agent: AgentLocal, Content: "world", Timestamp: time.Date(2026, 5, 15, 10, 2, 0, 0, time.UTC)},
	}
	return s
}

func TestExportText_ContainsMetadata(t *testing.T) {
	s := testSession()
	out := ExportText(s)

	for _, want := range []string{"test-session-id-1234", "/home/user/project", "ROUTING"} {
		if !strings.Contains(out, want) {
			t.Errorf("ExportText missing %q", want)
		}
	}
}

func TestExportText_ContainsHistory(t *testing.T) {
	s := testSession()
	out := ExportText(s)

	if !strings.Contains(out, "hello") {
		t.Error("ExportText missing user turn content")
	}
	if !strings.Contains(out, "world") {
		t.Error("ExportText missing assistant turn content")
	}
}

func TestExportText_ToolCallsFormatted(t *testing.T) {
	s := testSession()
	s.History = append(s.History, Turn{
		Role:  RoleAssistant,
		Agent: AgentLocal,
		ToolCalls: []ToolCall{
			{ID: "tc1", Name: "bash", Arguments: `{"command":"ls"}`},
		},
		Timestamp: time.Now(),
	})
	out := ExportText(s)
	if !strings.Contains(out, "bash") {
		t.Error("ExportText should include tool call name")
	}
}

func TestExportText_ToolResultTruncated(t *testing.T) {
	s := testSession()
	long := strings.Repeat("x", 400)
	s.History = append(s.History, Turn{
		Role:      RoleToolResult,
		Content:   long,
		Timestamp: time.Now(),
	})
	out := ExportText(s)
	if strings.Contains(out, long) {
		t.Error("ExportText should truncate long tool results")
	}
	if !strings.Contains(out, "...") {
		t.Error("ExportText should show truncation indicator")
	}
}

func TestExportText_NoANSI(t *testing.T) {
	s := testSession()
	out := ExportText(s)
	if strings.Contains(out, "\033[") {
		t.Error("ExportText should not contain ANSI escape codes")
	}
}

func TestExportTextColorized_ContainsANSI(t *testing.T) {
	s := testSession()
	out := ExportTextColorized(s)
	if !strings.Contains(out, "\033[") {
		t.Error("ExportTextColorized should contain ANSI escape codes")
	}
	// Body content must still be present.
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Error("ExportTextColorized should include turn content")
	}
}

func TestExportJSON_ValidJSON(t *testing.T) {
	s := testSession()
	data, err := ExportJSON(s)
	if err != nil {
		t.Fatalf("ExportJSON: %v", err)
	}
	var roundtrip Session
	if err := json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatalf("ExportJSON produced invalid JSON: %v", err)
	}
	if roundtrip.ID != s.ID {
		t.Errorf("ID mismatch after round-trip: got %q", roundtrip.ID)
	}
	if len(roundtrip.History) != len(s.History) {
		t.Errorf("history length mismatch: got %d, want %d", len(roundtrip.History), len(s.History))
	}
}
