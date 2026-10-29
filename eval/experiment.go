package eval

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/scoutme/milk/internal/config"
	"gopkg.in/yaml.v3"
)

// Experiment is a parsed experiment file: the full evaluation suite in one
// YAML file — which adapters to run, which scenarios, how many repetitions,
// which judge, where results go, how runs are sandboxed, and the metric
// budgets asserted against every run — so a complete, repeatable evaluation
// needs no external scripting. `milk eval run --experiment <file>` executes
// it; explicit CLI flags override its fields.
//
// Minimal example:
//
//	agents: [milk-tui]
//	scenarios: eval/scenarios
//	select:
//	  category: smoke
//	repetitions: 3
//	sandbox:
//	  mode: fresh
//	budget:
//	  max_total_tokens: 50000
//	  max_tool_calls: 20
//	  max_duration: 2m
type Experiment struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// Agents are adapter specs (the same bracket syntax as --agents:
	// "milk-tui[--agent,mimo-local]"). Empty = all registered adapters.
	Agents []string `yaml:"agents"`
	// Scenarios is the scenario directory. Empty = "eval/scenarios".
	Scenarios string `yaml:"scenarios"`
	// Select narrows which scenarios from the directory run.
	Select ExperimentSelect `yaml:"select"`
	// JudgeAgent names the agent from config.json that scores responses
	// ("" = the primary agent, same as --judge-agent).
	JudgeAgent string `yaml:"judge_agent"`
	// Results is the output directory. Empty = <milk state root>/eval/results/
	// <name> (~/.milk/eval/results/<name>, or .../eval/results when the
	// experiment has no name) — outside the source tree.
	// "~/" is expanded.
	Results string `yaml:"results"`
	// Repetitions runs every scenario N times against every adapter, each
	// repetition fully isolated (fresh workdir, fresh sandbox) so nth-run
	// drift — cache warmth, memory influence, session carry-over — is
	// measurable instead of invisible. Default 1.
	Repetitions int `yaml:"repetitions"`
	// Sandbox configures isolated agent state roots for the run (milk-tui).
	Sandbox ExperimentSandbox `yaml:"sandbox"`
	// Budget is the default metric budget asserted against every run; a
	// scenario's own `budget:` block refines it field-by-field.
	Budget *Budget `yaml:"budget"`
	// Pricing overrides the built-in model price table ($ per 1M tokens) for
	// this experiment's runs: model name → prices, e.g.
	//
	//	pricing:
	//	  mimo-v2.6-pro: {input: 0.435, output: 0.87, cache_read: 0.0036}
	//
	// Keys match run model names (exact, or path-qualified like
	// xiaomi/mimo-v2.6-pro). Feeds eval's computed-cost adapter (milk);
	// claude-code and mimo-code report their own cost and ignore it.
	Pricing map[string]ModelPricing `yaml:"pricing,omitempty"`
	// Versions is the milk version matrix: one labeled milk-tui adapter per
	// entry, all running the same suite side by side (see ResolveAdapterSpecs).
	// Combines with Agents (e.g. a claude-code baseline alongside versions).
	Versions []Version `yaml:"versions"`
}

// Version is one milk version in the experiment's version matrix. Each entry
// becomes its own labeled milk-tui adapter, so one run compares effectiveness
// (judge scores, tokens, tool calls, time) across milk versions — same
// scenarios, same judge, same budgets, fresh sandbox per run.
type Version struct {
	// Label names the version's column in reports and its key in results.json
	// (default: the ref, or the binary's basename). Keep it short — report
	// columns are 12 chars wide.
	Label string `yaml:"label"`
	// Ref is a git ref (tag / branch / commit) of the milk repo, built on
	// demand via `go build ./cmd/milk` in a throwaway git worktree; the
	// binary is cached keyed by the resolved commit, so re-runs skip the
	// build. Ignored when MilkBinary is set.
	Ref string `yaml:"ref"`
	// MilkBinary is a prebuilt milk binary to drive instead ("~" is
	// expanded). Wins over Ref when both are set.
	MilkBinary string `yaml:"milk_binary"`
}

// ExperimentSelect filters the scenario directory.
type ExperimentSelect struct {
	// Category keeps only scenarios of one category.
	Category string `yaml:"category"`
	// Names keeps only scenarios with these exact names. Unknown names are
	// an error (a typo silently shrinking the suite would be worse).
	Names []string `yaml:"names"`
	// MultiTurn keeps only multi_turn scenarios (like --multi-turn).
	MultiTurn bool `yaml:"multi_turn"`
}

