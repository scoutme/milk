package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
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

	text := strings.TrimSpace(textBuf.String())
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
