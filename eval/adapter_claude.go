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

	"github.com/google/uuid"

	"github.com/scoutme/milk/internal/config"
)

func init() {
	Register("claude-code", func() AgentAdapter { return &claudeAdapter{} })
}

// claudeAdapter implements AgentAdapter by driving the Claude Code CLI inside a tmux session.
// Claude Code writes a per-session transcript at a deterministic path derived from the
// working directory and --session-id, as long as the workdir is its own git repository
// (otherwise it falls back to the enclosing repo's project directory/transcript).
type claudeAdapter struct {
	sessionName      string   // tmux session name
	workdir          string   // working directory passed to Start
	sessionID        string   // UUID for --session-id
	transcriptPath   string   // ~/.claude/projects/<encoded-workdir>/<sessionID>.jsonl
	extraArgs        []string // extra CLI args from --agents spec
	cacheCooldown    time.Duration
	cacheCooldownErr error
}

// claudeActivityStatePath returns the file used to persist when a claude-code
// turn last completed. Persisted to disk (rather than kept in memory) because
// the cooldown needs to compare across separate `milk eval run` invocations,
// each a fresh process — in-memory state alone only works within one run.
// Overridden in tests to avoid touching the real ~/.milk directory.
var claudeActivityStatePath = func() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "eval-claude-cache-activity"), nil
}

// readClaudeLastActivity returns the persisted last-activity time, or the
// zero Time if none is recorded yet (never run, or the state file is
// unreadable) — treated as "long enough ago" by waitForCacheCooldown.
func readClaudeLastActivity() time.Time {
	path, err := claudeActivityStatePath()
	if err != nil {
		return time.Time{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}
	}
	return t
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

func (a *claudeAdapter) Name() string { return "claude-code" }

// SetArgs consumes "--cache-cooldown <duration>" for this adapter's own use
// (see waitForCacheCooldown) and forwards everything else to the claude CLI
// launch command unchanged. Usage: --agents "claude-code[--cache-cooldown,5m]".
func (a *claudeAdapter) SetArgs(args []string) {
	var remaining []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--cache-cooldown" {
			if i+1 >= len(args) {
				a.cacheCooldownErr = fmt.Errorf("--cache-cooldown requires a duration value (e.g. 5m)")
				break
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				a.cacheCooldownErr = fmt.Errorf("invalid --cache-cooldown %q: %w", args[i+1], err)
			} else {
				a.cacheCooldown = d
			}
			i++ // skip the value
			continue
		}
		remaining = append(remaining, args[i])
	}
	a.extraArgs = remaining
}

// waitForCacheCooldown blocks, if --cache-cooldown was set, until at least
// that long has elapsed since the last claude-code turn completed — tracked
// via a state file (readClaudeLastActivity) rather than in-memory, so this
// also holds across separate `milk eval run` invocations, not just multiple
// scenarios within one. Anthropic's prompt cache for Claude Code's
// tool-definitions system-prompt block is a prefix match keyed on content,
// not on workdir or session — a new session started less than the cache TTL
// after another session's last turn will read that still-warm cache instead
// of getting a cold measurement. This is opt-in (default: no wait, unchanged
// behavior).
func (a *claudeAdapter) waitForCacheCooldown(ctx context.Context) error {
	if a.cacheCooldown <= 0 {
		return nil
	}
	wait := a.cacheCooldown - time.Since(readClaudeLastActivity())
	if wait <= 0 {
		return nil
	}
	fmt.Fprintf(os.Stderr, "claude-code: waiting %s for the prompt cache to expire (--cache-cooldown %s)\n",
		wait.Round(time.Second), a.cacheCooldown)
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// recordClaudeActivity persists "now" as the last time a claude-code turn was
// attempted, for waitForCacheCooldown to measure against. Best-effort: a
// failure to write just means the next cooldown check undercounts elapsed
// time, not a reason to fail the eval run.
func recordClaudeActivity() {
	path, err := claudeActivityStatePath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(time.Now().Format(time.RFC3339Nano)), 0644)
}

func (a *claudeAdapter) Start(ctx context.Context, workdir string) error {
	if a.cacheCooldownErr != nil {
		return a.cacheCooldownErr
	}
	if err := a.waitForCacheCooldown(ctx); err != nil {
		return fmt.Errorf("waiting for cache cooldown: %w", err)
	}

	a.workdir = workdir
	a.sessionID = uuid.New().String()
	a.sessionName = "claude-eval-" + a.sessionID[:8]

	// Init a git repo so Claude Code treats this as its own project and writes
	// a transcript under this workdir's project directory rather than an
	// enclosing repo's.
	exec.Command("git", "init", workdir).Run()
	exec.Command("git", "-C", workdir, "add", ".").Run()
	exec.Command("git", "-C", workdir, "-c", "user.email=eval@eval", "-c", "user.name=eval",
		"commit", "-m", "init", "--allow-empty").Run()

	// Pre-create project-level settings so Claude Code trusts the workdir.
	// This alone does not skip the workspace-trust dialog (handled below).
	if err := a.setupProjectSettings(workdir); err != nil {
		return fmt.Errorf("setting up project settings: %w", err)
	}

	// The transcript path is deterministic once the workdir is its own git repo.
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home dir: %w", err)
	}
	encodedCwd := strings.ReplaceAll(workdir, "/", "-")
	a.transcriptPath = filepath.Join(home, ".claude", "projects", encodedCwd, a.sessionID+".jsonl")

	// Create tmux session.
	if err := tmuxNewSession(a.sessionName, 200, 50); err != nil {
		return fmt.Errorf("tmux new-session: %w", err)
	}

	// Launch Claude Code.
	cmd := fmt.Sprintf("cd %s && claude --session-id %s --dangerously-skip-permissions", workdir, a.sessionID)
	if len(a.extraArgs) > 0 {
		cmd += " " + strings.Join(a.extraArgs, " ")
	}
	if err := tmuxSendKeys(a.sessionName, cmd); err != nil {
		return fmt.Errorf("tmux send-keys: %w", err)
	}
	if err := tmuxSendEnter(a.sessionName); err != nil {
		return fmt.Errorf("tmux send-enter: %w", err)
	}

	// Wait for Claude Code ready. The first "❯" may be a workspace trust dialog
	// ("Yes, I trust this folder") rather than the main prompt.
	pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := pollPaneContains(pollCtx, a.sessionName, "❯", 500*time.Millisecond); err != nil {
		return fmt.Errorf("waiting for Claude Code ready: %w", err)
	}

	// Handle trust dialog if present (settings.json above is not sufficient
	// to skip it, so we confirm interactively).
	if err := a.confirmTrustDialogIfShown(ctx); err != nil {
		return fmt.Errorf("handling trust dialog: %w", err)
	}

	return nil
}

