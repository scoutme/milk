package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/scoutme/milk/internal/transport/streamjson"
)

func init() {
	Register("milk-tui", func() AgentAdapter { return &milkAdapter{} })
}

// milkAdapter implements AgentAdapter by driving milk in batch mode: one
// `milk --output-format stream-json <prompt>` subprocess per turn, consuming
// the machine-readable JSONL event stream from stdout (ADR-0049 / ADR-0050,
// design §6). This replaces the old session-file scraping — the terminal
// result line carries the turn's response, token usage (incl. cache_read /
// cache_creation) and route history directly, so no ~/.milk/sessions state is
// touched. Multi-turn scenarios keep their conversation because milk keys
// sessions per working directory and each batch run resumes the workdir's
// session. Extra args from the --agents spec (e.g. ["--agent", "mimo-local"])
// are forwarded to the milk invocation unchanged, except milk-tui's own
// pseudo-flags (--sandbox, --sandbox-seed, --milk-bin — see SetArgs), which
// configure the adapter instead.
type milkAdapter struct {
	workdir   string   // working directory passed to Start
	milkBin   string   // resolved milk binary (overridden in tests)
	extraArgs []string // extra CLI args from --agents spec
	cmd       *exec.Cmd

	// sandbox is the isolated-state-root config (--sandbox / --milk-bin
	// pseudo-flags in the --agents spec, or the experiment's sandbox:
	// section via SetSandbox). When enabled, each Start gets a brand-new
	// MILK_CONFIG_ROOT so no run sees a previous run's sessions or memory.
	sandbox    SandboxConfig
	sandboxDir string // created per Start, removed per Stop

	// label renames this adapter in reports and results.json (Name()) — for
	// side-by-side version matrices where several milk-tui adapters run the
	// same suite against different milk binaries. Empty = "milk-tui".
	label string
}

// milkBinary resolves the milk binary to launch. A variable so tests can
// point the adapter at a recorded-run stub.
var milkBinary = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin", "milk"), nil
}

// --- parsing milk's stream-json batch output ---

// milkStreamRun is everything one `--output-format stream-json` run reports on
// stdout, extracted from the §6 event catalog.
type milkStreamRun struct {
	Response   string // result.result, falling back to concatenated assistant text
	Text       string // concatenated assistant text blocks
	Tokens     TokenUsage
	ToolCalls  []ToolCall
	Subtype    string // result subtype (open set)
	IsError    bool
	NumTurns   int64
	DurationMS int64
	StopReason string
	RouteHist  []string // route_history targets, one per turn hop
	TTFTMS     int64    // result.ttft_ms — mean time-to-first-token
	OutputTPS  float64  // result.output_tokens_per_second — generation throughput

	// system/init + system/state mapping (onto the eval report shapes):
	// model identity + config warnings from init, and the session-state
	// machine's idle stop reason from state (the result.stop_reason fallback).
	Model           string   // system/init primary agent model
	Warnings        []string // system/init.warnings + system/warning messages
	State           string   // last system/state session state (running|idle|requires_action)
	StateStopReason string   // stop_reason from the last system/state on idle

	// CostUSD is the computed dollar cost of this turn: the pricing table
	// (builtin + experiment `pricing:`) over the result's per-model usage
	// buckets, falling back to the init-reported primary model over the total
	// tokens. 0 when no price is known — never a fabricated figure
	// (see ComputeCost).
	CostUSD float64
}

