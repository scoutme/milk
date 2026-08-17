package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// Command builds the "eval" command tree: run scenarios against agent
// adapters, judge results, and generate comparison reports. Mounted as a
// subcommand of the main milk CLI (`milk eval ...`).
func Command() *cobra.Command {
	var listAdapters bool

	root := &cobra.Command{
		Use:   "eval",
		Short: "Agent evaluation harness for milk",
		Long:  "Run scenarios against agent adapters, judge results, and generate comparison reports.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if listAdapters {
				names := List()
				if len(names) == 0 {
					fmt.Println("No adapters registered.")
					return nil
				}
				fmt.Println("Available adapters:")
				for _, n := range names {
					fmt.Printf("  %s\n", n)
				}
				fmt.Printf("\nUsage: milk eval run --agents %s\n", strings.Join(names, ","))
				fmt.Println("With args: milk eval run --agents \"milk-tui[--agent,mimo-local]\"")
				return nil
			}
			return cmd.Help()
		},
	}

	root.Flags().BoolVar(&listAdapters, "list", false, "List available agent adapters")

	root.AddCommand(evalRunCmd())
	root.AddCommand(evalJudgeCmd())
	root.AddCommand(evalReportCmd())

	return root
}

// ---------------------------------------------------------------------------
// run subcommand
// ---------------------------------------------------------------------------

func evalRunCmd() *cobra.Command {
	var (
		scenarioDir string
		agents      string
		category    string
		multiTurn   bool
		resultsDir  string
		judgeAgent  string
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run scenarios against agent adapters",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			// Parse agent names.
			agentNames := parseCommaList(agents)
			if len(agentNames) == 0 {
				agentNames = List()
			}
			if len(agentNames) == 0 {
				return fmt.Errorf("no adapters registered; import adapter packages for side effects")
			}

			// Create harness.
			h, err := NewHarness(agentNames, judgeAgent)
			if err != nil {
				return fmt.Errorf("creating harness: %w", err)
			}

			// Run scenarios.
			results, err := h.RunAll(ctx, scenarioDir, agentNames, category, multiTurn)
			if err != nil {
				return fmt.Errorf("running scenarios: %w", err)
			}

			// Write results.
			if err := writeResults(resultsDir, results); err != nil {
				return fmt.Errorf("writing results: %w", err)
			}

			// Print summary report to stdout.
			report := GenerateReport(results)
			fmt.Println(report)

			return nil
		},
	}

	cmd.Flags().StringVar(&scenarioDir, "scenarios", "eval/scenarios", "directory of scenario YAML files")
	cmd.Flags().StringVar(&agents, "agents", "", "comma-separated adapter names (default: all registered)")
	cmd.Flags().StringVar(&category, "category", "", "filter by category name")
	cmd.Flags().BoolVar(&multiTurn, "multi-turn", false, "only run multi-turn scenarios")
	cmd.Flags().StringVar(&resultsDir, "results", "eval/results", "output directory for results")
	cmd.Flags().StringVar(&judgeAgent, "judge-agent", "", "agent name from ~/.milk/config.json to use as the LLM judge (default: primary agent)")

	return cmd
}

// ---------------------------------------------------------------------------
// judge subcommand
// ---------------------------------------------------------------------------

