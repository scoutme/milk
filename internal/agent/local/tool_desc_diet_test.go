package local

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/contextreduce"
)

// dietSampleTools mirrors the OpenAI tool shape with a nested parameter
// description (object property + array item) so the lean pass is exercised
// below the top level.
func dietSampleTools() []map[string]any {
	return []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "alpha",
				"description": "First sentence. Second sentence that must not survive compaction.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"cmd": map[string]any{
							"type":        "string",
							"description": "The command to run.",
						},
						"cfg": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"deep": map[string]any{"type": "string", "description": "nested param doc"},
							},
							"description": "outer param doc",
						},
						"ids": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string", "description": "item doc"},
							"description": "list param doc",
						},
					},
					"required": []string{"cmd"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "beta",
				"description": "Beta does things.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
	}
}

func dietNames(tools []map[string]any) []string {
	var out []string
	for _, t := range tools {
		if fn, ok := t["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok {
				out = append(out, n)
			}
		}
	}
	return out
}

// collectDescriptions gathers every "description" value under a schema
// fragment (for the lean pass: none must remain inside parameters).
func collectDescriptions(v any, into *[]string) {
	switch n := v.(type) {
	case map[string]any:
		if d, ok := n["description"].(string); ok {
			*into = append(*into, d)
		}
		for _, child := range n {
			collectDescriptions(child, into)
		}
	case []any:
		for _, child := range n {
			collectDescriptions(child, into)
		}
	}
}

func TestApplyToolDescDiet_FullIsStatusQuo(t *testing.T) {
	in := dietSampleTools()
	frozen, _ := json.Marshal(in)

	for _, tier := range []string{"", "full", "FULL", " unknown-typo "} {
		got := applyToolDescDiet(in, tier)
		if !reflect.DeepEqual(got, in) {
			t.Errorf("tier %q mutated the tool list — diet must be byte-for-byte status quo", tier)
		}
		if b, _ := json.Marshal(got); string(b) != string(frozen) {
			t.Errorf("tier %q changed serialized bytes", tier)
		}
	}
}

func TestApplyToolDescDiet_CompactKeepsFirstSentenceOnly(t *testing.T) {
	in := dietSampleTools()
	frozen, _ := json.Marshal(in)

	got := applyToolDescDiet(in, "compact")

	// Input never mutated (deep copy): a later full-tier consumer sharing
	// the same maps (e.g. MCP-cached schemas) must be unaffected.
	if b, _ := json.Marshal(in); string(b) != string(frozen) {
		t.Fatalf("compact diet mutated its input")
	}
	fn, _ := got[0]["function"].(map[string]any)
	want := contextreduce.CompactToolDescription("First sentence. Second sentence that must not survive compaction.")
	if fn["description"] != want {
		t.Errorf("alpha description = %v, want compact %q", fn["description"], want)
	}
	if s, _ := fn["description"].(string); strings.Contains(s, "Second sentence") {
		t.Errorf("second sentence survived compaction: %q", s)
	}
	// Parameter descriptions are NOT part of compact (they are part of lean):
	params, _ := fn["parameters"].(map[string]any)
	var descs []string
	collectDescriptions(params, &descs)
	if len(descs) == 0 {
		t.Errorf("compact diet dropped parameter descriptions — that is lean's job")
	}
}

func TestApplyToolDescDiet_LeanDropsParameterDescriptions(t *testing.T) {
	in := dietSampleTools()
	frozen, _ := json.Marshal(in)

	got := applyToolDescDiet(in, "lean")
	if b, _ := json.Marshal(in); string(b) != string(frozen) {
		t.Fatalf("lean diet mutated its input")
	}

	if len(got) != len(in) {
		t.Fatalf("lean changed tool count: %d → %d", len(in), len(got))
	}
	for i, tool := range got {
		if !reflect.DeepEqual(dietNames([]map[string]any{tool}), dietNames([]map[string]any{in[i]})) {
			t.Errorf("tool %d renamed/reordered", i)
		}
		fn, _ := tool["function"].(map[string]any)
		if fn["description"] == "" {
			t.Errorf("tool %d lost its function description", i)
		}
		params, _ := fn["parameters"].(map[string]any)
		var descs []string
		collectDescriptions(params, &descs)
		if len(descs) != 0 {
			t.Errorf("tool %d still carries parameter descriptions: %v", i, descs)
		}
		// Structure survives: type/properties/required intact.
		if params["type"] != "object" {
			t.Errorf("tool %d lost parameters.type", i)
		}
		if _, ok := params["properties"]; !ok {
			t.Errorf("tool %d lost parameters.properties", i)
		}
	}
	// required survives with its entries.
	fn, _ := got[0]["function"].(map[string]any)
	params, _ := fn["parameters"].(map[string]any)
	if req, ok := params["required"].([]string); !ok || len(req) != 1 || req[0] != "cmd" {
		t.Errorf("required = %#v, want [cmd]", params["required"])
	}
}

