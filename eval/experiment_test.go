package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func writeExperiment(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "experiment.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadExperiment_Defaults(t *testing.T) {
	path := writeExperiment(t, "name: my-suite\nagents: [milk-tui]\n")
	exp, err := LoadExperiment(path)
	if err != nil {
		t.Fatalf("LoadExperiment: %v", err)
	}
	if exp.Repetitions != 1 {
		t.Errorf("Repetitions = %d, want default 1", exp.Repetitions)
	}
	if exp.Scenarios == "" {
		t.Error("Scenarios should default, not stay empty")
	}
	if exp.Results != filepath.Join("eval", "results", "my-suite") {
		t.Errorf("Results = %q", exp.Results)
	}
	if exp.SandboxEnabled() {
		t.Error("sandbox should default to off")
	}
}

func TestLoadExperiment_Full(t *testing.T) {
	path := writeExperiment(t, `
name: testbed
description: fixed request, repeated
agents:
  - milk-tui[--sandbox]
  - claude-code[--cache-cooldown,5m]
scenarios: scenarios
select:
  category: metrics
  names: [fixed-request]
repetitions: 3
judge_agent: big-model
results: out/results
sandbox:
  mode: fresh
  seed: config
budget:
  max_total_tokens: 50000
  max_tool_calls: 20
  max_duration: 2m
`)
	exp, err := LoadExperiment(path)
	if err != nil {
		t.Fatalf("LoadExperiment: %v", err)
	}
	if len(exp.Agents) != 2 || exp.Agents[0] != "milk-tui[--sandbox]" {
		t.Errorf("Agents = %v", exp.Agents)
	}
	if exp.Scenarios != "scenarios" {
		t.Errorf("Scenarios = %q (missing dir keeps the literal)", exp.Scenarios)
	}
	if exp.Select.Category != "metrics" || len(exp.Select.Names) != 1 {
		t.Errorf("Select = %+v", exp.Select)
	}
	if exp.Repetitions != 3 {
		t.Errorf("Repetitions = %d", exp.Repetitions)
	}
	if exp.JudgeAgent != "big-model" || exp.Results != "out/results" {
		t.Errorf("judge/results = %q/%q", exp.JudgeAgent, exp.Results)
	}
	if !exp.SandboxEnabled() {
		t.Error("sandbox.mode fresh should enable the sandbox")
	}
	if exp.Budget == nil || exp.Budget.MaxToolCalls != 20 || exp.Budget.MaxDurationMS != 120000 {
		t.Errorf("Budget = %+v", exp.Budget)
	}
}

func TestLoadExperiment_Validation(t *testing.T) {
	if _, err := LoadExperiment(writeExperiment(t, "repetitions: -1\n")); err == nil {
		t.Error("negative repetitions must fail")
	}
	if _, err := LoadExperiment(writeExperiment(t, "sandbox: {mode: everywhere}\n")); err == nil {
		t.Error("unknown sandbox.mode must fail")
	}
	if _, err := LoadExperiment(writeExperiment(t, "sandbox: {seed: vibes}\n")); err == nil {
		t.Error("unknown sandbox.seed must fail")
	}
}

func TestExperimentSandboxConfig(t *testing.T) {
	// The seed is the *current* config: point milk's state root at a temp
	// dir with a config.json and it must be picked up.
	root := t.TempDir()
	t.Setenv("MILK_CONFIG_ROOT", root)
	seedBody := `{"agents":[{"name":"mimo-local","provider":"local"}]}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(seedBody), 0644); err != nil {
		t.Fatal(err)
	}

	fresh := &Experiment{Sandbox: ExperimentSandbox{Mode: "fresh", Seed: "config"}}
	cfg, err := fresh.SandboxConfig()
	if err != nil {
		t.Fatalf("SandboxConfig: %v", err)
	}
	if !cfg.Enabled {
		t.Error("fresh mode should enable the sandbox")
	}
	if cfg.SeedConfigPath != filepath.Join(root, "config.json") {
		t.Errorf("SeedConfigPath = %q, want the current config", cfg.SeedConfigPath)
	}

	none := &Experiment{Sandbox: ExperimentSandbox{Mode: "fresh", Seed: "none"}}
	cfg, err = none.SandboxConfig()
	if err != nil {
		t.Fatalf("SandboxConfig: %v", err)
	}
	if !cfg.Enabled || cfg.SeedConfigPath != "" {
		t.Errorf("seed none should enable without seeding: %+v", cfg)
	}

	off := &Experiment{}
	cfg, err = off.SandboxConfig()
	if err != nil {
		t.Fatalf("SandboxConfig: %v", err)
	}
	if cfg.Enabled {
		t.Errorf("off mode must not enable the sandbox: %+v", cfg)
	}
}

func TestFilterScenarios(t *testing.T) {
	scenarios := []Scenario{
		{Name: "a", Category: "smoke", MultiTurn: true},
		{Name: "b", Category: "debugging"},
		{Name: "c", Category: "smoke"},
	}

	got, err := FilterScenarios(scenarios, "smoke", nil, false)
	if err != nil || len(got) != 2 {
		t.Errorf("category filter: %v, %v", got, err)
	}

	got, err = FilterScenarios(scenarios, "", nil, true)
	if err != nil || len(got) != 1 || got[0].Name != "a" {
		t.Errorf("multi-turn filter: %v, %v", got, err)
	}

	got, err = FilterScenarios(scenarios, "", []string{"b", "c"}, false)
	if err != nil || len(got) != 2 {
		t.Errorf("name filter: %v, %v", got, err)
	}

	if _, err := FilterScenarios(scenarios, "", []string{"typo"}, false); err == nil {
		t.Error("unknown scenario name must be an error, not a silent shrink")
	}
}
