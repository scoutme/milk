package smolagent

import (
	"fmt"
	"io"
	"strings"

	"github.com/scoutme/milk/internal/agent/subprocess"
)

// Parser implements subprocess.StreamParser for the milk-smolagent NDJSON protocol.
type Parser struct{}

func (p *Parser) Parse(r io.Reader, out io.Writer, opts subprocess.ParseOpts) (subprocess.ParseResult, error) {
	return subprocess.ParseMilkNDJSON(r, out, opts, handleEvent)
}

func handleEvent(ev subprocess.MilkEvent, out io.Writer, textBuf *strings.Builder) {
	switch ev.Type {
	case "step":
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
	}
}
