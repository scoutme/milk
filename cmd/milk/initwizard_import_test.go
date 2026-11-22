package main

// Tests for the harness-import side of the /config init wizard: the import
// step (selection grammar + commit wiring) and the two commit modes — the
// default additive merge (existing config kept) vs the destructive rebuild
// (/config init rebuild). The scan itself is pinned in harness_import_test.go.

import (
	"errors"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

// stubHarnessScan fixes what the import scan finds for one test.
func stubHarnessScan(t *testing.T, creds []importedCredential, servers []importedMCPServer) {
	t.Helper()
	prev := harnessScanFn
	harnessScanFn = func(string) ([]importedCredential, []importedMCPServer) { return creds, servers }
	t.Cleanup(func() { harnessScanFn = prev })
}

// importTestServers is the scan fixture the import tests share.
func importTestServers() []importedMCPServer {
	return []importedMCPServer{
		{Name: "github", Origin: "~/.claude.json", Detail: "stdio: npx -y github-srv",
			Cfg: config.MCPServerConfig{Name: "github", Transport: "stdio", Command: "npx", Args: []string{"-y", "github-srv"}}},
		{Name: "web", Origin: "Claude Desktop", Detail: "https://web/mcp",
			Cfg: config.MCPServerConfig{Name: "web", Transport: "http", URL: "https://web/mcp"}},
	}
}

// driveWizard feeds answers one per step and fails unless the wizard is done
// with a committed config on the last one. It returns the final result and
// every step's output concatenated (intermediate confirmations land there).
func driveWizard(t *testing.T, st *initWizardState, answers []string) (initWizardResult, string) {
	t.Helper()
	var res initWizardResult
	var all strings.Builder
	for i, a := range answers {
		res = initWizardApply(st, a, localWizardOpts(1))
		all.WriteString(res.Output)
		wantDone := i == len(answers)-1
		if res.Done != wantDone {
			t.Fatalf("step %d (input %q): Done = %v, want %v (output+prompt %q)", i, a, res.Done, wantDone, res.Output+res.Prompt)
		}
	}
	if !res.Done || res.Committed == nil {
		t.Fatalf("final result = %+v, want Done with a committed config", res)
	}
	return res, all.String()
}

// TestInitWizard_ImportStepCommit: when the scan found MCP servers in other
// harnesses' configs, the wizard asks before the tuning gate; "all" imports
// both, adds them to the config and assigns them to the primary and
// escalation agents.
func TestInitWizard_ImportStepCommit(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))
	stubWizardDiscovery(t, nil, claudeAuthState{})
	stubHarnessScan(t, nil, importTestServers())
	pinClaudeOnPath(t, true)

	dir := t.TempDir()
	st, _ := initWizardStart(dir)
	st.scope = "global"

	// provider → url → model → escalation → import → tuning → commit
	res, allOut := driveWizard(t, st, []string{"1", "", "qwen2.5-coder", "", "all", ""})
	if !strings.Contains(allOut, "importing github, web") {
		t.Fatalf("import confirmation missing from %q", allOut)
	}
	cfg := *res.Committed
	if len(cfg.MCPServers) != 2 {
		t.Fatalf("committed %d MCP servers: %+v", len(cfg.MCPServers), cfg.MCPServers)
	}
	byName := map[string]config.MCPServerConfig{}
	for _, s := range cfg.MCPServers {
		byName[s.Name] = s
	}
	if s := byName["github"]; s.Transport != "stdio" || s.Command != "npx" || s.Origin != "~/.claude.json" {
		t.Errorf("github server wrong: %+v", s)
	}
	// Assigned to primary and escalation.
	for _, agent := range cfg.Agents {
		if len(agent.MCPServers) != 2 {
			t.Errorf("agent %q mcp_servers = %v, want both imports", agent.Name, agent.MCPServers)
		}
	}
	if !strings.Contains(res.Output, "imported 2 MCP server(s)") {
		t.Errorf("summary missing the import line: %q", res.Output)
	}
}

// TestInitWizard_ImportStepSkip: a blank answer imports nothing — the pick
// must be explicit — and the wizard finishes normally.
func TestInitWizard_ImportStepSkip(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))
	stubWizardDiscovery(t, nil, claudeAuthState{})
	stubHarnessScan(t, nil, importTestServers())
	pinClaudeOnPath(t, true)

	st, _ := initWizardStart(t.TempDir())
	st.scope = "global"
	res, allOut := driveWizard(t, st, []string{"1", "", "qwen2.5-coder", "", "", ""})
	if !strings.Contains(allOut, "skipped") {
		t.Errorf("output = %q, want the skip note", allOut)
	}
	if cfg := *res.Committed; len(cfg.MCPServers) != 0 {
		t.Errorf("blank import answer must import nothing: %+v", cfg.MCPServers)
	}
}

