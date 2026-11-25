package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func init() {
	Register("pi", func() AgentAdapter { return &piAdapter{} })
}

// piAdapter implements AgentAdapter by driving pi in batch mode: one
// `pi --mode json [--continue] -- <prompt>` subprocess per turn, consuming
// the machine-readable JSONL event stream from stdout (pi's docs/json.md
// contract: strict JSONL, diagnostics on stderr). This mirrors
// milkAdapter's and mimoAdapter's headless design (one process per turn, no
// session-file scraping): a `session` header opens the stream, each
// `message_end` for role=assistant carries that response's final text,
// model identity, stop reason and per-message usage (incl. cache split and
// the catalog-derived cost), `tool_execution_start`/`_end` pairs carry the
// tool trail, assistant `message_end` count marks each model iteration, and
// `agent_settled` is the terminal event. Multi-turn scenarios keep their conversation the same
// way milk does: pi keys sessions per working directory, so a fresh temp
// workdir starts new and `--continue` from turn 2 on resumes this scenario's
// own session — no session id needs capturing (unlike mimo's `-s`). Batch
// runs need no approval flag: pi executes mutating tools without an
// interactive permission ask in json mode (verified against a live install).
// Extra args from the --agents spec are forwarded verbatim onto the `pi`
// invocation (e.g. `pi[--model,xiaomi/mimo-v2.6-flash]`), except pi's own
// pseudo-flag --pi-bin (see SetArgs).
type piAdapter struct {
	workdir   string   // working directory passed to Start
	piBin     string   // resolved pi binary (overridden in tests)
	extraArgs []string // extra CLI args from --agents spec
	cmd       *exec.Cmd

	// turns run since Start: turn 2+ passes --continue to resume the
	// workdir's session (turn 1 never does — a fresh scenario must not
	// attach to anything). Reset per Start — the harness reuses adapter
	// instances across scenarios, each in a fresh workdir.
	turns int

	// binArg pins the binary to launch (--pi-bin pseudo-flag).
	binArg string

	// isolation is the resolved filesystem isolation (issue #355, "" = off):
	// IsolateBwrap wraps every batch run in a bubblewrap sandbox with this
	// adapter's state root writable.
	isolation string
}

// piBinary resolves the pi CLI to launch. A variable so tests can point the
// adapter at a recorded-run stub. Found on PATH — pi installs `pi` there
// (e.g. ~/.local/bin/pi).
var piBinary = func() (string, error) {
	return exec.LookPath("pi")
}

// --- parsing pi's `--mode json` event stream ---

// piStreamRun is everything one `pi --mode json` run reports on stdout,
// extracted from the observed event catalog (session, message_end,
// tool_execution_start/_end, turn_end, agent_settled, plus open-set
// tolerance for anything else — auto_retry_*, compaction_*, queue_update,
// message_update, …).
type piStreamRun struct {
	Response   string // the final assistant message's text (falls back to all text)
	Text       string // concatenated assistant text across the whole run
	Tokens     TokenUsage
	CostUSD    float64 // summed message-end usage.cost.total — pi reports it (models.dev catalog)
	ToolCalls  []ToolCall
	NumTurns   int64    // assistant message_end count = model iterations
	StopReason string   // last assistant message's stop_reason ("stop", "error", …)
	Model      string   // last assistant message's model identity
	ErrorMsg   string   // from a terminal stop_reason of "error"/"aborted", or agent_settled
	Settled    bool     // agent_settled seen — the run is complete
	Warnings   []string // non-JSON stdout lines, kept instead of failing the turn
}

// piWireEvent is the envelope of every JSONL line pi emits. Content-carrying
// payloads share the `message` slot (message_end) or the tool-execution
// slots; unknown events decode with everything empty and are ignored.
type piWireEvent struct {
	Type       string          `json:"type"`
	Message    json.RawMessage `json:"message"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Result     json.RawMessage `json:"result"`
	Aborted    bool            `json:"aborted"`
}

// piMessage is a message_end payload. Role selects which fields matter:
// only assistant messages carry usage/model/stop_reason and an array
// content (system messages serialize content as a plain string, so content
// stays raw until the role is known).
type piMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	Model        string          `json:"model"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	Usage        *piUsage        `json:"usage"`
}

// piContentBlock is one assistant content item; only "text" blocks join the
// response ("thinking" and "toolCall" do not — the judge sees final prose,
// the tool trail comes from the tool_execution_* events).
type piContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// piUsage is one assistant message's usage. Semantics differ from mimo's
// step tokens in two ways the mapping must respect: `output` ALREADY
// includes reasoning (pi-ai's Usage: reasoning is a subset of output — do
// not add it again), and `input` excludes cached tokens (cacheRead carries
// them — summing input over messages totals uncached input, cacheRead
// totals cached reads, exactly the split eval's TokenUsage expects).
type piUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Cost       struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

// piToolResult is a tool_execution_end `result` payload: an object whose
// content array carries text blocks (structuredContent is ignored — the
// text blocks are the same output).
type piToolResult struct {
	Content []piContentBlock `json:"content"`
}

