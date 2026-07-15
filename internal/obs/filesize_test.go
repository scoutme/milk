package obs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

func TestCheckFileSizesPreciseThresholds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.jsonl")
	if err := os.WriteFile(path, make([]byte, 1024*1024-1), 0o600); err != nil {
		t.Fatal(err)
	}
	warn, exceeded := CheckFileSizes(config.OtelConfig{Enabled: true, WarnMB: 1, MaxMB: 2}, dir)
	if warn != "" || exceeded {
		t.Fatalf("unexpected warn/exceeded: %q %v", warn, exceeded)
	}
	if err := os.WriteFile(path, make([]byte, 1024*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	warn, exceeded = CheckFileSizes(config.OtelConfig{Enabled: true, WarnMB: 1, MaxMB: 2}, dir)
	if warn == "" || exceeded {
		t.Fatalf("expected warning only, got %q %v", warn, exceeded)
	}
	warn, exceeded = CheckFileSizes(config.OtelConfig{Enabled: true, WarnMB: 1, MaxMB: 1}, dir)
	if !exceeded || warn == "" {
		t.Fatalf("expected max exceed, got %q %v", warn, exceeded)
	}
}
