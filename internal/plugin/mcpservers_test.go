package plugin

import (
	"strings"
	"testing"
)

func TestMilkServers_Mapping(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "plugin.json", minManifest)
	writePlugin(t, root, "mcp.json", `{"$schema": "`+SchemaMCP+`", "mcpServers": {
		"local": {"type": "stdio", "command": "tool", "args": ["--x"], "env": {"K": "v"}, "cwd": "${PLUGIN_ROOT}"},
		"remote": {"type": "streamable-http", "url": "https://example.com/mcp", "headers": {"X-Tenant": "t"}},
		"legacy": {"type": "sse", "url": "https://legacy.example.com/sse"}
	}}`)
	p, err := Load(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	servers, warns := p.MilkServers()

	byName := map[string]bool{}
	for _, s := range servers {
		byName[s.Name] = true
	}
	// Namespaced names; legacy sse skipped with a report (§11.3 rule 4).
	if !byName["plugin.my-plugin.local"] || !byName["plugin.my-plugin.remote"] {
		t.Fatalf("mapped names: %v (%v)", byName, servers)
	}
	if len(servers) != 2 {
		t.Fatalf("want 2 mapped (sse skipped), got %d", len(servers))
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "sse") {
		t.Fatalf("want sse skip warning, got %v", warns)
	}
	for _, s := range servers {
		if strings.HasPrefix(s.Name, "plugin.my-plugin.local") {
			if s.Transport != "stdio" || s.Command != "tool" || s.Env["K"] != "v" || s.Cwd == "" {
				t.Errorf("stdio mapping: %+v", s)
			}
			if s.PluginRoot != p.Root || s.PluginData != p.DataDir {
				t.Errorf("reserved vars missing: %+v", s)
			}
		} else {
			if s.Transport != "http" || s.Headers["X-Tenant"] != "t" {
				t.Errorf("remote mapping: %+v", s)
			}
		}
	}
}
