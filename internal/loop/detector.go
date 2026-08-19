// Package loop detects when an LLM agent is stuck in a loop — repeating
// responses, burning tokens without progress, or echoing tool calls across
// turns. It emits Verdict signals that the TUI can use to warn or interrupt.
package loop

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Config controls loop detection thresholds. Zero values use defaults.
type Config struct {
	Enabled bool `json:"enabled"`

	// Response repetition: fire when N consecutive turns have similarity above threshold.
	MaxConsecutiveSimilarResponses int     `json:"max_consecutive_similar_responses"` // default 3
	ResponseSimilarityThreshold    float64 `json:"response_similarity_threshold"`     // default 0.85

	// Token velocity: fire when tokens consumed in the window exceed threshold.
	// Warn-only — never auto-interrupts. High token consumption is normal for active work.
	TokenVelocityWindow    time.Duration `json:"-"`                             // derived from seconds
	TokenVelocitySeconds   int           `json:"token_velocity_window_seconds"` // default 60
	TokenVelocityThreshold int64         `json:"token_velocity_threshold"`      // default 300000

	// Silent burn: fire when input tokens are high but output is near-zero.
	MaxSilentBurnTokens int64 `json:"max_silent_burn_tokens"` // default 20000

	// Turn flood: fire when N turns pass without user input.
	MaxConsecutiveTurnsWithoutUser int `json:"max_consecutive_turns_without_user"` // default 10

	// Tool echo: fire when the same tool+args appear in N consecutive turns.
	ToolEchoThreshold int `json:"tool_echo_threshold"` // default 3

	// Chunk repetition (intra-turn): fire when the same chunk text appears
	// N+ times consecutively within a sliding window of recent chunks.
	ChunkRepetitionThreshold int `json:"chunk_repetition_threshold"` // default 5
	ChunkWindowSize          int `json:"chunk_window_size"`          // default 50

	// Auto-interrupt: when true, high-confidence signals cancel the turn.
	// Token velocity never auto-interrupts regardless of this setting.
	AutoInterrupt bool `json:"auto_interrupt"`
}

func (c *Config) defaults() {
	if c.MaxConsecutiveSimilarResponses <= 0 {
		c.MaxConsecutiveSimilarResponses = 3
	}
	if c.ResponseSimilarityThreshold <= 0 {
		c.ResponseSimilarityThreshold = 0.85
	}
	if c.TokenVelocitySeconds <= 0 {
		c.TokenVelocitySeconds = 60
	}
	c.TokenVelocityWindow = time.Duration(c.TokenVelocitySeconds) * time.Second
	if c.TokenVelocityThreshold <= 0 {
		c.TokenVelocityThreshold = 300000
	}
	if c.MaxSilentBurnTokens <= 0 {
		c.MaxSilentBurnTokens = 20000
	}
	if c.MaxConsecutiveTurnsWithoutUser <= 0 {
		c.MaxConsecutiveTurnsWithoutUser = 10
	}
	if c.ToolEchoThreshold <= 0 {
		c.ToolEchoThreshold = 3
	}
	if c.ChunkRepetitionThreshold <= 0 {
		c.ChunkRepetitionThreshold = 5
	}
	if c.ChunkWindowSize <= 0 {
		c.ChunkWindowSize = 50
	}
}

// Signal identifies which detection rule fired.
type Signal int

const (
	SignalResponseRepetition Signal = iota
	SignalTokenVelocity
	SignalToolCallEcho
	SignalSilentBurn
	SignalTurnFlood
	SignalChunkRepetition // intra-turn: same chunk text repeating
)

func (s Signal) String() string {
	switch s {
	case SignalResponseRepetition:
		return "response_repetition"
	case SignalTokenVelocity:
		return "token_velocity"
	case SignalToolCallEcho:
		return "tool_call_echo"
	case SignalSilentBurn:
		return "silent_burn"
	case SignalTurnFlood:
		return "turn_flood"
	case SignalChunkRepetition:
		return "chunk_repetition"
	default:
		return "unknown"
	}
}

// Verdict is emitted when a detection signal fires.
type Verdict struct {
	Signal          Signal
	Confidence      float64 // 0.0–1.0
	Message         string
	ShouldInterrupt bool // true when confidence is high enough for auto-interrupt
}

// TurnSummary is the caller's description of one completed turn.
type TurnSummary struct {
	Text         string // agent's response text
	InputTokens  int64
	OutputTokens int64
	ToolCalls    []string // opaque identifiers (e.g. "bash\x00ls -la"), empty if unknown
	Timestamp    time.Time
	IsUserTurn   bool // true when the user sent input (not an auto-resume)
}