func evalJudgeCmd() *cobra.Command {
	var (
		resultsDir  string
		scenarioDir string
		judgeAgent  string
	)

	cmd := &cobra.Command{
		Use:   "judge",
		Short: "Re-score existing results against scenario rubrics",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			// Load scenarios for rubric lookup.
			scenarios, err := LoadScenarios(scenarioDir)
			if err != nil {
				return fmt.Errorf("loading scenarios: %w", err)
			}
			scenarioMap := make(map[string]Scenario, len(scenarios))
			for _, s := range scenarios {
				scenarioMap[s.Name] = s
			}

			// Load existing results.
			results, err := loadResults(resultsDir)
			if err != nil {
				return fmt.Errorf("loading results: %w", err)
			}

			// Create judge.
			judge, err := NewJudgeFromConfig(judgeAgent)
			if err != nil {
				return fmt.Errorf("creating judge: %w", err)
			}

			// Re-score each scenario result.
			for i, sr := range results {
				scenario, ok := scenarioMap[sr.ScenarioName]
				if !ok {
					fmt.Fprintf(os.Stderr, "warning: scenario %q not found in %s, skipping\n", sr.ScenarioName, scenarioDir)
					continue
				}

				for agentName, ar := range sr.AgentResults {
					scores, err := judge.Score(ctx, scenario, ar.RunResults)
					if err != nil {
						fmt.Fprintf(os.Stderr, "warning: scoring %s/%s: %v\n", sr.ScenarioName, agentName, err)
						continue
					}
					ar.Scores = scores
					ar.WeightedScore = WeightedScore(scores, scenario.Rubric)
					sr.AgentResults[agentName] = ar
				}
				results[i] = sr
			}

			// Write updated results.
			if err := writeResults(resultsDir, results); err != nil {
				return fmt.Errorf("writing re-scored results: %w", err)
			}

			// Print report.
			report := GenerateReport(results)
			fmt.Println(report)

			return nil
		},
	}

	cmd.Flags().StringVar(&resultsDir, "results", "eval/results", "results directory from a prior run")
	cmd.Flags().StringVar(&scenarioDir, "scenarios", "eval/scenarios", "scenario files directory for rubric lookup")
	cmd.Flags().StringVar(&judgeAgent, "judge-agent", "", "agent name from ~/.milk/config.json to use as the LLM judge (default: primary agent)")

	return cmd
}

// ---------------------------------------------------------------------------
// report subcommand
// ---------------------------------------------------------------------------

func evalReportCmd() *cobra.Command {
	var (
		resultsDir string
		outputFile string
		cacheOnly  bool
		jsonFmt    bool
	)

	cmd := &cobra.Command{
		Use:   "report",
		Short: "Generate a comparison report from existing results",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Load results.
			results, err := loadResults(resultsDir)
			if err != nil {
				return fmt.Errorf("loading results: %w", err)
			}

			var report string
			if jsonFmt {
				data := GenerateJSON(results)
				report = string(data)
			} else if cacheOnly {
				report = GenerateCacheReport(results)
			} else {
				report = GenerateReport(results)
			}

			// Write output.
			if outputFile != "" {
				if err := os.WriteFile(outputFile, []byte(report), 0644); err != nil {
					return fmt.Errorf("writing report: %w", err)
				}
				fmt.Fprintf(os.Stderr, "Report written to %s\n", outputFile)
			} else {
				fmt.Print(report)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&resultsDir, "results", "eval/results", "results directory")
	cmd.Flags().StringVarP(&outputFile, "output", "o", "", "output file (default: stdout)")
	cmd.Flags().BoolVar(&cacheOnly, "cache-only", false, "only show cache analysis")
	cmd.Flags().BoolVar(&jsonFmt, "json", false, "output in JSON format")

	return cmd
}

// ---------------------------------------------------------------------------
// Result persistence
// ---------------------------------------------------------------------------

// writeResults persists ScenarioResults as JSON. Each scenario gets its own
// directory with a result file per agent.
func writeResults(dir string, results []ScenarioResult) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating results dir: %w", err)
	}

	// Write aggregated results file.
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling results: %w", err)
	}
	resultsFile := dir + "/results.json"
	if err := os.WriteFile(resultsFile, data, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", resultsFile, err)
	}

	return nil
}

// loadResults reads ScenarioResults from a results directory.
func loadResults(dir string) ([]ScenarioResult, error) {
	resultsFile := dir + "/results.json"
	data, err := os.ReadFile(resultsFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", resultsFile, err)
	}

	var results []ScenarioResult
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, fmt.Errorf("parsing results JSON: %w", err)
	}

	return results, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// parseCommaList splits a comma-separated string into trimmed, non-empty
// tokens, treating "[...]" adapter-arg brackets as atomic — a comma inside
// brackets (e.g. "claude-code[--cache-cooldown,5m]") does not split the token.
func parseCommaList(s string) []string {
	if s == "" {
		return nil
	}
	var result []string
	depth := 0
	start := 0
	flush := func(end int) {
		if tok := strings.TrimSpace(s[start:end]); tok != "" {
			result = append(result, tok)
		}
	}
	for i, r := range s {
		switch r {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				flush(i)
				start = i + 1
			}
		}
	}
	flush(len(s))
	return result
}
