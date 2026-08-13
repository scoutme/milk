package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

func init() {
	Register("claude-code", func() AgentAdapter { return &claudeAdapter{} })
}

// claudeAdapter implements AgentAdapter by driving the Claude Code CLI inside a tmux session.
type claudeAdapter struct {
	sessionName    string   // tmux session name
	workdir        string   // working directory passed to Start
	sessionID      string   // UUID for --session-id
	transcriptPath string   // ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl
	extraArgs      []string // extra CLI args from --agents spec
}

// --- JSON types for parsing Claude Code transcript JSONL ---

type claudeJSONLLine struct {
	Type    string        `json:"type"`
	Message claudeMessage `json:"message"`
}

type claudeMessage struct {
	Role       string               `json:"role"`
	Content    []claudeContentBlock `json:"content"`
	Usage      claudeUsage          `json:"usage"`
	StopReason string               `json:"stop_reason"`
}

type claudeContentBlock struct {
	Type     string          `json:"type"` // "text", "thinking", "tool_use"
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func (a *claudeAdapter) Name() string         { return "claude-code" }
func (a *claudeAdapter) SetArgs(args []string) { a.extraArgs = args }

func (a *claudeAdapter) Start(ctx context.Context, workdir string) error {
	a.workdir = workdir
	a.sessionID = uuid.New().String()
	a.sessionName = "claude-eval-" + a.sessionID[:8]

	// Compute the transcript path.
	// Encode workdir: replace "/" with "-", which produces a leading "-" that is kept.
	encodedCwd := strings.ReplaceAll(workdir, "/", "-")
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home dir: %w", err)
	}
	a.transcriptPath = filepath.Join(home, ".claude", "projects", encodedCwd, a.sessionID+".jsonl")

	// Create tmux session.
	if err := tmuxNewSession(a.sessionName, 200, 50); err != nil {
		return fmt.Errorf("tmux new-session: %w", err)
	}

	// Launch Claude Code with the deterministic session ID.
	cmd := fmt.Sprintf("cd %s && claude --session-id %s", workdir, a.sessionID)
	if len(a.extraArgs) > 0 {
		cmd += " " + strings.Join(a.extraArgs, " ")
	}
	if err := tmuxSendKeys(a.sessionName, cmd); err != nil {
		return fmt.Errorf("tmux send-keys: %w", err)
	}
	if err := tmuxSendEnter(a.sessionName); err != nil {
		return fmt.Errorf("tmux send-enter: %w", err)
	}

	// Wait for Claude Code ready: poll tmux pane for the "❯" prompt.
	pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := pollPaneContains(pollCtx, a.sessionName, "❯", 500*time.Millisecond); err != nil {
		return fmt.Errorf("waiting for Claude Code ready: %w", err)
	}

	return nil
}

func (a *claudeAdapter) RunPrompt(ctx context.Context, prompt string) (RunResult, error) {
	start := time.Now()

	// 1. Snapshot current transcript file size.
	prevSize, err := fileSize(a.transcriptPath)
	if err != nil {
		return RunResult{}, fmt.Errorf("stat transcript: %w", err)
	}

	// 2. Snapshot workdir files before the turn.
	beforeFiles := snapshotDir(a.workdir)

	// 3. Send prompt via tmux.
	if err := tmuxSendKeys(a.sessionName, prompt); err != nil {
		return RunResult{}, fmt.Errorf("tmux send-keys: %w", err)
	}
	if err := tmuxSendEnter(a.sessionName); err != nil {
		return RunResult{}, fmt.Errorf("tmux send-enter: %w", err)
	}

	// 4. Wait for end_turn, collecting all new lines from the transcript.
	turnCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	turnLines, err := a.waitForTurn(turnCtx, prevSize)
	if err != nil {
		return RunResult{}, fmt.Errorf("waiting for turn: %w", err)
	}

	// 5. Extract tokens (sum all assistant messages in the turn).
	tokens := sumTurnTokens(turnLines)

	// 6. Extract response text.
	response := extractResponse(turnLines)

	// 7. Extract tool calls.
	toolCalls := extractToolCalls(turnLines)

	// 8. Diff workdir files after the turn.
	afterFiles := snapshotDir(a.workdir)
	fileChanges := diffSnapshots(beforeFiles, afterFiles)

	return RunResult{
		Response:    response,
		Tokens:      tokens,
		Duration:    time.Since(start),
		ToolCalls:   toolCalls,
		FileChanges: fileChanges,
	}, nil
}

func (a *claudeAdapter) Stop() error {
	return tmuxKillSession(a.sessionName)
}

// waitForTurn polls the transcript file from prevSize until it finds an assistant
// message with stop_reason "end_turn". It collects all new JSONL lines between
// prevSize and the end_turn line (inclusive) for later extraction.
func (a *claudeAdapter) waitForTurn(ctx context.Context, prevSize int64) ([]claudeJSONLLine, error) {
	for {
		lines, done, err := a.readNewLines(prevSize)
		if err != nil {
			// File may not exist yet; retry.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		if done {
			return lines, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// readNewLines reads JSONL lines starting from offset. Returns the lines read so far,
// whether an end_turn was found, and any error.
func (a *claudeAdapter) readNewLines(offset int64) ([]claudeJSONLLine, bool, error) {
	f, err := os.Open(a.transcriptPath)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, false, err
	}

	var lines []claudeJSONLLine
	scanner := bufio.NewScanner(f)
	// Increase buffer for large JSONL lines (thinking blocks can be big).
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		var line claudeJSONLLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue // skip malformed lines
		}
		lines = append(lines, line)
		if line.Type == "assistant" && line.Message.StopReason == "end_turn" {
			return lines, true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}
	return lines, false, nil
}

// sumTurnTokens sums token usage from all assistant messages in a turn.
func sumTurnTokens(lines []claudeJSONLLine) TokenUsage {
	var total TokenUsage
	for _, l := range lines {
		if l.Type == "assistant" {
			total.InputTokens += l.Message.Usage.InputTokens
			total.OutputTokens += l.Message.Usage.OutputTokens
			total.CacheCreate += l.Message.Usage.CacheCreationInputTokens
			total.CacheRead += l.Message.Usage.CacheReadInputTokens
		}
	}
	return total
}

// extractResponse concatenates all text content blocks from assistant messages.
func extractResponse(lines []claudeJSONLLine) string {
	var sb strings.Builder
	for _, l := range lines {
		if l.Type != "assistant" {
			continue
		}
		for _, block := range l.Message.Content {
			if block.Type == "text" {
				sb.WriteString(block.Text)
			}
		}
	}
	return sb.String()
}

// extractToolCalls collects all tool_use content blocks from assistant messages.
func extractToolCalls(lines []claudeJSONLLine) []ToolCall {
	var calls []ToolCall
	for _, l := range lines {
		if l.Type != "assistant" {
			continue
		}
		for _, block := range l.Message.Content {
			if block.Type == "tool_use" {
				calls = append(calls, ToolCall{
					Name: block.Name,
					Args: string(block.Input),
				})
			}
		}
	}
	return calls
}

// fileSize returns the size of a file, or 0 if the file does not exist.
func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