// TestInitWizardCommit_AdditiveKeepsExisting: the default commit merges the
// answers into the existing config — MCP servers, extra agents and every
// other setting survive; only the primary agent is (re)configured and the
// escalation is set up.
func TestInitWizardCommit_AdditiveKeepsExisting(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))
	stubWizardDiscovery(t, nil, claudeAuthState{})
	stubHarnessScan(t, nil, nil)
	pinClaudeOnPath(t, true)

	pre := config.Config{
		Agent: "custom",
		Agents: []config.AgentConfig{
			{Name: "custom", Provider: "bearer", URL: "https://old", Model: "old-model"},
		},
		MCPServers: []config.MCPServerConfig{{Name: "keep", URL: "http://keep/mcp"}},
		AgentTools: []config.AgentToolEntry{{Agent: "custom", Description: "mine"}},
		Rules:      config.Rules{EscalateAboveTokens: 12345},
	}
	if err := config.Save(pre); err != nil {
		t.Fatal(err)
	}

	st, _ := initWizardStart(t.TempDir())
	st.scope = "global"
	res, _ := driveWizard(t, st, []string{"1", "", "qwen2.5-coder", "", ""})
	cfg := *res.Committed

	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Name != "keep" {
		t.Errorf("existing MCP server lost: %+v", cfg.MCPServers)
	}
	if cfg.Rules.EscalateAboveTokens != 12345 {
		t.Errorf("routing rules reset: %+v", cfg.Rules)
	}
	names := map[string]bool{}
	for _, a := range cfg.Agents {
		names[a.Name] = true
	}
	if !names["custom"] || !names["local"] {
		t.Errorf("agents = %v, want the existing \"custom\" kept and the wizard's \"local\" added", names)
	}
	if cfg.Agent != "local" || cfg.EscalationAgent != "claude" {
		t.Errorf("agent = %q, escalation = %q", cfg.Agent, cfg.EscalationAgent)
	}
	if len(cfg.AgentTools) != 1 || cfg.AgentTools[0].Agent != "custom" {
		t.Errorf("agent_tools = %+v, want the existing entry kept", cfg.AgentTools)
	}
	if !strings.Contains(res.Output, "kept your existing settings") {
		t.Errorf("summary missing the additive note: %q", res.Output)
	}
	// And the file on disk is the committed one.
	onDisk, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk.MCPServers) != 1 || onDisk.Rules.EscalateAboveTokens != 12345 {
		t.Errorf("on-disk config not the additive result: %+v", onDisk)
	}
}

// TestInitWizardCommit_RebuildResetsAndWarns: /config init rebuild keeps the
// pre-additive semantics — the config is rebuilt from the answers alone — and
// the summary warns about what the rebuild reset.
func TestInitWizardCommit_RebuildResetsAndWarns(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))
	stubWizardDiscovery(t, nil, claudeAuthState{})
	stubHarnessScan(t, nil, nil)
	pinClaudeOnPath(t, true)

	pre := config.Config{
		Agents:     []config.AgentConfig{{Name: "custom", Provider: "bearer", URL: "https://old", Model: "m"}},
		MCPServers: []config.MCPServerConfig{{Name: "keep", URL: "http://keep/mcp"}},
		Rules:      config.Rules{EscalateAboveTokens: 12345},
	}
	if err := config.Save(pre); err != nil {
		t.Fatal(err)
	}

	st, _ := initWizardStart(t.TempDir())
	st.scope = "global"
	st.rebuild = true
	res, _ := driveWizard(t, st, []string{"1", "", "qwen2.5-coder", "", ""})
	cfg := *res.Committed

	if len(cfg.MCPServers) != 0 {
		t.Errorf("rebuild kept MCP servers: %+v", cfg.MCPServers)
	}
	for _, a := range cfg.Agents {
		if a.Name == "custom" {
			t.Errorf("rebuild kept the previous agent: %+v", cfg.Agents)
		}
	}
	if cfg.Rules.EscalateAboveTokens != 2000 {
		t.Errorf("rebuild kept custom rules: %+v", cfg.Rules)
	}
	if !strings.Contains(res.Output, "note: rebuild rebuilt the config") ||
		!strings.Contains(res.Output, "keep") ||
		!strings.Contains(res.Output, "config.json.bak") {
		t.Errorf("summary missing the reset warning: %q", res.Output)
	}
}

// TestInitWizardTake_LiteralKey: a credential found only in another harness's
// file is copied into the config like a typed key (with a note saying so),
// and the now-redundant API-key question is skipped.
func TestInitWizardTake_LiteralKey(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubWizardModels(t, nil, errors.New("no server"))
	pinClaudeOnPath(t, true)
	f := initWizardFinding{
		Name: "openrouter", Label: "OpenRouter", Provider: "bearer",
		URL: "https://openrouter.ai/api", Value: "sk-literal-1234567890",
		Origin: "~/.claude/settings.json", Detail: "key in ~/.claude/settings.json (sk-l…7890)",
	}
	stubWizardDiscovery(t, []initWizardFinding{f}, claudeAuthState{})
	stubHarnessScan(t, nil, nil)

	st, _ := initWizardStart(t.TempDir())
	st.scope = "global"

	res := initWizardApply(st, "1", localWizardOpts(1))
	if !strings.Contains(res.Output, "copied from ~/.claude/settings.json") {
		t.Errorf("take output = %q, want the copy note", res.Output)
	}
	if st.primary.APIKey != "sk-literal-1234567890" || st.primary.APIKeyEnv != "" {
		t.Errorf("primary = %+v, want the literal in api_key and no env pointer", st.primary)
	}
	// model → escalation: the API-key question must not come back.
	res = initWizardApply(st, "gpt-4o-mini", localWizardOpts(1))
	if strings.Contains(res.Prompt, "API key") {
		t.Errorf("API key asked although the finding carried one: %q", res.Prompt)
	}
	if !strings.Contains(res.Prompt, "escalation agent?") {
		t.Errorf("prompt = %q, want the escalation question", res.Prompt)
	}
}
