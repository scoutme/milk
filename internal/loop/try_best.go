package loop

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"
)

// TryBestMonitor detects when the agent is stuck repeating near-identical
// tool-level actions: similar edits to the same file, retried failing bash
// commands, or non-progressing action streaks. It mirrors MiMoCode's
// try-best detector but operates at the milk tool-call/result boundary.
type TryBestMonitor struct {
	mu sync.Mutex

	// Edit tracking: recent edit_file/write_file calls with their target path
	// and normalised diff text.
	edits      []editRecord
	editWindow int // max records to keep (default tryBestEditWindow)

	// Bash tracking: recent bash commands with their exit status.
	bashCalls      []bashRecord
	bashWindow     int // max records to keep
	bashRetryCount int // consecutive retries of the same failing command

	// Action streak: consecutive non-progressing actions of the same kind.
	actionKind      string // "edit" or "verify"
	actionStreak    int
	actionMaxStreak int // threshold (default tryBestActionStreak)

	// Firing state: once a signal fires, don't fire again until reset.
	fired bool
}

type editRecord struct {
	path   string // target file path
	norm   string // normalised diff text (for Jaccard comparison)
	result string // tool result (empty = success, non-empty = error)
}

type bashRecord struct {
	command string // normalised command
	exitOK  bool   // true if exit code 0
	result  string // first 200 chars of output
}

const (
	tryBestEditWindow    = 12
	tryBestEditMatches   = 2   // how many prior edits must be similar
	tryBestEditThreshold = 0.8 // Jaccard similarity threshold
	tryBestBashRetries   = 3   // same failing command retried N times
	tryBestActionStreak  = 4   // consecutive non-progressing actions
	tryBestMinDiffLen    = 20  // ignore trivially short diffs
)

// TryBestSignal identifies which try-best sub-detector fired.
type TryBestSignal int

const (
	TryBestEditRepeat TryBestSignal = iota
	TryBestBashRetry
	TryBestActionStreak
)

func (s TryBestSignal) String() string {
	switch s {
	case TryBestEditRepeat:
		return "edit_repeat"
	case TryBestBashRetry:
		return "bash_retry"
	case TryBestActionStreak:
		return "action_streak"
	default:
		return "unknown"
	}
}

// TryBestVerdict is emitted when a try-best signal fires.
type TryBestVerdict struct {
	Signal   TryBestSignal
	Message  string
	FilePath string // target file for edit_repeat, empty otherwise
}

// NewTryBestMonitor creates a monitor with default thresholds.
func NewTryBestMonitor() *TryBestMonitor {
	return &TryBestMonitor{
		editWindow:      tryBestEditWindow,
		bashWindow:      tryBestBashRetries + 1,
		actionMaxStreak: tryBestActionStreak,
	}
}

// Reset clears all tracking state (e.g. on session switch).
func (m *TryBestMonitor) Reset() {
	m.mu.Lock()
	m.edits = nil
	m.bashCalls = nil
	m.bashRetryCount = 0
	m.actionKind = ""
	m.actionStreak = 0
	m.fired = false
	m.mu.Unlock()
}

// RecordToolCall records a tool call and its result. Returns a verdict if a
// try-best signal fires, nil otherwise. Call this after each tool completes.
//
// toolName is the tool function name (e.g. "edit_file", "bash").
// argsJSON is the raw JSON arguments string.
// result is the tool result text (empty for success, error message for failure).
// exitOK is true when the tool succeeded (for bash: exit code 0).
func (m *TryBestMonitor) RecordToolCall(toolName, argsJSON, result string, exitOK bool) *TryBestVerdict {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.fired {
		return nil
	}

	switch toolName {
	case "edit_file", "write_file":
		return m.recordEdit(toolName, argsJSON, result, exitOK)
	case "bash":
		return m.recordBash(argsJSON, result, exitOK)
	default:
		// Other tools: track action streak but not content similarity.
		m.recordAction(toolName, exitOK)
		return nil
	}
}

