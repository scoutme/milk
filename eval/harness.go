package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Harness is the orchestrator that runs scenarios against adapters, scores
// results via the judge, and produces ScenarioResult aggregates.
type Harness struct {
	Adapters []AgentAdapter
	Judge    *Judge
}

// NewHarness creates a Harness from adapter specs. The judge is created from
// the milk config (primary agent URL and model).
// Adapter specs support args: "milk-tui[--agent,mimo-local]"
func NewHarness(adapterSpecs []string) (*Harness, error) {
	var adapters []AgentAdapter
	for _, spec := range adapterSpecs {
		name, args := ParseAdapterSpec(spec)
		a, err := Get(name)
		if err != nil {
			return nil, err
		}
		a.SetArgs(args)
		adapters = append(adapters, a)
	}

	judge, err := NewJudgeFromConfig()
	if err != nil {
		return nil, fmt.Errorf("creating judge: %w", err)
	}

	return &Harness{Adapters: adapters, Judge: judge}, nil
}

// LoadScenarios reads all YAML files from dir and returns parsed Scenarios.
// Files whose names start with "_" are treated as non-executable (e.g. _base.yaml)
// and are skipped.
func LoadScenarios(dir string) ([]Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading scenario dir %s: %w", dir, err)
	}

	var scenarios []Scenario
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		// Skip non-executable base files.
		if strings.HasPrefix(name, "_") {
			continue
		}

		path := filepath.Join(dir, name)
		loaded, err := loadScenarioFile(path)
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", path, err)
		}
		scenarios = append(scenarios, loaded...)
	}

	// Sort by category then name for deterministic ordering.
	sort.Slice(scenarios, func(i, j int) bool {
		if scenarios[i].Category != scenarios[j].Category {
			return scenarios[i].Category < scenarios[j].Category
		}
		return scenarios[i].Name < scenarios[j].Name
	})

	return scenarios, nil
}

// scenarioFile is the top-level structure for a YAML file that contains
// either a single scenario or a list of scenarios under a "scenarios" key.
// A "category" field at the top level is inherited by all scenarios in the file.
type scenarioFile struct {
	Category  string     `yaml:"category"`
	Scenarios []Scenario `yaml:"scenarios"`
}

// loadScenarioFile reads a YAML file and returns the parsed Scenarios.
// It supports two formats:
//   - A flat Scenario (single scenario per file)
//   - A file with a "scenarios" key containing a list (multiple scenarios per file)
//
// Files with a top-level "category" key inherit that category for all contained
// scenarios unless overridden per-scenario.
func loadScenarioFile(path string) ([]Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Load base defaults if present in the same directory.
	dir := filepath.Dir(path)
	basePath := filepath.Join(dir, "_base.yaml")
	var base map[string]interface{}
	if baseData, err := os.ReadFile(basePath); err == nil {
		_ = yaml.Unmarshal(baseData, &base)
	}

	// Try multi-scenario format first.
	var sf scenarioFile
	if err := yaml.Unmarshal(data, &sf); err == nil && len(sf.Scenarios) > 0 {
		for i := range sf.Scenarios {
			s := &sf.Scenarios[i]
			// Inherit category from file level if not set per scenario.
			if s.Category == "" && sf.Category != "" {
				s.Category = sf.Category
			}
			if base != nil {
				applyBaseDefaults(s, base)
			}
			if len(s.Turns) == 0 && s.Prompt != "" {
				s.Turns = []Turn{{Prompt: s.Prompt, Rubric: s.Rubric}}
			}
		}
		return sf.Scenarios, nil
	}

	// Fall back to single-scenario format.
	var s Scenario
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}

	if base != nil {
		applyBaseDefaults(&s, base)
	}

	// Derive single-turn from prompt field when no turns are defined.
	if len(s.Turns) == 0 && s.Prompt != "" {
		s.Turns = []Turn{{Prompt: s.Prompt, Rubric: s.Rubric}}
	}

	return []Scenario{s}, nil
}

// applyBaseDefaults fills in missing rubric fields from _base.yaml defaults.
func applyBaseDefaults(s *Scenario, base map[string]interface{}) {
	defaultScoring, _ := base["default_scoring"].(string)
	defaultWeight, _ := base["default_weight"].(int)

	if defaultScoring == "" {
		defaultScoring = "scale_1_5"
	}
	if defaultWeight == 0 {
		defaultWeight = 1
	}

	applyRubricDefaults := func(rubric []RubricCriterion) []RubricCriterion {
		for i := range rubric {
			if rubric[i].Scoring == "" {
				rubric[i].Scoring = defaultScoring
			}
			if rubric[i].Weight == 0 {
				rubric[i].Weight = defaultWeight
			}
		}
		return rubric
	}

	s.Rubric = applyRubricDefaults(s.Rubric)
	for i := range s.Turns {
		s.Turns[i].Rubric = applyRubricDefaults(s.Turns[i].Rubric)
	}
}

