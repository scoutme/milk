package local

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/contextreduce"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
)

// ---- fixtures ----

// tsFullEntry builds a full (pre-diet) tool entry in the OpenAI shape.
func tsFullEntry(name, desc string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": desc,
			"parameters":  params,
		},
	}
}

// tsStringParam is a one-required-string-parameter schema.
func tsStringParam(field string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{field: map[string]any{"type": "string"}},
		"required":   []string{field},
	}
}

func tsAgent() *Agent {
	return &Agent{toolSearchMode: "auto", toolSearch: newToolSearchStore()}
}

func tsWireNames(wire []map[string]any) []string {
	out := make([]string, 0, len(wire))
	for _, e := range wire {
		out = append(out, toolName(e))
	}
	return out
}

// tsToolResultText unwraps a toolResult-JSON payload to its plain
// output/error text (runToolSearch and the intercept refusals deliver their
// text inside toolResult JSON).
func tsToolResultText(t *testing.T, s string) string {
	t.Helper()
	var r toolResult
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return s // plain text result (e.g. "No matching tools found.")
	}
	return r.Output + r.Error
}

// ---- (a) ranker ----

func TestToolSearchRankExactNameFirst(t *testing.T) {
	docs := []toolSearchDoc{
		{name: "thread_history", text: "thread read history of threads reads reading every thread"},
		{name: "mcp__app__thread_read", text: "zzz unrelated body"},
	}
	// The exact name (full `__`-qualified) pins first even though the other
	// document covers the same terms with far more text hits.
	got := rankToolSearchDocs("thread_read", docs)
	if len(got) != 2 || got[0] != "mcp__app__thread_read" {
		t.Fatalf("exact __-suffix name must pin first, got %v", got)
	}
	// The fully qualified query pins the same way.
	got = rankToolSearchDocs("mcp__app__thread_read", docs)
	if len(got) != 2 || got[0] != "mcp__app__thread_read" {
		t.Fatalf("exact full name must pin first, got %v", got)
	}
}

func TestToolSearchRankCoverageBeatsScore(t *testing.T) {
	// "alpha" fires the name weight plus fifty body mentions (huge raw
	// score for one term), "zeta" matches both query terms weakly.
	var body strings.Builder
	for i := 0; i < 50; i++ {
		body.WriteString("alpha ")
	}
	docs := []toolSearchDoc{
		{name: "alpha", text: body.String()},
		{name: "zeta", text: "alpha beta"},
	}
	got := rankToolSearchDocs("alpha beta", docs)
	if len(got) != 2 || got[0] != "zeta" {
		t.Fatalf("two-term coverage must beat one-term score, got %v", got)
	}
}

func TestToolSearchRankLocalNameBonus(t *testing.T) {
	// Identical name-token weights and bodies; only the `__`-split local
	// name of the second doc is fully contained in the query. It must win.
	// Input order puts the non-bonus doc first so a tie would keep it first.
	docs := []toolSearchDoc{
		{name: "app__alpha_beta_x", text: "filler"},
		{name: "app__alpha_beta", text: "filler"},
	}
	got := rankToolSearchDocs("alpha beta", docs)
	if len(got) != 2 || got[0] != "app__alpha_beta" {
		t.Fatalf("__-split local-name bonus must break the tie, got %v", got)
	}
}

func TestToolSearchRankNameWeightBeatsBodyMentions(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 10; i++ {
		body.WriteString("gamma ")
	}
	docs := []toolSearchDoc{
		{name: "delta", text: body.String()},
		{name: "gamma_ray", text: "zz"},
	}
	got := rankToolSearchDocs("gamma", docs)
	if len(got) != 2 || got[0] != "gamma_ray" {
		t.Fatalf("name-field match must beat body mentions, got %v", got)
	}
}

