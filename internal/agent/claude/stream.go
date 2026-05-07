package claude

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

type msgType string

const (
	msgTypeSystem    msgType = "system"
	msgTypeAssistant msgType = "assistant"
	msgTypeResult    msgType = "result"
)

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type assistantMessage struct {
	Content []contentBlock `json:"content"`
}

type streamEvent struct {
	Type      msgType          `json:"type"`
	Subtype   string           `json:"subtype"`
	SessionID string           `json:"session_id"`
	Message   assistantMessage `json:"message"`
	Result    string           `json:"result"`
	IsError   bool             `json:"is_error"`
}

// ParseResult holds the parsed output of a claude subprocess run.
type ParseResult struct {
	SessionID        string
	Text             string
	EndsWithQ        bool   // true if the final text ends with a question mark
	IsError          bool
	PermissionDenied bool   // true if Claude was blocked waiting for a tool approval
	DeniedTool       string // the tool name Claude mentioned needing permission for
	DirRestricted    bool   // true if Claude refused due to directory access restrictions
}

// permissionPhrases are substrings that appear when Claude is blocked on a tool approval.
var permissionPhrases = []string{
	"approve the", "approve this", "need permission",
	"require permission", "waiting for approval",
	"permission to", "grant permission", "allow me to",
}

// detectPermissionDenied scans text for known permission-request phrases.
// It returns true when a phrase matches, plus the first word from knownTools
// that appears in the text (empty string if none found).
// knownTools should come from config so the caller controls the list.
func detectPermissionDenied(text string, knownTools []string) (bool, string) {
	lower := strings.ToLower(text)
	for _, phrase := range permissionPhrases {
		if strings.Contains(lower, strings.ToLower(phrase)) {
			for _, tool := range knownTools {
				if strings.Contains(text, tool) {
					return true, tool
				}
			}
			return true, ""
		}
	}
	return false, ""
}

// dirRestrictionPhrases are substrings that appear when Claude refuses due to
// directory access restrictions (not a tool approval — requires --add-dir).
var dirRestrictionPhrases = []string{
	"is restricted",
	"outside the allowed",
	"not within the allowed",
	"access to",
	"only list files within",
	"cannot access",
	"outside of the",
}

// detectDirRestricted returns true when Claude's response indicates it was
// blocked by directory access restrictions.
func detectDirRestricted(text string) bool {
	lower := strings.ToLower(text)
	for _, phrase := range dirRestrictionPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// Stream parses NDJSON lines from the claude subprocess, writes text tokens
// to out as they arrive, and returns a ParseResult when the stream ends.
// knownTools is the configured allowed-tools list, used to identify which tool
// Claude is asking permission for when a permission phrase is detected.
func Stream(r io.Reader, out io.Writer, knownTools []string) (ParseResult, error) {
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
		applyEvent(&res, &textBuf, out, ev)
	}

	if err := scanner.Err(); err != nil {
		return res, err
	}

	text := strings.TrimSpace(textBuf.String())
	res.Text = text
	res.EndsWithQ = strings.HasSuffix(text, "?")
	res.PermissionDenied, res.DeniedTool = detectPermissionDenied(text, knownTools)
	if !res.PermissionDenied {
		res.DirRestricted = detectDirRestricted(text)
	}
	if text != "" {
		io.WriteString(out, "\n") //nolint:errcheck
	}
	return res, nil
}

// applyEvent updates res and textBuf based on a single stream event.
func applyEvent(res *ParseResult, textBuf *strings.Builder, out io.Writer, ev streamEvent) {
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
	}
}