// ExperimentSandbox configures the always-fresh isolated state root each
// milk-tui run gets (claude-code runs against the real ~/.claude and is not
// sandboxed).
type ExperimentSandbox struct {
	// Mode is "fresh" — every run gets a brand-new milk state root, seeded
	// per Seed, so no run's sessions, memory or history can influence the
	// next — or "off" (the default) for live-state runs.
	Mode string `yaml:"mode"`
	// Seed is "config" (the default): copy the current config.json into the
	// fresh root so the run uses your current agent set and settings but
	// none of the state. "none" seeds nothing (first-run defaults).
	Seed string `yaml:"seed"`
	// MilkBinary optionally pins the milk binary the adapter drives
	// (default: ~/.local/bin/milk — rebuild with `task build` after code
	// changes).
	MilkBinary string `yaml:"milk_binary"`
	// AllowTools grants the seeded agents dangerously_skip_permissions so
	// batch runs can execute mutating tools (bash, edit_file, write_file …).
	// Without it those tools are denied in non-interactive runs ("requires
	// permission — run interactively or enable skip-permissions") and any
	// code-editing request silently degrades to read-only. The live config
	// is never modified — only the sandbox's seeded copy.
	AllowTools bool `yaml:"allow_tools"`
}

// LoadExperiment reads and validates an experiment YAML file.
func LoadExperiment(path string) (*Experiment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading experiment file: %w", err)
	}
	var exp Experiment
	if err := yaml.Unmarshal(data, &exp); err != nil {
		return nil, fmt.Errorf("parsing experiment file %s: %w", path, err)
	}

	// Scenario and result paths: "~/" is expanded in both. Relative scenario
	// paths resolve against the experiment file's directory first — an
	// experiment is self-contained wherever milk eval is invoked from —
	// falling back to the current working directory (the CLI flag semantics).
	// Relative result paths behave like the --results flag (working directory).
	// The defaults live outside the source tree, under the eval artifact home
	// (docs/internal/eval-artifacts.md).
	dir := filepath.Dir(path)
	exp.Scenarios = expandHome(exp.Scenarios)
	if exp.Scenarios == "" {
		exp.Scenarios = "eval/scenarios"
	}
	if !filepath.IsAbs(exp.Scenarios) {
		if _, serr := os.Stat(filepath.Join(dir, exp.Scenarios)); serr == nil {
			exp.Scenarios = filepath.Join(dir, exp.Scenarios)
		}
	}
	exp.Results = expandHome(exp.Results)
	if exp.Results == "" {
		if exp.Name != "" {
			exp.Results = filepath.Join(DefaultResultsDir(), exp.Name)
		} else {
			exp.Results = DefaultResultsDir()
		}
	}
	if exp.Repetitions == 0 {
		exp.Repetitions = 1
	}

	if err := exp.Validate(); err != nil {
		return nil, fmt.Errorf("experiment file %s: %w", path, err)
	}
	return &exp, nil
}

// Validate rejects inconsistent experiment definitions early, before any
// agent is launched.
func (e *Experiment) Validate() error {
	if e.Repetitions < 1 {
		return fmt.Errorf("repetitions must be >= 1, got %d", e.Repetitions)
	}
	switch e.Sandbox.Mode {
	case "", "off", "fresh":
	default:
		return fmt.Errorf("sandbox.mode must be \"fresh\" or \"off\", got %q", e.Sandbox.Mode)
	}
	switch e.Sandbox.Seed {
	case "", "config", "none":
	default:
		return fmt.Errorf("sandbox.seed must be \"config\" or \"none\", got %q", e.Sandbox.Seed)
	}
	seen := map[string]bool{}
	for i, v := range e.Versions {
		if v.Ref == "" && v.MilkBinary == "" {
			return fmt.Errorf("versions[%d]: needs ref or milk_binary", i)
		}
		label := v.label()
		if seen[label] {
			return fmt.Errorf("duplicate version label %q — labels name report columns and must be unique", label)
		}
		seen[label] = true
	}
	return nil
}

// SandboxEnabled reports whether the experiment asked for fresh sandboxes.
func (e *Experiment) SandboxEnabled() bool { return e.Sandbox.Mode == "fresh" }

