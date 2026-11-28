package obs

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/scoutme/milk/internal/config"
)

// ── #458 runtime emission gate ─────────────────────────────────────────────

func TestGatedWriter_DiscardsWhileDisabled(t *testing.T) {
	var buf bytes.Buffer
	g := &gatedWriter{w: &buf}

	SetEnabled(false)
	t.Cleanup(func() { SetEnabled(true) })
	n, err := g.Write([]byte("dropped\n"))
	if err != nil {
		t.Fatalf("disabled write must not error: %v", err)
	}
	if n != len("dropped\n") {
		t.Fatalf("disabled write must report a full write, got %d", n)
	}
	if buf.Len() != 0 {
		t.Fatalf("disabled gate must discard, got %q", buf.String())
	}

	SetEnabled(true)
	if _, err := g.Write([]byte("kept\n")); err != nil {
		t.Fatalf("enabled write: %v", err)
	}
	if buf.String() != "kept\n" {
		t.Fatalf("enabled gate must pass through, got %q", buf.String())
	}
}

func TestSetEnabled_OnClearsSizeTrip(t *testing.T) {
	SetEnabled(true)
	sizeTrip.Store(true)
	t.Cleanup(func() { sizeTrip.Store(false) })
	if Enabled() {
		t.Fatal("size trip must close the gate")
	}
	SetEnabled(true) // explicit re-enable overrides a stale trip
	if !Enabled() {
		t.Fatal("/otel on must reopen the gate")
	}
}

func TestFormatStats_ReportsGateState(t *testing.T) {
	dir := t.TempDir()

	SetEnabled(false)
	t.Cleanup(func() { SetEnabled(true) })
	if out := FormatStats(dir); !strings.Contains(out, "disabled for this session") {
		t.Fatalf("expected manual-disable state, got:\n%s", out)
	}

	SetEnabled(true)
	sizeTrip.Store(true)
	t.Cleanup(func() { sizeTrip.Store(false) })
	if out := FormatStats(dir); !strings.Contains(out, "DISABLED") || !strings.Contains(out, "max_mb") {
		t.Fatalf("expected size-cap state, got:\n%s", out)
	}

	SetEnabled(true) // clears the trip on the re-enable path
	if out := FormatStats(dir); strings.Contains(out, "state:") {
		t.Fatalf("enabled state must not report a disabled state:\n%s", out)
	}
}

func TestMaybeEnforceSizeCap_TripsAndTrimReopens(t *testing.T) {
	isolateDebugLogPaths(t)
	dir := t.TempDir()

	prev := runtimeState
	t.Cleanup(func() { runtimeState = prev })
	runtimeState.dir = dir
	runtimeState.cfg = config.OtelConfig{MaxMB: 1}
	enforceAt.Store(0)
	t.Cleanup(func() { enforceAt.Store(0); sizeTrip.Store(false) })
	SetEnabled(true)
	t.Cleanup(func() { SetEnabled(true) })

	// Under the cap: still enabled, nothing tripped.
	if err := os.WriteFile(filepath.Join(dir, "logs.jsonl"), make([]byte, 1024*1024-1), 0o600); err != nil {
		t.Fatal(err)
	}
	MaybeEnforceSizeCap()
	if sizeTrip.Load() {
		t.Fatal("under-cap file must not trip the gate")
	}

	// Over the cap: gate closes.
	enforceAt.Store(0)
	if err := os.WriteFile(filepath.Join(dir, "logs.jsonl"), make([]byte, 1024*1024+1), 0o600); err != nil {
		t.Fatal(err)
	}
	MaybeEnforceSizeCap()
	if !sizeTrip.Load() || Enabled() {
		t.Fatal("over-cap file must close the gate")
	}

	// A further sweep while tripped is a no-op (already disabled).
	enforceAt.Store(0)
	MaybeEnforceSizeCap()
	if !sizeTrip.Load() {
		t.Fatal("tripped state must persist until trim")
	}

	// /otel trim archives the oversize data and reopens the gate.
	if err := Trim(dir); err != nil {
		t.Fatalf("Trim: %v", err)
	}
	if sizeTrip.Load() {
		t.Fatal("Trim must clear the size-cap trip")
	}
	if !Enabled() {
		t.Fatal("Trim must reopen the gate when the user didn't turn OTel off")
	}
}

