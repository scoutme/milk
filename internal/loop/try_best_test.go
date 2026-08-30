package loop

import (
	"strings"
	"testing"
)

func TestTryBestEditRepeat_Triggers(t *testing.T) {
	m := NewTryBestMonitor()
	editArgs := `{"path":"/tmp/foo.go","new_text":"package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}"}`
	// Record 2 prior similar edits (succeeded).
	for i := 0; i < tryBestEditMatches; i++ {
		v := m.RecordToolCall("edit_file", editArgs, "", true)
		if v != nil {
			t.Fatalf("edit %d: unexpected verdict: %+v", i, v)
		}
	}
	// Third similar edit should trigger.
	v := m.RecordToolCall("edit_file", editArgs, "", true)
	if v == nil {
		t.Fatal("expected try-best verdict on 3rd similar edit")
	}
	if v.Signal != TryBestEditRepeat {
		t.Errorf("signal = %v, want TryBestEditRepeat", v.Signal)
	}
	if v.FilePath != "/tmp/foo.go" {
		t.Errorf("FilePath = %q, want /tmp/foo.go", v.FilePath)
	}
}

func TestTryBestEditRepeat_IgnoresDifferentFile(t *testing.T) {
	m := NewTryBestMonitor()
	edit1 := `{"path":"/tmp/a.go","new_text":"package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}"}`
	edit2 := `{"path":"/tmp/b.go","new_text":"package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}"}`
	m.RecordToolCall("edit_file", edit1, "", true)
	m.RecordToolCall("edit_file", edit1, "", true)
	v := m.RecordToolCall("edit_file", edit2, "", true)
	if v != nil {
		t.Errorf("different file should not trigger: %+v", v)
	}
}

func TestTryBestEditRepeat_IgnoresFailedEdits(t *testing.T) {
	m := NewTryBestMonitor()
	editArgs := `{"path":"/tmp/foo.go","new_text":"package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}"}`
	m.RecordToolCall("edit_file", editArgs, "error: file not found", false)
	m.RecordToolCall("edit_file", editArgs, "error: file not found", false)
	v := m.RecordToolCall("edit_file", editArgs, "", true)
	if v != nil {
		t.Errorf("failed edits should not count: %+v", v)
	}
}

func TestTryBestBashRetry_Triggers(t *testing.T) {
	m := NewTryBestMonitor()
	cmd := `{"command":"go test ./..."}`
	for i := 0; i < tryBestBashRetries-1; i++ {
		v := m.RecordToolCall("bash", cmd, "exit status 1", false)
		if v != nil {
			t.Fatalf("retry %d: unexpected verdict: %+v", i, v)
		}
	}
	v := m.RecordToolCall("bash", cmd, "exit status 1", false)
	if v == nil {
		t.Fatal("expected try-best verdict on bash retry")
	}
	if v.Signal != TryBestBashRetry {
		t.Errorf("signal = %v, want TryBestBashRetry", v.Signal)
	}
}

func TestTryBestBashRetry_ResetsOnSuccess(t *testing.T) {
	m := NewTryBestMonitor()
	cmd := `{"command":"go test ./..."}`
	m.RecordToolCall("bash", cmd, "exit status 1", false)
	m.RecordToolCall("bash", cmd, "exit status 1", false)
	// Successful command resets the streak.
	m.RecordToolCall("bash", cmd, "ok", true)
	m.RecordToolCall("bash", cmd, "exit status 1", false)
	m.RecordToolCall("bash", cmd, "exit status 1", false)
	v := m.RecordToolCall("bash", cmd, "exit status 1", false)
	if v == nil {
		t.Fatal("expected try-best verdict after reset + 3 more failures")
	}
}

func TestTryBestBashRetry_ResetsOnDifferentCommand(t *testing.T) {
	m := NewTryBestMonitor()
	cmd1 := `{"command":"go test ./..."}`
	cmd2 := `{"command":"go build ./..."}`
	m.RecordToolCall("bash", cmd1, "exit status 1", false)
	m.RecordToolCall("bash", cmd1, "exit status 1", false)
	// Different command resets the streak.
	m.RecordToolCall("bash", cmd2, "exit status 1", false)
	m.RecordToolCall("bash", cmd2, "exit status 1", false)
	v := m.RecordToolCall("bash", cmd2, "exit status 1", false)
	if v == nil {
		t.Fatal("expected try-best verdict for cmd2 retries")
	}
}

