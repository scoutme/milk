package local

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/session"
)

func TestDispatchOneTool_SpillsOverCapOutputToFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tool-output")
	a := (&Agent{}).WithMemConfig(MemConfig{ToolResultMaxBytes: 4096, ToolOutputDir: dir})
	tc := toolCall{ID: "call/../weird id", Function: toolCallFunction{
		Name:      "bash",
		Arguments: `{"command":"head -c 20000 /dev/zero | tr '\\0' 'a'"}`,
	}}
	out := a.dispatchOneTool(context.Background(), tc, 0, "", "", io.Discard, &session.Session{}, nil, nil)

	var r toolResult
	if err := json.Unmarshal([]byte(out.msg.Content), &r); err != nil {
		t.Fatalf("result must stay valid toolResult JSON: %v", err)
	}
	if len(r.Output) > 4096 {
		t.Errorf("in-context output must stay capped, got %d", len(r.Output))
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.txt"))
	if len(files) != 1 {
		t.Fatalf("want one spill file, got %v", files)
	}
	if !strings.Contains(r.Output, files[0]) || !strings.Contains(r.Output, "read_file with offset/limit") {
		t.Errorf("marker must name the file and how to read it: %.400s", r.Output)
	}
	got, _ := os.ReadFile(files[0])
	if len(got) != 20000 {
		t.Errorf("spill file must hold the FULL output (20000 bytes), got %d", len(got))
	}
	if info, _ := os.Stat(files[0]); info.Mode().Perm() != 0o600 {
		t.Errorf("spill file must be private, mode %v", info.Mode().Perm())
	}
	if strings.Contains(filepath.Base(files[0]), "..") || strings.ContainsAny(filepath.Base(files[0]), " /") {
		t.Errorf("call id must not leak into the path unsanitised: %s", files[0])
	}
}

func TestSpillToolOutput_Skips(t *testing.T) {
	dir := t.TempDir()
	big := toolResult{Output: strings.Repeat("x", 9000)}.String()
	for name, got := range map[string]string{
		"read_file":   spillToolOutput(dir, "read_file", "1", big, 4096),
		"under cap":   spillToolOutput(dir, "bash", "1", toolResult{Output: "small"}.String(), 4096),
		"no dir":      spillToolOutput("", "bash", "1", big, 4096),
		"no cap":      spillToolOutput(dir, "bash", "1", big, 0),
		"not json":    spillToolOutput(dir, "bash", "1", "plain text "+strings.Repeat("x", 9000), 4096),
		"error shape": spillToolOutput(dir, "bash", "1", toolResult{Error: "boom"}.String(), 4096),
	} {
		if got != "" {
			t.Errorf("%s: must not spill, got %s", name, got)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("nothing should have been written, found %d entries", len(entries))
	}
}

func TestSweepSpillDir_RemovesOnlyExpiredSpillFiles(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.txt")
	fresh := filepath.Join(dir, "fresh.txt")
	other := filepath.Join(dir, "keep.md")
	for _, p := range []string{old, fresh, other} {
		os.WriteFile(p, []byte("x"), 0o600) //nolint:errcheck
	}
	past := time.Now().Add(-spillRetention - time.Hour)
	os.Chtimes(old, past, past)   //nolint:errcheck
	os.Chtimes(other, past, past) //nolint:errcheck

	sweepSpillDir(dir)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("expired spill file must be removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh spill file must stay")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("files that aren't spill output must never be touched")
	}
}

func TestReadFileAndGrepNeedNoPermission(t *testing.T) {
	// The spill file lives outside the project; the model must be able to read
	// it back without a permission prompt.
	if toolNeedsPermission("read_file") || toolNeedsPermission("grep") {
		t.Fatal("read_file/grep are permission-free; the spill hint relies on that")
	}
}
