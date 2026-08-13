package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/scoutme/milk/eval"
	"github.com/spf13/cobra"
)

func main() {
	var listAdapters bool

	root := &cobra.Command{
		Use:   "milk-eval",
		Short: "Agent evaluation harness for milk",
		Long:  "Run scenarios against agent adapters, judge results, and generate comparison reports.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if listAdapters {
				names := eval.List()
				if len(names) == 0 {
					fmt.Println("No adapters registered.")
					return nil
				}
				fmt.Println("Available adapters:")
				for _, n := range names {
					fmt.Printf("  %s\n", n)
				}
				fmt.Printf("\nUsage: milk-eval run --agents %s\n", strings.Join(names, ","))
				fmt.Println("With args: milk-eval run --agents \"milk-tui[--agent,mimo-local]\"")
				return nil
			}
			return cmd.Help()
		},
	}

	root.Flags().BoolVar(&listAdapters, "list", false, "List available agent adapters")

	root.AddCommand(runCmd())
	root.AddCommand(judgeCmd())
	root.AddCommand(reportCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// run subcommand
// ---------------------------------------------------------------------------

func runCmd() *cobra.Command {
	var (
		scenarioDir string
		agents      string
		category    string
		multiTurn   bool
		resultsDir  string
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run scenarios against agent adapters",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			// Parse agent names.
			agentNames := parseCommaList(agents)
			if len(agentNames) == 0 {
				agentNames = eval.List()
			}
			if len(agentNames) == 0 {
				return fmt.Errorf("no adapters registered; import adapter packages for side effects")
			}

			// Create harness.
			h, err := eval.NewHarness(agentNames)
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
			report := eval.GenerateReport(results)
			fmt.Println(report)

			return nil
		},
	}

	cmd.Flags().StringVar(&scenarioDir, "scenarios", "eval/scenarios", "directory of scenario YAML files")
	cmd.Flags().StringVar(&agents, "agents", "", "comma-separated adapter names (default: all registered)")
	cmd.Flags().StringVar(&category, "category", "", "filter by category name")
	cmd.Flags().BoolVar(&multiTurn, "multi-turn", false, "only run multi-turn scenarios")
	cmd.Flags().StringVar(&resultsDir, "results", "eval/results", "output directory for results")

	return cmd
}

// ---------------------------------------------------------------------------
// judge subcommand
// ---------------------------------------------------------------------------

func judgeCmd() *cobra.Command {
	var (
		resultsDir  string
		scenarioDir string
	)

	cmd := &cobra.Command{
		Use:   "judge",
		Short: "Re-score existing results against scenario rubrics",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			// Load scenarios for rubric lookup.
			scenarios, err := eval.LoadScenarios(scenarioDir)
			if err != nil {
				return fmt.Errorf("loading scenarios: %w", err)
			}
			scenarioMap := make(map[string]eval.Scenario, len(scenarios))
			for _, s := range scenarios {
				scenarioMap[s.Name] = s
			}

			// Load existing results.
			results, err := loadResults(resultsDir)
			if err != nil {
				return fmt.Errorf("loading results: %w", err)
			}

			// Create judge.
			judge, err := eval.NewJudgeFromConfig()
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
					ar.WeightedScore = eval.WeightedScore(scores, scenario.Rubric)
					sr.AgentResults[agentName] = ar
				}
				results[i] = sr
			}

			// Write updated results.
			if err := writeResults(resultsDir, results); err != nil {
				return fmt.Errorf("writing re-scored results: %w", err)
			}

			// Print report.
			report := eval.GenerateReport(results)
			fmt.Println(report)

			return nil
		},
	}

	cmd.Flags().StringVar(&resultsDir, "results", "eval/results", "results directory from a prior run")
	cmd.Flags().StringVar(&scenarioDir, "scenarios", "eval/scenarios", "scenario files directory for rubric lookup")

	return cmd
}

// ---------------------------------------------------------------------------
// report subcommand
// ---------------------------------------------------------------------------

func reportCmd() *cobra.Command {
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
				data := eval.GenerateJSON(results)
				report = string(data)
			} else if cacheOnly {
				report = eval.GenerateCacheReport(results)
			} else {
				report = eval.GenerateReport(results)
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
func writeResults(dir string, results []eval.ScenarioResult) error {
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
func loadResults(dir string) ([]eval.ScenarioResult, error) {
	resultsFile := dir + "/results.json"
	data, err := os.ReadFile(resultsFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", resultsFile, err)
	}

	var results []eval.ScenarioResult
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, fmt.Errorf("parsing results JSON: %w", err)
	}

	return results, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// parseCommaList splits a comma-separated string into trimmed, non-empty tokens.
func parseCommaList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
