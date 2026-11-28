package local

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/obs"
)

// tool_search — deferred tool schemas with on-demand loading (issue #480,
// design/tool-search:docs/internal/design/tool-search-design.md §3).
//
// Wire shape (config tool_search: "auto"): the tools array becomes three
// layers and never changes within a session —
//
//  1. pinned tools keep their full schemas (subject to tool_description_tier):
//     the core coding surface plus every tool the prompt rules mandate;
//  2. the tool_search tool ({query, limit?}) searches deferred tool metadata
//     (BM25) and returns full schemas as tool-result text;
//  3. deferred tools get a small stub (name + first description sentence +
//     "deferred: run tool_search('<name>') …") — the first N of them only
//     (stub cap); past the cap a deferred tool is discoverable via
//     tool_search only.
//
// Loading a tool never changes the wire: the schema is delivered inside a
// tool result (message tail, cache-friendly) and the model may call the tool
// immediately — milk validates emitted args against the real schema at
// dispatch time. Deferred tools called without loading run under a guess-
// safe policy (read-only tools execute on valid args; state-changing tools
// are refused), and every validation failure echoes the full schema as the
// canonical corrective path.

const (
	toolSearchToolName     = "tool_search"
	toolSearchDefaultLimit = 5
	toolSearchMaxLimit     = 20
	// toolSearchStubDescMax is the budget for the auto-generated stub
	// excerpt (the first description sentence, truncated at a word
	// boundary with "…"). The full stub description also carries the
	// fixed "— deferred: run tool_search(…)" directive; the full stub
	// ENTRY (wire envelope + name + description + open schema) is capped
	// at toolSearchStubEntryMax bytes. "<120 B per stub" cannot hold as a
	// total: the wire envelope alone is ~130 B (issue #480 resolution 1).
	toolSearchStubDescMax  = 120
	toolSearchStubEntryMax = 320
)

// toolSearchDescription is the fixed tool_search description (design §3.1 —
// fixed so it does not change while deferred tools register).
const toolSearchDescription = "Searches deferred tool metadata (BM25) and returns full schemas for the matches. Some tools — MCP servers, plugins, rarely used built-ins — are deferred; search before calling them. For MCP tool discovery always use tool_search."

// toolSearchSchema is the tool_search wire schema: {query: string,
// limit?: int} — default 5, cap 20.
func toolSearchSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        toolSearchToolName,
			"description": toolSearchDescription,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Search query for deferred tools."},
					"limit": map[string]any{"type": "integer", "description": fmt.Sprintf("Maximum number of tools to return (default %d, cap %d).", toolSearchDefaultLimit, toolSearchMaxLimit)},
				},
				"required": []string{"query"},
			},
		},
	}
}

// corePinnedTools is the core coding surface (issue #480's core 10): always
// pinned, always callable with full schemas.
var corePinnedTools = []string{
	"bash", "bash_output", "find_files", "grep", "read_file",
	"write_file", "edit_file", "multiedit", "list_dir", "project_map",
}

// rulePinnedTools are the tools the prompt rules mandate (plus the ones the
// rule-text scan in TestPinDriftGuard picks up — see that test): a rule must
// never reference a tool the model cannot call immediately. Keep in sync
// with the rule constants in local.go / boardtools.go — the guard test fails
// the build when a rule starts referencing a tool missing from this list.
var rulePinnedTools = []string{
	"record_memory", "get_memory", "list_memory", "forget_memory",
	"get_metrics", "create_task", "update_task", "list_tasks", "complete_task",
	"current_need", "get_session_context", "spawn_background_agent",
	"board_get", "board_claim", "board_update", "board_post",
	"status_background_agent", "wait_background_agent", "cancel_background_agent",
	// Referenced by the git-provenance / config-write rules:
	"escalate", "milk_config_help",
}

