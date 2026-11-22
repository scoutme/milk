package main

// Harness import — the read-only half of milk's "use what you already have"
// setup (the machine scan in initwizard_discover.go is the other half): look
// at other AI harnesses' config files and offer to adopt what maps onto
// milk's own config:
//
//   - MCP servers (Claude Code's mcpServers maps in ~/.claude.json and the
//     settings.json files, Claude Desktop's claude_desktop_config.json) —
//     converted 1:1 to config.MCPServerConfig entries and origin-tagged with
//     the file they came from. Never connected silently: the wizard's import
//     step and /mcp import each ask first.
//   - provider credentials (Claude's settings "env" maps) — surfaced as
//     wizard findings that reference the env var when it is exported (a
//     pointer, api_key_env) and carry the literal only when the key exists
//     just in the file (then it is stored in config.json exactly like a
//     typed key — and never displayed).
//
// Nothing here writes: the harness's files are read, never modified, and
// nothing is copied into milk's config without an explicit pick. Secrets
// leave this file only inside importedCredential.Value, and only to be
// written to the config the user confirmed — displays always use the masked
// fingerprint.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/scoutme/milk/internal/config"
)

// importedMCPServer is one MCP server found in another harness's config.
type importedMCPServer struct {
	Name   string
	Origin string // human label of the source file, e.g. "~/.claude.json"
	Detail string // one-line display: "stdio: npx -y …" or the server URL
	Cfg    config.MCPServerConfig
}

// importedCredential is one provider key found in another harness's config.
// Value is a secret: it is only ever written to config.json after an explicit
// pick, and only rendered as initWizardMask's fingerprint.
type importedCredential struct {
	Name   string // agent name for the finding (from initWizardKeySources)
	Label  string // display label ("OpenRouter", …)
	EnvVar string // environment variable the key conventionally lives in
	Value  string // the literal from the file (SECRET)
	Origin string // human label of the source file
	URL    string // base URL override ("" = the provider's standard)
	Model  string // suggested model override ("" = the provider's default)
}

// harnessHomeDir resolves the user's home directory — a var so tests can pin
// it (the TestMain state-root sandbox already redirects HOME, so most tests
// simply write fixtures under the sandbox home).
var harnessHomeDir = os.UserHomeDir

// harnessScanFn runs the import scan — a var so wizard tests are hermetic.
var harnessScanFn = harnessImportScan

// initWizardImportPick maps the import step's answer to the servers to adopt.
// Blank (or "none"/"skip") imports nothing — a pick must be explicit — while
// "all" takes everything. Otherwise the answer is a comma-separated list of
// numbers (as displayed) or names. The second return is a validation message
// ("" = the answer was understood).
func initWizardImportPick(servers []importedMCPServer, answer string) ([]importedMCPServer, string) {
	answer = strings.ToLower(strings.TrimSpace(answer))
	switch answer {
	case "", "none", "skip", "0":
		return nil, ""
	case "all":
		return servers, ""
	}
	byName := map[string]importedMCPServer{}
	for _, s := range servers {
		byName[strings.ToLower(s.Name)] = s
	}
	var picked []importedMCPServer
	seen := map[string]bool{}
	for _, tok := range strings.Split(answer, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		var s importedMCPServer
		var ok bool
		if n, err := strconv.Atoi(tok); err == nil && n >= 1 && n <= len(servers) {
			s, ok = servers[n-1], true
		} else {
			s, ok = byName[tok]
		}
		if !ok {
			return nil, fmt.Sprintf("invalid choice %q — enter numbers 1–%d, names, 'all' or 'none'", tok, len(servers))
		}
		if !seen[strings.ToLower(s.Name)] {
			seen[strings.ToLower(s.Name)] = true
			picked = append(picked, s)
		}
	}
	return picked, ""
}

