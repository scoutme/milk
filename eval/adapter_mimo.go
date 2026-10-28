package eval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

func init() {
	Register("mimo-code", func() AgentAdapter { return &mimoAdapter{} })
}

// mimoAdapter implements AgentAdapter by driving mimocode (`mimo`) in batch
// mode: one `mimo run --format json <prompt>` subprocess per turn, consuming
// the machine-readable JSONL event stream from stdout. This mirrors
// milkAdapter's headless design (one process per turn, no session-file
// scraping): a `text` event carries the response, each `step_finish` carries
// that model step's token usage and cost, `tool_use` events carry the tool
// trail, and every event repeats the `sessionID` — which the adapter stashes
// and resumes via `-s` on the next turn, so multi-turn scenarios keep their
// conversation (mimo's equivalent of milk's workdir-keyed session
// continuation). Extra args from the --agents spec are forwarded to the
// `mimo run` invocation unchanged, except mimo-code's own pseudo-flags
// (--mimo-bin, --no-yolo — see SetArgs).
type mimoAdapter struct {
	workdir   string   // working directory passed to Start
	mimoBin   string   // resolved mimo binary (overridden in tests)
	extraArgs []string // extra CLI args from --agents spec
	cmd       *exec.Cmd

	// sessionID is captured from the first turn's events and passed via -s
	// on later turns (multi-turn scenarios). Reset per Start — the harness
	// reuses adapter instances across scenarios, each in a fresh workdir.
	sessionID string

	// noYolo disables the default --yolo (auto-approve permissions): batch
	// runs have no interactive permission answer, so without it mutating
	// tools get denied and code-editing scenarios silently degrade.
	noYolo bool

	// binArg pins the binary to launch (--mimo-bin pseudo-flag).
	binArg string
}

// mimoBinary resolves the mimocode CLI to launch. A variable so tests can
// point the adapter at a recorded-run stub. Found on PATH — mimocode
// installs `mimo` there (e.g. ~/.mimocode/bin).
var mimoBinary = func() (string, error) {
	return exec.LookPath("mimo")
}

// --- parsing mimo's `--format json` event stream ---

// mimoStreamRun is everything one `mimo run --format json` run reports on
// stdout, extracted from the observed event catalog (`step_start`, `text`,
// `tool_use`, `step_finish`, plus open-set tolerance for anything else).
type mimoStreamRun struct {
	Response   string // the final assistant message's text (falls back to all text)
	Text       string // concatenated assistant text across the whole run
	SessionID  string // the run's mimo session (resume key for later turns)
	Tokens     TokenUsage
	CostUSD    float64 // summed step cost — mimo reports it, unlike milk adapters
	ToolCalls  []ToolCall
	NumTurns   int64    // step_finish count = model iterations (1 + tool-calling iterations)
	StopReason string   // last step_finish reason ("stop", "tool-calls", …)
	ErrorMsg   string   // from an `error` event, if any
	Warnings   []string // non-JSON stdout lines, kept instead of failing the turn
}

// mimoWireEvent is the envelope of every JSONL line mimo emits.
type mimoWireEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID"`
	Part      json.RawMessage `json:"part"`
}

// mimoTextPart is a `text` event's part: one assistant text block, owned by
// a messageID (the run can emit several messages — narration before tool
// calls, then the final answer).
type mimoTextPart struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	MessageID string `json:"messageID"`
}

// mimoToolPart is a `tool_use` event's part: a completed tool call with its
// input and output. The same completed call is re-emitted across its
// lifecycle events (and once more after the step's step_finish), so the
// parser dedupes by callID.
type mimoToolPart struct {
	Type      string `json:"type"`
	Execution string `json:"execution"`
	CallID    string `json:"callID"`
	Tool      string `json:"tool"`
	State     struct {
		Status string          `json:"status"`
		Input  json.RawMessage `json:"input"`
		Output json.RawMessage `json:"output"`
	} `json:"state"`
}

// mimoStepFinishPart is a `step_finish` event's part: one model step's
// usage (per-step, so the parser sums over the run) and stop reason.
type mimoStepFinishPart struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
	Tokens struct {
		Total     int64 `json:"total"`
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Write int64 `json:"write"`
			Read  int64 `json:"read"`
		} `json:"cache"`
	} `json:"tokens"`
	Cost float64 `json:"cost"`
}

