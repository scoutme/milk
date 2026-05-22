package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"time"
)

type msgType string

const (
	msgTypeSystem         msgType = "system"
	msgTypeAssistant      msgType = "assistant"
	msgTypeResult         msgType = "result"
	msgTypeControlRequest msgType = "control_request"
	msgTypeStreamEvent    msgType = "stream_event"
)

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type assistantMessage struct {
	Content []contentBlock `json:"content"`
}

// streamEventWrapper is the outer envelope for type:"stream_event" lines
// emitted when --include-partial-messages is passed to claude.
type streamEventWrapper struct {
	Event streamEventInner `json:"event"`
}

type streamEventInner struct {
	Type         string             `json:"type"`
	Delta        streamEventDelta   `json:"delta"`
	ContentBlock streamContentBlock `json:"content_block"`
}

type streamEventDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`     // populated for thinking_delta events
	PartialJSON string `json:"partial_json"` // populated for input_json_delta events
}

type streamContentBlock struct {
	Type string `json:"type"`
	Name string `json:"name"` // non-empty for tool_use blocks
}

// controlRequestBody is the inner "request" object in a control_request event.
type controlRequestBody struct {
	Subtype              string         `json:"subtype"`
	ToolName             string         `json:"tool_name"`
	ToolUseID            string         `json:"tool_use_id"`
	Input                map[string]any `json:"input"`
	DisplayName          string         `json:"display_name"`
	Title                string         `json:"title"`
	Description          string         `json:"description"`
	BlockedPath          string         `json:"blocked_path"`
	DecisionReasonType   string         `json:"decision_reason_type"`
	ClassifierApprovable *bool          `json:"classifier_approvable"`
}

// ControlRequest is the structured permission request emitted by Claude when
// --permission-prompt-tool stdio is active and a tool use requires approval.
type ControlRequest struct {
	RequestID string
	Body      controlRequestBody
}

// PermissionHandler is called synchronously when a control_request arrives.
// It must return "allow" or "deny". The handler writes the control_response
// to stdinW before returning. stdinW is Claude's stdin pipe.
type PermissionHandler func(req ControlRequest, stdinW io.Writer)

type streamEvent struct {
	Type              msgType                  `json:"type"`
	Subtype           string                   `json:"subtype"`
	SessionID         string                   `json:"session_id"`
	Message           assistantMessage         `json:"message"`
	Result            string                   `json:"result"`
	IsError           bool                     `json:"is_error"`
	RequestID         string                   `json:"request_id"`
	Request           controlRequestBody       `json:"request"`
	PermissionDenials []PermissionDenialRecord `json:"permission_denials"`
}

// PermissionDenialRecord records a tool that was blocked in the final result event.
type PermissionDenialRecord struct {
	ToolName  string         `json:"tool_name"`
	ToolUseID string         `json:"tool_use_id"`
	ToolInput map[string]any `json:"tool_input"`
}

// ParseResult holds the parsed output of a claude subprocess run.
type ParseResult struct {
	SessionID         string
	Text              string
	EndsWithQ         bool // true if the final text ends with a question mark
	IsError           bool
	PermissionDenials []PermissionDenialRecord // tools silently blocked (post-hoc, from result event)
	// Reactive phrase detection — populated when control_request is not available.
	PermissionDenied bool
	DeniedTool       string
	DirRestricted    bool
	// streamedViaDeltas is set when text_delta events were received, so the
	// final assistant event's text is skipped to avoid double-printing.
	streamedViaDeltas bool
	// hadThinking is set when at least one thinking_delta was emitted, so the
	// first text_delta can insert a newline separator.
	hadThinking bool
}

// detectPermissionDenied scans text for permission-request phrases.
// phrases and toolNames come from config so detection is language-configurable.
func detectPermissionDenied(text string, phrases, toolNames []string) (bool, string) {
	lower := strings.ToLower(text)
	for _, phrase := range phrases {
		if strings.Contains(lower, strings.ToLower(phrase)) {
			for _, tool := range toolNames {
				if strings.Contains(text, tool) {
					return true, tool
				}
			}
			return true, ""
		}
	}
	return false, ""
}

// detectDirRestricted scans text for directory-restriction phrases.
func detectDirRestricted(text string, phrases []string) bool {
	lower := strings.ToLower(text)
	for _, phrase := range phrases {
		if strings.Contains(lower, strings.ToLower(phrase)) {
			return true
		}
	}
	return false
}

// GenerateNonce returns a random 6-character alphanumeric string suitable for
// use as a per-session percept nonce. It is safe to call from multiple goroutines.
func GenerateNonce() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	r := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec
	b := make([]byte, 6)
	for i := range b {
		b[i] = chars[r.Intn(len(chars))]
	}
	return string(b)
}

// StreamOpts holds optional phrase lists for reactive permission detection.
type StreamOpts struct {
	PermissionPhrases     []string
	DirRestrictionPhrases []string
	AllowedTools          []string // used to match tool names in permission phrases
	OnPermission          PermissionHandler
	// OnToolUse is called as soon as Claude begins a tool call (content_block_start
	// with type=tool_use). The tool name is passed; called from the stream goroutine.
	OnToolUse func(name string)
	// OnToolUseReady is called when a tool call block is complete (content_block_stop)
	// and the full input map is available. Supersedes OnToolUse when params are needed.
	OnToolUseReady func(name string, input map[string]any)
	// OnThinking is called for each thinking_delta token. The caller is responsible
	// for any formatting (e.g. dimming). Called from the stream goroutine.
	OnThinking func(text string)
	// OnPercept is called for each <milk:percept:NONCE>…</milk:percept:NONCE> tag found
	// in the stream. The tag content is stripped from the display output before calling.
	// consumerHint is "local", "claude", or "" (all), parsed from an optional "@local: "
	// or "@claude: " prefix in the tag body.
	// PerceptNonce must be set to the same nonce used in the system prompt instruction.
	OnPercept    func(content, consumerHint string)
	PerceptNonce string // session-specific nonce; required when OnPercept is non-nil
	// DebugLog receives every raw NDJSON line from the claude subprocess when non-nil.
	DebugLog io.Writer
}

// scanLines reads r line-by-line in a goroutine (so the pipe is always drained
// even when fn blocks) and calls fn for each non-empty line. Returns the first
// scanner error, if any.
func scanLines(r io.Reader, debugLog io.Writer, fn func([]byte)) error {
	type item struct {
		text string
		err  error
	}
	ch := make(chan item, 256)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			ch <- item{text: sc.Text()}
		}
		if err := sc.Err(); err != nil {
			ch <- item{err: err}
		}
	}()
	for it := range ch {
		if it.err != nil {
			return it.err
		}
		if it.text == "" {
			continue
		}
		if debugLog != nil {
			fmt.Fprintln(debugLog, it.text) //nolint:errcheck
		}
		fn([]byte(it.text))
	}
	return nil
}

// Stream parses NDJSON lines from the claude subprocess, writes text tokens
// to out as they arrive, and returns a ParseResult when the stream ends.
//
// The scanner runs in a goroutine so the OS pipe is always drained even while
// onPermission blocks waiting for user input. Without this, Claude's stdout
// pipe buffer (64 KB) can fill when --verbose produces output after a
// control_request, causing Claude to block on write and terminate.
func Stream(r io.Reader, out io.Writer, stdinW io.Writer, opts StreamOpts) (ParseResult, error) {
	onPermission := opts.OnPermission
	if onPermission == nil {
		onPermission = denyAllHandler
	}
	if opts.OnPercept != nil {
		out = &perceptWriter{w: out, onPercept: opts.OnPercept, recordNonce: opts.PerceptNonce}
	}
	cb := eventCallbacks{onPermission: onPermission, onToolUse: opts.OnToolUse, onToolUseReady: opts.OnToolUseReady, onThinking: opts.OnThinking}

	var res ParseResult
	var textBuf strings.Builder
	var tb toolBlockState

	if err := scanLines(r, opts.DebugLog, func(raw []byte) {
		var ev streamEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return
		}
		applyEvent(&res, &textBuf, out, ev, raw, stdinW, cb, &tb)
	}); err != nil {
		return res, err
	}

	if pw, ok := out.(*perceptWriter); ok {
		pw.flush() //nolint:errcheck
	}

	text := strings.TrimSpace(textBuf.String())
	if opts.OnPercept != nil {
		text = stripPerceptTags(text, opts.PerceptNonce)
	}
	res.Text = text
	res.EndsWithQ = strings.HasSuffix(text, "?")

	// Reactive phrase detection — fallback for when control_request is not emitted.
	if !res.PermissionDenied && len(opts.PermissionPhrases) > 0 {
		res.PermissionDenied, res.DeniedTool = detectPermissionDenied(text, opts.PermissionPhrases, opts.AllowedTools)
	}
	if !res.DirRestricted && !res.PermissionDenied && len(opts.DirRestrictionPhrases) > 0 {
		res.DirRestricted = detectDirRestricted(text, opts.DirRestrictionPhrases)
	}

	if text != "" {
		io.WriteString(out, "\n") //nolint:errcheck
	}
	return res, nil
}

// eventCallbacks groups the optional callbacks passed to applyEvent.
type eventCallbacks struct {
	onPermission   PermissionHandler
	onToolUse      func(string)
	onToolUseReady func(string, map[string]any)
	onThinking     func(string)
}

// applyEvent updates res and textBuf based on a single stream event.
func applyEvent(res *ParseResult, textBuf *strings.Builder, out io.Writer, ev streamEvent, raw []byte, stdinW io.Writer, cb eventCallbacks, tb *toolBlockState) {
	switch ev.Type {
	case msgTypeSystem:
		applySystem(res, ev)
	case msgTypeStreamEvent:
		applyStreamEvent(res, textBuf, out, raw, cb, tb)
	case msgTypeAssistant:
		applyAssistant(res, textBuf, out, ev)
	case msgTypeResult:
		applyResult(res, ev)
	case msgTypeControlRequest:
		cb.onPermission(ControlRequest{RequestID: ev.RequestID, Body: ev.Request}, stdinW)
	}
}

func applySystem(res *ParseResult, ev streamEvent) {
	if ev.SessionID != "" {
		res.SessionID = ev.SessionID
	}
}

func applyResult(res *ParseResult, ev streamEvent) {
	if ev.SessionID != "" {
		res.SessionID = ev.SessionID
	}
	res.IsError = ev.IsError
	res.PermissionDenials = ev.PermissionDenials
}

func applyAssistant(res *ParseResult, textBuf *strings.Builder, out io.Writer, ev streamEvent) {
	if res.streamedViaDeltas {
		return
	}
	ensureNewline(textBuf, out)
	for _, block := range ev.Message.Content {
		if block.Type == "text" && block.Text != "" {
			textBuf.WriteString(block.Text)
			io.WriteString(out, block.Text) //nolint:errcheck
		}
	}
}

func applyStreamEvent(res *ParseResult, textBuf *strings.Builder, out io.Writer, raw []byte, cb eventCallbacks, toolBlock *toolBlockState) {
	var wrapper streamEventWrapper
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return
	}
	switch wrapper.Event.Type {
	case "message_start":
		ensureNewline(textBuf, out)
	case "content_block_start":
		if wrapper.Event.ContentBlock.Type == "tool_use" && wrapper.Event.ContentBlock.Name != "" {
			if cb.onToolUse != nil {
				cb.onToolUse(wrapper.Event.ContentBlock.Name)
			}
			if cb.onToolUseReady != nil {
				toolBlock.name = wrapper.Event.ContentBlock.Name
				toolBlock.buf.Reset()
			}
		}
	case "content_block_delta":
		if cb.onToolUseReady != nil && wrapper.Event.Delta.Type == "input_json_delta" {
			toolBlock.buf.WriteString(wrapper.Event.Delta.PartialJSON)
		}
		applyDelta(res, textBuf, out, wrapper.Event.Delta, cb.onThinking)
	case "content_block_stop":
		if cb.onToolUseReady != nil && toolBlock.name != "" {
			var input map[string]any
			if s := toolBlock.buf.String(); s != "" {
				json.Unmarshal([]byte(s), &input) //nolint:errcheck
			}
			cb.onToolUseReady(toolBlock.name, input)
			toolBlock.name = ""
			toolBlock.buf.Reset()
		}
	}
}

// toolBlockState tracks the current tool_use content block being streamed.
type toolBlockState struct {
	name string
	buf  strings.Builder
}

func applyDelta(res *ParseResult, textBuf *strings.Builder, out io.Writer, delta streamEventDelta, onThinking func(string)) {
	switch delta.Type {
	case "text_delta":
		if delta.Text == "" {
			return
		}
		if res.hadThinking && !res.streamedViaDeltas {
			io.WriteString(out, "\n") //nolint:errcheck
		}
		textBuf.WriteString(delta.Text)
		io.WriteString(out, delta.Text) //nolint:errcheck
		res.streamedViaDeltas = true
	case "thinking_delta":
		if delta.Thinking != "" {
			res.hadThinking = true
			if onThinking != nil {
				onThinking(delta.Thinking)
			}
		}
	}
}

// ensureNewline appends a newline to textBuf and out if the last byte is not already '\n'.
func ensureNewline(textBuf *strings.Builder, out io.Writer) {
	if textBuf.Len() > 0 && textBuf.String()[textBuf.Len()-1] != '\n' {
		textBuf.WriteByte('\n')
		io.WriteString(out, "\n") //nolint:errcheck
	}
}

// perceptTagPair returns the open and close tag strings for the given nonce.
// e.g. nonce "ab1c2d" → "<milk:percept:ab1c2d>", "</milk:percept:ab1c2d>"
func perceptTagPair(nonce string) (open, close_ string) {
	if nonce == "" {
		return perceptOpenLegacy, perceptCloseLegacy
	}
	return "<milk:percept:" + nonce + ">", "</milk:percept:" + nonce + ">"
}

// stripPerceptTags removes all <milk:percept:*>…</milk:percept:*> occurrences from s,
// regardless of nonce. This prevents stale-nonce tags (from old injected context) from
// leaking into the display output.
func stripPerceptTags(s, _ string) string {
	const openPrefix = "<milk:percept:"
	const closePrefix = "</milk:percept:"
	for {
		open := strings.Index(s, openPrefix)
		if open < 0 {
			break
		}
		// Find the end of the open tag.
		openEnd := strings.Index(s[open:], ">")
		if openEnd < 0 {
			s = s[:open]
			break
		}
		openEnd += open + 1 // position after '>'
		// Derive the expected close tag from the nonce inside the open tag.
		noncePart := s[open+len(openPrefix) : openEnd-1] // text between "<milk:percept:" and ">"
		closeTag := closePrefix + noncePart + ">"
		closeIdx := strings.Index(s[openEnd:], closeTag)
		if closeIdx < 0 {
			// Also try any close tag as fallback.
			closeAny := strings.Index(s[openEnd:], closePrefix)
			if closeAny < 0 {
				s = s[:open]
				break
			}
			closeAny += openEnd
			closeEnd := strings.Index(s[closeAny:], ">")
			if closeEnd < 0 {
				s = s[:open]
				break
			}
			s = s[:open] + s[closeAny+closeEnd+1:]
		} else {
			s = s[:open] + s[openEnd+closeIdx+len(closeTag):]
		}
	}
	return strings.TrimSpace(s)
}

// perceptWriter wraps an io.Writer and intercepts <milk:percept:*>…</milk:percept:*>
// tags in the byte stream. Tags matching the current nonce have their body passed
// to onPercept. ALL percept tags (any nonce) are stripped from the display output,
// preventing stale-nonce tags from leaked context from reaching the TUI.
//
// Tags may span multiple Write calls, so partial tag bytes are buffered until
// a complete open tag, body, and close tag have been seen.
//
// The tag body may be prefixed with "@local: " or "@claude: " to target a
// specific agent; parsePerceptBody strips the prefix and returns the hint.
// perceptOpenPrefix is the fixed prefix of all percept open tags.
const perceptOpenPrefix = "<milk:percept:"

type perceptWriter struct {
	w           io.Writer
	onPercept   func(content, consumerHint string)
	recordNonce string          // only tags with this nonce call onPercept; others are still stripped
	closeTag    string          // set once an open tag is fully parsed; cleared on close
	buf         strings.Builder // accumulates bytes while inside or possibly inside a tag
	inTag       bool            // true once the open tag is confirmed
}

// consumerHintFrom strips an optional "@local: " or "@claude: " prefix from s
// and returns the remaining body and the hint label ("local", "claude", or "").
func consumerHintFrom(s string) (body, hint string) {
	for _, h := range []string{"local", "claude"} {
		prefix := "@" + h + ": "
		if strings.HasPrefix(s, prefix) {
			return strings.TrimPrefix(s, prefix), h
		}
	}
	return s, ""
}

// perceptOpenLegacy / perceptCloseLegacy are kept only for test backwards-compatibility
// with the zero-nonce code path. Production code always uses a nonce.
const perceptOpenLegacy = "<milk:percept>"
const perceptCloseLegacy = "</milk:percept>"

func (pw *perceptWriter) Write(p []byte) (int, error) {
	n := len(p)
	for _, b := range p {
		if pw.inTag {
			// Accumulate until we see the close tag.
			pw.buf.WriteByte(b)
			s := pw.buf.String()
			if idx := strings.Index(s, pw.closeTag); idx >= 0 {
				raw := strings.TrimSpace(s[:idx])
				// Only record into memory when the nonce matches the current turn.
				if pw.onPercept != nil && raw != "" && pw.closeTag == "</milk:percept:"+pw.recordNonce+">" {
					body, hint := consumerHintFrom(raw)
					pw.onPercept(body, hint)
				}
				// Emit any bytes after the close tag to the real writer (tag body is always stripped).
				tail := s[idx+len(pw.closeTag):]
				pw.buf.Reset()
				pw.closeTag = ""
				pw.inTag = false
				if tail != "" {
					if _, err := io.WriteString(pw.w, tail); err != nil {
						return n, err
					}
				}
			}
		} else {
			pw.buf.WriteByte(b)
			s := pw.buf.String()
			if idx := strings.Index(s, perceptOpenPrefix); idx >= 0 {
				// Check if we have the full open tag yet (needs closing '>').
				afterPrefix := s[idx+len(perceptOpenPrefix):]
				closeAngle := strings.Index(afterPrefix, ">")
				if closeAngle < 0 {
					// Partial open tag — flush everything before the prefix, keep buffering.
					before := s[:idx]
					if before != "" {
						if _, err := io.WriteString(pw.w, before); err != nil {
							return n, err
						}
						pw.buf.Reset()
						pw.buf.WriteString(s[idx:])
					}
					continue
				}
				// Full open tag found: extract nonce and set closeTag.
				nonce := afterPrefix[:closeAngle]
				pw.closeTag = "</milk:percept:" + nonce + ">"
				before := s[:idx]
				if before != "" {
					if _, err := io.WriteString(pw.w, before); err != nil {
						return n, err
					}
				}
				pw.buf.Reset()
				pw.inTag = true
			} else if !strings.HasPrefix(perceptOpenPrefix, s) {
				// buf is not a prefix of the open prefix — flush and reset.
				if _, err := io.WriteString(pw.w, s); err != nil {
					return n, err
				}
				pw.buf.Reset()
			}
			// else: buf is a prefix of perceptOpenPrefix — keep buffering.
		}
	}
	return n, nil
}

// flush emits any bytes remaining in the buffer to the underlying writer.
// Must be called after the last Write to avoid dropping buffered content.
// If inTag is true (unclosed open tag), the buffered content is discarded
// because it is part of an incomplete percept that was never closed — emitting
// partial tag markup would corrupt the display.
func (pw *perceptWriter) flush() error {
	if pw.inTag || pw.buf.Len() == 0 {
		pw.buf.Reset()
		return nil
	}
	s := pw.buf.String()
	pw.buf.Reset()
	_, err := io.WriteString(pw.w, s)
	return err
}

// denyAllHandler is the fallback when no PermissionHandler is provided.
func denyAllHandler(req ControlRequest, stdinW io.Writer) {
	sendControlResponse(stdinW, req.RequestID, "deny")
}

// Allow sends an allow control_response to Claude's stdin pipe.
func Allow(requestID string, w io.Writer) { sendControlResponse(w, requestID, "allow") }

// Deny sends a deny control_response to Claude's stdin pipe.
func Deny(requestID string, w io.Writer) { sendControlResponse(w, requestID, "deny") }

// sendControlResponse writes a control_response NDJSON line to Claude's stdin.
func sendControlResponse(w io.Writer, requestID, behavior string) {
	resp := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   map[string]any{"behavior": behavior, "updatedInput": map[string]any{}},
		},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "%s\n", b) //nolint:errcheck
}