// toolSearchPinSet is the implicit pin set (core + rule-mandated) plus the
// agent's config extras (AgentConfig.ToolSearchPin).
func toolSearchPinSet(extra []string) map[string]bool {
	pin := make(map[string]bool, len(corePinnedTools)+len(rulePinnedTools)+len(extra))
	for _, n := range corePinnedTools {
		pin[n] = true
	}
	for _, n := range rulePinnedTools {
		pin[n] = true
	}
	for _, n := range extra {
		if n = strings.TrimSpace(n); n != "" {
			pin[n] = true
		}
	}
	return pin
}

// toolSearchDeferred classifies one registered name: deferred unless pinned.
// Dynamic-prefix tools (mcp_<server>_<tool>, agent_<name>) are deferred by
// default — a config tool_search_pin entry pins them explicitly.
func toolSearchDeferred(name string, pin map[string]bool) bool {
	return !pin[name]
}

// guessSafeTools are the read-only tools a deferred call may execute without
// a prior tool_search when the args validate against the real schema (design
// §3.2). Everything else is treated as state-changing and refused until
// loaded — a guessed destructive call that happens to validate is worse than
// any token cost. Static classification, never inferred at runtime.
var guessSafeTools = map[string]bool{
	"bash_output": true, "find_files": true, "grep": true, "read_file": true,
	"list_dir": true, "project_map": true, "get_session_context": true,
	"http_request": true, "web_fetch": true, "http_get": true, // http_get: deprecated alias of web_fetch
	"open_file": true, "get_memory": true, "list_memory": true,
	"search_signals": true, "get_metrics": true, "get_context_stats": true,
	"milk_config_help": true, "board_get": true,
	"status_background_agent": true, "wait_background_agent": true,
	"skill": true, // skill loads read-only procedure text
}

// toolSearchStore is the session-wide tool_search state on the Agent: the
// loaded set (mutex-map pointer, the toolUseTracker pattern — see
// reduction.go), the wire-side counts, and the prompt line. Shared across
// With*/cloneForBackground shallow copies; nil-safe like toolUseTracker.
type toolSearchStore struct {
	mu     sync.Mutex
	loaded map[string]bool
	loads  int // total tools marked loaded (metric)
	line   string
	stubs  int // stubs on the wire (metric)
}

func newToolSearchStore() *toolSearchStore {
	return &toolSearchStore{loaded: map[string]bool{}}
}

// markLoaded records tools as loaded (schema delivered or valid guess-safe
// call). Nil-safe.
func (s *toolSearchStore) markLoaded(names ...string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded == nil {
		s.loaded = map[string]bool{}
	}
	for _, n := range names {
		if !s.loaded[n] {
			s.loaded[n] = true
			s.loads++
		}
	}
}

func (s *toolSearchStore) has(name string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loaded[name]
}

// setWireInfo records the per-session wire shape (prompt line, stub count).
// The tools array never changes within a session, so this is constant after
// the first Run's wire assembly; ReshapeCacheWarm's prompt re-render reads
// promptLine and must stay byte-exact with Run's.
func (s *toolSearchStore) setWireInfo(line string, stubs int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.line = line
	s.stubs = stubs
}

func (s *toolSearchStore) promptLine() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.line
}

func (s *toolSearchStore) counts() (stubs, loads int) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stubs, s.loads
}

// toolSearchRuntime is the dispatch-time state of one tool-search-enabled
// turn: it travels in the context (the WithTurnContext pattern) so only the
// Run path is intercepted — background jobs (RunBackgroundTask) build their
// tool lists elsewhere and never carry it.
type toolSearchRuntime struct {
	pinned   map[string]bool           // pin set incl. config extras
	deferred map[string]map[string]any // name → pre-diet full entry ({"type":"function",…})
	docs     []toolSearchDoc           // search index, name-sorted
	store    *toolSearchStore          // session loaded set
	tier     string                    // tool_description_tier for echoed schemas
}

type toolSearchKey struct{}

func withToolSearch(ctx context.Context, rt *toolSearchRuntime) context.Context {
	return context.WithValue(ctx, toolSearchKey{}, rt)
}

func toolSearchFromContext(ctx context.Context) *toolSearchRuntime {
	rt, _ := ctx.Value(toolSearchKey{}).(*toolSearchRuntime)
	return rt
}

