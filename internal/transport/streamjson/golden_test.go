package streamjson

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Golden-file wire-contract suite over ../../../testdata/events. Each .jsonl
// file is a recorded one-shot `--output-format stream-json` run pinning one
// slice of the §6 event catalog (ADR-0049). The suite asserts, per file and
// across the corpus:
//
//   - golden comparison: every line round-trips byte-exactly through the typed
//     model (Decode → EncodeLine) — run `go test ./internal/transport/streamjson
//     -update` to regenerate goldens after a ratified contract change;
//   - semantic preservation on that round-trip (no data loss through the typed
//     model — this is also what "provider detail confined to extension/_meta"
//     means mechanically: anything outside the reserved extension points must
//     survive the typed round-trip);
//   - explicit confinement: unknown keys outside input/tool_use_result/task/
//     extension/_meta subtrees are rejected by a raw walk;
//   - no ANSI escape sequences anywhere (machine transports never emit them);
//   - open-set capabilities: system/init.capabilities tolerates unknown
//     strings and always advertises stream_v1;
//   - framing: system/init first, exactly one terminal result last, monotonic
//     seq, stable session_id, RFC 3339 ts;
//   - corpus coverage: every feature the contract promises is exercised by at
//     least one golden.
var update = flag.Bool("update", false, "rewrite testdata/events/*.jsonl golden files in canonical form")

const eventsDir = "../../../testdata/events"

func loadGoldenFiles(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(eventsDir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no golden files in %s", eventsDir)
	}
	sort.Strings(paths)
	return paths
}

// splitLines returns non-empty lines with trailing whitespace stripped.
func splitLines(raw []byte) [][]byte {
	var lines [][]byte
	for _, l := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestGoldenWireContract(t *testing.T) {
	for _, path := range loadGoldenFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			t.Run("no_ansi", func(t *testing.T) {
				if i := bytes.IndexByte(raw, 0x1b); i >= 0 {
					t.Fatalf("ANSI escape (0x1b) at byte %d: machine transports must never emit escape sequences", i)
				}
			})

			t.Run("provider_detail_confined", func(t *testing.T) {
				if err := checkConfinement(raw); err != nil {
					t.Fatal(err)
				}
			})

			lines := splitLines(raw)
			if len(lines) < 2 {
				t.Fatalf("golden run too short: %d lines (need init + result)", len(lines))
			}

			var events []*Event
			var canonical [][]byte
			for i, line := range lines {
				ev, err := Decode(line)
				if err != nil {
					t.Fatalf("line %d: %v", i+1, err)
				}
				re, err := EncodeLine(ev)
				if err != nil {
					t.Fatalf("line %d: encode: %v", i+1, err)
				}
				re = bytes.TrimSuffix(re, []byte("\n"))

				// Semantic preservation: the typed model must not lose any
				// data present on the wire (a dropped key means either a
				// missing struct field or provider detail outside extension/
				// _meta).
				var fromWire, fromTyped any
				if err := json.Unmarshal(line, &fromWire); err != nil {
					t.Fatalf("line %d: %v", i+1, err)
				}
				if err := json.Unmarshal(re, &fromTyped); err != nil {
					t.Fatalf("line %d: re-encode: %v", i+1, err)
				}
				if !reflect.DeepEqual(fromWire, fromTyped) {
					t.Fatalf("line %d: typed round-trip lost or altered data:\n wire: %s\n  got: %s", i+1, line, re)
				}

				events = append(events, ev)
				canonical = append(canonical, re)
			}

			t.Run("golden_comparison", func(t *testing.T) {
				stale := false
				for i, line := range lines {
					if !bytes.Equal(line, canonical[i]) {
						stale = true
						if !*update {
							t.Errorf("line %d: golden is not in canonical form (want -update regeneration)\n got: %s\nwant: %s", i+1, canonical[i], line)
						}
					}
				}
				if stale && *update {
					var out bytes.Buffer
					for _, c := range canonical {
						out.Write(c)
						out.WriteByte('\n')
					}
					if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
						t.Fatal(err)
					}
					t.Logf("regenerated %s", path)
				}
			})

			assertFraming(t, events)
			assertCapabilitiesOpenSet(t, events)
			assertResultContract(t, events)
		})
	}
}

