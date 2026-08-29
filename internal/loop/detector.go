// Package loop detects when an LLM agent is stuck in a loop — repeating
// responses, burning tokens without progress, or echoing tool calls across
// turns. It emits Verdict signals that the TUI can use to warn or interrupt.
package loop

import (
	"fmt"
	"log/slog"
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

	// Reasoning chunk repetition (intra-turn): same as ChunkRepetitionThreshold
	// but applied to thinking/reasoning stream chunks, not final output. Higher
	// default threshold — reasoning naturally repeats phrases more than final
	// answers do, so it needs more slack before it's treated as a stuck loop.
	ReasoningChunkRepetitionThreshold int `json:"reasoning_chunk_repetition_threshold"` // default 10

	// Reasoning chunk flood (intra-turn): fires when the total number of
	// reasoning chunks exceeds this threshold within a single turn, even if
	// no individual chunk repeats. Catches "death by a thousand cuts" loops
	// where the model cycles through many short, varied reasoning fragments.
	ReasoningChunkFloodThreshold int `json:"reasoning_chunk_flood_threshold"` // default 200

	// Scattered chunk repetition (intra-turn): fires when the same chunk text
	// recurs ChunkRepetitionThreshold+ times within the window WITHOUT requiring
	// adjacency (unlike the consecutive check above). Only chunks at or above
	// this length (in runes) are eligible — short common phrases ("Let me read
	// the config file") legitimately recur across unrelated contexts and must
	// not trigger; a full clause/sentence recurring verbatim with other content
	// between occurrences is a much stronger stuck-loop signal.
	ChunkRepetitionMinScatteredLength int `json:"chunk_repetition_min_scattered_length"` // default 40

	// Reasoning response repetition (cross-turn): same as
	// MaxConsecutiveSimilarResponses but applied to accumulated reasoning text
	// per turn. Higher default threshold for the same reason as above.
	ReasoningMaxConsecutiveSimilarResponses int `json:"reasoning_max_consecutive_similar_responses"` // default 6

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
	if c.ReasoningChunkRepetitionThreshold <= 0 {
		c.ReasoningChunkRepetitionThreshold = c.ChunkRepetitionThreshold * 2
	}
	if c.ReasoningChunkFloodThreshold <= 0 {
		c.ReasoningChunkFloodThreshold = 200
	}
	if c.ChunkRepetitionMinScatteredLength <= 0 {
		c.ChunkRepetitionMinScatteredLength = 40
	}
	if c.ReasoningMaxConsecutiveSimilarResponses <= 0 {
		c.ReasoningMaxConsecutiveSimilarResponses = c.MaxConsecutiveSimilarResponses * 2
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
	SignalChunkRepetition                   // intra-turn: same chunk text repeating
	SignalReasoningChunkRepetition          // intra-turn: same reasoning/thinking chunk text repeating
	SignalReasoningRepetition               // cross-turn: reasoning text repeating across turns
	SignalScatteredChunkRepetition          // intra-turn: same long chunk text recurring without adjacency
	SignalScatteredReasoningChunkRepetition // intra-turn: same long reasoning chunk text recurring without adjacency
	SignalReasoningChunkFlood               // intra-turn: too many reasoning chunks without content output
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
	case SignalReasoningChunkRepetition:
		return "reasoning_chunk_repetition"
	case SignalReasoningRepetition:
		return "reasoning_repetition"
	case SignalScatteredChunkRepetition:
		return "scattered_chunk_repetition"
	case SignalScatteredReasoningChunkRepetition:
		return "scattered_reasoning_chunk_repetition"
	case SignalReasoningChunkFlood:
		return "reasoning_chunk_flood"
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
	Text          string // agent's response text
	ReasoningText string // agent's accumulated thinking/reasoning text for the turn, empty if none
	InputTokens   int64
	OutputTokens  int64
	ToolCalls     []string // opaque identifiers (e.g. "bash\x00ls -la"), empty if unknown
	Timestamp     time.Time
	IsUserTurn    bool // true when the user sent input (not an auto-resume)
}

// Detector accumulates turn history and emits Verdicts when signals fire.
type Detector struct {
	mu      sync.Mutex
	cfg     Config
	history []TurnSummary

	// Intra-turn consecutive-repeat tracking (same chunk text back-to-back,
	// nothing else in between).
	firedChunk    bool   // true once SignalChunkRepetition fired for the current streak text
	consecRepeat  int    // count of consecutive same-text chunks
	lastChunkText string // text of the most recent chunk (for consecutive detection)

	// Intra-turn scattered-repeat tracking (same long chunk text recurring
	// within the window, adjacency not required — see chunkScatter docs).
	chunkScatter scatterState

	// Reasoning counterparts — same shape as the chunk fields above, fed from
	// thinking/reasoning stream chunks and checked against separate (higher)
	// thresholds.
	firedReasonChunk    bool
	consecReasonRepeat  int
	lastReasonChunkText string
	reasonScatter       scatterState

	// Reasoning chunk flood: total count of reasoning chunks in the current
	// turn. Fires once when ReasoningChunkFloodThreshold is exceeded.
	reasonChunkCount int
	firedReasonFlood bool
}

// scatterState tracks exact-match occurrence counts of chunk texts within a
// sliding window, independent of adjacency. Unlike the consecutive-repeat
// counters, this lets a long phrase/sentence that recurs with other content
// in between still be recognized as a loop signal.
type scatterState struct {
	buf   []string // ring buffer of recent eligible (long-enough) chunk texts
	idx   int      // next write position in buf
	occur map[string]int
	fired map[string]bool // texts that already produced a verdict this turn
}

// feed records a chunk in the window (evicting the oldest entry's count as
// it wraps) and returns the resulting occurrence count for text.
func (s *scatterState) feed(windowSize int, text string) int {
	if s.buf == nil {
		s.buf = make([]string, windowSize)
		s.occur = make(map[string]int)
	}
	if old := s.buf[s.idx]; old != "" {
		s.occur[old]--
		if s.occur[old] <= 0 {
			delete(s.occur, old)
		}
	}
	s.buf[s.idx] = text
	s.idx = (s.idx + 1) % len(s.buf)
	s.occur[text]++
	return s.occur[text]
}

func (s *scatterState) alreadyFired(text string) bool {
	return s.fired[text]
}

func (s *scatterState) markFired(text string) {
	if s.fired == nil {
		s.fired = make(map[string]bool)
	}
	s.fired[text] = true
}

func (s *scatterState) reset() {
	*s = scatterState{}
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
		slog.Default().Warn("loop: SIGNAL FIRED (cross-turn)", "signal", v.Signal, "confidence", v.Confidence, "interrupt", v.ShouldInterrupt, "message", v.Message)
		verdicts = append(verdicts, *v)
	}
	if v := d.checkReasoningRepetition(); v != nil {
		slog.Default().Warn("loop: SIGNAL FIRED (cross-turn)", "signal", v.Signal, "confidence", v.Confidence, "interrupt", v.ShouldInterrupt, "message", v.Message)
		verdicts = append(verdicts, *v)
	}
	if v := d.checkTokenVelocity(); v != nil {
		slog.Default().Warn("loop: SIGNAL FIRED (cross-turn)", "signal", v.Signal, "confidence", v.Confidence, "interrupt", v.ShouldInterrupt, "message", v.Message)
		verdicts = append(verdicts, *v)
	}
	if v := d.checkSilentBurn(turn); v != nil {
		slog.Default().Warn("loop: SIGNAL FIRED (cross-turn)", "signal", v.Signal, "confidence", v.Confidence, "interrupt", v.ShouldInterrupt, "message", v.Message)
		verdicts = append(verdicts, *v)
	}
	if v := d.checkToolCallEcho(); v != nil {
		slog.Default().Warn("loop: SIGNAL FIRED (cross-turn)", "signal", v.Signal, "confidence", v.Confidence, "interrupt", v.ShouldInterrupt, "message", v.Message)
		verdicts = append(verdicts, *v)
	}
	if v := d.checkTurnFlood(); v != nil {
		slog.Default().Warn("loop: SIGNAL FIRED (cross-turn)", "signal", v.Signal, "confidence", v.Confidence, "interrupt", v.ShouldInterrupt, "message", v.Message)
		verdicts = append(verdicts, *v)
	}

	slog.Default().Debug("loop: Feed cross-turn",
		"history_len", len(d.history),
		"verdicts", len(verdicts),
		"input_tokens", turn.InputTokens,
		"output_tokens", turn.OutputTokens,
	)

	return verdicts
}