func (m *TryBestMonitor) recordEdit(toolName, argsJSON, result string, exitOK bool) *TryBestVerdict {
	path := extractFilePath(toolName, argsJSON)
	norm := normaliseDiff(toolName, argsJSON)

	rec := editRecord{path: path, norm: norm, result: result}
	m.edits = append(m.edits, rec)
	if len(m.edits) > m.editWindow {
		m.edits = m.edits[len(m.edits)-m.editWindow:]
	}

	// Update action streak.
	m.recordAction("edit", exitOK)

	// Check for near-identical edits to the same file.
	if !exitOK || len(norm) < tryBestMinDiffLen {
		return nil
	}

	similarCount := 0
	for _, prev := range m.edits[:len(m.edits)-1] {
		if prev.path != path || len(prev.norm) < tryBestMinDiffLen || prev.result != "" {
			continue
		}
		sim := jaccardShingles(prev.norm, norm, 3)
		if sim >= tryBestEditThreshold {
			similarCount++
		}
	}

	if similarCount >= tryBestEditMatches {
		m.fired = true
		v := &TryBestVerdict{
			Signal:   TryBestEditRepeat,
			Message:  fmt.Sprintf("near-identical edits to %s (%d similar prior edits)", path, similarCount),
			FilePath: path,
		}
		slog.Default().Warn("try_best: SIGNAL FIRED", "signal", v.Signal, "message", v.Message)
		return v
	}
	return nil
}

func (m *TryBestMonitor) recordBash(argsJSON, result string, exitOK bool) *TryBestVerdict {
	cmd := normaliseBashCommand(argsJSON)

	rec := bashRecord{command: cmd, exitOK: exitOK, result: truncateStr(result, 200)}
	m.bashCalls = append(m.bashCalls, rec)
	if len(m.bashCalls) > m.bashWindow {
		m.bashCalls = m.bashCalls[len(m.bashCalls)-m.bashWindow:]
	}

	// Update action streak.
	m.recordAction("bash", exitOK)

	// Track consecutive retries of the same failing command.
	// bashRetryCount counts retries AFTER the first failure (so 3 retries
	// means 4 total calls: 1 initial + 3 retries).
	if !exitOK && cmd != "" && len(m.bashCalls) >= 2 {
		prev := m.bashCalls[len(m.bashCalls)-2]
		if !prev.exitOK && prev.command == cmd {
			m.bashRetryCount++
		} else {
			m.bashRetryCount = 1 // first failure of a new command counts as retry 1
		}
	} else if !exitOK && cmd != "" {
		m.bashRetryCount = 1
	} else {
		m.bashRetryCount = 0
	}

	if m.bashRetryCount >= tryBestBashRetries {
		m.fired = true
		v := &TryBestVerdict{
			Signal:  TryBestBashRetry,
			Message: fmt.Sprintf("bash command retried %d times without success: %s", m.bashRetryCount+1, truncateStr(cmd, 80)),
		}
		slog.Default().Warn("try_best: SIGNAL FIRED", "signal", v.Signal, "message", v.Message)
		return v
	}

	return nil
}

func (m *TryBestMonitor) recordAction(kind string, success bool) {
	if success {
		// A successful action breaks the non-progressing streak.
		m.actionKind = kind
		m.actionStreak = 0
		return
	}
	if kind == m.actionKind {
		m.actionStreak++
	} else {
		m.actionKind = kind
		m.actionStreak = 1
	}
}

// CheckActionStreak returns a verdict if the non-progressing action streak
// exceeds the threshold. Call this after processing all tool results for a
// turn (not per-tool, since the streak spans the whole turn).
func (m *TryBestMonitor) CheckActionStreak() *TryBestVerdict {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.fired {
		return nil
	}
	if m.actionStreak >= m.actionMaxStreak {
		m.fired = true
		v := &TryBestVerdict{
			Signal:  TryBestActionStreak,
			Message: fmt.Sprintf("%d consecutive non-progressing %s actions", m.actionStreak, m.actionKind),
		}
		slog.Default().Warn("try_best: SIGNAL FIRED", "signal", v.Signal, "message", v.Message)
		return v
	}
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// extractFilePath pulls the file path from edit_file/write_file arguments.
func extractFilePath(toolName, argsJSON string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ""
	}
	if p, ok := args["path"].(string); ok {
		return p
	}
	if p, ok := args["file_path"].(string); ok {
		return p
	}
	return ""
}

