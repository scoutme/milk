package main

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

// --- #165: comma-separated multi-value parameters ---

func TestSplitValueSegment(t *testing.T) {
	cases := []struct{ in, keep, partial string }{
		{"w", "", "w"},
		{"db,w", "db,", "w"},
		{"db,wiki,x", "db,wiki,", "x"},
		{"db,", "db,", ""},
		{"", "", ""},
		{"a, b", "", "b"},     // space before the cursor word: nothing to keep
		{"a, b,c", "b,", "c"}, // only the last whitespace word is replaced
	}
	for _, c := range cases {
		keep, partial := splitValueSegment(c.in)
		if keep != c.keep || partial != c.partial {
			t.Errorf("splitValueSegment(%q) = (%q, %q), want (%q, %q)", c.in, keep, partial, c.keep, c.partial)
		}
	}
}

func TestMergeCommaContinuations(t *testing.T) {
	vs := cmdVariants["/mcp"]
	if len(vs) == 0 {
		t.Fatal("cmdVariants[/mcp] empty — help text missing the signature")
	}
	cases := []struct {
		rel  []string
		want []string
	}{
		// Multi-value <server>: "db," + "wiki" folds into one list word.
		{[]string{"/mcp", "assign", "db,", "wiki"}, []string{"/mcp", "assign", "db, wiki"}},
		// Chain of continuations.
		{[]string{"/mcp", "assign", "db,", "wiki,", "x"}, []string{"/mcp", "assign", "db, wiki, x"}},
		// No trailing comma: nothing merges.
		{[]string{"/mcp", "assign", "db", "wiki"}, []string{"/mcp", "assign", "db", "wiki"}},
		// The command token never absorbs the next word.
		{[]string{"/mcp,", "assign"}, []string{"/mcp,", "assign"}},
	}
	for _, c := range cases {
		got := mergeCommaContinuations(c.rel, vs)
		if !stringSlicesEqual(got, c.want) {
			t.Errorf("mergeCommaContinuations(%v) = %v, want %v", c.rel, got, c.want)
		}
	}

	// Single-value parameter: the comma is part of the name, no folding.
	sw := cmdVariants["/agent switch"]
	got := mergeCommaContinuations([]string{"/agent", "switch", "foo,", "bar"}, sw)
	if !stringSlicesEqual(got, []string{"/agent", "switch", "foo,", "bar"}) {
		t.Errorf("single-value merge = %v, want words unchanged", got)
	}
}

// TestMultiValueParamsCoverRealSignatures is the drift guard for
// multiValueParams (namespaces.go): every declared key must correspond to a
// real parameter position in the help-derived command signatures. A failure
// means a signature changed and the map was left behind — fix the key or the
// signature.
func TestMultiValueParamsCoverRealSignatures(t *testing.T) {
	type posKey struct{ prefix, inner string }
	have := map[posKey]bool{}
	for _, vs := range cmdVariants {
		for _, v := range vs {
			for _, w := range strings.Fields(v.sig) {
				if inner := placeholderInner(w); inner != "" {
					have[posKey{sigCmdPrefix(v.sig), inner}] = true
				}
			}
		}
	}
	for key := range multiValueParams {
		i := strings.LastIndexByte(key, ' ')
		k := posKey{key[:i], key[i+1:]}
		if !have[k] {
			t.Errorf("multiValueParams key %q matches no help-text parameter position — update multiValueParams in namespaces.go", key)
		}
	}
}

func TestParamIsMultiValue(t *testing.T) {
	multi := []struct{ sig, tok string }{
		{"/mcp assign <server> for <agent>", "<server>"},
		{"/mcp assign <server> for <agent>", "<agent>"},
		{"/mcp unassign <server> for <agent>", "<agent>"},
		{"/agent tool enable <tool> [for <agent>|global]", "<agent>|global]"},
		{"/agent tool remove <tool> [for <agent>|global]", "<agent>|global]"},
	}
	for _, c := range multi {
		if !paramIsMultiValue(c.sig, c.tok) {
			t.Errorf("paramIsMultiValue(%q, %q) = false, want true", c.sig, c.tok)
		}
	}
	single := []struct{ sig, tok string }{
		{"/mcp assign <server> for <agent>", "for"},
		{"/agent tool enable <tool> [for <agent>|global]", "<tool>"},
		{"/agent switch <name> as primary|escalation]", "<name>"},
	}
	for _, c := range single {
		if paramIsMultiValue(c.sig, c.tok) {
			t.Errorf("paramIsMultiValue(%q, %q) = true, want false", c.sig, c.tok)
		}
	}
}