// RunScenario runs a single scenario against all adapters, judges results,
// and returns the ScenarioResult.
func (h *Harness) RunScenario(ctx context.Context, scenario Scenario) (ScenarioResult, error) {
	sr := ScenarioResult{
		ScenarioName: scenario.Name,
		Category:     scenario.Category,
		Difficulty:   scenario.Difficulty,
		AgentResults: make(map[string]AgentResult),
	}

	for _, adapter := range h.Adapters {
		ar, err := h.runOne(ctx, scenario, adapter)
		if err != nil {
			return sr, fmt.Errorf("running scenario %s with adapter %s: %w",
				scenario.Name, adapter.Name(), err)
		}
		sr.AgentResults[adapter.Name()] = ar
	}

	return sr, nil
}

// runOne runs a scenario against a single adapter: creates a temp workdir,
// sets up files, starts the adapter, runs all turns, judges, and computes
// cache metrics.
func (h *Harness) runOne(ctx context.Context, scenario Scenario, adapter AgentAdapter) (AgentResult, error) {
	// 1. Create temp workdir and write setup files.
	workdir, err := setupWorkdir(scenario.Setup.Files)
	if err != nil {
		return AgentResult{}, fmt.Errorf("setting up workdir: %w", err)
	}
	defer os.RemoveAll(workdir)

	// 2. Start adapter.
	if err := adapter.Start(ctx, workdir); err != nil {
		return AgentResult{}, fmt.Errorf("starting adapter: %w", err)
	}
	defer adapter.Stop()

	// 3. Run each turn.
	var results []RunResult
	for _, turn := range scenario.Turns {
		result, err := adapter.RunPrompt(ctx, turn.Prompt)
		if err != nil {
			result.Error = err
		}
		results = append(results, result)
	}

	// 4. Judge scores.
	scores, err := h.Judge.Score(ctx, scenario, results)
	if err != nil {
		// Log but do not fail — we still have results.
		fmt.Fprintf(os.Stderr, "warning: judging %s/%s: %v\n", scenario.Name, adapter.Name(), err)
	}

	// 5. Compute weighted score.
	weightedScore := WeightedScore(scores, scenario.Rubric)
	// For multi-turn, accumulate rubric criteria across turns.
	if len(scenario.Turns) > 1 {
		var allRubric []RubricCriterion
		for _, t := range scenario.Turns {
			allRubric = append(allRubric, t.Rubric...)
		}
		if len(allRubric) > 0 {
			weightedScore = WeightedScore(scores, allRubric)
		}
	}

	// 6. Compute cache metrics.
	cache := AggregateCacheMetrics(results)

	return AgentResult{
		RunResults:    results,
		Scores:        scores,
		WeightedScore: weightedScore,
		Cache:         cache,
	}, nil
}

// RunAll runs all scenarios from the scenario directory against the specified
// adapter names and returns all ScenarioResults.
func (h *Harness) RunAll(ctx context.Context, scenarioDir string, adapterNames []string, category string, multiTurnOnly bool) ([]ScenarioResult, error) {
	scenarios, err := LoadScenarios(scenarioDir)
	if err != nil {
		return nil, fmt.Errorf("loading scenarios: %w", err)
	}

	// Filter by category.
	if category != "" {
		var filtered []Scenario
		for _, s := range scenarios {
			if strings.EqualFold(s.Category, category) {
				filtered = append(filtered, s)
			}
		}
		scenarios = filtered
	}

	// Filter for multi-turn only.
	if multiTurnOnly {
		var filtered []Scenario
		for _, s := range scenarios {
			if s.MultiTurn {
				filtered = append(filtered, s)
			}
		}
		scenarios = filtered
	}

	if len(scenarios) == 0 {
		return nil, fmt.Errorf("no scenarios found matching filters")
	}

	// Resolve adapters (specs may include args: "milk-tui[--agent,mimo-local]").
	if len(adapterNames) > 0 {
		h.Adapters = nil
		for _, spec := range adapterNames {
			name, args := ParseAdapterSpec(spec)
			a, err := Get(name)
			if err != nil {
				return nil, err
			}
			a.SetArgs(args)
			h.Adapters = append(h.Adapters, a)
		}
	}

	if len(h.Adapters) == 0 {
		return nil, fmt.Errorf("no adapters specified; available: %s", strings.Join(List(), ", "))
	}

	var results []ScenarioResult
	for _, scenario := range scenarios {
		fmt.Fprintf(os.Stderr, "Running scenario: %s\n", scenario.Name)
		sr, err := h.RunScenario(ctx, scenario)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ERROR: %v\n", err)
			continue
		}
		results = append(results, sr)
	}

	return results, nil
}

// setupWorkdir creates a temporary directory and writes the setup files into it.
func setupWorkdir(files []FileDef) (string, error) {
	dir, err := os.MkdirTemp("", "milk-eval-")
	if err != nil {
		return "", fmt.Errorf("creating temp dir: %w", err)
	}

	for _, f := range files {
		path := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			os.RemoveAll(dir)
			return "", fmt.Errorf("creating dir for %s: %w", f.Path, err)
		}
		if err := os.WriteFile(path, []byte(f.Content), 0644); err != nil {
			os.RemoveAll(dir)
			return "", fmt.Errorf("writing %s: %w", f.Path, err)
		}
	}

	return dir, nil
}