func TestTryBestActionStreak_Triggers(t *testing.T) {
	m := NewTryBestMonitor()
	// Non-progressing edits (different content so edit_repeat doesn't fire,
	// but long enough to exceed tryBestMinDiffLen).
	for i := 0; i < tryBestActionStreak; i++ {
		content := strings.Repeat("x", 30+i) // each edit is different but long enough
		args := `{"path":"/tmp/` + string(rune('a'+i)) + `.go","new_text":"` + content + `"}`
		m.RecordToolCall("edit_file", args, "error: validation failed", false)
	}
	v := m.CheckActionStreak()
	if v == nil {
		t.Fatal("expected action streak verdict")
	}
	if v.Signal != TryBestActionStreak {
		t.Errorf("signal = %v, want TryBestActionStreak", v.Signal)
	}
}

func TestTryBestActionStreak_ResetsOnSuccess(t *testing.T) {
	m := NewTryBestMonitor()
	longContent := strings.Repeat("x", 40)
	for i := 0; i < tryBestActionStreak-1; i++ {
		m.RecordToolCall("edit_file", `{"path":"/tmp/a.go","new_text":"`+longContent+`"}`, "error", false)
	}
	// Successful action resets.
	m.RecordToolCall("edit_file", `{"path":"/tmp/a.go","new_text":"`+longContent+`"}`, "", true)
	v := m.CheckActionStreak()
	if v != nil {
		t.Errorf("success should reset streak, got: %+v", v)
	}
}

func TestTryBestFired_OnlyOnce(t *testing.T) {
	m := NewTryBestMonitor()
	editArgs := `{"path":"/tmp/foo.go","new_text":"package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}"}`
	for i := 0; i < tryBestEditMatches+1; i++ {
		m.RecordToolCall("edit_file", editArgs, "", true)
	}
	// Second trigger should not fire again.
	v := m.RecordToolCall("edit_file", editArgs, "", true)
	if v != nil {
		t.Errorf("should not fire again after first trigger: %+v", v)
	}
}

func TestTryBestReset_ClearsFired(t *testing.T) {
	m := NewTryBestMonitor()
	editArgs := `{"path":"/tmp/foo.go","new_text":"package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}"}`
	for i := 0; i < tryBestEditMatches+1; i++ {
		m.RecordToolCall("edit_file", editArgs, "", true)
	}
	m.Reset()
	// After reset, should fire again.
	for i := 0; i < tryBestEditMatches; i++ {
		m.RecordToolCall("edit_file", editArgs, "", true)
	}
	v := m.RecordToolCall("edit_file", editArgs, "", true)
	if v == nil {
		t.Fatal("expected verdict after reset")
	}
}

func TestJaccardShingles(t *testing.T) {
	tests := []struct {
		a, b string
		n    int
		want float64
	}{
		{"hello", "hello", 3, 1.0},
		{"hello", "world", 3, 0.0},
		{"", "", 3, 1.0},
	}
	for _, tt := range tests {
		got := jaccardShingles(tt.a, tt.b, tt.n)
		if got != tt.want {
			t.Errorf("jaccardShingles(%q, %q, %d) = %f, want %f", tt.a, tt.b, tt.n, got, tt.want)
		}
	}
}

func TestNormaliseBashCommand(t *testing.T) {
	tests := []struct {
		args string
		want string
	}{
		{`{"command":"go test ./..."}`, "go test ./..."},
		{`{"command":"  Go  Test  ./...  "}`, "go test ./..."},
		{`{}`, ""},
	}
	for _, tt := range tests {
		got := normaliseBashCommand(tt.args)
		if got != tt.want {
			t.Errorf("normaliseBashCommand(%q) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

func TestExtractFilePath(t *testing.T) {
	tests := []struct {
		tool string
		args string
		want string
	}{
		{"edit_file", `{"path":"/tmp/foo.go"}`, "/tmp/foo.go"},
		{"write_file", `{"file_path":"/tmp/bar.go"}`, "/tmp/bar.go"},
		{"edit_file", `{}`, ""},
	}
	for _, tt := range tests {
		got := extractFilePath(tt.tool, tt.args)
		if got != tt.want {
			t.Errorf("extractFilePath(%q, %q) = %q, want %q", tt.tool, tt.args, got, tt.want)
		}
	}
}