// effectiveToolSearchStubCap resolves the configured stub cap: 0 →
// config.DefaultToolSearchStubCap, negative → no stubs at all.
func effectiveToolSearchStubCap(configured int) int {
	switch {
	case configured < 0:
		return 0
	case configured == 0:
		return config.DefaultToolSearchStubCap
	default:
		return configured
	}
}

// applyToolSearch is the wire assembly (design §3.1): run after the tool-
// description diet, it replaces every non-pinned tool with its stub (up to
// the stub cap), inserts the tool_search schema, and returns the dispatch
// runtime for the turn (nil when tool_search is off — the input list is then
// returned unchanged, byte-for-byte status quo). preDiet is the same tool
// list before applyToolDescDiet; the search index and validation read the
// full schemas from it. The input slices are never mutated.
func (a *Agent) applyToolSearch(wire, preDiet []map[string]any) ([]map[string]any, *toolSearchRuntime) {
	if !strings.EqualFold(strings.TrimSpace(a.toolSearchMode), "auto") {
		return wire, nil
	}
	preDietByName := make(map[string]map[string]any, len(preDiet))
	for _, e := range preDiet {
		preDietByName[toolName(e)] = e
	}
	pin := toolSearchPinSet(a.toolSearchPin)
	var pinnedEntries []map[string]any
	var deferredNames []string
	for _, e := range wire {
		n := toolName(e)
		if n == toolSearchToolName {
			continue // defensive: never double-insert
		}
		if toolSearchDeferred(n, pin) {
			deferredNames = append(deferredNames, n)
		} else {
			pinnedEntries = append(pinnedEntries, e)
		}
	}
	if len(deferredNames) == 0 {
		// Nothing deferred this run: clear any stale wire info so the prompt
		// line never outlives the deferred set (nil-safe).
		a.toolSearch.setWireInfo("", 0)
		return wire, nil
	}
	sort.Strings(deferredNames) // deterministic stub order: stable by name

	stubCap := effectiveToolSearchStubCap(a.toolSearchStubCap)
	stubbed := deferredNames
	beyondCap := 0
	if len(stubbed) > stubCap {
		beyondCap = len(stubbed) - stubCap
		stubbed = stubbed[:stubCap]
	}

	out := make([]map[string]any, 0, len(pinnedEntries)+1+len(stubbed))
	out = append(out, pinnedEntries...)
	out = append(out, toolSearchSchema())
	for _, n := range stubbed {
		out = append(out, toolSearchStubEntry(n, preDietByName[n]))
	}

	rt := &toolSearchRuntime{
		pinned:   pin,
		deferred: make(map[string]map[string]any, len(deferredNames)),
		store:    a.toolSearch,
		tier:     a.toolDescTier,
	}
	if rt.store == nil {
		rt.store = newToolSearchStore()
		a.toolSearch = rt.store
	}
	docs := make([]toolSearchDoc, 0, len(deferredNames))
	for _, n := range deferredNames {
		entry := preDietByName[n]
		if entry == nil {
			continue
		}
		rt.deferred[n] = entry
		docs = append(docs, toolSearchDoc{name: n, text: toolSearchText(entry)})
	}
	rt.docs = docs
	rt.store.setWireInfo(toolSearchPromptLine(len(deferredNames), beyondCap, toolSearchCategories(deferredNames)), len(stubbed))
	return out, rt
}

// toolSearchPromptLine renders the one prompt line of the tool_search policy
// (design §3.5): how many tools are deferred, how to load them, the
// categories present (one word each), and — past the stub cap — how many
// exist beyond the listing. Empty when nothing is deferred.
func toolSearchPromptLine(deferred, beyondCap int, categories []string) string {
	if deferred == 0 {
		return ""
	}
	line := fmt.Sprintf("%d tools deferred (MCP, plugins, rarely used). Use tool_search(query) to load schemas; all deferred tools are callable once their schema is known.", deferred)
	if len(categories) > 0 {
		line += " Categories: " + strings.Join(categories, ", ") + "."
	}
	if beyondCap > 0 {
		line += fmt.Sprintf(" %d tools deferred beyond listing — find them with tool_search.", beyondCap)
	}
	return line
}