// Reset clears the turn history (e.g. on session switch or user interrupt).
func (d *Detector) Reset() {
	d.mu.Lock()
	d.history = d.history[:0]
	d.firedChunk = false
	d.consecRepeat = 0
	d.lastChunkText = ""
	d.chunkScatter.reset()
	d.firedReasonChunk = false
	d.consecReasonRepeat = 0
	d.lastReasonChunkText = ""
	d.reasonScatter.reset()
	d.reasonChunkCount = 0
	d.firedReasonFlood = false
	d.mu.Unlock()
}

// ResetTurn clears the intra-turn chunk buffers. Call this when a new turn starts.
func (d *Detector) ResetTurn() {
	d.mu.Lock()
	d.firedChunk = false
	d.consecRepeat = 0
	d.lastChunkText = ""
	d.chunkScatter.reset()
	d.firedReasonChunk = false
	d.consecReasonRepeat = 0
	d.lastReasonChunkText = ""
	d.reasonScatter.reset()
	d.reasonChunkCount = 0
	d.firedReasonFlood = false
	d.mu.Unlock()
}

// truncateForDisplay caps text at 60 runes for verdict messages.
func truncateForDisplay(text string) string {
	runes := []rune(text)
	if len(runes) > 60 {
		return string(runes[:60]) + "…"
	}
	return text
}

