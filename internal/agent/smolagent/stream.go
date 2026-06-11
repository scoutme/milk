package smolagent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/scoutme/milk/internal/agent/subprocess"
)

// milkEvent is a single NDJSON line emitted by milk-smolagent.
type milkEvent struct {
	Type        string  `json:"type"`
	// system
	SessionID   string  `json:"session_id"`
	// text_delta
	Text        string  `json:"text"`
	// step
	Number      int     `json:"number"`
	Thought     string  `json:"thought"`
	Code        string  `json:"code"`
	// observation
	Content     string  `json:"content"`
	// final_answer — uses Text field
	// result
	IsError     bool    `json:"is_error"`
	Error       string  `json:"error"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// Parser implements subprocess.StreamParser for the milk subprocess protocol.
type Parser struct{}

func (p *Parser) Parse(r io.Reader, out io.Writer, opts subprocess.ParseOpts) (subprocess.ParseResult, error) {
	var res subprocess.ParseResult
	var textBuf strings.Builder

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

		var ev milkEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}

		switch ev.Type {
		case "system":
			if ev.SessionID != "" {
				res.SessionID = ev.SessionID
			}

		case "text_delta":
			fmt.Fprint(out, ev.Text) //nolint:errcheck
			textBuf.WriteString(ev.Text)

		case "step":
			// Render thought and code as a formatted block.
			var b strings.Builder
			b.WriteString("\n\033[2m")
			if ev.Thought != "" {
				fmt.Fprintf(&b, "⚙ step %d: %s\n", ev.Number, ev.Thought)
			}
			if ev.Code != "" {
				fmt.Fprintf(&b, "```python\n%s\n```\n", ev.Code)
			}
			b.WriteString("\033[0m")
			block := b.String()
			fmt.Fprint(out, block) //nolint:errcheck
			textBuf.WriteString(block)

		case "observation":
			if ev.Content != "" {
				block := fmt.Sprintf("\n\033[2m⚙ observation: %s\033[0m\n", ev.Content)
				fmt.Fprint(out, block) //nolint:errcheck
				textBuf.WriteString(block)
			}

		case "final_answer":
			if ev.Text != "" {
				fmt.Fprint(out, ev.Text) //nolint:errcheck
				textBuf.WriteString(ev.Text)
			}

		case "result":
			res.IsError = ev.IsError
			if ev.IsError && ev.Error != "" {
				fmt.Fprintf(out, "\n[error: %s]\n", ev.Error) //nolint:errcheck
			}
			res.InputTokens = ev.InputTokens
			res.OutputTokens = ev.OutputTokens
			res.TotalCostUSD = ev.TotalCostUSD
		}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}

	text := strings.TrimSpace(textBuf.String())
	res.Text = text
	res.EndsWithQ = endsWithQuestion(text)
	return res, nil
}

func endsWithQuestion(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasSuffix(s, "?")
}