// Detector accumulates turn history and emits Verdicts when signals fire.
type Detector struct {
	mu      sync.Mutex
	cfg     Config
	history []TurnSummary

	// Intra-turn chunk buffer for detecting within-turn repetition.
	chunkBuf      []string // ring buffer of recent chunk texts
	chunkIdx      int      // next write position in chunkBuf
	chunkCount    int      // total chunks seen in this turn (capped at len(chunkBuf))
	firedChunk    bool     // true once SignalChunkRepetition fired this turn (don't re-fire)
	consecRepeat  int      // count of consecutive same-text chunks at tail of buffer
	lastChunkText string   // text of the most recent chunk (for consecutive detection)
}

// New creates a Detector with the given config. Call Config.defaults() first.
func New(cfg Config) *Detector {
	cfg.defaults()
	return &Detector{cfg: cfg}
}

// Feed records a completed turn and returns any triggered verdicts.
// The caller decides what to do with them (warn, interrupt, log).
func (d *Detector) Feed(turn TurnSummary) []Verdict {
	if !d.cfg.Enabled {
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.history = append(d.history, turn)

	var verdicts []Verdict

	if v := d.checkResponseRepetition(); v != nil {
		verdicts = append(verdicts, *v)
	}
	if v := d.checkTokenVelocity(); v != nil {
		verdicts = append(verdicts, *v)
	}
	if v := d.checkSilentBurn(turn); v != nil {
		verdicts = append(verdicts, *v)
	}
	if v := d.checkToolCallEcho(); v != nil {
		verdicts = append(verdicts, *v)
	}
	if v := d.checkTurnFlood(); v != nil {
		verdicts = append(verdicts, *v)
	}

	return verdicts
}

// Reset clears the turn history (e.g. on session switch or user interrupt).
func (d *Detector) Reset() {
	d.mu.Lock()
	d.history = d.history[:0]
	d.chunkBuf = nil
	d.chunkIdx = 0
	d.chunkCount = 0
	d.firedChunk = false
	d.consecRepeat = 0
	d.lastChunkText = ""
	d.mu.Unlock()
}

// ResetTurn clears the intra-turn chunk buffer. Call this when a new turn starts.
func (d *Detector) ResetTurn() {
	d.mu.Lock()
	d.chunkBuf = nil
	d.chunkIdx = 0
	d.chunkCount = 0
	d.firedChunk = false
	d.consecRepeat = 0
	d.lastChunkText = ""
	d.mu.Unlock()
}

// FeedChunk records a streaming chunk within the current turn and returns
// any triggered verdicts. This is the primary intra-turn loop detector —
// call it for every chunkMsg the TUI receives.
//
// Detection requires N *consecutive* identical chunks (not just N occurrences
// in the window), which avoids false positives when the agent reads multiple
// files and common short phrases appear across different contexts.
func (d *Detector) FeedChunk(text string) []Verdict {
	if !d.cfg.Enabled {
		return nil
	}
	// Skip empty or very short chunks — they're noise (newlines, spaces).
	trimmed := strings.TrimSpace(text)
	if len([]rune(trimmed)) < 5 {
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Lazily allocate the ring buffer.
	if d.chunkBuf == nil {
		d.chunkBuf = make([]string, d.cfg.ChunkWindowSize)
	}

	// Write into the ring buffer.
	d.chunkBuf[d.chunkIdx] = trimmed
	d.chunkIdx = (d.chunkIdx + 1) % len(d.chunkBuf)
	if d.chunkCount < len(d.chunkBuf) {
		d.chunkCount++
	}

	// Track consecutive repeats — reset fired flag when the streak breaks
	// so a new identical streak later in the turn can fire again.
	if trimmed == d.lastChunkText {
		d.consecRepeat++
	} else {
		d.consecRepeat = 1
		d.lastChunkText = trimmed
		d.firedChunk = false
	}

	// Don't re-fire within the same turn.
	if d.firedChunk {
		return nil
	}

	if d.consecRepeat < d.cfg.ChunkRepetitionThreshold {
		return nil
	}

	d.firedChunk = true

	// Truncate the repeated text for the message (max 60 runes).
	display := trimmed
	runes := []rune(display)
	if len(runes) > 60 {
		display = string(runes[:60]) + "…"
	}

	return []Verdict{{
		Signal:          SignalChunkRepetition,
		Confidence:      0.9,
		Message:         fmt.Sprintf("chunk repeating %d×: %q", d.consecRepeat, display),
		ShouldInterrupt: true,
	}}
}

// ── Signal: Response Repetition ──────────────────────────────────────────────

func (d *Detector) checkResponseRepetition() *Verdict {
	n := d.cfg.MaxConsecutiveSimilarResponses
	if len(d.history) < n {
		return nil
	}

	// Check the last n turns for pairwise similarity.
	recent := d.history[len(d.history)-n:]
	for i := 1; i < len(recent); i++ {
		sim := trigramSimilarity(recent[0].Text, recent[i].Text)
		if sim < d.cfg.ResponseSimilarityThreshold {
			return nil // not all similar
		}
	}

	confidence := 0.7 + 0.1*float64(n-3) // 3 turns=0.7, 4=0.8, 5+=0.9
	if confidence > 1.0 {
		confidence = 1.0
	}

	return &Verdict{
		Signal:          SignalResponseRepetition,
		Confidence:      confidence,
		Message:         "agent repeating similar responses",
		ShouldInterrupt: confidence >= 0.8,
	}
}

// ── Signal: Token Velocity ───────────────────────────────────────────────────

func (d *Detector) checkTokenVelocity() *Verdict {
	window := d.cfg.TokenVelocityWindow
	if window <= 0 {
		return nil
	}

	now := time.Now()
	var totalTokens int64
	for i := len(d.history) - 1; i >= 0; i-- {
		if now.Sub(d.history[i].Timestamp) > window {
			break
		}
		totalTokens += d.history[i].InputTokens + d.history[i].OutputTokens
	}

	if totalTokens < d.cfg.TokenVelocityThreshold {
		return nil
	}

	// Confidence scales with how far above threshold we are.
	ratio := float64(totalTokens) / float64(d.cfg.TokenVelocityThreshold)
	confidence := 0.5 + 0.1*(ratio-1.0) // 1x=0.5, 2x=0.6, 3x=0.7, ...
	if confidence > 0.9 {
		confidence = 0.9
	}

	// Token velocity is warn-only — high consumption is normal for active work.
	// Only auto-interrupt when the agent is clearly stuck (other signals).
	return &Verdict{
		Signal:          SignalTokenVelocity,
		Confidence:      confidence,
		Message:         "high token burn rate",
		ShouldInterrupt: false,
	}
}

// ── Signal: Silent Burn ──────────────────────────────────────────────────────

func (d *Detector) checkSilentBurn(turn TurnSummary) *Verdict {
	if turn.InputTokens < d.cfg.MaxSilentBurnTokens {
		return nil
	}
	if turn.OutputTokens > 100 {
		return nil // not silent — agent produced output
	}

	return &Verdict{
		Signal:          SignalSilentBurn,
		Confidence:      0.6,
		Message:         "high input tokens with minimal output",
		ShouldInterrupt: false, // medium confidence — warn only
	}
}

// ── Signal: Tool Call Echo ───────────────────────────────────────────────────

func (d *Detector) checkToolCallEcho() *Verdict {
	n := d.cfg.ToolEchoThreshold
	if len(d.history) < n {
		return nil
	}

	recent := d.history[len(d.history)-n:]

	// All turns must have tool calls.
	for _, t := range recent {
		if len(t.ToolCalls) == 0 {
			return nil
		}
	}

	// Check if every turn has the same tool call set.
	base := toolCallSet(recent[0].ToolCalls)
	for i := 1; i < len(recent); i++ {
		if !toolCallSetEqual(base, toolCallSet(recent[i].ToolCalls)) {
			return nil
		}
	}

	return &Verdict{
		Signal:          SignalToolCallEcho,
		Confidence:      0.85,
		Message:         "same tool calls repeated across turns",
		ShouldInterrupt: true,
	}
}

// ── Signal: Turn Flood ───────────────────────────────────────────────────────

func (d *Detector) checkTurnFlood() *Verdict {
	n := d.cfg.MaxConsecutiveTurnsWithoutUser
	if len(d.history) < n {
		return nil
	}

	// Count consecutive non-user turns at the end.
	streak := 0
	for i := len(d.history) - 1; i >= 0; i-- {
		if d.history[i].IsUserTurn {
			break
		}
		streak++
	}

	if streak < n {
		return nil
	}

	return &Verdict{
		Signal:          SignalTurnFlood,
		Confidence:      0.5,
		Message:         "many turns without user input",
		ShouldInterrupt: false, // low confidence — warn only
	}
}

// ── Trigram Similarity ───────────────────────────────────────────────────────

// trigramSimilarity computes Jaccard similarity on character trigrams.
// Returns 1.0 for identical strings, 0.0 for completely different.
func trigramSimilarity(a, b string) float64 {
	if a == b {
		return 1.0
	}
	aT := trigrams(a)
	bT := trigrams(b)
	if len(aT) == 0 && len(bT) == 0 {
		return 1.0
	}
	intersection := 0
	for t := range aT {
		if bT[t] {
			intersection++
		}
	}
	union := len(aT) + len(bT) - intersection
	if union == 0 {
		return 1.0
	}
	return float64(intersection) / float64(union)
}

func trigrams(s string) map[string]bool {
	runes := []rune(strings.ToLower(s))
	if len(runes) < 3 {
		return map[string]bool{string(runes): true}
	}
	m := make(map[string]bool, len(runes)-2)
	for i := 0; i <= len(runes)-3; i++ {
		m[string(runes[i:i+3])] = true
	}
	return m
}

// ── Tool Call Set Helpers ────────────────────────────────────────────────────

func toolCallSet(calls []string) map[string]bool {
	m := make(map[string]bool, len(calls))
	for _, c := range calls {
		m[c] = true
	}
	return m
}

func toolCallSetEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
