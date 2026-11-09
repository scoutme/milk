// Package plugin implements the Agent Plugins Specification v1.0.0
// (https://agent-plugins.org/specification): directory-based plugin packages
// with a root plugin.json manifest and two portable component types —
// Agent Skills (skills/*/SKILL.md, https://agentskills.io/specification) and
// MCP servers (mcp.json) — plus two de-facto directory types read as client
// behavior (ADR-0104 Parts III/IV): agents/*.md subagents and
// commands/*.md slash commands.
//
// The package is host-neutral: it validates and loads a plugin with the
// spec's failure isolation (a broken component never blocks independent
// ones) and leaves execution to the caller. All normative section numbers in
// comments refer to the Agent Plugins Specification v1.0.0; skill format
// rules refer to the Agent Skills specification.
package plugin

import (
	"fmt"
	"os"
	"path/filepath"
)

// Schema identifiers recognized by this client (§5.2, §10.1). Exactly the
// v1.0.0 canonical values; any other $schema is rejected as an unsupported
// version.
const (
	SchemaPlugin = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	SchemaMCP    = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
)

// Plugin is one loaded plugin package (§3). Warnings carries the SHOULD-report
// diagnostics collected during loading (unknown manifest fields, invalid
// entries, skipped skills…); loading continues wherever the spec says it
// must, so a plugin with warnings can still be fully usable.
type Plugin struct {
	Root     string // filesystem-resolved plugin root
	DataDir  string // client-managed PLUGIN_DATA dir for this instance (§9.1)
	Manifest Manifest
	Skills   []Skill
	Agents   []Agent   // de-facto agents/*.md subagent definitions (ADR-0104 Part III)
	Commands []Command // de-facto commands/*.md slash commands (ADR-0104 Part IV)
	MCP      *MCPFile  // nil when the plugin has no mcp.json

	// MCPDisabled is set when mcp.json exists but is invalid as a whole
	// (§7.2.2 rule 2): MCP is disabled for the plugin, other components live.
	MCPDisabled bool

	Warnings []string
}

func (p *Plugin) warnf(format string, args ...any) {
	p.Warnings = append(p.Warnings, fmt.Sprintf(format, args...))
}

// Name returns the manifest name.
func (p *Plugin) Name() string { return p.Manifest.Name }

// Load reads and validates the plugin rooted at root (§4.1, §5, §6, §7, §11.3).
// dataRoot is the client-managed base directory for PLUGIN_DATA (§9.1); the
// per-plugin instance dir is dataRoot/<plugin name> and is created on demand
// by the caller when launching subprocesses — Load only computes and
// validates against it.
//
// Failure boundaries (§4.1): manifest failure rejects the whole plugin; a
// broken component location disables only that component type; a broken
// skill or server entry skips only that entry. Any returned error means the
// plugin is rejected; diagnostics are in the error.
func Load(root, dataRoot string) (*Plugin, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("plugin root: %w", err)
	}
	resolved, err := safeEvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("plugin root: %w", err)
	}
	p := &Plugin{Root: resolved}

	// plugin.json must itself resolve within the root (§4.1 boundary 1).
	manifestPath, err := resolveWithin(resolved, "./plugin.json")
	if err != nil {
		return nil, fmt.Errorf("plugin.json: %w", err)
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("plugin.json: %w", err)
	}
	man, warns, err := ParseManifest(raw)
	if err != nil {
		return nil, fmt.Errorf("plugin.json: %w", err)
	}
	p.Manifest = man
	for _, w := range warns {
		p.warnf("plugin.json: %s", w)
	}
	p.DataDir = filepath.Join(dataRoot, man.Name)

	// Skills (§6, §7.1): missing location is fine, wrong kind invalidates
	// only the component type (§6.2, §4.1 boundary 2).
	p.loadSkills()

	// Agents (de-facto agents/*.md — client behavior, ADR-0104 Part III):
	// same per-directory boundary and §11.3 isolation as skills.
	p.loadAgents()

	// Commands (de-facto commands/*.md — Claude custom slash commands, the
	// second de-facto component type, ADR-0104 Part IV): same boundaries.
	p.loadCommands()

	// MCP (§7.2): same component-type boundary; per-entry failures skip the
	// entry only (§7.2.2 rules 3-5).
	p.loadMCP()

	return p, nil
}

// Discover scans each dir in roots for plugin subdirectories (a plugin root is
// any immediate child directory containing plugin.json — §5.1). Non-plugin
// children are skipped silently; a directory with an invalid manifest is
// reported as a warning on the returned report and skipped. The returned
// plugins are ordered by (root order, directory name) for determinism.
func Discover(dataRoot string, roots ...string) ([]*Plugin, []string) {
	var out []*Plugin
	var report []string
	for _, dir := range roots {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				report = append(report, fmt.Sprintf("%s: %v", dir, err))
			}
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			cand := filepath.Join(dir, e.Name())
			if _, err := os.Stat(filepath.Join(cand, "plugin.json")); err != nil {
				continue // not a plugin root
			}
			p, err := Load(cand, dataRoot)
			if err != nil {
				report = append(report, fmt.Sprintf("%s: %v", cand, err))
				continue
			}
			out = append(out, p)
		}
	}
	return out, report
}
