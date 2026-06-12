package subprocess

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/scoutme/milk/internal/tags"
)

// MilkEvent is a single NDJSON line emitted by any milk subprocess adapter.
// Fields are the union of all event types across all providers.
type MilkEvent struct {
	Type string `json:"type"`
	// system
	SessionID string `json:"session_id"`
	// text_delta / final_answer
	Text string `json:"text"`
	// edit (aider)
	Path    string `json:"path"`
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
	// step (smolagents)
	Number  int    `json:"number"`
	Thought string `json:"thought"`
	Code    string `json:"code"`
	// observation (smolagents)
	Content string `json:"content"`
	// result
	IsError      bool    `json:"is_error"`
	Error        string  `json:"error"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// EventHandler is called by ParseMilkNDJSON for provider-specific event types
// (i.e. anything other than "system", "text_delta", and "result").
// Implementations may write to out and/or append to textBuf for display.
type EventHandler func(ev MilkEvent, out io.Writer, textBuf *strings.Builder)

// ParseMilkNDJSON is the shared NDJSON parse loop for all milk subprocess providers.
// It handles the common events (system, text_delta, result) and delegates unknown
// event types to handler, which may be nil if the provider has no custom events.
func ParseMilkNDJSON(r io.Reader, out io.Writer, opts ParseOpts, handler EventHandler) (ParseResult, error) {
	var res ParseResult
	var textBuf strings.Builder

	// Wrap out with tag interceptors so need/percept/escalate tags are stripped
	// from display output and their callbacks are fired, mirroring claude.Stream.
	displayOut := out
	var escalateWriter *tags.TagWriter
	var needWriter *tags.TagWriter
	var perceptWriter *tags.PerceptWriter
	if opts.OnEscalate != nil {
		escalateWriter = &tags.TagWriter{
			W:           displayOut,
			OpenPrefix:  tags.EscalateOpenPrefix,
			OnTag:       func(body string) { opts.OnEscalate(strings.TrimSpace(body)) },
			RecordNonce: opts.EscalateNonce,
		}
		displayOut = escalateWriter
	}
	if opts.OnNeed != nil {
		needWriter = &tags.TagWriter{
			W:           displayOut,
			OpenPrefix:  tags.NeedOpenPrefix,
			OnTag:       func(body string) { opts.OnNeed(strings.TrimSpace(body)) },
			RecordNonce: opts.NeedNonce,
		}
		displayOut = needWriter
	}
	if opts.OnPercept != nil {
		perceptWriter = &tags.PerceptWriter{
			W:           displayOut,
			OnPercept:   opts.OnPercept,
			RecordNonce: opts.PerceptNonce,
			AgentNames:  opts.AgentNames,
		}
		displayOut = perceptWriter
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if opts.DebugLog != nil {
			fmt.Fprintln(opts.DebugLog, string(line)) //nolint:errcheck
		}

		var ev MilkEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}

		switch ev.Type {
		case "system":
			if ev.SessionID != "" {
				res.SessionID = ev.SessionID
			}

		case "text_delta":
			fmt.Fprint(displayOut, ev.Text) //nolint:errcheck
			textBuf.WriteString(ev.Text)

		case "result":
			res.IsError = ev.IsError
			if ev.IsError && ev.Error != "" {
				fmt.Fprintf(displayOut, "\n[error: %s]\n", ev.Error) //nolint:errcheck
			}
			res.InputTokens = ev.InputTokens
			res.OutputTokens = ev.OutputTokens
			res.TotalCostUSD = ev.TotalCostUSD

		default:
			if handler != nil {
				handler(ev, displayOut, &textBuf)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}

	if perceptWriter != nil {
		perceptWriter.Flush() //nolint:errcheck
	}
	if needWriter != nil {
		needWriter.Flush() //nolint:errcheck
	}
	if escalateWriter != nil {
		escalateWriter.Flush() //nolint:errcheck
	}

	text := strings.TrimSpace(textBuf.String())
	if opts.OnPercept != nil {
		text = tags.StripPerceptTags(text)
	}
	if opts.OnNeed != nil {
		text = tags.StripTagsByPrefix(text, tags.NeedOpenPrefix)
	}
	if opts.OnEscalate != nil {
		text = tags.StripTagsByPrefix(text, tags.EscalateOpenPrefix)
	}
	res.Text = text
	res.EndsWithQ = endsWithQ(text)
	return res, nil
}

// endsWithQ returns true if the last non-whitespace character of text is '?'.
func endsWithQ(text string) bool {
	last := strings.TrimRight(text, " \t\n\r")
	if last == "" {
		return false
	}
	if idx := strings.LastIndexByte(last, '\n'); idx >= 0 {
		last = last[idx+1:]
	}
	last = strings.TrimSpace(last)
	if last == "" {
		return false
	}
	trimmed := strings.TrimRight(last, " \t")
	return len(trimmed) > 0 && trimmed[len(trimmed)-1] == '?'
}