// SandboxConfig resolves the experiment's sandbox section into the adapter
// instruction. Seed resolution ("config") happens here so the adapter stays
// dumb: the current config.json (honoring --config-root / $MILK_CONFIG_ROOT)
// is the seed source — "my current agent set" is literally the live config.
func (e *Experiment) SandboxConfig() (SandboxConfig, error) {
	cfg := SandboxConfig{
		Enabled:    e.SandboxEnabled(),
		MilkBinary: e.Sandbox.MilkBinary,
		AllowTools: e.Sandbox.AllowTools,
	}
	if !cfg.Enabled || e.Sandbox.Seed == "none" {
		return cfg, nil
	}
	seed := e.Sandbox.Seed
	if seed == "" {
		seed = "config"
	}
	switch seed {
	case "config":
		// The current milk config — same resolution milk itself uses, so the
		// seed is exactly "my current agent set" even under a custom
		// --config-root. A missing config.json (fresh machine) seeds nothing
		// rather than failing the run.
		if p := configPath(); p != "" {
			if _, err := os.Stat(p); err == nil {
				cfg.SeedConfigPath = p
			}
		}
	default:
		return cfg, fmt.Errorf("sandbox.seed %q: must be \"config\" or \"none\"", seed)
	}
	return cfg, nil
}

// configPath resolves the current milk config.json (honoring --config-root /
// $MILK_CONFIG_ROOT) — the seed source for "my current agent set".
func configPath() string {
	d, err := config.Dir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "config.json")
}

