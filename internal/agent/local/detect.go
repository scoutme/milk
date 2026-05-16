package local

import "strings"

// ToolFormat identifies a model's tool-call encoding style.
type ToolFormat string

const (
	ToolFormatUnknown      ToolFormat = ""
	ToolFormatNative       ToolFormat = "native"            // delta tool_calls field
	ToolFormatGemmaSpecial ToolFormat = "gemma_special"     // <|tool_call>...</tool_call|>
	ToolFormatToolCallTag  ToolFormat = "tool_call_tag"     // <tool_call>...</tool_call>
	ToolFormatToolsTag     ToolFormat = "tools_tag"         // <tools>...</tools>
	ToolFormatFencedJSON   ToolFormat = "fenced_json"       // ```json\n...\n```
	ToolFormatBracketCalls ToolFormat = "bracket_tool_calls" // [TOOL_CALLS][...]
)

// delim pairs an opening prefix with its closing string and the format it identifies.
// Longer prefixes must come first so that e.g. "<|tool_call>" is matched before "<tool_call>".
type delim struct {
	open   string
	close  string
	format ToolFormat
}

var knownDelims = []delim{
	{open: "<|tool_call>", close: "<tool_call|>", format: ToolFormatGemmaSpecial},
	{open: "<tool_call>",  close: "</tool_call>", format: ToolFormatToolCallTag},
	{open: "<tools>",      close: "</tools>",     format: ToolFormatToolsTag},
	{open: "```json\n",    close: "```",          format: ToolFormatFencedJSON},
	{open: "```xml\n",     close: "```",          format: ToolFormatFencedJSON},
	{open: "[TOOL_CALLS]", close: "]",            format: ToolFormatBracketCalls},
}

// modelFormatHints maps lower-case model-name substrings to a likely format.
var modelFormatHints = []struct {
	substr string
	format ToolFormat
}{
	{"gemma", ToolFormatGemmaSpecial},
	{"qwen", ToolFormatToolCallTag},
	{"mistral", ToolFormatBracketCalls},
	{"llama", ToolFormatBracketCalls},
}

// GuessFormatFromModel returns the most likely ToolFormat for a model name,
// or ToolFormatUnknown if no hint matches.
func GuessFormatFromModel(modelName string) ToolFormat {
	lower := strings.ToLower(modelName)
	for _, h := range modelFormatHints {
		if strings.Contains(lower, h.substr) {
			return h.format
		}
	}
	return ToolFormatUnknown
}

// detectorState is the internal FSM state.
type detectorState int

const (
	statePrinting       detectorState = iota
	stateMatchingPrefix               // accumulating a potential delimiter prefix
	stateInBlock                      // inside a confirmed tool-call block
)

// StreamDetector is a per-turn, token-by-token detector and accumulator.
//
// Three-state FSM:
//
//	printing ──[prefix of known delimiter]──► matching_prefix
//	matching_prefix ──[delimiter completes]──► in_block
//	matching_prefix ──[no match possible]──► flush pending → printing
type StreamDetector struct {
	// Format is the confirmed format for this session. Preserved across Reset()
	// calls so subsequent turns skip cold detection.
	Format ToolFormat

	state      detectorState
	pendingBuf strings.Builder // held during stateMatchingPrefix
	blockBuf   strings.Builder // accumulated tool-call content in stateInBlock
	activeDelim *delim          // delimiter being matched/closed
}

// NewStreamDetector creates a detector pre-seeded with a known format.
// Pass ToolFormatUnknown to start in full discovery mode.
func NewStreamDetector(known ToolFormat) *StreamDetector {
	return &StreamDetector{Format: known}
}

// Feed accepts one token from the stream.
//
// Returns:
//   - flush: bytes to write to out immediately (pending prefix that failed to
//     match, or plain text in printing state)
//   - complete: true when a full tool-call block is in blockBuf and Extract may
//     be called
func (d *StreamDetector) Feed(token string) (flush []byte, complete bool) {
	switch d.state {
	case statePrinting:
		return d.feedPrinting(token)
	case stateMatchingPrefix:
		return d.feedMatchingPrefix(token)
	case stateInBlock:
		return d.feedInBlock(token)
	}
	return []byte(token), false
}

