package mockrecorder

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRecorderWriteTurn(t *testing.T) {
	dir := t.TempDir()
	rec, err := New("test", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec.SetSessionID("sess-abc")

	if err := rec.WriteTurn("hello", "ctx", nil, "hi there"); err != nil {
		t.Fatalf("WriteTurn: %v", err)
	}
	if err := rec.WriteTurn("world", "ctx2", []ToolCallRecord{{Name: "bash", Args: map[string]any{"raw": "echo hi"}}}, "ok"); err != nil {
		t.Fatalf("WriteTurn 2: %v", err)
	}
	if err := rec.WriteMetrics(100); err != nil {
		t.Fatalf("WriteMetrics: %v", err)
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify sessions file.
	sessPath := filepath.Join(dir, "test-sessions.jsonl")
	f, err := os.Open(sessPath)
	if err != nil {
		t.Fatalf("open sessions file: %v", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	var turns []TurnRecord
	for sc.Scan() {
		var tr TurnRecord
		if err := json.Unmarshal(sc.Bytes(), &tr); err != nil {
			t.Fatalf("unmarshal TurnRecord: %v", err)
		}
		turns = append(turns, tr)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("want 2 turns, got %d", len(turns))
	}
	if turns[0].Turn != 1 || turns[0].Input != "hello" || turns[0].Response != "hi there" {
		t.Errorf("turn 1 mismatch: %+v", turns[0])
	}
	if turns[1].Turn != 2 || len(turns[1].ToolCalls) != 1 {
		t.Errorf("turn 2 mismatch: %+v", turns[1])
	}

	// Verify metrics file.
	mpath := filepath.Join(dir, "test-metrics.jsonl")
	data, err := os.ReadFile(mpath)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	var met MetricsRecord
	if err := json.Unmarshal(data[:len(data)-1], &met); err != nil {
		t.Fatalf("unmarshal metrics: %v", err)
	}
	if met.SessionID != "sess-abc" || met.Turns != 2 {
		t.Errorf("metrics mismatch: %+v", met)
	}
}

func TestParseToolCalls(t *testing.T) {
	tests := []struct {
		input string
		want  int
		name  string
	}{
		{"[tool:bash,echo hi]", 1, "bash"},
		{"no tool here", 0, ""},
		{"two [tool:bash,echo] and [tool:read,/tmp/f]", 2, "bash"},
		{"[tool:bash_exec,{\"cmd\":\"ls\"}]", 1, "bash_exec"},
	}
	for _, tt := range tests {
		tc := ParseToolCalls(tt.input)
		if len(tc) != tt.want {
			t.Errorf("%q: got %d calls, want %d", tt.input, len(tc), tt.want)
		}
		if tt.want > 0 && tc[0].Name != tt.name {
			t.Errorf("%q: first tool name = %q, want %q", tt.input, tc[0].Name, tt.name)
		}
	}
}

func TestHasToolCall(t *testing.T) {
	if !HasToolCall("[tool:bash,echo hi]") {
		t.Error("expected true")
	}
	if HasToolCall("no tools") {
		t.Error("expected false")
	}
}
