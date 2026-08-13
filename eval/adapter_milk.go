package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

func init() {
	Register("milk-tui", func() AgentAdapter { return &milkAdapter{} })
}

// milkAdapter implements AgentAdapter by driving the milk TUI inside a tmux session.
type milkAdapter struct {
	sessionName string // tmux session name
	workdir     string // working directory passed to Start
	sessionPath string // path to ~/.milk/sessions/<uuid>.json
	sessionID   string // the milk session UUID
}

// --- JSON types for parsing milk session files ---

type milkIndexEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	LastUsed string `json:"last_used"`
}

type milkSessionFile struct {
	ID      string                     `json:"id"`
	CWD     string                     `json:"cwd"`
	State   string                     `json:"state"`
	History []milkTurn                 `json:"history"`
	Tokens  map[string]*milkTokenUsage `json:"tokens,omitempty"`
}

type milkTurn struct {
	Role      string         `json:"role"`
	Agent     string         `json:"agent,omitempty"`
	Content   string         `json:"content"`
	ToolCalls []milkToolCall `json:"tool_calls,omitempty"`
}

type milkToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type milkTokenUsage struct {
	Model         string `json:"model"`
	Agent         string `json:"agent"`
	Prompt        int64  `json:"prompt"`
	Completion    int64  `json:"completion"`
	CacheRead     int64  `json:"cache_read,omitempty"`
	CacheCreation int64  `json:"cache_creation,omitempty"`
}

func (a *milkAdapter) Name() string { return "milk-tui" }

func (a *milkAdapter) Start(ctx context.Context, workdir string) error {
	a.workdir = workdir
	a.sessionName = "milk-eval-" + uuid.New().String()[:8]

	// Create tmux session.
	if err := tmuxNewSession(a.sessionName, 200, 50); err != nil {
		return fmt.Errorf("tmux new-session: %w", err)
	}

	// Launch milk binary directly from the scenario workdir.
	// We don't use "task run" because the workdir is a temp directory with
	// scenario files, not the milk project root. The binary is pre-built.
	home, _ := os.UserHomeDir()
	milkBin := filepath.Join(home, ".local", "bin", "milk")
	if err := tmuxSendKeys(a.sessionName, "cd "+workdir+" && "+milkBin); err != nil {
		return fmt.Errorf("tmux send-keys: %w", err)
	}
	if err := tmuxSendEnter(a.sessionName); err != nil {
		return fmt.Errorf("tmux send-enter: %w", err)
	}

	// Wait for TUI ready: the milk prompt indicator "❯".
	pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := pollPaneContains(pollCtx, a.sessionName, "❯", 500*time.Millisecond); err != nil {
		return fmt.Errorf("waiting for milk TUI ready: %w", err)
	}

	// Discover session file from the index.
	if err := a.discoverSession(workdir); err != nil {
		return fmt.Errorf("session discovery: %w", err)
	}

	return nil
}

// discoverSession reads ~/.milk/sessions/index.json to find the session for workdir.
func (a *milkAdapter) discoverSession(workdir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	indexPath := filepath.Join(home, ".milk", "sessions", "index.json")

	// Retry a few times — the session file may not be written yet.
	var idx map[string][]milkIndexEntry
	for attempt := 0; attempt < 10; attempt++ {
		data, err := os.ReadFile(indexPath)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err := json.Unmarshal(data, &idx); err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		entries, ok := idx[workdir]
		if !ok || len(entries) == 0 {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		// Most recent entry (first in list, sorted by last_used desc).
		a.sessionID = entries[0].ID
		a.sessionPath = filepath.Join(home, ".milk", "sessions", entries[0].ID+".json")
		return nil
	}
	return fmt.Errorf("no session found for workdir %s in index after retries", workdir)
}

func (a *milkAdapter) RunPrompt(ctx context.Context, prompt string) (RunResult, error) {
	start := time.Now()

	// 1. Load current session and snapshot history length + tokens.
	beforeSess, err := a.loadSession()
	if err != nil {
		return RunResult{}, fmt.Errorf("loading session before prompt: %w", err)
	}
	prevHistoryLen := len(beforeSess.History)
	beforeTokens := snapshotTokens(beforeSess)

	// 2. Snapshot workdir files before the turn.
	beforeFiles := snapshotDir(a.workdir)

	// 3. Send prompt via tmux.
	if err := tmuxSendKeys(a.sessionName, prompt); err != nil {
		return RunResult{}, fmt.Errorf("tmux send-keys: %w", err)
	}
	if err := tmuxSendEnter(a.sessionName); err != nil {
		return RunResult{}, fmt.Errorf("tmux send-enter: %w", err)
	}

	// 4. Wait for turn completion.
	turnCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	afterSess, err := a.waitForTurn(turnCtx, prevHistoryLen)
	if err != nil {
		return RunResult{}, fmt.Errorf("waiting for turn: %w", err)
	}

	// 5. Extract response from last assistant turn.
	response := ""
	for i := len(afterSess.History) - 1; i >= 0; i-- {
		if afterSess.History[i].Role == "assistant" {
			response = afterSess.History[i].Content
			break
		}
	}

	// 6. Diff workdir files after the turn.
	afterFiles := snapshotDir(a.workdir)
	fileChanges := diffSnapshots(beforeFiles, afterFiles)

	// 7. Diff tokens.
	afterTokens := snapshotTokens(afterSess)
	turnTokens := TokenUsage{
		InputTokens:  afterTokens.InputTokens - beforeTokens.InputTokens,
		OutputTokens: afterTokens.OutputTokens - beforeTokens.OutputTokens,
		CacheRead:    afterTokens.CacheRead - beforeTokens.CacheRead,
		CacheCreate:  afterTokens.CacheCreate - beforeTokens.CacheCreate,
	}

	// 8. Extract tool calls from new turns.
	var toolCalls []ToolCall
	for _, t := range afterSess.History[prevHistoryLen:] {
		for _, tc := range t.ToolCalls {
			toolCalls = append(toolCalls, ToolCall{Name: tc.Name, Args: tc.Arguments})
		}
	}

	return RunResult{
		Response:    response,
		Tokens:      turnTokens,
		Duration:    time.Since(start),
		ToolCalls:   toolCalls,
		FileChanges: fileChanges,
	}, nil
}

func (a *milkAdapter) Stop() error {
	return tmuxKillSession(a.sessionName)
}

// loadSession reads and parses the session JSON file.
func (a *milkAdapter) loadSession() (*milkSessionFile, error) {
	data, err := os.ReadFile(a.sessionPath)
	if err != nil {
		return nil, err
	}
	var sess milkSessionFile
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

// waitForTurn polls the session file until the turn completes.
// A turn is complete when state=="ROUTING", history grew, and the last turn is assistant.
func (a *milkAdapter) waitForTurn(ctx context.Context, prevHistoryLen int) (*milkSessionFile, error) {
	for {
		sess, err := a.loadSession()
		if err == nil {
			// Turn is complete when state returned to ROUTING and history grew.
			// We don't require last.Role == "assistant" because milk skips the
			// assistant turn when the response is empty (e.g. self-escalation,
			// tool-only turns, or empty completions).
			if sess.State == "ROUTING" && len(sess.History) > prevHistoryLen {
				return sess, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// snapshotTokens sums all cumulative token counters across all model/role keys.
func snapshotTokens(sess *milkSessionFile) TokenUsage {
	var total TokenUsage
	for _, tu := range sess.Tokens {
		total.InputTokens += tu.Prompt
		total.OutputTokens += tu.Completion
		total.CacheRead += tu.CacheRead
		total.CacheCreate += tu.CacheCreation
	}
	return total
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
