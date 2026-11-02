package memory

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	otel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/obs"
)

// --- telemetry parity test (issue #378, checklist item 1) ---
//
// The #375 verdict claimed the memory streams disagreed ("1537 consolidate
// increments vs 135 consolidation_run lines"). Forensics later showed the
// 1537 was the sum of cumulative counter values (a counting artifact), but
// it also exposed that nobody had ever *verified* parity between the log
// stream, the metric stream and the store's real effects. This test does —
// in-process, per event: every log line must have exactly one metric
// increment and one real store effect, and metric-only events (reinforce,
// global_archived) are pinned as such so a future "fix" cannot silently
// change the contract.

type parityLogLine struct {
	Body struct {
		Type  string `json:"Type"`
		Value string `json:"Value"`
	} `json:"Body"`
	Attributes []struct {
		Key   string `json:"Key"`
		Value struct {
			Type  string `json:"Type"`
			Value any    `json:"Value"`
		} `json:"Value"`
	} `json:"Attributes"`
}

type parityMetricLine struct {
	ScopeMetrics []struct {
		Metrics []struct {
			Name string `json:"Name"`
			Data struct {
				DataPoints []struct {
					Attributes []struct {
						Key   string `json:"Key"`
						Value struct {
							Type  string `json:"Type"`
							Value any    `json:"Value"`
						} `json:"Value"`
					} `json:"Attributes"`
					Value float64 `json:"Value"`
				} `json:"DataPoints"`
			} `json:"Data"`
		} `json:"Metrics"`
	} `json:"ScopeMetrics"`
}

// parityLogCount counts log records whose body equals want.
func parityLogCount(t *testing.T, path, want string) int {
	t.Helper()
	n := 0
	for _, l := range parityReadLogs(t, path) {
		if l.Body.Value == want {
			n++
		}
	}
	return n
}

// parityLogAttrSum sums an Int64 attribute over all records of one body.
func parityLogAttrSum(t *testing.T, path, body, attr string) float64 {
	t.Helper()
	sum := 0.0
	for _, l := range parityReadLogs(t, path) {
		if l.Body.Value != body {
			continue
		}
		for _, a := range l.Attributes {
			if a.Key != attr {
				continue
			}
			if v, ok := a.Value.Value.(float64); ok {
				sum += v
			}
		}
	}
	return sum
}

func parityReadLogs(t *testing.T, path string) []parityLogLine {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("logs file: %v", err)
	}
	defer f.Close()
	var out []parityLogLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var l parityLogLine
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.Body.Value != "" {
			out = append(out, l)
		}
	}
	return out
}

// parityMetricFinal sums the *final* value of each attribute-series of a
// metric (cumulative counters: the last exported point of a series is its
// total; summing all points would count running totals — the exact mistake
// behind issue #375's "1537"). event filters percept_events by event label;
// "" takes every series of the name.
func parityMetricFinal(t *testing.T, path, name, event string) float64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("metrics file: %v", err)
	}
	defer f.Close()
	final := map[string]float64{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4<<20), 4<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var m parityMetricLine
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		for _, sm := range m.ScopeMetrics {
			for _, metric := range sm.Metrics {
				if metric.Name != name {
					continue
				}
				for _, dp := range metric.Data.DataPoints {
					ev := ""
					key := ""
					for _, a := range dp.Attributes {
						key += fmt.Sprintf("%s=%v|", a.Key, a.Value.Value)
						if a.Key == "event" {
							ev = fmt.Sprint(a.Value.Value)
						}
					}
					if event != "" && ev != event {
						continue
					}
					final[key] = dp.Value
				}
			}
		}
	}
	sum := 0.0
	for _, v := range final {
		sum += v
	}
	return sum
}

