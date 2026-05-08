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
)

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type assistantMessage struct {
	Content []contentBlock `json:"content"`
}

// controlRequestBody is the inner "request" object in a control_request event.
type controlRequestBody struct {
	Subtype            string         `json:"subtype"`
	ToolName           string         `json:"tool_name"`
	ToolUseID          string         `json:"tool_use_id"`
	Input              map[string]any `json:"input"`
	DisplayName        string         `json:"display_name"`
	Title              string         `json:"title"`
	Description        string         `json:"description"`
	BlockedPath        string         `json:"blocked_path"`
	DecisionReasonType string         `json:"decision_reason_type"`
	ClassifierApprovable *bool        `json:"classifier_approvable"`
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
	Type      msgType            `json:"type"`
	Subtype   string             `json:"subtype"`
	SessionID string             `json:"session_id"`
	Message   assistantMessage   `json:"message"`
	Result    string             `json:"result"`
	IsError   bool               `json:"is_error"`
	RequestID string             `json:"request_id"`
	Request   controlRequestBody `json:"request"`
}

// PermissionDenialRecord records a tool that was blocked in the final result event.
type PermissionDenialRecord struct {
	ToolName  string         `json:"tool_name"`
	ToolUseID string         `json:"tool_use_id"`
	ToolInput map[string]any `json:"tool_input"`
}

type resultEvent struct {
	PermissionDenials []PermissionDenialRecord `json:"permission_denials"`
}

// ParseResult holds the parsed output of a claude subprocess run.
type ParseResult struct {
	SessionID         string
	Text              string
	EndsWithQ         bool // true if the final text ends with a question mark
	IsError           bool
	PermissionDenials []PermissionDenialRecord // tools silently blocked (post-hoc)
}

// Stream parses NDJSON lines from the claude subprocess, writes text tokens
// to out as they arrive, and returns a ParseResult when the stream ends.
// onPermission is called synchronously for each control_request event;
// pass nil to use the default deny-all handler.
func Stream(r io.Reader, out io.Writer, stdinW io.Writer, onPermission PermissionHandler) (ParseResult, error) {
	if onPermission == nil {
		onPermission = denyAllHandler
	}

	var res ParseResult
	var textBuf strings.Builder

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		applyEvent(&res, &textBuf, out, ev, stdinW, onPermission)
	}

	if err := scanner.Err(); err != nil {
		return res, err
	}

	text := strings.TrimSpace(textBuf.String())
	res.Text = text
	res.EndsWithQ = strings.HasSuffix(text, "?")
	if text != "" {
		io.WriteString(out, "\n") //nolint:errcheck
	}
	return res, nil
}

// applyEvent updates res and textBuf based on a single stream event.
func applyEvent(res *ParseResult, textBuf *strings.Builder, out io.Writer, ev streamEvent, stdinW io.Writer, onPermission PermissionHandler) {
	switch ev.Type {
	case msgTypeSystem:
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
	case msgTypeAssistant:
		for _, block := range ev.Message.Content {
			if block.Type == "text" && block.Text != "" {
				textBuf.WriteString(block.Text)
				io.WriteString(out, block.Text) //nolint:errcheck
			}
		}
	case msgTypeResult:
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		res.IsError = ev.IsError
		// Parse permission_denials from the raw result event
		var re resultEvent
		// re-marshal and re-unmarshal via the dedicated type to pick up the field
		if b, err := json.Marshal(ev); err == nil {
			json.Unmarshal(b, &re) //nolint:errcheck
		}
		res.PermissionDenials = re.PermissionDenials
	case msgTypeControlRequest:
		req := ControlRequest{RequestID: ev.RequestID, Body: ev.Request}
		onPermission(req, stdinW)
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