func (d *StreamDetector) feedPrinting(token string) (flush []byte, complete bool) {
	for i := range knownDelims {
		if !d.formatAllowed(knownDelims[i].format) {
			continue
		}
		open := knownDelims[i].open
		switch {
		case strings.HasPrefix(token, open):
			// Single token contains the full delimiter (possibly plus content).
			d.activeDelim = &knownDelims[i]
			d.Format = knownDelims[i].format
			d.state = stateInBlock
			excess := token[len(open):]
			if excess != "" {
				d.blockBuf.WriteString(excess)
			}
			return nil, false
		case strings.HasPrefix(open, token):
			// Token is a valid prefix of this delimiter.
			d.pendingBuf.WriteString(token)
			d.activeDelim = &knownDelims[i]
			d.state = stateMatchingPrefix
			return nil, false
		}
	}
	return []byte(token), false
}

func (d *StreamDetector) feedMatchingPrefix(token string) (flush []byte, complete bool) {
	candidate := d.pendingBuf.String() + token

	for i := range knownDelims {
		if !d.formatAllowed(knownDelims[i].format) {
			continue
		}
		open := knownDelims[i].open
		switch {
		case strings.HasPrefix(candidate, open):
			// Candidate contains or equals the full open delimiter.
			d.pendingBuf.Reset()
			d.activeDelim = &knownDelims[i]
			d.Format = knownDelims[i].format
			d.state = stateInBlock
			excess := candidate[len(open):]
			if excess != "" {
				d.blockBuf.WriteString(excess)
			}
			return nil, false
		case strings.HasPrefix(open, candidate):
			// Candidate is still a valid prefix — keep accumulating.
			d.pendingBuf.WriteString(token)
			d.activeDelim = &knownDelims[i]
			return nil, false
		}
	}

	// No delimiter matches — release pending buffer as printable text.
	out := candidate
	d.pendingBuf.Reset()
	d.activeDelim = nil
	d.state = statePrinting
	return []byte(out), false
}

func (d *StreamDetector) feedInBlock(token string) (flush []byte, complete bool) {
	d.blockBuf.WriteString(token)
	if d.activeDelim != nil && strings.Contains(d.blockBuf.String(), d.activeDelim.close) {
		return nil, true
	}
	return nil, false
}

// formatAllowed returns true if the given format is compatible with the
// detector's current confirmed format (or if no format is confirmed yet).
func (d *StreamDetector) formatAllowed(f ToolFormat) bool {
	return d.Format == ToolFormatUnknown || d.Format == f
}

// Extract parses and returns tool calls from the accumulated block.
// Must only be called after Feed returns complete=true, or at end-of-stream
// when InBlock() is true (unclosed delimiter fallback).
func (d *StreamDetector) Extract() []toolCall {
	raw := d.blockBuf.String()
	if d.activeDelim != nil {
		// Strip the closing delimiter.
		if idx := strings.Index(raw, d.activeDelim.close); idx >= 0 {
			raw = raw[:idx]
		}
		raw = strings.TrimSpace(raw)
	}
	calls := extractToolCalls(raw)
	if len(calls) == 0 {
		calls = extractToolCalls(d.blockBuf.String())
	}
	return calls
}

// RawBlock returns the raw accumulated block content (including closing delimiter).
func (d *StreamDetector) RawBlock() string {
	return d.blockBuf.String()
}

// InBlock reports whether the detector is currently accumulating a tool block.
func (d *StreamDetector) InBlock() bool {
	return d.state == stateInBlock
}

// Reset clears per-turn state while preserving the detected Format.
func (d *StreamDetector) Reset() {
	d.state = statePrinting
	d.pendingBuf.Reset()
	d.blockBuf.Reset()
	d.activeDelim = nil
}