// assertFraming pins §8.3: system/init first, exactly one terminal result
// last, monotonic seq from 0, stable session_id, non-empty RFC 3339 ts.
func assertFraming(t *testing.T, events []*Event) {
	t.Helper()
	if events[0].Type != TypeSystem || events[0].Subtype != SubtypeInit {
		t.Errorf("first line must be system/init, got %s/%s", events[0].Type, events[0].Subtype)
	}
	results := 0
	for i, ev := range events {
		if ev.Type == TypeResult {
			results++
			if i != len(events)-1 {
				t.Errorf("result at line %d is not terminal", i+1)
			}
		}
		if ev.Seq != int64(i) {
			t.Errorf("line %d: seq=%d, want monotonic %d", i+1, ev.Seq, i)
		}
		if ev.SessionID == "" {
			t.Errorf("line %d: missing session_id", i+1)
		}
		if ev.SessionID != events[0].SessionID {
			t.Errorf("line %d: session_id %q differs from init %q", i+1, ev.SessionID, events[0].SessionID)
		}
		if ev.TS == "" {
			t.Errorf("line %d: missing ts", i+1)
		}
		if ev.Type != TypeSystem && ev.Type != TypeStreamEvent && ev.Type != TypeAssistant && ev.Type != TypeUser && ev.Type != TypeResult {
			t.Errorf("line %d: unknown type %q (must the corpus use a documented one)", i+1, ev.Type)
		}
	}
	if results != 1 {
		t.Errorf("want exactly one terminal result, got %d", results)
	}
}

// assertCapabilitiesOpenSet pins §9: capabilities is a set of open strings —
// unknown values must survive the typed round-trip (checked above) and
// stream_v1 must be advertised on init.
func assertCapabilitiesOpenSet(t *testing.T, events []*Event) {
	t.Helper()
	init := events[0]
	if len(init.Capabilities) == 0 {
		t.Errorf("system/init must advertise capabilities")
		return
	}
	found := false
	for _, c := range init.Capabilities {
		if c == CapabilityStream {
			found = true
		}
	}
	if !found {
		t.Errorf("system/init.capabilities %v must include %q", init.Capabilities, CapabilityStream)
	}
}

// assertResultContract pins §6.4: the terminal result carries an explicit
// is_error boolean; the subtype enum is open-set (any string allowed here —
// the five known values are just vocabulary).
func assertResultContract(t *testing.T, events []*Event) {
	t.Helper()
	res := events[len(events)-1]
	if res.Type != TypeResult {
		return // assertFraming already reported
	}
	if res.IsError == nil {
		t.Errorf("result must carry explicit is_error")
	}
	if res.Subtype == "" {
		t.Errorf("result must carry subtype")
	}
}

// TestGoldenCorpusCoverage pins the corpus itself: every feature the sprint
// contract promises must be exercised by at least one golden file.
func TestGoldenCorpusCoverage(t *testing.T) {
	type seen struct {
		textDelta, thinkingDelta, toolArgsDelta          bool
		toolUse, toolResult, toolUseResult               bool
		taskStarted, taskProgress, taskNotification      bool
		backgroundTasksChanged, permissionDenied, sysErr bool
		usage, modelUsage, cacheRead, cacheCreation      bool
		routeHistory                                     bool
		nestedParent                                     bool
		resultSubtypes                                   map[string]bool
		capabilities                                     map[string]bool
	}
	s := seen{resultSubtypes: map[string]bool{}, capabilities: map[string]bool{}}

	for _, path := range loadGoldenFiles(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range splitLines(raw) {
			ev, err := Decode(line)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if ev.ParentToolUseID != "" {
				s.nestedParent = true
			}
			for _, c := range ev.Capabilities {
				s.capabilities[c] = true
			}
			if ev.Type == TypeResult {
				s.resultSubtypes[ev.Subtype] = true
				if ev.Usage != nil {
					s.usage = true
					if ev.Usage.CacheRead != 0 {
						s.cacheRead = true
					}
					if ev.Usage.CacheCreation != 0 {
						s.cacheCreation = true
					}
				}
				if len(ev.ModelUsage) > 0 {
					s.modelUsage = true
				}
				if len(ev.RouteHistory) > 0 {
					s.routeHistory = true
				}
			}
			if ev.Type == TypeStreamEvent && ev.Event != nil {
				switch ev.Event.Type {
				case "tool_args_delta":
					s.toolArgsDelta = true
				case "content_block_delta":
					if ev.Event.Delta != nil {
						switch ev.Event.Delta.Type {
						case "text_delta":
							s.textDelta = true
						case "thinking_delta":
							s.thinkingDelta = true
						}
					}
				}
			}
			if ev.Type == TypeAssistant || ev.Type == TypeUser {
				if msg, err := ev.AsMessage(); err == nil && msg != nil {
					for _, b := range msg.Content {
						switch b.Type {
						case "tool_use":
							s.toolUse = true
						case "tool_result":
							s.toolResult = true
						}
					}
				}
			}
			if ev.Type == TypeUser && len(ev.ToolUseResult) > 0 {
				s.toolUseResult = true
			}
			if ev.Type == TypeSystem {
				switch ev.Subtype {
				case SubtypeTaskStarted:
					s.taskStarted = true
				case SubtypeTaskProgress:
					s.taskProgress = true
				case SubtypeTaskNotification:
					s.taskNotification = true
				case SubtypeBackgroundTasksChanged:
					s.backgroundTasksChanged = true
				case SubtypePermissionDenied:
					s.permissionDenied = true
				case SubtypeError:
					s.sysErr = true
				}
			}
		}
	}

	for name, ok := range map[string]bool{
		"text_delta":               s.textDelta,
		"thinking_delta":           s.thinkingDelta,
		"tool_args_delta":          s.toolArgsDelta,
		"tool_use":                 s.toolUse,
		"tool_result":              s.toolResult,
		"tool_use_result":          s.toolUseResult,
		"task_started":             s.taskStarted,
		"task_progress":            s.taskProgress,
		"task_notification":        s.taskNotification,
		"background_tasks_changed": s.backgroundTasksChanged,
		"permission_denied":        s.permissionDenied,
		"system/error":             s.sysErr,
		"result.usage":             s.usage,
		"result.model_usage":       s.modelUsage,
		"usage.cache_read":         s.cacheRead,
		"usage.cache_creation":     s.cacheCreation,
		"result.route_history":     s.routeHistory,
		"parent_tool_use_id":       s.nestedParent,
	} {
		if !ok {
			t.Errorf("golden corpus does not exercise %s", name)
		}
	}
	for _, sub := range []string{
		ResultSuccess, ResultErrorDuringExecution, ResultErrorMaxTurns, ResultInterrupted, ResultRefused,
	} {
		if !s.resultSubtypes[sub] {
			t.Errorf("golden corpus does not exercise result subtype %q", sub)
		}
	}
	if !s.capabilities[CapabilityPartialMessages] {
		t.Errorf("golden corpus does not exercise %s", CapabilityPartialMessages)
	}
}