// FilterScenarios narrows loaded scenarios per the experiment's select
// section (or the equivalent CLI flags). Unknown names are an error.
func FilterScenarios(scenarios []Scenario, category string, names []string, multiTurnOnly bool) ([]Scenario, error) {
	if category != "" {
		var filtered []Scenario
		for _, s := range scenarios {
			if strings.EqualFold(s.Category, category) {
				filtered = append(filtered, s)
			}
		}
		scenarios = filtered
	}
	if multiTurnOnly {
		var filtered []Scenario
		for _, s := range scenarios {
			if s.MultiTurn {
				filtered = append(filtered, s)
			}
		}
		scenarios = filtered
	}
	if len(names) > 0 {
		byName := make(map[string]bool, len(scenarios))
		for _, s := range scenarios {
			byName[s.Name] = true
		}
		var missing []string
		for _, n := range names {
			if !byName[n] {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("scenario(s) not found: %s", strings.Join(missing, ", "))
		}
		keep := make(map[string]bool, len(names))
		for _, n := range names {
			keep[n] = true
		}
		var filtered []Scenario
		for _, s := range scenarios {
			if keep[s.Name] {
				filtered = append(filtered, s)
			}
		}
		scenarios = filtered
	}
	return scenarios, nil
}

// RunExperiment executes the experiment's full suite against the harness's
// adapters: scenarios filtered per the experiment's select section, every
// run isolated per the sandbox section, every scenario repeated per the
// repetitions field (fully isolated again), and every run checked against
// the effective budget (the experiment's, refined per scenario).
func (h *Harness) RunExperiment(ctx context.Context, exp *Experiment) ([]ScenarioResult, error) {
	h.progress = newProgressReporter(os.Stderr)
	defer h.progress.Stop()

	// Model prices: the experiment's `pricing:` block overrides the built-in
	// table before any adapter runs, so milk's computed costs use it.
	SetPricing(exp.Pricing)

	scenarios, err := LoadScenarios(exp.Scenarios)
	if err != nil {
		return nil, fmt.Errorf("loading scenarios: %w", err)
	}
	scenarios, err = FilterScenarios(scenarios, exp.Select.Category, exp.Select.Names, exp.Select.MultiTurn)
	if err != nil {
		return nil, err
	}
	if len(scenarios) == 0 {
		return nil, fmt.Errorf("no scenarios found matching the experiment's selection in %s", exp.Scenarios)
	}

	if len(h.Adapters) == 0 {
		return nil, fmt.Errorf("no adapters specified; available: %s", strings.Join(List(), ", "))
	}

	scfg, err := exp.SandboxConfig()
	if err != nil {
		return nil, err
	}
	for _, a := range h.Adapters {
		if sa, ok := a.(Sandboxable); ok {
			sa.SetSandbox(scfg)
		} else if scfg.Enabled {
			h.progress.Logf("note: %s has no sandbox support — running against live state", a.Name())
		}
	}

	h.Repetitions = exp.Repetitions
	h.DefaultBudget = exp.Budget

	h.progress.Logf("Experiment %q: %d scenario(s) × %d repetition(s) × %d adapter(s)",
		nameOr(exp.Name, "(unnamed)"), len(scenarios), exp.Repetitions, len(h.Adapters))

	return h.runScenarios(ctx, scenarios), nil
}

func nameOr(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

// ---------------------------------------------------------------------------
// Version matrix (compare milk versions side by side)
// ---------------------------------------------------------------------------

// label is the version's display label: explicit, else the ref, else the
// binary's basename. Report columns are 12 chars wide — keep labels short.
func (v Version) label() string {
	if v.Label != "" {
		return v.Label
	}
	if v.Ref != "" {
		return v.Ref
	}
	return filepath.Base(expandHome(v.MilkBinary))
}

// resolveBinary returns the milk binary to drive for this version: the
// prebuilt MilkBinary when set, otherwise Ref built (and cached) on demand.
func (v Version) resolveBinary() (string, error) {
	if v.MilkBinary != "" {
		p := expandHome(v.MilkBinary)
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("version %q: milk_binary %s: %w", v.label(), p, err)
		}
		return p, nil
	}
	return buildMilkFromRef(v.Ref)
}

// ResolveAdapterSpecs returns the adapter specs the experiment runs: the
// plain Agents specs plus one labeled milk-tui spec per versions entry
// (`milk-tui[--label,<label>,--milk-bin,<binary>]`), building git refs on
// demand. Labels are validated unique — two adapters sharing a label would
// silently overwrite each other's results.
func (e *Experiment) ResolveAdapterSpecs() ([]string, error) {
	specs := append([]string(nil), e.Agents...)
	seen := map[string]bool{}
	for _, v := range e.Versions {
		bin, err := v.resolveBinary()
		if err != nil {
			return nil, err
		}
		label := v.label()
		if seen[label] {
			return nil, fmt.Errorf("duplicate version label %q — labels name report columns and must be unique", label)
		}
		seen[label] = true
		specs = append(specs, fmt.Sprintf("milk-tui[--label,%s,--milk-bin,%s]", label, bin))
	}
	return specs, nil
}

// buildMilkFromRef builds cmd/milk from a git ref of the milk repo (found via
// the current working directory) in a throwaway git worktree and returns the
// binary path. The binary is cached under the user cache dir keyed by the
// resolved commit — re-runs skip the build, and moving branches rebuild
// automatically when the commit changes.
func buildMilkFromRef(ref string) (string, error) {
	rootBytes, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("version ref %q: not inside a git checkout of the milk repo (%v)", ref, err)
	}
	root := strings.TrimSpace(string(rootBytes))
	shaBytes, err := exec.Command("git", "-C", root, "rev-parse", ref+"^{commit}").Output()
	if err != nil {
		return "", fmt.Errorf("version ref %q: resolving ref: %w", ref, err)
	}
	sha := strings.TrimSpace(string(shaBytes))
	short := sha[:min(12, len(sha))]

	cacheDir, err := versionBuildCacheDir()
	if err != nil {
		return "", err
	}
	bin := filepath.Join(cacheDir, "milk-"+short)
	if _, err := os.Stat(bin); err == nil {
		return bin, nil // already built for this exact commit
	}

	// Throwaway worktree at the ref; removed either way. The build writes to
	// a temp name and renames, so concurrent evals building the same commit
	// can't tear the cached binary.
	parent, err := os.MkdirTemp("", "milk-eval-worktree-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(parent) //nolint:errcheck // best-effort cleanup
	wt := filepath.Join(parent, "wt")
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", wt, ref).CombinedOutput(); err != nil {
		return "", fmt.Errorf("version ref %q: git worktree add: %w\n%s", ref, err, out)
	}
	defer exec.Command("git", "-C", root, "worktree", "remove", "--force", wt).Run() //nolint:errcheck

	tmpBin := fmt.Sprintf("%s.partial-%d", bin, os.Getpid())
	build := exec.Command("go", "build", "-o", tmpBin, "./cmd/milk")
	build.Dir = wt
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		os.Remove(tmpBin)
		return "", fmt.Errorf("version ref %q: go build: %w\n%s", ref, err, out)
	}
	if err := os.Rename(tmpBin, bin); err != nil {
		os.Remove(tmpBin)
		return "", fmt.Errorf("version ref %q: caching built binary: %w", ref, err)
	}
	return bin, nil
}

// versionBuildCacheDir is where version-matrix binaries are cached
// (~/.cache/milk-eval-builds, or a throwaway dir when no cache dir resolves).
func versionBuildCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return os.MkdirTemp("", "milk-eval-builds-")
	}
	dir := filepath.Join(base, "milk-eval-builds")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("creating version build cache %s: %w", dir, err)
	}
	return dir, nil
}

// expandHome expands a leading ~/ to the user's home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
