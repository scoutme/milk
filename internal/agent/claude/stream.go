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
	SessionID   string
	Text        string
	EndsWithQ   bool // true if the final text ends with a question mark
	IsError     bool
}

// Stream parses NDJSON lines from the claude subprocess, writes text tokens
// to out as they arrive, and returns a ParseResult when the stream ends.
func Stream(r io.Reader, out io.Writer) (ParseResult, error) {
	var res ParseResult
	var textBuf strings.Builder

	scanner := bufio.NewScanner(r)
	// Increase buffer for long lines (large context responses)
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

	if err := scanner.Err(); err != nil {
		return res, err
	}

	text := strings.TrimSpace(textBuf.String())
	res.Text = text
	res.EndsWithQ = strings.HasSuffix(text, "?")

	// Ensure trailing newline was written
	if text != "" {
		io.WriteString(out, "\n") //nolint:errcheck
	}

	return res, nil
}