// TestToolDescDiet_LiveSizes pins the measured payload effect on the real
// built-in tool set and the budget ceilings (issue #462). Full is the
// status quo (measured 15,064 bytes for the 21-schema no-store set at
// 2026-11-25); compact/lean ceilings leave ~10% headroom for benign
// description edits — raising them should be a conscious decision, not a
// side effect.
func TestToolDescDiet_LiveSizes(t *testing.T) {
	base := schemas(nil, "", nil, nil, nil, nil)
	size := func(tier string) int {
		b, err := json.Marshal(applyToolDescDiet(base, tier))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return len(b)
	}
	full := size("")
	compact := size("compact")
	lean := size("lean")

	const fullCeiling, compactCeiling, leanCeiling = 16_000, 12_500, 9_500
	if full > fullCeiling {
		t.Errorf("full tool payload = %d bytes > budget %d — the default grew; raise consciously with a diet offset", full, fullCeiling)
	}
	if compact > compactCeiling {
		t.Errorf("compact tool payload = %d bytes > budget %d", compact, compactCeiling)
	}
	if lean > leanCeiling {
		t.Errorf("lean tool payload = %d bytes > budget %d", lean, leanCeiling)
	}
	if !(lean < compact && compact < full) {
		t.Errorf("tiers must shrink monotonically: full=%d compact=%d lean=%d", full, compact, lean)
	}
	if lean*10 > full*6 {
		t.Errorf("lean = %d bytes is >60%% of full %d — the diet no longer moves the payload", lean, full)
	}
	t.Logf("tools payload: full=%d compact=%d lean=%d bytes (lean is %.0f%% of full)", full, compact, lean, 100*float64(lean)/float64(full))

	// Names and order identical across tiers — a diet never reorders tools
	// (cache-stability + model muscle memory, per issue #462).
	baseNames := dietNames(base)
	for _, tier := range []string{"compact", "lean"} {
		if got := dietNames(applyToolDescDiet(base, tier)); !reflect.DeepEqual(got, baseNames) {
			t.Errorf("tier %q changed tool names/order: %v → %v", tier, baseNames, got)
		}
	}
}

// TestSystemPrompt_Budget guards the other half of the stable prefix: the
// standard-tier system prompt (measured 6,393 bytes for a cwd without
// project instructions, 2026-11-25).
func TestSystemPrompt_Budget(t *testing.T) {
	standard := buildSystemPromptOpts("", "milk", "mimo-pro-local", false, "standard", false, 0, false)
	if len(standard) > 7_500 {
		t.Errorf("standard system prompt = %d bytes > budget 7500", len(standard))
	}
	minimal := buildSystemPromptOpts("", "milk", "mimo-pro-local", false, "minimal", false, 0, false)
	if len(minimal) > 600 {
		t.Errorf("minimal system prompt = %d bytes > budget 600", len(minimal))
	}
}

// TestFirstPayload_RecordsOnce covers the meter itself: recorded on first
// assembly, sticky afterwards, absent for agents without a slot.
func TestFirstPayload_RecordsOnce(t *testing.T) {
	agent := NewFromConfig(config.AgentConfig{
		Name: "diet-agent", Provider: "local", URL: "http://127.0.0.1:1",
		ToolDescriptionTier: "lean",
	})
	tools := applyToolDescDiet(dietSampleTools(), agent.toolDescTier)
	msgs := []Message{
		{Role: "system", Content: "sys prompt"},
		{Role: "user", Content: "hello"},
	}
	agent.noteFirstPayload("sys prompt", msgs, tools)

	p, ok := agent.FirstPayload()
	if !ok {
		t.Fatalf("FirstPayload not recorded")
	}
	if p.SystemBytes != int64(len("sys prompt")) {
		t.Errorf("SystemBytes = %d, want %d", p.SystemBytes, len("sys prompt"))
	}
	if p.MessagesBytes != int64(len("sys prompt")+len("hello")) {
		t.Errorf("MessagesBytes = %d, want %d", p.MessagesBytes, len("sys prompt")+len("hello"))
	}
	if p.Diet != "lean" {
		t.Errorf("Diet = %q, want lean", p.Diet)
	}
	if p.ToolsBytes <= 0 {
		t.Errorf("ToolsBytes = %d, want > 0", p.ToolsBytes)
	}

	// Sticky: a later assembly (tool loop iteration, memory-block
	// re-injection) must not move the meter.
	agent.noteFirstPayload("a much longer second system prompt", msgs, tools)
	again, _ := agent.FirstPayload()
	if again != p {
		t.Errorf("payload moved after first record: %+v → %+v", p, again)
	}

	// Bare agents (tests, providers without a slot) report none.
	bare := &Agent{}
	if _, ok := bare.FirstPayload(); ok {
		t.Errorf("bare agent reported a payload")
	}
}