// toolSearchCategories lists the categories present among the deferred
// names, one word each, deduplicated and sorted (Anthropic's category-hint
// best practice, design §3.5).
func toolSearchCategories(deferredNames []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range deferredNames {
		c := toolSearchCategory(n)
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func toolSearchCategory(name string) string {
	switch {
	case strings.HasPrefix(name, "mcp_"):
		return "mcp"
	case strings.HasPrefix(name, "agent_"):
		return "plugin"
	}
	switch name {
	case "http_request", "web_fetch", "http_get":
		return "network"
	case "start_workflow":
		return "workflow"
	case "export_session", "get_context_stats", "search_signals":
		return "observability"
	case "kill_bash", "delete_file", "move_file", "open_file", "skill":
		return "files"
	case "send_background_agent":
		return "background"
	case "ask_user", "escalate":
		return "interaction"
	}
	return "misc"
}

// toolSearchStubEntry builds a deferred tool's wire stub: name + first
// description sentence (≤ toolSearchStubDescMax bytes, word-boundary
// truncated with "…") + the fixed "— deferred: run tool_search(…)"
// directive, over an open schema ({"type":"object"} +
// additionalProperties: true) so no provider-level shape validation blocks a
// call. The full entry stays within toolSearchStubEntryMax bytes.
func toolSearchStubEntry(name string, full map[string]any) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": toolSearchStubDescription(name, full),
			"parameters": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": true,
			},
		},
	}
}

// stubSuffix is the fixed directive appended to every stub excerpt.
func stubSuffix(name string) string {
	return " — deferred: run tool_search('" + name + "') for its schema before first use"
}

func toolSearchStubDescription(name string, full map[string]any) string {
	fn, _ := full["function"].(map[string]any)
	desc, _ := fn["description"].(string)
	excerpt := truncateAtWord(firstSentence(desc), toolSearchStubDescMax)
	suffix := stubSuffix(name)
	// Enforce the full-entry budget: shrink the excerpt (never the
	// directive) while the entry would exceed toolSearchStubEntryMax.
	for entryLen(name, excerpt+suffix) > toolSearchStubEntryMax && excerpt != "" {
		excerpt = truncateAtWord(excerpt, len([]rune(excerpt))*4/5-1)
	}
	return excerpt + suffix
}

// entryLen is the serialized size of the stub entry for name+description.
func entryLen(name, description string) int {
	b, _ := json.Marshal(toolSearchStubEntryRaw(name, description))
	return len(b)
}

func toolSearchStubEntryRaw(name, description string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": description,
			"parameters": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": true,
			},
		},
	}
}

// firstSentence returns the description's first sentence (up to and
// including the first "." at a boundary, or the first line when the text is
// not sentence-punctuated).
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\n':
			return strings.TrimSpace(s[:i])
		case '.':
			if i+1 == len(s) || s[i+1] == ' ' {
				return strings.TrimSpace(s[:i+1])
			}
		}
	}
	return s
}