// parseMimoStream consumes a complete `mimo run --format json` run and
// extracts the eval-relevant fields. It requires at least one step_finish:
// a stream without one is a truncated run and returns an error. Mapping onto
// the eval shapes: step_finish tokens summed → TokenUsage (reasoning folded
// into output — thinking bills as output tokens in Anthropic-style usage,
// which the other adapters' totals also include) and CostUSD; step_finish
// count → NumTurns, last reason → StopReason; tool_use (deduped by callID)
// → ToolCalls; the final message's text → Response. Non-JSON stdout lines
// and unknown event types are open-set: kept as warnings / ignored, never
// rejected.
func parseMimoStream(r io.Reader) (*mimoStreamRun, error) {
	run := &mimoStreamRun{}
	toolIndex := map[string]int{}
	type textPart struct{ msg, text string }
	var texts []textPart

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // tool outputs can be large

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev mimoWireEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			// mimo occasionally mixes non-JSON prose into stdout — keep the
			// first few lines as warnings instead of failing the turn.
			if len(run.Warnings) < 5 {
				run.Warnings = append(run.Warnings, line)
			}
			continue
		}
		if run.SessionID == "" && ev.SessionID != "" {
			run.SessionID = ev.SessionID
		}

		switch ev.Type {
		case "text":
			var p mimoTextPart
			if err := json.Unmarshal(ev.Part, &p); err != nil {
				return nil, fmt.Errorf("mimo text event: %w", err)
			}
			texts = append(texts, textPart{msg: p.MessageID, text: p.Text})

		case "tool_use":
			var p mimoToolPart
			if err := json.Unmarshal(ev.Part, &p); err != nil {
				return nil, fmt.Errorf("mimo tool_use event: %w", err)
			}
			if p.CallID == "" {
				continue // not a tool call record (defensive: open-set part shape)
			}
			args, err := normalizeJSON(p.State.Input)
			if err != nil {
				return nil, fmt.Errorf("mimo tool %s input: %w", p.Tool, err)
			}
			result := rawText(p.State.Output)
			if idx, ok := toolIndex[p.CallID]; ok {
				// Re-emission of an already-recorded call: refresh the entry
				// (last emission wins) instead of double-counting it.
				run.ToolCalls[idx].Result = result
				continue
			}
			toolIndex[p.CallID] = len(run.ToolCalls)
			run.ToolCalls = append(run.ToolCalls, ToolCall{Name: p.Tool, Args: args, Result: result})

		case "step_finish":
			var p mimoStepFinishPart
			if err := json.Unmarshal(ev.Part, &p); err != nil {
				return nil, fmt.Errorf("mimo step_finish event: %w", err)
			}
			run.NumTurns++
			run.StopReason = p.Reason
			run.Tokens.InputTokens += p.Tokens.Input
			run.Tokens.OutputTokens += p.Tokens.Output + p.Tokens.Reasoning
			run.Tokens.CacheRead += p.Tokens.Cache.Read
			run.Tokens.CacheCreate += p.Tokens.Cache.Write
			run.CostUSD += p.Cost

		case "error":
			run.ErrorMsg = mimoErrorMessage(ev.Part)

		default:
			// open-set: step_start and any future event types carry nothing
			// the eval report needs — ignored, never rejected.
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if run.NumTurns == 0 {
		return nil, fmt.Errorf("mimo run truncated: no step_finish event")
	}

	// Response is the final assistant message's text — narration emitted
	// before tool calls lives in earlier messages and must not leak into
	// the judged response. Fallback: every text block.
	if len(texts) > 0 {
		lastMsg := texts[len(texts)-1].msg
		var final, all strings.Builder
		for _, t := range texts {
			all.WriteString(t.text)
			if t.msg == lastMsg {
				final.WriteString(t.text)
			}
		}
		run.Text = all.String()
		run.Response = final.String()
		if run.Response == "" {
			run.Response = run.Text
		}
	}
	return run, nil
}

