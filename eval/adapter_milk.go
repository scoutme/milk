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
// are forwarded to the milk invocation unchanged.
type milkAdapter struct {
	workdir   string   // working directory passed to Start
	milkBin   string   // resolved milk binary (overridden in tests)
	extraArgs []string // extra CLI args from --agents spec
	cmd       *exec.Cmd
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

	// system/init + system/state mapping (onto the eval report shapes):
	// model identity + config warnings from init, and the session-state
	// machine's idle stop reason from state (the result.stop_reason fallback).
	Model           string   // system/init primary agent model
	Warnings        []string // system/init.warnings + system/warning messages
	State           string   // last system/state session state (running|idle|requires_action)
	StateStopReason string   // stop_reason from the last system/state on idle
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
			return nil, err
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
			// tool_use_result carries the TUI ⚙ summary (design §6.2); prefer
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
		}
	}

	if !seenResult {
		return nil, fmt.Errorf("milk stream-json run truncated: no terminal result event")
	}
	if run.Response == "" {
		run.Response = run.Text
	}
	// system/state's idle stop_reason fills in for a missing result.stop_reason.
	if run.StopReason == "" {
		run.StopReason = run.StateStopReason
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

func (a *milkAdapter) Name() string          { return "milk-tui" }
func (a *milkAdapter) SetArgs(args []string) { a.extraArgs = args }

func (a *milkAdapter) Start(ctx context.Context, workdir string) error {
	a.workdir = workdir
	if a.milkBin == "" {
		bin, err := milkBinary()
		if err != nil {
			return fmt.Errorf("resolving milk binary: %w", err)
		}
		a.milkBin = bin
	}
	if _, err := os.Stat(a.milkBin); err != nil {
		return fmt.Errorf("milk binary %s: %w", a.milkBin, err)
	}
	return nil
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
	if parseErr != nil {
		return RunResult{}, fmt.Errorf("parsing milk stream-json output: %w", parseErr)
	}
	if waitErr != nil {
		return RunResult{}, fmt.Errorf("milk batch run: %w", waitErr)
	}

	afterFiles := snapshotDir(a.workdir)

	res := RunResult{
		Response:    run.Response,
		Tokens:      run.Tokens,
		Duration:    time.Since(start),
		ToolCalls:   run.ToolCalls,
		FileChanges: diffSnapshots(beforeFiles, afterFiles),
	}
	if run.IsError {
		res.Error = fmt.Errorf("milk run failed (result subtype %q): %s", run.Subtype, run.Response)
	}
	return res, nil
}

// Stop kills an in-flight batch run, if any. One-shot runs need no other
// teardown (the tmux session the TUI adapter drove is gone by design).
func (a *milkAdapter) Stop() error {
	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
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
