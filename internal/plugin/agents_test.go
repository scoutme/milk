package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const claudeAgentMD = `---
name: code-reviewer
description: Expert code review specialist. Proactively reviews code for quality.
tools: Read, Grep, Glob, Bash
---
You are a senior code reviewer. Run git diff and begin review immediately.`

func TestParseAgent_ClaudeShape(t *testing.T) {
	ag, warns, err := ParseAgent([]byte(claudeAgentMD))
	if err != nil {
		t.Fatalf("ParseAgent: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("warns = %v want none", warns)
	}
	if ag.Name != "code-reviewer" {
		t.Errorf("Name = %q", ag.Name)
	}
	if !strings.HasPrefix(ag.Description, "Expert code review") {
		t.Errorf("Description = %q", ag.Description)
	}
	if strings.Contains(ag.Body, "---") || strings.Contains(ag.Body, "name:") {
		t.Errorf("Body must be frontmatter-stripped: %q", ag.Body)
	}
	wantTools := []string{"read_file", "grep", "find_files", "bash"}
	if len(ag.MilkTools) != len(wantTools) {
		t.Fatalf("MilkTools = %v want %v", ag.MilkTools, wantTools)
	}
	for i, w := range wantTools {
		if ag.MilkTools[i] != w {
			t.Errorf("MilkTools[%d] = %q want %q", i, ag.MilkTools[i], w)
		}
	}
	declared := []string{"Read", "Grep", "Glob", "Bash"}
	for i, d := range declared {
		if ag.Tools[i] != d {
			t.Errorf("Tools[%d] = %q want %q", i, ag.Tools[i], d)
		}
	}
}

func TestParseAgent_ListFormTools(t *testing.T) {
	md := "---\nname: list-tools\ndescription: d\ntools:\n  - Read\n  - Grep\n---\nbody"
	ag, _, err := ParseAgent([]byte(md))
	if err != nil {
		t.Fatalf("ParseAgent: %v", err)
	}
	if len(ag.MilkTools) != 2 || ag.MilkTools[0] != "read_file" || ag.MilkTools[1] != "grep" {
		t.Errorf("MilkTools = %v", ag.MilkTools)
	}
}

func TestParseAgent_UnknownToolWarnedDropped(t *testing.T) {
	md := "---\nname: odd\ndescription: d\ntools: Read, TodoWrite, NotebookEdit, read\n---\nbody"
	ag, warns, err := ParseAgent([]byte(md))
	if err != nil {
		t.Fatalf("ParseAgent: %v", err)
	}
	// mapped entries dedupe case-insensitively (Read + read = one read_file)
	if len(ag.MilkTools) != 1 || ag.MilkTools[0] != "read_file" {
		t.Errorf("MilkTools = %v", ag.MilkTools)
	}
	if len(warns) != 2 {
		t.Fatalf("warns = %v want 2 (one per unknown tool)", warns)
	}
	if !strings.Contains(warns[0], `tool "TodoWrite"`) || !strings.Contains(warns[1], `tool "NotebookEdit"`) {
		t.Errorf("warns = %v", warns)
	}
}

func TestParseAgent_Rejections(t *testing.T) {
	cases := []struct {
		what string
		md   string
		want string
	}{
		{"no frontmatter", "just prose", "frontmatter"},
		{"bad yaml", "---\nname: [broken\ndescription: x\n---\nbody", "frontmatter"},
		{"missing name", "---\ndescription: d\n---\nbody", "name"},
		{"invalid name", "---\nname: BAD Name\ndescription: d\n---\nbody", "name"},
		{"missing description", "---\nname: x\n---\nbody", "description"},
		{"empty body", "---\nname: x\ndescription: d\n---\n   \n", "empty body"},
	}
	for _, c := range cases {
		if _, _, err := ParseAgent([]byte(c.md)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v want containing %q", c.what, err, c.want)
		}
	}
}

func TestLoadAgents_DiscoveryRules(t *testing.T) {
	root := t.TempDir()
	// valid + README (no frontmatter: silent) + subdir (ignored) + .txt (ignored)
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"code-reviewer.md": claudeAgentMD,
		"README.md":        "This directory holds subagents.\n",
		"helper.txt":       "not markdown",
		"empty.md":         "no frontmatter here either",
	} {
		if err := os.WriteFile(filepath.Join(root, "agents", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "agents", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	writePlugin(t, root, "plugin.json", minManifest)

	p, err := Load(root, t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.Agents) != 1 {
		t.Fatalf("Agents = %d want 1 (README/empty/txt/subdir ignored silently)", len(p.Agents))
	}
	ag := p.Agents[0]
	if ag.Name != "code-reviewer" || ag.Plugin != "my-plugin" {
		t.Errorf("agent = %+v", ag)
	}
	for _, w := range p.Warnings {
		if strings.Contains(w, "README") || strings.Contains(w, "helper.txt") {
			t.Errorf("non-agent files must be silent, got warning: %s", w)
		}
	}
}

func TestLoadAgents_InvalidFrontmatterWarnedSiblingLives(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	// frontmatter present but invalid (name violates the charset) → warn + skip
	broken := "---\nname: BAD Name\ndescription: d\n---\nbody"
	if err := os.WriteFile(filepath.Join(root, "agents", "broken.md"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "good.md"), []byte(claudeAgentMD), 0o644); err != nil {
		t.Fatal(err)
	}
	writePlugin(t, root, "plugin.json", minManifest)

	p, err := Load(root, t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.Agents) != 1 || p.Agents[0].Name != "code-reviewer" {
		t.Fatalf("Agents = %+v want only the good one", p.Agents)
	}
	found := false
	for _, w := range p.Warnings {
		if strings.Contains(w, "broken.md") && strings.Contains(w, "name") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v want a broken.md name report", p.Warnings)
	}
}

func TestKeepSurface_AgentsKept(t *testing.T) {
	for _, keep := range []string{"agents/code-reviewer.md", "skills/x/SKILL.md"} {
		if !keepSurface(keep) {
			t.Errorf("keepSurface(%q) = false want true", keep)
		}
	}
	if keepSurface("assets/video.mp4") {
		t.Error("assets must stay filtered")
	}
}

func TestCommonWrapper_SurfaceAtTop(t *testing.T) {
	// plugin surface (agents/) at top level = no wrapper strip
	if w := commonWrapper([]string{"agents/code-reviewer.md", "plugin.json"}); w != "" {
		t.Errorf("commonWrapper = %q want \"\"", w)
	}
	// wrapped repo → strip detected
	if w := commonWrapper([]string{"owner-repo-sha/agents/code-reviewer.md"}); w != "owner-repo-sha/" {
		t.Errorf("commonWrapper = %q want owner-repo-sha/", w)
	}
}
