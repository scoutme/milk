package main

// Tests for the harness-import scan (harness_import.go): the tolerant parse
// of Claude Code / Claude Desktop config files, the MCP entry conversion, and
// the credential findings that feed the wizard's first question.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

// stubHarnessHome pins the home directory the scan reads, returning the
// fixture home so tests can write files into it.
func stubHarnessHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	prev := harnessHomeDir
	harnessHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { harnessHomeDir = prev })
	return home
}

func writeHarnessFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestClaudeMCPServer_Conversion pins the Claude → milk MCP entry mapping:
// command/args/env/cwd → stdio, url/type/headers → http|sse, and "no command,
// no URL" is not importable at all.
func TestClaudeMCPServer_Conversion(t *testing.T) {
	cases := []struct {
		name   string
		raw    map[string]any
		wantOK bool
		check  func(t *testing.T, cfg importedMCPServer)
	}{
		{
			name: "stdio",
			raw: map[string]any{
				"command": "npx", "args": []any{"-y", "srv"},
				"env": map[string]any{"K": "V"}, "cwd": "/work",
			},
			wantOK: true,
			check: func(t *testing.T, s importedMCPServer) {
				if s.Cfg.Transport != "stdio" || s.Cfg.Command != "npx" ||
					len(s.Cfg.Args) != 2 || s.Cfg.Env["K"] != "V" || s.Cfg.Cwd != "/work" {
					t.Errorf("stdio conversion wrong: %+v", s.Cfg)
				}
				if s.Detail != "stdio: npx -y srv" {
					t.Errorf("detail = %q", s.Detail)
				}
			},
		},
		{
			name:   "http",
			raw:    map[string]any{"type": "http", "url": "https://x/mcp", "headers": map[string]any{"H": "h"}},
			wantOK: true,
			check: func(t *testing.T, s importedMCPServer) {
				if s.Cfg.Transport != "http" || s.Cfg.URL != "https://x/mcp" || s.Cfg.Headers["H"] != "h" {
					t.Errorf("http conversion wrong: %+v", s.Cfg)
				}
			},
		},
		{
			name:   "sse",
			raw:    map[string]any{"type": "sse", "url": "https://x/sse"},
			wantOK: true,
			check: func(t *testing.T, s importedMCPServer) {
				if s.Cfg.Transport != "sse" {
					t.Errorf("sse conversion wrong: %+v", s.Cfg)
				}
			},
		},
		{
			name:   "url without type defaults to http",
			raw:    map[string]any{"url": "https://x/mcp"},
			wantOK: true,
			check: func(t *testing.T, s importedMCPServer) {
				if s.Cfg.Transport != "http" {
					t.Errorf("transport = %q, want http", s.Cfg.Transport)
				}
			},
		},
		{name: "neither command nor url", raw: map[string]any{"args": []any{"x"}}, wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, ok := claudeMCPServer("srv", c.raw, "test")
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok {
				c.check(t, s)
			}
		})
	}
}