// parsePiStream consumes a complete `pi --mode json` run and extracts the
// eval-relevant fields. It requires `agent_settled` (pi's terminal event —
// "pi will not continue automatically through retries, compaction recovery,
// or queued messages"); a stream without one is a truncated run and returns
// the partial run WITH the error so the harness keeps whatever streamed
// before the cut (a killed turn must not score as nothing — the milk
// parser's lesson). Mapping onto the eval shapes: assistant message_end
// usage summed → TokenUsage (output already includes reasoning; input and
// cacheRead stay separate, matching how eval counts the other adapters) and
// CostUSD; assistant message_end count → NumTurns; tool_execution pairs →
// ToolCalls (args from _start, result text from _end, keyed by toolCallId);
// the final assistant message's text → Response. Non-JSON stdout lines are
// warnings; unknown event types, other roles' message_end, and the streaming
// `message_update` (whose usage is cumulative per response — summing it
// would multiply every turn) are ignored, never rejected.
func parsePiStream(r io.Reader) (*piStreamRun, error) {
	run := &piStreamRun{}
	toolIndex := map[string]int{}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // tool results can be large

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev piWireEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			// pi promises strict JSONL on stdout, but tolerate prose the
			// same way the mimo parser does — keep the first few lines as
			// warnings instead of failing the turn.
			if len(run.Warnings) < 5 {
				run.Warnings = append(run.Warnings, line)
			}
			continue
		}

		switch ev.Type {
		case "message_end":
			var m piMessage
			if err := json.Unmarshal(ev.Message, &m); err != nil {
				return run, fmt.Errorf("pi message_end: %w", err)
			}
			if m.Role != "assistant" {
				continue // system/user/toolResult: no usage, no response text
			}
			run.NumTurns++
			if m.Usage != nil {
				run.Tokens.InputTokens += m.Usage.Input
				run.Tokens.OutputTokens += m.Usage.Output // reasoning already included
				run.Tokens.CacheRead += m.Usage.CacheRead
				run.Tokens.CacheCreate += m.Usage.CacheWrite
				run.CostUSD += m.Usage.Cost.Total
			}
			// Content decode is tolerant: the response needs the text
			// blocks, but a malformed content array must not lose the
			// turn's tokens/model/stop_reason.
			var blocks []piContentBlock
			_ = json.Unmarshal(m.Content, &blocks)
			var text strings.Builder
			for _, b := range blocks {
				if b.Type == "text" {
					text.WriteString(b.Text)
				}
			}
			run.Text += text.String()
			// Response tracks the latest assistant message — the last one
			// wins (earlier narration must not leak into the judged
			// response).
			run.Response = text.String()
			run.StopReason = m.StopReason
			run.Model = m.Model
			// Run health tracks the LAST assistant message: pi auto-retries
			// internally, so a successful message supersedes an earlier
			// failed attempt (settled-aborted below only fills the gap
			// when no message carried the reason).
			switch m.StopReason {
			case "error", "aborted":
				run.ErrorMsg = m.ErrorMessage
				if run.ErrorMsg == "" {
					run.ErrorMsg = "run " + m.StopReason
				}
			default:
				run.ErrorMsg = ""
			}

		case "tool_execution_start":
			args, err := normalizeJSON(ev.Args)
			if err != nil {
				return run, fmt.Errorf("pi tool %s args: %w", ev.ToolName, err)
			}
			toolIndex[ev.ToolCallID] = len(run.ToolCalls)
			run.ToolCalls = append(run.ToolCalls, ToolCall{Name: ev.ToolName, Args: args})

		case "tool_execution_end":
			result := piToolResultText(ev.Result)
			if idx, ok := toolIndex[ev.ToolCallID]; ok {
				run.ToolCalls[idx].Result = result
			} else {
				// _end without _start (stream began mid-execution): keep
				// the call with empty args rather than dropping evidence
				// that it happened.
				run.ToolCalls = append(run.ToolCalls, ToolCall{Name: ev.ToolName, Args: "{}", Result: result})
			}

		case "agent_settled":
			run.Settled = true
			if ev.Aborted && run.ErrorMsg == "" {
				run.ErrorMsg = "run aborted"
			}

		default:
			// open set: session, agent_start/end, turn_start/end,
			// message_start, message_update (streaming usage — cumulative,
			// never summed), tool_execution_update, auto_retry_*,
			// compaction_*, queue_update, entry_appended, … carry nothing
			// the eval report needs.
		}
	}
	if err := sc.Err(); err != nil {
		return run, err
	}

	if run.Response == "" {
		// No final prose (a run cut after a tool call): fall back to every
		// text block so the turn still has a response to judge.
		run.Response = run.Text
	}
	if !run.Settled {
		return run, fmt.Errorf("pi run truncated: no agent_settled event")
	}
	return run, nil
}

