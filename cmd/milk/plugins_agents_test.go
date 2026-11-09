package main

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/plugin"
)

// loadedPluginAgents builds a loadedPlugin carrying plugin agents directly —
// unit-level input for buildPluginAgentSet, no filesystem needed.
func loadedPluginAgents(name string, ext map[string]any, agents ...plugin.Agent) loadedPlugin {
	p := &plugin.Plugin{
		Root:     "/plugins/" + name,
		Manifest: plugin.Manifest{Name: name},
		Agents:   agents,
	}
	if ext != nil {
		p.Manifest.Extensions = map[string]map[string]any{pluginNamespace: ext}
	}
	return loadedPlugin{plugin: p}
}

func TestBuildPluginAgentSet_ClonesPrimaryTransport(t *testing.T) {
	cfg := config.Config{Agents: []config.AgentConfig{{
		Name: "prim", URL: "http://127.0.0.1:9000/v1", Model: "mm", Provider: "local",
		APIKey: "k", Prompt: "primary's own prompt", Tools: []config.AgentToolEntry{{Agent: "x"}},
	}}}
	rev := plugin.Agent{Name: "code-reviewer", Description: "Reviews code.", Body: "You are a senior reviewer.", MilkTools: []string{"read_file", "grep"}}
	set, warns := buildPluginAgentSet(cfg, []loadedPlugin{loadedPluginAgents("cr", nil, rev)})
	if len(warns) != 0 {
		t.Fatalf("warns = %v", warns)
	}
	if len(set.Agents) != 1 || len(set.Tools) != 1 {
		t.Fatalf("set = %+v", set)
	}
	ac := set.Agents[0]
	if ac.Name != "code-reviewer" || ac.URL != "http://127.0.0.1:9000/v1" || ac.Provider != "local" || ac.APIKey != "k" {
		t.Errorf("transport not cloned: %+v", ac)
	}
	if ac.Prompt != "You are a senior reviewer." {
		t.Errorf("Prompt = %q want the agent body", ac.Prompt)
	}
	if ac.PromptFile != "" || len(ac.Tools) != 0 {
		t.Errorf("template prompt/tools must not leak: PromptFile=%q Tools=%v", ac.PromptFile, ac.Tools)
	}
	if ac.Limits == nil || len(ac.Limits.IncludedTools) != 2 || ac.Limits.IncludedTools[0] != "read_file" {
		t.Errorf("Limits = %+v want mapped whitelist", ac.Limits)
	}
	te := set.Tools[0]
	if te.Agent != "code-reviewer" || te.Description != "Reviews code." {
		t.Errorf("exposure = %+v", te)
	}
}

func TestBuildPluginAgentSet_ModelAndWarnings(t *testing.T) {
	cfg := config.Config{Agents: []config.AgentConfig{
		{Name: "prim", URL: "http://primary"},
		{Name: "alt", URL: "http://alt-endpoint", Model: "special-model"},
	}}
	clone := plugin.Agent{Name: "cloner", Description: "d", Body: "b", Model: "alt"}
	lost := plugin.Agent{Name: "stranger", Description: "d", Body: "b", Model: "not-configured"}
	set, warns := buildPluginAgentSet(cfg, []loadedPlugin{loadedPluginAgents("p", nil, clone, lost)})
	if len(set.Agents) != 2 {
		t.Fatalf("set.Agents = %d want 2", len(set.Agents))
	}
	for _, a := range set.Agents {
		switch a.Name {
		case "cloner": // frontmatter model names a configured agent → cloned from it
			if a.URL != "http://alt-endpoint" || a.Model != "special-model" {
				t.Errorf("cloner = %+v want alt's transport", a)
			}
		case "stranger": // unknown model → primary + warning
			if a.URL != "http://primary" {
				t.Errorf("stranger = %q want primary fallback", a.URL)
			}
		}
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w, `model "not-configured"`) && strings.Contains(w, "not a configured agent") {
			found = true
		}
	}
	if !found {
		t.Errorf("warns = %v want the unknown-model report", warns)
	}
}