func TestMaybeEnforceSizeCap_SurvivesManualOffAcrossTrim(t *testing.T) {
	isolateDebugLogPaths(t)
	dir := t.TempDir()

	prev := runtimeState
	t.Cleanup(func() { runtimeState = prev })
	runtimeState.dir = dir
	runtimeState.cfg = config.OtelConfig{MaxMB: 1}
	enforceAt.Store(0)
	t.Cleanup(func() { enforceAt.Store(0); sizeTrip.Store(false) })

	SetEnabled(false) // the user ran /otel off
	t.Cleanup(func() { SetEnabled(true) })
	if err := os.WriteFile(filepath.Join(dir, "logs.jsonl"), make([]byte, 2*1024*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Trim(dir); err != nil {
		t.Fatalf("Trim: %v", err)
	}
	if Enabled() {
		t.Fatal("Trim must not reopen a manually closed gate")
	}
}

// ── #455 instance/session attribution ──────────────────────────────────────

func TestInstanceID_CarriesPIDAndNonce(t *testing.T) {
	pid := strconv.Itoa(os.Getpid())
	if !strings.HasPrefix(instanceID, pid+"-") || len(instanceID) <= len(pid)+1 {
		t.Fatalf("instance id must be <pid>-<nonce>, got %q", instanceID)
	}
	if b, err := strconv.Atoi(strings.SplitN(instanceID, "-", 2)[0]); err != nil || b != os.Getpid() {
		t.Fatalf("instance id must start with the pid, got %q", instanceID)
	}
}

func TestStartSpan_CarriesSessionIDFromContext(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	spanAttr := func(name string) (attribute.Value, bool) {
		for _, s := range rec.Ended() {
			if s.Name() != name {
				continue
			}
			for _, a := range s.Attributes() {
				if a.Key == "session_id" {
					return a.Value, true
				}
			}
		}
		return attribute.Value{}, false
	}

	ctx := ContextWithSessionID(context.Background(), "sess-123")
	_, span := StartSpan(ctx, "test.with-session")
	span.End()
	v, ok := spanAttr("test.with-session")
	if !ok || v.AsString() != "sess-123" {
		t.Fatalf("span must carry session_id=sess-123, got ok=%v v=%v", ok, v)
	}

	_, bare := StartSpan(context.Background(), "test.without-session")
	bare.End()
	if _, ok := spanAttr("test.without-session"); ok {
		t.Fatal("span without a session context must not invent a session_id")
	}
}

func TestNewLogHandler_StampsPid(t *testing.T) {
	var buf bytes.Buffer
	h := newLogHandler("json", &buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.New(h).Info("hello", "k", "v")

	want := fmt.Sprintf(`"pid":%d`, os.Getpid())
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("milk.log record must carry %s, got %s", want, buf.String())
	}
	if !strings.Contains(buf.String(), `"k":"v"`) {
		t.Fatalf("existing attributes must survive, got %s", buf.String())
	}
}

// ── #457 trim / rotation safety ────────────────────────────────────────────

func TestTrim_SameDaySecondTrimKeepsBothArchives(t *testing.T) {
	isolateDebugLogPaths(t)
	dir := t.TempDir()
	live := filepath.Join(dir, "logs.jsonl")

	if err := os.WriteFile(live, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Trim(dir); err != nil {
		t.Fatalf("first Trim: %v", err)
	}
	archived, _ := filepath.Glob(filepath.Join(dir, "logs.*.jsonl"))
	if len(archived) != 1 {
		t.Fatalf("expected 1 archive after first trim, got %v", archived)
	}
	first := archived[0]
	if got, _ := os.ReadFile(first); string(got) != "first\n" {
		t.Fatalf("first archive content = %q", got)
	}

	// Second trim the same day must not overwrite the first archive.
	if err := os.WriteFile(live, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Trim(dir); err != nil {
		t.Fatalf("second Trim: %v", err)
	}
	archived, _ = filepath.Glob(filepath.Join(dir, "logs.*.jsonl"))
	if len(archived) != 2 {
		t.Fatalf("expected 2 archives after second trim, got %v", archived)
	}
	contents := map[string]bool{}
	for _, p := range archived {
		b, _ := os.ReadFile(p)
		contents[string(b)] = true
	}
	if !contents["first\n"] || !contents["second\n"] {
		t.Fatalf("both trims' data must survive, got %v", contents)
	}
	if got, _ := os.ReadFile(live); len(got) != 0 {
		t.Fatalf("live file must be empty, got %q", got)
	}
}

func TestRotatingWriter_RotatesOnSharedFileSize(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "shared.log")

	rw, err := NewRotatingWriter(base, 50, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()

	// Another session's writes count toward the shared cap — there is no
	// per-process byte counter anymore.
	ext, err := os.OpenFile(base, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ext.WriteString(strings.Repeat("x", 49)); err != nil {
		t.Fatal(err)
	}
	if err := ext.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := rw.Write([]byte("yy")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	rotated, err := os.ReadFile(filepath.Join(dir, "shared.1.log"))
	if err != nil {
		t.Fatalf("expected rotation triggered by the shared size: %v", err)
	}
	if len(rotated) != 51 {
		t.Fatalf("rotated file should hold all 51 shared bytes, got %d", len(rotated))
	}
	if info, err := os.Stat(base); err != nil || info.Size() != 0 {
		t.Fatalf("primary must be fresh after rotate, err=%v", err)
	}
}

func TestRotatingWriter_RotateSkipsWhenSharedFileUnderCap(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "skip.log")

	rw, err := NewRotatingWriter(base, 50, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()

	if _, err := rw.Write([]byte("small\n")); err != nil {
		t.Fatal(err)
	}
	// Direct call (as if the size check raced another process): under the
	// cap the chain must not shift.
	if err := rw.rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "skip.1.log")); !os.IsNotExist(err) {
		t.Fatalf("rotate under cap must not shift files, err=%v", err)
	}
	if got, _ := os.ReadFile(base); string(got) != "small\n" {
		t.Fatalf("primary content must be untouched, got %q", got)
	}
}

// ── #456 instance-aware readers ────────────────────────────────────────────

func metricLine(instance, name string, value float64, ts, temporality string, attrs map[string]string) string {
	attrJSON := ""
	for k, v := range attrs {
		if attrJSON != "" {
			attrJSON += ","
		}
		attrJSON += fmt.Sprintf(`{"Key":%q,"Value":{"Type":"STRING","Value":%q}}`, k, v)
	}
	temp := ""
	if temporality != "" {
		temp = fmt.Sprintf(`,"Temporality":%q,"IsMonotonic":true`, temporality)
	}
	return fmt.Sprintf(
		`{"Resource":[{"Key":"service.instance.id","Value":{"Type":"STRING","Value":%q}}],`+
			`"ScopeMetrics":[{"Metrics":[{"Name":%q,"Data":{"DataPoints":`+
			`[{"Value":%g,"Attributes":[%s],"Time":%q}]%s}}]}]}`,
		instance, name, value, attrJSON, ts, temp)
}

func TestFormatMetrics_AggregatesAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		// Cumulative counters: summed across the two sessions (3+4=7).
		metricLine("100-aa", "milk.turns.total", 3, "2024-01-01T00:00:00Z",
			"CumulativeTemporality", map[string]string{"agent": "primary"}),
		metricLine("200-bb", "milk.turns.total", 4, "2024-01-01T00:01:00Z",
			"CumulativeTemporality", map[string]string{"agent": "primary"}),
		// Gauge (file size): both sessions observe the same file — newest
		// datapoint wins (222), not the sum (333).
		metricLine("100-aa", "milk.otel.logs_bytes", 111, "2024-01-01T00:00:00Z",
			"", map[string]string{"file": "logs.jsonl"}),
		metricLine("200-bb", "milk.otel.logs_bytes", 222, "2024-01-01T00:01:00Z",
			"", map[string]string{"file": "logs.jsonl"}),
	}
	if err := os.WriteFile(filepath.Join(dir, "metrics.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := FormatMetrics(dir)
	if m, _ := regexp.MatchString(`milk\.turns\.total\{agent=primary\}\s+7(\s|$)`, out); !m {
		t.Fatalf("cumulative counter must be summed across instances:\n%s", out)
	}
	if m, _ := regexp.MatchString(`milk\.otel\.logs_bytes\{file=logs\.jsonl\}\s+222\s+@ 2024-01-01T00:01:00Z`, out); !m {
		t.Fatalf("gauge must take the newest datapoint, not the sum:\n%s", out)
	}
}

func TestFormatTokenUsage_SumsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		metricLine("100-aa", "milk.tokens.prompt", 100, "2024-01-01T00:00:00Z",
			"CumulativeTemporality", map[string]string{"model": "m", "agent": "primary"}),
		metricLine("200-bb", "milk.tokens.prompt", 200, "2024-01-01T00:01:00Z",
			"CumulativeTemporality", map[string]string{"model": "m", "agent": "primary"}),
	}
	if err := os.WriteFile(filepath.Join(dir, "metrics.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := FormatTokenUsage(context.TODO(), dir, nil, 0)
	if m, _ := regexp.MatchString(`primary\s+m\s+300\s`, out); !m {
		t.Fatalf("cumulative token table must sum both sessions (100+200):\n%s", out)
	}
}

// ── #459 session-labeled token counters / cumulative de-duplication ─────────

// TestRecordTokens_StampsSessionID: token counters carry the turn's session
// id as a data-point label when the ctx carries one — the reader-side half
// of #459 option C — and invent no label without it (mirrors the StartSpan
// #455 stamping contract).
func TestRecordTokens_StampsSessionID(t *testing.T) {
	mr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(mr))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		_ = mp.Shutdown(context.Background())
		ResetSessionTokens()
	})

	collectPromptAttrs := func() []map[string]string {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := mr.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		var out []map[string]string
		for _, sm := range rm.ScopeMetrics {
			for _, met := range sm.Metrics {
				if met.Name != "milk.tokens.prompt" {
					continue
				}
				sum, ok := met.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("%s data is %T, want Sum[int64]", met.Name, met.Data)
				}
				for _, dp := range sum.DataPoints {
					attrs := map[string]string{}
					for _, kv := range dp.Attributes.ToSlice() {
						attrs[string(kv.Key)] = kv.Value.AsString()
					}
					out = append(out, attrs)
				}
			}
		}
		return out
	}

	RecordTokens(ContextWithSessionID(context.Background(), "sess-455"), "m", "primary", 10, 5)
	labeled := false
	for _, attrs := range collectPromptAttrs() {
		if attrs["session_id"] == "sess-455" {
			labeled = true
			if attrs["model"] != "m" || attrs["agent"] != "primary" {
				t.Fatalf("model/agent labels must survive session stamping, got %v", attrs)
			}
		}
	}
	if !labeled {
		t.Fatalf("session ctx must produce a session_id-labeled data point, got %v", collectPromptAttrs())
	}

	// A ctx without a session must invent no label (separate data point).
	RecordTokens(context.Background(), "m", "primary", 7, 3)
	unlabeled := false
	for _, attrs := range collectPromptAttrs() {
		if _, has := attrs["session_id"]; !has {
			unlabeled = true
		}
	}
	if !unlabeled {
		t.Fatalf("a ctx without a session must not invent a session_id, got %v", collectPromptAttrs())
	}
}