func TestToolSearchRankStableTies(t *testing.T) {
	docs := []toolSearchDoc{
		{name: "one__alpha", text: "alpha stuff"},
		{name: "two__alpha", text: "alpha stuff"},
	}
	got := rankToolSearchDocs("alpha", docs)
	if len(got) != 2 || got[0] != "one__alpha" || got[1] != "two__alpha" {
		t.Fatalf("ties must keep document order, got %v", got)
	}
	rev := []toolSearchDoc{docs[1], docs[0]}
	got = rankToolSearchDocs("alpha", rev)
	if len(got) != 2 || got[0] != "two__alpha" || got[1] != "one__alpha" {
		t.Fatalf("ties must follow the input order, got %v", got)
	}
}

// ---- (b) stubs ----

func TestToolSearchStubBudget(t *testing.T) {
	long := "First sentence here is quite long and keeps going with many words " +
		"to push well past the one hundred twenty byte excerpt budget for sure " +
		"and then some more words beyond it. Second sentence never survives."
	full := tsFullEntry("mcp__srv__thing", long, tsStringParam("q"))
	stub := toolSearchStubEntry("mcp__srv__thing", full)
	fn := stub["function"].(map[string]any)
	desc := fn["description"].(string)
	excerpt := strings.TrimSuffix(desc, stubSuffix("mcp__srv__thing"))
	if n := len([]byte(excerpt)); n > toolSearchStubDescMax {
		t.Errorf("excerpt is %d bytes, cap is %d: %q", n, toolSearchStubDescMax, excerpt)
	}
	if !strings.HasSuffix(desc, stubSuffix("mcp__srv__thing")) {
		t.Errorf("stub description must end with the fixed directive, got %q", desc)
	}
	if strings.Contains(desc, "Second sentence") {
		t.Errorf("stub excerpt must not carry later sentences: %q", desc)
	}
	b, err := json.Marshal(stub)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > toolSearchStubEntryMax {
		t.Errorf("stub entry is %d bytes, cap is %d: %s", len(b), toolSearchStubEntryMax, b)
	}
	// Open schema: no required, additionalProperties true, empty properties.
	params := fn["parameters"].(map[string]any)
	if params["additionalProperties"] != true {
		t.Errorf("stub schema must declare additionalProperties: true, got %v", params["additionalProperties"])
	}
	if _, ok := params["required"]; ok {
		t.Errorf("stub schema must not declare required parameters: %v", params)
	}
	if props, ok := params["properties"].(map[string]any); !ok || len(props) != 0 {
		t.Errorf("stub schema properties must be empty, got %v", params["properties"])
	}
}

func TestToolSearchStubOrderCapAndNegativeCap(t *testing.T) {
	entries := []map[string]any{
		tsFullEntry("bash", "Execute a shell command.", nil), // core-pinned
		tsFullEntry("mcp__zeta__z", "Zeta tool does zed things.", nil),
		tsFullEntry("mcp__alpha__a", "Alpha tool does a things.", nil),
		tsFullEntry("agent_helper", "A helpful peer agent.", nil),
	}
	a := tsAgent()
	wire, rt := a.applyToolSearch(entries, entries)
	got := tsWireNames(wire)
	want := []string{"bash", "tool_search", "agent_helper", "mcp__alpha__a", "mcp__zeta__z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wire order = %v, want %v", got, want)
	}
	if len(rt.deferred) != 3 {
		t.Fatalf("all deferred tools must stay searchable, got %d", len(rt.deferred))
	}
	// Deterministic: a second assembly produces byte-identical wire JSON.
	a2 := tsAgent()
	wire2, _ := a2.applyToolSearch(entries, entries)
	j1, _ := json.Marshal(wire)
	j2, _ := json.Marshal(wire2)
	if string(j1) != string(j2) {
		t.Error("wire assembly must be deterministic")
	}

	// Cap: only the first stub-cap (name-sorted) deferred tools are stubbed.
	ac := &Agent{toolSearchMode: "auto", toolSearchStubCap: 1, toolSearch: newToolSearchStore()}
	wirec, rtc := ac.applyToolSearch(entries, entries)
	if got := tsWireNames(wirec); !reflect.DeepEqual(got, []string{"bash", "tool_search", "agent_helper"}) {
		t.Fatalf("stub cap 1 wire = %v", got)
	}
	if len(rtc.deferred) != 3 {
		t.Errorf("cap must not shrink the search index, got %d deferred", len(rtc.deferred))
	}
	if stubs, _ := ac.toolSearch.counts(); stubs != 1 {
		t.Errorf("store stub count = %d, want 1", stubs)
	}

	// Negative cap: no stubs at all, everything stays discoverable.
	an := &Agent{toolSearchMode: "auto", toolSearchStubCap: -1, toolSearch: newToolSearchStore()}
	wiren, rtn := an.applyToolSearch(entries, entries)
	if got := tsWireNames(wiren); !reflect.DeepEqual(got, []string{"bash", "tool_search"}) {
		t.Fatalf("negative cap must leave no stubs, got %v", got)
	}
	if len(rtn.deferred) != 3 {
		t.Errorf("negative cap must keep the search index, got %d deferred", len(rtn.deferred))
	}

	// Cap resolution: 0 → default, negative → none, positive → exact.
	if got := effectiveToolSearchStubCap(0); got != config.DefaultToolSearchStubCap {
		t.Errorf("cap 0 resolved to %d, want default %d", got, config.DefaultToolSearchStubCap)
	}
	if got := effectiveToolSearchStubCap(-3); got != 0 {
		t.Errorf("negative cap resolved to %d, want 0", got)
	}
	if got := effectiveToolSearchStubCap(7); got != 7 {
		t.Errorf("cap 7 resolved to %d, want 7", got)
	}
}