func TestBuildPluginAgentSet_CollisionAndDupAndExpose(t *testing.T) {
	cfg := config.Config{Agents: []config.AgentConfig{{Name: "prim", URL: "http://p"}, {Name: "shadow", URL: "http://config-shadow"}}}
	shadow := plugin.Agent{Name: "shadow", Description: "d", Body: "b"}
	dupe1 := plugin.Agent{Name: "dupe", Description: "one", Body: "body-one"}
	dupe2 := plugin.Agent{Name: "dupe", Description: "two", Body: "body-two"}
	hidden := plugin.Agent{Name: "hidden", Description: "d", Body: "b"}

	set, warns := buildPluginAgentSet(cfg, []loadedPlugin{
		loadedPluginAgents("a", nil, shadow, dupe1),
		loadedPluginAgents("b", map[string]any{"expose_as_tool": false}, dupe2, hidden),
	})
	// shadow dropped (config wins), dupe resolved across plugins → 2 entries: dupe + hidden
	if len(set.Agents) != 2 {
		t.Fatalf("set.Agents = %+v want dupe+hidden (shadow and dup2 skipped)", set.Agents)
	}
	for _, a := range set.Agents {
		if a.Name == "shadow" {
			t.Errorf("collision entry leaked: %+v", a)
		}
	}
	if set.Agents[0].Prompt != "body-one" {
		t.Errorf("dupe kept the first plugin's copy: Prompt = %q", set.Agents[0].Prompt)
	}
	// expose_as_tool:false → hidden has no tool entry; dupe (from plugin a) does
	for _, te := range set.Tools {
		if te.Agent == "hidden" {
			t.Errorf("hidden must not be exposed: %+v", te)
		}
	}
	if len(set.Tools) != 1 || set.Tools[0].Agent != "dupe" {
		t.Errorf("Tools = %+v want only dupe", set.Tools)
	}
	wantWarns := []string{"collides with a configured agent", "duplicate plugin agent name"}
	for _, want := range wantWarns {
		ok := false
		for _, w := range warns {
			if strings.Contains(w, want) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("warns = %v want one containing %q", warns, want)
		}
	}
}

// End-to-end-ish: a real plugin directory in the state root flows through
// initPlugins into config resolution and tool exposure.
func TestInitPlugins_AgentsWireEndToEnd(t *testing.T) {
	cfg := config.Config{Agents: []config.AgentConfig{{Name: "prim", URL: "http://primary-host/v1"}}}
	writeStateFile(t, "plugins/revman/plugin.json", `{"$schema":"`+plugin.SchemaPlugin+`","name":"revman"}`)
	writeStateFile(t, "plugins/revman/agents/code-reviewer.md", "---\nname: code-reviewer\ndescription: Expert code review specialist.\ntools: Read, Grep, Bash\n---\nYou are a senior code reviewer. Run git diff and begin review.")
	t.Cleanup(func() {
		config.SetPluginAgents(nil)
		initPlugins(config.Config{}) // wipe back to an empty bundle for the shared process
	})

	initPlugins(cfg)

	all := cfg.AllAgents()
	var found *config.AgentConfig
	for i := range all {
		if all[i].Name == "code-reviewer" {
			found = &all[i]
		}
	}
	if found == nil {
		t.Fatalf("plugin agent not resolvable: %v", all)
	}
	if found.URL != "http://primary-host/v1" {
		t.Errorf("URL = %q want primary clone", found.URL)
	}
	if !strings.Contains(found.Prompt, "senior code reviewer") || strings.Contains(found.Prompt, "name:") {
		t.Errorf("Prompt = %q want stripped body", found.Prompt)
	}
	if found.Limits == nil || len(found.Limits.IncludedTools) != 3 {
		t.Errorf("Limits = %+v want 3 mapped tools", found.Limits)
	}
	entries := cfg.EffectiveToolAgents("prim")
	exposed := false
	for _, e := range entries {
		if e.Agent == "code-reviewer" {
			exposed = e.Description == "Expert code review specialist."
		}
	}
	if !exposed {
		t.Errorf("agent not exposed as a tool: %v", entries)
	}
	// /agent list marker
	st := &interactiveState{cfg: cfg}
	if line := execAgentList(st); !strings.Contains(line, "(plugin)") || !strings.Contains(line, "code-reviewer") {
		t.Errorf("execAgentList missing plugin marker:\n%s", line)
	}
}
