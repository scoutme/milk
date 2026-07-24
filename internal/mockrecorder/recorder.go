package mockrecorder

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TurnRecord captures one prompt/response exchange for a named mock instance.
type TurnRecord struct {
	Turn           int               `json:"turn"`
	Input          string            `json:"input"`
	ContextSummary string            `json:"context_summary"`
	ToolCalls      []ToolCallRecord  `json:"tool_calls,omitempty"`
	Response       string            `json:"response"`
	TimestampMS    int64             `json:"timestamp_ms"`
}

// ToolCallRecord holds a single tool call parsed from the input prompt.
type ToolCallRecord struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// MetricsRecord summarises a session's usage.
type MetricsRecord struct {
	SessionID    string `json:"session_id"`
	Turns        int    `json:"turns"`
	ApproxTokens int    `json:"approx_tokens"`
	DurationMS   int64  `json:"duration_ms"`
}

// Recorder writes per-turn and per-session records to NDJSON files.
type Recorder struct {
	mu          sync.Mutex
	name        string
	dir         string
	sessionFile *os.File
	metricsFile *os.File
	turn        int
	startMS     int64
	sessionID   string
}

// New creates a Recorder that writes to dir/<name>-sessions.jsonl and
// dir/<name>-metrics.jsonl. Call Close when the session ends.
func New(name, dir string) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mockrecorder: mkdir %s: %w", dir, err)
	}
	sf, err := os.OpenFile(filepath.Join(dir, name+"-sessions.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("mockrecorder: open sessions file: %w", err)
	}
	mf, err := os.OpenFile(filepath.Join(dir, name+"-metrics.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		sf.Close()
		return nil, fmt.Errorf("mockrecorder: open metrics file: %w", err)
	}
	return &Recorder{
		name:        name,
		dir:         dir,
		sessionFile: sf,
		metricsFile: mf,
		startMS:     time.Now().UnixMilli(),
	}, nil
}

// SetSessionID sets the session ID for metrics records.
func (r *Recorder) SetSessionID(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionID = id
}

// WriteTurn appends a TurnRecord to the sessions file.
func (r *Recorder) WriteTurn(input, contextSummary string, toolCalls []ToolCallRecord, response string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turn++
	rec := TurnRecord{
		Turn:           r.turn,
		Input:          input,
		ContextSummary: contextSummary,
		ToolCalls:      toolCalls,
		Response:       response,
		TimestampMS:    time.Now().UnixMilli(),
	}
	return r.writeJSON(r.sessionFile, rec)
}

// WriteMetrics appends a MetricsRecord to the metrics file. approxTokens is a
// rough estimate (len(input+response)/4).
func (r *Recorder) WriteMetrics(approxTokens int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := MetricsRecord{
		SessionID:    r.sessionID,
		Turns:        r.turn,
		ApproxTokens: approxTokens,
		DurationMS:   time.Now().UnixMilli() - r.startMS,
	}
	return r.writeJSON(r.metricsFile, rec)
}

// Close flushes and closes both files.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	if err := r.sessionFile.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := r.metricsFile.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("mockrecorder close: %v", errs)
	}
	return nil
}

func (r *Recorder) writeJSON(f *os.File, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = f.Write(b)
	return err
}