// applyImportedMCPServers merges the picked imports into cfg: each is added
// under its own name (an existing same-name server is left untouched — never
// overwritten) and assigned to the primary and escalation agents, so the
// tools appear where they were already in use. Returns the names added and
// those skipped as already configured. Shared by the wizard's commit and
// /mcp import.
func applyImportedMCPServers(cfg *config.Config, picked []importedMCPServer) (added, skipped []string) {
	for _, s := range picked {
		if findMCPServerIdx(cfg.MCPServers, s.Name) >= 0 {
			skipped = append(skipped, s.Name)
			continue
		}
		if s.Cfg.Origin == "" {
			s.Cfg.Origin = s.Origin
		}
		config.UpsertMCPServer(cfg, s.Cfg)
		added = append(added, s.Name)
		for _, agent := range []string{cfg.Agent, cfg.EscalationAgent} {
			if agent != "" {
				assignMCPServer(cfg, s.Name, agent, true)
			}
		}
	}
	return added, skipped
}

// harnessImportScan reads other harnesses' config files under home/cwd and
// returns the credentials and MCP servers that could be adopted. Most
// specific source first; a name already seen (case-insensitively) is not
// duplicated, so the most specific file wins.
func harnessImportScan(cwd string) (creds []importedCredential, servers []importedMCPServer) {
	home, _ := harnessHomeDir()
	seen := map[string]bool{}
	addServer := func(s importedMCPServer) {
		if s.Name == "" || seen[strings.ToLower(s.Name)] {
			return
		}
		seen[strings.ToLower(s.Name)] = true
		servers = append(servers, s)
	}

	// Settings files, project-first (env maps + mcpServers).
	env := map[string]string{} // key → literal, most specific source wins
	envOrigin := map[string]string{}
	for _, path := range claudeSettingsFiles(home, cwd) {
		f := parseHarnessJSON(path)
		label := harnessLabel(path, home)
		for k, v := range f.env {
			if _, ok := env[k]; !ok {
				env[k], envOrigin[k] = v, label
			}
		}
		for name, raw := range f.servers {
			if s, ok := claudeMCPServer(name, raw, label); ok {
				addServer(s)
			}
		}
	}

	// Claude Code's user config: global mcpServers plus the per-project map.
	if home != "" {
		path := filepath.Join(home, ".claude.json")
		label := "~/.claude.json"
		f := parseHarnessJSON(path)
		for name, raw := range f.servers {
			if s, ok := claudeMCPServer(name, raw, label); ok {
				addServer(s)
			}
		}
		if cwd != "" {
			proj, ok := f.projects[filepath.Clean(cwd)]
			if ok {
				for name, raw := range proj {
					if s, ok := claudeMCPServer(name, raw, label+" (this project)"); ok {
						addServer(s)
					}
				}
			}
		}
	}

	// Claude Desktop.
	if home != "" {
		path := claudeDesktopConfigPath(home)
		label := "Claude Desktop"
		f := parseHarnessJSON(path)
		for name, raw := range f.servers {
			if s, ok := claudeMCPServer(name, raw, label); ok {
				addServer(s)
			}
		}
	}

	// Credentials: one entry per known key source, from the env maps above.
	// ANTHROPIC_BASE_URL / ANTHROPIC_MODEL reshape the Anthropic entry (the
	// relay/proxy setup the key is usually paired with).
	for _, k := range initWizardKeySources {
		v, ok := env[k.Env]
		if !ok {
			continue
		}
		c := importedCredential{
			Name: k.Name, Label: k.Label, EnvVar: k.Env, Value: v,
			Origin: envOrigin[k.Env], URL: k.URL, Model: k.Model,
		}
		if k.Env == "ANTHROPIC_API_KEY" {
			if u := env["ANTHROPIC_BASE_URL"]; u != "" {
				c.URL = strings.TrimRight(u, "/")
			}
			if m := env["ANTHROPIC_MODEL"]; m != "" {
				c.Model = m
			}
		}
		creds = append(creds, c)
	}
	return creds, servers
}