// normaliseDiff produces a comparable string from edit/write tool arguments.
// For edit_file: the new_text field. For write_file: the content field.
func normaliseDiff(toolName, argsJSON string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ""
	}
	var text string
	switch toolName {
	case "edit_file":
		if t, ok := args["new_text"].(string); ok {
			text = t
		} else if t, ok := args["new_string"].(string); ok {
			text = t
		}
	case "write_file":
		if t, ok := args["content"].(string); ok {
			text = t
		}
	}
	return normaliseText(text)
}

// normaliseBashCommand extracts and normalises the command from bash arguments.
// Strips tmp paths, large numbers, and seeds to reduce false negatives.
func normaliseBashCommand(argsJSON string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ""
	}
	cmd, _ := args["command"].(string)
	if cmd == "" {
		return ""
	}
	return normaliseText(cmd)
}

// normaliseText lowercases, collapses whitespace, and truncates.
func normaliseText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace {
				b.WriteRune(' ')
				prevSpace = true
			}
		} else {
			b.WriteRune(r)
			prevSpace = false
		}
	}
	return b.String()
}

// jaccardShingles computes Jaccard similarity on character-level shingles of
// size n. Returns 1.0 for identical strings, 0.0 for completely different.
func jaccardShingles(a, b string, n int) float64 {
	if a == b {
		return 1.0
	}
	aS := shingles(a, n)
	bS := shingles(b, n)
	if len(aS) == 0 && len(bS) == 0 {
		return 1.0
	}
	intersection := 0
	for s := range aS {
		if bS[s] {
			intersection++
		}
	}
	union := len(aS) + len(bS) - intersection
	if union == 0 {
		return 1.0
	}
	return float64(intersection) / float64(union)
}

func shingles(s string, n int) map[string]bool {
	runes := []rune(s)
	if len(runes) < n {
		return map[string]bool{s: true}
	}
	m := make(map[string]bool, len(runes)-n+1)
	for i := 0; i <= len(runes)-n; i++ {
		m[string(runes[i:i+n])] = true
	}
	return m
}

func truncateStr(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "…"
}

// Similarity returns the Jaccard shingle similarity between two strings.
// Exported for testing.
func Similarity(a, b string) float64 {
	return jaccardShingles(normaliseText(a), normaliseText(b), 3)
}

// ── Integration with loop.Detector ──────────────────────────────────────────

// FeedTryBest converts a TryBestVerdict into a loop.Verdict for unified
// handling in the TUI.
func FeedTryBest(v *TryBestVerdict) Verdict {
	var signal Signal
	confidence := 0.9
	shouldInterrupt := true

	switch v.Signal {
	case TryBestEditRepeat:
		signal = Signal(100) // custom signal ID, not in the iota enum
	case TryBestBashRetry:
		signal = Signal(101)
	case TryBestActionStreak:
		signal = Signal(102)
		confidence = 0.85
	}

	return Verdict{
		Signal:          signal,
		Confidence:      confidence,
		Message:         fmt.Sprintf("try-best: %s", v.Message),
		ShouldInterrupt: shouldInterrupt,
	}
}

// TryBestSignalName returns the signal name for display.
func TryBestSignalName(v *Verdict) string {
	switch v.Signal {
	case Signal(100):
		return "try_best_edit_repeat"
	case Signal(101):
		return "try_best_bash_retry"
	case Signal(102):
		return "try_best_action_streak"
	default:
		return "unknown"
	}
}

// ── Diff similarity for testing ─────────────────────────────────────────────

// EditSimilarity computes the similarity between two edit diffs for testing.
func EditSimilarity(diffA, diffB string) float64 {
	return jaccardShingles(normaliseText(diffA), normaliseText(diffB), 3)
}

// CountSimilarEdits counts how many prior edits in the window are similar to
// the given diff for the given file path. Exported for testing.
func CountSimilarEdits(edits []struct{ Path, Diff string }, path, diff string, threshold float64) int {
	norm := normaliseText(diff)
	count := 0
	for _, e := range edits {
		if e.Path != path {
			continue
		}
		if jaccardShingles(normaliseText(e.Diff), norm, 3) >= threshold {
			count++
		}
	}
	return count
}