// knownKeys is the §6/§8.3 modeled vocabulary: every structurally-contracted
// JSON key on the wire. Any key outside this set must live inside an
// extension/_meta reserved field or a free-form payload (tool_use.input,
// tool_use_result, tool_result.content, task) — provider detail never gets its
// own modeled key.
var knownKeys = map[string]bool{
	// envelope
	"type": true, "subtype": true, "session_id": true, "seq": true, "ts": true,
	"parent_tool_use_id": true,
	// system/init
	"cwd": true, "milk_version": true, "agent": true, "escalation_agent": true,
	"tools": true, "mcp_servers": true, "route": true, "session_state": true,
	"warnings": true, "capabilities": true,
	// system lifecycle
	"from": true, "to": true, "reason": true, "id": true, "severity": true,
	"command_hint": true, "body": true, "category": true, "count": true,
	"limit": true, "message": true, "recoverable": true, "tool": true,
	"task_id": true, "kind": true, "status": true, "running_ids": true,
	"finished_ids": true, "op": true, "percept_id": true, "subject": true,
	"commands": true, "option": true, "value": true, "state": true,
	"stop_reason": true, "task": true, "is_error": true,
	// content
	"event": true, "tool_use_result": true, "index": true, "delta": true,
	"tool_use_id": true, "partial_json": true, "text": true, "thinking": true,
	"role": true, "content": true, "input": true,
	// result
	"num_turns": true, "duration_ms": true, "result": true, "route_history": true,
	"usage": true, "model_usage": true, "total_cost_usd": true,
	// nested modeled
	"name": true, "provider": true, "model": true, "context_window_tokens": true,
	"target": true, "conclusive": true, "turn": true, "hint": true,
	"description": true, "input_tokens": true, "output_tokens": true,
	"cache_read": true, "cache_creation": true,
	// reserved extension points
	"extension": true, "_meta": true,
}

// freeFormRoots are wire keys whose subtrees are free-form by contract
// (tool-defined or provider payloads). Keys inside them are unconstrained.
var freeFormRoots = map[string]bool{
	"input": true, "tool_use_result": true, "task": true,
	"extension": true, "_meta": true,
}

// checkConfinement walks every line's raw JSON and rejects any key outside
// knownKeys unless it lives under a free-form root (or under `content` of a
// tool_result block). This is the explicit "provider detail confined to
// extension/_meta" assertion.
func checkConfinement(raw []byte) error {
	for i, line := range splitLines(raw) {
		var v any
		if err := json.Unmarshal(line, &v); err != nil {
			return err
		}
		if err := walkKeys(v, nil, false); err != nil {
			return &lineError{line: i + 1, err: err}
		}
	}
	return nil
}