// ---- (c) pin-drift guard ----

// TestPinDriftGuard scans every prompt rule text for registered tool names
// (word-boundary matches over the full registered-name universe) and asserts
// each referenced tool is both registered and pinned: a rule must never
// mandate a tool the model would have to tool_search for first. When a rule
// starts referencing a new tool, add it to rulePinnedTools deliberately (see
// the escalate/milk_config_help entries) — this test fails until you do.
func TestPinDriftGuard(t *testing.T) {
	texts := map[string]string{
		"systemPromptRulesCore":          systemPromptRulesCore,
		"systemPromptToolboxMeta":        systemPromptToolboxMeta,
		"systemPromptActFirst":           systemPromptActFirst,
		"systemPromptToolboxProtocols":   systemPromptToolboxProtocols,
		"systemPromptSharedFull":         systemPromptSharedFull,
		"primaryRoleTail":                primaryRoleTail,
		"escalationOwnedRoleTail":        escalationOwnedRoleTail,
		"escalationHandoffRoleTail":      escalationHandoffRoleTail,
		"boardToolGuidance":              boardToolGuidance,
		"taskToolGuidance":               taskToolGuidance,
		"memoryToolGuidance":             memoryToolGuidance,
		"backgroundAgentGuidance":        backgroundAgentGuidance,
		"backgroundPeerGuidance":         backgroundPeerGuidance,
		"projectMapGuidance":             projectMapGuidance,
		"openTasksReminder (fake store)": openTasksReminder(fakeTaskStore{{ID: "a1", Title: "t", Status: "pending"}}),
	}

	// Registered-name universe: built-ins with every optional surface wired
	// (memory, otel, tasks, tool agents) plus board and background tools.
	mem, err := memory.NewStore(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	universe := map[string]bool{}
	for _, e := range schemas(mem, t.TempDir(), &session.Session{CWD: "."},
		[]config.AgentToolEntry{{Agent: "peer", Description: "d"}},
		fakeTaskStore{{ID: "a1", Title: "t", Status: "pending"}}, nil) {
		universe[toolName(e)] = true
	}
	for _, n := range boardToolNames {
		universe[n] = true
	}
	for _, e := range []map[string]any{
		spawnBackgroundAgentSchema(nil), cancelBackgroundAgentSchema(),
		statusBackgroundAgentSchema(), waitBackgroundAgentSchema(),
		sendBackgroundAgentSchema(),
	} {
		universe[toolName(e)] = true
	}

	pin := toolSearchPinSet(nil)
	referenced := 0
	for textName, text := range texts {
		for tool := range universe {
			if !regexp.MustCompile(`\b` + tool + `\b`).MatchString(text) {
				continue
			}
			referenced++
			if !universe[tool] {
				t.Errorf("%s references %q which is not registered", textName, tool)
			}
			if !pin[tool] {
				t.Errorf("%s references %q which is not pinned — add it to rulePinnedTools (or corePinnedTools) deliberately", textName, tool)
			}
		}
	}
	if referenced == 0 {
		t.Fatal("guard matched nothing — the universe or the texts are wrong")
	}
}

// ---- (d) guess-safe matrix ----

func TestToolSearchInterceptMatrix(t *testing.T) {
	mk := func() (*Agent, *toolSearchRuntime) {
		entries := []map[string]any{
			tsFullEntry("web_fetch", "Fetch a web page over the network.", tsStringParam("url")),
			tsFullEntry("delete_file", "Delete a file from disk.", tsStringParam("path")),
			tsFullEntry("mcp__srv__widget", "Frobnicates widgets on the frob service.", tsStringParam("id")),
			tsFullEntry("mcp__s2__gadget", "Gadget inspection helper.", tsStringParam("id")),
		}
		a := tsAgent()
		_, rt := a.applyToolSearch(entries, entries)
		return a, rt
	}
	ctx := context.Background()

	// 1. Guess-safe deferred call with valid args executes and marks loaded.
	a, rt := mk()
	out, handled := a.interceptToolSearch(ctx, rt, toolCall{ID: "c1", Function: toolCallFunction{Name: "web_fetch", Arguments: `{"url":"https://example.com"}`}})
	if handled {
		t.Fatalf("guess-safe valid call must fall through to normal dispatch, got %+v", out)
	}
	if !rt.store.has("web_fetch") {
		t.Error("guess-safe valid call must mark the tool loaded")
	}

	// 2. Unsafe unloaded call is refused with the prescribed text.
	out, handled = a.interceptToolSearch(ctx, rt, toolCall{ID: "c2", Function: toolCallFunction{Name: "delete_file", Arguments: `{"path":"x"}`}})
	if !handled {
		t.Fatal("unsafe unloaded call must be intercepted")
	}
	want := "delete_file is deferred and state-changing; run tool_search('delete_file') and re-call with the documented semantics."
	if !strings.Contains(tsToolResultText(t, out.msg.Content), want) {
		t.Errorf("refusal text = %q, want it to contain %q", out.msg.Content, want)
	}
	if rt.store.has("delete_file") {
		t.Error("a refused call must not mark the tool loaded")
	}

	// 3. Unsafe but loaded call executes normally.
	rt.store.markLoaded("delete_file")
	if _, handled := a.interceptToolSearch(ctx, rt, toolCall{ID: "c3", Function: toolCallFunction{Name: "delete_file", Arguments: `{"path":"x"}`}}); handled {
		t.Error("loaded tools must dispatch normally")
	}

	// 4. Guess-safe call with invalid args → tier'd schema echo + fix hint.
	a2, rt2 := mk()
	out, handled = a2.interceptToolSearch(ctx, rt2, toolCall{ID: "c4", Function: toolCallFunction{Name: "web_fetch", Arguments: `{"bogus":1}`}})
	if !handled {
		t.Fatal("validation failure must be intercepted")
	}
	echo := tsToolResultText(t, out.msg.Content)
	if !strings.Contains(echo, `"name":"web_fetch"`) || !strings.Contains(echo, "fix the arguments and call again") {
		t.Errorf("schema echo = %q, want tier'd schema plus fix hint", echo)
	}
	if !strings.Contains(echo, "missing required parameter") && !strings.Contains(echo, "unknown parameter") {
		t.Errorf("schema echo must name the validation problem, got %q", echo)
	}
	if rt2.store.has("web_fetch") {
		t.Error("a failed validation must not mark the tool loaded")
	}

	// 5. tool_search itself: result format, marks loaded, limit defaults.
	a3, rt3 := mk()
	out, handled = a3.interceptToolSearch(ctx, rt3, toolCall{ID: "c5", Function: toolCallFunction{Name: "tool_search", Arguments: `{"query":"widget"}`}})
	if !handled {
		t.Fatal("tool_search must be intercepted")
	}
	result := tsToolResultText(t, out.msg.Content)
	if !strings.HasPrefix(result, "Loaded 1 tools:") || !strings.Contains(result, "mcp__srv__widget") {
		t.Errorf("tool_search result = %q, want \"Loaded 1 tools:\" and the match", result)
	}
	if !strings.Contains(result, "schema: ") {
		t.Errorf("tool_search result must deliver the schema as tool text, got %q", result)
	}
	if !rt3.store.has("mcp__srv__widget") {
		t.Error("tool_search results must be marked loaded")
	}
	// Nothing found is never an error.
	if got := a3.runToolSearch(ctx, rt3, `{"query":"qqqq nonmatching"}`); got != "No matching tools found." {
		t.Errorf("empty result = %q", got)
	}

	// Limits: default 5, cap 20.
	big := make([]map[string]any, 0, 25)
	for i := 0; i < 25; i++ {
		big = append(big, tsFullEntry(fmt.Sprintf("mcp__srv__widget%02d", i), "Frobnicates widgets on the frob service.", tsStringParam("id")))
	}
	a4 := tsAgent()
	_, rt4 := a4.applyToolSearch(big, big)
	got := tsToolResultText(t, a4.runToolSearch(ctx, rt4, `{"query":"widget"}`))
	if !strings.HasPrefix(got, "Loaded 5 tools:") {
		t.Errorf("default limit result = %.60q, want 5", got)
	}
	got = tsToolResultText(t, a4.runToolSearch(ctx, rt4, `{"query":"widget","limit":100}`))
	if !strings.HasPrefix(got, "Loaded 20 tools:") {
		t.Errorf("limit cap result = %.60q, want 20", got)
	}
	got = tsToolResultText(t, a4.runToolSearch(ctx, rt4, `{"query":"widget","limit":3}`))
	if !strings.HasPrefix(got, "Loaded 3 tools:") || !strings.Contains(got, "Also matched:") {
		t.Errorf("limit 3 result = %.60q", got)
	}
}

// TestToolSearchPromptLineByteExact pins the sysPromptBuilder seam
// (reduction.go): the wire assembly's prompt line is rendered inside the
// level < LevelInstructions gate, and a re-render from the shared store —
// what ReshapeCacheWarm does after a settled turn — reproduces it
// byte-exactly. With no line set, render output is unchanged.
func TestToolSearchPromptLineByteExact(t *testing.T) {
	line := "7 tools deferred (MCP, plugins, rarely used). Use tool_search(query) to load schemas; all deferred tools are callable once their schema is known."
	sess := &session.Session{CWD: "."}
	entries := []map[string]any{tsFullEntry("bash", "Execute a shell command.", nil)}

	a := tsAgent()
	// The system-prompt seam is the classic layout's placement (stable
	// rides the line in the <milk-context> block — see the check below).
	a.promptLayoutClassic = true
	a.toolSearch.setWireInfo(line, 7)
	got := a.newSystemPromptBuilder(context.Background(), sess, entries).render(contextreduce.LevelNone, contextreduce.DefaultPolicy())
	if !strings.Contains(got, line) {
		t.Fatalf("LevelNone render missing the tool_search line: %.200q", got)
	}
	// ReshapeCacheWarm's path: a fresh builder over the shared store must
	// render byte-identically.
	again := a.newSystemPromptBuilder(context.Background(), sess, entries).render(contextreduce.LevelNone, contextreduce.DefaultPolicy())
	if got != again {
		t.Error("re-render from the shared store must reproduce the prompt byte-exactly")
	}
	// The line rides the guidance layers and is shed with them under pressure.
	dropped := a.newSystemPromptBuilder(context.Background(), sess, entries).render(contextreduce.LevelInstructions, contextreduce.DefaultPolicy())
	if strings.Contains(dropped, line) {
		t.Error("LevelInstructions render must shed the tool_search line with the guidance layers")
	}
	// Never set (off / nothing deferred): zero bytes change.
	b := &Agent{toolSearchMode: "off", toolSearch: newToolSearchStore()}
	plain := b.newSystemPromptBuilder(context.Background(), sess, entries).render(contextreduce.LevelNone, contextreduce.DefaultPolicy())
	if strings.Contains(plain, "tool_search") {
		t.Error("an unset wire line must not leak tool_search text")
	}
	// Stable layout (tsAgent's default): the line lives in the
	// <milk-context> block only — the system prompt must stay
	// byte-constant regardless of tool_search, and the policy must not
	// appear twice in one request.
	c := tsAgent()
	c.toolSearch.setWireInfo(line, 7)
	stable := c.newSystemPromptBuilder(context.Background(), sess, entries).render(contextreduce.LevelNone, contextreduce.DefaultPolicy())
	if strings.Contains(stable, line) {
		t.Error("stable layout must not put the tool_search line in the system prompt")
	}
	if got := turnContextBlock("TURN", c.toolSearch.promptLine()); !strings.Contains(got, line) {
		t.Error("stable layout's <milk-context> block must carry the tool_search line")
	}
}

// ---- (f) wire ceiling ----

// tsRichParams builds a realistic MCP-style parameter schema: N named string
// params, each with a description, one with an enum. Descriptions disappear
// under the lean diet; names, types and enums survive.
func tsRichParams(n int) map[string]any {
	props := map[string]any{}
	for i := 0; i < n; i++ {
		p := map[string]any{"type": "string", "description": fmt.Sprintf("Parameter number %d of the call: what it controls and how it interacts with the rest of the request payload.", i)}
		if i == 0 {
			p["enum"] = []string{"create", "update", "delete"}
		}
		props[fmt.Sprintf("arg%02d", i)] = p
	}
	return map[string]any{"type": "object", "properties": props, "required": []string{"arg00"}}
}

// tsWireFixture is the wire-ceiling fixture: the built-in tool surface plus
// three fake MCP tools (rich schemas, the realistic deferred bulk) and three
// fake agent_ tools.
func tsWireFixture() []map[string]any {
	builtIns := schemas(nil, "", &session.Session{CWD: "."}, nil, nil, nil)
	out := append([]map[string]any{}, builtIns...)
	for i := 0; i < 3; i++ {
		out = append(out, tsFullEntry(fmt.Sprintf("mcp__srv%02d__manage_widget", i),
			fmt.Sprintf("Manages widgets on service %d through its admin API. Creates, updates and retires widget instances with full lifecycle control over their configuration.", i),
			tsRichParams(12)))
	}
	for i := 0; i < 3; i++ {
		out = append(out, tsFullEntry(fmt.Sprintf("agent_peer%02d", i),
			fmt.Sprintf("Delegates a self-contained research request to peer agent %02d, a differently-equipped helper agent.", i),
			tsStringParam("request")))
	}
	return out
}

func tsWireBytes(t *testing.T, tools []map[string]any) int {
	t.Helper()
	b, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

// TestToolSearchWireCeiling pins the payload win: with tool_search "auto" the
// total wire must stay ≤ 70% of the full-schema wire at full tier. Under the
// lean diet the full schemas are already so small that stubs only save a
// little — auto still must not exceed full (the small delta is logged).
func TestToolSearchWireCeiling(t *testing.T) {
	fixture := tsWireFixture()

	fullWire := applyToolDescDiet(fixture, "")
	a := tsAgent()
	autoWire, rt := a.applyToolSearch(fullWire, fixture)
	if rt == nil {
		t.Fatal("fixture must produce deferred tools")
	}
	fullBytes := tsWireBytes(t, fullWire)
	autoBytes := tsWireBytes(t, autoWire)
	t.Logf("full tier: full=%d B auto=%d B (%.0f%%, %d deferred)", fullBytes, autoBytes, 100*float64(autoBytes)/float64(fullBytes), len(rt.deferred))
	if autoBytes*100 > fullBytes*70 {
		t.Errorf("auto wire is %.0f%% of full at full tier, ceiling is 70%% (%d vs %d B)", 100*float64(autoBytes)/float64(fullBytes), autoBytes, fullBytes)
	}

	// Lean: the diet already shrank every schema, so the stub win is small —
	// measured here at ~1% (stubs ≈ already-lean schemas; for small built-ins
	// the stub is actually a bit bigger and the rich MCP fakes carry the
	// saving). Auto must never exceed full at lean; the delta is logged so a
	// regression that flips the sign is visible.
	fullLean := applyToolDescDiet(fixture, "lean")
	alean := tsAgent()
	autoLean, _ := alean.applyToolSearch(fullLean, fixture)
	fullLeanBytes := tsWireBytes(t, fullLean)
	autoLeanBytes := tsWireBytes(t, autoLean)
	t.Logf("lean tier: full=%d B auto=%d B (%.0f%%)", fullLeanBytes, autoLeanBytes, 100*float64(autoLeanBytes)/float64(fullLeanBytes))
	if autoLeanBytes > fullLeanBytes {
		t.Errorf("auto wire exceeds full wire at lean tier: %d > %d B", autoLeanBytes, fullLeanBytes)
	}
}

// ---- (e) off path: zero byte changes ----

func TestToolSearchOffPathUnchanged(t *testing.T) {
	entries := []map[string]any{
		tsFullEntry("bash", "Execute a shell command.", nil),
		tsFullEntry("mcp__srv__thing", "A deferred tool.", nil),
	}
	// applyToolSearch is inert when the mode is off: same slice out, nil rt.
	for _, mode := range []string{"", "off", "OFF"} {
		a := &Agent{toolSearchMode: mode, toolSearch: newToolSearchStore()}
		out, rt := a.applyToolSearch(entries, entries)
		if rt != nil {
			t.Fatalf("mode %q: runtime must be nil", mode)
		}
		if len(out) != len(entries) || (len(out) > 0 && &out[0] != &entries[0]) {
			t.Fatalf("mode %q: wire must pass through untouched", mode)
		}
	}

	// System prompt output is unchanged for role × tier combos: it equals the
	// pre-feature concatenation (custom + buildSystemPromptOpts + guidance —
	// no guidance here) and carries no tool_search bytes. The layout knob
	// can't change the system prompt, so one pass covers both layouts; the
	// classic/stable split's user-block side is asserted below.
	roles := []struct {
		name     string
		esc      string
		workflow bool
	}{
		{"primary", "", false},
		{"escalation", "haiku", false},
		{"workflow", "", true},
	}
	sess := &session.Session{CWD: "."}
	for _, role := range roles {
		for _, tier := range []string{"minimal", "standard", "full", "lean"} {
			a := &Agent{toolSearchMode: "off", toolSearch: newToolSearchStore(), systemPromptTier: tier, escalationName: role.esc, workflowRole: role.workflow}
			spb := a.newSystemPromptBuilder(context.Background(), sess, entries)
			got := spb.render(contextreduce.LevelNone, contextreduce.DefaultPolicy())
			owned := a.ownsConversationFromStart(sess)
			want := buildSystemPromptOpts(sess.CWD, a.selfName, role.esc, role.workflow, tier, false, 0, owned)
			if got != want {
				t.Errorf("%s/%s: mode-off system prompt diverged from the baseline\n got: %.200q\nwant: %.200q", role.name, tier, got, want)
			}
			if strings.Contains(got, "tool_search") || strings.Contains(got, "deferred") {
				t.Errorf("%s/%s: mode-off prompt leaked tool_search text", role.name, tier)
			}
		}
	}

	// Stable layout: an empty tool-search line contributes zero bytes to both
	// the full and the reduced <milk-context> block paths.
	if turnContextBlock("PM", "TURN", "") != turnContextBlock("PM", "TURN") {
		t.Error("empty tool-search line must not change the full context block")
	}
	if turnContextBlock("TURN", "") != turnContextBlock("TURN") {
		t.Error("empty tool-search line must not change the reduced context block")
	}
}