func TestBuildTabMatches_MultiValueSegment(t *testing.T) {
	lookup := fakeLookup(map[string][]string{
		nsMCP:   {"db", "wiki", "zone"},
		nsAgent: {"claude", "local", "zeta"},
	})
	cases := []struct {
		input     string
		want      []string
		segPrefix string
	}{
		// The issue's core rule: the comma starts a fresh completion segment.
		{"/mcp assign db,w", []string{"wiki"}, "db,"},
		{"/mcp assign db,", []string{"db", "wiki", "zone"}, "db,"},
		{"x /mcp assign db,wiki,z", []string{"zone"}, "db,wiki,"},
		// Space after the comma still completes the same segment.
		{"/mcp assign db, w", []string{"wiki"}, ""},
		{"/mcp assign db, wiki, z", []string{"zone"}, ""},
		// Multi-value on the second parameter: after "for".
		{"/mcp assign db for claude,z", []string{"zeta"}, "claude,"},
		// Single-value parameters never segment on commas.
		{"/agent switch cl,a", nil, ""},
	}
	for _, c := range cases {
		res := buildTabMatches(c.input, ".", lookup)
		if !stringSlicesEqual(res.matches, c.want) {
			t.Errorf("buildTabMatches(%q).matches = %v, want %v", c.input, res.matches, c.want)
		}
		if res.segPrefix != c.segPrefix {
			t.Errorf("buildTabMatches(%q).segPrefix = %q, want %q", c.input, res.segPrefix, c.segPrefix)
		}
		if c.want != nil && !res.valueMode {
			t.Errorf("buildTabMatches(%q) should be value mode", c.input)
		}
	}
}

func TestBuildTabMatches_MultiValueTrailingCommaContinuesSameParam(t *testing.T) {
	// "db, " + Tab must keep completing the <server> list instead of
	// advancing to the literal "for" position.
	res := buildTabMatches("/mcp assign db, ", ".", fakeLookup(testAgents))
	if !res.valueMode {
		t.Fatalf("expected value mode for '/mcp assign db, ', got %+v", res)
	}
	if !stringSlicesEqual(res.matches, testAgents[nsMCP]) {
		t.Errorf("matches = %v, want %v", res.matches, testAgents[nsMCP])
	}
	// Same for the agent side: "… for claude, " + Tab lists agents again.
	res = buildTabMatches("/mcp assign db for claude, ", ".", fakeLookup(testAgents))
	if !stringSlicesEqual(res.matches, testAgents[nsAgent]) {
		t.Errorf("agent-side continuation matches = %v, want %v", res.matches, testAgents[nsAgent])
	}
	// A trailing comma at a single-value parameter must NOT hold the position.
	res = buildTabMatches("/agent switch foo, ", ".", fakeLookup(testAgents))
	if stringSlicesEqual(res.matches, testAgents[nsAgent]) {
		t.Errorf("single-value trailing comma should not re-list values, got %v", res.matches)
	}
}

func TestApplyCompletionToken_MultiValue(t *testing.T) {
	cases := []struct{ before, segPrefix, token, want string }{
		{"/mcp assign db,w", "db,", "wiki", "/mcp assign db,wiki"},
		{"/mcp assign db,wiki,z", "db,wiki,", "zeta", "/mcp assign db,wiki,zeta"},
		{"/mcp assign db, w", "", "wiki", "/mcp assign db, wiki"},
		{"/mcp assign db, ", "", "wiki", "/mcp assign db, wiki"},
		{"/mcp assign d", "", "db", "/mcp assign db"},
	}
	for _, c := range cases {
		m := model{tabValueMode: true, tabSegPrefix: c.segPrefix, tabBeforeCursor: c.before}
		if got := m.applyCompletionToken(c.token); got != c.want {
			t.Errorf("applyCompletionToken(%q, seg=%q, tok=%q) = %q, want %q",
				c.before, c.segPrefix, c.token, got, c.want)
		}
	}
}