// FeedChunk records a streaming chunk within the current turn and returns
// any triggered verdicts. This is the primary intra-turn loop detector —
// call it for every chunkMsg the TUI receives.
//
// Two independent checks run:
//   - Consecutive: N identical chunks back-to-back with nothing else in
//     between. Avoids false positives when the agent reads multiple files
//     and common short phrases appear across different contexts.
//   - Scattered: the same chunk recurs N+ times within the window without
//     requiring adjacency, but only for chunks at or above
//     ChunkRepetitionMinScatteredLength runes — long enough that recurrence
//     is a real signal rather than boilerplate ("Let me read...").
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

	var verdicts []Verdict

	// Track consecutive repeats — reset fired flag when the streak breaks
	// so a new identical streak later in the turn can fire again.
	if trimmed == d.lastChunkText {
		d.consecRepeat++
	} else {
		d.consecRepeat = 1
		d.lastChunkText = trimmed
		d.firedChunk = false
	}
	slog.Default().Debug("loop: FeedChunk",
		"consec", d.consecRepeat,
		"threshold", d.cfg.ChunkRepetitionThreshold,
		"chunk", truncateForDisplay(trimmed),
	)
	if !d.firedChunk && d.consecRepeat >= d.cfg.ChunkRepetitionThreshold {
		d.firedChunk = true
		v := Verdict{
			Signal:          SignalChunkRepetition,
			Confidence:      0.9,
			Message:         fmt.Sprintf("chunk repeating %d×: %q", d.consecRepeat, truncateForDisplay(trimmed)),
			ShouldInterrupt: true,
		}
		slog.Default().Warn("loop: SIGNAL FIRED", "signal", v.Signal, "confidence", v.Confidence, "interrupt", v.ShouldInterrupt, "message", v.Message)
		verdicts = append(verdicts, v)
	}

	if len([]rune(trimmed)) >= d.cfg.ChunkRepetitionMinScatteredLength {
		count := d.chunkScatter.feed(d.cfg.ChunkWindowSize, trimmed)
		slog.Default().Debug("loop: FeedChunk scattered",
			"scatter_count", count,
			"scatter_threshold", d.cfg.ChunkRepetitionThreshold,
			"min_len", d.cfg.ChunkRepetitionMinScatteredLength,
			"chunk", truncateForDisplay(trimmed),
		)
		if count >= d.cfg.ChunkRepetitionThreshold && !d.chunkScatter.alreadyFired(trimmed) {
			d.chunkScatter.markFired(trimmed)
			v := Verdict{
				Signal:          SignalScatteredChunkRepetition,
				Confidence:      0.85,
				Message:         fmt.Sprintf("phrase recurring %d× in recent output: %q", count, truncateForDisplay(trimmed)),
				ShouldInterrupt: true,
			}
			slog.Default().Warn("loop: SIGNAL FIRED", "signal", v.Signal, "confidence", v.Confidence, "interrupt", v.ShouldInterrupt, "message", v.Message)
			verdicts = append(verdicts, v)
		}
	}

	return verdicts
}

