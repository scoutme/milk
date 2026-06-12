package aider

import (
	"fmt"
	"io"
	"strings"

	"github.com/scoutme/milk/internal/agent/subprocess"
)

// Parser implements subprocess.StreamParser for the milk-aider NDJSON protocol.
type Parser struct{}

func (p *Parser) Parse(r io.Reader, out io.Writer, opts subprocess.ParseOpts) (subprocess.ParseResult, error) {
	return subprocess.ParseMilkNDJSON(r, out, opts, handleEvent)
}

func handleEvent(ev subprocess.MilkEvent, out io.Writer, textBuf *strings.Builder) {
	if ev.Type == "edit" && ev.Path != "" {
		block := fmt.Sprintf("\n\033[2m✎ %s\033[0m\n", ev.Path)
		fmt.Fprint(out, block) //nolint:errcheck
		textBuf.WriteString(block)
	}
}