// TestHarnessImportScan_ClaudeSources drives the whole scan over a fixture
// machine: project settings first (wins on name clashes), then the user
// settings, ~/.claude.json (global + per-project), then Claude Desktop. Also
// pins the credential findings: literals from the settings env maps, with
// ANTHROPIC_BASE_URL / ANTHROPIC_MODEL reshaping the Anthropic entry.
func TestHarnessImportScan_ClaudeSources(t *testing.T) {
	home := stubHarnessHome(t)
	cwd := t.TempDir()

	writeHarnessFile(t, filepath.Join(cwd, ".claude", "settings.local.json"), `{
		"env": {"GROQ_API_KEY": "gsk_project_literal_123456"},
		"mcpServers": {
			"dup": {"command": "project-version", "args": ["a"]},
			"proj": {"type": "sse", "url": "https://proj/sse", "headers": {"A": "b"}}
		}
	}`)
	writeHarnessFile(t, filepath.Join(home, ".claude", "settings.json"), `{
		"env": {
			"OPENROUTER_API_KEY": "sk-or-literal-abcdef123456",
			"ANTHROPIC_API_KEY": "sk-ant-literal-abcdef123456",
			"ANTHROPIC_BASE_URL": "https://relay.example.com/",
			"ANTHROPIC_MODEL": "claude-sonnet-4.6",
			"UNRELATED": "ignored"
		},
		"mcpServers": {
			"dup": {"command": "user-version", "args": []},
			"settings": {"command": "npx", "args": ["-y", "settings-srv"], "env": {"TOK": "secret-value"}}
		}
	}`)
	writeHarnessFile(t, filepath.Join(home, ".claude.json"), `{
		"mcpServers": {"global1": {"command": "g", "args": ["1"]}},
		"projects": {
			`+strconvQuote(filepath.ToSlash(filepath.Clean(cwd)))+`: {
				"mcpServers": {"proj2": {"url": "https://proj2/mcp"}}
			},
			"/other/project": {"mcpServers": {"notmine": {"command": "n"}}}
		}
	}`)
	writeHarnessFile(t, claudeDesktopConfigPath(home), `{
		"mcpServers": {"desktop": {"command": "d", "args": [], "env": {"D": "1"}}}
	}`)

	creds, servers := harnessImportScan(cwd)

	// Servers: 6 sources, "dup" deduped in favour of the project file.
	if len(servers) != 6 {
		t.Fatalf("got %d servers: %+v", len(servers), servers)
	}
	byName := map[string]importedMCPServer{}
	for _, s := range servers {
		byName[s.Name] = s
	}
	if s := byName["dup"]; s.Cfg.Command != "project-version" || !strings.Contains(s.Origin, "settings.local.json") {
		t.Errorf("dup not won by the project file: %+v", s)
	}
	if s := byName["proj2"]; s.Cfg.URL != "https://proj2/mcp" || !strings.Contains(s.Origin, ".claude.json") {
		t.Errorf("per-project server wrong: %+v", s)
	}
	if _, ok := byName["notmine"]; ok {
		t.Errorf("another project's server was imported")
	}
	for _, name := range []string{"proj", "settings", "global1", "desktop"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("server %q missing (have %v)", name, byName)
		}
	}
	if s := byName["settings"]; s.Cfg.Origin == "" || s.Cfg.Env["TOK"] != "secret-value" {
		t.Errorf("settings server missing origin/env: %+v", s.Cfg)
	}

	// Credentials: only known key sources, only from env maps (UNRELATED is
	// not one), each with the file it came from.
	if len(creds) != 3 {
		t.Fatalf("got %d creds: %+v", len(creds), creds)
	}
	byEnv := map[string]importedCredential{}
	for _, c := range creds {
		byEnv[c.EnvVar] = c
	}
	if c := byEnv["OPENROUTER_API_KEY"]; c.Value != "sk-or-literal-abcdef123456" || !strings.Contains(c.Origin, "settings.json") {
		t.Errorf("openrouter cred wrong: %+v", c)
	}
	if c := byEnv["GROQ_API_KEY"]; c.Value != "gsk_project_literal_123456" || !strings.Contains(c.Origin, "settings.local.json") {
		t.Errorf("groq cred wrong: %+v", c)
	}
	if c := byEnv["ANTHROPIC_API_KEY"]; c.URL != "https://relay.example.com" || c.Model != "claude-sonnet-4.6" {
		t.Errorf("anthropic relay overrides not applied: %+v", c)
	}
	if _, ok := byEnv["UNRELATED"]; ok {
		t.Errorf("unknown env var became a credential")
	}
}

// TestInitWizardImportPick pins the import step's answer grammar: blank and
// the explicit no-words import nothing (a pick must be explicit), "all" takes
// everything, otherwise numbers or names — with a validation message for
// anything else.
func TestInitWizardImportPick(t *testing.T) {
	servers := []importedMCPServer{
		{Name: "one"}, {Name: "two"}, {Name: "three"},
	}
	cases := []struct {
		in   string
		want []string
		err  bool
	}{
		{"", nil, false},
		{"none", nil, false},
		{"skip", nil, false},
		{"all", []string{"one", "two", "three"}, false},
		{"1,3", []string{"one", "three"}, false},
		{"two", []string{"two"}, false},
		{"One, TWO", []string{"one", "two"}, false},
		{"1,1", []string{"one"}, false},
		{"4", nil, true},
		{"nope", nil, true},
	}
	for _, c := range cases {
		picked, errMsg := initWizardImportPick(servers, c.in)
		if c.err != (errMsg != "") {
			t.Errorf("initWizardImportPick(%q) errMsg = %q, want err = %v", c.in, errMsg, c.err)
			continue
		}
		var names []string
		for _, s := range picked {
			names = append(names, s.Name)
		}
		if strings.Join(names, ",") != strings.Join(c.want, ",") {
			t.Errorf("initWizardImportPick(%q) = %v, want %v", c.in, names, c.want)
		}
	}
}