// parseMilkStream consumes a complete stream-json run (init … result) and
// extracts the eval-relevant fields. It requires the terminal result line: a
// stream that ends without one is a truncated run and returns an error.
// Mapping onto the eval shapes: result.usage (or the model_usage sum) →
// TokenUsage; system/init → model + warnings; system/state → session state +
// the idle stop_reason fallback; assistant.tool_use / user.tool_result →
// ToolCalls (tool_use_result.summary preferred as the result). Everything else
// is open-set (§8.3) and ignored, never rejected.
func parseMilkStream(r io.Reader) (*milkStreamRun, error) {
	dec := streamjson.NewDecoder(r)
	run := &milkStreamRun{}
	toolIndex := map[string]int{}
	seenResult := false

	for {
		ev, err := dec.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Keep the partial run: a killed process (turn deadline, crash)
			// still streamed everything before the cut, and the harness
			// records partial results next to the error.
			return run, err
		}
		switch ev.Type {
		case streamjson.TypeSystem:
			// system/init + system/state map onto the eval report shapes:
			// model identity + config warnings (init), and the session-state
			// machine incl. the idle stop reason (state — applied below as the
			// result.stop_reason fallback).
			switch ev.Subtype {
			case streamjson.SubtypeInit:
				agent, err := ev.AsAgent()
				if err != nil {
					return nil, err
				}
				if agent != nil && run.Model == "" {
					run.Model = agent.Model
				}
				run.Warnings = append(run.Warnings, ev.Warnings...)
			case streamjson.SubtypeState:
				// ADR-0050: state events carry `session_state` (the same key
				// as init) + `stop_reason` on idle.
				run.State = ev.SessionState
				if ev.StopReason != "" {
					run.StateStopReason = ev.StopReason
				}
			case streamjson.SubtypeWarning:
				if msg, err := ev.MessageText(); err == nil && msg != "" {
					run.Warnings = append(run.Warnings, msg)
				}
			default:
				// open set (§8.3): agent_switch, route, notification, task_*,
				// memory, commands, config_option, permission_denied, error —
				// carry nothing the eval report needs.
			}

		case streamjson.TypeAssistant:
			msg, err := ev.AsMessage()
			if err != nil {
				return nil, err
			}
			if msg == nil {
				continue
			}
			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					run.Text += block.Text
				case "tool_use":
					args, err := normalizeJSON(block.Input)
					if err != nil {
						return nil, err
					}
					toolIndex[block.ID] = len(run.ToolCalls)
					run.ToolCalls = append(run.ToolCalls, ToolCall{Name: block.Name, Args: args})
				}
			}

		case streamjson.TypeUser:
			msg, err := ev.AsMessage()
			if err != nil {
				return nil, err
			}
			if msg == nil {
				continue
			}
			// tool_use_result carries the TUI » summary (design §6.2); prefer
			// it over the raw tool_result payload as ToolCall.Result. It is
			// free-form on the wire — shapes without a summary/path fall back
			// to the tool_result content.
			var tuResult struct {
				Path    string `json:"path"`
				IsError bool   `json:"is_error"`
				Summary string `json:"summary"`
			}
			_ = json.Unmarshal(ev.ToolUseResult, &tuResult) // free-form: tolerate non-object payloads
			for _, block := range msg.Content {
				if block.Type != "tool_result" {
					continue
				}
				idx, ok := toolIndex[block.ToolUseID]
				if !ok {
					continue
				}
				resultText := rawText(block.Content)
				if tuResult.Summary != "" {
					resultText = tuResult.Summary
				} else if tuResult.Path != "" {
					resultText = tuResult.Path
				}
				run.ToolCalls[idx].Result = resultText
			}

		case streamjson.TypeResult:
			seenResult = true
			run.Subtype = ev.Subtype
			run.IsError = ev.IsError != nil && *ev.IsError
			run.NumTurns = ev.NumTurns
			run.DurationMS = ev.DurationMS
			run.StopReason = ev.StopReason
			run.TTFTMS = ev.TTFTMS
			run.OutputTPS = ev.OutputTokensPerSecond
			for _, hop := range ev.RouteHistory {
				run.RouteHist = append(run.RouteHist, hop.Target)
			}
			run.Response = ev.Result
			if ev.Usage != nil {
				run.Tokens = usageToTokens(ev.Usage)
			} else if len(ev.ModelUsage) > 0 {
				// No `usage` total on the result line: sum the per-model
				// breakdown (§6.4 model_usage) into the same eval shape.
				for _, u := range ev.ModelUsage {
					bucket := usageToTokens(u)
					run.Tokens.InputTokens += bucket.InputTokens
					run.Tokens.OutputTokens += bucket.OutputTokens
					run.Tokens.CacheRead += bucket.CacheRead
					run.Tokens.CacheCreate += bucket.CacheCreate
				}
			}
			// Price the turn: prefer the per-model buckets (an escalation hop
			// may be priced by a different model than the session's primary),
			// falling back to the init-reported primary model over the total
			// tokens. Unknown models contribute 0 (never a fake dollar figure).
			if len(ev.ModelUsage) > 0 {
				for m, u := range ev.ModelUsage {
					run.CostUSD += ComputeCost(m, usageToTokens(u))
				}
			}
			if run.CostUSD == 0 && run.Model != "" {
				run.CostUSD = ComputeCost(run.Model, run.Tokens)
			}
		}
	}

	if run.Response == "" {
		run.Response = run.Text
	}
	// system/state's idle stop_reason fills in for a missing result.stop_reason.
	if run.StopReason == "" {
		run.StopReason = run.StateStopReason
	}
	if !seenResult {
		// A killed run (turn deadline, crash, non-zero exit) never writes the
		// terminal result line: return the partial run WITH the error so the
		// streamed output survives — the harness keeps partials, and an empty
		// run would score a long killed turn as nothing.
		return run, fmt.Errorf("milk stream-json run truncated: no terminal result event")
	}
	return run, nil
}

