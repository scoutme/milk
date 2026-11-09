package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// maxAgentFileBytes bounds one agents/*.md file (frontmatter + body).
const maxAgentFileBytes = 64 * 1024

// Agent is one discovered Claude-style subagent definition (agents/<name>.md).
//
// agents/ is deliberately NOT a portable Agent Plugins component type: §3
// defines a Component as "a skill or MCP server entry". It is the ecosystem's
// de-facto cross-client agent directory (Claude Code's subagent format), which
// milk reads as client behavior under §4.1 containment — the same posture as
// the .claude-plugin/ shim input (ADR-0104 Part III). The body becomes the
// agent's system prompt; the frontmatter's description becomes the
// agent-as-tool schema description.
type Agent struct {
	Name        string
	Description string
	Model       string   // frontmatter model: — honored only when it names a configured agent
	Tools       []string // Claude tool names as declared (Read, Grep…)
	MilkTools   []string // mapped milk tool names (read_file, grep…) — the executable whitelist
	Body        string
	Plugin      string // owning plugin name (set by loadAgents)
}

// agentFrontmatter is the Claude Code subagent frontmatter shape.
type agentFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Model       string `yaml:"model"`
	Tools       any    `yaml:"tools"` // "Read, Grep, Bash" scalar or [Read, Grep] list
}

// claudeToolToMilk maps Claude Code's subagent tool names onto milk's built-in
// tool names. A tools list is a whitelist: an unmapped name would silently
// narrow the agent to nothing, so unmapped entries are dropped with a warning
// instead (§11.3 report-and-continue). Deliberately absent: TodoWrite (milk's
// task tools are a different contract) and NotebookEdit (no milk equivalent).
var claudeToolToMilk = map[string]string{
	"Read":      "read_file",
	"Grep":      "grep",
	"Glob":      "find_files",
	"Bash":      "bash",
	"Write":     "write_file",
	"Edit":      "edit_file",
	"MultiEdit": "multiedit",
	"WebFetch":  "web_fetch",
}

// loadAgents discovers agents/*.md (immediate children only, like skills/):
// each regular .md file with frontmatter is one agent. Files without
// frontmatter are not agent definitions and pass silently (a README in the
// directory must not produce load warnings); a file with frontmatter that
// fails validation is skipped with a warning; siblings load normally (§11.3).
func (p *Plugin) loadAgents() {
	dir, err := resolveWithin(p.Root, "./agents")
	if err != nil {
		p.warnf("agents/: %v", err)
		return
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return // no agents/ location is fine (§6.2)
	}
	if !fi.IsDir() {
		p.warnf("agents: not a directory — agents disabled")
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		p.warnf("agents: %v", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			continue
		}
		path, err := resolveWithin(p.Root, "./agents/"+e.Name())
		if err != nil {
			p.warnf("agents/%s: %v", e.Name(), err) // §4.1 containment
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			p.warnf("agents/%s: %v", e.Name(), err)
			continue
		}
		if len(raw) > maxAgentFileBytes {
			p.warnf("agents/%s: larger than %d bytes — skipped", e.Name(), maxAgentFileBytes)
			continue
		}
		ag, warns, err := ParseAgent(raw)
		if err != nil {
			if !hasFrontmatter(raw) {
				continue // no frontmatter = not an agent file (README), silent
			}
			p.warnf("agents/%s: %v — skipped", e.Name(), err)
			continue
		}
		ag.Plugin = p.Manifest.Name
		p.Agents = append(p.Agents, ag)
		for _, w := range warns {
			p.warnf("agents/%s: %s", ag.Name, w)
		}
	}
}

// hasFrontmatter reports whether raw starts with a YAML frontmatter delimiter.
func hasFrontmatter(raw []byte) bool {
	_, _, ok := splitFrontmatter(string(raw))
	return ok
}

// ParseAgent validates one agents/*.md payload. Warnings are the
// report-and-continue cases (tool names with no milk equivalent); any other
// error rejects that agent file.
func ParseAgent(raw []byte) (Agent, []string, error) {
	var ag Agent
	fmRaw, body, ok := splitFrontmatter(string(raw))
	if !ok {
		return ag, nil, fmt.Errorf("missing YAML frontmatter")
	}
	var fm agentFrontmatter
	if err := yaml.Unmarshal([]byte(fmRaw), &fm); err != nil {
		return ag, nil, fmt.Errorf("frontmatter: %w", err)
	}
	var warns []string

	name := strings.TrimSpace(fm.Name)
	if err := validatePluginName(name); err != nil {
		return ag, nil, fmt.Errorf("name: %w", err)
	}
	desc := strings.TrimSpace(fm.Description)
	if desc == "" {
		return ag, nil, fmt.Errorf("description: required (it becomes the agent-as-tool schema description)")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return ag, nil, fmt.Errorf("empty body: the agent would have no system prompt")
	}

	declared := parseAgentTools(fm.Tools)
	milkTools, dropped := mapAgentTools(declared)
	for _, d := range dropped {
		warns = append(warns, fmt.Sprintf("tool %q has no milk equivalent — dropped from the whitelist", d))
	}

	ag = Agent{
		Name:        name,
		Description: desc,
		Model:       strings.TrimSpace(fm.Model),
		Tools:       declared,
		MilkTools:   milkTools,
		Body:        body,
	}
	return ag, warns, nil
}

// parseAgentTools accepts both declared forms — the Claude scalar
// ("Read, Grep, Bash") and a YAML list — and returns trimmed entries.
func parseAgentTools(v any) []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	switch t := v.(type) {
	case string:
		for _, part := range strings.Split(t, ",") {
			add(part)
		}
	case []any:
		for _, part := range t {
			if s, ok := part.(string); ok {
				add(s)
			}
		}
	}
	return out
}

// mapAgentTools maps declared Claude names onto milk tools, case-insensitively;
// unmapped names land in dropped (deduped, order preserved).
func mapAgentTools(declared []string) (milk []string, dropped []string) {
	seenMilk := map[string]bool{}
	seenDrop := map[string]bool{}
	for _, d := range declared {
		mapped := ""
		for k, v := range claudeToolToMilk {
			if strings.EqualFold(k, d) {
				mapped = v
				break
			}
		}
		if mapped == "" {
			if !seenDrop[strings.ToLower(d)] {
				seenDrop[strings.ToLower(d)] = true
				dropped = append(dropped, d)
			}
			continue
		}
		if !seenMilk[mapped] {
			seenMilk[mapped] = true
			milk = append(milk, mapped)
		}
	}
	return milk, dropped
}
