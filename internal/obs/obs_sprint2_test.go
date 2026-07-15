package obs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
)

func TestCheckFileSizes_WarnAndMax(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(name string, size int) {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("logs.jsonl", 2*1024*1024)
	mustWrite("traces.jsonl", 1)
	mustWrite("metrics.jsonl", 1)

	warn, exceeded := CheckFileSizes(config.OtelConfig{Enabled: true, WarnMB: 1, MaxMB: 3}, dir)
	if warn == "" || exceeded {
		t.Fatalf("expected warning only, got warn=%q exceeded=%v", warn, exceeded)
	}

	warn, exceeded = CheckFileSizes(config.OtelConfig{Enabled: true, WarnMB: 1, MaxMB: 2}, dir)
	if !exceeded {
		t.Fatalf("expected max cap exceeded")
	}
}

func TestFormatStatsAndTrim(t *testing.T) {
	dir := t.TempDir()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	content := []byte(`{"time":"` + stamp + `","body":"x"}
`)
	for _, name := range []string{"logs.jsonl", "traces.jsonl", "metrics.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stats := FormatStats(dir)
	if stats == "" || filepath.Base(dir) == "" {
		t.Fatal("expected stats output")
	}
	if err := Trim(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"logs.jsonl", "traces.jsonl", "metrics.jsonl"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != 0 {
			t.Fatalf("expected empty %s after trim", name)
		}
	}
}

func TestSearchSignalsFiltersBySignal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "logs.jsonl"), []byte("alpha\nBeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metrics.jsonl"), []byte("gamma beta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	results := SearchSignals(dir, "BETA", []string{"logs"}, 10)
	if len(results) != 1 || results[0].File != "logs.jsonl" {
		t.Fatalf("unexpected results: %#v", results)
	}
}

func TestFormatMetricsStable(t *testing.T) {
	dir := t.TempDir()
	if got := FormatMetrics(dir); got == "" {
		t.Fatal("expected non-empty metrics output")
	}
	_ = context.Background()
}

func TestFormatMetricsShowsHintAndLatestTimestamp(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "metrics.jsonl"), []byte(`{"ScopeMetrics":[{"Metrics":[{"Name":"milk.turns.total","Data":{"DataPoints":[{"Value":1,"Attributes":[{"Key":"agent","Value":{"Value":"primary"}}],"Time":"2024-01-01T00:00:00Z"}]}}]}]}
{"ScopeMetrics":[{"Metrics":[{"Name":"milk.turns.total","Data":{"DataPoints":[{"Value":2,"Attributes":[{"Key":"agent","Value":{"Value":"primary"}}],"Time":"2024-01-01T00:01:00Z"}]}}]}]}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	out := FormatMetrics(dir)
	if !strings.Contains(out, "milk.turns.total{agent=primary}") || !strings.Contains(out, "2") || !strings.Contains(out, "@ 2024-01-01T00:01:00Z") {
		t.Fatalf("unexpected metrics output: %q", out)
	}
	if !strings.Contains(out, "hint: /otel for file sizes, /otel trim to archive") {
		t.Fatalf("expected hint in metrics output: %q", out)
	}
}