// finalText joins the text blocks of one message in order.
func finalText(blocks []piContentBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// piToolResultText renders a tool_execution_end result as text: the joined
// text blocks when the payload is the expected object, else the raw payload
// (string or structured) rather than losing the output.
func piToolResultText(result json.RawMessage) string {
	var r piToolResult
	if err := json.Unmarshal(result, &r); err == nil {
		if text := finalText(r.Content); text != "" {
			return text
		}
	}
	return rawText(result)
}

// --- AgentAdapter ---

func (a *piAdapter) Name() string { return "pi" }

// SetArgs splits pi's pseudo-flag (--pi-bin — adapter behavior, never
// forwarded to pi) from the extra CLI flags forwarded verbatim onto the `pi`
// command (e.g. --model to pin the model for this eval run).
func (a *piAdapter) SetArgs(args []string) {
	var forwarded []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pi-bin":
			if i+1 < len(args) {
				i++
				a.binArg = args[i]
			}
		default:
			forwarded = append(forwarded, args[i])
		}
	}
	a.extraArgs = forwarded
}

func (a *piAdapter) Start(ctx context.Context, workdir string) error {
	a.workdir = workdir
	// Fresh pi session per scenario: the harness reuses this instance
	// across scenarios, each in its own temp workdir, and turn 1 of each
	// scenario must not pass --continue.
	a.turns = 0
	if a.piBin == "" {
		bin := expandHome(a.binArg)
		if bin == "" {
			resolved, err := piBinary()
			if err != nil {
				return fmt.Errorf("resolving pi binary: %w", err)
			}
			bin = resolved
		}
		a.piBin = bin
	}
	if _, err := os.Stat(a.piBin); err != nil {
		return fmt.Errorf("pi binary %s: %w", a.piBin, err)
	}
	return nil
}

// SetIsolation applies the harness-resolved filesystem isolation (issue
// #355): IsolateBwrap wraps every batch run in a bubblewrap sandbox.
func (a *piAdapter) SetIsolation(effective string) { a.isolation = effective }

// isolateRW is this adapter's legitimate writable surface outside the
// scenario workdir: pi's state root (~/.pi — sessions resume from there,
// auth and trust live there). pi-code runs are not state-sandboxed today
// and isolation must not change what's measured (issue #355 constraint 5),
// so it stays writable — the config it reads (settings, models-store)
// falls out of the same bind.
func (a *piAdapter) isolateRW() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{ensureRWDir(filepath.Join(home, ".pi"))}
}

// RunPrompt runs one batch turn: `pi --mode json [--continue]
// <extraArgs> -- <prompt>` in the scenario workdir. stdout is the event
// stream (consumed via parsePiStream); stderr is captured (capped) so a
// failed run reports what pi complained about. Turn 2+ adds --continue to
// resume the workdir's own session (pi keys sessions per directory, like
// milk — no session id to capture).
func (a *piAdapter) RunPrompt(ctx context.Context, prompt string) (RunResult, error) {
	start := time.Now()

	beforeFiles := snapshotDir(a.workdir)

	args := []string{"--mode", "json"}
	if a.turns > 0 {
		args = append(args, "--continue")
	}
	args = append(args, a.extraArgs...)
	args = append(args, "--", prompt) // `--` guards prompts starting with "-"

	cmd := isolatedExec(ctx, a.isolation, append([]string{a.piBin}, args...), a.workdir, a.isolateRW()...)
	stderr := &tailBuffer{max: 8192}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return RunResult{}, fmt.Errorf("stdout pipe: %w", err)
	}
	a.cmd = cmd
	defer func() { a.cmd = nil }()

	if err := cmd.Start(); err != nil {
		return RunResult{}, fmt.Errorf("starting pi batch run: %w", err)
	}
	a.turns++

	// Consume the stream while the process runs (one JSON value per line).
	run, parseErr := parsePiStream(stdout)
	waitErr := cmd.Wait()

	// Map whatever the stream produced even on failure: a killed run still
	// streamed responses, edits and tool calls before dying, and the
	// harness keeps a partial result next to its error — an empty result
	// would score a long killed turn as 0.
	afterFiles := snapshotDir(a.workdir)
	res := RunResult{
		Response:    run.Response,
		Tokens:      run.Tokens,
		CostUSD:     run.CostUSD,
		Duration:    time.Since(start),
		ToolCalls:   run.ToolCalls,
		FileChanges: diffSnapshots(beforeFiles, afterFiles),
		Model:       run.Model,
		NumTurns:    run.NumTurns,
		StopReason:  run.StopReason,
		Warnings:    run.Warnings,
	}
	if run.ErrorMsg != "" {
		res.Error = fmt.Errorf("pi run failed: %s", run.ErrorMsg)
	}
	if parseErr != nil {
		return res, fmt.Errorf("parsing pi run output: %w", parseErr)
	}
	if waitErr != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return res, fmt.Errorf("pi run: %w (stderr: %s)", waitErr, msg)
		}
		return res, fmt.Errorf("pi run: %w", waitErr)
	}
	return res, nil
}

// Stop kills an in-flight batch run, if any. No state root to tear down —
// pi-code, like mimo-code, runs against the live pi state (sessions
// accumulate under ~/.pi; each scenario still starts a fresh session
// because Start resets the --continue gate and every scenario gets its own
// temp workdir).
func (a *piAdapter) Stop() error {
	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
	}
	return nil
}