// confirmTrustDialogIfShown watches for the workspace-trust dialog and
// confirms it if shown. The dialog shares the "❯" glyph as its menu-selection
// marker and can render a second or two after the initial readiness poll
// already succeeded on an earlier frame, so a single check right after that
// poll is a race — checking once here instead of watching for a window let a
// still-rendering dialog slip through Start() undetected, only to swallow the
// first real prompt sent by RunPrompt (its Enter just confirmed the dialog).
func (a *claudeAdapter) confirmTrustDialogIfShown(ctx context.Context) error {
	watchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		pane, _ := tmuxCapturePane(a.sessionName)
		if strings.Contains(pane, "trust this folder") || strings.Contains(pane, "Yes, I trust") {
			if err := tmuxSendEnter(a.sessionName); err != nil {
				return fmt.Errorf("confirming trust dialog: %w", err)
			}
			clearCtx, cancel2 := context.WithTimeout(ctx, 30*time.Second)
			defer cancel2()
			return pollPaneNotContains(clearCtx, a.sessionName, "trust this folder", 300*time.Millisecond)
		}
		select {
		case <-watchCtx.Done():
			return nil // no trust dialog appeared; assume none is coming.
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// setupProjectSettings creates a project-level settings file that pre-trusts the workdir.
func (a *claudeAdapter) setupProjectSettings(workdir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	encodedCwd := strings.ReplaceAll(workdir, "/", "-")
	settingsDir := filepath.Join(home, ".claude", "projects", encodedCwd)
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		return fmt.Errorf("creating settings dir: %w", err)
	}
	settings := map[string]any{
		"permissions": map[string]any{
			"allow": []string{
				"Read(" + workdir + "/**)",
				"Write(" + workdir + "/**)",
				"Bash(" + workdir + "/**)",
				"Bash(**)",
			},
			"additionalDirectories": []string{workdir},
		},
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling settings: %w", err)
	}
	settingsPath := filepath.Join(settingsDir, "settings.json")
	return os.WriteFile(settingsPath, data, 0644)
}

func (a *claudeAdapter) RunPrompt(ctx context.Context, prompt string) (RunResult, error) {
	defer recordClaudeActivity()
	start := time.Now()

	// 1. Snapshot transcript size and workdir files before the turn.
	prevSize, err := fileSize(a.transcriptPath)
	if err != nil {
		return RunResult{}, fmt.Errorf("stat transcript: %w", err)
	}
	beforeFiles := snapshotDir(a.workdir)

	// 2. Send prompt via tmux, retrying if Claude Code's TUI wasn't yet
	// listening for input (see sendPromptUntilAccepted).
	if err := a.sendPromptUntilAccepted(ctx, prompt); err != nil {
		return RunResult{}, err
	}

	// 3. Wait for end_turn, collecting all new lines from the transcript.
	turnCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	turnLines, err := a.waitForTurn(turnCtx, prevSize)
	if err != nil {
		return RunResult{}, fmt.Errorf("waiting for turn: %w", err)
	}

	// 4. Extract tokens (sum all assistant messages in the turn).
	tokens := sumTurnTokens(turnLines)

	// 5. Extract response text.
	response := extractResponse(turnLines)

	// 6. Extract tool calls.
	toolCalls := extractToolCalls(turnLines)

	// 7. Diff workdir files after the turn.
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

// sendPromptUntilAccepted sends prompt via tmux and verifies Claude Code
// actually received it. Claude Code renders its "❯" prompt (the readiness
// marker polled in Start) before it has attached its input handler, so
// keystrokes sent in that window — right after startup, or right after the
// previous turn's response finishes rendering — are silently dropped rather
// than queued. A dropped send leaves the pane pixel-identical to before the
// send (no echoed text, no spinner), which is what we check for here; a
// genuine send always changes the pane within a couple hundred ms.
func (a *claudeAdapter) sendPromptUntilAccepted(ctx context.Context, prompt string) error {
	const maxAttempts = 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		before, err := tmuxCapturePane(a.sessionName)
		if err != nil {
			return fmt.Errorf("tmux capture-pane: %w", err)
		}
		if err := tmuxSendKeys(a.sessionName, prompt); err != nil {
			return fmt.Errorf("tmux send-keys: %w", err)
		}
		if err := tmuxSendEnter(a.sessionName); err != nil {
			return fmt.Errorf("tmux send-enter: %w", err)
		}

		settleCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		accepted := pollPaneChanged(settleCtx, a.sessionName, before, 300*time.Millisecond)
		cancel()
		if accepted {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return fmt.Errorf("prompt not accepted by Claude Code after %d attempts", maxAttempts)
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
			continue // skip malformed lines (e.g. non-message control records)
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
