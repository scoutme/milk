package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/plugin"
)

// setTestPlugins swaps the process-wide registry for one test and restores
// the runtime seams afterwards (the config hook and skill catalog are
// process-global by design — startup wiring).
func setTestPlugins(t *testing.T, list []loadedPlugin) {
	t.Helper()
	pluginsMu.Lock()
	prev, prevLog := plugins, pluginLog
	plugins, pluginLog = list, nil
	pluginsMu.Unlock()
	t.Cleanup(func() {
		pluginsMu.Lock()
		plugins, pluginLog = prev, prevLog
		pluginsMu.Unlock()
		config.SetPluginMCPServers(nil)
		local.SetSkillCatalog(nil)
	})
}

func TestPluginServersForAgent_Assignment(t *testing.T) {
	p := &plugin.Plugin{Manifest: plugin.Manifest{Name: "x"}}
	setTestPlugins(t, []loadedPlugin{
		{plugin: p, servers: []config.MCPServerConfig{{Name: "plugin.x.s"}}, assign: []string{"primary"}},
		{plugin: &plugin.Plugin{Manifest: plugin.Manifest{Name: "y"}}, servers: []config.MCPServerConfig{{Name: "plugin.y.s"}}},
	})
	if got := pluginServersForAgent("primary"); len(got) != 2 {
		t.Fatalf("primary must see both: %+v", got)
	}
	if got := pluginServersForAgent("escalation"); len(got) != 1 || got[0].Name != "plugin.y.s" {
		t.Fatalf("escalation must see only unassigned plugins: %+v", got)
	}
}

func TestInitPlugins_EndToEnd(t *testing.T) {
	t.Cleanup(func() {
		config.SetPluginMCPServers(nil)
		local.SetSkillCatalog(nil)
	})
	stateRoot, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(stateRoot, "plugins", "demo")
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("plugin.json", `{"$schema": "`+plugin.SchemaPlugin+`", "name": "demo",
		"extensions": {"dev.milkai.milk": {"mcp_assign": ["primary"]}}}`)
	write("skills/ship/SKILL.md", "---\nname: ship\ndescription: Ship safely.\n---\n# Ship\n")
	write("mcp.json", `{"$schema": "`+plugin.SchemaMCP+`", "mcpServers": {
		"tools": {"type": "stdio", "command": "true"}}}`)

	initPlugins(config.Config{})

	cfg := config.Config{Agents: []config.AgentConfig{
		{Name: "primary"},
		{Name: "escalation"},
	}}
	// EffectiveMCPServers resolves agent refs + the plugin seam; with no
	// refs the plugin seam still contributes for assigned agents.
	if servers := cfg.EffectiveMCPServers("primary"); len(servers) != 1 || servers[0].Name != "plugin.demo.tools" {
		t.Fatalf("primary plugin servers: %+v", servers)
	}
	if servers := cfg.EffectiveMCPServers("escalation"); len(servers) != 0 {
		t.Fatalf("escalation must not see the assigned-only plugin: %+v", servers)
	}

	// Skills: catalog seeded, tool schema carries the listing, dispatch
	// re-reads the body from disk.
	entries := local.SkillCatalog()
	if len(entries) != 1 || entries[0].Name != "ship" {
		t.Fatalf("skill catalog: %+v", entries)
	}

	// /plugins render mentions the plugin and its components.
	out := handlePluginsCmd()
	for _, want := range []string{"demo", "ship", "mcp: tools", "assigned agents: primary"} {
		if !strings.Contains(out, want) {
			t.Errorf("/plugins output missing %q:\n%s", want, out)
		}
	}
}
