package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

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
func initPlugins(cfg config.Config) {
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

	// Runtime plugin agents (ADR-0104 Part III): synthesized entries cloned
	// from this config's primary (plugins ship behavior, never credentials),
	// installed through the same never-persisted seam as the MCP servers.
	// Warnings land in the discovery report so /plugins shows them.
	agentSet, agentWarns := buildPluginAgentSet(cfg, loaded)
	log = append(log, agentWarns...)

	// Plugin slash commands (ADR-0104 Part IV): the command store derives
	// here too, so mutations (/plugins add/remove, drop-ins) re-register
	// alongside everything else.
	cmds, cmdWarns := buildPluginSlashCmds(loaded)
	log = append(log, cmdWarns...)
	setPluginCommands(cmds)

	pluginsMu.Lock()
	plugins = loaded
	pluginLog = log
	pluginsMu.Unlock()

	config.SetPluginMCPServers(pluginServersForAgent)
	config.SetPluginAgents(func() config.PluginAgentSet { return agentSet })
	seedSkillCatalog()
}

// buildPluginAgentSet synthesizes config entries for every agent shipped by an
// installed plugin (ADR-0104 Part III). Transport, auth and limits are cloned
// from a template agent — the primary by default, or the configured agent a
// frontmatter `model:` names — because plugins carry no credentials. The
// agent's body becomes Prompt (in-memory only; the seam never persists).
// The frontmatter description rides as the default agent-as-tool exposure,
// which the extensions["dev.milkai.milk"].expose_as_tool flag can disable.
// A name colliding with a configured agent is skipped: user config wins.
func buildPluginAgentSet(cfg config.Config, loaded []loadedPlugin) (config.PluginAgentSet, []string) {
	var set config.PluginAgentSet
	var warns []string
	primary := cfg.ActiveAgent()
	seen := map[string]bool{}
	configured := func(n string) bool {
		for _, a := range cfg.Agents {
			if strings.EqualFold(a.Name, n) {
				return true
			}
		}
		return strings.EqualFold(n, "claude")
	}
	for _, lp := range loaded {
		ext := lp.plugin.Manifest.Extensions[pluginNamespace]
		expose := true
		if ext != nil {
			if b, ok := ext["expose_as_tool"].(bool); ok {
				expose = b
			}
		}
		for _, pa := range lp.plugin.Agents {
			if seen[strings.ToLower(pa.Name)] {
				warns = append(warns, fmt.Sprintf("agent %s: duplicate plugin agent name — skipped", pa.Name))
				continue
			}
			seen[strings.ToLower(pa.Name)] = true
			if configured(pa.Name) {
				warns = append(warns, fmt.Sprintf("agent %s: collides with a configured agent — kept from config, plugin copy skipped", pa.Name))
				delete(seen, strings.ToLower(pa.Name)) // resolution sees the config entry; not a plugin agent
				continue
			}

			tmpl := primary
			if pa.Model != "" {
				if named, ok := cfg.AgentByName(pa.Model); ok {
					tmpl = named
				} else {
					warns = append(warns, fmt.Sprintf("agent %s: frontmatter model %q is not a configured agent — running on primary", pa.Name, pa.Model))
				}
			}
			if tmpl.Name == "" && tmpl.URL == "" {
				warns = append(warns, fmt.Sprintf("agent %s: no primary configured to inherit transport from — registered but not runnable", pa.Name))
			}

			ac := tmpl // struct copy: URL/provider/auth/TLS/AWS/limits inherited
			ac.Name = pa.Name
			ac.Prompt = pa.Body // replaces any template prompt: the agent IS its persona
			ac.PromptFile = ""
			ac.Tools = nil // the template's per-agent tool overrides are not the plugin's business
			if len(pa.MilkTools) > 0 {
				lim := config.AgentLimits{}
				if tmpl.Limits != nil {
					lim = *tmpl.Limits
				}
				lim.IncludedTools = pa.MilkTools
				ac.Limits = &lim
			}
			set.Agents = append(set.Agents, ac)
			if expose {
				set.Tools = append(set.Tools, config.AgentToolEntry{Agent: pa.Name, Description: pa.Description})
			}
		}
	}
	return set, warns
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

// ---- Plugin slash commands (ADR-0104 Part IV) ----
//
// commands/*.md is the second de-facto component type milk reads (after
// agents/): each becomes a real slash command — registered into the same
// surfaces every built-in command uses (recognition, completion, /help, ACP
// advertising) but kept in its OWN runtime store instead of mutating
// commandRegistry, so ACP goroutines reading the static registry never race
// a TUI-side hot reload. Readers (specFor, registryNames, the help builder,
// the ACP table) consult both stores.

type pluginSlashCmd struct {
	pluginName string
	cmd        plugin.Command
	spec       cmdSpec
}

var (
	pluginCmdsMu sync.RWMutex
	pluginCmds   []pluginSlashCmd
	// pluginSpecsCache is the immutable []cmdSpec snapshot the per-keystroke
	// readers load (review S5): registryNames walks it on every completion
	// keypress, so it must not take a lock or rebuild — setPluginCommands
	// is the single writer and refreshes the snapshot eagerly.
	pluginSpecsCache atomic.Value // []cmdSpec
)

// buildPluginSlashCmds derives collision-checked specs from the loaded
// plugins. Collisions — against commandRegistry (names + aliases) or
// another plugin's command — are dropped with a report: built-ins always
// win, first plugin wins among themselves.
func buildPluginSlashCmds(loaded []loadedPlugin) ([]pluginSlashCmd, []string) {
	builtin := make(map[string]bool, len(commandRegistry))
	for _, s := range commandRegistry {
		builtin[strings.ToLower(s.name)] = true
		for _, a := range s.aliases {
			builtin[strings.ToLower(a)] = true
		}
	}
	seen := map[string]bool{}
	var out []pluginSlashCmd
	var warns []string
	for _, lp := range loaded {
		for _, c := range lp.plugin.Commands {
			name := "/" + c.Name
			key := strings.ToLower(name)
			if builtin[key] || seen[key] {
				warns = append(warns, fmt.Sprintf("plugin %s: command %s collides with an existing command — skipped", lp.plugin.Name(), name))
				continue
			}
			seen[key] = true
			sig := name
			if c.ArgumentHint != "" {
				sig += " " + c.ArgumentHint
			}
			out = append(out, pluginSlashCmd{
				pluginName: lp.plugin.Name(),
				cmd:        c,
				spec: cmdSpec{
					name:  name,
					hosts: hostBoth,
					desc:  c.Description, // always non-empty: loadCommands derives a fallback
					hint:  c.ArgumentHint,
					help:  fmt.Sprintf("  %-24s %s", sig, c.Description),
				},
			})
		}
	}
	return out, warns
}

// setPluginCommands replaces the store (startup and every hot reload —
// removals drop their commands with everything else) and refreshes the
// spec snapshot in one step.
func setPluginCommands(cmds []pluginSlashCmd) {
	pluginCmdsMu.Lock()
	pluginCmds = cmds
	pluginCmdsMu.Unlock()
	pluginSpecsCache.Store(pluginSpecsBuild())
}

func pluginSpecsBuild() []cmdSpec {
	pluginCmdsMu.RLock()
	defer pluginCmdsMu.RUnlock()
	out := make([]cmdSpec, len(pluginCmds))
	for i, pc := range pluginCmds {
		out[i] = pc.spec
	}
	return out
}

// pluginSlashSpecs returns the specs for the dual-store readers — a pure
// snapshot load (review S5), with a build fallback for reads that happen
// before any setPluginCommands call (tests, early startup).
func pluginSlashSpecs() []cmdSpec {
	if v := pluginSpecsCache.Load(); v != nil {
		return v.([]cmdSpec)
	}
	out := pluginSpecsBuild()
	pluginSpecsCache.Store(out)
	return out
}

// lookupPluginSlash resolves a command name against the plugin store.
func lookupPluginSlash(name string) (pluginSlashCmd, bool) {
	pluginCmdsMu.RLock()
	defer pluginCmdsMu.RUnlock()
	for _, pc := range pluginCmds {
		if pc.spec.name == name {
			return pc, true
		}
	}
	return pluginSlashCmd{}, false
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
		if len(p.Agents) > 0 {
			names := make([]string, 0, len(p.Agents))
			for _, a := range p.Agents {
				names = append(names, a.Name)
			}
			fmt.Fprintf(&b, "    agents: %s\n", strings.Join(names, ", "))
		}
		if len(p.Commands) > 0 {
			names := make([]string, 0, len(p.Commands))
			for _, c := range p.Commands {
				names = append(names, "/"+c.Name)
			}
			fmt.Fprintf(&b, "    commands: %s\n", strings.Join(names, ", "))
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