// usageToTokens maps one §6.4 usage bucket (snake_case cache_read /
// cache_creation) onto the eval TokenUsage shape (cache_create).
func usageToTokens(u *streamjson.Usage) TokenUsage {
	if u == nil {
		return TokenUsage{}
	}
	return TokenUsage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		CacheRead:    u.CacheRead,
		CacheCreate:  u.CacheCreation,
	}
}

// normalizeJSON renders free-form JSON payloads (tool_use.input) as a compact
// string for the report; a nil/absent payload becomes "{}".
func normalizeJSON(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "{}", nil
	}
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, raw); err != nil {
		return "", fmt.Errorf("tool input is not valid JSON: %w", err)
	}
	return compacted.String(), nil
}

// rawText renders a tool_result payload (string or structured) as text.
func rawText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	return string(content)
}

// --- AgentAdapter ---

func (a *milkAdapter) Name() string {
	if a.label != "" {
		return a.label
	}
	return "milk-tui"
}

// SetArgs splits milk-tui's pseudo-flags (--sandbox, --sandbox-seed,
// --milk-bin — adapter behavior, never forwarded to milk) from the extra
// CLI flags forwarded verbatim onto the milk launch command.
func (a *milkAdapter) SetArgs(args []string) {
	var forwarded []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--sandbox":
			a.sandbox.Enabled = true
		case "--sandbox-seed":
			if i+1 < len(args) {
				i++
				a.sandbox.SeedConfigPath = args[i]
				a.sandbox.Enabled = true
			}
		case "--milk-bin":
			if i+1 < len(args) {
				i++
				a.sandbox.MilkBinary = args[i]
			}
		case "--allow-tools":
			a.sandbox.AllowTools = true
		case "--label":
			if i+1 < len(args) {
				i++
				a.label = args[i]
			}
		default:
			forwarded = append(forwarded, args[i])
		}
	}
	a.extraArgs = forwarded
}

// SetSandbox applies an experiment's sandbox section. Explicit adapter-spec
// pseudo-flags (parsed first in SetArgs) win over experiment values: the
// more specific setting is the one that survives.
func (a *milkAdapter) SetSandbox(c SandboxConfig) {
	if c.Enabled && !a.sandbox.Enabled {
		a.sandbox.Enabled = true
	}
	if a.sandbox.SeedConfigPath == "" {
		a.sandbox.SeedConfigPath = c.SeedConfigPath
	}
	if a.sandbox.MilkBinary == "" {
		a.sandbox.MilkBinary = c.MilkBinary
	}
	if c.AllowTools {
		a.sandbox.AllowTools = true
	}
}

