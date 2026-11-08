package plugin

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPFile_WholeFileRules(t *testing.T) {
	ok := func(raw, pluginSchema string) {
		t.Helper()
		if _, _, err := ParseMCPFile([]byte(raw), pluginSchema); err != nil {
			t.Errorf("want ok: %v", err)
		}
	}
	bad := func(raw, pluginSchema string) {
		t.Helper()
		if _, _, err := ParseMCPFile([]byte(raw), pluginSchema); err == nil {
			t.Errorf("want failure for %s", raw)
		}
	}
	ok(`{"$schema": "`+SchemaMCP+`", "mcpServers": {}}`, SchemaPlugin)
	ok(`{"$schema": "`+SchemaMCP+`", "mcpServers": {"s": {"type": "stdio", "command": "x"}}}`, SchemaPlugin)
	bad(`{"mcpServers": {}}`, SchemaPlugin)                                                                       // missing $schema
	bad(`{"$schema": "`+SchemaMCP+`"}`, SchemaPlugin)                                                             // missing mcpServers
	bad(`{"$schema": "`+SchemaMCP+`", "mcpServers": []}`, SchemaPlugin)                                           // not an object
	bad(`{"$schema": "`+SchemaMCP+`", "mcpServers": {}, "extra": 1}`, SchemaPlugin)                               // closed top level
	bad(`{"$schema": "https://agent-plugins.org/schemas/1.1.0/mcp.schema.json", "mcpServers": {}}`, SchemaPlugin) // unsupported version
	// Version mismatch between plugin.json and mcp.json (§10.1).
	bad(`{"$schema": "`+SchemaMCP+`", "mcpServers": {}}`, "https://agent-plugins.org/schemas/1.1.0/plugin.schema.json")
}

func TestMCPFile_EntryShape(t *testing.T) {
	load := func(entries string) (*Plugin, map[string]MCPServer) {
		t.Helper()
		root := t.TempDir()
		writePlugin(t, root, "plugin.json", minManifest)
		writePlugin(t, root, "mcp.json", `{"$schema": "`+SchemaMCP+`", "mcpServers": `+entries+`}`)
		p, err := Load(root, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if p.MCPDisabled {
			t.Fatalf("whole file must load: %v", p.Warnings)
		}
		return p, p.MCP.Servers
	}
	skipped := func(label, entry string) {
		t.Helper()
		p, srv := load(`{"s": ` + entry + `}`)
		if _, ok := srv["s"]; ok {
			t.Errorf("%s: entry must be skipped", label)
		}
		if !hasWarning(p, `"s"`) {
			t.Errorf("%s: want skip warning, got %v", label, p.Warnings)
		}
	}

	// Valid variants.
	_, srv := load(`{"s": {"type": "stdio", "command": "bare"}}`)
	if srv["s"].Stdio == nil || srv["s"].Stdio.Command != "bare" {
		t.Fatalf("bare stdio: %+v", srv["s"])
	}

	// Closed union violations (§7.2.1): unknown fields, unknown type,
	// cross-variant fields — entry invalid (§7.2.2 rule 3).
	skipped("unknown field", `{"type": "stdio", "command": "x", "bogus": 1}`)
	skipped("unknown type", `{"type": "websocket", "url": "https://x"}`)
	skipped("cross variant", `{"type": "stdio", "command": "x", "url": "https://x"}`)
	skipped("missing type", `{"command": "x"}`)
	skipped("reserved env", `{"type": "stdio", "command": "x", "env": {"PLUGIN_ROOT": "/evil"}}`)
	skipped("command tokens", `{"type": "stdio", "command": "two words"}`)
	skipped("command abs path", `{"type": "stdio", "command": "/usr/bin/x"}`)
	skipped("args not strings", `{"type": "stdio", "command": "x", "args": [1]}`)
	skipped("cwd bare", `{"type": "stdio", "command": "x", "cwd": "data"}`)
	skipped("url userinfo", `{"type": "streamable-http", "url": "https://user@example.com/mcp"}`)
	skipped("url fragment", `{"type": "streamable-http", "url": "https://example.com/mcp#frag"}`)
	skipped("url non-loopback http", `{"type": "streamable-http", "url": "http://example.com/mcp"}`)
	skipped("url not absolute", `{"type": "streamable-http", "url": "/mcp"}`)
	skipped("dup headers", `{"type": "sse", "url": "https://example.com/sse", "headers": {"X-A": "1", "x-a": "2"}}`)
	skipped("header CRLF", `{"type": "sse", "url": "https://example.com/sse", "headers": {"X-A": "a\r\nb"}}`)
}

func TestMCPFile_RemoteAndLoopback(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "plugin.json", minManifest)
	writePlugin(t, root, "mcp.json", `{"$schema": "`+SchemaMCP+`", "mcpServers": {
		"http-local": {"type": "streamable-http", "url": "http://localhost:8080/mcp"},
		"http-ip":    {"type": "streamable-http", "url": "http://127.0.0.1:1/mcp"},
		"sse":        {"type": "sse", "url": "https://legacy.example.com/sse"}
	}}`)
	p, err := Load(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.MCP.Servers) != 3 {
		t.Fatalf("want 3 servers, got %+v", p.MCP.Servers)
	}
}