type lineError struct {
	line int
	err  error
}

func (e *lineError) Error() string { return "line " + strconv.Itoa(e.line) + ": " + e.err.Error() }
func (e *lineError) Unwrap() error { return e.err }

// walkKeys recursively checks keys. In toolResult is true when the current
// object is a tool_result content block (its `content` value is free-form).
func walkKeys(v any, keyPath []string, toolResult bool) error {
	switch t := v.(type) {
	case map[string]any:
		isToolResult := t["type"] == "tool_result"
		for k, sub := range t {
			if !knownKeys[k] {
				return &unknownKeyError{path: strings.Join(append(keyPath, k), "."), key: k}
			}
			if freeFormRoots[k] {
				continue
			}
			if k == "content" && isToolResult {
				continue // tool_result payload is free-form
			}
			if k == "model_usage" {
				// Keys are model names (free-form); values are modeled usage objects.
				models, _ := sub.(map[string]any)
				for model, usage := range models {
					if err := walkKeys(usage, append(keyPath, k, model), false); err != nil {
						return err
					}
				}
				continue
			}
			if err := walkKeys(sub, append(keyPath, k), isToolResult); err != nil {
				return err
			}
		}
	case []any:
		for _, sub := range t {
			if err := walkKeys(sub, keyPath, toolResult); err != nil {
				return err
			}
		}
	}
	return nil
}

type unknownKeyError struct {
	path string
	key  string
}

func (e *unknownKeyError) Error() string {
	return "key " + e.key + " at " + e.path + " is outside the §6 vocabulary — provider detail belongs in extension/_meta (or a free-form payload)"
}

// TestConfinementRejectsProviderDetail proves the confinement check has teeth
// on the three interesting shapes: a stray top-level key, a provider-shaped
// key inside a modeled container, and a legitimate extension payload.
func TestConfinementRejectsProviderDetail(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		wantErr bool
	}{
		{
			name:    "clean line",
			line:    `{"type":"result","subtype":"success","session_id":"s","seq":0,"ts":"t","is_error":false}`,
			wantErr: false,
		},
		{
			name:    "stray top-level provider key",
			line:    `{"type":"assistant","session_id":"s","seq":0,"ts":"t","raw_finish_reason":"stop","message":"x"}`,
			wantErr: true,
		},
		{
			name:    "provider key inside modeled container",
			line:    `{"type":"result","session_id":"s","seq":0,"ts":"t","usage":{"input_tokens":1,"output_tokens":2,"cache_read":3,"cache_creation":4,"cache_read_input_tokens":3}}`,
			wantErr: true,
		},
		{
			name:    "provider detail inside extension",
			line:    `{"type":"assistant","session_id":"s","seq":0,"ts":"t","extension":{"provider":"local","raw_finish_reason":"stop"}}`,
			wantErr: false,
		},
		{
			name:    "provider detail inside _meta",
			line:    `{"type":"system","subtype":"init","session_id":"s","seq":0,"ts":"t","_meta":{"llama_seed":42}}`,
			wantErr: false,
		},
		{
			name:    "tool-defined keys inside input",
			line:    `{"type":"assistant","session_id":"s","seq":0,"ts":"t","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"provider_hint":"x","command":"ls"}}]}}`,
			wantErr: false,
		},
		{
			name:    "tool_result content is free-form",
			line:    `{"type":"user","session_id":"s","seq":0,"ts":"t","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":{"raw":"anything"}}]}}`,
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkConfinement([]byte(tc.line))
			if tc.wantErr && err == nil {
				t.Fatalf("expected confinement error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected confinement error: %v", err)
			}
		})
	}
}

// TestCapabilitiesOpenSetUnknownPreserved pins that an unrecognized capability
// string survives decode → encode untouched (open-set contract §9).
func TestCapabilitiesOpenSetUnknownPreserved(t *testing.T) {
	line := `{"type":"system","subtype":"init","session_id":"s","seq":0,"ts":"t","capabilities":["stream_v1","future_capability_v9"]}`
	ev, err := Decode([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	re, err := EncodeLine(ev)
	if err != nil {
		t.Fatal(err)
	}
	var fromWire, fromTyped any
	if err := json.Unmarshal([]byte(line), &fromWire); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(re, &fromTyped); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromWire, fromTyped) {
		t.Fatalf("unknown capability not preserved: %s", re)
	}
}