// FeedReasoningChunk is the reasoning/thinking-stream counterpart to FeedChunk.
// It mirrors the same consecutive + scattered detection but against separate
// buffers and separate (higher) thresholds, since reasoning text repeats
// phrases more often than final output without necessarily being stuck.
func (d *Detector) FeedReasoningChunk(text string) []Verdict {
	if !d.cfg.Enabled {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if len([]rune(trimmed)) < 5 {
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	var verdicts []Verdict

	if trimmed == d.lastReasonChunkText {
		d.consecReasonRepeat++
	} else {
		d.consecReasonRepeat = 1
		d.lastReasonChunkText = trimmed
		d.firedReasonChunk = false
	}
	slog.Default().Debug("loop: FeedReasoningChunk",
		"consec", d.consecReasonRepeat,
		"threshold", d.cfg.ReasoningChunkRepetitionThreshold,
		"chunk", truncateForDisplay(trimmed),
	)
	if !d.firedReasonChunk && d.consecReasonRepeat >= d.cfg.ReasoningChunkRepetitionThreshold {
		d.firedReasonChunk = true
		v := Verdict{
			Signal:          SignalReasoningChunkRepetition,
			Confidence:      0.9,
			Message:         fmt.Sprintf("reasoning repeating %d×: %q", d.consecReasonRepeat, truncateForDisplay(trimmed)),
			ShouldInterrupt: true,
		}
		slog.Default().Warn("loop: SIGNAL FIRED", "signal", v.Signal, "confidence", v.Confidence, "interrupt", v.ShouldInterrupt, "message", v.Message)
		verdicts = append(verdicts, v)
	}

	if len([]rune(trimmed)) >= d.cfg.ChunkRepetitionMinScatteredLength {
		count := d.reasonScatter.feed(d.cfg.ChunkWindowSize, trimmed)
		slog.Default().Debug("loop: FeedReasoningChunk scattered",
			"scatter_count", count,
			"scatter_threshold", d.cfg.ReasoningChunkRepetitionThreshold,
			"chunk", truncateForDisplay(trimmed),
		)
		if count >= d.cfg.ReasoningChunkRepetitionThreshold && !d.reasonScatter.alreadyFired(trimmed) {
			d.reasonScatter.markFired(trimmed)
			v := Verdict{
				Signal:          SignalScatteredReasoningChunkRepetition,
				Confidence:      0.8,
				Message:         fmt.Sprintf("reasoning phrase recurring %d×: %q", count, truncateForDisplay(trimmed)),
				ShouldInterrupt: true,
			}
			slog.Default().Warn("loop: SIGNAL FIRED", "signal", v.Signal, "confidence", v.Confidence, "interrupt", v.ShouldInterrupt, "message", v.Message)
			verdicts = append(verdicts, v)
		}
	}

	// Reasoning chunk flood: total count of reasoning chunks per turn exceeds
	// the threshold. Catches "death by a thousand cuts" loops where the model
	// cycles through many short, varied reasoning fragments that individually
	// evade both consecutive and scattered repetition detectors.
	d.reasonChunkCount++
	slog.Default().Debug("loop: FeedReasoningChunk flood_count",
		"count", d.reasonChunkCount,
		"threshold", d.cfg.ReasoningChunkFloodThreshold,
	)
	if !d.firedReasonFlood && d.cfg.ReasoningChunkFloodThreshold > 0 &&
		d.reasonChunkCount >= d.cfg.ReasoningChunkFloodThreshold {
		d.firedReasonFlood = true
		v := Verdict{
			Signal:          SignalReasoningChunkFlood,
			Confidence:      0.85,
			Message:         fmt.Sprintf("reasoning chunk flood: %d chunks in single turn", d.reasonChunkCount),
			ShouldInterrupt: true,
		}
		slog.Default().Warn("loop: SIGNAL FIRED", "signal", v.Signal, "confidence", v.Confidence, "interrupt", v.ShouldInterrupt, "message", v.Message)
		verdicts = append(verdicts, v)
	}

	return verdicts
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

// ── Signal: Reasoning Repetition ─────────────────────────────────────────────

// checkReasoningRepetition is the cross-turn counterpart to
// checkResponseRepetition, applied to accumulated reasoning/thinking text
// instead of final output. It requires more consecutive similar turns
// (ReasoningMaxConsecutiveSimilarResponses) and reports lower confidence,
// since reasoning naturally revisits similar phrasing across turns without
// the agent being stuck.
func (d *Detector) checkReasoningRepetition() *Verdict {
	n := d.cfg.ReasoningMaxConsecutiveSimilarResponses
	if len(d.history) < n {
		return nil
	}

	recent := d.history[len(d.history)-n:]

	// All turns must have reasoning text — skip turns/models with none.
	for _, t := range recent {
		if strings.TrimSpace(t.ReasoningText) == "" {
			return nil
		}
	}

	for i := 1; i < len(recent); i++ {
		sim := trigramSimilarity(recent[0].ReasoningText, recent[i].ReasoningText)
		if sim < d.cfg.ResponseSimilarityThreshold {
			return nil // not all similar
		}
	}

	confidence := 0.6 + 0.05*float64(n-6) // 6 turns=0.6, 7=0.65, 8+=0.7...
	if confidence > 0.85 {
		confidence = 0.85
	}

	return &Verdict{
		Signal:          SignalReasoningRepetition,
		Confidence:      confidence,
		Message:         "agent repeating similar reasoning",
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