// TestFormatTokenUsage_SubtractsCurrentSessionFlush pins #459 option C: the
// cumulative table counts the current session exactly once — other
// sessions'/instances' flushed datapoints plus THIS session's store totals
// (its flushed share subtracted, its unflushed tail included), while
// file-only rows the store doesn't track stay untouched.
func TestFormatTokenUsage_SubtractsCurrentSessionFlush(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		// Current session s1 — flushed share so far (1000).
		metricLine("100-aa", "milk.tokens.prompt", 1000, "2024-01-01T00:00:00Z",
			"CumulativeTemporality", map[string]string{"model": "m", "agent": "primary", "session_id": "s1"}),
		// Another session's writer instance (500) — stays.
		metricLine("200-bb", "milk.tokens.prompt", 500, "2024-01-01T00:01:00Z",
			"CumulativeTemporality", map[string]string{"model": "m", "agent": "primary", "session_id": "s2"}),
		// A role the session store doesn't track (300): file-only row —
		// subtracting without a store entry to replace it would lose usage.
		metricLine("100-aa", "milk.tokens.prompt", 300, "2024-01-01T00:00:00Z",
			"CumulativeTemporality", map[string]string{"model": "m", "agent": "router", "session_id": "s1"}),
	}
	if err := os.WriteFile(filepath.Join(dir, "metrics.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Store: the current session's FULL totals (1000 flushed + 200 unflushed).
	entries := []SessionTokenEntry{{Model: "m", Agent: "primary", Prompt: 1200, Completion: 100}}
	ctx := ContextWithSessionID(context.TODO(), "s1")
	out := FormatTokenUsage(ctx, dir, entries, 4)

	// (1000+500) − 1000 + 1200 = 1700 — not 2700 (the #459 double count) and
	// not 1500 (store dropped). completion is store-only: 100 → total 1800.
	if m, _ := regexp.MatchString(`primary\s+m\s+1700\s+100\s+1800\s`, out); !m {
		t.Fatalf("cumulative primary row must be 1700/100/1800 (flushed once, tail kept):\n%s", out)
	}
	if m, _ := regexp.MatchString(`router\s+m\s+300\s`, out); !m {
		t.Fatalf("file-only router row must keep 300:\n%s", out)
	}
}

// TestFormatTokenUsage_Issue459Repro: the issue's minimal repro — 1000
// flushed + 1000 stored renders 1000 when the session is known on ctx, and
// keeps the historical additive total (2000) when it isn't: without a
// session id there is nothing to subtract, and guessing would lose usage.
func TestFormatTokenUsage_Issue459Repro(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		metricLine("100-aa", "milk.tokens.prompt", 1000, "2024-01-01T00:00:00Z",
			"CumulativeTemporality", map[string]string{"model": "m", "agent": "primary", "session_id": "s1"}),
	}
	if err := os.WriteFile(filepath.Join(dir, "metrics.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries := []SessionTokenEntry{{Model: "m", Agent: "primary", Prompt: 1000}}

	out := FormatTokenUsage(ContextWithSessionID(context.TODO(), "s1"), dir, entries, 2)
	if m, _ := regexp.MatchString(`primary\s+m\s+1000\s`, out); !m {
		t.Fatalf("session known: 1000 flushed + 1000 stored must render 1000, not 2000:\n%s", out)
	}

	out = FormatTokenUsage(context.TODO(), dir, entries, 2)
	if m, _ := regexp.MatchString(`primary\s+m\s+2000\s`, out); !m {
		t.Fatalf("no session on ctx: the legacy additive total (2000) must remain:\n%s", out)
	}
}

// ── #457 archive naming / retention ────────────────────────────────────────

func TestSplitArchiveName_AcceptsTimeAndCounterSuffixes(t *testing.T) {
	bases := map[string]bool{"logs": true, "milk": true, "metrics": true}
	cases := []struct {
		name string
		want bool
	}{
		{"logs.2026-11-25.jsonl", true},
		{"logs.2026-11-25.143012.jsonl", true},
		{"logs.2026-11-25.143012.3.jsonl", true},
		{"milk.2026-11-25.log", true},
		{"milk.2026-11-25.143012.log", true},
		{"milk.1.log", false},                // rotation file, no date
		{"logs.jsonl", false},                // live file
		{"metrics.2026-01-012.jsonl", false}, // malformed date+tail
		{"logs.2026-11-25.txt", false},       // wrong extension
		{"logs.2026-11-25.14301.log", false}, // 5-digit clock
		{"logs.2026-11-25.abc.log", false},   // non-numeric suffix
		{"logs.2026-11-25.143012.jsonl.x", false},
	}
	for _, c := range cases {
		if _, _, ok := splitArchiveName(c.name, bases); ok != c.want {
			t.Errorf("splitArchiveName(%q) = %v, want %v", c.name, ok, c.want)
		}
	}
}

func TestPruneArchives_OrdersByModTimeAcrossSuffixes(t *testing.T) {
	isolateDebugLogPaths(t)
	dir := t.TempDir()

	// Four same-day archives. Name order would rank the plain name first
	// (and drop the .090000 one); mod times say the plain name is the
	// OLDEST — retention must follow the mod times.
	write := func(name string, mtime time.Time) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	write("logs.2026-11-25.jsonl", base) // oldest by mtime
	write("logs.2026-11-25.140000.jsonl", base.Add(time.Hour))
	write("logs.2026-11-25.150000.jsonl", base.Add(2*time.Hour))
	write("logs.2026-11-25.160000.jsonl", base.Add(3*time.Hour)) // newest

	removed, _ := PruneArchives(dir)
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs.2026-11-25.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("oldest archive (by mtime) must be pruned, stat err=%v", err)
	}
	for _, keep := range []string{
		"logs.2026-11-25.140000.jsonl",
		"logs.2026-11-25.150000.jsonl",
		"logs.2026-11-25.160000.jsonl",
	} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Fatalf("newest three must survive: %v", err)
		}
	}
}