// --- dispatch: all-or-nothing multi-value application ---

// mvState builds an interactiveState with two MCP servers, two agents and one
// global tool-agent "helper" for multi-value dispatch tests. HOME and cwd are
// isolated so config saves stay inside the test.
func mvState(t *testing.T) *interactiveState {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	return &interactiveState{cfg: config.Config{
		Agent:      "local",
		MCPServers: []config.MCPServerConfig{{Name: "db"}, {Name: "wiki"}},
		Agents:     []config.AgentConfig{{Name: "local"}, {Name: "claude"}},
		AgentTools: []config.AgentToolEntry{{Agent: "helper", Description: "h"}},
	}}
}

func TestSplitCommaNames(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a,b", []string{"a", "b"}},
		{"a, b ,c", []string{"a", "b", "c"}},
		{"a,,b,", []string{"a", "b"}},
		{"", nil},
	}
	for _, c := range cases {
		if got := splitCommaNames(c.in); !stringSlicesEqual(got, c.want) {
			t.Errorf("splitCommaNames(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestResolveToolScopes(t *testing.T) {
	cfg := config.Config{
		Agent:  "local",
		Agents: []config.AgentConfig{{Name: "local"}, {Name: "claude"}},
	}
	// Empty scope resolves to the active primary agent.
	scopes, invalid := resolveToolScopes("", cfg)
	if !stringSlicesEqual(scopes, []string{"local"}) || len(invalid) != 0 {
		t.Errorf("empty scope = %v, %v; want [local], no invalid", scopes, invalid)
	}
	// "global" alone is a scope.
	scopes, invalid = resolveToolScopes("global", cfg)
	if !stringSlicesEqual(scopes, []string{"global"}) || len(invalid) != 0 {
		t.Errorf("global = %v, %v; want [global], no invalid", scopes, invalid)
	}
	// "global" cannot be combined with agent names — per-item report; valid
	// names still resolve (the caller applies nothing while invalid is set).
	scopes, invalid = resolveToolScopes("global,claude", cfg)
	if !stringSlicesEqual(scopes, []string{"claude"}) || len(invalid) != 1 ||
		!strings.Contains(invalid[0], "global") {
		t.Errorf("global,claude = %v, %v; want [claude] plus a 'global' report", scopes, invalid)
	}
	// Unknown agent: reported, others still resolve (caller applies nothing).
	scopes, invalid = resolveToolScopes("claude,ghost", cfg)
	if !stringSlicesEqual(scopes, []string{"claude"}) || len(invalid) != 1 ||
		!strings.Contains(invalid[0], "ghost") {
		t.Errorf("claude,ghost = %v, %v; want [claude] plus a 'ghost' report", scopes, invalid)
	}
	// Duplicates collapse to one scope.
	scopes, invalid = resolveToolScopes("claude,claude", cfg)
	if !stringSlicesEqual(scopes, []string{"claude"}) || len(invalid) != 0 {
		t.Errorf("claude,claude = %v, %v; want [claude] once", scopes, invalid)
	}
}

func TestExecMCPAssign_MultiValueAppliesAllCombinations(t *testing.T) {
	st := mvState(t)
	out := execMCPAssign("db,wiki for local,claude", true, st)
	for _, want := range []string{`"db" assigned to agents "local", "claude"`, `"wiki" assigned to agents "local", "claude"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, a := range st.cfg.Agents {
		if !stringSlicesEqual(a.MCPServers, []string{"db", "wiki"}) {
			t.Errorf("agent %s MCPServers = %v, want [db wiki]", a.Name, a.MCPServers)
		}
	}
}

func TestExecMCPAssign_MixedValidInvalidAppliesNothing(t *testing.T) {
	st := mvState(t)
	out := execMCPAssign("db,bogus for local", true, st)
	if !strings.Contains(out, "nothing applied") {
		t.Errorf("expected 'nothing applied' in:\n%s", out)
	}
	if !strings.Contains(out, "bogus") {
		t.Errorf("expected per-item report naming 'bogus' in:\n%s", out)
	}
	if len(st.cfg.Agents[0].MCPServers) != 0 {
		t.Errorf("agent MCPServers = %v, want unchanged (no silent partial apply)", st.cfg.Agents[0].MCPServers)
	}
	// Same on the agent side.
	out = execMCPAssign("db for local,ghost", true, st)
	if !strings.Contains(out, "nothing applied") || !strings.Contains(out, "ghost") {
		t.Errorf("agent-side mixed input = %q, want per-item report + nothing applied", out)
	}
	if len(st.cfg.Agents[0].MCPServers) != 0 {
		t.Errorf("agent MCPServers = %v, want unchanged", st.cfg.Agents[0].MCPServers)
	}
}

func TestExecMCPAssign_NoopReportedPerItem(t *testing.T) {
	st := mvState(t)
	execMCPAssign("db for local", true, st)
	out := execMCPAssign("db for local,claude", true, st)
	if !strings.Contains(out, "already assigned to agent \"local\"") {
		t.Errorf("expected noop line for 'local' in:\n%s", out)
	}
	if !strings.Contains(out, "assigned to agent \"claude\"") {
		t.Errorf("expected assign line for 'claude' in:\n%s", out)
	}
	if strings.Contains(out, "nothing applied") {
		t.Errorf("partial noop is not a failure; got:\n%s", out)
	}
}

func TestExecAgentTool_MultiScopeAllOrNothing(t *testing.T) {
	st := mvState(t)
	// Mixed valid/invalid scope: report + no mutation at all.
	out := execAgentTool("enable helper for local,ghost", st)
	if !strings.Contains(out, "nothing applied") || !strings.Contains(out, "ghost") {
		t.Errorf("mixed scope = %q, want per-item report + nothing applied", out)
	}
	if len(st.cfg.Agents[0].Tools) != 0 || len(st.cfg.Agents[1].Tools) != 0 {
		t.Errorf("per-agent tool lists changed: %v / %v, want both empty",
			st.cfg.Agents[0].Tools, st.cfg.Agents[1].Tools)
	}
	// All valid: one override per scope.
	out = execAgentTool("enable helper for local,claude", st)
	if strings.Contains(out, "nothing applied") {
		t.Fatalf("unexpected failure:\n%s", out)
	}
	for _, name := range []string{"local", "claude"} {
		ai := findAgentIdx(st.cfg, name)
		if idx := findToolEntryIdx(st.cfg.Agents[ai].Tools, "helper"); idx < 0 {
			t.Errorf("agent %s: helper override missing after multi-scope enable", name)
		} else if *st.cfg.Agents[ai].Tools[idx].Enabled != true {
			t.Errorf("agent %s: helper not enabled", name)
		}
	}
}

func TestExecAgentToolAdd_MultiScopeNameAndDesc(t *testing.T) {
	st := mvState(t)
	out := execAgentTool("add coder description=writes code for local,claude", st)
	if strings.Contains(out, "nothing applied") {
		t.Fatalf("unexpected failure:\n%s", out)
	}
	for _, name := range []string{"local", "claude"} {
		ai := findAgentIdx(st.cfg, name)
		idx := findToolEntryIdx(st.cfg.Agents[ai].Tools, "coder")
		if idx < 0 {
			t.Fatalf("agent %s: 'coder' not added (output %q)", name, out)
		}
		e := st.cfg.Agents[ai].Tools[idx]
		if e.Agent != "coder" {
			t.Errorf("agent %s: tool name = %q, want %q — the name must not absorb description=…", name, e.Agent, "coder")
		}
		if e.Description != "writes code" {
			t.Errorf("agent %s: description = %q, want %q", name, e.Description, "writes code")
		}
	}
}

func TestParseAgentToolScope_StripsDescription(t *testing.T) {
	cases := []struct{ in, scope, tool string }{
		{"helper for claude,local", "claude,local", "helper"},
		{"helper", "", "helper"},
		{"helper description=does things", "", "helper"},
		{"helper description=does things for global", "global", "helper"},
	}
	for _, c := range cases {
		scope, tool := parseAgentToolScope(c.in)
		if scope != c.scope || tool != c.tool {
			t.Errorf("parseAgentToolScope(%q) = (%q, %q), want (%q, %q)", c.in, scope, tool, c.scope, c.tool)
		}
	}
}