// mimoErrorMessage renders an `error` event's part as text — free-form on
// the wire (message/text/error keys may be absent or structured), so fall
// back to the raw payload rather than losing the failure.
func mimoErrorMessage(part json.RawMessage) string {
	if len(part) == 0 {
		return "unknown error event"
	}
	var probe struct {
		Message string `json:"message"`
		Text    string `json:"text"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(part, &probe); err == nil {
		if probe.Message != "" {
			return probe.Message
		}
		if probe.Text != "" {
			return probe.Text
		}
		if probe.Error != "" {
			return probe.Error
		}
	}
	return string(part)
}

// tailBuffer keeps the last max bytes written to it — mimo's stderr is
// captured for error diagnostics without letting a chatty run balloon
// memory.
type tailBuffer struct {
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if t.max <= 0 || n == 0 {
		return n, nil
	}
	if n >= t.max {
		t.buf.Reset()
		t.buf.Write(p[n-t.max:])
		return n, nil
	}
	if overflow := t.buf.Len() + n - t.max; overflow > 0 {
		kept := append([]byte(nil), t.buf.Bytes()[overflow:]...)
		t.buf.Reset()
		t.buf.Write(kept)
	}
	t.buf.Write(p)
	return n, nil
}

func (t *tailBuffer) String() string { return t.buf.String() }

// --- AgentAdapter ---

func (a *mimoAdapter) Name() string { return "mimo-code" }

// SetArgs splits mimo-code's pseudo-flags (--mimo-bin, --no-yolo — adapter
// behavior, never forwarded to mimo) from the extra CLI flags forwarded
// verbatim onto the `mimo run` command.
func (a *mimoAdapter) SetArgs(args []string) {
	var forwarded []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--mimo-bin":
			if i+1 < len(args) {
				i++
				a.binArg = args[i]
			}
		case "--no-yolo":
			a.noYolo = true
		default:
			forwarded = append(forwarded, args[i])
		}
	}
	a.extraArgs = forwarded
}

func (a *mimoAdapter) Start(ctx context.Context, workdir string) error {
	a.workdir = workdir
	// Fresh mimo session per scenario: the harness reuses this instance
	// across scenarios, each in its own temp workdir.
	a.sessionID = ""
	if a.mimoBin == "" {
		bin := expandHome(a.binArg)
		if bin == "" {
			resolved, err := mimoBinary()
			if err != nil {
				return fmt.Errorf("resolving mimo binary: %w", err)
			}
			bin = resolved
		}
		a.mimoBin = bin
	}
	if _, err := os.Stat(a.mimoBin); err != nil {
		return fmt.Errorf("mimo binary %s: %w", a.mimoBin, err)
	}
	return nil
}

// RunPrompt runs one batch turn: `mimo run --format json [--yolo] [-s
// sessionID] -- <prompt>` in the scenario workdir. stdout is the event
// stream (consumed via parseMimoStream); stderr is captured (capped) so a
// failed run reports what mimo complained about. The run's sessionID is
// stashed so the next turn resumes the same conversation.
func (a *mimoAdapter) RunPrompt(ctx context.Context, prompt string) (RunResult, error) {
	start := time.Now()

	beforeFiles := snapshotDir(a.workdir)

	args := []string{"run", "--format", "json", "--dir", a.workdir}
	if !a.noYolo {
		args = append(args, "--yolo") // batch runs have no one to answer a permission ask
	}
	if a.sessionID != "" {
		args = append(args, "-s", a.sessionID)
	}
	args = append(args, a.extraArgs...)
	args = append(args, "--", prompt) // `--` guards prompts starting with "-"

	cmd := exec.CommandContext(ctx, a.mimoBin, args...)
	cmd.Dir = a.workdir
	stderr := &tailBuffer{max: 8192}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return RunResult{}, fmt.Errorf("stdout pipe: %w", err)
	}
	a.cmd = cmd
	defer func() { a.cmd = nil }()

	if err := cmd.Start(); err != nil {
		return RunResult{}, fmt.Errorf("starting mimo batch run: %w", err)
	}

	// Consume the stream while the process runs (one JSON value per line).
	run, parseErr := parseMimoStream(stdout)
	waitErr := cmd.Wait()
	if parseErr != nil {
		return RunResult{}, fmt.Errorf("parsing mimo run output: %w", parseErr)
	}
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return RunResult{}, fmt.Errorf("mimo run: %w (stderr: %s)", waitErr, msg)
		}
		return RunResult{}, fmt.Errorf("mimo run: %w", waitErr)
	}

	afterFiles := snapshotDir(a.workdir)
	if run.SessionID != "" {
		a.sessionID = run.SessionID
	}

	res := RunResult{
		Response:    run.Response,
		Tokens:      run.Tokens,
		CostUSD:     run.CostUSD,
		Duration:    time.Since(start),
		ToolCalls:   run.ToolCalls,
		FileChanges: diffSnapshots(beforeFiles, afterFiles),
		NumTurns:    run.NumTurns,
		StopReason:  run.StopReason,
		Warnings:    run.Warnings,
	}
	if run.ErrorMsg != "" {
		res.Error = fmt.Errorf("mimo run failed: %s", run.ErrorMsg)
	}
	return res, nil
}

// Stop kills an in-flight batch run, if any. No state root to tear down —
// mimo-code, like claude-code, runs against the live mimo state (sessions
// accumulate under ~/.mimocode; each scenario still starts a fresh session
// because Start resets the resume key).
func (a *mimoAdapter) Stop() error {
	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
	}
	return nil
}