// TestFoldCredentialFindings pins how harness credentials join the machine
// scan's findings: an exported variable stays a pointer finding (value never
// copied), a file-only literal carries Value + its origin, and an existing
// finding for the same variable is enriched with the file's URL/model
// overrides instead of duplicated.
func TestFoldCredentialFindings(t *testing.T) {
	t.Setenv("EXPORTED_KEY", "exported-value-123456")
	creds := []importedCredential{
		{Name: "a", Label: "A", EnvVar: "EXPORTED_KEY", Value: "file-value-123456", Origin: "~/.claude/settings.json", URL: "https://a"},
		{Name: "b", Label: "B", EnvVar: "FILE_ONLY_KEY", Value: "literal-value-123456", Origin: "~/.claude/settings.json", URL: "https://b"},
		{Name: "anthropic", Label: "Anthropic API", EnvVar: "ANTHROPIC_API_KEY", Value: "x", URL: "https://relay", Model: "m1"},
	}
	found := []initWizardFinding{
		{KeyEnv: "ANTHROPIC_API_KEY", URL: "https://api.anthropic.com"},
	}

	out := foldCredentialFindings(found, creds)
	if len(out) != 3 {
		t.Fatalf("got %d findings: %+v", len(out), out)
	}
	if out[0].KeyEnv != "ANTHROPIC_API_KEY" || out[0].URL != "https://relay" || out[0].Model != "m1" {
		t.Errorf("existing finding not enriched: %+v", out[0])
	}
	if out[1].KeyEnv != "EXPORTED_KEY" || out[1].Value != "" || !strings.Contains(out[1].Detail, "EXPORTED_KEY") {
		t.Errorf("exported key must stay a pointer finding: %+v", out[1])
	}
	if out[2].Value != "literal-value-123456" || out[2].KeyEnv != "" || !strings.Contains(out[2].Detail, "key in ~/.claude/settings.json") {
		t.Errorf("file-only key must carry the literal and its origin: %+v", out[2])
	}
	if strings.Contains(out[2].Detail, "literal-value") {
		t.Errorf("finding detail leaked the secret: %q", out[2].Detail)
	}
}

// TestExecMCPImport: /mcp import without arguments lists what the scan
// found; "all" imports everything (origin-tagged, assigned to primary and
// escalation) and saves; already-configured names are skipped, never
// overwritten.
func TestExecMCPImport(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	stubHarnessScan(t, nil, importTestServers())
	st := &interactiveState{
		cfg: config.Config{
			Agent:           "local",
			Agents:          []config.AgentConfig{{Name: "local", Provider: "local"}, {Name: "claude", Provider: "claude-cli"}},
			EscalationAgent: "claude",
		},
		cwd: t.TempDir(),
	}

	if out := execMCPImport("", st); !strings.Contains(out, "github") || !strings.Contains(out, "/mcp import all") {
		t.Fatalf("listing = %q", out)
	}
	out := execMCPImport("all", st)
	if !strings.Contains(out, "imported 2 MCP server(s)") || !strings.Contains(out, "assigned to local, claude") {
		t.Fatalf("import report = %q", out)
	}
	if len(st.cfg.MCPServers) != 2 || st.cfg.MCPServers[0].Origin == "" {
		t.Fatalf("cfg.MCPServers = %+v", st.cfg.MCPServers)
	}
	for _, a := range st.cfg.Agents {
		if len(a.MCPServers) != 2 {
			t.Errorf("agent %q mcp_servers = %v", a.Name, a.MCPServers)
		}
	}
	// Second run: everything is already configured.
	if out := execMCPImport("all", st); !strings.Contains(out, "already configured (skipped): github, web") {
		t.Fatalf("re-import report = %q", out)
	}
	// And it landed on disk.
	if onDisk, err := config.Load(); err != nil || len(onDisk.MCPServers) != 2 {
		t.Fatalf("on-disk config = %+v (err %v)", onDisk, err)
	}
}

func strconvQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
