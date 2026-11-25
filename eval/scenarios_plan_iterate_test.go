package eval

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlanIterateScenarioFixture validates that the tooling-plan-and-iterate
// scenario is well-formed and discriminating: the fixture starts green (turn
// 1 is additive feature work, not debugging), the planted crash capture is
// deterministic (23 lines, the last truncated, 22 recoverable events with
// contiguous Seq 1..22), and the mandated events API is implementable with
// the exact semantics the prompts specify — proven by a canonical reference
// implementation plus spec tests that must pass against it. Without this,
// a fixture or spec drift could quietly turn the scenario into noise.
func TestPlanIterateScenarioFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("materializes and runs the scenario's Go fixture")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	scenarios, err := LoadScenarios("scenarios")
	if err != nil {
		t.Fatalf("LoadScenarios: %v", err)
	}
	var sc *Scenario
	for i := range scenarios {
		if scenarios[i].Name == "tooling-plan-and-iterate" {
			sc = &scenarios[i]
			break
		}
	}
	if sc == nil {
		t.Fatal("scenario tooling-plan-and-iterate not found")
	}
	if !sc.MultiTurn {
		t.Error("scenario must be multi_turn")
	}
	if len(sc.Turns) != 3 {
		t.Errorf("want 3 turns, got %d", len(sc.Turns))
	}
	for i, turn := range sc.Turns {
		if len(turn.Rubric) != 5 {
			t.Errorf("turn %d rubric: want 5 criteria, got %d", i+1, len(turn.Rubric))
		}
	}

	dir := t.TempDir()
	for _, f := range sc.Setup.Files {
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(f.Content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	goTest := func() (string, error) {
		cmd := exec.Command("go", "test", "./...")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// 1. The fixture must start green: turn 1 is feature work on a
	// healthy baseline, not a debugging task.
	if out, err := goTest(); err != nil {
		t.Fatalf("fixture suite must start green (turn 1 is additive): %v\n%s", err, out)
	}

	// 2. The planted capture must be deterministic: 23 lines, 22 valid
	// events with contiguous Seq 1..22 (line 7 carries a nil Detail), and
	// a truncated 23rd line that is not valid JSON.
	capRaw, err := os.ReadFile(filepath.Join(dir, "data", "capture.jsonl"))
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(capRaw), "\n"), "\n")
	if len(lines) != 23 {
		t.Fatalf("capture must have 23 lines, got %d", len(lines))
	}
	for i, line := range lines[:22] {
		var ev struct {
			Seq    uint64
			Kind   string
			Ref    string
			Detail map[string]string
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("capture line %d not valid JSON: %v", i+1, err)
		}
		if ev.Seq != uint64(i+1) {
			t.Errorf("capture line %d: Seq = %d, want %d", i+1, ev.Seq, i+1)
		}
		if i == 6 && ev.Detail != nil {
			t.Errorf("capture line 7 must carry a nil Detail (round-trip pin), got %v", ev.Detail)
		}
	}
	var tail map[string]any
	if err := json.Unmarshal([]byte(lines[22]), &tail); err == nil {
		t.Errorf("capture line 23 must be truncated (invalid JSON), parsed cleanly: %v", tail)
	}

	// 3. The mandated API must be implementable exactly as specified:
	// write a canonical reference implementation + spec tests into the
	// fixture and require everything to pass.
	eventsDir := filepath.Join(dir, "internal", "events")
	if err := os.MkdirAll(eventsDir, 0o755); err != nil {
		t.Fatalf("mkdir events: %v", err)
	}
	if err := os.WriteFile(filepath.Join(eventsDir, "events.go"), []byte(canonicalEvents), 0o644); err != nil {
		t.Fatalf("write canonical events.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(eventsDir, "spec_test.go"), []byte(canonicalEventsSpecTest), 0o644); err != nil {
		t.Fatalf("write canonical spec_test.go: %v", err)
	}
	if out, err := goTest(); err != nil {
		t.Errorf("canonical events implementation must satisfy the spec tests: %v\n%s", err, out)
	}
}

// canonicalEvents is a reference implementation of the events package the
// prompts mandate (turn 1 API + turn 2 persistence + turn 3 recovery). It
// exists only inside this test: the scenario never ships it.
const canonicalEvents = `package events

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Event is one order lifecycle event.
type Event struct {
	Seq    uint64
	Kind   string
	Ref    string
	Detail map[string]string
}

// Log is an in-memory append-only event log.
type Log struct {
	events []Event
}

// Append adds one event and returns it. Seq starts at 1 and is contiguous.
func (l *Log) Append(kind, ref string, detail map[string]string) Event {
	e := Event{Seq: uint64(len(l.events) + 1), Kind: kind, Ref: ref, Detail: detail}
	l.events = append(l.events, e)
	return e
}

// All returns every event in Seq order as a defensive copy.
func (l *Log) All() []Event {
	out := make([]Event, len(l.events))
	copy(out, l.events)
	return out
}

// Filter returns events of the given kind in Seq order as a defensive copy.
func (l *Log) Filter(kind string) []Event {
	var out []Event
	for _, e := range l.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// Save writes one JSON object per line, creating parent directories.
func (l *Log) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, e := range l.events {
		if err := enc.Encode(e); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// ErrSeqGap reports a non-contiguous Seq sequence.
var ErrSeqGap = errors.New("events: sequence gap")

// ErrTruncated reports a line that is not a valid JSON event.
var ErrTruncated = errors.New("events: truncated record")

// Load reads a JSONL event file strictly.
func Load(path string) (*Log, error) { return load(path, false) }

// Recover reads a JSONL event file, dropping a truncated final line.
func Recover(path string) (*Log, error) { return load(path, true) }

func load(path string, allowTruncatedTail bool) (*Log, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if allowTruncatedTail && errors.Is(err, os.ErrNotExist) {
			return &Log{}, nil
		}
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	var l Log
	for i, line := range lines {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			if allowTruncatedTail && i == len(lines)-1 {
				break
			}
			return nil, ErrTruncated
		}
		if e.Seq != uint64(len(l.events)+1) {
			return nil, ErrSeqGap
		}
		if e.Detail == nil {
			e.Detail = map[string]string{}
		}
		l.events = append(l.events, e)
	}
	return &l, nil
}
`

// canonicalEventsSpecTest pins every mandated behavior: turn 1 append/order/
// Filter/defensive-copy/nil-Detail semantics, turn 2 Save/Load round-trip and
// ErrSeqGap/ErrTruncated strictness, turn 3 Recover leniency against the
// planted capture file and missing-file behavior.
const canonicalEventsSpecTest = `package events

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAppendSeqContiguous(t *testing.T) {
	var l Log
	if e := l.Append("confirmed", "ord-1", nil); e.Seq != 1 {
		t.Fatalf("first Seq = %d, want 1", e.Seq)
	}
	if e := l.Append("confirmed", "ord-2", nil); e.Seq != 2 {
		t.Fatalf("second Seq = %d, want 2", e.Seq)
	}
	all := l.All()
	if len(all) != 2 || all[0].Seq != 1 || all[1].Seq != 2 {
		t.Fatalf("All = %+v, want Seq 1,2 in order", all)
	}
}

func TestAllIsDefensiveCopy(t *testing.T) {
	var l Log
	l.Append("confirmed", "ord-1", nil)
	all := l.All()
	all[0].Ref = "mutated"
	if got := l.All()[0].Ref; got != "ord-1" {
		t.Fatalf("mutating All() result changed the log: %q", got)
	}
}

func TestFilterByKind(t *testing.T) {
	var l Log
	l.Append("confirmed", "ord-1", nil)
	l.Append("reserved", "ord-2", nil)
	l.Append("confirmed", "ord-3", nil)
	got := l.Filter("confirmed")
	if len(got) != 2 || got[0].Ref != "ord-1" || got[1].Ref != "ord-3" {
		t.Fatalf("Filter = %+v, want ord-1,ord-3 in Seq order", got)
	}
}

func TestNilDetailRoundTrip(t *testing.T) {
	dir := t.TempDir()
	var l Log
	l.Append("confirmed", "ord-1", nil)
	l.Append("confirmed", "ord-2", map[string]string{"via": "ConfirmOrder"})
	p := filepath.Join(dir, "nested", "events.jsonl")
	if err := l.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	back, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	all := back.All()
	if len(all) != 2 {
		t.Fatalf("round-trip lost events: %+v", all)
	}
	if all[0].Detail == nil || len(all[0].Detail) != 0 {
		t.Fatalf("nil Detail must load as empty map, got %v", all[0].Detail)
	}
	if all[1].Detail["via"] != "ConfirmOrder" {
		t.Fatalf("Detail not preserved: %v", all[1].Detail)
	}
	if all[0].Seq != 1 || all[0].Kind != "confirmed" || all[0].Ref != "ord-1" {
		t.Fatalf("event not preserved: %+v", all[0])
	}
}

func TestLoadRejectsGap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gap.jsonl")
	if err := os.WriteFile(p, []byte("{\"Seq\":1,\"Kind\":\"a\",\"Ref\":\"r\"}\n{\"Seq\":3,\"Kind\":\"a\",\"Ref\":\"r\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); !errors.Is(err, ErrSeqGap) {
		t.Fatalf("Load gap = %v, want ErrSeqGap", err)
	}
	if _, err := Recover(p); !errors.Is(err, ErrSeqGap) {
		t.Fatalf("Recover gap = %v, want ErrSeqGap", err)
	}
}

func TestLoadRejectsTruncated(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "trunc.jsonl")
	if err := os.WriteFile(p, []byte("{\"Seq\":1,\"Kind\":\"a\",\"Ref\":\"r\"}\n{\"Seq\":2,\"Kind\":\"a\""), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); !errors.Is(err, ErrTruncated) {
		t.Fatalf("Load truncated = %v, want ErrTruncated (Load stays strict)", err)
	}
}

func TestLoadMissingFileIsError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Fatal("Load on missing file must return the filesystem error")
	}
}

func TestRecoverCapture(t *testing.T) {
	l, err := Recover(filepath.Join("..", "..", "data", "capture.jsonl"))
	if err != nil {
		t.Fatalf("Recover capture: %v", err)
	}
	all := l.All()
	if len(all) != 22 {
		t.Fatalf("recovered %d events, want 22", len(all))
	}
	for i, e := range all {
		if e.Seq != uint64(i+1) {
			t.Fatalf("recovered Seq order broken at %d: %d", i, e.Seq)
		}
	}
	if e := l.Append("confirmed", "ord-1023", nil); e.Seq != 23 {
		t.Fatalf("Append after Recover got Seq %d, want 23", e.Seq)
	}
}

func TestRecoverMissingFileIsEmpty(t *testing.T) {
	l, err := Recover(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatalf("Recover missing = %v, want nil", err)
	}
	if got := l.All(); len(got) != 0 {
		t.Fatalf("Recover missing = %+v, want empty log", got)
	}
}
`
