package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePlugin(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const minManifest = `{"$schema": "` + SchemaPlugin + `", "name": "my-plugin"}`

func TestManifest_Minimal(t *testing.T) {
	m, warns, err := ParseManifest([]byte(minManifest))
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	if m.Name != "my-plugin" || m.Schema != SchemaPlugin {
		t.Fatalf("got %+v", m)
	}
}

func TestManifest_Full(t *testing.T) {
	raw := `{
		"$schema": "` + SchemaPlugin + `",
		"name": "plugin-name",
		"version": "1.2.0",
		"description": "Brief plugin description",
		"author": {"name": "Author Name", "email": "author@example.com", "url": "https://example.com"},
		"homepage": "https://docs.example.com/plugin",
		"repository": "https://github.com/example/plugin",
		"license": "MIT",
		"keywords": ["keyword1", "keyword2"],
		"extensions": {"dev.milkai.milk": {"setting": true}}
	}`
	m, warns, err := ParseManifest([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	if m.Author.Name != "Author Name" || len(m.Keywords) != 2 || m.Extensions["dev.milkai.milk"]["setting"] != true {
		t.Fatalf("got %+v", m)
	}
}

func TestManifest_UnknownFieldReportedIgnored(t *testing.T) {
	raw := `{"$schema": "` + SchemaPlugin + `", "name": "x", "bogus": 1}`
	m, warns, err := ParseManifest([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "x" {
		t.Fatalf("plugin must keep loading: %+v", m)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], `"bogus"`) {
		t.Fatalf("want unknown-field warning, got %v", warns)
	}
}

func TestManifest_Rejections(t *testing.T) {
	cases := map[string]string{
		"missing schema":  `{"name": "x"}`,
		"wrong schema":    `{"$schema": "https://agent-plugins.org/schemas/1.1.0/plugin.schema.json", "name": "x"}`,
		"missing name":    `{"$schema": "` + SchemaPlugin + `"}`,
		"empty name":      `{"$schema": "` + SchemaPlugin + `", "name": ""}`,
		"name uppercase":  `{"$schema": "` + SchemaPlugin + `", "name": "My-Plugin"}`,
		"name leading -":  `{"$schema": "` + SchemaPlugin + `", "name": "-start"}`,
		"name double -":   `{"$schema": "` + SchemaPlugin + `", "name": "has--double"}`,
		"name double ..":  `{"$schema": "` + SchemaPlugin + `", "name": "too.many..dots"}`,
		"name too long":   `{"$schema": "` + SchemaPlugin + `", "name": "` + strings.Repeat("a", 65) + `"}`,
		"version number":  `{"$schema": "` + SchemaPlugin + `", "name": "x", "version": 2}`,
		"keywords string": `{"$schema": "` + SchemaPlugin + `", "name": "x", "keywords": "a"}`,
		"author bad key":  `{"$schema": "` + SchemaPlugin + `", "name": "x", "author": {"phone": "1"}}`,
		"author bad type": `{"$schema": "` + SchemaPlugin + `", "name": "x", "author": {"name": 4}}`,
		"ext member scal": `{"$schema": "` + SchemaPlugin + `", "name": "x", "extensions": {"dev.a.b": 1}}`,
		"top level array": `[]`,
	}
	for label, raw := range cases {
		if _, _, err := ParseManifest([]byte(raw)); err == nil {
			t.Errorf("%s: expected rejection", label)
		}
	}
}

func TestManifest_ValidNames(t *testing.T) {
	for _, name := range []string{"my-plugin", "acme.tools", "lint3r", "a"} {
		raw := `{"$schema": "` + SchemaPlugin + `", "name": "` + name + `"}`
		if _, _, err := ParseManifest([]byte(raw)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestManifest_ExtensionsNotObjectIsWarning(t *testing.T) {
	raw := `{"$schema": "` + SchemaPlugin + `", "name": "x", "extensions": "oops"}`
	_, warns, err := ParseManifest([]byte(raw))
	if err != nil {
		t.Fatalf("must not be fatal: %v", err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "extensions") {
		t.Fatalf("want extensions warning, got %v", warns)
	}
}

// --- Load: failure boundaries (§4.1, §11.3) ---

func TestLoad_Boundaries(t *testing.T) {
	t.Run("manifest fatal rejects plugin", func(t *testing.T) {
		root := t.TempDir()
		writePlugin(t, root, "plugin.json", `{"name": "no-schema"}`)
		if _, err := Load(root, t.TempDir()); err == nil {
			t.Fatal("want rejection")
		}
	})

	t.Run("skills wrong kind disables skills only", func(t *testing.T) {
		root := t.TempDir()
		writePlugin(t, root, "plugin.json", minManifest)
		writePlugin(t, root, "skills", "i am a file")
		writePlugin(t, root, "mcp.json", `{"$schema": "`+SchemaMCP+`", "mcpServers": {}}`)
		p, err := Load(root, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if p.MCP == nil || p.MCPDisabled {
			t.Fatalf("mcp must survive: %+v", p.MCP)
		}
		if !hasWarning(p, "skills") {
			t.Fatalf("want skills warning, got %v", p.Warnings)
		}
	})

	t.Run("invalid mcp.json disables mcp only", func(t *testing.T) {
		root := t.TempDir()
		writePlugin(t, root, "plugin.json", minManifest)
		writePlugin(t, root, "mcp.json", `{not json`)
		writePlugin(t, root, "skills/sum/SKILL.md", "---\nname: sum\ndescription: does sums\n---\nbody")
		p, err := Load(root, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if !p.MCPDisabled || p.MCP != nil {
			t.Fatalf("mcp must be disabled: %+v", p)
		}
		if len(p.Skills) != 1 {
			t.Fatalf("skills must survive: %+v", p.Skills)
		}
	})

	t.Run("one bad server skips only that entry", func(t *testing.T) {
		root := t.TempDir()
		writePlugin(t, root, "plugin.json", minManifest)
		writePlugin(t, root, "mcp.json", `{
			"$schema": "`+SchemaMCP+`",
			"mcpServers": {
				"good": {"type": "streamable-http", "url": "https://example.com/mcp"},
				"bad": {"type": "stdio", "command": "two words"}
			}
		}`)
		p, err := Load(root, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if p.MCPDisabled {
			t.Fatalf("whole-file must stay enabled: %v", p.Warnings)
		}
		if _, ok := p.MCP.Servers["good"]; !ok {
			t.Fatal("good server must load")
		}
		if _, ok := p.MCP.Servers["bad"]; ok {
			t.Fatal("bad server must be skipped")
		}
		if !hasWarning(p, `"bad"`) {
			t.Fatalf("want skip warning, got %v", p.Warnings)
		}
	})

	t.Run("data dir derived from plugin name", func(t *testing.T) {
		root := t.TempDir()
		writePlugin(t, root, "plugin.json", minManifest)
		p, err := Load(root, "/data/root")
		if err != nil {
			t.Fatal(err)
		}
		if p.DataDir != filepath.Join("/data/root", "my-plugin") {
			t.Fatalf("got %q", p.DataDir)
		}
	})
}

func TestDiscover(t *testing.T) {
	base := t.TempDir()
	data := t.TempDir()
	user := filepath.Join(base, "user")
	writePlugin(t, filepath.Join(user, "alpha"), "plugin.json", `{"$schema": "`+SchemaPlugin+`", "name": "alpha"}`)
	writePlugin(t, filepath.Join(user, "beta"), "plugin.json", `{"$schema": "`+SchemaPlugin+`", "name": "beta"}`)
	writePlugin(t, filepath.Join(user, "broken"), "plugin.json", `{"name": "no-schema"}`)
	writePlugin(t, filepath.Join(user, "not-a-plugin"), "README", "hi")

	plugins, report := Discover(data, filepath.Join(base, "missing"), user)
	if len(plugins) != 2 {
		t.Fatalf("got %d plugins: %+v", len(plugins), plugins)
	}
	if plugins[0].Name() != "alpha" || plugins[1].Name() != "beta" {
		t.Fatalf("deterministic order: %s %s", plugins[0].Name(), plugins[1].Name())
	}
	if len(report) != 1 || !strings.Contains(report[0], "broken") {
		t.Fatalf("want one broken report, got %v", report)
	}
}

func hasWarning(p *Plugin, substr string) bool {
	for _, w := range p.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
