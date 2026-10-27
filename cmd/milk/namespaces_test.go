package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

// fakeLookup returns a paramLookup backed by a fixed map.
func fakeLookup(m map[string][]string) paramLookup {
	return func(ns string) []string { return m[ns] }
}

var testAgents = map[string][]string{
	nsAgent:    {"claude", "local", "zeta"},
	nsMCP:      {"db", "wiki"},
	nsTool:     {"helper"},
	nsPanel:    {"memory", "tasks", "background", "workflow", "project", "git"},
	nsWorkflow: {"dev", "pair", "swarm", "team"},
}

func TestBuildTabMatches_ValueAgentSwitch(t *testing.T) {
	// The issue's canonical example: third position of "/cmd subcommand <agent>".
	res := buildTabMatches("/agent switch cl", ".", fakeLookup(testAgents))
	if !res.valueMode {
		t.Fatalf("expected value mode for '/agent switch cl', got %+v", res)
	}
	if res.replaceBase != "" {
		t.Errorf("expected empty replaceBase in value mode, got %q", res.replaceBase)
	}
	if len(res.matches) != 1 || res.matches[0] != "claude" {
		t.Errorf("matches = %v, want [claude]", res.matches)
	}
	if res.nsLabel != nsAgent {
		t.Errorf("nsLabel = %q, want %q", res.nsLabel, nsAgent)
	}
}

func TestBuildTabMatches_ValueTrailingSpaceListsAll(t *testing.T) {
	res := buildTabMatches("/agent switch ", ".", fakeLookup(testAgents))
	if !res.valueMode {
		t.Fatalf("expected value mode for '/agent switch ', got %+v", res)
	}
	want := testAgents[nsAgent]
	if !stringSlicesEqual(res.matches, want) {
		t.Errorf("matches = %v, want %v", res.matches, want)
	}
}

func TestBuildTabMatches_ValueMCPAssign(t *testing.T) {
	// Second parameter position: "/mcp assign <server>…" → MCP server names.
	res := buildTabMatches("/mcp assign ", ".", fakeLookup(testAgents))
	if !res.valueMode || !stringSlicesEqual(res.matches, testAgents[nsMCP]) {
		t.Errorf("'/mcp assign ' → %+v, want value mode with %v", res, testAgents[nsMCP])
	}
	// Fourth position after the literal keyword: "…for <agent>" → agent names.
	res = buildTabMatches("/mcp assign db for ", ".", fakeLookup(testAgents))
	if !res.valueMode || !stringSlicesEqual(res.matches, testAgents[nsAgent]) {
		t.Errorf("'/mcp assign db for ' → %+v, want value mode with %v", res, testAgents[nsAgent])
	}
	// Third position is the literal keyword "for".
	res = buildTabMatches("/mcp assign db f", ".", fakeLookup(testAgents))
	if !res.valueMode || !stringSlicesEqual(res.matches, []string{"for"}) {
		t.Errorf("'/mcp assign db f' → %+v, want value mode with [for]", res)
	}
}

func TestBuildTabMatches_ValueToolScope(t *testing.T) {
	// "[<agent>|global]" → name space members plus the literal alternative.
	res := buildTabMatches("/agent tool list ", ".", fakeLookup(testAgents))
	if !res.valueMode {
		t.Fatalf("expected value mode for '/agent tool list ', got %+v", res)
	}
	want := append(append([]string{}, testAgents[nsAgent]...), "global")
	if !stringSlicesEqual(res.matches, want) {
		t.Errorf("matches = %v, want %v", res.matches, want)
	}
	if res.nsLabel != nsAgent {
		t.Errorf("nsLabel = %q, want %q", res.nsLabel, nsAgent)
	}
}

func TestBuildTabMatches_ValueDeeperLiteral(t *testing.T) {
	// Third-position literal subcommand — previously had no completion at all.
	res := buildTabMatches("/agent tool l", ".", fakeLookup(testAgents))
	if !res.valueMode || !stringSlicesEqual(res.matches, []string{"list"}) {
		t.Errorf("'/agent tool l' → %+v, want value mode with [list]", res)
	}
}

func TestBuildTabMatches_ValueWorkflowNames(t *testing.T) {
	res := buildTabMatches("/workflow de", ".", fakeLookup(testAgents))
	if !res.valueMode || !stringSlicesEqual(res.matches, []string{"dev"}) {
		t.Errorf("'/workflow de' → %+v, want value mode with [dev]", res)
	}
	if res.nsLabel != nsWorkflow {
		t.Errorf("nsLabel = %q, want %q", res.nsLabel, nsWorkflow)
	}
}

func TestBuildTabMatches_SigModeStillWins(t *testing.T) {
	// Position-1 literal prefixes keep the existing full-signature behavior
	// even when a name space lookup is available.
	res := buildTabMatches("/memory sh", ".", fakeLookup(testAgents))
	if res.valueMode {
		t.Errorf("'/memory sh' must stay signature mode, got value mode %+v", res)
	}
	if res.replaceBase != "/memory" {
		t.Errorf("replaceBase = %q, want /memory", res.replaceBase)
	}
}

func TestBuildTabMatches_FreeTextNoCompletion(t *testing.T) {
	// <msg>-style free-text parameters never complete from name spaces.
	res := buildTabMatches("/escalate hello", ".", fakeLookup(testAgents))
	if len(res.matches) != 0 || res.valueMode {
		t.Errorf("'/escalate hello' → %+v, want no matches", res)
	}
	// Unknown name space (e.g. <id>) also yields nothing.
	res = buildTabMatches("/task done 12", ".", fakeLookup(testAgents))
	if len(res.matches) != 0 || res.valueMode {
		t.Errorf("'/task done 12' → %+v, want no matches", res)
	}
}