// truncateAtWord caps s at max bytes on a rune boundary, cutting back to the
// last space and appending "…" when a cut happened.
func truncateAtWord(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	// Stay on a rune boundary.
	for len(cut) > 0 && !isRuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	if i := strings.LastIndexByte(cut, ' '); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:(") + "…"
}

// isRuneStart reports whether b can start a UTF-8 rune (not a continuation).
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// toolSearchText is a tool's search text: its description plus its
// parameters' names and descriptions, recursively resolving local $refs
// (pi's schemaText, tool-search.ts). The name is not included — the ranker
// reads it from the name field.
func toolSearchText(entry map[string]any) string {
	fn, _ := entry["function"].(map[string]any)
	var parts []string
	if d, _ := fn["description"].(string); strings.TrimSpace(d) != "" {
		parts = append(parts, d)
	}
	if params, ok := fn["parameters"]; ok {
		collectSchemaText(params, params, &parts, map[string]bool{})
	}
	return strings.Join(parts, " ")
}

// collectSchemaText appends the description and property-name text of a
// schema fragment, following local $refs into root once each (seen is keyed
// by the $ref string — schemas are plain maps, which cannot key a set).
func collectSchemaText(schema, root any, parts *[]string, seen map[string]bool) {
	m, ok := schema.(map[string]any)
	if !ok {
		return
	}
	if ref, ok := m["$ref"].(string); ok && !seen[ref] {
		seen[ref] = true
		if target := resolveSchemaRef(ref, root); target != nil {
			collectSchemaText(target, root, parts, seen)
		}
	}
	if d, ok := m["description"].(string); ok && strings.TrimSpace(d) != "" {
		*parts = append(*parts, d)
	}
	if props, ok := m["properties"].(map[string]any); ok {
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			*parts = append(*parts, k)
			collectSchemaText(props[k], root, parts, seen)
		}
	}
	collectSchemaText(m["items"], root, parts, seen)
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if variants, ok := m[key].([]any); ok {
			for _, v := range variants {
				collectSchemaText(v, root, parts, seen)
			}
		}
	}
}

// resolveSchemaRef resolves a local JSON-pointer $ref ("#/$defs/Foo") inside
// root. Remote or malformed refs resolve to nil and are skipped.
func resolveSchemaRef(ref string, root any) any {
	if !strings.HasPrefix(ref, "#") {
		return nil
	}
	path := strings.TrimPrefix(ref, "#")
	if path == "" || path == "/" {
		return root
	}
	cur := root
	for _, seg := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[seg]
		if !ok {
			return nil
		}
	}
	return cur
}

// tieredFunctionSchema returns the "function" submap of a full entry with
// the agent's tool_description_tier applied (search results and schema
// echoes honor the same diet as the wire).
func tieredFunctionSchema(entry map[string]any, tier string) map[string]any {
	dieted := applyToolDescDiet([]map[string]any{entry}, tier)
	if len(dieted) == 0 {
		return map[string]any{}
	}
	fn, _ := dieted[0]["function"].(map[string]any)
	return fn
}

// interceptToolSearch is the dispatch-time interception (issue #480),
// called at the top of dispatchOneTool when the turn carries a
// toolSearchRuntime. handled=false means "fall through to normal dispatch":
// pinned/loaded/unknown tools, and guess-safe deferred tools whose args
// validated (they are marked loaded and executed normally).
func (a *Agent) interceptToolSearch(ctx context.Context, rt *toolSearchRuntime, tc toolCall) (toolCallOutcome, bool) {
	name := tc.Function.Name
	if name == toolSearchToolName {
		result := a.runToolSearch(ctx, rt, tc.Function.Arguments)
		return toolCallOutcome{msg: Message{Role: "tool", Content: result, ToolCallID: tc.ID}}, true
	}
	if rt.pinned[name] || rt.store.has(name) {
		return toolCallOutcome{}, false
	}
	entry, deferred := rt.deferred[name]
	if !deferred {
		return toolCallOutcome{}, false
	}
	if guessSafeTools[name] {
		if err := validateToolArgs(entry, tc.Function.Arguments); err != nil {
			result := toolSearchSchemaEcho(name, entry, rt.tier, err)
			obs.Inc(ctx, inferenceScope, "milk.tools.tool_search_schema_echoes",
				attribute.String("name", name))
			return toolCallOutcome{msg: Message{Role: "tool", Content: result, ToolCallID: tc.ID}}, true
		}
		rt.store.markLoaded(name) // valid read: execute + mark loaded
		return toolCallOutcome{}, false
	}
	result := toolResult{Error: fmt.Sprintf("%s is deferred and state-changing; run tool_search('%s') and re-call with the documented semantics.", name, name)}.String()
	obs.Inc(ctx, inferenceScope, "milk.tools.tool_search_refusals",
		attribute.String("name", name))
	return toolCallOutcome{msg: Message{Role: "tool", Content: result, ToolCallID: tc.ID}}, true
}