// claudeSettingsFiles lists the Claude Code settings files that may carry an
// env map or mcpServers, most specific first.
func claudeSettingsFiles(home, cwd string) []string {
	var paths []string
	for _, dir := range []string{cwd, home} {
		if dir == "" {
			continue
		}
		paths = append(paths,
			filepath.Join(dir, ".claude", "settings.local.json"),
			filepath.Join(dir, ".claude", "settings.json"),
		)
	}
	return paths
}

// claudeDesktopConfigPath is Claude Desktop's config file for this OS. It is
// always derived from home (not %APPDATA%) so a sandboxed home sandboxes the
// scan with it.
func claudeDesktopConfigPath(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Claude", "claude_desktop_config.json")
	default:
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json")
	}
}

// harnessLabel names a source file for display: ~-shortened against home.
func harnessLabel(path, home string) string {
	if home != "" {
		if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return filepath.Base(path)
}

// harnessFile is the tolerant parse result of one harness config file: the
// pieces milk can adopt, whatever else the file carries.
type harnessFile struct {
	env      map[string]string            // settings "env" map
	servers  map[string]map[string]any    // "mcpServers" map
	projects map[string]map[string]map[string]any // ~/.claude.json "projects" → mcpServers
}

// parseHarnessJSON reads one harness config file. A missing or malformed file
// yields an empty result — the scan is best-effort and never fails the wizard.
func parseHarnessJSON(path string) harnessFile {
	var f harnessFile
	data, err := os.ReadFile(path)
	if err != nil {
		return f
	}
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return f
	}
	f.env = anyStringMap(root["env"])
	f.servers = anyServerMap(root["mcpServers"])
	if proj, ok := root["projects"].(map[string]any); ok {
		for dir, pv := range proj {
			pm, ok := pv.(map[string]any)
			if !ok {
				continue
			}
			servers := anyServerMap(pm["mcpServers"])
			if len(servers) > 0 {
				if f.projects == nil {
					f.projects = map[string]map[string]map[string]any{}
				}
				f.projects[filepath.Clean(dir)] = servers
			}
		}
	}
	return f
}

// claudeMCPServer converts one Claude-format MCP entry to milk's config:
//
//	{"command": "npx", "args": [...], "env": {...}}           → stdio
//	{"type": "http"|"sse", "url": "...", "headers": {...}}    → http/sse
//
// Returns false for an entry with neither a command nor a URL — nothing milk
// could ever connect to.
func claudeMCPServer(name string, raw map[string]any, origin string) (importedMCPServer, bool) {
	cfg := config.MCPServerConfig{Name: name, Origin: origin}
	detail := ""
	switch {
	case anyString(raw["command"]) != "":
		cfg.Transport = "stdio"
		cfg.Command = anyString(raw["command"])
		cfg.Args = anyStrings(raw["args"])
		cfg.Env = anyStringMap(raw["env"])
		cfg.Cwd = anyString(raw["cwd"])
		detail = "stdio: " + cfg.Command
		if len(cfg.Args) > 0 {
			detail += " " + strings.Join(cfg.Args, " ")
		}
	case anyString(raw["url"]) != "":
		cfg.URL = anyString(raw["url"])
		if strings.EqualFold(anyString(raw["type"]), "sse") {
			cfg.Transport = "sse"
		} else {
			cfg.Transport = "http"
		}
		cfg.Headers = anyStringMap(raw["headers"])
		detail = cfg.URL
	default:
		return importedMCPServer{}, false
	}
	return importedMCPServer{Name: name, Origin: origin, Detail: detail, Cfg: cfg}, true
}

func anyString(v any) string {
	s, _ := v.(string)
	return s
}

func anyStrings(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func anyStringMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for k, e := range m {
		out[k] = fmt.Sprint(e)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func anyServerMap(v any) map[string]map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]map[string]any{}
	for k, e := range m {
		if sm, ok := e.(map[string]any); ok {
			out[k] = sm
		}
	}
	return out
}