func TestMCP_StdioExpansionAndCwd(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(t.TempDir(), "data-root")
	writePlugin(t, root, "plugin.json", minManifest)
	writePlugin(t, root, "bin/tool", "#!/bin/sh\n")
	writePlugin(t, root, "mcp.json", `{"$schema": "`+SchemaMCP+`", "mcpServers": {
		"srv": {
			"type": "stdio",
			"command": "./bin/tool",
			"args": ["--config", "${PLUGIN_ROOT}/config.json", "${PLUGIN_DATA}/cache", "${NOPE}"],
			"env": {"DATA": "${PLUGIN_DATA}/x", "LITERAL": "has ${PLUGIN_ROOT} text"},
			"cwd": "${PLUGIN_ROOT}/bin"
		}
	}}`)
	p, err := Load(root, data)
	if err != nil {
		t.Fatal(err)
	}
	st := p.MCP.Servers["srv"].Stdio
	if st == nil {
		t.Fatal("server must load")
	}
	if st.Command != filepath.Join(root, "bin", "tool") {
		t.Errorf("command: %q", st.Command)
	}
	wantArgs := []string{"--config", filepath.Join(root, "config.json"), filepath.Join(data, "my-plugin", "cache"), "${NOPE}"}
	for i, w := range wantArgs {
		if st.Args[i] != w {
			t.Errorf("args[%d] = %q, want %q", i, st.Args[i], w)
		}
	}
	if st.Env["DATA"] != filepath.Join(data, "my-plugin", "x") {
		t.Errorf("env DATA: %q", st.Env["DATA"])
	}
	if st.Env["LITERAL"] != "has "+root+" text" {
		t.Errorf("env LITERAL: %q", st.Env["LITERAL"])
	}
	if st.Cwd != filepath.Join(root, "bin") {
		t.Errorf("cwd: %q", st.Cwd)
	}
	// Unknown placeholders stay literal (§9.2).
	if st.Args[3] != "${NOPE}" {
		t.Errorf("unknown placeholder must stay literal: %q", st.Args[3])
	}
}

func TestMCP_CwdForms(t *testing.T) {
	run := func(cwd string) error {
		root := t.TempDir()
		writePlugin(t, root, "plugin.json", minManifest)
		writePlugin(t, root, "mcp.json", `{"$schema": "`+SchemaMCP+`", "mcpServers": {
			"s": {"type": "stdio", "command": "x", "cwd": `+jsonStr(cwd)+`}
		}}`)
		_, err := Load(root, t.TempDir())
		return err
	}
	// Validate at the entry level via a full load: forms that make the entry
	// invalid leave the plugin usable but the server absent.
	entryOk := func(cwd string, wantOk bool) {
		t.Helper()
		root := t.TempDir()
		writePlugin(t, root, "plugin.json", minManifest)
		writePlugin(t, root, "mcp.json", `{"$schema": "`+SchemaMCP+`", "mcpServers": {
			"s": {"type": "stdio", "command": "x", "cwd": `+jsonStr(cwd)+`}
		}}`)
		p, err := Load(root, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		_, loaded := p.MCP.Servers["s"]
		if loaded != wantOk {
			t.Errorf("cwd %q: loaded=%v want=%v (warnings %v)", cwd, loaded, wantOk, p.Warnings)
		}
	}
	_ = run
	entryOk("./data", true)
	entryOk("${PLUGIN_ROOT}", true)
	entryOk("${PLUGIN_ROOT}/sub", true)
	entryOk("${PLUGIN_DATA}", true)
	entryOk("${PLUGIN_DATA}/sub", true)
	entryOk("${PLUGIN_DATA}/../escape", false)
	entryOk("./../escape", false)
	entryOk("data", false)
	entryOk("/abs", false)
}

func TestExpand_NonRecursiveSinglePass(t *testing.T) {
	// Replacement text is never rescanned (§9.2): the plugin root here
	// literally contains ${PLUGIN_DATA}, which must survive untouched.
	got := Expand("a ${PLUGIN_ROOT} b ${PLUGIN_DATA}", "/root/${PLUGIN_DATA}", "/data")
	want := "a /root/${PLUGIN_DATA} b /data"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if Expand("no placeholders ${NOPE}", "/r", "/d") != "no placeholders ${NOPE}" {
		t.Fatal("unrecognized placeholders must stay literal")
	}
}

func jsonStr(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}