// runToolSearch executes the tool_search tool: BM25 over the deferred tools'
// metadata, full schemas (tier'd) in the result text, returned matches
// marked loaded. Never an error for "nothing found".
func (a *Agent) runToolSearch(ctx context.Context, rt *toolSearchRuntime, argsJSON string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return toolResult{Error: "invalid arguments: " + err.Error()}.String()
	}
	query, _ := args["query"].(string)
	if strings.TrimSpace(query) == "" {
		return toolResult{Error: "query must not be empty"}.String()
	}
	limit := toolSearchDefaultLimit
	if raw, ok := args["limit"]; ok && raw != nil {
		f, isNum := raw.(float64)
		if !isNum || f != float64(int(f)) || f <= 0 {
			return toolResult{Error: "limit must be a positive integer"}.String()
		}
		limit = int(f)
		if limit > toolSearchMaxLimit {
			limit = toolSearchMaxLimit
		}
	}
	obs.Inc(ctx, inferenceScope, "milk.tools.tool_search_calls")
	ranked := rankToolSearchDocs(query, rt.docs)
	if len(ranked) == 0 {
		return "No matching tools found."
	}
	top := ranked
	if len(top) > limit {
		top = top[:limit]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Loaded %d tools:\n", len(top))
	for _, name := range top {
		fn := tieredFunctionSchema(rt.deferred[name], rt.tier)
		first := firstDescriptionLine(fn)
		marker := ""
		if rt.store.has(name) {
			marker = " (already loaded)"
		}
		fmt.Fprintf(&b, "- %s — %s%s\n", name, first, marker)
		if schemaJSON, err := json.Marshal(fn); err == nil {
			fmt.Fprintf(&b, "schema: %s\n", schemaJSON)
		}
	}
	if rest := ranked[len(top):]; len(rest) > 0 {
		fmt.Fprintf(&b, "Also matched: %s\n", strings.Join(rest, ", "))
	}
	rt.store.markLoaded(top...)
	return toolResult{Output: b.String()}.String()
}

// firstDescriptionLine is the one-line summary of a schema's description.
func firstDescriptionLine(fn map[string]any) string {
	desc, _ := fn["description"].(string)
	if i := strings.IndexByte(desc, '\n'); i >= 0 {
		desc = desc[:i]
	}
	if d := strings.TrimSpace(desc); d != "" {
		return d
	}
	return "(no description)"
}

// validateToolArgs checks emitted arguments against the real schema: valid
// JSON object, every required parameter present, no unknown parameters when
// the schema declares properties (design §3.2's guess-safe gate).
func validateToolArgs(entry map[string]any, argsJSON string) error {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Errorf("arguments must be a JSON object: %v", err)
	}
	fn, _ := entry["function"].(map[string]any)
	params, _ := fn["parameters"].(map[string]any)
	for _, req := range stringSlice(params["required"]) {
		if _, ok := args[req]; !ok {
			return fmt.Errorf("missing required parameter %q", req)
		}
	}
	if props, ok := params["properties"].(map[string]any); ok {
		for k := range args {
			if _, has := props[k]; !has {
				return fmt.Errorf("unknown parameter %q", k)
			}
		}
	}
	return nil
}

// stringSlice normalizes a schema "required" value ([]string in built-in
// schemas, []any after a JSON round trip).
func stringSlice(v any) []string {
	switch n := v.(type) {
	case []string:
		return n
	case []any:
		out := make([]string, 0, len(n))
		for _, e := range n {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// toolSearchSchemaEcho is the canonical corrective path (design §3.2): a
// validation failure returns the tier'd full schema plus the one-line fix
// hint — it covers guess-wrong args and schema text lost to compaction.
func toolSearchSchemaEcho(name string, entry map[string]any, tier string, problem error) string {
	fn := tieredFunctionSchema(entry, tier)
	schemaJSON, _ := json.Marshal(fn)
	return toolResult{Error: fmt.Sprintf("%s: %v\n\n%s\n\n%s schema above; fix the arguments and call again.", name, problem, schemaJSON, name)}.String()
}
