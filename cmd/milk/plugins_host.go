package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/plugin"
)

// pluginNamespace is milk's Agent Plugins extension namespace (§8): a
// reverse-domain identifier for the domain milk controls (milkai.dev). Plugin
// authors can put milk-specific options under
// extensions["dev.milkai.milk"] in plugin.json, or ship a
// "dev.milkai.milk/" top-level directory (§8.2) — the manifest form is what
// milk reads today.
const pluginNamespace = "dev.milkai.milk"

// loadedPlugin is one discovered plugin with its precomputed milk-side
// projections, so per-turn lookups never re-derive anything.
type loadedPlugin struct {
	plugin   *plugin.Plugin
	servers  []config.MCPServerConfig
	warns    []string // MilkServers skips (e.g. legacy sse)
	assign   []string // extension mcp_assign; nil = every agent
	noSkills bool     // extension disable_skills
}

var (
	pluginsMu sync.RWMutex
	plugins   []loadedPlugin
	pluginLog []string // discovery-level report for /plugins
)

// initPlugins discovers installed plugins and wires them into milk:
//
//   - user plugins: <state root>/plugins (the shared install location)
//   - project plugins: <cwd>/.milk/plugins (repo-local, like .milk/skills)
//
// MCP servers are exposed through the config's runtime plugin seam (never
// persisted — config.SetPluginMCPServers), assigned to every agent unless
// extensions["dev.milkai.milk"].mcp_assign restricts the list. Skills feed the
// model-facing skill tool (progressive disclosure: name+description in the
// tool schema, body on demand).
//
// PLUGIN_DATA (Agent Plugins §9.1) lives at <state root>/plugin-data/<name>,
// created on demand when a stdio server launches, preserved across updates.
func initPlugins() {
	cwd, _ := os.Getwd()
	stateRoot, _ := config.Dir()
	dataRoot := filepath.Join(stateRoot, "plugin-data")
	roots := []string{filepath.Join(stateRoot, "plugins")}
	if cwd != "" {
		roots = append(roots, filepath.Join(cwd, ".milk", "plugins"))
	}
	found, report := plugin.Discover(dataRoot, roots...)

	loaded := make([]loadedPlugin, 0, len(found))
	log := append([]string(nil), report...)
	for _, p := range found {
		servers, warns := p.MilkServers()
		lp := loadedPlugin{plugin: p, servers: servers, warns: warns}
		if ext := p.Manifest.Extensions[pluginNamespace]; ext != nil {
			lp.assign = stringList(ext["mcp_assign"])
			if b, ok := ext["disable_skills"].(bool); ok {
				lp.noSkills = b
			}
		}
		loaded = append(loaded, lp)
	}

	pluginsMu.Lock()
	plugins = loaded
	pluginLog = log
	pluginsMu.Unlock()

	config.SetPluginMCPServers(pluginServersForAgent)
	seedSkillCatalog()
}

// pluginServersForAgent is the config seam: plugin-derived MCP servers for
// one agent, honoring the extension's mcp_assign (default: all agents).
func pluginServersForAgent(agentName string) []config.MCPServerConfig {
	pluginsMu.RLock()
	defer pluginsMu.RUnlock()
	var out []config.MCPServerConfig
	for _, lp := range plugins {
		if !pluginAssignsTo(lp, agentName) {
			continue
		}
		out = append(out, lp.servers...)
	}
	return out
}

func pluginAssignsTo(lp loadedPlugin, agentName string) bool {
	if len(lp.assign) == 0 {
		return true
	}
	for _, want := range lp.assign {
		if strings.EqualFold(want, agentName) {
			return true
		}
	}
	return false
}

// seedSkillCatalog flattens every plugin's skills into the model-facing
// catalog. Load re-reads SKILL.md at activation time so an edited skill is
// picked up without a restart (progressive disclosure; the body is never
// held in the prompt until asked for).
func seedSkillCatalog() {
	pluginsMu.RLock()
	defer pluginsMu.RUnlock()
	var entries []local.SkillEntry
	for _, lp := range plugins {
		if lp.noSkills {
			continue
		}
		for _, sk := range lp.plugin.Skills {
			sk := sk
			entries = append(entries, local.SkillEntry{
				Name:        sk.Name,
				Description: sk.Description,
				Load: func() (string, error) {
					raw, err := os.ReadFile(filepath.Join(sk.Dir, "SKILL.md"))
					if err != nil {
						return "", err
					}
					parsed, err := plugin.ParseSkill(raw)
					if err != nil {
						return "", err
					}
					return parsed.Body, nil
				},
			})
		}
	}
	local.SetSkillCatalog(entries)
}

// handlePluginsCmd renders /plugins: the loaded plugins with their
// components, and every collected warning (discovery failures, invalid
// entries, skipped skills/servers — the spec's SHOULD-report surface).
func handlePluginsCmd() string {
	pluginsMu.RLock()
	defer pluginsMu.RUnlock()
	if len(plugins) == 0 && len(pluginLog) == 0 {
		return "\n[milk] plugins: none installed (user: " + filepath.Join(stateDir(), "plugins") + ", project: .milk/plugins)\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n[milk] plugins: %d loaded\n", len(plugins))
	for _, lp := range plugins {
		p := lp.plugin
		fmt.Fprintf(&b, "  %s", p.Manifest.Name)
		if p.Manifest.Version != "" {
			fmt.Fprintf(&b, " %s", p.Manifest.Version)
		}
		if p.Manifest.Description != "" {
			fmt.Fprintf(&b, " — %s", p.Manifest.Description)
		}
		fmt.Fprintf(&b, "\n    root: %s\n", p.Root)
		if len(p.Skills) > 0 {
			names := make([]string, 0, len(p.Skills))
			for _, s := range p.Skills {
				names = append(names, s.Name)
			}
			fmt.Fprintf(&b, "    skills: %s\n", strings.Join(names, ", "))
		}
		switch {
		case p.MCPDisabled:
			fmt.Fprintf(&b, "    mcp: disabled (invalid mcp.json)\n")
		case p.MCP != nil && len(p.MCP.Servers) > 0:
			names := make([]string, 0, len(p.MCP.Servers))
			for n := range p.MCP.Servers {
				names = append(names, n)
			}
			sort.Strings(names)
			fmt.Fprintf(&b, "    mcp: %s\n", strings.Join(names, ", "))
		}
		if len(lp.assign) > 0 {
			fmt.Fprintf(&b, "    assigned agents: %s\n", strings.Join(lp.assign, ", "))
		}
	}
	warns := append([]string(nil), pluginLog...)
	for _, lp := range plugins {
		for _, w := range lp.plugin.Warnings {
			warns = append(warns, lp.plugin.Manifest.Name+": "+w)
		}
		for _, w := range lp.warns {
			warns = append(warns, lp.plugin.Manifest.Name+": "+w)
		}
	}
	if len(warns) > 0 {
		fmt.Fprintf(&b, "  warnings:\n")
		for _, w := range warns {
			fmt.Fprintf(&b, "    - %s\n", w)
		}
	}
	return b.String()
}

// stateDir is config.Dir() with the error dropped — the state root is
// created on demand and the error path is not actionable here.
func stateDir() string {
	d, _ := config.Dir()
	return d
}

// stringList coerces a decoded YAML/JSON list value to a []string.
func stringList(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	}
	return nil
}