// TestObsParity_MemoryEvents runs a realistic slice of memory activity
// through the real OTel pipeline (temp dir, clean shutdown so both the log
// batch and the metric periodic reader flush) and asserts stream↔stream↔
// store parity for every event class.
func TestObsParity_MemoryEvents(t *testing.T) {
	otelDir := t.TempDir()
	shutdown, err := obs.Init(config.OtelConfig{
		Enabled:             true,
		Metrics:             true,
		Traces:              false,
		MetricsFlushMinutes: 1,
	}, otelDir)
	if err != nil {
		t.Fatalf("obs.Init: %v", err)
	}
	shutdownDone := false
	t.Cleanup(func() {
		if !shutdownDone {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			shutdown(ctx) //nolint:errcheck
			cancel()
		}
		// Restore no-op globals so later tests in this package emit into
		// nothing — never into this test's already-closed files.
		global.SetLoggerProvider(sdklog.NewLoggerProvider())
		otel.SetMeterProvider(sdkmetric.NewMeterProvider())
	})

	ctx := context.Background()
	base := t.TempDir()
	s, err := NewStoreParams(base, "parity-test", Params{GlobalArchiveAfterDays: 1})
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	// 3 new records (distinct — no accidental reinforcement among them).
	for _, c := range []string{
		"alpha entry describes the widget inventory workflow",
		"beta entry documents the gadget release checklist",
		"gamma entry explains the gizmo rollback procedure",
	} {
		if _, err := s.Record(ctx, c, ProducerLocal, ConsumerAll, Roles{}, false); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	// Re-statement of #1 → reinforce (metric-only event by contract).
	out, err := s.Record(ctx, "alpha entry describes the widget inventory workflow",
		ProducerLocal, ConsumerAll, Roles{}, false)
	if err != nil || !out.Reinforced {
		t.Fatalf("expected reinforcement, got %+v, %v", out, err)
	}
	// One recall.
	s.Query(ctx, "widget inventory", 0, 5, ConsumerAll)

	// A floored, long-idle global: archived by the next pass (#375 item).
	s.global.Percepts = append(s.global.Percepts, Percept{
		ID:          "parity-archive-test-percept",
		Content:     "ancient floored global fact",
		Producer:    ProducerLocal,
		W:           0.15,
		LastUsedAt:  time.Now().Add(-30 * 24 * time.Hour),
		IdleSeconds: 100 * 24 * 3600,
	})

	// Two consolidation passes.
	for i := 0; i < 2; i++ {
		if err := s.Consolidate(); err != nil {
			t.Fatalf("consolidate %d: %v", i, err)
		}
	}
	if arch, err := s.Archived(); err != nil || len(arch) != 1 {
		t.Fatalf("store archive effect: %d entries, %v; want exactly 1", len(arch), err)
	}

	// Flush both pipelines: the log batch and the periodic metric reader
	// export their pending state during Shutdown.
	ctx2, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := shutdown(ctx2); err != nil {
		cancel()
		t.Fatalf("obs shutdown: %v", err)
	}
	cancel()
	shutdownDone = true

	logsPath := filepath.Join(otelDir, "logs.jsonl")
	metPath := filepath.Join(otelDir, "metrics.jsonl")

	// record: log lines ⇔ metric total ⇔ store effect (3 new, the repeat
	// reinforces instead of recording).
	if got := parityLogCount(t, logsPath, "percept_recorded"); got != 3 {
		t.Errorf("percept_recorded log lines = %d, want 3", got)
	}
	if m := parityMetricFinal(t, metPath, "milk.memory.percepts_recorded", ""); m != 3 {
		t.Errorf("percepts_recorded metric = %g, want 3", m)
	}
	// recall: 1 ⇔ 1.
	if got := parityLogCount(t, logsPath, "percept_recalled"); got != 1 {
		t.Errorf("percept_recalled log lines = %d, want 1", got)
	}
	if m := parityMetricFinal(t, metPath, "milk.memory.percepts_recalled", ""); m != 1 {
		t.Errorf("percepts_recalled metric = %g, want 1", m)
	}
	// consolidate: pass count on both streams.
	if got := parityLogCount(t, logsPath, "consolidation_run"); got != 2 {
		t.Errorf("consolidation_run log lines = %d, want 2", got)
	}
	if m := parityMetricFinal(t, metPath, "milk.memory.percept_events", "consolidate"); m != 2 {
		t.Errorf("percept_events{consolidate} = %g, want 2", m)
	}
	// consolidation detail counters ⇔ the consolidation_run attributes.
	for attr, metric := range map[string]string{
		"decayed":            "milk.memory.consolidation.decayed",
		"pruned":             "milk.memory.consolidation.pruned",
		"promoted":           "milk.memory.consolidation.promoted",
		"promotion_deferred": "milk.memory.consolidation.promotion_deferred",
	} {
		l := parityLogAttrSum(t, logsPath, "consolidation_run", attr)
		m := parityMetricFinal(t, metPath, metric, "")
		if l != m {
			t.Errorf("consolidation %s: log-attr sum %g ≠ metric %g", attr, l, m)
		}
	}
	// reinforce: metric-only by contract — the log stream has no body for it.
	if m := parityMetricFinal(t, metPath, "milk.memory.percept_events", "reinforce"); m != 1 {
		t.Errorf("percept_events{reinforce} = %g, want 1", m)
	}
	// archive: metric + store effect (the log stream proves it only via the
	// post-archive global_total, asserted next).
	if m := parityMetricFinal(t, metPath, "milk.memory.global_archived", ""); m != 1 {
		t.Errorf("global_archived = %g, want 1", m)
	}
	// gauge reflects the post-archive store state on both streams.
	if m := parityMetricFinal(t, metPath, "milk.memory.global_size", ""); m != 0 {
		t.Errorf("global_size gauge = %g, want 0 (the one global was archived)", m)
	}
	if got := parityLogAttrSum(t, logsPath, "consolidation_run", "global_total"); got != 0 {
		t.Errorf("consolidation_run global_total = %g, want 0", got)
	}
}