func (a *milkAdapter) Start(ctx context.Context, workdir string) error {
	a.workdir = workdir
	if a.milkBin == "" {
		bin := expandHome(a.sandbox.MilkBinary)
		if bin == "" {
			resolved, err := milkBinary()
			if err != nil {
				return fmt.Errorf("resolving milk binary: %w", err)
			}
			bin = resolved
		}
		a.milkBin = bin
	}
	if _, err := os.Stat(a.milkBin); err != nil {
		return fmt.Errorf("milk binary %s: %w", a.milkBin, err)
	}
	if a.sandbox.Enabled {
		if err := a.setupSandbox(); err != nil {
			return err
		}
	}
	return nil
}

// setupSandbox creates a brand-new milk state root for this run and seeds
// its config.json from the current one (the agent set and settings — the
// point is skipping the *state*: sessions, memory, history, jobs, otel and
// mcp_oauth all live under the state root and start empty). Seeding is a
// plain copy of config.json: deterministically "my current agent set," no
// more and no less.
func (a *milkAdapter) setupSandbox() error {
	// Defensive: Start without a prior Stop (never happens in the harness
	// lifecycle) must still not leak the previous root.
	if a.sandboxDir != "" {
		_ = os.RemoveAll(a.sandboxDir)
		a.sandboxDir = ""
	}
	dir, err := os.MkdirTemp("", "milk-eval-home-")
	if err != nil {
		return fmt.Errorf("creating sandbox state root: %w", err)
	}
	if a.sandbox.SeedConfigPath != "" {
		seed, err := os.ReadFile(expandHome(a.sandbox.SeedConfigPath))
		if err != nil {
			os.RemoveAll(dir)
			return fmt.Errorf("reading sandbox seed config %s: %w", a.sandbox.SeedConfigPath, err)
		}
		if a.sandbox.AllowTools {
			seed, err = allowToolsInConfig(seed)
			if err != nil {
				os.RemoveAll(dir)
				return fmt.Errorf("applying sandbox allow_tools: %w", err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), seed, 0644); err != nil {
			os.RemoveAll(dir)
			return fmt.Errorf("seeding sandbox config: %w", err)
		}
	}
	a.sandboxDir = dir
	return nil
}

// allowToolsInConfig returns config JSON with dangerously_skip_permissions
// set on every entry of the agents array — batch runs have no one to answer
// a permission ask, so mutating tools would be denied without it. Unknown
// shapes pass through untouched; the live config is never modified (this
// only ever runs on a seeded sandbox copy).
func allowToolsInConfig(cfg []byte) ([]byte, error) {
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.UseNumber() // preserve large integer values byte-exact through re-encode
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	agents, ok := doc["agents"].([]any)
	if !ok {
		return cfg, nil
	}
	for _, entry := range agents {
		if agent, ok := entry.(map[string]any); ok {
			agent["dangerously_skip_permissions"] = true
		}
	}
	return json.Marshal(doc)
}

// RunPrompt runs one batch turn: `milk --output-format stream-json <prompt>`
// in the scenario workdir. stdout is the event stream (consumed via
// parseMilkStream); stderr is prose and is forwarded unchanged per the
// contract's stdout/stderr split.
func (a *milkAdapter) RunPrompt(ctx context.Context, prompt string) (RunResult, error) {
	start := time.Now()

	beforeFiles := snapshotDir(a.workdir)

	args := []string{"--output-format", "stream-json"}
	args = append(args, a.extraArgs...)
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, a.milkBin, args...)
	cmd.Dir = a.workdir
	cmd.Stderr = os.Stderr
	if a.sandboxDir != "" {
		cmd.Env = append(os.Environ(), "MILK_CONFIG_ROOT="+a.sandboxDir)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return RunResult{}, fmt.Errorf("stdout pipe: %w", err)
	}
	a.cmd = cmd
	defer func() { a.cmd = nil }()

	if err := cmd.Start(); err != nil {
		return RunResult{}, fmt.Errorf("starting milk batch run: %w", err)
	}

	// Consume the stream while the process runs (stream-safe: one event per
	// line, nothing buffered until exit).
	run, parseErr := parseMilkStream(stdout)
	waitErr := cmd.Wait()

	// Map whatever the stream produced even on failure: a killed run (turn
	// deadline, crash, non-zero exit) still streamed output and file edits
	// before dying, and the harness keeps a partial result next to its error.
	// Returning an empty result here scored a ~10-minute killed turn as 0.
	afterFiles := snapshotDir(a.workdir)
	res := milkRunResult(run, start, beforeFiles, afterFiles)
	if parseErr != nil {
		return res, fmt.Errorf("parsing milk stream-json output: %w", parseErr)
	}
	if waitErr != nil {
		if res.Error != nil {
			return res, fmt.Errorf("milk batch run: %w (stream: %v)", waitErr, res.Error)
		}
		return res, fmt.Errorf("milk batch run: %w", waitErr)
	}
	return res, nil
}