func TestBuildTabMatches_NilLookupDisablesValueMode(t *testing.T) {
	res := buildTabMatches("/agent switch cl", ".", nil)
	if res.valueMode || len(res.matches) != 0 {
		t.Errorf("nil lookup → %+v, want no value completion", res)
	}
}

func TestApplyValueCompletion(t *testing.T) {
	cases := []struct {
		line, value, want string
	}{
		{"/agent switch cl", "claude", "/agent switch claude"},
		{"/agent switch ", "claude", "/agent switch claude"},
		{"", "claude", "claude"},
		{"x /mcp assign ", "db", "x /mcp assign db"},
		{"/mcp assign d", "db", "/mcp assign db"},
	}
	for _, c := range cases {
		if got := applyValueCompletion(c.line, c.value); got != c.want {
			t.Errorf("applyValueCompletion(%q, %q) = %q, want %q", c.line, c.value, got, c.want)
		}
	}
}

func TestParamLookup(t *testing.T) {
	m := model{st: &interactiveState{cfg: config.Config{
		Agents: []config.AgentConfig{
			{Name: "local", Tools: []config.AgentToolEntry{{Agent: "per-agent-tool"}}},
			{Name: "claude"},
		},
		MCPServers: []config.MCPServerConfig{{Name: "db"}, {Name: "wiki"}},
		AgentTools: []config.AgentToolEntry{{Agent: "helper"}},
	}}}

	if got := m.paramLookup(nsAgent); !stringSlicesEqual(got, []string{"local", "claude"}) {
		t.Errorf("agent namespace = %v, want [local claude]", got)
	}
	if got := m.paramLookup(nsMCP); !stringSlicesEqual(got, []string{"db", "wiki"}) {
		t.Errorf("mcp namespace = %v, want [db wiki]", got)
	}
	if got := m.paramLookup(nsTool); !stringSlicesEqual(got, []string{"helper", "per-agent-tool"}) {
		t.Errorf("tool namespace = %v, want union of global and per-agent entries", got)
	}
	if got := m.paramLookup(nsPanel); !stringSlicesEqual(got, []string{"memory", "tasks", "background", "workflow", "project", "git", "autoopen"}) {
		t.Errorf("panel namespace = %v, want the six panel names plus autoopen", got)
	}
	wf := m.paramLookup(nsWorkflow)
	found := false
	for _, n := range wf {
		if n == "dev" {
			found = true
		}
	}
	if !found {
		t.Errorf("workflow namespace = %v, want builtin dev present", wf)
	}
	// Unknown namespaces resolve to nil rather than guessing.
	if got := m.paramLookup("no-such-ns"); got != nil {
		t.Errorf("unknown namespace = %v, want nil", got)
	}
}

func TestNamespaceForParam(t *testing.T) {
	cases := []struct {
		cmd, inner, want string
	}{
		{cmdAgent, "name", nsAgent},       // scoped: /agent <name>
		{cmdMCP, "name", nsMCP},           // scoped: /mcp <name>
		{cmdPanel, "name", nsPanel},       // scoped: /panel <name>
		{cmdWorkflow, "name", nsWorkflow}, // scoped: /workflow <name>
		{cmdAgent, "agent", nsAgent},      // global placeholder text
		{cmdMCP, "server", nsMCP},         //
		{cmdMCP, "server-name", nsMCP},    //
		{cmdAgent, "tool", nsTool},        //
		{cmdAgent, "tool-agent", nsTool},  //
		{cmdAgent, "msg", ""},             // free text
		{cmdMemory, "pat|#id", ""},        // free text
		{cmdFooUnmapped, "name", ""},      // unmapped command + generic name
	}
	for _, c := range cases {
		if got := namespaceForParam(c.cmd, c.inner); got != c.want {
			t.Errorf("namespaceForParam(%q, %q) = %q, want %q", c.cmd, c.inner, got, c.want)
		}
	}
}

// cmdFooUnmapped is any command token without a scoped <name> mapping.
const cmdFooUnmapped = "/no-such-command"

// TestParamNamespacesCoverHelpPlaceholders is the drift guard for the name
// space index (#166): every <placeholder> appearing in the help-derived
// signatures must either resolve to a registered name space or be listed in
// freeTextParams as a deliberate free-text parameter. A failure here means a
// new command introduced a placeholder that neither completes nor documents
// why it should not — fix the signature or update the tables in namespaces.go.
func TestParamNamespacesCoverHelpPlaceholders(t *testing.T) {
	re := regexp.MustCompile(`<([^<>]+)>`)
	for cmd, vs := range cmdVariants {
		for _, v := range vs {
			for _, w := range strings.Fields(v.sig) {
				for _, match := range re.FindAllStringSubmatch(w, -1) {
					inner := match[1]
					if namespaceForParam(cmd, inner) != "" {
						continue
					}
					if freeTextParams[inner] {
						continue
					}
					t.Errorf("sig %q: placeholder <%s> has no name space and is not in freeTextParams — register it in nsByPlaceholder/nsByCmdScoped, or add it to freeTextParams in namespaces.go", v.sig, inner)
				}
			}
		}
	}
}