// milkRunResult maps a parsed stream (full or partial) onto the eval
// RunResult shape.
func milkRunResult(run *milkStreamRun, start time.Time, beforeFiles, afterFiles map[string]string) RunResult {
	res := RunResult{
		Response:              run.Response,
		Tokens:                run.Tokens,
		CostUSD:               run.CostUSD,
		Duration:              time.Since(start),
		ToolCalls:             run.ToolCalls,
		FileChanges:           diffSnapshots(beforeFiles, afterFiles),
		Model:                 run.Model,
		NumTurns:              run.NumTurns,
		StopReason:            run.StopReason,
		RouteHistory:          run.RouteHist,
		TTFTMS:                run.TTFTMS,
		OutputTokensPerSecond: run.OutputTPS,
		Warnings:              run.Warnings,
	}
	if run.IsError {
		res.Error = fmt.Errorf("milk run failed (result subtype %q): %s", run.Subtype, run.Response)
	}
	return res
}

// Stop kills an in-flight batch run, if any, and tears down the sandbox
// state root (a fresh one is created per Start — per scenario run, per
// repetition).
func (a *milkAdapter) Stop() error {
	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
	}
	if a.sandboxDir != "" {
		_ = os.RemoveAll(a.sandboxDir)
		a.sandboxDir = ""
	}
	return nil
}

// snapshotDir reads all files in dir and returns a map of relative path → content.
// Skips directories and files larger than 1MB (to avoid snapshotting binaries/logs).
func snapshotDir(dir string) map[string]string {
	snap := make(map[string]string)
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Size() > 1_000_000 {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		snap[rel] = string(data)
		return nil
	})
	return snap
}

// diffSnapshots compares before and after file snapshots and returns FileChanges.
func diffSnapshots(before, after map[string]string) []FileChange {
	var changes []FileChange

	// Check for modified and new files.
	for path, afterContent := range after {
		beforeContent, existed := before[path]
		if !existed {
			changes = append(changes, FileChange{
				Path:  path,
				After: afterContent,
				IsNew: true,
			})
		} else if beforeContent != afterContent {
			changes = append(changes, FileChange{
				Path:     path,
				Before:   beforeContent,
				After:    afterContent,
				Modified: true,
			})
		}
	}

	// Check for deleted files.
	for path, beforeContent := range before {
		if _, exists := after[path]; !exists {
			changes = append(changes, FileChange{
				Path:   path,
				Before: beforeContent,
			})
		}
	}

	return changes
}
